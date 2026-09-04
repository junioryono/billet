package e2e

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/fakeactions"
	"github.com/junioryono/billet/internal/provider"
)

// A JOB THAT OUTLIVES EVERY DEFERRED OPERATION, AND NONE OF THEM ENDS IT.
//
// The acceptance criterion nothing exercised: "a fake-clock multi-day job blocks
// shutdown, update, replacement, and teardown without destruction; the deferred
// operation resumes automatically after completion."
//
// It is a promise the code already CLAIMS to keep, so these tests can fail
// informatively. Everything under them is real — a real dispatch, the real node
// wire, the real drain, and on the docker leg a real container runtime — and the
// single thing steered is the allocator's clock.
//
// WHAT THE CLOCK IS FOR HERE, AND WHAT IT IS NOT. It carries the scenario across
// two simulated days, so "billet imposes no job limit" is asserted rather than
// asserted about a job that ran for four seconds. It is NOT a test of lease
// renewal across that span: `expires_at` is written from this same clock and
// `Reap` reads it, so a clock that jumps two hours between two heartbeats
// manufactures an expiry continuous time cannot produce. That rule has its own
// deterministic test in the package that owns it — alloc's
// TestHeartbeatExtendsTheLease — and re-proving it here through a stepped clock
// would be a test whose verdict depends on which goroutine ran.
//
// So the reaper is off, exactly as computebarrier_test.go turns it off and for a
// related reason: what these tests are about must not race billet's own timers.
// Worth knowing that even with it ON nothing here would be DESTROYED — Reap
// moves a lease that may have compute to quarantine and leaves the container
// alone — but a quarantined lease is a job whose bookkeeping was lost, which is
// a different scenario from one that is merely long.
const (
	// simulatedJobDuration is comfortably past GitHub's own six-hour default for
	// a hosted runner, which is the number an operator would expect to be the
	// limit and which billet deliberately does not impose.
	simulatedJobDuration = 48 * time.Hour
	// simulatedStep keeps the advance visible in a failure: an assertion that
	// trips reports how far into the job it got.
	simulatedStep = 2 * time.Hour
	// nodeDrainReportAfter is this node's `drain_timeout`, set far below the
	// window a drain is watched for.
	//
	// THE NUMBER USED TO END THE WAIT AND DESTROY WHATEVER WAS LEFT, which made a
	// timer the thing that failed somebody's build; it is now a reporting
	// threshold with no deadline behind it. Left at the default of hours, no test
	// ever reaches it and the two readings are indistinguishable. At 200ms against
	// a three-second watch, a drain still waiting has outlived its own configured
	// threshold by more than an order of magnitude — which is the difference,
	// observed rather than argued.
	nodeDrainReportAfter = 200 * time.Millisecond
)

// multiDayStack is barrierStack with a drain_timeout a test can outlive.
func multiDayStack(t *testing.T, opts ...stackOpt) (*stack, *offsetClock) {
	t.Helper()

	clock := &offsetClock{}

	return newStackIn(t, t.TempDir(), newPlane(t), append([]stackOpt{overTheWire,
		withClock(clock.now), withReapInterval(time.Hour),
		withNodeDrainTimeout(nodeDrainReportAfter)}, opts...)...), clock
}

// TestAMultiDayJobBlocksUpdateAndTeardown proves the deferred operations
// reachable without stopping the control plane, and that the one that was
// WAITING finishes by itself once the job does.
//
// The legs, and what each actually is in billet:
//
//   - TEARDOWN / REPLACEMENT is `billet nodes decommission`, whose
//     outstanding-lease refusal is the one guard in that command with no --force
//     behind it. An infrastructure replacement that removed this host goes
//     through it, and Terraform timing out is not permission to terminate a job.
//   - THE DRAIN BARRIER is what `billet drain --wait` and `billet local down`
//     both wait on, and what an infrastructure operation must invoke before it
//     may replace anything.
//   - UPDATE is `hostupgrade.Host.StopNode`, which is `systemctl stop
//     billet-node` — the node's SIGTERM drain, documented in
//     nodeclient.stopGracefully as carrying no deadline at all. That is the real
//     thing here rather than a fake: the goroutine below makes the same call the
//     upgrade transaction makes.
func TestAMultiDayJobBlocksUpdateAndTeardown(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			s, clock := multiDayStack(t, b.opts...)

			leaseID, _ := s.startAMultiDayJob(t, 5101, clock)

			// TEARDOWN, AND IT IS REFUSED FOR THE RIGHT REASON. Asserted on the sentinel
			// AND in both directions of --force: `Decommission` has several refusals, and
			// a test happy with "an error" would pass against the liveness one, which
			// --force overrides. This one deliberately has nothing behind it, because a
			// decommissioned host is excluded from every tier's floor while its capacity
			// stays charged either way.
			for _, force := range []bool{false, true} {
				_, err := s.alloc.Decommission(t.Context(), alloc.DecommissionRequest{
					Node: s.node, Actor: "e2e", Force: force,
				})
				if !errors.Is(err, alloc.ErrNotDecommissionable) {
					t.Fatalf("decommission(force=%v) of a host running a two-day job returned %v, want "+
						"%v — its capacity is still charged, so excluding it silently changes what "+
						"every tier's floor believes is already met",
						force, err, alloc.ErrNotDecommissionable)
				}
			}

			// THE BARRIER BOTH OTHER OPERATIONS WAIT ON, and it holds at both stages.
			sealed := s.seal(t)

			q, err := s.alloc.Quiescence(t.Context())
			if err != nil {
				t.Fatalf("Quiescence: %v", err)
			}

			if q.Quiet() {
				t.Fatalf("the ledger reported quiet while a job had been running for %s: %+v",
					simulatedJobDuration, q)
			}

			// NAMED, NOT COUNTED. The report exists so an operator can recognise their
			// own work in it; "1 lease" tells them nothing about whether to keep waiting.
			if !outstandingHolds(q, leaseID) {
				t.Fatalf("the barrier does not name the lease it is waiting on (%s): %+v",
					leaseID, q.Outstanding)
			}

			barrier, err := s.alloc.RequestComputeBarrier(t.Context(), sealed.Generation, "e2e")
			if err != nil {
				t.Fatalf("RequestComputeBarrier: %v", err)
			}

			s.ask(t, barrier.ID)

			c, host := s.clearanceFor(t, s.node)
			if c.Clear() || host.State != alloc.ClearanceRunning {
				t.Fatalf("the fleet is clear=%v with %s in state %v, want not-clear and %v",
					c.Clear(), s.node, host.State, alloc.ClearanceRunning)
			}

			// AND ELAPSED TIME ALONE NEVER CLEARS A HOST THAT IS RUNNING SOMETHING.
			// computeAbsenceGrace is a span between two EMPTY answers, so moving past it
			// while the host keeps answering "I am running something" must change nothing
			// whatever.
			clock.advance(6 * time.Minute)
			s.ask(t, barrier.ID)

			c, host = s.clearanceFor(t, s.node)
			if c.Clear() || host.State != alloc.ClearanceRunning {
				t.Fatalf("advancing past the absence grace cleared a host that is still running "+
					"compute: clear=%v state=%v", c.Clear(), host.State)
			}

			// UPDATE. The real node drain, on its own goroutine because it is not
			// expected to return.
			drained := make(chan struct{})

			go func() {
				defer close(drained)

				s.stopNode()
			}()

			// DAYS PASS WHILE THE DRAIN IS WAITING, which is a different claim from the
			// days that passed before it started and is the one this test is named for. A
			// review of the first version caught that: it advanced the clock and only
			// afterwards began the drain, so it proved that an old lease blocks a new
			// operation briefly rather than that a waiting operation survives elapsed
			// time.
			//
			// Everything the allocator decides — the barrier's grace, lease expiry,
			// quarantine — moves with this. What it cannot reach is a real-time deadline
			// inside the drain itself, and the assertion below is what covers that: this
			// node's drain_timeout is 200ms, so by the time the window closes the drain
			// has been waiting more than an order of magnitude past its own configured
			// threshold and has still not ended. That number USED to bound the wait and
			// then destroy whatever was left; a regression restoring it lands here.
			for elapsed := time.Duration(0); elapsed < simulatedJobDuration; elapsed += simulatedStep {
				clock.advance(simulatedStep)
			}

			// IT MUST NOT RETURN, and the window is generous because the claim is a
			// negative one. A node holding nothing returns from this immediately
			// (stopGracefully's Holding() fast path), so a drain that stopped waiting for
			// running compute lands here in milliseconds rather than timing out.
			select {
			case <-drained:
				t.Fatal("the node's drain returned while it was still holding a running job; a stop " +
					"is not evidence that the work on a host should end, and GitHub does not requeue " +
					"a job whose runner vanished after starting")
			case <-time.After(3 * time.Second):
			}

			// ...and the container is untouched, which is the substantive half. An error
			// value is the cheapest thing a function produces; what matters is that
			// nothing else happened.
			s.assertStillRunning(t, leaseID)

			// THE JOB FINISHES, AND THE DEFERRED OPERATION RESUMES BY ITSELF.
			s.plane.queue(fakeactions.StatisticsJSON(0, 0),
				fakeactions.JobJSON("JobCompleted", 5101, "push", testTier))

			select {
			case <-drained:
			case <-time.After(90 * time.Second):
				t.Fatal("the node's drain never returned after its job completed; a drain that does " +
					"not end on its own is one an operator has to kill")
			}

			s.awaitGone(t)

			// The ledger barrier settles first...
			deadline := time.Now().Add(30 * time.Second)

			for {
				q, err := s.alloc.Quiescence(t.Context())
				if err != nil {
					t.Fatalf("Quiescence: %v", err)
				}

				if q.Quiet() {
					break
				}

				if time.Now().After(deadline) {
					t.Fatalf("the ledger never went quiet after the job finished: %+v", q)
				}

				time.Sleep(100 * time.Millisecond)
			}

			// ...AND THE COMPUTE BARRIER DOES NOT, WHICH IS THE ORDERING LESSON THIS
			// SCENARIO ENDS ON RATHER THAN A GAP IN IT.
			//
			// The second stage asks each HOST what its provider is running, and the host
			// that would answer is the one this leg just stopped. Whatever it is reported
			// as afterwards — still running, from telemetry that was already stale when it
			// arrived; settling, waiting on an empty answer that can never come; or
			// unreachable — every one of those is not-clear, and none of them can become
			// clear on its own, because the proof is two observations and nothing is left
			// to give the second.
			//
			// THAT IS WHY `billet local down` PROVES THE FLEET CLEAR BEFORE IT STOPS
			// ANYTHING, rather than stopping and then asking. Running the drain in this
			// order — which is what an update does, since the node stops first so its
			// compute drains while the control plane is still there to record what
			// happened to it — leaves the deployment unprovable until the host comes back.
			//
			// Asserted as `!Clear()` rather than on a particular state, because all three
			// are the safe direction and pinning one would make this test about which of
			// them the host happened to land in. That a barrier DOES clear once a host
			// answers empty twice across the grace is
			// TestTheBarrierSeesRealComputeWhoseLeaseIsGone's subject, on a stack whose
			// node is still there to answer.
			//
			// AND NOTHING IS ASKED HERE, deliberately. The first version put two more
			// `ask` rounds in — which a review caught: the node loop has exited, so
			// `AskNodeForTest` dispatches to nobody, returns nil whatever happened, and
			// can spend its whole 60-second deadline doing it. The assertion then passed
			// on the record from BEFORE the stop while looking like it rested on two
			// fresh rounds, and the test took two minutes to say so. Time is advanced
			// instead, which is the only thing that could still change the answer.
			clock.advance(6 * time.Minute)

			if c, _ = s.clearanceFor(t, s.node); c.Clear() {
				t.Fatal("the fleet was reported CLEAR with no host left to answer for it; a proof is " +
					"two observations from a machine billet can reach, and stopping that machine is " +
					"not one of them")
			}
		})
	}
}

// TestAMultiDayJobSurvivesAControlPlaneShutdown is the fourth deferred
// operation, and it needs its own stack because it ENDS the control plane.
//
// WHAT A STOP LEAVES BEHIND IS DELIBERATE, and it used to be the opposite: the
// listener's teardown bounded its wait by drain_timeout and then destroyed
// whatever was still running, which made a timer the thing that failed
// somebody's build. `destroyAll` now takes includeRunning, a shutdown passes
// false, and `billet force-destroy` is the only caller that passes true.
//
// THE HARNESS'S STOP HURRIES, WHICH MAKES THIS THE STRONGER CASE rather than a
// weaker one: `run`'s stop closes the operator's second signal, so this is not a
// drain that was allowed to finish — it is the give-up path, and even there the
// guests keep running, the leases stay CHARGED, and the next control plane
// re-adopts them. Freeing a slot whose container is live is the overcommit the
// whole ordering exists to prevent.
func TestAMultiDayJobSurvivesAControlPlaneShutdown(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			s, clock := multiDayStack(t, b.opts...)

			leaseID, stop := s.startAMultiDayJob(t, 5102, clock)

			before, err := s.alloc.Usage(t.Context())
			if err != nil {
				t.Fatalf("Usage: %v", err)
			}

			if before.VCPU == 0 {
				t.Fatalf("nothing was charged for a running job: %+v", before)
			}

			// THE CONTROL PLANE STOPS, hurried.
			stop()

			// The container is still there, still RUNNING, and still this job's own.
			s.assertStillRunning(t, leaseID)

			// AND ITS CAPACITY IS STILL CHARGED. Asserted against the LEASE rather than
			// against total usage, which never falls to zero while a listener runs: free
			// escrow a listener holds to offer GitHub is indistinguishable from a leak in
			// an aggregate.
			held, err := s.alloc.Lease(t.Context(), leaseID)
			if err != nil {
				t.Fatalf("the job's lease is gone after a shutdown, so its container is now compute "+
					"nothing accounts for: %v", err)
			}

			if held.Phase.Terminal() {
				t.Fatalf("the job's lease is %s after a shutdown; a stop is not a completion", held.Phase)
			}

			if held.VCPU == 0 || held.Memory == 0 {
				t.Fatalf("the surviving lease is charged nothing (%d vCPU, %s), so its host's capacity "+
					"can be sold twice", held.VCPU, held.Memory)
			}
		})
	}
}

// startAMultiDayJob runs a real job to a real running container and then moves
// the allocator's clock across simulatedJobDuration, asserting on every step
// that nothing has ended it. It returns the job's lease id and the control
// plane's stop.
func (s *stack) startAMultiDayJob(
	t *testing.T, requestID int64, clock *offsetClock,
) (string, func()) {
	t.Helper()

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", requestID, "push", testTier))

	stop := s.runBarrierStack(t)

	deadline := time.Now().Add(30 * time.Second)
	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", requestID, "push", testTier))

	names := s.awaitOneRunning(t)

	leaseID, ours := provider.LeaseOf(names[0])
	if !ours || leaseID == "" {
		t.Fatalf("container %q does not carry a billet lease name", names[0])
	}

	for elapsed := time.Duration(0); elapsed < simulatedJobDuration; elapsed += simulatedStep {
		clock.advance(simulatedStep)

		// CHECKED EVERY STEP rather than only at the end, so a failure names how
		// far into the job it got. Elapsed time is not evidence that a job stopped
		// making progress, and nothing here is entitled to act as though it were.
		if inst := s.runningInstance(t); inst == nil {
			t.Fatalf("the container was gone %s into a job billet imposes no limit on",
				elapsed+simulatedStep)
		}
	}

	s.assertStillRunning(t, leaseID)

	return leaseID, stop
}

// assertStillRunning is the substantive claim in every leg: a container carrying
// THIS lease's name is on the host, and it is RUNNING.
//
// RUNNING, NOT MERELY PRESENT. `provider.List` reports exited containers too, so
// without this a job whose runner had died would satisfy every assertion here
// while proving nothing about compute that is still executing.
//
// AND IT IS THE JOB'S OWN. Counting one running container would pass against a
// billet that destroyed this one and launched another, which is the failure
// these tests exist to catch wearing different clothes.
func (s *stack) assertStillRunning(t *testing.T, leaseID string) {
	t.Helper()

	inst := s.runningInstance(t)
	if inst == nil {
		// RETURNED EXPLICITLY rather than leaning on t.Fatalf ending the
		// goroutine. It does, but only from the goroutine running the test — this
		// is a helper, and staticcheck reads the fall-through as a nil dereference
		// because that is exactly what it would be if this were ever called from
		// anywhere else.
		t.Fatalf("no running container remains for lease %s", leaseID)

		return
	}

	got, ours := provider.LeaseOf(inst.Name)
	if !ours || got != leaseID {
		t.Fatalf("the running container is %q (lease %q), not this job's lease %s",
			inst.Name, got, leaseID)
	}
}

// runningInstance returns the single RUNNING instance, or nil.
func (s *stack) runningInstance(t *testing.T) *provider.Instance {
	t.Helper()

	instances, err := s.provider.List(t.Context())
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var running []*provider.Instance

	for _, inst := range instances {
		if inst.Running {
			running = append(running, inst)
		}
	}

	switch len(running) {
	case 0:
		return nil
	case 1:
		return running[0]
	default:
		t.Fatalf("expected at most one running container, found %s", describe(instances))

		return nil
	}
}

// outstandingHolds reports whether the barrier names this exact lease.
func outstandingHolds(q alloc.Quiescence, leaseID string) bool {
	for _, o := range q.Outstanding {
		if o.ID == leaseID {
			return true
		}
	}

	return false
}
