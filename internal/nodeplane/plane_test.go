package nodeplane

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

const deployment = "0123456789abcdef0123456789abcdef"

func testPlane(t *testing.T, opts ...Option) *Plane {
	t.Helper()

	return New(slog.New(slog.DiscardHandler), deployment, time.Minute, opts...)
}

func register(t *testing.T, p *Plane, name string, provider config.ProviderKind) {
	t.Helper()

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:    nodeapi.Version,
		Node:       name,
		Provider:   provider,
		Deployment: deployment,
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
		if _, _, err := p.Poll(t.Context(), "n1"); err != nil {
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
				cmd, ok, err := p.Poll(t.Context(), "n1")
				if err != nil || !ok {
					return
				}

				res := tc.res
				res.ID = cmd.ID

				if err := p.Result("n1", res); err != nil {
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

	p := testPlane(t, WithCommandTimeout(5*time.Second))
	register(t, p, "n1", config.ProviderDocker)

	taken := make(chan struct{})

	go func() {
		if _, _, err := p.Poll(t.Context(), "n1"); err == nil {
			close(taken)
		}
	}()

	launched := make(chan error, 1)

	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	select {
	case <-taken:
	case <-time.After(5 * time.Second):
		t.Fatal("the node never took the command")
	}

	register(t, p, "n1", config.ProviderDocker)

	select {
	case err := <-launched:
		if !errors.Is(err, server.ErrCustody) {
			t.Fatalf("a launch lost to a node restart must report custody, got %v", err)
		}
	case <-time.After(5 * time.Second):
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

	_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version:    nodeapi.Version + 1,
		Node:       "n1",
		Deployment: deployment,
	})
	if err == nil {
		t.Fatal("a node speaking a different protocol version was accepted")
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
			cmd, ok, err := p.Poll(t.Context(), name)
			if err != nil || !ok {
				return
			}

			got <- name

			if err := p.Result(name, nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
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
		cmd, ok, err := p.Poll(t.Context(), "other")
		if err != nil || !ok {
			return
		}

		if err := p.Result("other", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
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

	if _, _, err := p.Poll(t.Context(), "ghost"); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("want ErrUnregistered, got %v", err)
	}

	if err := p.Result("ghost", nodeapi.CommandResult{ID: "x"}); !errors.Is(err, ErrUnregistered) {
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

	cmd, ok, err := p.Poll(t.Context(), "n1")
	if err != nil {
		t.Fatalf("an idle poll reported an error: %v", err)
	}

	if ok {
		t.Errorf("an idle poll produced a command: %+v", cmd)
	}
}

// ONE WEDGED NODE MUST NOT HOLD UP A DESTROY ON THE OTHERS.
//
// Destroy broadcasts, and the first version asked each node in turn — so a
// single host that never answers would block the whole call for the command
// timeout before the next node was even asked. Destroy runs on shutdown and on
// completion paths, where minutes of that turns one bad host into a stalled
// control plane.
//
// The timing assertion is deliberately loose: what is being proved is
// "concurrent, not serial", and a threshold near the timeout distinguishes those
// two without being sensitive to how fast the machine is.
func TestOneWedgedNodeDoesNotStallADestroy(t *testing.T) {
	t.Parallel()

	const timeout = 600 * time.Millisecond

	p := testPlane(t, WithCommandTimeout(timeout))

	for _, name := range []string{"wedged-a", "wedged-b", "wedged-c"} {
		register(t, p, name, config.ProviderDocker)

		// Each takes its command and never answers.
		go func() {
			if _, _, err := p.Poll(t.Context(), name); err != nil {
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
			cmd, ok, err := p.Poll(t.Context(), name)
			if err != nil || !ok {
				return
			}

			if err := p.Result(name, nodeapi.CommandResult{
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
