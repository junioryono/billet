package nodeplane_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/hostsource"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/wirecert"
)

// poolRig is a bootstrap listener whose ledger and judge are fakes that record what
// they were asked.
type poolRig struct {
	mu         sync.Mutex
	orders     map[string]string // token -> order
	verdict    hostsource.Testimony
	admitErr   error
	known      alloc.Enrollment
	requested  []string
	claims     []hostsource.Claim
	admitted   []alloc.PoolAdmission
	held       []string
	requestErr error
}

func (p *poolRig) PoolJoinOrder(_ context.Context, token string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.orders[token], nil
}

func (p *poolRig) JudgePoolJoin(_ context.Context, c hostsource.Claim) hostsource.Testimony {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.claims = append(p.claims, c)

	return p.verdict
}

func (p *poolRig) AdmitPoolEnrollment(_ context.Context, adm alloc.PoolAdmission) (alloc.Enrollment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.admitted = append(p.admitted, adm)
	if p.admitErr != nil {
		return alloc.Enrollment{}, p.admitErr
	}

	return alloc.Enrollment{Name: alloc.PoolNodeName(adm.Order), Fingerprint: adm.Fingerprint,
		CertPEM: adm.CertPEM, State: alloc.EnrollApproved}, nil
}

func (p *poolRig) HoldPoolJoin(_ context.Context, order, reason string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held = append(p.held, order+": "+reason)

	return nil
}

func (p *poolRig) RequestEnrollmentWithToken(
	_ context.Context, name, fingerprint, csrPEM, _ string,
) (alloc.Enrollment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requested = append(p.requested, name)
	if p.requestErr != nil {
		return alloc.Enrollment{}, p.requestErr
	}

	return alloc.Enrollment{Name: name, Fingerprint: fingerprint, CSRPEM: csrPEM, State: alloc.EnrollPending}, nil
}

func (p *poolRig) LookupEnrollment(_ context.Context, name string) (alloc.Enrollment, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.known, p.known.Name == name, nil
}

func servePool(t *testing.T, rig *poolRig, seesAddress bool) (*httptest.Server, *wirecert.CA) {
	t.Helper()
	log := slog.New(slog.DiscardHandler)
	ca := testAuthority(t)
	srv := httptest.NewServer(nodeplane.BootstrapHandler(log, nodeplane.New(log, deployment, time.Minute), ca,
		nodeplane.WithEnrollment(rig), nodeplane.WithPoolJoins(rig, seesAddress)))
	t.Cleanup(srv.Close)

	return srv, ca
}

// askToJoin posts one enrollment and answers its status and decoded answer.
func askToJoin(t *testing.T, srv *httptest.Server, req nodeapi.EnrollRequest) (int, nodeapi.EnrollResponse) {
	t.Helper()
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/v1/enroll", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	res, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out nodeapi.EnrollResponse
	if res.StatusCode == http.StatusOK {
		if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
	}

	return res.StatusCode, out
}

func poolRequest(t *testing.T, name string) (nodeapi.EnrollRequest, string) {
	t.Helper()
	csr, _, err := wirecert.NewNodeCSR(name)
	if err != nil {
		t.Fatal(err)
	}
	fp, err := wirecert.FingerprintOfCSR(csr)
	if err != nil {
		t.Fatal(err)
	}

	return nodeapi.EnrollRequest{Node: name, CSRPEM: string(csr), JoinToken: "tok-pa",
		IdentityDocument: `{"instanceId":"i-1"}`, IdentitySignature: "c2ln"}, fp
}

// A POOL HOST ITS SOURCE VOUCHES FOR IS ADMITTED ON THE SOURCE'S WORD: the claim
// carries its key, its document and the connection's peer, the admission carries the
// certificate signed for its order's name and that certificate's serial, and the
// answer hands over that certificate and the authority beside it. Behind a router the
// peer is not the host, and the claim carries no address.
func TestAPoolHostItsSourceVouchesForIsAdmitted(t *testing.T) {
	t.Parallel()
	for _, sees := range []bool{true, false} {
		rig := &poolRig{orders: map[string]string{"tok-pa": "pa"},
			verdict: hostsource.Testimony{Verdict: hostsource.VerdictVouches, Reason: "agree"}}
		srv, ca := servePool(t, rig, sees)
		req, fp := poolRequest(t, alloc.PoolNodeName("pa"))
		status, res := askToJoin(t, srv, req)
		if status != http.StatusOK || res.State != alloc.EnrollApproved || res.CertPEM == "" || res.CAPEM == "" {
			t.Fatalf("sees=%v: %d %+v", sees, status, res)
		}
		leaf, err := wirecert.LeafOf(wirecert.Bundle{CertPEM: []byte(res.CertPEM)})
		if err != nil || leaf.Subject.CommonName != alloc.PoolNodeName("pa") {
			t.Fatalf("sees=%v: the certificate handed over: %v, %v", sees, leaf, err)
		}
		if err := leaf.CheckSignatureFrom(mustCert(t, ca.CertPEM())); err != nil {
			t.Fatalf("sees=%v: the certificate is not the deployment's: %v", sees, err)
		}
		if len(rig.claims) != 1 || rig.claims[0].Order != "pa" || rig.claims[0].Fingerprint != fp ||
			string(rig.claims[0].Document) != `{"instanceId":"i-1"}` || rig.claims[0].Signature != "c2ln" {
			t.Fatalf("sees=%v: claims %+v", sees, rig.claims)
		}
		if want := map[bool]string{true: "127.0.0.1", false: ""}[sees]; rig.claims[0].Address != want {
			t.Fatalf("sees=%v: the claim's address is %q, want %q", sees, rig.claims[0].Address, want)
		}
		if len(rig.admitted) != 1 || rig.admitted[0].Serial != wirecert.Serial(leaf) || rig.admitted[0].Token != "tok-pa" ||
			rig.admitted[0].CertPEM != res.CertPEM || rig.admitted[0].Fingerprint != fp {
			t.Fatalf("sees=%v: admitted %+v", sees, rig.admitted)
		}
		if len(rig.requested) != 0 {
			t.Fatalf("sees=%v: a vouched host was also recorded pending: %v", sees, rig.requested)
		}
	}
}

// A CONTRADICTION HOLDS THE ORDER AND SPENDS NOTHING: nothing is recorded or admitted,
// and the host is refused with what its source said.
func TestAContradictionHoldsTheOrderAndRecordsNothing(t *testing.T) {
	t.Parallel()
	rig := &poolRig{orders: map[string]string{"tok-pa": "pa"},
		verdict: hostsource.Testimony{Verdict: hostsource.VerdictContradicts, Reason: "the console names another key"}}
	srv, _ := servePool(t, rig, true)
	req, _ := poolRequest(t, alloc.PoolNodeName("pa"))
	if status, _ := askToJoin(t, srv, req); status != http.StatusForbidden {
		t.Fatalf("a contradicted join answered %d, want 403", status)
	}
	if len(rig.held) != 1 || rig.held[0] != "pa: the console names another key" {
		t.Fatalf("held %v", rig.held)
	}
	if len(rig.requested) != 0 || len(rig.admitted) != 0 {
		t.Fatalf("a contradicted join recorded %v and admitted %v", rig.requested, rig.admitted)
	}
}

// WHAT THE SOURCE CANNOT TELL IS RECORDED PENDING under the ordinary path, for a person
// or the next poll; a token of no order, or a key already decided, asks no source; and
// a pool token under another name is refused before any source is asked.
func TestAPoolJoinFallsToTheOrdinaryPathOnlyWhereItShould(t *testing.T) {
	t.Parallel()
	rig := &poolRig{orders: map[string]string{"tok-pa": "pa"}}
	srv, _ := servePool(t, rig, true)
	req, fp := poolRequest(t, alloc.PoolNodeName("pa"))
	if status, res := askToJoin(t, srv, req); status != http.StatusOK || res.State != alloc.EnrollPending {
		t.Fatalf("could-not-tell answered %d %+v, want pending", status, res)
	}
	if len(rig.claims) != 1 || len(rig.requested) != 1 || len(rig.admitted) != 0 {
		t.Fatalf("could-not-tell: claims %d, requested %v, admitted %d", len(rig.claims), rig.requested, len(rig.admitted))
	}

	rig.known = alloc.Enrollment{Name: alloc.PoolNodeName("pa"), Fingerprint: fp, State: alloc.EnrollApproved}
	askToJoin(t, srv, req)
	if len(rig.claims) != 1 || len(rig.requested) != 2 {
		t.Fatalf("a decided key asked its source: claims %d, requested %v", len(rig.claims), rig.requested)
	}

	plain := req
	plain.JoinToken = "operators-token"
	plain.Node = "epyc-1"
	askToJoin(t, srv, plain)
	if len(rig.claims) != 1 || len(rig.requested) != 3 {
		t.Fatalf("an operator's token asked a source: claims %d", len(rig.claims))
	}

	stolen := req
	stolen.Node = "epyc-1"
	if status, _ := askToJoin(t, srv, stolen); status != http.StatusUnauthorized {
		t.Fatalf("a pool token under another name answered %d, want 401", status)
	}
	if len(rig.claims) != 1 || len(rig.requested) != 3 {
		t.Fatalf("a pool token under another name was judged or recorded: claims %d, requested %v",
			len(rig.claims), rig.requested)
	}
}

// THE LEDGER'S REFUSALS OF A POOL JOIN ARE ANSWERED BY WHAT THE HOST SHOULD DO, on
// either path: an order not yet placed is asked again, a held one and a closed one are
// refused, and a token no longer usable is a credential problem.
func TestAPoolJoinsRefusalsAreAnsweredByWhatTheHostDoes(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"not yet placed": {alloc.ErrPoolOrderNotPlaced, http.StatusServiceUnavailable},
		"held":           {alloc.ErrPoolJoinHeld, http.StatusForbidden},
		"closed":         {alloc.ErrPoolOrderClosed, http.StatusConflict},
		"a conflict":     {alloc.ErrEnrollmentConflict, http.StatusConflict},
		"a spent token":  {alloc.ErrBadJoinToken, http.StatusUnauthorized},
		"anything else":  {fmt.Errorf("database is locked"), http.StatusServiceUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := &poolRig{orders: map[string]string{"tok-pa": "pa"},
				verdict: hostsource.Testimony{Verdict: hostsource.VerdictVouches}, admitErr: c.err}
			srv, _ := servePool(t, rig, true)
			req, _ := poolRequest(t, alloc.PoolNodeName("pa"))
			if status, res := askToJoin(t, srv, req); status != c.want || res.CertPEM != "" {
				t.Fatalf("admission refused with %v answered %d %+v, want %d and no certificate", c.err, status, res, c.want)
			}
		})
	}
	for name, c := range map[string]struct {
		err  error
		want int
	}{
		"not yet placed": {alloc.ErrPoolOrderNotPlaced, http.StatusServiceUnavailable},
		"held":           {alloc.ErrPoolJoinHeld, http.StatusForbidden},
		"closed":         {alloc.ErrPoolOrderClosed, http.StatusConflict},
	} {
		rig := &poolRig{orders: map[string]string{"tok-pa": "pa"}, requestErr: c.err}
		srv, _ := servePool(t, rig, true)
		req, _ := poolRequest(t, alloc.PoolNodeName("pa"))
		if status, _ := askToJoin(t, srv, req); status != c.want {
			t.Errorf("recording refused with %s answered %d, want %d", name, status, c.want)
		}
	}
}

func mustCert(t *testing.T, certPEM []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(certPEM)
	if block == nil {
		t.Fatal("no certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}

	return cert
}
