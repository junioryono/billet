package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/relay"
)

// The operator's way to a terminal in a job's VM. The peer has been checked by the socket
// (peerListener), the lease by the ledger, and the tier by this config, before a node is asked.
const (
	operatorAttachPath     = "/v1/attach"
	operatorAttachExitPath = "/v1/attach/exited"
	operatorAttachProtocol = "billet-attach"
	// operatorHoldEndWait bounds writing that a hold ended, after its session did.
	operatorHoldEndWait = 5 * time.Second
)

// attach joins an operator's connection to a terminal in a job's VM. Everything that can be
// refused is refused before the upgrade, so it arrives as a status.
func (s *operatorServer) attach(ctx context.Context, cfg *config.Config, w http.ResponseWriter, r *http.Request) {
	leaseID := r.URL.Query().Get("lease")

	openCtx, cancel := context.WithTimeout(ctx, operatorWait)
	defer cancel()

	lease, err := s.jobs.Lease(openCtx, leaseID)

	switch {
	// A LEASE THAT HAS ENDED READS AS NOT FOUND, so this is what a finished job gets too.
	case errors.Is(err, alloc.ErrLeaseNotFound):
		http.Error(w, "no job with this lease is running: it has finished, or never ran here", http.StatusNotFound)

		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusServiceUnavailable)

		return
	}

	tier, known := cfg.TierByLabel(lease.Tier)
	if !known || !tier.Attachable() {
		http.Error(w, fmt.Sprintf("tier %s does not set debug.attach", lease.Tier), http.StatusForbidden)

		return
	}

	if lease.Node == "" || !mayHoldVM(lease.Phase) {
		http.Error(w, fmt.Sprintf("this job's VM is not running: its lease is %s", lease.Phase), http.StatusConflict)

		return
	}

	guest, err := s.plane.OpenGuestStream(openCtx, lease.Node, leaseID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)

		return
	}

	operator, err := nodeplane.UpgradeStream(w, operatorAttachProtocol)
	if err != nil {
		_ = guest.Close()
		s.log.Warn("could not take a terminal's connection on the operator socket", "error", err)

		return
	}

	s.log.Info("an operator attached to a job's VM", "lease", leaseID, "node", lease.Node, "tier", lease.Tier)

	s.sessionBegan(leaseID)
	defer s.sessionEnded(ctx, leaseID)

	relay.Bridge(ctx, operator, guest)
}

// mayHoldVM says a lease in this phase may have a VM to reach. Whether it really does is the node's
// to say, which it does when it reaches for one, so this refuses only what certainly has none.
//
// LAUNCHING IS WHERE A RUNNING JOB'S LEASE STAYS: nothing in production advances one to online or
// busy. CUSTODY is what a node that restarted adopts what survived into, and QUARANTINE what a lease
// nobody renewed for a while becomes, a held VM among them in either.
func mayHoldVM(phase alloc.Phase) bool {
	switch phase {
	case alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy, alloc.PhaseCustody, alloc.PhaseQuarantine:
		return true
	default:
		return false
	}
}

func (s *operatorServer) sessionBegan(leaseID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sessions == nil {
		s.sessions = map[string]int{}
	}

	s.sessions[leaseID]++
}

// shellExited notes that a session's shell ended, which its command says, since neither this process
// nor the node reads the frames that carry it. THE WORD USUALLY ARRIVES AFTER THE SESSION HAS GONE: the
// guest closes behind its exit frame, and that close beats the command's new connection here. So with no
// session left this ends the hold itself, since this was the last and its shell exited.
func (s *operatorServer) shellExited(w http.ResponseWriter, r *http.Request) {
	leaseID := r.URL.Query().Get("lease")

	// UNDER THE LOCK A SESSION IS COUNTED UNDER, across the write: a session opening between the look
	// and the end would otherwise have its VM destroyed under it.
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.sessions[leaseID] > 0 {
		if s.exited == nil {
			s.exited = map[string]bool{}
		}

		s.exited[leaseID] = true
		w.WriteHeader(http.StatusNoContent)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), operatorHoldEndWait)
	defer cancel()

	if _, err := s.jobs.EndLeaseHold(ctx, leaseID, alloc.HoldEndedBySession); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// sessionEnded counts a session out. THE LAST ONE CLOSING ENDS A HOLD ONLY IF A SHELL EXITED: whoever
// was looking has finished, so the capacity need not wait out the bound. A connection that merely
// went says nothing of that, whether it was an operator's dropped link, a node restarting or this
// process stopping, and the VM they were looking at must not go with it; the bound still ends it.
func (s *operatorServer) sessionEnded(ctx context.Context, leaseID string) {
	s.log.Info("an operator's session in a job's VM ended", "lease", leaseID)

	// UNDER THE LOCK A SESSION IS COUNTED UNDER, across the write: a session opening between the look
	// and the end would otherwise have its VM destroyed under it.
	s.mu.Lock()
	defer s.mu.Unlock()

	s.sessions[leaseID]--
	last := s.sessions[leaseID] <= 0
	exited := s.exited[leaseID]

	if last {
		delete(s.sessions, leaseID)
		delete(s.exited, leaseID)
	}

	if !last || !exited || ctx.Err() != nil {
		return
	}

	endCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), operatorHoldEndWait)
	defer cancel()

	if _, err := s.jobs.EndLeaseHold(endCtx, leaseID, alloc.HoldEndedBySession); err != nil {
		s.log.Warn("could not end a held job's hold after its session closed; its bound still ends it",
			"lease", leaseID, "error", err)
	}
}

// AttachedShellExited tells the control plane that a session's shell ended, while the session is still
// connected. ErrNoController says nothing answered, which costs a hold its early end and nothing else.
func AttachedShellExited(ctx context.Context, cfg *config.Config, leaseID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"http://operator"+operatorAttachExitPath+"?lease="+url.QueryEscape(leaseID), http.NoBody)
	if err != nil {
		return err
	}

	resp, err := operatorClient(cfg).Do(req)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrNoController, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the control plane answered %s", resp.Status)
	}

	return nil
}

// AttachJob opens a terminal in a job's VM through the control plane running on this host, and
// answers the stream the guest's frames arrive on. ErrNoController says nothing answered; any
// other error is what it said.
func AttachJob(ctx context.Context, cfg *config.Config, leaseID string) (io.ReadWriteCloser, error) {
	if cfg.Server == nil {
		return nil, errors.New("this command runs on the control plane, and this config has no server section")
	}

	path := operatorSocketPath(cfg)

	// NO CLIENT TIMEOUT: it would wrap the body and cut the session. ctx bounds the wait for the
	// answer, and an upgraded connection is its caller's after that.
	client := &http.Client{Transport: &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer

			return d.DialContext(ctx, "unix", path)
		},
	}}

	openCtx, cancel := context.WithTimeout(ctx, operatorWait+5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(openCtx, http.MethodPost,
		"http://operator"+operatorAttachPath+"?lease="+url.QueryEscape(leaseID), http.NoBody)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", operatorAttachProtocol)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNoController, err)
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()

		text, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<10)) //nolint:errcheck // the status is the answer

		return nil, fmt.Errorf("the control plane answered %s: %s", resp.Status, strings.TrimSpace(string(text)))
	}

	stream, ok := resp.Body.(io.ReadWriteCloser)
	if !ok {
		_ = resp.Body.Close()

		return nil, errors.New("the control plane's connection cannot be written to")
	}

	return stream, nil
}
