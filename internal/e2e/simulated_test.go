package e2e

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/simulated"
)

// simulatedTier is the one tier a simulated stack serves. Its command is what
// tells the backend how long a runner occupies its instance.
const (
	simulatedTier     = "billet-2vcpu-simulated"
	simulatedDuration = 10 * time.Minute
)

// gatedProvider is the simulated backend with a Destroy the test can hold open.
//
// IT EXISTS TO MAKE AN ORDERING OBSERVABLE. The invariant under test is that the
// lease is released only after the compute is confirmed gone, and a scenario
// that reads the end state alone cannot see it: a regression that released first
// and destroyed a moment later leaves exactly the same final picture. Holding
// the destroy open is the only way to ask the ledger what it says in between.
type gatedProvider struct {
	*simulated.Provider

	mu sync.Mutex
	// hold, when set, is what a Destroy waits on before delegating; entered is
	// signalled once when a Destroy reaches the gate.
	hold    chan struct{}
	entered chan struct{}
}

// holdDestroys arms the gate. The first Destroy to arrive signals entered and
// waits until release is closed.
func (g *gatedProvider) holdDestroys() (entered <-chan struct{}, release func()) {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.hold = make(chan struct{})
	g.entered = make(chan struct{}, 1)

	return g.entered, sync.OnceFunc(func() { close(g.hold) })
}

func (g *gatedProvider) Destroy(ctx context.Context, id string) (provider.Teardown, error) {
	g.mu.Lock()
	hold, entered := g.hold, g.entered
	g.mu.Unlock()

	if hold != nil {
		select {
		case entered <- struct{}{}:
		default:
		}

		select {
		case <-hold:
		case <-ctx.Done():
			return provider.TeardownRequested, ctx.Err()
		}
	}

	return g.Provider.Destroy(ctx, id)
}

// simulatedStack is the real control plane, ledger and node runtime over the
// backend that starts no compute, on a clock the test moves.
//
// ONLY THE PROVIDER'S CLOCK IS STEERED. The ledger stays on wall time: jumping it
// past the lease TTL while heartbeats arrive on the real clock would quarantine
// the lease and the test would be measuring the reaper. How a replay moves both
// together is the harness's design (#74), not this scenario's.
func simulatedStack(t *testing.T, opts ...stackOpt) (*stack, *offsetClock, *gatedProvider) {
	t.Helper()

	clock := &offsetClock{}
	gate := &gatedProvider{}

	build := func(t *testing.T, deployment string) provider.Provider {
		t.Helper()

		p, err := simulated.New(deployment, simulated.WithClock(clock.now),
			simulated.WithLogger(testLogger(t)))
		if err != nil {
			t.Fatalf("simulated.New: %v", err)
		}

		gate.Provider = p

		return gate
	}

	tiers := []config.Tier{{
		Label:       simulatedTier,
		Provider:    config.ProviderSimulated,
		VCPU:        2,
		Memory:      2 * config.GiB,
		Image:       "simulated",
		RunnerGroup: testGroup,
		GuestOS:     config.GuestLinux,
		Trust:       config.WorkloadTrusted,
		Workflows:   []string{"acme/test/.github/workflows/e2e.yml@refs/heads/main"},
		Command:     simulated.RunFor(simulatedDuration),
	}}

	return newStack(t, append([]stackOpt{
		withBackend(config.ProviderSimulated, build), withTiers(tiers),
	}, opts...)...), clock, gate
}

// A LEASE PLACED ON THE SIMULATED BACKEND IS CHARGED, HELD FOR THE MODELLED
// DURATION, AND RELEASED THROUGH THE ORDINARY SETTLEMENT PATH.
//
// Against the real allocator, listener and node runtime, in-process and over the
// wire, because the backend exists to stand under those in a replay and a
// backend the real machinery cannot drive proves nothing about it. The scale set
// is provisioned, the job is bid for by the id GitHub offered, the instance is
// named after its lease, the lease stays charged while the modelled runner
// occupies the instance AND after it stops (a stopped instance is not a release;
// GitHub's completion is), and the completion destroys the instance and settles
// the lease exactly as a container's would.
func TestASimulatedJobIsChargedHeldAndSettledThroughTheOrdinaryPath(t *testing.T) {
	for name, opts := range map[string][]stackOpt{
		"in-process":    nil,
		"over the wire": {overTheWire},
	} {
		t.Run(name, func(t *testing.T) {
			s, clock, gate := simulatedStack(t, opts...)

			s.plane.queue(fakeactions.StatisticsJSON(1, 0),
				fakeactions.JobJSON("JobAvailable", 5001, "push", simulatedTier))

			stop := s.run(t)
			defer stop()

			deadline := time.Now().Add(30 * time.Second)

			for len(s.plane.acquiredIDs()) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("billet never bid for the available job")
				}

				time.Sleep(50 * time.Millisecond)
			}

			s.plane.queue(fakeactions.StatisticsJSON(0, 1),
				fakeactions.JobJSON("JobAssigned", 5001, "push", simulatedTier))

			names := s.awaitOneRunning(t)

			leaseID, ours := provider.LeaseOf(names[0])
			if !ours || leaseID == "" {
				t.Fatalf("instance %q does not carry a lease", names[0])
			}

			// CHARGED, TO THIS HOST, ON THIS BACKEND, FOR THE TIER REQUEST. Asserted
			// against the lease rather than against total usage, which never falls
			// to zero while a listener holds escrow to advertise.
			lease, err := s.alloc.Lease(t.Context(), leaseID)
			if err != nil {
				t.Fatalf("read the job's lease: %v", err)
			}

			if lease.Node != s.node || lease.Provider != config.ProviderSimulated {
				t.Fatalf("the lease is bound to %q on %q; want %q on %q",
					lease.Node, lease.Provider, s.node, config.ProviderSimulated)
			}

			if lease.VCPU != 2 || lease.Memory != 2*config.GiB {
				t.Fatalf("the lease is charged %d vCPU and %s; want the tier's 2 vCPU and 2GiB",
					lease.VCPU, lease.Memory)
			}

			// HELD. Several reaper ticks pass with the modelled runner still going,
			// and nothing settles the lease early.
			time.Sleep(time.Second)

			s.assertRunningAndCharged(t, names[0], leaseID)

			// THE MODELLED DURATION ELAPSES. The instance stops and is terminal, and
			// the lease is STILL charged: the backend's word that a runner finished
			// is not GitHub's word that the job did.
			clock.advance(simulatedDuration + time.Second)

			deadline = time.Now().Add(10 * time.Second)

			for {
				inst, found, err := s.provider.Find(t.Context(), names[0])
				if err != nil || !found {
					t.Fatalf("Find after the duration: found=%v err=%v", found, err)
				}

				if !inst.Running && inst.Terminal {
					break
				}

				if time.Now().After(deadline) {
					t.Fatalf("the instance still reports %+v after its modelled duration", inst)
				}

				time.Sleep(50 * time.Millisecond)
			}

			time.Sleep(time.Second)

			if _, err := s.alloc.Lease(t.Context(), leaseID); err != nil {
				t.Fatalf("the lease was settled on the backend's word alone, before GitHub "+
					"reported the job complete: %v", err)
			}

			// THE JOB COMPLETES, and the ordinary path runs: destroy, THEN release.
			// The destroy is held open so the ledger can be asked mid-teardown; a
			// lease released before the compute is confirmed gone is the overcommit
			// the ordering exists to prevent, and only visible from in here.
			entered, release := gate.holdDestroys()
			defer release()

			s.plane.queue(fakeactions.StatisticsJSON(0, 0),
				fakeactions.JobJSON("JobCompleted", 5001, "push", simulatedTier))

			select {
			case <-entered:
			case <-time.After(30 * time.Second):
				t.Fatal("the completion never reached the backend's Destroy")
			}

			if _, err := s.alloc.Lease(t.Context(), leaseID); err != nil {
				t.Fatalf("the lease was released while its teardown was still in flight: %v", err)
			}

			release()

			s.awaitGone(t)

			deadline = time.Now().Add(30 * time.Second)

			for {
				_, err := s.alloc.Lease(t.Context(), leaseID)
				if errors.Is(err, alloc.ErrLeaseNotFound) {
					break
				}

				if err != nil {
					t.Fatalf("read the job's lease: %v", err)
				}

				if time.Now().After(deadline) {
					t.Fatalf("lease %s still holds capacity after its instance was destroyed", leaseID)
				}

				time.Sleep(100 * time.Millisecond)
			}
		})
	}
}

// assertRunningAndCharged reads the instance from the backend and the lease from
// the ledger, and fails if either has moved on.
func (s *stack) assertRunningAndCharged(t *testing.T, name, leaseID string) {
	t.Helper()

	inst, found, err := s.provider.Find(t.Context(), name)
	if err != nil || !found {
		t.Fatalf("Find: found=%v err=%v", found, err)
	}

	if !inst.Running {
		t.Fatalf("the instance stopped before its modelled duration: %+v", inst)
	}

	lease, err := s.alloc.Lease(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("the lease was released while its instance was running: %v", err)
	}

	if lease.Phase == alloc.PhaseDone || lease.Phase == alloc.PhaseFailed {
		t.Fatalf("the lease reached %s while its instance was running", lease.Phase)
	}
}

// UNTRUSTED WORK NEVER REACHES THE SIMULATED BACKEND, AND NOTHING IS MINTED FOR IT.
//
// The refusal has to come before the registration: a registration with nothing
// to consume it is an orphan on GitHub, one per pull request. This backend has
// no boundary at all, so the refusal is the same one docker makes.
func TestUntrustedWorkNeverReachesTheSimulatedBackend(t *testing.T) {
	s, _, _ := simulatedStack(t, untrustedPool)

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 5002, "pull_request", simulatedTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)

	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 5002, "pull_request", simulatedTier))

	// THE ASSIGNMENT WAS HANDLED, proved by its acknowledgement: the listener acks
	// a message only after acting on it, and the fake keeps an unacknowledged
	// head in place. Asserting "nothing happened" after a sleep would pass while
	// the assignment sat undelivered behind an unacknowledged offer.
	const assignedMessage = 2

	deadline = time.Now().Add(30 * time.Second)

	for !slices.Contains(s.plane.ackedIDs(), assignedMessage) {
		if time.Now().After(deadline) {
			t.Fatalf("the assignment was never acknowledged; acked %v", s.plane.ackedIDs())
		}

		time.Sleep(50 * time.Millisecond)
	}

	instances, err := s.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(instances) != 0 {
		t.Fatalf("modelled pull-request work on a backend with no boundary: %v", instances)
	}

	if calls := s.plane.Calls("generatejitconfig"); len(calls) != 0 {
		t.Errorf("minted %d runner registration(s) for a job that was refused; "+
			"each one is an orphan on GitHub", len(calls))
	}
}
