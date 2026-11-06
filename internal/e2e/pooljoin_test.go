package e2e

import (
	"context"
	"errors"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/pool"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgertest"
	"github.com/junioryono/billet/internal/wirecert"
)

// consoleSource is a source whose hosts' consoles are a buffer a test writes, read the
// way a real source reads them: through hostsource.ConsoleFingerprints. Only Witness is
// ever asked of it here.
type consoleSource struct {
	hostsource.Source
	mu      sync.Mutex
	console map[string]string // host -> console
	asked   []string
}

func (s *consoleSource) write(host, line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.console[host] += "\r\n" + line + "\r\n"
}

func (s *consoleSource) Witness(_ context.Context, c hostsource.Claim) hostsource.Testimony {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, c.HostID)
	keys := hostsource.ConsoleFingerprints(s.console[c.HostID], c.Order)
	switch {
	case len(keys) == 0:
		return hostsource.Testimony{Reason: "nothing on the console yet"}
	case len(keys) == 1 && keys[0] == c.Fingerprint:
		return hostsource.Testimony{Verdict: hostsource.VerdictVouches, Reason: "the console names it"}
	}

	return hostsource.Testimony{Verdict: hostsource.VerdictContradicts, Reason: "the console names " + strings.Join(keys, ",")}
}

// poolJoinWire is the enrollment listener as the control plane assembles it for a pool:
// the real allocator and ledger, and the real judge over a console source.
type poolJoinWire struct {
	ca    *wirecert.CA
	base  string
	alloc *alloc.Allocator
	src   *consoleSource
	mu    sync.Mutex
	now   time.Time
}

type joinsOf struct {
	*alloc.Allocator
	*pool.Joins
}

func newPoolJoinWire(t *testing.T) *poolJoinWire {
	t.Helper()
	dir := t.TempDir()
	ledgertest.Seed(t, dir)
	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 64, MaxMemory: 256 * config.GiB}, []config.Tier{{
		Label: "fc", Provider: config.ProviderFirecracker, GuestOS: config.GuestLinux, VCPU: 2, Memory: 4 * config.GiB,
		Image: "ubuntu@1",
	}})
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}
	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), wireDeployment)
	if err != nil {
		t.Fatal(err)
	}
	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	conf, err := wirecert.BootstrapTLS(serving)
	if err != nil {
		t.Fatal(err)
	}
	w := &poolJoinWire{ca: ca, alloc: a, now: time.Now(), src: &consoleSource{console: map[string]string{}}}
	clock := func() time.Time {
		w.mu.Lock()
		defer w.mu.Unlock()

		return w.now
	}
	joins := joinsOf{Allocator: a, Joins: pool.NewJoins(a, map[string]hostsource.Source{"metal": w.src}, clock)}
	log := slog.New(slog.DiscardHandler)
	srv := httptest.NewUnstartedServer(nodeplane.BootstrapHandler(log, nodeplane.New(log, wireDeployment, time.Minute), ca,
		nodeplane.WithEnrollment(a), nodeplane.WithPoolJoins(joins, true)))
	srv.TLS = conf
	srv.StartTLS()
	t.Cleanup(srv.Close)
	w.base = srv.URL

	return w
}

// order records and places an order, minting its token as the pool does.
func (w *poolJoinWire) order(t *testing.T, id string) string {
	t.Helper()
	ctx := t.Context()
	if err := w.alloc.RecordPoolOrder(ctx, alloc.PoolOrder{ID: id, Source: "metal", Scope: "metal:here", Shape: "m",
		Arch: config.ArchX64}); err != nil {
		t.Fatal(err)
	}
	token, err := w.alloc.NewPoolJoinToken(ctx, id, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.alloc.PoolOrderPlaced(ctx, id, "host-"+id); err != nil {
		t.Fatal(err)
	}

	return token
}

func (w *poolJoinWire) ask(t *testing.T, name, token string, csrPEM []byte) ([]byte, error) {
	t.Helper()
	caPEM, _, err := nodeclient.FetchCA(t.Context(), w.base, w.ca.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := nodeclient.EnrollWithIdentity(t.Context(), w.base, name, token, caPEM, csrPEM,
		nodeclient.Identity{Document: []byte(`{"instanceId":"host"}`), Signature: "c2ln"})

	return cert, err
}

// A POOL HOST JOINS WITHOUT A PERSON ONCE ITS SOURCE VOUCHES FOR IT, through the
// assembly the control plane uses: its first ask, before its console shows its key, is
// recorded pending under its order; once the console names it and the judge asks
// again, it is handed a certificate for its order's name, and the order is linked to
// its node with that key and the certificate's serial. A retry is answered the same
// certificate.
func TestAPoolHostJoinsOnceItsSourceVouches(t *testing.T) {
	t.Parallel()
	w := newPoolJoinWire(t)
	token := w.order(t, "pa")
	name := alloc.PoolNodeName("pa")
	csr, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := wirecert.FingerprintOfCSR(csr)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := w.ask(t, name, token, csr); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("before the console names the key: %v, want not approved", err)
	}
	if e, ok, _ := w.alloc.LookupEnrollment(t.Context(), name); !ok || e.State != alloc.EnrollPending || e.PoolOrder != "pa" {
		t.Fatalf("the first ask left %+v, %v", e, ok)
	}

	w.src.write("host-pa", hostsource.ConsoleLine("pa", fp))
	w.mu.Lock()
	w.now = w.now.Add(pool.DefaultJudgeEvery)
	w.mu.Unlock()
	cert, err := w.ask(t, name, token, csr)
	if err != nil {
		t.Fatalf("once the console names the key: %v", err)
	}
	leaf, err := wirecert.LeafOf(wirecert.Bundle{CertPEM: cert})
	if err != nil || leaf.Subject.CommonName != name {
		t.Fatalf("the certificate: %v, %v", leaf, err)
	}
	o, err := w.alloc.ReadPoolOrder(t.Context(), "pa")
	if err != nil || o.NodeName != name || o.NodeFingerprint != fp || o.NodeCertSerial != wirecert.Serial(leaf) {
		t.Fatalf("the order after the join: %+v, %v", o, err)
	}
	again, err := w.ask(t, name, token, csr)
	if err != nil || string(again) != string(cert) {
		t.Fatalf("a retry: %v, the same certificate %v", err, string(again) == string(cert))
	}
}

// A HOST WHOSE CONSOLE NAMES ANOTHER KEY IS REFUSED AND ITS ORDER HELD, with nothing
// recorded and the token unspent; the host the console names is then refused too, until
// a person acts.
func TestAPoolHostTheConsoleContradictsHoldsItsOrder(t *testing.T) {
	t.Parallel()
	w := newPoolJoinWire(t)
	token := w.order(t, "pb")
	name := alloc.PoolNodeName("pb")
	csr, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	w.src.write("host-pb", hostsource.ConsoleLine("pb", "SHA256:theHostsOwnKey"))
	if _, err := w.ask(t, name, token, csr); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("a contradicted ask: %v, want refused", err)
	}
	if _, ok, _ := w.alloc.LookupEnrollment(t.Context(), name); ok {
		t.Fatal("a contradicted ask was recorded")
	}
	o, err := w.alloc.ReadPoolOrder(t.Context(), "pb")
	if err != nil || o.JoinHold == "" || o.NodeName != "" {
		t.Fatalf("the order after a contradiction: %+v, %v", o, err)
	}
	tokens, err := w.alloc.JoinTokens(t.Context())
	if err != nil || len(tokens) != 1 || tokens[0].Uses != 1 {
		t.Fatalf("the token after a contradiction: %+v, %v", tokens, err)
	}
}
