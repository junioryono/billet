package e2e

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/server"
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
type mtlsStore struct {
	// launched is what the ledger says this node already holds, which is how a
	// restarted control plane rebuilds ownership it never saw created.
	launched map[string]bool

	// launchedErr makes that query fail, which must not produce a node the plane
	// has accepted but whose existing compute it cannot attribute.
	launchedErr error
}

func (mtlsStore) Bind(context.Context, string, int64, string) error { return nil }

func (mtlsStore) Advance(context.Context, string, int64, alloc.Phase) error { return nil }

func (mtlsStore) Heartbeat(context.Context, string, int64) error { return nil }

func (mtlsStore) Release(context.Context, string, int64, alloc.Phase) error { return nil }

func (mtlsStore) Lease(context.Context, string) (*alloc.Lease, error) {
	return &alloc.Lease{ID: "l1", Epoch: 1}, nil
}

func (m mtlsStore) QuarantinedLeaseIDs(context.Context, string) (map[string]bool, error) {
	return map[string]bool{}, nil
}

func (m mtlsStore) LaunchedLeaseIDs(context.Context, string) (map[string]bool, error) {
	if m.launchedErr != nil {
		return nil, m.launchedErr
	}

	if m.launched == nil {
		return map[string]bool{}, nil
	}

	return m.launched, nil
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

	serving, err := ca.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the server certificate: %v", err)
	}

	conf, err := wirecert.ServerTLS(serving)
	if err != nil {
		t.Fatalf("server tls: %v", err)
	}

	log := slog.New(slog.DiscardHandler)
	plane := nodeplane.New(log, wireDeployment, time.Minute,
		nodeplane.WithTierCatalog([]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64", RunnerGroup: "billet",
		}}))

	// A SHORT POLL WINDOW, because these tests poll to observe a REFUSAL. At the
	// production window an accepted poll blocks for the best part of a minute,
	// which turns a fencing assertion into a fifty-second wait.
	plane.SetPollWindowForTest(200 * time.Millisecond)

	srv := httptest.NewUnstartedServer(
		nodeplane.Handler(log, plane, mtlsStore{}, nil, nodeplane.RequireClientCert()))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	return ca, srv.URL
}

// mtlsWireWithJIT is the same wire with a minting source attached, so a refusal
// is the entitlement check speaking rather than the absence of a GitHub client.
func mtlsWireWithJIT(t *testing.T) (*wirecert.CA, string) {
	t.Helper()

	ca, base, _ := mtlsWireWithPlane(t)

	return ca, base
}

func mtlsWireWithPlane(t *testing.T) (*wirecert.CA, string, *nodeplane.Plane) {
	t.Helper()

	return mtlsWireWithClock(t, nil)
}

// wireClock is a clock these tests move.
//
// Atomic because the plane reads it from its own goroutines; a closure over a
// plain variable is a data race the moment anything but the test consults it.
type wireClock struct {
	nanos atomic.Int64
}

func newWireClock() *wireClock {
	c := &wireClock{}
	c.nanos.Store(time.Now().UnixNano())

	return c
}

func (c *wireClock) now() time.Time { return time.Unix(0, c.nanos.Load()) }

func (c *wireClock) advancePastSilence() { c.nanos.Add(int64(10 * time.Minute)) }

func mtlsWireWithClock(t *testing.T, clock func() time.Time) (*wirecert.CA, string, *nodeplane.Plane) {
	t.Helper()

	return mtlsWireWithStore(t, clock, mtlsStore{})
}

func mtlsWireWithStore(
	t *testing.T, clock func() time.Time, store mtlsStore,
) (*wirecert.CA, string, *nodeplane.Plane) {
	t.Helper()

	ca, base, plane, _ := mtlsWireWithSets(t, clock, store)

	return ca, base, plane
}

// awaitQueued blocks until the plane holds a command for a node.
//
// EVERY LAUNCH BELOW IS SPAWNED IN A GOROUTINE AND POLLED FOR IMMEDIATELY, which
// is a race against the scheduler rather than a sequence: the poll can reach the
// plane before the launch has queued anything, and a poll with nothing to take
// answers "no command" instead of waiting for one. Locally the goroutine
// essentially always wins; on a contended CI runner it does not, and this failed
// exactly that way — `ok=false err=<nil>` in twenty milliseconds, which reads as
// the plane refusing to deliver rather than as a test that asked too early.
//
// Synchronising on the queue makes the ordering caused rather than hoped for. The
// deadline bounds a stall; it is not a budget for the work, which takes
// microseconds when it is scheduled at all.
func awaitQueued(t *testing.T, plane *nodeplane.Plane, node string) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)

	for plane.QueuedForTest(node) == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the launch never reached the plane's queue for %s", node)
		}

		time.Sleep(time.Millisecond)
	}
}

func mtlsWireWithSets(
	t *testing.T, clock func() time.Time, store mtlsStore,
) (*wirecert.CA, string, *nodeplane.Plane, *atomic.Int64) {
	t.Helper()

	sets := &atomic.Int64{}
	sets.Store(7)

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

	opts := []nodeplane.Option{}
	if clock != nil {
		opts = append(opts, nodeplane.WithClock(clock))
	}

	plane := nodeplane.New(log, wireDeployment, time.Minute, append(opts,
		nodeplane.WithTierCatalog([]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64", RunnerGroup: "billet",
		}}))...)

	// A SHORT POLL WINDOW, because these tests poll to observe a REFUSAL. At the
	// production window an accepted poll blocks for the best part of a minute,
	// which turns a fencing assertion into a fifty-second wait.
	plane.SetPollWindowForTest(200 * time.Millisecond)

	srv := httptest.NewUnstartedServer(
		nodeplane.Handler(log, plane, store, alwaysMints{setID: sets},
			nodeplane.RequireClientCert(),
			// The catalogue a JIT request is checked against. Its runner group is
			// part of a tier's address, so a wire without it refuses every
			// legitimate registration for a tier outside the default group.
		))
	srv.TLS = conf
	srv.StartTLS()

	t.Cleanup(srv.Close)

	return ca, srv.URL, plane, sets
}

// alwaysMints hands out a registration for anything it is asked, so the only
// thing that can refuse is the entitlement check under test.
// PER WIRE, NOT PER PACKAGE. The id was a package-level variable so one test
// could model an operator recreating a scale set — and every other test in the
// package shares it, so that change reached tests running in parallel and broke
// one of them. Test state that one test MUTATES cannot be global.
type alwaysMints struct {
	setID *atomic.Int64
}

// THE GROUP IS PART OF THE ADDRESS, and a fake that ignores it cannot catch the
// bug where billet resolves a tier in the wrong one. Real Describe defaults an
// empty group to "default", so a tier deliberately placed elsewhere is simply
// not found — and its legitimate registrations are refused.
func (m alwaysMints) Describe(_ context.Context, name, group string) (*nodeplane.JITSet, []string, error) {
	if name != "billet-2vcpu" || group != "billet" {
		return nil, nil, nil
	}

	return &nodeplane.JITSet{ID: int(m.setID.Load()), Name: name}, nil, nil
}

func (alwaysMints) JITConfig(_ context.Context, _ int, runnerName, _ string) (nodeplane.JITRegistration, error) {
	return mintedFor(runnerName), nil
}

type mintedFor string

func (m mintedFor) Config() string     { return "a credential" }
func (m mintedFor) RunnerName() string { return string(m) }

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

	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("a node presenting its own certificate was refused: %v", err)
	}
}

// ENROLLMENT GIVES A NEW NODE THE DEPLOYMENT IT IS JOINING.
//
// A node's state directory MINTS a random identity when it has none — correct for a
// control plane, which is where an installation begins, and wrong for a node, which
// joins one. So a freshly enrolled host invented an identity and was refused
// forever: the documented steps produced a node that could never connect.
//
// The certificate carries the identity, so the bundle is sufficient on its own, and
// it is one authority rather than two.
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

	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err == nil {
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
// Worth a test rather than a comment precisely because the listener normally makes
// the branch unreachable. If some future wiring serves this mux over a plain
// listener — a debug endpoint, a proxy — every request would authenticate as
// whatever the URL said. So it is served here over ordinary HTTP, which is the
// mistake being guarded against.
func TestAWireRequiringCertificatesRefusesAPlainConnection(t *testing.T) {
	t.Parallel()

	log := slog.New(slog.DiscardHandler)
	plane := nodeplane.New(log, wireDeployment, time.Minute,
		nodeplane.WithTierCatalog([]config.Tier{{
			Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
			VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64", RunnerGroup: "billet",
		}}))

	// A SHORT POLL WINDOW, because these tests poll to observe a REFUSAL. At the
	// production window an accepted poll blocks for the best part of a minute,
	// which turns a fencing assertion into a fifty-second wait.
	plane.SetPollWindowForTest(200 * time.Millisecond)

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
	err = c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory})
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
// The certificate is genuine on both machines — that is what copying means — so both
// authenticate as the same node and "whose compute is this" becomes whichever host
// polled last.
//
// The node name is configuration and the certificate is copyable, so neither
// distinguishes a restart from a duplicate. A per-PROCESS incarnation does: a restart
// brings a new value and the old process is gone, while a duplicate brings a new
// value and the old process keeps talking.
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

	if err := first.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("the first host could not register: %v", err)
	}

	if err := second.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("the second host could not register: %v", err)
	}

	// THE FIRST IS REFUSED NEW WORK. It is still running, still authenticated, and
	// still convinced it owns the name — and two hosts under one name cannot both
	// be given work.
	if _, _, err := first.Poll(t.Context()); !errors.Is(err, nodeclient.ErrSuperseded) {
		t.Errorf("a superseded host was still offered work (%v); two hosts would act as one "+
			"node and their compute could not be told apart", err)
	}

	if err := first.Bind(t.Context(), "l1", 1, "epyc-1"); !errors.Is(err, nodeclient.ErrSuperseded) {
		t.Errorf("a superseded host still bound a lease: %v", err)
	}

	// BUT IT IS NOT SILENCED, and that distinction is the whole of the fix. A
	// superseded host may be holding a container right now, and registration has
	// already told the listener that its lease is the node's — so the listener has
	// stopped heartbeating. If this process could neither report nor maintain what
	// it holds, that lease would expire while its container ran.
	//
	// Reporting is the handover itself, so it stays open to any process holding
	// the certificate. Renewal is scoped to leases this process was GIVEN, which
	// TestASupersededHostCanFinishItsOwnLease covers; this one holds none.
	if err := first.Report(t.Context(), nodeapi.CommandResult{ID: "c1", OK: true}); err != nil {
		t.Errorf("a superseded host could not report a result (%v); the tombstone recorded to "+
			"hand it custody is unreachable by the only process that could consume it", err)
	}

	// The host that registered most recently is the one that gets work.
	if _, err := second.Lease(t.Context(), "l1"); err != nil {
		t.Errorf("the current host was refused: %v", err)
	}
}

// AN OLDER NODE CANNOT SLIP PAST THE FENCE BY SAYING NOTHING.
//
// Compatibility is scoped to nodes that have not CLAIMED an incarnation, not to the
// REQUEST: scoped to the request, an absent header returns early and accepts
// everything, so a billet predating the field would never send it and both processes
// would take work as the same node forever.
//
// The same hole existed through registration, where an empty claim overwrote a live
// incarnation.
func TestAnOlderNodeCannotSlipPastTheFence(t *testing.T) {
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

	current, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := current.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// A build with no notion of incarnations: no header, and no field in the
	// registration body either.
	old := &http.Client{Transport: &http.Transport{TLSClientConfig: conf}}

	body := fmt.Sprintf(`{"version":%d,"node":"epyc-1","provider":"docker","deployment":%q}`,
		nodeapi.Version, wireDeployment)

	status, err := postAs(t, old, base+"/v1/register", body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	// It may register — that is the compatibility path — but it must not have
	// taken the name.
	if status == http.StatusOK {
		if _, _, err := current.Poll(t.Context()); errors.Is(err, nodeclient.ErrSuperseded) {
			t.Error("an older node with no incarnation took the name from the current one")
		}
	}

	// And it is refused work of its own, because the name is claimed.
	status, err = postAs(t, old, base+"/v1/nodes/epyc-1/poll", "")
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	if status != http.StatusConflict {
		t.Errorf("an older node polled for work as a name another process holds and got %d, "+
			"want 409; two hosts would take commands as one node", status)
	}
}

// postAs sends a literal body as a specific client, which is the only way to
// speak as a build that does not set the incarnation header.
func postAs(t *testing.T, c *http.Client, url, body string) (int, error) {
	t.Helper()

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return 0, err
	}

	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.Do(req)
	if err != nil {
		return 0, err
	}

	defer func() { _ = res.Body.Close() }()

	return res.StatusCode, nil
}

// A SUPERSEDED HOST CAN FINISH THE LEASE IT WAS GIVEN, which is the half that
// makes draining possible at all.
//
// The refusal in the next test is only safe because of this one. A superseded
// process keeps renewing what it holds and tends it until the compute is
// confirmed gone — and tending ENDS with a release. If that release were refused
// along with everything else, the drain could never complete and the lease would
// be held until an operator intervened.
func TestASupersededHostCanFinishItsOwnLease(t *testing.T) {
	t.Parallel()

	ca, base, plane := mtlsWireWithPlane(t)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	first, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	second, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := first.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The first process is GIVEN lease l1, which is what makes it l1's owner.
	go func() {
		lease := &alloc.Lease{
			ID:        "l1",
			Tier:      "billet-2vcpu",
			VCPU:      2,
			Memory:    8 * config.GiB,
			GuestOS:   config.GuestLinux,
			Providers: []config.ProviderKind{config.ProviderDocker},
			Epoch:     1,
		}

		//nolint:errcheck // the launch's fate is not what this test is about
		_ = plane.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	}()

	cmd, ok, err := first.Poll(t.Context())
	if err != nil || !ok || cmd.Lease == nil {
		t.Fatalf("the first process was not given the launch: ok=%v err=%v", ok, err)
	}

	// And then it is superseded, mid-launch.
	if err := second.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("the second process could not register: %v", err)
	}

	// RENEWAL FIRST, because that is what a drain spends its time doing: the
	// janitor keeps this lease alive for as long as the container runs.
	if err := first.Heartbeat(t.Context(), "l1", 1); err != nil {
		t.Errorf("a superseded process could not renew the lease it was actually given (%v); "+
			"nothing else is renewing it, so its capacity is resold under a running "+
			"container", err)
	}

	if err := first.Release(t.Context(), "l1", 1, alloc.PhaseDone); err != nil {
		t.Errorf("a superseded process could not release the lease it was actually given "+
			"(%v); its drain can never finish, so the capacity is held until somebody "+
			"intervenes by hand", err)
	}
}

// A SUPERSEDED HOST CANNOT RELEASE A LEASE IT WAS NEVER GIVEN.
//
// Permitting every lease route by node name was too broad. A superseded process
// shares the name and the certificate with its replacement, so it could ask for
// the current process's lease, read its epoch, and release it — returning
// capacity while the other host's container was still running. Both requests
// look identical to a handler that checks only the name.
//
// What it MAY do is maintain what it was actually given: renewing extends a
// lease and can never free one, so that stays open for every process.
func TestASupersededHostCannotReleaseAnotherProcessLease(t *testing.T) {
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

	first, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	second, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := first.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := second.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The superseded process was never given l1 — the second one owns the name
	// now — so it may not decide that lease's fate.
	err = first.Release(t.Context(), "l1", 1, alloc.PhaseFailed)
	if !errors.Is(err, nodeclient.ErrSuperseded) {
		t.Errorf("a superseded host released a lease it was never given (%v); the capacity "+
			"goes back while another host's container is still using it", err)
	}

	if err := first.Advance(t.Context(), "l1", 1, alloc.PhaseOnline); !errors.Is(err, nodeclient.ErrSuperseded) {
		t.Errorf("a superseded host advanced a lease it was never given: %v", err)
	}

	// AND RENEWAL IS REFUSED TOO, which corrects an argument this test used to
	// encode. "A heartbeat only extends a lease" is true of one heartbeat and
	// false of a process that keeps sending them: repeated renewal does not hold
	// capacity slightly longer, it denies it indefinitely. If the current process
	// dies before releasing, the reaper is what reclaims — and a superseded
	// process renewing that lease forever is exactly what stops it.
	//
	// A superseded process renewing its OWN lease is a different test, and the one
	// that makes draining possible.
	if err := first.Heartbeat(t.Context(), "l1", 1); !errors.Is(err, nodeclient.ErrSuperseded) {
		t.Errorf("a superseded host renewed a lease it was never given (%v); it can keep "+
			"doing that forever, and the reaper never reclaims the capacity", err)
	}

	// The read that would have supplied the epoch for any of the above is closed
	// for the same reason.
	if _, err := first.Lease(t.Context(), "l1"); !errors.Is(err, nodeclient.ErrSuperseded) {
		t.Errorf("a superseded host read a lease it was never given: %v", err)
	}

	// And the current process is unaffected.
	if err := second.Release(t.Context(), "l1", 1, alloc.PhaseFailed); err != nil {
		t.Errorf("the current process was refused its own lease: %v", err)
	}
}

// A POLL ADMITTED BEFORE SUPERSESSION IS REFUSED WHEN IT WAKES.
//
// A long poll is checked when it ARRIVES and answered when a command appears,
// and a supersession can land between the two. Without a second look, a process
// that has since been replaced still walks away with a command — and then holds
// a genuine entitlement to mint that runner's registration, having never done
// anything the handler could object to.
func TestAWokenPollFromASupersededProcessIsRefused(t *testing.T) {
	t.Parallel()

	ca, base, plane := mtlsWireWithPlane(t)

	// Long enough that the poll below is still waiting when the second process
	// registers, rather than having timed out and returned nothing.
	plane.SetPollWindowForTest(10 * time.Second)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	first, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	second, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := first.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	type polled struct {
		cmd nodeapi.Command
		ok  bool
		err error
	}

	answer := make(chan polled, 1)

	go func() {
		cmd, ok, err := first.Poll(t.Context())
		answer <- polled{cmd, ok, err}
	}()

	// Wait until the poll is genuinely waiting, or the supersession below would
	// race it and the test would prove the ordinary path instead.
	waitingBy := time.Now().Add(10 * time.Second)
	for plane.WaitersForTest("epyc-1") == 0 {
		if time.Now().After(waitingBy) {
			t.Fatal("the poll never blocked, so nothing was superseded mid-wait")
		}

		time.Sleep(time.Millisecond)
	}

	if err := second.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("the second process could not register: %v", err)
	}

	// A command arrives, waking the older poll.
	go func() {
		lease := &alloc.Lease{
			ID:        "l1",
			Tier:      "billet-2vcpu",
			VCPU:      2,
			Memory:    8 * config.GiB,
			GuestOS:   config.GuestLinux,
			Providers: []config.ProviderKind{config.ProviderDocker},
			Epoch:     1,
		}

		//nolint:errcheck // the launch's fate is not what this test is about
		_ = plane.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	}()

	select {
	case got := <-answer:
		if got.ok {
			t.Errorf("a poll admitted before supersession still took command %s; that process "+
				"is now entitled to mint the runner for a launch it should never have been "+
				"given", got.cmd.ID)
		}

		if !errors.Is(got.err, nodeclient.ErrSuperseded) {
			t.Errorf("want ErrSuperseded from a woken poll, got %v", got.err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the poll never returned")
	}
}

// OMITTING THE HEADER IS NOT A WAY ROUND THE FENCE.
//
// The same correction CheckIncarnation needed a round earlier, and I did not
// apply it to the lease routes or to the JIT entitlement: both treated an empty
// claim as a wildcard. A superseded process holding the shared certificate could
// simply stop sending the header, read a lease and its epoch, and release the
// replacement's work.
//
// Compatibility belongs to nodes that have never claimed an incarnation — a
// fleet mid-upgrade — not to any request that declines to mention one.
func TestOmittingTheIncarnationIsNotAWildcard(t *testing.T) {
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

	current, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := current.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// A process that holds the certificate and says nothing about which process
	// it is.
	quiet := &http.Client{Transport: &http.Transport{TLSClientConfig: conf}}

	for _, route := range []struct {
		what string
		path string
		body string
	}{
		{"release", "/v1/nodes/epyc-1/leases/l1/release", `{"epoch":1,"outcome":"done"}`},
		{"heartbeat", "/v1/nodes/epyc-1/leases/l1/heartbeat", `{"epoch":1}`},
		{"advance", "/v1/nodes/epyc-1/leases/l1/advance", `{"epoch":1,"phase":"online"}`},
	} {
		status, err := postAs(t, quiet, base+route.path, route.body)
		if err != nil {
			t.Fatalf("post %s: %v", route.what, err)
		}

		if status != http.StatusConflict {
			t.Errorf("%s with no incarnation got %d, want 409: a superseded process can drop "+
				"the header and act on the current one's leases", route.what, status)
		}
	}
}

// A DRAINING PROCESS DOES NOT VOUCH FOR THE NODE'S AVAILABILITY.
//
// Liveness was recorded for any request bearing the node's name, so a superseded
// process draining its custody kept refreshing the record for as long as its job
// ran. If the replacement then died, the plane went on choosing that node: every
// launch it sent waited out the full command timeout and failed, and the tier was
// effectively down for the length of somebody else's job.
func TestADrainingProcessDoesNotKeepADeadNodeSchedulable(t *testing.T) {
	t.Parallel()

	clock := newWireClock()

	ca, base, plane := mtlsWireWithClock(t, clock.now)

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	first, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	second, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := first.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := second.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// The replacement goes silent — it died — while the superseded process keeps
	// working through its drain.
	clock.advancePastSilence()

	// EVERY LATER REQUEST HAPPENS BEFORE ANYTHING ASKS Nodes(), deliberately.
	// Asking is what runs expiry, so a test that checks the fleet first has
	// already removed the node and can no longer observe anything refreshing it.
	if err := first.Report(t.Context(), nodeapi.CommandResult{ID: "c1", OK: true}); err != nil {
		t.Fatalf("the draining process could not report: %v", err)
	}

	// AND OMITTING THE HEADER IS NOT A WAY BACK IN. An empty claim was treated as
	// eligible to refresh liveness, so a process that simply stopped saying which
	// one it was could keep a dead node schedulable indefinitely — by posting
	// results nobody asked for, which the result route accepts from anybody
	// because reporting is the handover.
	quiet := &http.Client{Transport: &http.Transport{TLSClientConfig: conf}}

	status, err := postAs(t, quiet, base+"/v1/nodes/epyc-1/result",
		`{"id":"nobody-asked","ok":true}`)
	if err != nil {
		t.Fatalf("post: %v", err)
	}

	if status >= 500 {
		t.Fatalf("the result route failed with %d", status)
	}

	if got := plane.Nodes(); len(got) != 0 {
		t.Errorf("a node whose current process died is still in the fleet (%v), kept there by "+
			"a superseded process draining its own work — or by one that declined to say "+
			"which process it was; every launch sent to it waits out the command timeout "+
			"and fails", got)
	}
}

// OWNERSHIP IS REBUILT FROM THE LEDGER, so a drain survives a control plane
// restart.
//
// The plane's record of who was given which lease is in memory. The sequence
// that breaks without a rebuild: a node is holding compute, the control plane
// restarts, the node re-registers and adopts what it finds, a second host
// supersedes it — and the new plane never saw those launches. The draining
// process is then refused its own release, custody is never given up, and the
// drain runs forever.
//
// The ledger knows what the plane forgot: a lease still open on this node is
// this node's, and the process registering now is the one holding it.
func TestOwnershipSurvivesAControlPlaneRestart(t *testing.T) {
	t.Parallel()

	// A ledger that already places l1 on this node, which is what a restart finds.
	ca, base, _ := mtlsWireWithStore(t, nil, mtlsStore{launched: map[string]bool{"l1": true}})

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	first, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	second, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	// The node re-registers against the fresh plane; nothing here ever delivered
	// it a launch command for l1.
	if err := first.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := second.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("the second process could not register: %v", err)
	}

	if err := first.Heartbeat(t.Context(), "l1", 1); err != nil {
		t.Errorf("a superseded process could not renew a lease the ledger says it holds (%v); "+
			"its drain cannot even keep the lease alive", err)
	}

	if err := first.Release(t.Context(), "l1", 1, alloc.PhaseDone); err != nil {
		t.Errorf("a superseded process could not release a lease the ledger says it holds "+
			"(%v); the drain never finishes and the capacity is held until somebody "+
			"intervenes", err)
	}
}

// A RECREATED SCALE SET MUST NOT WEDGE ITS TIER FOREVER.
//
// The cached id is a fact about somebody else's system. Delete a scale set and
// recreate it and the id changes; the node discovers this on its own failure and
// arrives with the new one. A control plane that never re-checks refuses it
// against the old id, and every launch for that tier fails until somebody
// restarts the control plane.
func TestARecreatedScaleSetDoesNotWedgeTheTier(t *testing.T) {
	t.Parallel()

	ca, base, plane, sets := mtlsWireWithSets(t, nil, mtlsStore{})

	c := nodeClient(t, ca, base, "epyc-1", "epyc-1")

	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	launch := func(id string) {
		t.Helper()

		go func() {
			lease := &alloc.Lease{
				ID:        id,
				Tier:      "billet-2vcpu",
				VCPU:      2,
				Memory:    8 * config.GiB,
				GuestOS:   config.GuestLinux,
				Providers: []config.ProviderKind{config.ProviderDocker},
				Epoch:     1,
			}

			//nolint:errcheck // the launch's fate is not what this test is about
			_ = plane.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
		}()

		awaitQueued(t, plane, "epyc-1")

		if _, ok, err := c.Poll(t.Context()); err != nil || !ok {
			t.Fatalf("the node was not given the launch: ok=%v err=%v", ok, err)
		}
	}

	launch("l1")

	// The first mint caches the tier's set at its current id.
	if _, err := c.JITConfig(t.Context(), 7, "billet-l1", "_work"); err != nil {
		t.Fatalf("the first registration was refused: %v", err)
	}

	// Somebody deletes and recreates the scale set on GitHub.
	sets.Store(11)

	launch("l2")

	if _, err := c.JITConfig(t.Context(), 11, "billet-l2", "_work"); err != nil {
		t.Errorf("a launch after the scale set was recreated was refused against the cached "+
			"id (%v); every job on this tier fails until the control plane restarts", err)
	}
}

// A REGISTRATION WHOSE OWNERSHIP REBUILD FAILED IS NOT A REGISTRATION.
//
// Treating the rebuild as best-effort was wrong. The node goes on to adopt its
// surviving compute believing it is registered, and if it is later superseded
// the plane has no record that those leases are its — so its renewals are
// refused and the leases are reaped under running containers.
//
// A ledger that cannot answer is an outage, so this is a 503 the node retries
// rather than a verdict it stops on.
func TestARegistrationThatCannotRebuildOwnershipIsRefused(t *testing.T) {
	t.Parallel()

	ca, base, _ := mtlsWireWithStore(t, nil, mtlsStore{
		launchedErr: errors.New("database is locked"),
	})

	c := nodeClient(t, ca, base, "epyc-1", "epyc-1")

	err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory})
	if err == nil {
		t.Fatal("a node registered although the plane could not read which leases it already " +
			"holds; superseded later, it would be refused its own renewals")
	}

	if errors.Is(err, nodeclient.ErrRefused) {
		t.Errorf("a ledger outage was reported as a permanent refusal, so the node gives up "+
			"and stays down after the database recovers: %v", err)
	}
}

// A DRAIN OUTLIVES THE NODE RECORD, because that is the situation it is for.
//
// Ownership used to live on the node's own entry, which expires. A draining
// process outlives its replacement by design — so when the replacement went
// silent and the node was forgotten, the ownership went with it and the drain
// lost the right to renew the lease of compute that was still running.
//
// Liveness and ownership answer different questions and cannot share a lifetime.
func TestADrainOutlivesTheNodeRecord(t *testing.T) {
	t.Parallel()

	clock := newWireClock()

	ca, base, plane, _ := mtlsWireWithSets(t, clock.now, mtlsStore{
		launched: map[string]bool{"l1": true},
	})

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	conf, err := wirecert.ClientTLS(bundle)
	if err != nil {
		t.Fatalf("client tls: %v", err)
	}

	first, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	second, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	if err := first.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if err := second.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("the second process could not register: %v", err)
	}

	// The replacement dies. The draining process keeps going, and its traffic
	// deliberately does not vouch for the node — so the record is forgotten.
	clock.advancePastSilence()

	if got := plane.Nodes(); len(got) != 0 {
		t.Fatalf("the node was still in the fleet after its current process went silent: %v",
			got)
	}

	if err := first.Heartbeat(t.Context(), "l1", 1); err != nil {
		t.Errorf("a draining process could not renew its own lease once the node record had "+
			"expired (%v); nothing else is renewing it, so the capacity is reclaimed under "+
			"a running container", err)
	}

	if err := first.Release(t.Context(), "l1", 1, alloc.PhaseDone); err != nil {
		t.Errorf("a draining process could not finish once the node record had expired: %v", err)
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
		if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
			t.Fatalf("register: %v", err)
		}

		if _, err := c.Lease(t.Context(), "l1"); err != nil {
			t.Fatalf("a node that re-registered as itself was fenced: %v", err)
		}
	}
}

// A TIER OUTSIDE THE DEFAULT RUNNER GROUP STILL GETS ITS REGISTRATIONS.
//
// Resolving a scale set without a group silently means "the default group", and
// a tier deliberately placed elsewhere — which is how an operator keeps it away
// from every repository in the organisation — would simply not be found. Every
// launch on that tier would then fail at the moment it asked for its
// registration, on the control plane, for a reason no node could report usefully.
//
// The tightening that closed the scale-set substitution is what introduced this:
// the first version resolved the set with an empty group. It is the same defect
// shape as the substitution it was fixing — an address with a piece missing.
func TestATierInANonDefaultRunnerGroupCanStillMint(t *testing.T) {
	t.Parallel()

	ca, base, plane := mtlsWireWithPlane(t)

	c := nodeClient(t, ca, base, "epyc-1", "epyc-1")

	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	launched := make(chan error, 1)

	go func() {
		lease := &alloc.Lease{
			ID:        "l1",
			Tier:      "billet-2vcpu", // configured into runner group "billet"
			VCPU:      2,
			Memory:    8 * config.GiB,
			GuestOS:   config.GuestLinux,
			Providers: []config.ProviderKind{config.ProviderDocker},
			Epoch:     1,
		}

		launched <- plane.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	}()

	awaitQueued(t, plane, "epyc-1")

	if _, ok, err := c.Poll(t.Context()); err != nil || !ok {
		t.Fatalf("the node was not given the launch: ok=%v err=%v", ok, err)
	}

	if _, err := c.JITConfig(t.Context(), 7, "billet-l1", "_work"); err != nil {
		t.Errorf("a tier in a non-default runner group could not mint the registration for "+
			"the launch it was given: %v", err)
	}
}

// THE SCALE SET IS PART OF THE ENTITLEMENT, not just the lease.
//
// Checking only the lease id left a substitution open, and it is the more useful
// attack of the two: a compromised node holding an ORDINARY launch for a
// low-privilege tier asks for that lease's own runner name paired with another
// tier's scale set. The lease check passes. The runner it starts joins a tier
// with different labels, different jobs, and possibly different secrets.
//
// The set is resolved from the lease's own tier now, rather than taken from the
// request.
func TestANodeCannotMintIntoAnotherTiersScaleSet(t *testing.T) {
	t.Parallel()

	ca, base, plane := mtlsWireWithPlane(t)

	c := nodeClient(t, ca, base, "epyc-1", "epyc-1")

	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// A genuine launch, in flight and unanswered, so the node is entitled to
	// exactly one registration.
	launched := make(chan error, 1)

	go func() {
		lease := &alloc.Lease{
			ID:        "l1",
			Tier:      "billet-2vcpu",
			VCPU:      2,
			Memory:    8 * config.GiB,
			GuestOS:   config.GuestLinux,
			Providers: []config.ProviderKind{config.ProviderDocker},
			Epoch:     1,
		}

		// Buffered and never read: this launch exists to be IN FLIGHT, and its
		// eventual outcome is not what the test is about.
		launched <- plane.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	}()

	awaitQueued(t, plane, "epyc-1")

	cmd, ok, err := c.Poll(t.Context())
	if err != nil || !ok {
		t.Fatalf("the node was not given the launch: ok=%v err=%v", ok, err)
	}

	if cmd.Lease == nil || cmd.Lease.ID != "l1" {
		t.Fatalf("unexpected command: %+v", cmd)
	}

	// Its own runner name, somebody else's scale set.
	if _, err := c.JITConfig(t.Context(), 999, "billet-l1", "_work"); err == nil {
		t.Error("a node minted a registration in a scale set its launch does not name; the " +
			"runner it starts joins a tier with different labels and possibly different " +
			"secrets")
	}

	// And the legitimate request still works, or the check above would be
	// satisfied by refusing everything.
	if _, err := c.JITConfig(t.Context(), 7, "billet-l1", "_work"); err != nil {
		t.Errorf("the launch the node was actually given could not mint its registration: %v",
			err)
	}
}

// A REGISTERED NODE IS NOT AN ENTITLED ONE, and the JIT endpoint is where that
// distinction matters most.
//
// A JIT config registers a runner against the organisation. A node that could
// ask for one whenever it liked could start runners billet never escrowed
// capacity for, never tracked, and never tears down — for any scale set, under
// any name, in a loop. That contradicts the one containment property the design
// claims: compromising a compute host must not let it mint runners.
//
// A registration proves which host you are. It says nothing about what work you
// were given, and only a command can say that.
func TestANodeCannotMintRunnersItWasNotAskedToLaunch(t *testing.T) {
	t.Parallel()

	ca, base := mtlsWireWithJIT(t)

	c := nodeClient(t, ca, base, "epyc-1", "epyc-1")

	if err := c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// A well-formed request for a lease this node was never given.
	_, err := c.JITConfig(t.Context(), 7, "billet-l1", "_work")
	if err == nil {
		t.Fatal("a node with no launch in flight minted a runner registration; a compromised " +
			"host could start unmanaged runners against the organisation")
	}

	// And a name billet never assigns at all.
	if _, err := c.JITConfig(t.Context(), 7, "anything-i-like", "_work"); err == nil {
		t.Error("a runner name billet never assigned was accepted, so the entitlement check " +
			"has nothing to bind to")
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

	ca, base := mtlsWire(t)

	stranger, err := wirecert.LoadOrCreateCA(t.TempDir(), "ffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatalf("create the other authority: %v", err)
	}

	foreign, err := stranger.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	// THE FOREIGN IDENTITY, THE LEGITIMATE TRUST ROOT, and getting that pairing
	// wrong made this test prove nothing. Configuring RootCAs from the stranger
	// too meant the CLIENT rejected the server's certificate first — the
	// handshake failed before the server ever examined the client's, so the test
	// passed identically with server-side client authentication switched off.
	//
	// Assembled by hand rather than through ClientTLS, because ClientTLS now
	// refuses this exact combination: a node whose certificate does not chain to
	// the authority beside it is stopped on the node, which is the right place. A
	// real host cannot reach this state — but a hostile one is not obliged to use
	// billet's constructor, and the server must refuse it regardless.
	pair, err := tls.X509KeyPair(foreign.CertPEM, foreign.KeyPEM)
	if err != nil {
		t.Fatalf("key pair: %v", err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("could not parse the legitimate authority")
	}

	conf := &tls.Config{
		Certificates: []tls.Certificate{pair},
		RootCAs:      roots,
		MinVersion:   tls.VersionTLS13,
	}

	c, err := nodeclient.New(nodeclient.Options{Base: base, Node: "epyc-1", TLS: conf})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}

	err = c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory})
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

	// REFUSED BY THE HANDLER, not by the handshake, and that distinction is the
	// change: an unenrolled machine has no certificate and still has to reach
	// /v1/ca and /v1/enroll, so the listener verifies a certificate if one is
	// given rather than demanding one.
	//
	// What must not change is the outcome for everything else. 401 here means the
	// guard refused it; anything 2xx would mean the relaxation opened a route.
	res, err := anonymous.Get(base + "/v1/nodes/n1/launched") //nolint:noctx // no ctx needed here
	if err != nil {
		t.Fatalf("the anonymous connection did not complete: %v", err)
	}

	defer func() { _ = res.Body.Close() }()

	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a connection presenting no certificate got %d from a node route; want 401",
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

	err = c.Register(t.Context(), nodeclient.Registration{Provider: config.ProviderDocker, Deployment: wireDeployment, VCPU: testNodeVCPU, Memory: testNodeMemory})
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
