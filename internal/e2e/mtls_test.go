package e2e

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// wireDeployment is the installation identity these tests speak for.
const wireDeployment = "0123456789abcdef0123456789abcdef"

// mtlsStore answers every ledger call successfully.
//
// A PERMISSIVE STORE ON PURPOSE. These tests are about who may reach a handler
// at all, so a store that refused things would let a test pass because the
// ledger said no rather than because the certificate did.
type mtlsStore struct{}

func (mtlsStore) Bind(context.Context, string, int64, string) error { return nil }

func (mtlsStore) Advance(context.Context, string, int64, alloc.Phase) error { return nil }

func (mtlsStore) Heartbeat(context.Context, string, int64) error { return nil }

func (mtlsStore) Release(context.Context, string, int64, alloc.Phase) error { return nil }

func (mtlsStore) Lease(context.Context, string) (*alloc.Lease, error) {
	return &alloc.Lease{ID: "l1", Epoch: 1}, nil
}

func (mtlsStore) LaunchedLeaseIDs(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

// mtlsWire stands up the node wire exactly as `billet server` does on a network
// address: a deployment authority, a server certificate from it, and client
// certificates required.
func mtlsWire(t *testing.T) (*wirecert.CA, string) {
	t.Helper()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), wireDeployment)
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	server, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(server)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	log := slog.New(slog.DiscardHandler)
	plane := nodeplane.New(log, wireDeployment, time.Minute)

	srv := httptest.NewUnstartedServer(
		nodeplane.Handler(log, plane, mtlsStore{}, nil, nodeplane.RequireClientCert()))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	return ca, srv.URL
}

func nodeClient(t *testing.T, ca *wirecert.CA, base, certName, dialAs string) *nodeclient.Client {
	t.Helper()

	bundle, err := ca.IssueNode(certName)
	if err != nil {
		t.Fatalf("issue a node certificate: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: dialAs, TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	return c
}

// A NODE WITH A CERTIFICATE FROM THIS DEPLOYMENT GETS IN.
//
// The baseline the rest of this file is measured against: without it, a test
// proving that other connections are refused would also pass against a wire that
// refuses everybody.
func TestANodeWithItsOwnCertificateRegisters(t *testing.T) {
	t.Parallel()

	ca, base := mtlsWire(t)

	c := nodeClient(t, ca, base, "epyc-1", "epyc-1")

	if err := c.Register(t.Context(), config.ProviderDocker, nil, wireDeployment); err != nil {
		t.Fatalf("a node presenting its own certificate was refused: %v", err)
	}
}

// ENROLLMENT GIVES A NEW NODE THE DEPLOYMENT IT IS JOINING.
//
// The defect this pins is one nothing in the bundle could have fixed by hand. A
// node's state directory MINTS a random deployment identity when it has none —
// correct for a control plane, which is where an installation begins, and wrong
// for a node, which joins one. So a freshly enrolled host invented an identity,
// the control plane compared it with its own, and refused the registration
// forever. The documented enrollment steps produced a node that could never
// connect.
//
// The certificate carries the identity, so the bundle an operator copies is
// sufficient on its own — and it is one authority rather than two, since the
// same certificate already decides which deployment may connect at all.
func TestAFreshlyEnrolledNodeJoinsTheDeploymentItWasIssuedFor(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), wireDeployment)
	if err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	got, err := bundle.Deployment()
	if err != nil {
		t.Fatalf("read the bundle's deployment: %v", err)
	}

	if got != wireDeployment {
		t.Fatalf("the bundle names deployment %q, want %q; a node taking its identity from "+
			"here would be refused by the control plane that issued it", got, wireDeployment)
	}

	// A brand new state directory, exactly as a fresh host has.
	dir := t.TempDir()

	adopted, err := state.AdoptDeploymentID(dir, got)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}

	if adopted != wireDeployment {
		t.Errorf("the node adopted %q, want %q", adopted, wireDeployment)
	}

	// And it sticks, so the containers it labels stay attributable across a
	// restart.
	again, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}

	if again != wireDeployment {
		t.Errorf("after a restart the node reports deployment %q, want %q; every container "+
			"it labelled with the first value becomes invisible", again, wireDeployment)
	}
}

// A NODE IS NOT SILENTLY RELABELLED INTO ANOTHER DEPLOYMENT.
//
// Adopting over an existing identity would orphan every container the host is
// already managing: they carry the old label, and neither installation would
// look for them again.
func TestANodeWillNotAdoptADifferentDeployment(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if _, err := state.AdoptDeploymentID(dir, wireDeployment); err != nil {
		t.Fatalf("adopt: %v", err)
	}

	_, err := state.AdoptDeploymentID(dir, "ffffffffffffffffffffffffffffffff")
	if err == nil {
		t.Fatal("a node was relabelled into another deployment, orphaning whatever it was " +
			"already managing")
	}

	// The original survives the refusal.
	got, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}

	if got != wireDeployment {
		t.Errorf("the refused adoption still changed the identity to %q", got)
	}
}

// A CERTIFICATE CANNOT ACT FOR A NODE IT DOES NOT NAME.
//
// This is the property the whole scheme exists for. Before it, the node named
// itself in the request path and nothing checked the claim, so anything that
// could reach the listener could bind another host's leases, take its commands,
// and ask for a JIT registration — a credential that registers a runner against
// the organisation.
func TestACertificateCannotActForAnotherNode(t *testing.T) {
	t.Parallel()

	ca, base := mtlsWire(t)

	// Authenticated as mac-1, dialling every route as epyc-1.
	c := nodeClient(t, ca, base, "mac-1", "epyc-1")

	if err := c.Register(t.Context(), config.ProviderDocker, nil, wireDeployment); err == nil {
		t.Error("a certificate for mac-1 registered a node called epyc-1")
	}

	// Registration is not the only door. A host that skipped it and went straight
	// for the credential would be the interesting attack, so the lease routes are
	// checked with the same certificate.
	if _, err := c.Lease(t.Context(), "l1"); err == nil {
		t.Error("a certificate for mac-1 read epyc-1's lease")
	}

	if err := c.Bind(t.Context(), "l1", 1, "epyc-1"); err == nil {
		t.Error("a certificate for mac-1 bound a lease as epyc-1")
	}
}

// A WIRE THAT REQUIRES CERTIFICATES REFUSES A CONNECTION THAT CARRIES NONE, even
// when the listener under it did not ask for one.
//
// Belt and braces, and the reason it is worth a test rather than a comment: the
// listener normally makes this branch unreachable, since RequireAndVerifyClientCert
// rejects an anonymous connection before any handler runs. That is exactly what
// makes it fragile. If some future wiring serves this mux over a plain listener
// — a debug endpoint, a proxy, a --dev path that grew a network address — every
// request would authenticate as whatever the URL said, and nothing else in the
// system would notice.
//
// So the handler is served here over ordinary HTTP, which is the mistake being
// guarded against, and it must still refuse.
func TestAWireRequiringCertificatesRefusesAPlainConnection(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	plane := nodeplane.New(log, wireDeployment, time.Minute)

	// No TLS at all: r.TLS is nil for every request that arrives.
	srv := httptest.NewServer(
		nodeplane.Handler(log, plane, mtlsStore{}, nil, nodeplane.RequireClientCert()))

	t.Cleanup(srv.Close)

	c, err := nodeclient.New(nodeclient.Options{Base: srv.URL, Node: "n1"})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// THE SPECIFIC REFUSAL, not merely an error. Any bug that makes the handler
	// blow up also produces "an error", so a test satisfied by one cannot tell a
	// deliberate refusal from a crash — and a crash is precisely what removing
	// this guard causes, since the code after it reads the certificate that is
	// not there.
	err = c.Register(t.Context(), config.ProviderDocker, nil, wireDeployment)
	if !errors.Is(err, nodeclient.ErrUnauthenticated) {
		t.Errorf("a registration with no certificate must be refused as unauthenticated, "+
			"got %v; otherwise the node's name is taken from the request body unverified", err)
	}

	_, err = c.Lease(t.Context(), "l1")
	if !errors.Is(err, nodeclient.ErrUnauthenticated) {
		t.Errorf("a lease read over a connection carrying no certificate must be refused as "+
			"unauthenticated, got %v", err)
	}
}

// A COPIED BUNDLE ON TWO HOSTS IS CAUGHT, and mTLS alone cannot catch it.
//
// The certificate is genuine on both machines — that is what copying it means —
// so both authenticate as the same node and the control plane's answer to
// "whose compute is this" becomes whichever host polled last. Each host's
// reconciliation then reasons about leases the other one owns.
//
// The node name is configuration and the certificate is copyable, so neither can
// distinguish a restart from a duplicate. A per-PROCESS incarnation can: a
// restart brings a new value and the old process is gone, while a duplicate
// brings a new value and the old process keeps talking. Only the second produces
// requests carrying an incarnation that is no longer current.
func TestTwoHostsSharingOneNodeNameAreCaught(t *testing.T) {
	t.Parallel()

	ca, base := mtlsWire(t)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	// THE SAME BUNDLE, TWICE. Two clients, two processes, one certificate.
	first, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	second, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if first.Incarnation() == second.Incarnation() {
		t.Fatal("two node processes minted the same incarnation, so nothing can tell them apart")
	}

	if err := first.Register(t.Context(), config.ProviderDocker, nil, wireDeployment); err != nil {
		t.Fatalf("the first host could not register: %v", err)
	}

	// The first host is working normally at this point.
	if _, err := first.Lease(t.Context(), "l1"); err != nil {
		t.Fatalf("the first host was refused before anything superseded it: %v", err)
	}

	if err := second.Register(t.Context(), config.ProviderDocker, nil, wireDeployment); err != nil {
		t.Fatalf("the second host could not register: %v", err)
	}

	// AND NOW THE FIRST IS FENCED. It is still running, still authenticated, and
	// still convinced it owns the name.
	_, err = first.Lease(t.Context(), "l1")
	if !errors.Is(err, nodeclient.ErrSuperseded) {
		t.Errorf("the superseded host was not fenced (%v); two hosts are acting as one node "+
			"and their compute cannot be told apart", err)
	}

	// The host that registered most recently is the one that works.
	if _, err := second.Lease(t.Context(), "l1"); err != nil {
		t.Errorf("the current host was refused: %v", err)
	}
}

// A RESTART IS NOT A DUPLICATE, and refusing one would be worse than the bug.
//
// The same process re-registering after the control plane forgot it — a plane
// restart, a partition — keeps its incarnation, so nothing fences it. Getting
// this wrong takes out a healthy fleet at exactly the moment the control plane
// is least able to explain why.
func TestAReconnectingNodeIsNotFenced(t *testing.T) {
	t.Parallel()

	ca, base := mtlsWire(t)

	c := nodeClient(t, ca, base, "epyc-1", "epyc-1")

	for range 3 {
		if err := c.Register(t.Context(), config.ProviderDocker, nil, wireDeployment); err != nil {
			t.Fatalf("register: %v", err)
		}

		if _, err := c.Lease(t.Context(), "l1"); err != nil {
			t.Fatalf("a node that re-registered as itself was fenced: %v", err)
		}
	}
}

// A CERTIFICATE FROM ANOTHER DEPLOYMENT NEVER REACHES A HANDLER.
//
// Two billet installations on one network are ordinary — a laptop and a server,
// or a staging deployment beside production. Neither may drive the other's
// compute, and the handshake is where that is settled rather than a check some
// handler has to remember.
func TestACertificateFromAnotherDeploymentIsRefused(t *testing.T) {
	t.Parallel()

	_, base := mtlsWire(t)

	stranger, err := wirecert.LoadOrCreateCA(t.TempDir(), "ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("create the other authority: %v", err)
	}

	c := nodeClient(t, stranger, base, "epyc-1", "epyc-1")

	err = c.Register(t.Context(), config.ProviderDocker, nil, wireDeployment)
	if err == nil {
		t.Fatal("a node holding another deployment's certificate registered")
	}
}

// A CONNECTION WITH NO CERTIFICATE AT ALL IS REFUSED.
//
// The plain case, and the one an operator hits by pointing an unconfigured node
// at a real control plane. It must fail, and it must fail before any handler
// runs.
func TestAnAnonymousConnectionIsRefused(t *testing.T) {
	t.Parallel()

	ca, base := mtlsWire(t)

	pool, err := wirecert.ClientTLS(mustIssue(t, ca, "n1"))
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	// Trusts the control plane, presents nothing.
	anonymous := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool.RootCAs, MinVersion: tls.VersionTLS13},
	}}

	res, err := anonymous.Get(base + "/v1/nodes/n1/launched") //nolint:noctx // no ctx needed here
	if err == nil {
		defer func() { _ = res.Body.Close() }()

		t.Fatalf("a connection presenting no certificate reached the wire and got %d",
			res.StatusCode)
	}
}

// AN EXPIRED CERTIFICATE IS NOT A PERMANENT REFUSAL.
//
// A node that treated it as one would stop, and stopping is wrong here: the fix
// is an operator re-issuing the certificate, and a node that gave up must be
// restarted by hand once they have. This checks the classification the node acts
// on, not the transport.
func TestATLSFailureIsNotAVerdict(t *testing.T) {
	t.Parallel()

	_, base := mtlsWire(t)

	stranger, err := wirecert.LoadOrCreateCA(t.TempDir(), "ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("create the other authority: %v", err)
	}

	c := nodeClient(t, stranger, base, "n1", "n1")

	err = c.Register(t.Context(), config.ProviderDocker, nil, wireDeployment)
	if err == nil {
		t.Fatal("a rejected handshake was reported as a successful registration")
	}

	if errors.Is(err, nodeclient.ErrRefused) {
		t.Errorf("a handshake failure was classified as a permanent refusal, so the node "+
			"stops and stays down after an operator fixes the certificate: %v", err)
	}
}

func mustIssue(t *testing.T, ca *wirecert.CA, name string) wirecert.Bundle {
	t.Helper()

	b, err := ca.IssueNode(name)
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	return b
}
