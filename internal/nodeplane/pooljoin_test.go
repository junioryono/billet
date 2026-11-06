package nodeplane_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
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
// they were asked. Its order "pa" is placed and open unless a test says otherwise.
type poolRig struct {
	mu         sync.Mutex
	orders     map[string]string // token -> order
	spent      map[string]bool   // tokens no longer live
	order      alloc.PoolOrder
	orderErr   error
	tokenErr   error
	lookupErr  error
	verdict    hostsource.Testimony
	admitErr   error
	known      alloc.Enrollment
	requested  []string
	claims     []hostsource.Claim
	admitted   []alloc.PoolAdmission
	held       []string
	requestErr error
	// recordedHeld is each request recorded for a held order, and heldErr its answer.
	recordedHeld []string
	heldErr      error
	heldState    string
}

func (p *poolRig) RecordHeldPoolRequest(_ context.Context, order, fingerprint, _ string) (alloc.Enrollment, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.recordedHeld = append(p.recordedHeld, order+" "+fingerprint)
	if p.heldErr != nil {
		return alloc.Enrollment{}, p.heldErr
	}
	state := p.heldState
	if state == "" {
		state = alloc.EnrollPending
	}

	return alloc.Enrollment{Name: alloc.PoolNodeName(order), Fingerprint: fingerprint, State: state}, nil
}

func newPoolRig(verdict hostsource.Verdict) *poolRig {
	return &poolRig{
		orders:  map[string]string{"tok-pa": "pa"},
		spent:   map[string]bool{},
		order:   alloc.PoolOrder{ID: "pa", State: alloc.PoolPlaced, HostID: "i-1"},
		verdict: hostsource.Testimony{Verdict: verdict, Reason: "the console names i-1's key, 10.0.0.7"},
	}
}

func (p *poolRig) PoolJoinOrder(_ context.Context, token string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	order := p.orders[token]

	return order, order != "" && !p.spent[token], p.tokenErr
}

func (p *poolRig) ReadPoolOrder(_ context.Context, id string) (alloc.PoolOrder, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.orderErr != nil {
		return alloc.PoolOrder{}, p.orderErr
	}
	if id != p.order.ID {
		return alloc.PoolOrder{}, alloc.ErrPoolOrderClosed
	}

	return p.order, nil
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

	return p.known, p.known.Name == name, p.lookupErr
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

// askToJoin posts one enrollment and answers its status, its decoded answer and its
// body as text.
func askToJoin(t *testing.T, srv *httptest.Server, req nodeapi.EnrollRequest) (int, nodeapi.EnrollResponse, string) {
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
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out nodeapi.EnrollResponse
	if res.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
	}

	return res.StatusCode, out, string(raw)
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
		rig := newPoolRig(hostsource.VerdictVouches)
		srv, ca := servePool(t, rig, sees)
		req, fp := poolRequest(t, alloc.PoolNodeName("pa"))
		status, res, _ := askToJoin(t, srv, req)
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

// A CONTRADICTION HOLDS THE ORDER, SPENDS NOTHING AND IS ANSWERED PENDING: the request
// is recorded for a person without its token, the host keeps asking while a person
// looks, and the answer carries none of what the source said.
func TestAContradictionHoldsTheOrderAndAnswersPending(t *testing.T) {
	t.Parallel()
	rig := newPoolRig(hostsource.VerdictContradicts)
	srv, _ := servePool(t, rig, true)
	req, fp := poolRequest(t, alloc.PoolNodeName("pa"))
	status, res, body := askToJoin(t, srv, req)
	if status != http.StatusOK || res.State != alloc.EnrollPending || res.Fingerprint != fp || res.CertPEM != "" {
		t.Fatalf("a contradicted join answered %d %+v", status, res)
	}
	if strings.Contains(body, "i-1") || strings.Contains(body, "10.0.0.7") || strings.Contains(body, "console") {
		t.Fatalf("the answer quotes the source: %s", body)
	}
	if len(rig.held) != 1 || !strings.HasPrefix(rig.held[0], "pa: the console names") {
		t.Fatalf("held %v", rig.held)
	}
	if len(rig.requested) != 0 || len(rig.admitted) != 0 {
		t.Fatalf("a contradicted join spent its token recording %v, or admitted %v", rig.requested, rig.admitted)
	}
	if len(rig.recordedHeld) != 1 || rig.recordedHeld[0] != "pa "+fp {
		t.Fatalf("recorded for a person: %v", rig.recordedHeld)
	}
}

// A HELD ORDER'S REQUEST IS RECORDED FOR A PERSON AND ANSWERED PENDING, its source
// unasked: another key holding the name is pending too, a denied key hears its denial,
// and a ledger that cannot record it says so.
func TestAHeldOrdersRequestIsRecordedForAPerson(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		err    error
		state  string
		status int
		want   string
	}{
		"recorded":         {nil, "", http.StatusOK, alloc.EnrollPending},
		"another key":      {fmt.Errorf("%w: the secret key", alloc.ErrEnrollmentConflict), "", http.StatusOK, alloc.EnrollPending},
		"a denied key":     {nil, alloc.EnrollDenied, http.StatusOK, alloc.EnrollDenied},
		"a ledger failing": {errors.New("the secret reason"), "", http.StatusServiceUnavailable, ""},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newPoolRig(hostsource.VerdictVouches)
			rig.order.JoinHold, rig.heldErr, rig.heldState = "a person looks", c.err, c.state
			rig.spent["tok-pa"] = true
			srv, _ := servePool(t, rig, true)
			req, _ := poolRequest(t, alloc.PoolNodeName("pa"))
			status, res, body := askToJoin(t, srv, req)
			if status != c.status || res.State != c.want || strings.Contains(body, "secret") {
				t.Fatalf("answered %d %+v %s, want %d %q", status, res, body, c.status, c.want)
			}
			if len(rig.recordedHeld) != 1 || len(rig.claims) != 0 || len(rig.requested) != 0 {
				t.Fatalf("recorded %v held, asked the source %d times, spent a token %v",
					rig.recordedHeld, len(rig.claims), rig.requested)
			}
		})
	}
}

// THE LEDGER IS ASKED BEFORE THE SOURCE: a held order is answered pending and asks
// nothing; a closed order, one being ordered, a token neither live nor the token of the
// request already recorded, and a pool token under another name are refused, and none
// of them reaches the source.
func TestTheLedgerIsAskedBeforeTheSource(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		set    func(*poolRig, *nodeapi.EnrollRequest)
		status int
		state  string
		says   string
	}{
		"a joined order": {set: func(p *poolRig, _ *nodeapi.EnrollRequest) { p.order.NodeName = "pool-pa" },
			status: http.StatusConflict, says: "admits no node"},
		"a released order": {set: func(p *poolRig, _ *nodeapi.EnrollRequest) { p.order.State = alloc.PoolReleased },
			status: http.StatusConflict, says: "admits no node"},
		"no such order": {set: func(p *poolRig, _ *nodeapi.EnrollRequest) { p.order.ID = "other" },
			status: http.StatusConflict, says: "admits no node"},
		"an order being ordered": {set: func(p *poolRig, _ *nodeapi.EnrollRequest) { p.order.State = alloc.PoolOrdering },
			status: http.StatusServiceUnavailable, says: "not yet recorded as placed"},
		"a spent token and no request": {set: func(p *poolRig, _ *nodeapi.EnrollRequest) { p.spent["tok-pa"] = true },
			status: http.StatusUnauthorized, says: "join token"},
		"another name": {set: func(_ *poolRig, r *nodeapi.EnrollRequest) { r.Node = "epyc-1" },
			status: http.StatusUnauthorized, says: "join token"},
		"an order that cannot be read": {set: func(p *poolRig, _ *nodeapi.EnrollRequest) { p.orderErr = errors.New("locked") },
			status: http.StatusServiceUnavailable, says: "could not read"},
		"a request that cannot be read": {set: func(p *poolRig, _ *nodeapi.EnrollRequest) { p.lookupErr = errors.New("locked") },
			status: http.StatusServiceUnavailable, says: "could not read"},
		"a token that cannot be read": {set: func(p *poolRig, _ *nodeapi.EnrollRequest) { p.tokenErr = errors.New("locked") },
			status: http.StatusServiceUnavailable, says: "could not read"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig := newPoolRig(hostsource.VerdictVouches)
			srv, _ := servePool(t, rig, true)
			req, _ := poolRequest(t, alloc.PoolNodeName("pa"))
			c.set(rig, &req)
			status, res, body := askToJoin(t, srv, req)
			if status != c.status || res.State != c.state || !strings.Contains(body, c.says) {
				t.Fatalf("answered %d %+v %s, want %d %q saying %q", status, res, body, c.status, c.state, c.says)
			}
			if len(rig.claims) != 0 || len(rig.admitted) != 0 || len(rig.held) != 0 || len(rig.requested) != 0 {
				t.Fatalf("asked the source %d times, admitted %d, held %v, recorded %v",
					len(rig.claims), len(rig.admitted), rig.held, rig.requested)
			}
		})
	}
}

// THE REQUEST ALREADY RECORDED MAY ASK AGAIN WITH ITS SPENT TOKEN, and is judged; any
// other key with that spent token is refused unasked.
func TestASpentTokenAsksOnlyForTheRequestItRecorded(t *testing.T) {
	t.Parallel()
	rig := newPoolRig(hostsource.VerdictVouches)
	srv, _ := servePool(t, rig, true)
	req, fp := poolRequest(t, alloc.PoolNodeName("pa"))
	rig.spent["tok-pa"] = true
	rig.known = alloc.Enrollment{Name: alloc.PoolNodeName("pa"), Fingerprint: fp, State: alloc.EnrollPending, PoolOrder: "pa"}
	if status, res, _ := askToJoin(t, srv, req); status != http.StatusOK || res.State != alloc.EnrollApproved {
		t.Fatalf("the recorded request with its spent token answered %d %+v", status, res)
	}
	other, _ := poolRequest(t, alloc.PoolNodeName("pa"))
	if status, _, _ := askToJoin(t, srv, other); status != http.StatusUnauthorized {
		t.Fatalf("another key with the spent token answered %d", status)
	}
	if len(rig.claims) != 1 {
		t.Fatalf("the source was asked %d times, want once", len(rig.claims))
	}

	// THE SAME KEY RECORDED UNDER NO ORDER is not the order's request, so the spent
	// token asks nothing for it either.
	rig.known.PoolOrder = ""
	if status, _, _ := askToJoin(t, srv, req); status != http.StatusUnauthorized {
		t.Fatalf("a spent token for a request recorded under no order answered %d", status)
	}
	if len(rig.claims) != 1 {
		t.Fatalf("the source was asked %d times, want once", len(rig.claims))
	}
}

// WHAT THE SOURCE CANNOT TELL IS RECORDED PENDING under the ordinary path, for a person
// or the next poll; a token of no order, or a key already decided, asks no source.
func TestAPoolJoinFallsToTheOrdinaryPathOnlyWhereItShould(t *testing.T) {
	t.Parallel()
	rig := newPoolRig(hostsource.VerdictUnknown)
	srv, _ := servePool(t, rig, true)
	req, fp := poolRequest(t, alloc.PoolNodeName("pa"))
	if status, res, _ := askToJoin(t, srv, req); status != http.StatusOK || res.State != alloc.EnrollPending {
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
}

// THE LEDGER'S REFUSALS OF A POOL JOIN ARE ANSWERED BY WHAT THE HOST SHOULD DO, on
// either path: an order not yet placed is asked again, a held order or a conflict waits
// as pending, a closed one is refused, and a token no longer usable is a credential
// problem; none quotes what the ledger said.
func TestAPoolJoinsRefusalsAreAnsweredByWhatTheHostDoes(t *testing.T) {
	t.Parallel()
	type want struct {
		status int
		state  string
		says   string
	}
	cases := map[string]struct {
		err  error
		want want
	}{
		"not yet placed": {alloc.ErrPoolOrderNotPlaced, want{http.StatusServiceUnavailable, "", "not yet recorded as placed"}},
		"held":           {fmt.Errorf("%w: the secret reason", alloc.ErrPoolJoinHeld), want{http.StatusOK, alloc.EnrollPending, ""}},
		"closed":         {fmt.Errorf("%w: the secret reason", alloc.ErrPoolOrderClosed), want{http.StatusConflict, "", "admits no node"}},
		"a spent token":  {alloc.ErrBadJoinToken, want{http.StatusUnauthorized, "", "join token"}},
		"a conflict":     {fmt.Errorf("%w: the secret key", alloc.ErrEnrollmentConflict), want{http.StatusOK, alloc.EnrollPending, ""}},
	}
	for name, c := range cases {
		t.Run("recording "+name, func(t *testing.T) {
			t.Parallel()
			rig := newPoolRig(hostsource.VerdictUnknown)
			rig.requestErr = c.err
			srv, _ := servePool(t, rig, true)
			req, _ := poolRequest(t, alloc.PoolNodeName("pa"))
			status, res, body := askToJoin(t, srv, req)
			if status != c.want.status || res.State != c.want.state || !strings.Contains(body, c.want.says) ||
				strings.Contains(body, "secret") {
				t.Fatalf("answered %d %+v %s, want %+v", status, res, body, c.want)
			}
		})
	}
	cases["anything else"] = struct {
		err  error
		want want
	}{errors.New("the secret reason"), want{http.StatusServiceUnavailable, "", "could not record"}}
	for name, c := range cases {
		t.Run("admitting "+name, func(t *testing.T) {
			t.Parallel()
			rig := newPoolRig(hostsource.VerdictVouches)
			rig.admitErr = c.err
			srv, _ := servePool(t, rig, true)
			req, _ := poolRequest(t, alloc.PoolNodeName("pa"))
			status, res, body := askToJoin(t, srv, req)
			if status != c.want.status || res.State != c.want.state || res.CertPEM != "" ||
				!strings.Contains(body, c.want.says) || strings.Contains(body, "secret") {
				t.Fatalf("answered %d %+v %s, want %+v and no certificate", status, res, body, c.want)
			}
		})
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
