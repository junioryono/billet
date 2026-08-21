// Package e2e drives the whole of billet against a fake Actions service and a
// real container backend.
//
// Every other suite tests one seam. This one tests the relationships between
// them, which is where the defects that survived unit tests have all lived: a
// message arrives on the wire, capacity is escrowed, a lease is bound, a
// registration is minted, a container starts, the job completes, the container
// is destroyed and the capacity comes back. Each of those steps is covered
// elsewhere; that they compose is only covered here.
//
// The GitHub side is fake and the docker side is REAL. That split is deliberate:
// billet's relationship with GitHub is a protocol it can be lied to about
// safely, while its relationship with a container runtime is one where the
// interesting failures come from the runtime actually behaving like itself.
package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/provider/docker"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wiring"
)

// A registered host in these tests is deliberately larger than any budget they
// set, so the deployment-wide ceiling stays the binding constraint and every
// test written before nodes carried capacity keeps measuring what it did.
const (
	testNodeVCPU   = 1 << 20
	testNodeMemory = 1 << 20 * config.GiB
)

// plane is a scripted Actions service: it holds a queue of messages a test can
// push to, and records what billet acquired and acknowledged.
//
// Scripted rather than simulated. It does not model GitHub's assignment
// behaviour — it serves exactly what a test queues, so an assertion is about
// billet's response to a stated situation rather than about a guess at GitHub's.
type plane struct {
	*fakeactions.Server

	t *testing.T

	mu        sync.Mutex
	queued    []map[string]any
	nextMsgID int
	acquired  []int64
	deleted   []int64
	setID     int

	// exists models whether the scale set has been created yet. Without it the
	// service reported the set as already present on the very first GET, so
	// billet correctly ADOPTED it and never issued a create — and a test asserting
	// on the create body was asserting on nothing.
	exists           bool
	registeredRunner string
}

const (
	testTier  = "billet-2vcpu-ubuntu-2404"
	testGroup = "billet"

	// Not the real runner image: the assertions are about billet starting and
	// stopping the right container, and a 2GB pull would turn this into a network
	// test that fails for reasons the code cannot cause.
	//
	// It does have to STAY RUNNING, though, which busybox does not — its default
	// command exits immediately, so recovery correctly treated every container as
	// a finished job and destroyed it. A real runner occupies its container for
	// the length of the job, and adoption is meaningless without that.
	testImage = "nginx:alpine"
)

func newPlane(t *testing.T) *plane {
	t.Helper()

	p := &plane{t: t, nextMsgID: 1, setID: 7}
	p.Server = fakeactions.New(t, p.route)

	return p
}

// route answers the scale-set API. The auth handshake is answered upstream by
// the shared fake, so everything here is protocol proper.
func (p *plane) route(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	switch {
	case strings.Contains(path, "runnergroups"):
		fakeactions.WriteJSON(p.t, w, fakeactions.ListJSON(
			map[string]any{"id": 1, "name": testGroup, "isDefaultGroup": false}))

	case strings.Contains(path, "/actions/runner-groups/1"):
		fakeactions.WriteJSON(p.t, w, map[string]any{
			"restricted_to_workflows": true,
			"selected_workflows":      []string{"acme/test/.github/workflows/e2e.yml@refs/heads/main"},
		})

	case strings.Contains(path, "acquirejobs"):
		p.acquireJobs(w, r)

	case strings.Contains(path, "generatejitconfig"):
		p.generateJIT(w, r)

	case r.Method == http.MethodGet && strings.Contains(path, "/agents"):
		p.mu.Lock()
		name := p.registeredRunner
		p.mu.Unlock()
		if name == "" || r.URL.Query().Get("agentName") != name {
			fakeactions.WriteJSON(p.t, w, map[string]any{"count": 0, "value": []any{}})
			return
		}
		fakeactions.WriteJSON(p.t, w, map[string]any{"count": 1,
			"value": []map[string]any{{"id": 99, "name": name, "runnerScaleSetId": p.setID}}})

	case r.Method == http.MethodDelete && strings.Contains(path, "/agents/99"):
		p.mu.Lock()
		p.registeredRunner = ""
		p.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)

	case strings.HasSuffix(path, "/sessions"):
		fakeactions.WriteJSON(p.t, w, fakeactions.SessionJSON(
			"3f8a1c22-0000-4000-8000-000000000001", "billet-test",
			p.scaleSet(), p.URL+"/queue", "queue-token"))

	// Session teardown. The path is /sessions/{uuid}, so it must be matched
	// before the collection above would swallow it — hence HasSuffix there.
	case strings.Contains(path, "/sessions/"):
		w.WriteHeader(http.StatusNoContent)

	case r.Method == http.MethodGet && strings.Contains(path, "runnerscalesets"):
		p.mu.Lock()
		exists := p.exists
		p.mu.Unlock()

		if !exists {
			fakeactions.WriteJSON(p.t, w, fakeactions.ListJSON())

			return
		}

		fakeactions.WriteJSON(p.t, w, fakeactions.ListJSON(p.scaleSet()))

	case r.Method == http.MethodPost && strings.Contains(path, "runnerscalesets"):
		p.mu.Lock()
		p.exists = true
		p.mu.Unlock()

		fakeactions.WriteJSON(p.t, w, p.scaleSet())

	case r.Method == http.MethodDelete && strings.HasPrefix(path, "/queue/"):
		p.deleteMessage(w, path)

	case strings.HasPrefix(path, "/queue"):
		p.getMessage(w)

	default:
		p.t.Errorf("unexpected call %s %s", r.Method, path)
		w.WriteHeader(http.StatusNotFound)
	}
}

// scaleSet is what the service reports for this tier.
//
// Exactly the labels billet asks for, and no more. An extra one here is not a
// harmless embellishment: reconciliation refuses to adopt a set whose labels it
// did not set, so a fake that adds "self-hosted" makes billet correctly reject
// the very set the fake just handed it — a failure that reads like billet's bug
// and is entirely the fake's.
func (p *plane) scaleSet() map[string]any {
	return fakeactions.ScaleSetJSON(p.setID, testTier, testGroup, testTier)
}

// getMessage serves the head of the queue, or 202 for "nothing right now".
//
// 202 IS THE ORDINARY ANSWER, not an error: the real service holds the
// connection open for the poll interval and then says nothing happened. A fake
// that returned 200-with-empty instead would let billet mishandle the common
// case undetected.
//
// THE HEAD IS NOT REMOVED HERE. An unacknowledged message is REDELIVERED — that
// is the vendored client's stated contract and the reason DeleteMessage exists —
// so a fake that popped on read could never catch a missing acknowledgement, and
// the test claiming to check for one passed against a billet that never acked.
// The message goes when its id is deleted.
func (p *plane) getMessage(w http.ResponseWriter) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.queued) == 0 {
		w.WriteHeader(http.StatusAccepted)

		return
	}

	fakeactions.WriteJSON(p.t, w, p.queued[0])
}

// deleteMessage acknowledges the head of the queue and drops it.
func (p *plane) deleteMessage(w http.ResponseWriter, path string) {
	id, err := strconv.ParseInt(strings.TrimPrefix(path, "/queue/"), 10, 64)
	if err != nil {
		p.t.Errorf("acknowledged a message with an unreadable id: %s", path)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	p.deleted = append(p.deleted, id)

	// Only the message actually acknowledged is dropped. Acking an id that is not
	// the head means the cursor has run ahead of the work, which is the failure
	// that loses a job silently.
	head, ok := 0, false

	if len(p.queued) > 0 {
		head, ok = p.queued[0]["messageId"].(int)
	}

	if ok && int64(head) == id {
		p.queued = p.queued[1:]
	} else {
		p.t.Errorf("acknowledged message %d, which is not the head of the queue", id)
	}

	w.WriteHeader(http.StatusNoContent)
}

// acquireJobs records what billet bid for and grants all of it.
func (p *plane) acquireJobs(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		p.t.Errorf("read acquire body: %v", err)
	}

	var ids []int64

	if err := json.Unmarshal(body, &ids); err != nil {
		p.t.Errorf("acquire body is not a list of ids: %s", body)
	}

	p.mu.Lock()
	p.acquired = append(p.acquired, ids...)
	p.mu.Unlock()

	fakeactions.WriteJSON(p.t, w, map[string]any{"count": len(ids), "value": ids})
}

func (p *plane) generateJIT(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		WorkFolder string `json:"workFolder"`
	}

	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		p.t.Errorf("decode jit request: %v", err)
	}
	p.mu.Lock()
	p.registeredRunner = req.Name
	p.mu.Unlock()

	fakeactions.WriteJSON(p.t, w, fakeactions.JitConfigJSON(99, req.Name, "encoded-jit-config"))
}

// queue pushes one message envelope containing the given job messages.
func (p *plane) queue(stats map[string]any, jobs ...map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.queued = append(p.queued, fakeactions.MessageJSON(p.t, p.nextMsgID, stats, jobs...))
	p.nextMsgID++
}

// ackedIDs reports the messages billet has acknowledged, in order.
func (p *plane) ackedIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]int64(nil), p.deleted...)
}

func (p *plane) acquiredIDs() []int64 {
	p.mu.Lock()
	defer p.mu.Unlock()

	return append([]int64(nil), p.acquired...)
}

// ---------------------------------------------------------------- the stack --

// stack is billet, assembled the way cmd/billet assembles it.
type stack struct {
	// dir is the state directory, so a restart can be built over it.
	dir string

	// closeDB releases the state lock, which the next incarnation needs before it
	// can open the same directory. Idempotent — the cleanup calls it too.
	closeDB func()

	plane  *plane
	alloc  *alloc.Allocator
	runner *node.Runner
	// tiers is the catalogue this stack was built with, so a test driving the
	// runner directly can send the shape the control plane would have sent.
	tiers    []config.Tier
	server   *server.Server
	provider *docker.Provider
	node     string
}

func newStack(t *testing.T, opts ...stackOpt) *stack {
	t.Helper()

	return newStackIn(t, t.TempDir(), newPlane(t), opts...)
}

// newWireStack builds the same stack with the node REACHED OVER THE WIRE.
//
// The one place a real control plane, a real HTTP listener, a real node loop and
// a real Docker daemon are all in play at once. It matters because the wire is
// the only part with no in-process equivalent to fall back on: every unit test
// of it necessarily supplies one side.
//
// It used to be possible to skip this path entirely: `server --dev` ran a node
// in the control plane's own process. With that gone the wire carries every
// deployment shape billet has, including the single-machine one.
func newWireStack(t *testing.T) *stack {
	t.Helper()

	return newStackIn(t, t.TempDir(), newPlane(t), overTheWire)
}

// stackOpt varies how a stack is assembled.
type stackOpt func(*stackConfig)

type stackConfig struct {
	wire      bool
	untrusted bool
}

// directRunner drives the node runtime without a socket between it and the
// control plane.
//
// A TEST HARNESS, NOT A DEPLOYMENT SHAPE. billet has no in-process node any
// more, so the only thing that supplies a launch with its tier shape in
// production is the plane's dispatch. This stands in for that, which is exactly
// why the wire has its own end-to-end stack: a bug in what the plane puts ON the
// command is invisible from here.
type directRunner struct {
	runner *node.Runner
	tiers  []config.Tier
}

func (d directRunner) Launch(ctx context.Context, lease *alloc.Lease, job server.Job) error {
	for i := range d.tiers {
		if d.tiers[i].Label == lease.Tier {
			return d.runner.Launch(ctx, lease,
				nodeapi.TierSpecOf(d.tiers[i], config.ProviderDocker), job)
		}
	}

	return fmt.Errorf("e2e: no tier %q in the test catalogue", lease.Tier)
}

func (d directRunner) Destroy(ctx context.Context, requestID int64) error {
	return d.runner.Destroy(ctx, requestID)
}

func (d directRunner) Sweep(ctx context.Context) error { return d.runner.Sweep(ctx) }
func (d directRunner) Tend(ctx context.Context) error  { return d.runner.Tend(ctx) }
func (d directRunner) KeepAlive(ctx context.Context)   { d.runner.KeepAlive(ctx) }

// overTheWire puts a real node wire between the control plane and the runner.
func overTheWire(c *stackConfig) { c.wire = true }

// untrustedPool makes a test tier model a pool where any admitted workflow may
// be hostile. Docker then has to refuse it before a registration is minted.
func untrustedPool(c *stackConfig) { c.untrusted = true }

// newStackIn builds a stack over a GIVEN state directory and service.
//
// Restarting billet is exactly this: a new process over the same state and the
// same deployment identity, with empty in-memory maps. A test that built a fresh
// state directory instead would be testing a first run, which is the case where
// there is nothing to recover.
func newStackIn(t *testing.T, dir string, p *plane, opts ...stackOpt) *stack {
	t.Helper()

	var sc stackConfig

	for _, o := range opts {
		o(&sc)
	}

	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker is not installed")
	}

	client, err := scaleset.New(scaleset.Config{
		ConfigURL:      p.URL + "/acme",
		ClientID:       "12345",
		InstallationID: 67890,
		PrivateKey:     p.PrivateKeyPEM(),
		Org:            "acme",
		AppID:          12345,
		APIURL:         p.URL + "/api/v3",
	}, nil)
	if err != nil {
		t.Fatalf("scaleset.New: %v", err)
	}

	db, err := state.Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	closeDB := sync.OnceFunc(func() { _ = db.Close() })

	t.Cleanup(closeDB)

	tiers := []config.Tier{{
		Label:       testTier,
		Provider:    config.ProviderDocker,
		VCPU:        2,
		Memory:      2 * config.GiB,
		Disk:        10 * config.GiB,
		Image:       testImage,
		RunnerGroup: testGroup,
		GuestOS:     config.GuestLinux,
		Trust:       config.WorkloadTrusted,
		Workflows:   []string{"acme/test/.github/workflows/e2e.yml@refs/heads/main"},

		// SAID EXPLICITLY, because this is not a runner image. These tests are
		// about the plane's lifecycle — a container that starts, stays up, and is
		// destroyed — so what runs inside only has to keep running. The stock
		// default (./run.sh) does not exist in nginx:alpine, and a container that
		// exits at once fails every "is it still running" assertion here for a
		// reason that has nothing to do with what is being tested.
		Command: []string{"sleep", "300"},
	}}
	if sc.untrusted {
		tiers[0].Trust = config.WorkloadUntrusted
		tiers[0].Workflows = nil
	}

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	const host = "e2e-host"

	// PRE-REGISTERED ONLY FOR THE IN-PROCESS STACK. Over the wire the plane's own
	// registration must write this row, so doing it here would hide a regression
	// that stopped it — the node would bind happily against a row this harness
	// had helpfully created.
	if !sc.wire {
		if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{Name: host, Provider: config.ProviderDocker, VCPU: testNodeVCPU, Memory: testNodeMemory}); err != nil {
			t.Fatalf("RegisterNode: %v", err)
		}
	}

	// A per-test deployment identity, so two of these running concurrently do not
	// enumerate each other's containers. This is the property state.DeploymentID
	// exists for, exercised here rather than asserted about.
	deployment, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	prov := docker.New(deployment)

	// Whatever this test leaves behind goes with it, even on a failure path. Safe
	// to register per incarnation: it runs after the test body, so nothing it
	// destroys is still under assertion.
	t.Cleanup(func() {
		// BOUNDED, BECAUSE THIS TALKS TO A REAL CONTAINER RUNTIME. Every wait in
		// these tests carries a deadline, but this does not wait — it CALLS, and a
		// loaded or wedged Docker daemon answers whenever it feels like it. With no
		// deadline the call blocks until the whole test binary is killed, which
		// reports as a package that ran for six minutes and a goroutine dump,
		// rather than as one sweep that could not reach the daemon.
		//
		// WithoutCancel is still right — cleanup has to outlive the test's own
		// context — but WithoutCancel alone removes the deadline as well, which is
		// how something that only ever waits a moment became something that can
		// wait forever.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 90*time.Second)
		defer cancel()

		instances, err := prov.List(ctx)
		if err != nil {
			t.Logf("the container sweep could not list what this test left behind, so some "+
				"may survive it: %v", err)

			return
		}

		for _, inst := range instances {
			if _, err := prov.Destroy(ctx, inst.ID); err != nil {
				t.Logf("the container sweep could not remove %s: %v", inst.ID, err)
			}
		}
	})

	log := testLogger(t)

	var (
		runner      *node.Runner
		serverOpts  []server.ControlPlaneOption
		computeName = host
	)

	if sc.wire {
		runner, serverOpts = wireUp(t, log, a, client, prov, tiers, deployment, computeName)
	} else {
		runner = node.New(a, host, wiring.JITSource{Client: client, Pool: a}, prov, log)
		serverOpts = []server.ControlPlaneOption{
			server.WithNodeRunner(directRunner{runner: runner, tiers: tiers}),
		}
	}

	serverOpts = append(serverOpts,
		// Fast, because the sweep rides this tick and a test that waits a minute
		// for it is a test nobody runs.
		server.WithReapInterval(200*time.Millisecond),
		// AND A DRAIN THESE TESTS DO NOT WAIT OUT.
		//
		// Stopping the plane now begins a drain, and these scenarios stop it while
		// a container is deliberately still running — the fake GitHub never sends
		// the completion, because the point of the scenario was what happens
		// BEFORE one. On the real six-hour default the plane would correctly sit
		// there waiting.
		//
		// The drain has its own end-to-end coverage; here it is scenery, so it is
		// made short rather than the production default being lowered to keep the
		// suite quick.
		server.WithDrainTimeout(200*time.Millisecond))

	srv := server.New(a, wiring.Provisioner{Client: client}, tiers, "billet-test", log, serverOpts...)

	return &stack{
		dir: dir, closeDB: closeDB, plane: p, alloc: a,
		runner: runner, server: srv, provider: prov, node: host, tiers: tiers,
	}
}

// wireUp puts a real HTTP node wire between the control plane and the runner.
func wireUp(
	t *testing.T,
	log *slog.Logger,
	a *alloc.Allocator,
	client *scaleset.Client,
	prov *docker.Provider,
	tiers []config.Tier,
	deployment, host string,
) (*node.Runner, []server.ControlPlaneOption) {
	t.Helper()

	plane := nodeplane.New(log, deployment, a.LeaseTTL(),
		nodeplane.WithRegistrar(a), nodeplane.WithTierCatalog(tiers))

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	wire := &http.Server{
		Handler:           nodeplane.Handler(log, plane, a, wiring.NodeJIT{Client: client}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		// Ends when Shutdown is called, which is the only way this test stops it.
		if err := wire.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("the node wire stopped unexpectedly: %v", err)
		}
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(t.Context()), 5*time.Second)
		defer cancel()

		if err := wire.Shutdown(ctx); err != nil {
			t.Errorf("the node wire did not shut down cleanly: %v", err)
		}
	})

	nc, err := nodeclient.New(nodeclient.Options{Base: "http://" + ln.Addr().String(), Node: host})
	if err != nil {
		t.Fatalf("nodeclient.New: %v", err)
	}

	// THE CLIENT IS BOTH LEDGER AND MINT, exactly as `billet node` wires it. The
	// runner has no idea it became remote.
	runner := node.New(nc, host, nc, prov, log)

	ctx, cancel := context.WithCancel(t.Context())

	var loop sync.WaitGroup

	loop.Add(1)

	go func() {
		defer loop.Done()

		// Run only ends when the context does; anything else is a real failure.
		err := nodeclient.Run(ctx, nc, runner, nodeclient.LoopOptions{
			Provider:   config.ProviderDocker,
			Deployment: deployment,
			// What this host contributes. Required now: a node reporting nothing is
			// refused rather than joining the fleet and never being chosen.
			VCPU:    testNodeVCPU,
			Memory:  testNodeMemory,
			Log:     log,
			Backoff: 50 * time.Millisecond,
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("the node loop stopped for a reason other than shutdown: %v", err)
		}
	}()

	t.Cleanup(func() {
		cancel()
		loop.Wait()
	})

	// REGISTERED BEFORE THE CONTROL PLANE HAS ANYTHING TO GIVE IT. Otherwise the
	// first launch legitimately finds no node and the test measures startup order
	// rather than the wire.
	deadline := time.Now().Add(30 * time.Second)
	for len(plane.Nodes()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the node never registered over the wire")
		}

		time.Sleep(20 * time.Millisecond)
	}

	return runner, []server.ControlPlaneOption{server.WithNodeRunner(plane.NewRunner())}
}

// run starts the control plane and returns a stop function.
//
// A Run that fails is reported IMMEDIATELY rather than at stop. The first
// version of this harness only read the error when the test tore down, so a
// control plane that died during startup showed up as whatever assertion timed
// out first — thirty seconds later, naming the wrong thing. Debugging that
// misdirection cost more than the harness.
func (s *stack) run(t *testing.T) func() {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())

	var runErr error

	// CLOSED, not sent on. A channel that carries the error can only be read
	// once, so the watchdog below and the stop function would compete for it —
	// which is how the first version reported "the control plane did not stop"
	// on top of a failure it had already diagnosed correctly.
	finished := make(chan struct{})

	go func() {
		runErr = s.server.Run(ctx)

		close(finished)
	}()

	stopped := make(chan struct{})

	go func() {
		select {
		case <-finished:
			select {
			case <-stopped:
			default:
				// A Run that returns before it is asked to is a failure whatever
				// the error says, including nil.
				t.Errorf("the control plane stopped on its own: %v", runErr)
			}
		case <-stopped:
		}
	}()

	return func() {
		close(stopped)
		cancel()

		select {
		case <-finished:
		case <-time.After(30 * time.Second):
			t.Error("the control plane did not stop")
		}
	}
}

// awaitRunning waits for exactly `want` RUNNING containers and returns their
// names.
//
// Running, not merely present. `docker ps --all` lists exited containers too, so
// counting everything let a test pass while the "runner" had already died.
func (s *stack) awaitRunning(t *testing.T, want int) []string {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)

	for {
		instances, err := s.provider.List(t.Context())
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		names := make([]string, 0, len(instances))

		for _, inst := range instances {
			if inst.Running {
				names = append(names, inst.Name)
			}
		}

		if len(names) == want {
			return names
		}

		if time.Now().After(deadline) {
			t.Fatalf("waited for %d running container(s), have %d (%d in any state)",
				want, len(names), len(instances))
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// awaitGone waits until NOTHING remains, in any state.
//
// The right assertion for cleanup, and a genuinely different question from
// awaitRunning(0). A container that has merely been STOPPED still holds its
// name, its disk and its anonymous volumes, and still blocks a relaunch under
// that name — so a regression that stopped containers instead of removing them
// would pass a running-count check while leaking every job's disk.
func (s *stack) awaitGone(t *testing.T) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)

	for {
		instances, err := s.provider.List(t.Context())
		if err != nil {
			t.Fatalf("List: %v", err)
		}

		if len(instances) == 0 {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("containers were left behind in some state: %v", instances)
		}

		time.Sleep(100 * time.Millisecond)
	}
}

// ------------------------------------------------------------- the adapters --

// testLogger writes the control plane's diagnostics into the test log, where
// they appear only for a failing test. A discarded logger made the first
// failure here unreadable.
func testLogger(t *testing.T) *slog.Logger {
	t.Helper()

	return slog.New(slog.NewTextHandler(testWriter{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)

	return len(p), nil
}
