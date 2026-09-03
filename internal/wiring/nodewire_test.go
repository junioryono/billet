package wiring_test

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/wirecert"
	"github.com/junioryono/billet/internal/wiring"
)

const wireDeployment = "0123456789abcdef0123456789abcdef"

// A ROTATION IS AN OVERLAP, AND THE NODE WIRE IS WHERE THAT EITHER WORKS OR
// TAKES THE FLEET OFF THE AIR.
//
// The three answers the wire has to get right are each correct in wirecert on
// their own, and the seam that USES them lived in cmd/billet/main.go — excluded
// from coverage, six collaborators to stand up. So nothing asserted that the
// server hands nodes the whole trust bundle, and a mutant that handed out the
// issuing authority alone survived the whole suite. Everything below drives the
// artefacts BuildNodeWire returns, because that is what production installs.

// rotatedDeployment is a state directory mid-overlap, plus both authorities.
func rotatedDeployment(t *testing.T) (dir string, old, fresh *wirecert.CA) {
	t.Helper()

	dir = t.TempDir()

	old, err := wirecert.LoadOrCreateCA(dir, wireDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	fresh, err = wirecert.Rotate(dir, wireDeployment)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}

	return dir, old, fresh
}

// enrollments is a stand-in that records nothing and judges nothing.
//
// A FAKE THAT DECIDES NOTHING, deliberately: what these tests are about is which
// options production installs, and a fake that reimplemented the routes' own
// rules would be the thing under assertion instead of the wire.
type enrollments struct{ asked int }

func (e *enrollments) RequestEnrollmentWithToken(
	context.Context, string, string, string, string,
) (alloc.Enrollment, error) {
	e.asked++

	return alloc.Enrollment{State: "pending"}, nil
}

func (e *enrollments) LookupEnrollment(context.Context, string) (alloc.Enrollment, bool, error) {
	return alloc.Enrollment{}, false, nil
}

func buildWire(t *testing.T, dir string) wiring.NodeWire {
	t.Helper()

	return buildWireWith(t, dir, &enrollments{})
}

func buildWireWith(t *testing.T, dir string, enroll nodeplane.Enrollments) wiring.NodeWire {
	t.Helper()

	wire, err := wiring.BuildNodeWire(wiring.NodeWireRequest{
		StateDir:    dir,
		Deployment:  wireDeployment,
		Hosts:       []string{"127.0.0.1"},
		Log:         slog.New(slog.DiscardHandler),
		Plane:       nodeplane.New(slog.New(slog.DiscardHandler), wireDeployment, time.Minute),
		Enrollments: enroll,
	})
	if err != nil {
		t.Fatalf("BuildNodeWire: %v", err)
	}

	return wire
}

// TestTheWireAcceptsNodesFromEitherGenerationDuringAnOverlap is the assertion
// the surviving mutant needed.
//
// ClientCAs is built from the trust bundle, and during an overlap a node that
// has not renewed still holds a certificate from the PREVIOUS authority. Hand
// the listener the issuing authority alone and every one of those handshakes
// fails — which is the entire fleet, over the wire it would need to renew.
func TestTheWireAcceptsNodesFromEitherGenerationDuringAnOverlap(t *testing.T) {
	t.Parallel()

	dir, old, fresh := rotatedDeployment(t)
	wire := buildWire(t, dir)

	if wire.TLS.ClientCAs == nil {
		t.Fatal("the listener verifies client certificates against nothing")
	}

	for name, ca := range map[string]*wirecert.CA{"before the rotation": old, "after it": fresh} {
		issued, err := ca.IssueNode("epyc-1")
		if err != nil {
			t.Fatalf("%s: issue: %v", name, err)
		}

		leaf, err := wirecert.LeafOf(issued)
		if err != nil {
			t.Fatalf("%s: parse: %v", name, err)
		}

		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots:     wire.TLS.ClientCAs,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			t.Errorf("a node enrolled %s cannot complete a handshake with this listener, so "+
				"it drops out of the fleet: %v", name, err)
		}
	}
}

// TestTheWirePresentsWhatTheUnrenewedFleetCanVerify is the other half of the
// same moment.
//
// A node trusts the authority it was given, so during an overlap the server has
// to present a certificate from the PREVIOUS one. Presenting the new authority
// is unrecoverable remotely: the wire a node would use to renew is the wire it
// can no longer verify.
func TestTheWirePresentsWhatTheUnrenewedFleetCanVerify(t *testing.T) {
	t.Parallel()

	dir, old, _ := rotatedDeployment(t)
	wire := buildWire(t, dir)

	if len(wire.TLS.Certificates) != 1 {
		t.Fatalf("the listener presents %d certificates, want 1", len(wire.TLS.Certificates))
	}

	served, err := x509.ParseCertificate(wire.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse what the listener serves: %v", err)
	}

	// AGAINST THE OLD AUTHORITY ALONE, because that is all an un-renewed node
	// has. Verifying against the whole bundle would pass either way.
	oldOnly := x509.NewCertPool()
	if !oldOnly.AppendCertsFromPEM(old.CertPEM()) {
		t.Fatal("parse the old authority")
	}

	if _, err := served.Verify(x509.VerifyOptions{
		DNSName:   "",
		Roots:     oldOnly,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("a node that has not renewed cannot verify this control plane: %v", err)
	}
}

// TestTheWireHandsNodesTheWholeTrustBundleAndTheIssuingAuthority drives the real
// handler, because the options are only worth what the routes make of them.
//
// GET /v1/ca is what an enrolling node reads and what a renewal answer carries,
// and it reaches BOTH of the options that carry an authority: the fingerprint
// comes from the renewal CA and the PEM from the trust bundle. So one request
// says whether renewal signs with the new authority and whether a node is told
// to trust both.
func TestTheWireHandsNodesTheWholeTrustBundleAndTheIssuingAuthority(t *testing.T) {
	t.Parallel()

	dir, old, fresh := rotatedDeployment(t)
	wire := buildWire(t, dir)

	// THE BOOTSTRAP HANDLER THE WIRE BUILT, not one this test assembled from its
	// pieces. These two routes moved to a listener of their own so an anonymous
	// caller cannot spend the fleet's connection budget, which gave the authority
	// a second consumer — and assembling it here would leave production free to
	// build that one from a different read.
	srv := httptest.NewServer(wire.Bootstrap)
	t.Cleanup(srv.Close)

	res, err := http.Get(srv.URL + "/v1/ca") //nolint:noctx // httptest server in a test
	if err != nil {
		t.Fatalf("GET /v1/ca: %v", err)
	}

	t.Cleanup(func() { _ = res.Body.Close() })

	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/ca = %d; the wire carries no authority at all", res.StatusCode)
	}

	var got struct {
		CAPEM       string `json:"ca_pem"`
		Fingerprint string `json:"fingerprint"`
	}

	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// THE ISSUING AUTHORITY SIGNS RENEWALS, and that is the only way the new
	// authority reaches the fleet. Signing with the one being retired means no
	// node ever adopts it and `billet ca retire` strands all of them.
	if got.Fingerprint != fresh.Fingerprint() {
		t.Errorf("the wire names %s as its authority; renewals must be signed by the new one, "+
			"%s, or no node ever adopts it", got.Fingerprint, fresh.Fingerprint())
	}

	// AND THE WHOLE BUNDLE, not the issuer alone. A node given only the new
	// authority cannot verify the control plane it was just handed it by, which
	// is presenting the old one.
	for name, ca := range map[string]*wirecert.CA{"previous": old, "new": fresh} {
		if !strings.Contains(got.CAPEM, string(ca.CertPEM())) {
			t.Errorf("the %s authority is missing from the trust bundle the wire hands out", name)
		}
	}
}

// TestTheWireCarriesNoOverlapWhenNoRotationIsRunning is the other direction:
// every assertion above would pass against a wire that reported an overlap
// permanently.
func TestTheWireCarriesNoOverlapWhenNoRotationIsRunning(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	ca, err := wirecert.LoadOrCreateCA(dir, wireDeployment)
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	wire := buildWire(t, dir)

	if wire.Rotating {
		t.Error("a deployment that has never rotated reports an overlap")
	}

	if !wire.IssuingExpiry.Equal(ca.NotAfter()) {
		t.Errorf("it reports the authority expiring at %s rather than %s",
			wire.IssuingExpiry, ca.NotAfter())
	}

	served, err := x509.ParseCertificate(wire.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse what the listener serves: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("parse the authority")
	}

	if _, err := served.Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("the only authority there is cannot verify what the listener serves: %v", err)
	}
}

// TestTheWireRefusesAnAnonymousCallerOnEverythingButTheTwoWayIn covers
// RequireClientCert, which no assertion here reached before.
//
// The two routes a machine with no certificate needs are on a listener of their
// own now, so this one CAN require a certificate at the handshake — and still
// the handler is what decides which node a request is FOR, by the name in the
// certificate rather than the one in the path. An option missing from the wire
// is a caller acting for a host it is not, and nothing at the TLS layer would
// notice: a valid certificate for any node satisfies the handshake.
func TestTheWireRefusesAnAnonymousCallerOnEverythingButTheTwoWayIn(t *testing.T) {
	t.Parallel()

	dir, _, _ := rotatedDeployment(t)
	wire := buildWire(t, dir)

	srv := httptest.NewServer(wire.Handler)
	t.Cleanup(srv.Close)

	// No TLS at all, which is the strongest form of "no verified chain".
	res, err := http.Post(srv.URL+"/v1/nodes/epyc-1/poll", "application/json", //nolint:noctx // httptest
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST poll: %v", err)
	}

	t.Cleanup(func() { _ = res.Body.Close() })

	// THE SPECIFIC REFUSAL, NOT MERELY A NON-200. Without the guard this route
	// still answers something other than 200 — a poll with an empty body fails
	// for its own reasons — so "an error came back" is satisfied by the wire
	// being built wrong, which is the assertion this test exists to make.
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("an anonymous caller got %d from an authenticated route; the wire was built "+
			"without the guard that decides which node a request is from", res.StatusCode)
	}

	var body struct {
		Code string `json:"code"`
	}

	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if body.Code != "unauthenticated" {
		t.Errorf("the refusal is %q; it must be the one that says no verified chain was "+
			"presented", body.Code)
	}
}

// TestTheWireLetsAMachineWithNothingAskToJoin is the other direction, and the
// one a guard that refused everything would fail.
//
// Enrollment is the way in for a machine that holds no certificate yet. Left out
// of the wire, the route answers as unconfigured and a new host cannot join at
// all — which is silent, because nothing else about the deployment changes.
func TestTheWireLetsAMachineWithNothingAskToJoin(t *testing.T) {
	t.Parallel()

	dir, _, _ := rotatedDeployment(t)

	asked := &enrollments{}
	wire := buildWireWith(t, dir, asked)

	srv := httptest.NewServer(wire.Bootstrap)
	t.Cleanup(srv.Close)

	// A REAL CERTIFICATE REQUEST, because the route validates one before it
	// reaches the store. The first version of this test sent an empty CSR and
	// then accepted "the store was never called, and the answer was 400" —
	// which is exactly what a wire built with NO enrollment option produces, so
	// the assertion agreed with the bug.
	csrPEM, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("NewNodeCSR: %v", err)
	}

	// THE WIRE'S OWN TYPE, not an object written from memory: the wire decodes
	// with DisallowUnknownFields, so a field named wrongly is a 400 that looks
	// exactly like the failure this test exists to tell apart.
	body, err := json.Marshal(nodeapi.EnrollRequest{
		Node:      "epyc-1",
		CSRPEM:    string(csrPEM),
		JoinToken: "a-join-token",
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	res, err := http.Post(srv.URL+"/v1/enroll", "application/json", //nolint:noctx // httptest
		bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST enroll: %v", err)
	}

	t.Cleanup(func() { _ = res.Body.Close() })

	// THAT THE STORE WAS REACHED, exactly once. What it decided is its own
	// business and is tested where it lives; the difference this test exists for
	// is whether the request got that far at all.
	if asked.asked != 1 {
		t.Errorf("the enrollment store was asked %d times (status %d); a machine holding "+
			"nothing cannot join a wire built without a way in", asked.asked, res.StatusCode)
	}
}

// TestALoopbackWireHasNoCertificatesAndAsksForNoAuthority is the default
// single-machine deployment, and the one this refactor could have broken
// silently for everybody.
//
// Two processes on one box: the trust boundary is the MACHINE, so a loopback
// server serves plain HTTP and config validation refuses node.tls against one.
// The condition that decides it moved out of cmd/billet, so an inverted test or
// an authority read that is no longer skipped would make every such install fail
// to start — and there is no authority to read on a host that has never needed
// one.
func TestALoopbackWireHasNoCertificatesAndAsksForNoAuthority(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// THE AUTHORITY PATH IS POISONED RATHER THAN MERELY ABSENT. Checking
	// afterwards that the directory is still missing only catches a path that
	// CREATES one — a read of an absent file leaves nothing behind and would go
	// unnoticed. A regular file where the ca directory belongs makes every access
	// under it fail with ENOTDIR, so a loopback wire that builds at all is one
	// that did not look.
	if err := os.WriteFile(wirecert.CADir(dir), []byte("not a directory\n"), 0o600); err != nil {
		t.Fatalf("poison the authority path: %v", err)
	}

	policy := &cachePolicy{}

	wire, err := wiring.BuildNodeWire(wiring.NodeWireRequest{
		StateDir:    dir,
		Deployment:  wireDeployment,
		Loopback:    true,
		Log:         slog.New(slog.DiscardHandler),
		Plane:       nodeplane.New(slog.New(slog.DiscardHandler), wireDeployment, time.Minute),
		CachePolicy: policy,
	})
	if err != nil {
		t.Fatalf("a loopback wire could not be built: %v", err)
	}

	if wire.TLS != nil {
		t.Error("a loopback wire carries a TLS config; there is nothing between two processes " +
			"on one machine to authenticate")
	}

	if wire.Rotating {
		t.Error("a loopback wire reports a rotation")
	}

	// AND NOTHING TO ENROLL INTO. There are no certificates here, so there is no
	// machine that could be "outside" holding none — serving the enrollment
	// routes would be admitting strangers to a wire whose whole boundary is that
	// only this machine can reach it.
	if wire.Bootstrap != nil {
		t.Error("a loopback wire carries an enrollment handler")
	}

	// AND ITS ROUTES ARE OPEN, because the machine is the boundary. A loopback
	// wire built with the certificate guard would refuse its own node, which is
	// the other process in the same install.
	srv := httptest.NewServer(wire.Handler)
	t.Cleanup(srv.Close)

	res, err := http.Post(srv.URL+"/v1/nodes/epyc-1/poll", "application/json", //nolint:noctx // httptest
		strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST poll: %v", err)
	}

	t.Cleanup(func() { _ = res.Body.Close() })

	// THE CODE, NOT THE STATUS, because two different refusals share 401 here: an
	// unregistered node — which this one is, and which is the right answer — and
	// a wire demanding a certificate. Asserting the status alone would have this
	// test pass against exactly the wire it exists to reject.
	var body struct {
		Code string `json:"code"`
	}

	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// THE EXACT REFUSAL. "anything but unauthenticated" is satisfied by every
	// other way this could go wrong, and what this test needs to show is that the
	// request got PAST the certificate guard and reached the plane, which then
	// answered about a node it has never seen.
	if body.Code != nodeapi.CodeUnregistered {
		t.Errorf("a loopback poll was refused as %q; it must reach the plane, which is what a "+
			"wire with no certificate guard does", body.Code)
	}

	// AND THE CACHE KILL SWITCH IS INSTALLED, which is the one option the
	// loopback wire has ever had. Moving it inside the authenticated branch would
	// take interception control away from every single-machine deployment, and
	// nothing else here would notice.
	res, err = http.Get(srv.URL + "/v1/nodes/epyc-1/cache-policy?owner=acme&repository=x") //nolint:noctx // httptest
	if err != nil {
		t.Fatalf("GET cache-policy: %v", err)
	}

	t.Cleanup(func() { _ = res.Body.Close() })

	if policy.asked == 0 {
		t.Errorf("the cache policy was never consulted (status %d), so a loopback deployment "+
			"has no interception kill switch", res.StatusCode)
	}
}

// cachePolicy records that it was asked, and decides nothing.
type cachePolicy struct{ asked int }

func (c *cachePolicy) ActionsCacheAllowed(context.Context, string, string) (bool, error) {
	c.asked++

	return true, nil
}

// TestARenewalOverTheWireCarriesTheFleetOntoTheNewAuthority is the half of a
// rotation that actually moves it.
//
// A node adopts the new authority by RENEWING, and nothing else does it: the
// server keeps presenting the old one until every host has, which is what makes
// `billet ca retire` safe to run afterwards. So the operational wire has to sign
// a renewal with the ISSUING authority and hand back a bundle carrying BOTH — a
// renewal signed by the authority being retired means no node ever moves, the
// overlap never ends, and retiring takes the whole fleet off the wire.
//
// OVER A REAL HANDSHAKE, because the route is behind the certificate guard and
// there is no other way to reach it. The client presents a certificate from the
// OLD authority, which is exactly what an un-renewed node has.
func TestARenewalOverTheWireCarriesTheFleetOntoTheNewAuthority(t *testing.T) {
	t.Parallel()

	dir, old, fresh := rotatedDeployment(t)
	wire := buildWire(t, dir)

	// The node as it stands before renewing: enrolled under the old authority.
	enrolled, err := old.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	srv := httptest.NewUnstartedServer(wire.Handler)
	srv.TLS = wire.TLS
	srv.StartTLS()
	t.Cleanup(srv.Close)

	client := clientFor(t, enrolled, wire.Serving.CAPEM)

	csrPEM, _, err := wirecert.NewNodeCSR("epyc-1")
	if err != nil {
		t.Fatalf("NewNodeCSR: %v", err)
	}

	body, err := json.Marshal(nodeapi.RenewRequest{CSRPEM: string(csrPEM)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		srv.URL+"/v1/nodes/epyc-1/renew", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}

	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST renew: %v", err)
	}

	t.Cleanup(func() { _ = res.Body.Close() })

	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST renew = %d; a node holding a certificate from the previous authority "+
			"could not renew, which is the only way it ever adopts the new one", res.StatusCode)
	}

	var got struct {
		CertPEM string `json:"cert_pem"`
		CAPEM   string `json:"ca_pem"`
	}

	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// SIGNED BY THE NEW AUTHORITY, verified against it ALONE — against the bundle
	// it would pass either way, which is the assertion that proves nothing.
	renewed, err := wirecert.LeafOf(wirecert.Bundle{CertPEM: []byte(got.CertPEM)})
	if err != nil {
		t.Fatalf("parse the renewed certificate: %v", err)
	}

	newOnly := x509.NewCertPool()
	if !newOnly.AppendCertsFromPEM(fresh.CertPEM()) {
		t.Fatal("parse the new authority")
	}

	if _, err := renewed.Verify(x509.VerifyOptions{
		Roots: newOnly, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("the renewal was not signed by the authority nodes are supposed to move "+
			"onto, so the overlap can never end: %v", err)
	}

	// AND IT CARRIES BOTH, because the node writes this over its own ca.crt: with
	// the new authority alone it can no longer verify a control plane that is
	// still presenting the old one.
	for name, ca := range map[string]*wirecert.CA{"previous": old, "new": fresh} {
		if !strings.Contains(got.CAPEM, string(ca.CertPEM())) {
			t.Errorf("the renewal answer omits the %s authority, so the node that installs it "+
				"can no longer verify this control plane", name)
		}
	}
}

// clientFor dials the wire as a node holding bundle.
func clientFor(t *testing.T, bundle wirecert.Bundle, caPEM []byte) *http.Client {
	t.Helper()

	cert, err := tls.X509KeyPair(bundle.CertPEM, bundle.KeyPEM)
	if err != nil {
		t.Fatalf("load the node keypair: %v", err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("parse what the node trusts")
	}

	transport := &http.Transport{TLSClientConfig: &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      roots,
		MinVersion:   tls.VersionTLS13,
	}}

	t.Cleanup(transport.CloseIdleConnections)

	return &http.Client{Transport: transport}
}
