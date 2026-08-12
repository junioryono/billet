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
}

func (e *enrollments) RequestEnrollment(
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
	if _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", caPEM, csrPEM); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("an unapproved node was admitted: %v", err)
	}

	list.approve(t, ca, "epyc-1")

	certPEM, err := nodeclient.Enroll(t.Context(), base, "epyc-1", caPEM, csrPEM)
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

	if _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", caPEM, csrPEM); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("unexpected: %v", err)
	}

	list.approve(t, ca, "epyc-1")

	certPEM, err := nodeclient.Enroll(t.Context(), base, "epyc-1", caPEM, csrPEM)
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

	if _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", caPEM, first); !errors.Is(err, nodeclient.ErrNotApproved) {
		t.Fatalf("unexpected: %v", err)
	}

	second, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("csr: %v", err)
	}

	if _, err := nodeclient.Enroll(t.Context(), base, "epyc-1", caPEM, second); err == nil {
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
