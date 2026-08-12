package nodeplane

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

const deployment = "0123456789abcdef0123456789abcdef"

// testClock is a clock a test can move.
//
// ATOMIC BECAUSE THE PLANE READS IT FROM ITS OWN GOROUTINES. A closure over a
// plain variable is the obvious shape and it is a data race the moment the code
// under test consults the clock anywhere but the test's own goroutine — which is
// exactly what expiry does, from inside a broadcast.
type testClock struct {
	nanos atomic.Int64
}

func newTestClock() *testClock {
	c := &testClock{}
	c.nanos.Store(time.Now().UnixNano())

	return c
}

func (c *testClock) now() time.Time { return time.Unix(0, c.nanos.Load()) }

// advancePastSilence moves the clock beyond any node's silence window, which is
// the only jump these tests need: every one of them is asking what happens to a
// node the plane would now consider gone.
func (c *testClock) advancePastSilence() { c.nanos.Add(int64(10 * time.Minute)) }

func testPlane(t *testing.T, opts ...Option) *Plane {
	t.Helper()

	// WITH A CATALOGUE, because a launch carries the tier's shape to the node and
	// the plane refuses one it cannot describe. A test plane without it fails
	// every launch before dispatch, which reads as "the command was never
	// delivered".
	opts = append([]Option{WithTierCatalog([]config.Tier{testTier()})}, opts...)

	return New(slog.New(slog.DiscardHandler), deployment, time.Minute, opts...)
}

// testTier is the catalogue entry testLease names.
func testTier() config.Tier {
	return config.Tier{
		Label: "billet-2vcpu", Provider: config.ProviderDocker, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 8 * config.GiB, Image: "ubuntu-2404-x64",
	}
}

func register(t *testing.T, p *Plane, name string, provider config.ProviderKind) {
	t.Helper()

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:    nodeapi.Version,
		Node:       name,
		Provider:   provider,
		Deployment: deployment,
		// What this host contributes. The plane refuses a registration offering
		// nothing, so every test node has to say something.
		VCPU:   8,
		Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
}

func testLease() *alloc.Lease {
	return &alloc.Lease{
		ID:        "l1",
		Tier:      "billet-2vcpu",
		VCPU:      2,
		Memory:    8 * config.GiB,
		GuestOS:   config.GuestLinux,
		Providers: []config.ProviderKind{config.ProviderDocker},
		Epoch:     1,
		RequestID: 7,
	}
}

// A LAUNCH NOBODY TOOK IS NOT A LAUNCH THAT FAILED AMBIGUOUSLY.
//
// The distinction is the whole reason ErrNoNode exists: nothing was sent, so
// nothing started, and the caller may release the lease. Reporting custody here
// would strand capacity forever waiting on compute that does not exist.
func TestALaunchWithNoNodeStartedNothing(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	err := p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	if !errors.Is(err, ErrNoNode) {
		t.Fatalf("want ErrNoNode, got %v", err)
	}

	if errors.Is(err, server.ErrCustody) {
		t.Error("a launch that was never sent reported custody, which holds capacity for " +
			"compute that cannot exist")
	}
}

// A NODE THAT TOOK THE COMMAND AND WENT SILENT MEANS CUSTODY.
//
// This is the outcome a local runner never has and the one that decides whether
// capacity is safe to reuse. The command is executing or it is not, and the
// control plane cannot tell — so it must assume something is running and let the
// node's own recovery find it.
func TestASilentNodeLeavesTheLeaseInCustody(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(150*time.Millisecond))
	register(t, p, "n1", config.ProviderDocker)

	// The node takes the command and never answers.
	go func() {
		if _, _, err := p.Poll(t.Context(), "n1", ""); err != nil {
			return
		}
	}()

	err := p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("a delivered launch that went unanswered must report custody, got %v", err)
	}

	if errors.Is(err, ErrNoNode) {
		t.Error("it also claimed nothing was sent, which is the opposite of what happened")
	}
}

// A LAUNCH STILL IN THE QUEUE STARTED NOTHING, even when the wait expires.
//
// The mirror of the test above, and the reason `delivered` is tracked at all: a
// command no node ever took cannot have run, so the caller gets the certainty it
// is entitled to rather than a permanent maybe.
func TestAnUndeliveredLaunchIsNotCustody(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(100*time.Millisecond))
	register(t, p, "n1", config.ProviderDocker)

	// Nobody polls, so the command sits in the queue.
	err := p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	if !errors.Is(err, ErrNoNode) {
		t.Fatalf("want ErrNoNode for a command no node took, got %v", err)
	}

	if errors.Is(err, server.ErrCustody) {
		t.Error("a command that never left the queue reported custody")
	}
}

// The node's own verdict survives the wire, in both directions.
func TestANodesVerdictIsCarriedBack(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		res        nodeapi.CommandResult
		wantErr    bool
		wantCustdy bool
	}{
		{name: "success", res: nodeapi.CommandResult{OK: true}},
		{
			name:    "clean failure releases",
			res:     nodeapi.CommandResult{Error: "image not found"},
			wantErr: true,
		},
		{
			name:       "custody failure holds",
			res:        nodeapi.CommandResult{Error: "create ambiguous", Custody: true},
			wantErr:    true,
			wantCustdy: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := testPlane(t, WithCommandTimeout(2*time.Second))
			register(t, p, "n1", config.ProviderDocker)

			go func() {
				cmd, ok, err := p.Poll(t.Context(), "n1", "")
				if err != nil || !ok {
					return
				}

				res := tc.res
				res.ID = cmd.ID

				if err := p.Result("n1", "", res); err != nil {
					t.Errorf("Result: %v", err)
				}
			}()

			err := p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})

			if tc.wantErr && err == nil {
				t.Fatal("the node's failure did not reach the caller")
			}

			if !tc.wantErr && err != nil {
				t.Fatalf("the node succeeded but the caller saw: %v", err)
			}

			if got := errors.Is(err, server.ErrCustody); got != tc.wantCustdy {
				t.Errorf("custody = %v, want %v (err: %v)", got, tc.wantCustdy, err)
			}
		})
	}
}

// A RE-REGISTRATION IS A RESTART, and its in-flight launches become custody.
//
// The node that took them is gone and will never report, so a caller waiting
// forever is the alternative. They are NOT retried: the command may have started
// a container, and a retry would risk a second one for a single job.
func TestARestartedNodeLeavesItsLaunchesInCustody(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(60*time.Second))
	register(t, p, "n1", config.ProviderDocker)

	taken := make(chan struct{})

	go func() {
		if _, _, err := p.Poll(t.Context(), "n1", ""); err == nil {
			close(taken)
		}
	}()

	launched := make(chan error, 1)

	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	select {
	case <-taken:
	case <-time.After(60 * time.Second):
		t.Fatal("the node never took the command")
	}

	register(t, p, "n1", config.ProviderDocker)

	select {
	case err := <-launched:
		if !errors.Is(err, server.ErrCustody) {
			t.Fatalf("a launch lost to a node restart must report custody, got %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the caller was left waiting for a node that will never answer")
	}
}

// A node from another installation is refused.
//
// It labels its compute with its own deployment identity, so accepting it would
// produce containers this installation cannot attribute — which is exactly what
// the orphan sweeper would then find and be unable to reason about.
func TestANodeFromAnotherDeploymentIsRefused(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:    nodeapi.Version,
		Node:       "n1",
		Provider:   config.ProviderDocker,
		Deployment: "ffffffffffffffffffffffffffffffff",
	})
	if err == nil {
		t.Fatal("a node from another deployment was accepted")
	}
}

// A protocol mismatch is a refusal with a readable message, not a decode error
// halfway through a launch.
func TestAVersionMismatchIsRefused(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	// BOTH DIRECTIONS, AND ONE OF THEM IS THE REAL CASE. A node OLDER than the
	// server is what actually happens during a rolling upgrade — version 1
	// reports no capacity and no site — while a newer node is the rarer reverse.
	// Testing only Version+1 left the case an operator will actually hit
	// unasserted.
	for _, version := range []int{1, nodeapi.Version + 1} {
		_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version:    version,
			Node:       "n1",
			Deployment: deployment,
			VCPU:       8,
			Memory:     32 * config.GiB,
		})
		if err == nil {
			t.Fatalf("a node speaking protocol version %d was accepted", version)
		}

		// PERMANENT, which is the half that matters. A node retries anything that
		// might heal; this cannot, so a refusal reported as an outage would have
		// the node reconnecting forever instead of telling somebody to upgrade.
		if !errors.Is(err, ErrRefused) {
			t.Errorf("version %d was not refused permanently: %v", version, err)
		}

		// The diagnostic has to name both numbers, because "upgrade whichever is
		// older" is not actionable without knowing which one that is.
		if !strings.Contains(err.Error(), strconv.Itoa(version)) ||
			!strings.Contains(err.Error(), strconv.Itoa(nodeapi.Version)) {
			t.Errorf("the refusal does not name both versions: %v", err)
		}
	}
}

// PREFERENCE ORDER IS THE LEASE'S, NOT THE MAP'S.
//
// Providers is most-preferred-first, and picking by ranging a map would choose
// by hash order — which looks correct in a one-node test and silently ignores
// the preference the moment a second node exists.
func TestTheLeasesPreferenceOrderDecides(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(2*time.Second))
	register(t, p, "docker-host", config.ProviderDocker)
	register(t, p, "ec2-host", config.ProviderEC2)

	lease := testLease()
	lease.Providers = []config.ProviderKind{config.ProviderEC2, config.ProviderDocker}

	got := make(chan string, 1)

	for _, name := range []string{"docker-host", "ec2-host"} {
		go func() {
			cmd, ok, err := p.Poll(t.Context(), name, "")
			if err != nil || !ok {
				return
			}

			got <- name

			if err := p.Result(name, "", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
				t.Errorf("Result: %v", err)
			}
		}()
	}

	if err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	select {
	case name := <-got:
		if name != "ec2-host" {
			t.Errorf("the lease prefers ec2 first and the command went to %s", name)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no node received the command")
	}
}

// A pinned lease goes to its node or nowhere.
//
// THE OTHER NODE ANSWERS, and that detail is the test. An earlier version left
// it idle, so a lease that wandered still timed out and still produced
// ErrNoNode — the assertion passed whether or not the pin was honoured, and
// removing the pin left it green. A node that would happily succeed is the only
// way to tell "refused to wander" from "wandered and nobody was listening".
func TestAPinnedLeaseWillNotWander(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(2*time.Second))
	register(t, p, "other", config.ProviderDocker)

	go func() {
		cmd, ok, err := p.Poll(t.Context(), "other", "")
		if err != nil || !ok {
			return
		}

		if err := p.Result("other", "", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
			t.Errorf("Result: %v", err)
		}
	}()

	lease := testLease()
	lease.TargetNode = "mac-mini-1"

	err := p.NewRunner().Launch(t.Context(), lease, server.Job{RequestID: 7})
	if err == nil {
		t.Fatal("a lease pinned to an absent node was run on a different one")
	}

	if !errors.Is(err, ErrNoNode) {
		t.Fatalf("a lease pinned to an absent node: want ErrNoNode, got %v", err)
	}
}

// Polling as a node the plane has never heard of is refused, and that is what a
// node sees after the control plane restarts.
func TestAnUnregisteredNodeIsToldToRegister(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	if _, _, err := p.Poll(t.Context(), "ghost", ""); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("want ErrUnregistered, got %v", err)
	}

	if err := p.Result("ghost", "", nodeapi.CommandResult{ID: "x"}); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("want ErrUnregistered from Result, got %v", err)
	}
}

// An idle poll returns nothing rather than an error, which is how a quiet fleet
// stays quiet.
func TestAnIdlePollIsNotAnError(t *testing.T) {
	t.Parallel()

	p := testPlane(t)
	p.poll = 50 * time.Millisecond

	register(t, p, "n1", config.ProviderDocker)

	cmd, ok, err := p.Poll(t.Context(), "n1", "")
	if err != nil {
		t.Fatalf("an idle poll reported an error: %v", err)
	}

	if ok {
		t.Errorf("an idle poll produced a command: %+v", cmd)
	}
}

// ONE WEDGED NODE MUST NOT HOLD UP A DESTROY ON THE OTHERS.
//
// Asking each node in turn lets a single host that never answers block the whole
// call for the command timeout before the next is asked — and Destroy runs on
// shutdown and completion paths, where that turns one bad host into a stalled
// control plane.
//
// The timing assertion is deliberately loose: it proves "concurrent, not serial".
func TestOneWedgedNodeDoesNotStallADestroy(t *testing.T) {
	t.Parallel()

	const timeout = 600 * time.Millisecond

	p := testPlane(t, WithCommandTimeout(timeout))

	for _, name := range []string{"wedged-a", "wedged-b", "wedged-c"} {
		register(t, p, name, config.ProviderDocker)

		// Each takes its command and never answers.
		go func() {
			if _, _, err := p.Poll(t.Context(), name, ""); err != nil {
				return
			}
		}()
	}

	start := time.Now()

	err := p.NewRunner().Destroy(t.Context(), 7)
	if err == nil {
		t.Fatal("three silent nodes reported a successful destroy")
	}

	// Serial would be at least three timeouts; concurrent is about one.
	if elapsed := time.Since(start); elapsed > 2*timeout {
		t.Errorf("the destroy took %v against three nodes with a %v timeout, which means it "+
			"asked them one at a time", elapsed, timeout)
	}
}

// EVERY FAILING NODE IS NAMED, not just the first.
//
// A destroy that worked on four hosts and failed on one has left compute running
// somewhere specific, and reporting only the first failure hides the rest behind
// whichever goroutine finished soonest.
func TestADestroyReportsEveryNodeThatFailed(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(3*time.Second))

	for _, name := range []string{"n1", "n2"} {
		register(t, p, name, config.ProviderDocker)

		go func() {
			cmd, ok, err := p.Poll(t.Context(), name, "")
			if err != nil || !ok {
				return
			}

			if err := p.Result(name, "", nodeapi.CommandResult{
				ID:    cmd.ID,
				Error: "docker refused on " + name,
			}); err != nil {
				t.Errorf("Result: %v", err)
			}
		}()
	}

	err := p.NewRunner().Destroy(t.Context(), 7)
	if err == nil {
		t.Fatal("two failed destroys reported success")
	}

	for _, want := range []string{"n1", "n2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s, so an operator cannot tell which host still "+
				"has compute on it: %v", want, err)
		}
	}
}

// A NODE THAT WENT SILENT IS FORGOTTEN, and until it was, one dead machine
// leaked capacity forever.
//
// lastSeen was written and never read, so a host unplugged a week ago stayed in
// the fleet. Every Destroy broadcast then waited the full command timeout
// against it and returned an error — and the listener answers a failed destroy
// by holding its lease and heartbeating it indefinitely. So every completed job
// after that machine died kept its capacity, permanently.
func TestASilentNodeIsForgotten(t *testing.T) {
	t.Parallel()

	clock := newTestClock()

	p := testPlane(t, WithClock(clock.now))
	register(t, p, "n1", config.ProviderDocker)

	if len(p.Nodes()) != 1 {
		t.Fatalf("the node did not register: %v", p.Nodes())
	}

	// Silent for longer than the plane tolerates.
	clock.advancePastSilence()

	if got := p.Nodes(); len(got) != 0 {
		t.Fatalf("a node silent for 10 minutes is still in the fleet: %v", got)
	}
}

// A STALE NODE DOES NOT HOLD UP A DESTROY, which is the damage its presence did.
func TestADestroySkipsAForgottenNode(t *testing.T) {
	t.Parallel()

	clock := newTestClock()

	// A long command timeout on purpose: if the stale node were still consulted,
	// the destroy would block on it for this long and the test would time out
	// rather than merely fail.
	p := testPlane(t, WithClock(clock.now), WithCommandTimeout(time.Hour))
	register(t, p, "dead", config.ProviderDocker)

	clock.advancePastSilence()

	done := make(chan error, 1)

	go func() { done <- p.NewRunner().Destroy(t.Context(), 7) }()

	select {
	case err := <-done:
		// No live nodes, so there is nowhere for the compute to be and nothing to
		// remove — which is success, not failure.
		if err != nil {
			t.Fatalf("a destroy against a fleet of one dead node: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destroy waited on a node that has been silent for ten minutes")
	}
}

// A NODE WITH WORK IN FLIGHT IS BUSY, NOT SILENT.
//
// The node loop executes commands synchronously, so a host pulling a five-minute
// image does not poll while it works. Expiring it answers an in-flight launch with
// custody, the listener stops heartbeating, and the lease is reaped before the
// provider has returned — after which the launch starts a runner on capacity already
// sold. The command timeout bounds this instead.
func TestABusyNodeIsNotForgotten(t *testing.T) {
	t.Parallel()

	clock := newTestClock()

	p := testPlane(t, WithClock(clock.now), WithCommandTimeout(time.Hour))
	register(t, p, "n1", config.ProviderDocker)

	taken := make(chan struct{})

	go func() {
		if _, _, err := p.Poll(t.Context(), "n1", ""); err == nil {
			close(taken)
		}
	}()

	// Buffered and never read: the launch's outcome is not what this test is
	// about, but a launch has to be IN FLIGHT for there to be anything to expire
	// around.
	inFlight := make(chan error, 1)

	go func() {
		inFlight <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	select {
	case <-taken:
	case <-time.After(60 * time.Second):
		t.Fatal("the node never took the command")
	}

	// Long past the silence window, but it is working.
	clock.advancePastSilence()

	if got := p.Nodes(); len(got) != 1 {
		t.Fatalf("a node executing a command was forgotten: %v", got)
	}
}

// A COMMAND THAT NEVER LEFT THE QUEUE IS RELEASED when its node is forgotten.
//
// The mirror of the case above, and the one where expiry genuinely helps: the
// node never took this command, so nothing started and the caller is entitled to
// that certainty rather than to a wait it cannot end.
func TestAForgottenNodeReleasesItsQueuedWork(t *testing.T) {
	t.Parallel()

	clock := newTestClock()

	p := testPlane(t, WithClock(clock.now), WithCommandTimeout(time.Hour))
	register(t, p, "n1", config.ProviderDocker)

	launched := make(chan error, 1)

	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	// Nobody polls, so the command sits queued. Wait until it is there, or the
	// expiry below would race the dispatch that creates it.
	deadline := time.Now().Add(60 * time.Second)
	for p.QueuedForTest("n1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the command was never queued")
		}

		time.Sleep(5 * time.Millisecond)
	}

	clock.advancePastSilence()

	// Something has to consult the clock for expiry to happen.
	_ = p.Nodes()

	select {
	case err := <-launched:
		if err == nil {
			t.Fatal("a launch nobody took reported success")
		}

		if errors.Is(err, server.ErrCustody) {
			t.Errorf("a command that never left the queue reported custody: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the caller was left waiting on a node the plane has forgotten")
	}
}

// A RESULT THAT LANDS AS THE TIMER FIRES IS STILL A RESULT.
//
// Both branches of dispatch's select are live at once. Result takes the mutex first,
// deletes the inflight entry, and replies 204 to a node now certain its report
// landed; a timeout branch that declared custody anyway left nobody holding the
// lease while the container ran.
//
// Written against settle directly rather than raced, because a test that runs the
// race proves whichever ordering it happened to get.
func TestAResultThatLandedBeforeTheTimeoutIsTaken(t *testing.T) {
	t.Parallel()

	p := testPlane(t)
	register(t, p, "n1", config.ProviderDocker)

	p.mu.Lock()
	n := p.nodes["n1"]
	p.mu.Unlock()

	pend := &pending{
		cmd:       nodeapi.Command{ID: "c1", Kind: nodeapi.CommandLaunch, RequestID: 7},
		done:      make(chan nodeapi.CommandResult, 1),
		delivered: true,
	}

	// Exactly what Result leaves behind when it wins the mutex.
	pend.done <- nodeapi.CommandResult{ID: "c1", OK: true}

	res, err := p.settle(n, pend, errors.New("the command timed out"))
	if err != nil {
		t.Fatalf("a launch whose result had already arrived was reported as %v; the node was "+
			"told 204 and will not take custody, so nothing holds that lease", err)
	}

	if !res.OK {
		t.Errorf("the delivered result was not returned: %+v", res)
	}
}

// The mirror: with nothing delivered, a timed-out launch still means custody.
//
// Without this the test above would pass against a settle that returned success
// unconditionally, which is the opposite bug and a worse one.
func TestATimedOutLaunchWithNoResultStillMeansCustody(t *testing.T) {
	t.Parallel()

	p := testPlane(t)
	register(t, p, "n1", config.ProviderDocker)

	p.mu.Lock()
	n := p.nodes["n1"]
	p.mu.Unlock()

	pend := &pending{
		cmd:       nodeapi.Command{ID: "c1", Kind: nodeapi.CommandLaunch, RequestID: 7},
		done:      make(chan nodeapi.CommandResult, 1),
		delivered: true,
	}

	n.inflight[pend.cmd.ID] = pend

	_, err := p.settle(n, pend, errors.New("the command timed out"))
	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("a delivered launch that never answered must report custody, got %v", err)
	}

	// And it left the tombstone that tells a late report the lease is now the
	// node's.
	if _, ok := n.abandoned[pend.cmd.ID]; !ok {
		t.Error("no tombstone was recorded, so a late success is answered with a shrug and " +
			"the container runs under a lease nothing renews")
	}
}

// A QUEUED COMMAND IS NOT HANDED TO A PROCESS THAT HAS LOST THE NAME.
//
// The HTTP guard checks the incarnation before this function takes the mutex, so a
// supersession landing in between reaches a fast path that never looked again — and
// since the JIT entitlement follows the command, that process would hold a genuine
// right to mint the runner's registration.
//
// Driven against Plane.Poll directly: the guard would refuse the request long before
// the window opens.
func TestAQueuedCommandIsNotGivenToASupersededProcess(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(time.Second))

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Incarnation: "first",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Something is waiting to be delivered.
	go func() {
		//nolint:errcheck // the launch's fate is not what this test is about
		_ = p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	deadline := time.Now().Add(60 * time.Second)
	for p.QueuedForTest("n1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the command was never queued")
		}

		time.Sleep(time.Millisecond)
	}

	// A second process takes the name while that command sits in the queue.
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Incarnation: "second",
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	cmd, took, err := p.Poll(t.Context(), "n1", "first")
	if took {
		t.Errorf("a superseded process was handed queued command %s; it is now that lease's "+
			"recorded owner and may mint the runner for it", cmd.ID)
	}

	if !errors.Is(err, ErrSuperseded) {
		t.Errorf("want ErrSuperseded, got %v", err)
	}
}

// OWNERSHIP ENDS WITH THE COMPUTE, NOT WITH THE COMMAND, and getting that
// backwards was a way to lose a container.
//
// A launch that SUCCEEDS leaves something running. Dropping the owner then let a
// second host register, take that lease through AdoptOwnership, and refuse the
// process actually running it when it later took custody — while a completion
// routed to the new owner found nothing, reported success, and released the
// lease under a live job.
//
// A launch that FAILED without claiming custody started nothing, so there is
// nothing left to own.
func TestOwnershipEndsWithTheCompute(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		ok      bool
		custody bool
		want    bool
	}{
		{"a successful launch keeps its owner while its container runs", true, false, true},
		{"a result claiming custody keeps its owner", false, true, true},
		{"a clean failure started nothing to own", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p, cmd := deliverLaunch(t)

			if err := p.Result("n1", "first", nodeapi.CommandResult{
				ID: cmd.ID, OK: tc.ok, Custody: tc.custody,
			}); err != nil {
				t.Fatalf("result: %v", err)
			}

			if got := p.OwnsForTest("l1", "n1", "first"); got != tc.want {
				t.Errorf("ownership retained = %v, want %v", got, tc.want)
			}
		})
	}
}

// AND THE DESTROY IS WHERE AN ORDINARY JOB'S OWNERSHIP ENDS.
//
// That is the only moment the wire hears about it: the lease of a job that ran
// cleanly is released in-process by the listener, through the allocator, without
// ever calling the node. Without this, every completed job left an entry for the
// life of the installation, because a node that never goes quiet is never
// expired.
func TestADestroyEndsALeasesOwnership(t *testing.T) {
	t.Parallel()

	p, cmd := deliverLaunch(t)

	if err := p.Result("n1", "first", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("result: %v", err)
	}

	if !p.OwnsForTest("l1", "n1", "first") {
		t.Fatal("a successful launch did not keep its owner")
	}

	// The node answers the destroy, as it would when the job completes.
	go func() {
		got, took, err := p.Poll(t.Context(), "n1", "first")
		if err != nil || !took {
			return
		}

		//nolint:errcheck // the result's fate is not what this test is about
		_ = p.Result("n1", "first", nodeapi.CommandResult{ID: got.ID, OK: true})
	}()

	// Parked first, for the reason recorded above the other two: a command whose
	// poller has not run yet is discarded on the command timeout.
	waitFor(t, "the node to park on a poll",
		func() bool { return p.WaitersForTest("n1") == 1 })

	if err := p.NewRunner().Destroy(t.Context(), 7); err != nil {
		t.Fatalf("destroy: %v", err)
	}

	if p.OwnsForTest("l1", "n1", "first") {
		t.Error("the ownership record outlived the compute it described; one per historical " +
			"job accumulates for the life of the installation")
	}
}

// deliverLaunch registers a node and hands it a launch, which is the state every
// ownership question starts from.
//
// The command timeout is a PARAMETER because its callers want opposite things.
// Most are asking about ownership and need the timeout never to fire, so they
// take the generous default — a shorter one there is a bet on how quickly a
// goroutine gets scheduled, which is what made this suite flaky. One caller is
// asking what an UNANSWERED destroy does, and needs it to fire promptly; sixty
// seconds there is a minute of waiting for a result the test already knows.
func deliverLaunch(t *testing.T, opts ...Option) (*Plane, nodeapi.Command) {
	t.Helper()

	p := testPlane(t, append([]Option{WithCommandTimeout(60 * time.Second)}, opts...)...)

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Incarnation: "first",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	go func() {
		//nolint:errcheck // the launch's fate is not what these tests are about
		_ = p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	deadline := time.Now().Add(60 * time.Second)

	for {
		got, took, err := p.Poll(t.Context(), "n1", "first")
		if err != nil {
			t.Fatalf("poll: %v", err)
		}

		if took {
			if !p.OwnsForTest("l1", "n1", "first") {
				t.Fatal("delivering a launch did not record its owner")
			}

			return p, got
		}

		if time.Now().After(deadline) {
			t.Fatal("the command was never delivered")
		}
	}
}

// A REGISTRATION DROPS OWNERSHIP THE LEDGER NO LONGER HAS OPEN.
//
// The ledger snapshot is read before the registration commits, so a lease can
// end in between and be adopted as an entry nothing can ever name: it carries no
// request id, so no destroy matches it, and the plane's map outlives node
// expiry. Repeated registration races would accumulate those forever.
//
// A lease in CUSTODY is still open in the ledger, so a draining process keeps
// what it holds; only genuinely terminal leases go.
func TestRegistrationPrunesOwnershipTheLedgerHasEnded(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Incarnation: "first",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	// Two leases the ledger reported as open at that moment.
	p.AdoptOwnership("n1", "first", []string{"l1", "l2"})

	if !p.OwnsForTest("l1", "n1", "first") || !p.OwnsForTest("l2", "n1", "first") {
		t.Fatal("adoption did not record both leases")
	}

	// The node registers again, and by now l2 has ended.
	p.AdoptOwnership("n1", "first", []string{"l1"})

	if !p.OwnsForTest("l1", "n1", "first") {
		t.Error("a lease the ledger still has open lost its owner; the process holding it " +
			"can no longer renew or release it")
	}

	if p.OwnsForTest("l2", "n1", "first") {
		t.Error("a lease the ledger no longer has open kept its owner; nothing can ever name " +
			"that entry again, so it accumulates for the life of the installation")
	}
}

// SUPERSESSION DURING A DESTROY IS STILL NOT A CONFIRMATION.
//
// The owner's currency is read before the command is dispatched, and it can
// change while the command is in flight: a replacement registers, TAKES the
// destroy, and truthfully reports it has nothing to remove. A decision made on
// the earlier reading treats that as the owner confirming, and the lease is
// released under a live container.
//
// THE ORDERING IS ESTABLISHED, NOT HOPED FOR, and the first version of this test
// did hope. It started the replacement in a goroutine alongside Destroy, so the
// registration could land FIRST — and in that ordering even the buggy code reads
// the owner as already superseded and returns custody, so every assertion passed
// without the race ever happening. Waiting for the command to be queued proves
// the owner snapshot has already been taken.
func TestASupersessionDuringADestroyIsNotAConfirmation(t *testing.T) {
	t.Parallel()

	p, _ := deliverLaunch(t)

	destroyed := make(chan error, 1)

	go func() {
		destroyed <- p.NewRunner().Destroy(t.Context(), 7)
	}()

	// The command is queued, so OwnerOfRequest has already been read and the
	// snapshot says "first" is current. Only now can the supersession race it.
	deadline := time.Now().Add(60 * time.Second)
	for p.QueuedForTest("n1") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the destroy was never queued")
		}

		time.Sleep(time.Millisecond)
	}

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Incarnation: "second",
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	// The replacement takes the destroy and answers it truthfully: it has nothing
	// to remove, because it does not have it.
	go func() {
		got, took, err := p.Poll(t.Context(), "n1", "second")
		if err != nil || !took {
			return
		}

		//nolint:errcheck // the result's fate is not what this test is about
		_ = p.Result("n1", "second", nodeapi.CommandResult{ID: got.ID, OK: true})
	}()

	select {
	case err := <-destroyed:
		if !errors.Is(err, server.ErrCustody) {
			t.Errorf("a destroy answered by the process that replaced the owner reported %v; "+
				"the listener releases the lease while the container is still running", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the destroy never returned")
	}

	if !p.OwnsForTest("l1", "n1", "first") {
		t.Error("the owner's record was dropped on somebody else's confirmation")
	}
}

// A DESTROY THE OWNER COMPLETED BUT COULD NOT REPORT STILL ENDS ITS OWNERSHIP.
//
// The process that takes a destroy can succeed and be superseded before it
// answers. Registration then fails its in-flight command, and its late result
// used to be discarded — so the ownership record survived, that process drained
// to nothing and exited, and every later destroy was answered by its
// replacement, which cannot confirm somebody else's ownership. The plane
// reported custody forever for a container that had already been removed, and
// the listener went on heartbeating its capacity.
func TestALateDestroyResultFromTheOwnerEndsItsOwnership(t *testing.T) {
	t.Parallel()

	p, _ := deliverLaunch(t)

	destroyed := make(chan error, 1)

	go func() {
		destroyed <- p.NewRunner().Destroy(t.Context(), 7)
	}()

	// The owner takes the destroy — and then says nothing.
	var taken nodeapi.Command

	deadline := time.Now().Add(60 * time.Second)

	for {
		got, took, err := p.Poll(t.Context(), "n1", "first")
		if err != nil {
			t.Fatalf("poll: %v", err)
		}

		if took {
			if got.Kind != nodeapi.CommandDestroy {
				t.Fatalf("polled a %s command, want the destroy", got.Kind)
			}

			taken = got

			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the destroy was never delivered")
		}
	}

	// A replacement arrives, which fails the in-flight destroy.
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Incarnation: "second",
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	if err := <-destroyed; err == nil {
		t.Fatal("a destroy failed by a re-registration reported success")
	}

	// The owner had already removed the container, and now reports it.
	if err := p.Result("n1", "first", nodeapi.CommandResult{ID: taken.ID, OK: true}); err != nil {
		t.Fatalf("late result: %v", err)
	}

	if p.OwnsForTest("l1", "n1", "first") {
		t.Error("a late destroy result proving the compute gone left the ownership behind; " +
			"once that process drains and exits, every later destroy reports custody " +
			"forever and the capacity is never reclaimed")
	}
}

// THE OWNER'S CONFIRMATION STANDS ON ITS OWN, even when another node fails.
//
// Holding the record back until every leg succeeded stranded it: the owner had
// already destroyed its container and would later drain to nothing, after which
// every future destroy saw a superseded owner and reported custody forever for
// compute that no longer existed.
func TestAnOwnersConfirmationClearsTheRecordAndNobodyElseIsAsked(t *testing.T) {
	t.Parallel()

	p, _ := deliverLaunch(t)

	// A second node in the fleet that never answers anything. It used to be asked
	// too — the destroy was broadcast — and its silence failed the whole call for
	// a container it had never heard of.
	register(t, p, "n2", config.ProviderDocker)

	// The owner answers its own leg.
	go func() {
		got, took, err := p.Poll(t.Context(), "n1", "first")
		if err != nil || !took {
			return
		}

		//nolint:errcheck // the result's fate is not what this test is about
		_ = p.Result("n1", "first", nodeapi.CommandResult{ID: got.ID, OK: true})
	}()

	// PARKED FIRST. A dispatched command is discarded once the command timeout
	// elapses, so starting the poller and the destroy together races the
	// scheduler — under the full suite's parallelism this goroutine may not run
	// inside that window, and the test fails on a timeout rather than on what it
	// is about.
	waitFor(t, "the owner to park on a poll",
		func() bool { return p.WaitersForTest("n1") == 1 })

	// ADDRESSED, so a silent bystander is irrelevant. This assertion is the
	// inverse of what it used to be, and deliberately: the machine that has the
	// container is the only one asked, so the one that does not have it can no
	// longer fail a destroy it was never part of.
	if err := p.NewRunner().Destroy(t.Context(), 7); err != nil {
		t.Fatalf("a destroy addressed to the owner was failed by a bystander: %v", err)
	}

	if p.OwnsForTest("l1", "n1", "first") {
		t.Error("the owner confirmed its own destroy and kept the record anyway; once it " +
			"drains to nothing, every later destroy reports custody forever for compute " +
			"that no longer exists")
	}
}

// A DELIVERED LAUNCH IS NOT PRUNED BY A SNAPSHOT THAT PREDATES IT.
//
// LaunchedLeaseIDs reports only launching, online and busy — so a lease that has
// been delivered and is still `assigned` is legitimately absent, and treating
// absence as terminality deletes the owner of a container that is about to
// exist. Only records adopted FROM a snapshot, which carry no request id, are
// candidates.
func TestPruningDoesNotTouchADeliveredLaunch(t *testing.T) {
	t.Parallel()

	p, _ := deliverLaunch(t)

	// A registration whose snapshot does not mention l1 at all.
	p.AdoptOwnership("n1", "first", []string{"somebody-else"})

	if !p.OwnsForTest("l1", "n1", "first") {
		t.Error("a launch this plane delivered lost its owner to a snapshot taken before it; " +
			"a later destroy finds no owner and the capacity is released under the container")
	}
}

// AND AN EMPTY SNAPSHOT STILL PRUNES, which is the shape that used to strand an
// entry for the life of the process.
func TestAnEmptySnapshotStillPrunesAdoptedOwnership(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Incarnation: "first",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	p.AdoptOwnership("n1", "first", []string{"l1"})

	if !p.OwnsForTest("l1", "n1", "first") {
		t.Fatal("adoption did not record the lease")
	}

	// The node registers again and the ledger now reports nothing open.
	p.AdoptOwnership("n1", "first", nil)

	if p.OwnsForTest("l1", "n1", "first") {
		t.Error("an adopted record survived a snapshot that reported no open leases; nothing " +
			"can ever name it again, so it stays for the life of the control plane")
	}
}

// A LATE DESTROY FROM SOMEBODY ELSE PROVES NOTHING ABOUT THE OWNER.
//
// A destroy answered by a process that is not the owner is telling the truth
// about ITSELF — it has nothing to remove — and saying nothing about a container
// on a machine it cannot see. Ending the owner's record on it lets the next
// completion accept a no-op destroy and release capacity under a live job.
//
// Reachable without any restart: A owns a container, B supersedes A and takes a
// destroy, C supersedes B before B reports, and B's late success arrives.
func TestALateDestroyFromANonOwnerDoesNotEndOwnership(t *testing.T) {
	t.Parallel()

	p, _ := deliverLaunch(t)

	// B takes the name and the destroy.
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "n1", Provider: config.ProviderDocker,
		Deployment: deployment,
		VCPU:       8,
		Memory:     32 * config.GiB, Incarnation: "second",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	destroyed := make(chan error, 1)

	go func() {
		destroyed <- p.NewRunner().Destroy(t.Context(), 7)
	}()

	var taken nodeapi.Command

	deadline := time.Now().Add(60 * time.Second)

	for {
		got, took, err := p.Poll(t.Context(), "n1", "second")
		if err != nil {
			t.Fatalf("poll: %v", err)
		}

		if took {
			taken = got

			break
		}

		if time.Now().After(deadline) {
			t.Fatal("the destroy was never delivered")
		}
	}

	// C supersedes B, tombstoning B's in-flight destroy.
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "n1", Provider: config.ProviderDocker,
		Deployment: deployment,
		VCPU:       8,
		Memory:     32 * config.GiB, Incarnation: "third",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	<-destroyed

	// B reports the success it truthfully had: it removed nothing.
	if err := p.Result("n1", "second", nodeapi.CommandResult{ID: taken.ID, OK: true}); err != nil {
		t.Fatalf("late result: %v", err)
	}

	if !p.OwnsForTest("l1", "n1", "first") {
		t.Error("a destroy answered by a process that never held the container ended the " +
			"owner's record; the next completion accepts a no-op destroy and releases " +
			"capacity while that container is still running")
	}
}

// SWEEPS AND TENDS DO NOT COMPETE FOR THE TOMBSTONE BUDGET.
//
// A late sweep or tend result tells the plane nothing it acts on, and letting
// them share a bounded budget with launches let a burst of them EVICT a
// safety-critical entry: the launch that then reported success was answered with
// an ordinary 204, so the node never adopted the lease the listener had already
// stopped renewing, and it expired under a running container.
func TestOnlyLaunchesAndDestroysAreTombstoned(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "n1", Provider: config.ProviderDocker,
		Deployment: deployment,
		VCPU:       8,
		Memory:     32 * config.GiB, Incarnation: "first",
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	p.mu.Lock()
	n := p.nodes["n1"]

	for _, kind := range []nodeapi.CommandKind{
		nodeapi.CommandSweep, nodeapi.CommandTend,
		nodeapi.CommandLaunch, nodeapi.CommandDestroy,
	} {
		n.rememberAbandoned(nodeapi.Command{ID: string(kind), Kind: kind}, "first", time.Now())
	}

	got := len(n.abandoned)
	p.mu.Unlock()

	if got != 2 {
		t.Errorf("%d commands were tombstoned, want only the launch and the destroy; the "+
			"others share a bounded budget and can evict one that matters", got)
	}
}

// A DESTROY IS ONLY CONFIRMED BY THE PROCESS THAT HAS THE CONTAINER.
//
// The wire broadcasts to whoever is polling, which is the CURRENT incarnation. A
// superseded process draining its custody never polls, so it is never asked —
// and its replacement answers honestly that it has nothing to remove, because it
// does not have it. Believing that answer releases the lease under a live job.
//
// Custody is the right answer: somebody is holding this. The draining process
// renews the lease and destroys the compute once its own tend confirms the job
// finished.
func TestADestroyIsNotConfirmedByTheWrongProcess(t *testing.T) {
	t.Parallel()

	p, _ := deliverLaunch(t)

	// A second process takes the name. The first is now draining, and does not
	// poll.
	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:     nodeapi.Version,
		Node:        "n1",
		Provider:    config.ProviderDocker,
		Deployment:  deployment,
		VCPU:        8,
		Memory:      32 * config.GiB,
		Incarnation: "second",
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	// The replacement answers the destroy, truthfully: it has nothing to remove.
	go func() {
		got, took, err := p.Poll(t.Context(), "n1", "second")
		if err != nil || !took {
			return
		}

		//nolint:errcheck // the result's fate is not what this test is about
		_ = p.Result("n1", "second", nodeapi.CommandResult{ID: got.ID, OK: true})
	}()

	// PARKED BEFORE THE DESTROY IS DISPATCHED, for the reason destroy_test.go
	// already records: a dispatched command is discarded once the command timeout
	// elapses, so starting the poller and the destroy together is a race against
	// the scheduler. Under the full suite's parallelism this goroutine can simply
	// not run inside that window, and the test then fails on a timeout that has
	// nothing to do with who answered.
	waitFor(t, "the replacement to park on a poll",
		func() bool { return p.WaitersForTest("n1") == 1 })

	err := p.NewRunner().Destroy(t.Context(), 7)
	if !errors.Is(err, server.ErrCustody) {
		t.Errorf("a destroy answered by a process that does not hold the container reported "+
			"%v; the listener releases the lease while the job is still running", err)
	}

	// And the record survives, because the draining process still needs to renew
	// and release what it is holding.
	if !p.OwnsForTest("l1", "n1", "first") {
		t.Error("the ownership record was dropped by a destroy its owner never answered; " +
			"that process can no longer renew or release, and its drain cannot end")
	}
}

// A FAILED DESTROY KEEPS THE OWNERSHIP RECORD TOO.
//
// An unconditional forget dropped it on failure as well, which left a process
// that was later superseded unable to renew or release what it was holding.
func TestAFailedDestroyKeepsItsOwnershipRecord(t *testing.T) {
	t.Parallel()

	// SHORT, because the timeout IS the stimulus here rather than a watchdog:
	// this test wants the destroy to go unanswered and expire.
	p, _ := deliverLaunch(t, WithCommandTimeout(500*time.Millisecond))

	// Nobody answers the destroy, so it times out.
	if err := p.NewRunner().Destroy(t.Context(), 7); err == nil {
		t.Fatal("a destroy nobody answered reported success")
	}

	if !p.OwnsForTest("l1", "n1", "first") {
		t.Error("a failed destroy dropped the ownership record; the process holding that " +
			"compute can no longer renew or release it")
	}
}

// A DESTROY NOBODY CONFIRMED IS A FAILURE, EVEN WHEN THE NODE HAS GONE.
//
// The tempting reading is that a vanished node took its containers with it, so
// its unanswered destroy may as well be forgiven — otherwise the listener holds
// that lease for good, with nothing to retry it and no host coming back. This
// code was written that way for exactly one review round.
//
// It is false wherever the compute runtime outlives the billet process, which is
// every provider there is. A node that takes a destroy and then crashes leaves a
// Docker daemon holding a container that is still running and still occupying
// the capacity this lease represents. Releasing the lease sells that capacity
// twice, which is the one thing the allocator exists to prevent.
//
// So the failure stands and the lease is held. That leaks a slot when a host
// never returns, and the leak is the better half of the trade: it costs
// capacity, where the alternative costs correctness.
func TestADestroyNoNodeConfirmedIsAFailure(t *testing.T) {
	t.Parallel()

	clock := newTestClock()

	p := testPlane(t, WithClock(clock.now), WithCommandTimeout(150*time.Millisecond))
	register(t, p, "n1", config.ProviderDocker)

	// The node takes the destroy and then dies holding it.
	taken := make(chan struct{})

	go func() {
		if _, _, err := p.Poll(t.Context(), "n1", ""); err == nil {
			close(taken)
		}
	}()

	destroyed := make(chan error, 1)

	go func() {
		destroyed <- p.NewRunner().Destroy(t.Context(), 7)
	}()

	select {
	case <-taken:
	case <-time.After(60 * time.Second):
		t.Fatal("the node never took the destroy")
	}

	// Silent well past the window, so the plane forgets it once the command stops
	// waiting — the case that used to be forgiven.
	clock.advancePastSilence()

	select {
	case err := <-destroyed:
		if err == nil {
			t.Error("a destroy no node confirmed reported success, so the listener releases " +
				"capacity that a container may still be using")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destroy never returned")
	}
}

// Destroy with no nodes is not a failure: there is nowhere for the compute to
// be, so there is nothing to remove.
func TestDestroyWithNoNodesIsQuiet(t *testing.T) {
	t.Parallel()

	if err := testPlane(t).NewRunner().Destroy(t.Context(), 7); err != nil {
		t.Fatalf("Destroy on an empty fleet: %v", err)
	}
}

// A caller that gives up does not wedge the plane.
func TestACancelledCallerIsNotStuck(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(time.Hour))
	register(t, p, "n1", config.ProviderDocker)

	ctx, cancel := context.WithCancel(t.Context())

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := p.NewRunner().Launch(ctx, testLease(), server.Job{RequestID: 7})
	if err == nil {
		t.Fatal("a cancelled launch reported success")
	}
}

// A NODE MAY NOT TOUCH ANOTHER NODE'S LEASE, and being registered is not the
// thing that decides it.
//
// MayMutateLease checks ownership first, which is right, and then falls through
// to fleet membership when the owner does not match — which admitted ANY current
// node for ANY lease id. Registration proves which host you are; it says nothing
// about what work you were given. EntitledToLaunch already draws that line for
// JIT credentials, and this is the same line for the lease's fate.
//
// The reachable damage is capacity, not just tidiness. Lease ids are not secret:
// provider.InstanceName puts them in the runner name, which is visible in the
// organisation's runner list. A host that reads one and releases it terminalises
// a lease whose container is still running on somebody else's machine, and the
// freed vCPU is escrowed to another tier — the double-admission the ledger
// exists to prevent, caused from outside the ledger.
func TestANodeCannotReleaseAnotherNodesLease(t *testing.T) {
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute)

	register(t, p, "a", config.ProviderDocker)
	register(t, p, "b", config.ProviderDocker)

	// The launch went to a, so a owns it.
	p.AdoptOwnership("a", "inc-a", []string{"l1"})

	if err := p.MayMutateLease("a", "inc-a", "l1"); err != nil {
		t.Fatalf("the node the lease was given to may not maintain it: %v", err)
	}

	err := p.MayMutateLease("b", "inc-b", "l1")
	if err == nil {
		t.Fatal("a registered node was allowed to change the fate of a lease belonging to " +
			"another node; it can release capacity out from under a running container")
	}
}

// THE EPOCH BELONGS TO THE PROCESS THAT SENT THE INVENTORY.
//
// The route wrapper refuses a superseded incarnation, and then the epoch was
// captured under a SECOND lock acquisition — a check-then-act rather than a
// fence. A replacement registering in that gap meant the stale report was
// accepted under the REPLACEMENT's epoch, freeing capacity for a container the
// newer incarnation had just vouched for.
//
// Tested here rather than through the route, because the route's own check
// refuses the caller before this one is reached: the window this closes exists
// only between the two, and only a direct call can stand in it.
func TestReconcileRefusesAnInventoryFromASupersededProcess(t *testing.T) {
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithRegistrar(&countingRegistrar{}))

	// REGISTERED WITH AN INCARNATION, because an empty one means the plane does
	// not know which process it is talking to and accepts anyone — the same rule
	// CheckIncarnation follows, and a test that left it empty would assert
	// nothing.
	const current = "second"

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "n1", Provider: config.ProviderDocker,
		Deployment: deployment, VCPU: 8, Memory: 32 * config.GiB,
		Incarnation: current,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	if _, err := p.ReconcileInventory(t.Context(), "n1", "first", nil); err == nil {
		t.Fatal("an inventory from a process the plane has already replaced was accepted; " +
			"it can free capacity the current one is using")
	}

	// And the current process is still able to report.
	if _, err := p.ReconcileInventory(t.Context(), "n1", current, nil); err != nil {
		t.Errorf("the current process could not report its inventory: %v", err)
	}
}

// countingRegistrar is a Registrar that records nothing and answers everything,
// so a test can reach the plane's own decisions.
type countingRegistrar struct{}

func (countingRegistrar) RegisterNode(context.Context, alloc.NodeRegistration) (int64, error) {
	return 1, nil
}

func (countingRegistrar) NodeGone(context.Context, string, int64) error { return nil }

func (countingRegistrar) ForgetEveryNode(context.Context) error { return nil }

func (countingRegistrar) ResolveQuarantineFor(
	context.Context, string, []string, int64,
) (int, error) {
	return 0, nil
}
