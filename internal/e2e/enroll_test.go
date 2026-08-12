package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/wirecert"
)

// enrollments is an in-memory version of the ledger's enrollment table.
type enrollments struct {
	mu sync.Mutex
	by map[string]alloc.Enrollment

	// spendErr stands in for a ledger that cannot answer, which is a different
	// thing from a credential that is not good.
	spendErr error
}

// RequestEnrollmentWithToken mirrors the real one: the token is charged only for
// a request that is new, and both facts land together.
func (e *enrollments) RequestEnrollmentWithToken(
	ctx context.Context, name, fingerprint, csrPEM, token string,
) (alloc.Enrollment, error) {
	e.mu.Lock()
	existing, known := e.by[name]
	e.mu.Unlock()

	if !known || existing.Fingerprint != fingerprint {
		if err := e.spendJoinToken(token); err != nil {
			return alloc.Enrollment{}, err
		}
	}

	return e.requestEnrollment(ctx, name, fingerprint, csrPEM)
}

func (e *enrollments) requestEnrollment(
	_ context.Context, name, fingerprint, csrPEM string,
) (alloc.Enrollment, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.by == nil {
		e.by = map[string]alloc.Enrollment{}
	}

	if existing, ok := e.by[name]; ok {
		if existing.Fingerprint != fingerprint {
			return alloc.Enrollment{}, alloc.ErrEnrollmentConflict
		}

		return existing, nil
	}

	rec := alloc.Enrollment{
		Name: name, Fingerprint: fingerprint, CSRPEM: csrPEM, State: alloc.EnrollPending,
	}
	e.by[name] = rec

	return rec, nil
}

func (e *enrollments) LookupEnrollment(_ context.Context, name string) (alloc.Enrollment, bool, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	rec, ok := e.by[name]

	return rec, ok, nil
}

// SpendJoinToken accepts anything non-empty: what these tests are about is the
// enrollment handshake, and the token's own rules have their own test.
func (e *enrollments) spendJoinToken(token string) error {
	if e.spendErr != nil {
		return e.spendErr
	}

	if token == "" {
		return alloc.ErrBadJoinToken
	}

	return nil
}

func (e *enrollments) approve(t *testing.T, ca *wirecert.CA, name string) {
	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()

	rec := e.by[name]

	bundle, err := ca.SignNodeCSR(name, []byte(rec.CSRPEM))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	rec.State, rec.CertPEM = alloc.EnrollApproved, string(bundle.CertPEM)
	e.by[name] = rec
}

// enrollableWire is a control plane that admits new machines.
func enrollableWire(t *testing.T) (*wirecert.CA, string, *enrollments) {
	t.Helper()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), wireDeployment)
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(serving)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	log := slog.New(slog.DiscardHandler)
	list := &enrollments{}

	plane := nodeplane.New(log, wireDeployment, time.Minute,
		nodeplane.WithTierCatalog([]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
		}}))

	srv := httptest.NewUnstartedServer(
		nodeplane.Handler(log, plane, mtlsStore{}, alwaysMints{},
			nodeplane.RequireClientCert(),
			nodeplane.WithRenewal(ca),
			nodeplane.WithEnrollment(list)))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	return ca, srv.URL, list
}

// A NODE THAT HAS NOTHING CAN ASK, AND ASKING GRANTS NOTHING.
//
// Admission used to be entirely out of band: an operator ran `billet ca issue`
// and copied a bundle to the machine. That works and it is not discoverable — a
// node powered on and pointed at a control plane appeared nowhere, so the
// operator had to already know it existed.
//
// Now it asks, waits, and is admitted only once a human has compared the
// fingerprint it printed against the one the control plane shows.
func TestAnEnrollingNodeWaitsForApproval(t *testing.T) {
	t.Parallel()

	ca, base, list := enrollableWire(t)

	caPEM, deployment, err := nodeclient.FetchCA(t.Context(), base, ca.Fingerprint())
	if err != nil {
		t.Fatalf("fetch the authority: %v", err)
	}

	if deployment != wireDeployment {
		t.Errorf("the control plane reported deployment %q, want %q", deployment, wireDeployment)
	}

	csrPEM, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("csr: %v", err)
	}

	// PENDING, not admitted. Nothing has decided yet.
	if _, _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", "a-token", caPEM, csrPEM); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("an unapproved node was admitted: %v", err)
	}

	list.approve(t, ca, "epyc-1")

	certPEM, _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", "a-token", caPEM, csrPEM)
	if err != nil {
		t.Fatalf("an approved node was not admitted: %v", err)
	}

	got, err := wirecert.LeafOf(wirecert.Bundle{CertPEM: certPEM})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got.Subject.CommonName != "epyc-1" {
		t.Errorf("the certificate names %q, want %q", got.Subject.CommonName, "epyc-1")
	}
}

// THE FINGERPRINT THE OPERATOR COMPARES IS THE ONE THE CERTIFICATE CARRIES.
//
// The whole flow rests on two ends displaying the same number. If signing
// changed it, the value an operator compared would describe nothing, and the
// approval would be theatre.
func TestTheApprovedFingerprintSurvivesSigning(t *testing.T) {
	t.Parallel()

	ca, base, list := enrollableWire(t)

	caPEM, _, err := nodeclient.FetchCA(t.Context(), base, ca.Fingerprint())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	csrPEM, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("csr: %v", err)
	}

	asked, err := wirecert.FingerprintOfCSR(csrPEM)
	if err != nil {
		t.Fatalf("fingerprint: %v", err)
	}

	if _, _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", "a-token", caPEM, csrPEM); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("unexpected: %v", err)
	}

	list.approve(t, ca, "epyc-1")

	certPEM, _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", "a-token", caPEM, csrPEM)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	leaf, err := wirecert.LeafOf(wirecert.Bundle{CertPEM: certPEM})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if got := wirecert.FingerprintOfCert(leaf); got != asked {
		t.Errorf("the node showed %s and its certificate carries %s; the value the operator "+
			"compared describes nothing", asked, got)
	}
}

// A NODE WILL NOT ENROLL WITH SOMETHING IT CANNOT IDENTIFY.
//
// The first connection has no authority to verify against — that is what it is
// fetching — so the fingerprint is the only thing standing between this node and
// whatever answered. Accepting on first use would be trust with no verification:
// an attacker answers first, the node enrolls with them, and every job it runs
// afterwards is theirs.
func TestEnrollingRefusesAnUnverifiedControlPlane(t *testing.T) {
	t.Parallel()

	_, base, _ := enrollableWire(t)

	if _, _, err := nodeclient.FetchCA(t.Context(), base, ""); err == nil {
		t.Fatal("a node fetched an authority without being told which one to expect")
	} else if !strings.Contains(err.Error(), "ca-fingerprint") {
		t.Errorf("the refusal does not say what to supply: %v", err)
	}

	_, _, err := nodeclient.FetchCA(t.Context(), base, "SHA256:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err == nil {
		t.Fatal("a node accepted an authority whose fingerprint it was not expecting")
	}

	if !strings.Contains(err.Error(), "not the control plane you meant") {
		t.Errorf("the refusal does not say what it means: %v", err)
	}
}

// A NAME IS CLAIMED BY THE FIRST KEY TO ASK.
//
// Overwriting would lose the property approval exists for: an operator who
// compared a fingerprint yesterday would be approving a different machine today,
// under a name they already trust.
func TestASecondKeyCannotTakeAClaimedName(t *testing.T) {
	t.Parallel()

	ca, base, _ := enrollableWire(t)

	caPEM, _, err := nodeclient.FetchCA(t.Context(), base, ca.Fingerprint())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	first, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("csr: %v", err)
	}

	if _, _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", "a-token", caPEM, first); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("unexpected: %v", err)
	}

	second, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("csr: %v", err)
	}

	if _, _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", "a-token", caPEM, second); err == nil {
		t.Fatal("a second key took a name that was already claimed")
	}
}

// RELAXING THE HANDSHAKE MUST NOT OPEN ANYTHING ELSE.
//
// The listener verifies a certificate if one is given but no longer requires
// one, so that a machine with nothing can reach /v1/enroll and /v1/ca. That is a
// deliberate hole in exactly two places, and this is the test that says so: every
// other route must still refuse a connection that proved nothing.
//
// Without this the change is indistinguishable from removing authentication:
// registration is what puts a node in the fleet, and everything downstream
// trusts that membership.
func TestAnUnenrolledConnectionCanReachNothingElse(t *testing.T) {
	t.Parallel()

	ca, base, _ := enrollableWire(t)

	// No client certificate at all — an unenrolled machine.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("parse the authority")
	}

	client := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS13},
	}}

	for _, route := range []struct {
		method, path string
	}{
		{http.MethodPost, "/v1/register"},
		{http.MethodPost, "/v1/nodes/epyc-1/poll"},
		{http.MethodPost, "/v1/nodes/epyc-1/result"},
		{http.MethodPost, "/v1/nodes/epyc-1/jit"},
		{http.MethodPost, "/v1/nodes/epyc-1/renew"},
		{http.MethodGet, "/v1/nodes/epyc-1/launched"},
		{http.MethodPost, "/v1/nodes/epyc-1/leases/l1/bind"},
	} {
		req, err := http.NewRequestWithContext(t.Context(), route.method, base+route.path,
			strings.NewReader(`{"node":"epyc-1","version":5}`))
		if err != nil {
			t.Fatalf("build %s: %v", route.path, err)
		}

		res, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", route.path, err)
		}

		body, readErr := io.ReadAll(res.Body)
		res.Body.Close()

		if readErr != nil {
			t.Fatalf("read %s: %v", route.path, readErr)
		}

		if res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s answered %s to a connection with no certificate; want 401.\n%s",
				route.method, route.path, res.Status, body)
		}
	}
}

// ENROLLING NEEDS A CREDENTIAL, even though approval is what admits.
//
// Approval cannot be tricked — an operator matches a fingerprint against what
// the node printed — but an unauthenticated endpoint lets anyone who can reach
// the port fill the pending list with plausible entries, and TAKE A NAME before
// the machine that should have it. "First key claims the name" protects an
// operator from approving a substitute; without a credential in front of it, it
// also lets a stranger deny a machine its own name.
func TestEnrollingNeedsAJoinToken(t *testing.T) {
	t.Parallel()

	ca, base, _ := enrollableWire(t)

	caPEM, _, err := nodeclient.FetchCA(t.Context(), base, ca.Fingerprint())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	csrPEM, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("csr: %v", err)
	}

	_, _, err = nodeclient.Enroll(t.Context(), base, "epyc-1", "", caPEM, csrPEM)
	if err == nil {
		t.Fatal("a machine with no join token was allowed to claim a name")
	}

	if !strings.Contains(err.Error(), "join token") {
		t.Errorf("the refusal does not say what is missing: %v", err)
	}
}

// A LEDGER THAT CANNOT ANSWER IS NOT A CREDENTIAL THAT IS NOT GOOD.
//
// Both used to come back as "enrolling needs a join token", and the underlying
// error was not logged at all — the warning carried the node and the fingerprint
// and nothing about what actually failed. An operator meeting a locked database
// during a fleet build-out is told to run `billet ca token`, does, watches the
// fresh token fail the same way, and has nothing anywhere saying why.
//
// The node acts on the difference too: unauthenticated is a verdict and stops
// it, unavailable is an outage and it keeps asking.
func TestALedgerOutageIsNotReportedAsAMissingJoinToken(t *testing.T) {
	t.Parallel()

	ca, base, list := enrollableWire(t)

	list.mu.Lock()
	list.spendErr = errors.New("database is locked")
	list.mu.Unlock()

	caPEM, _, err := nodeclient.FetchCA(t.Context(), base, ca.Fingerprint())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	csrPEM, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("csr: %v", err)
	}

	_, _, err = nodeclient.Enroll(t.Context(), base, "epyc-1", "a-token", caPEM, csrPEM)
	if err == nil {
		t.Fatal("enrolling succeeded while the ledger could not answer")
	}

	if strings.Contains(err.Error(), "run `billet ca token`") {
		t.Errorf("a ledger outage was reported as a missing credential, which sends an "+
			"operator to mint tokens that cannot help: %v", err)
	}
}

// ENROLLMENT THAT SPANS A ROTATION INSTALLS THE AUTHORITY IT WAS SIGNED BY.
//
// The node pins the bootstrap authority, prints its fingerprint, and waits for a
// human. That wait is unbounded by design, so an operator rotating the
// deployment's CA in the middle of it is an ordinary sequence, not a contrived
// one — approval then signs the CSR with the NEW authority.
//
// The server gets this right: it answers with a trust bundle holding both. The
// client threw it away and returned only the certificate, so the caller wrote
// the bootstrap authority it had been holding since before the rotation. The
// node's own certificate does not chain to its own ca.crt, and it cannot start —
// the one outcome enrollment exists to avoid, arriving at the end of a
// successful approval.
func TestEnrollmentAcrossARotationWritesTheAuthorityThatSignedIt(t *testing.T) {
	t.Parallel()

	// The authority the node bootstraps against, and the one a rotation moves to.
	oldCA, err := wirecert.LoadOrCreateCA(t.TempDir(), wireDeployment)
	if err != nil {
		t.Fatalf("create the old authority: %v", err)
	}

	newCA, err := wirecert.LoadOrCreateCA(t.TempDir(), wireDeployment)
	if err != nil {
		t.Fatalf("create the new authority: %v", err)
	}

	// Mid-overlap: the OLD authority still signs what the server presents, and
	// both are trusted for clients.
	serving, err := oldCA.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(serving)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	trust := append(append([]byte(nil), newCA.CertPEM()...), oldCA.CertPEM()...)

	log := slog.New(slog.DiscardHandler)
	list := &enrollments{}

	plane := nodeplane.New(log, wireDeployment, time.Minute)

	srv := httptest.NewUnstartedServer(
		nodeplane.Handler(log, plane, mtlsStore{}, alwaysMints{},
			nodeplane.RequireClientCert(),
			nodeplane.WithRenewal(newCA),
			nodeplane.WithTrustBundle(trust),
			nodeplane.WithEnrollment(list)))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	// WHAT THE NODE IS HOLDING WHEN THE ROTATION HAPPENS. It fetched and pinned
	// the authority before any of this, which is the only CA it knows.
	caPEM := oldCA.CertPEM()

	csrPEM, keyPEM, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("csr: %v", err)
	}

	if _, _, err := nodeclient.Enroll(t.Context(), srv.URL, "epyc-1", "a-token", caPEM, csrPEM); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("unexpected: %v", err)
	}

	// The rotation happens while the operator is deciding, so approval signs with
	// the new authority.
	list.approve(t, newCA, "epyc-1")

	certPEM, installCA, err := nodeclient.Enroll(t.Context(), srv.URL, "epyc-1", "a-token", caPEM, csrPEM)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}

	// WHAT THE NODE WOULD WRITE HAS TO WORK. Loading it is the same thing the
	// node does on its next start, and it is where the old behaviour died.
	if _, err := wirecert.ClientTLS(wirecert.Bundle{
		CertPEM: certPEM, KeyPEM: keyPEM, CAPEM: installCA,
	}); err != nil {
		t.Fatalf("the bundle this node would install does not verify against itself, so it "+
			"cannot start: %v", err)
	}
}
