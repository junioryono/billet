package server

import (
	"errors"
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// wiredListeners builds one listener per tier the way the control plane does,
// through Server.listenerOpts, so a test exercises the options production
// actually hands a listener rather than a hand-assembled subset.
func wiredListeners(t *testing.T, tiers []config.Tier, vcpu int) (*alloc.Allocator, []*Listener) {
	t.Helper()

	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: vcpu, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	registerHost(t, a)

	s := New(a, nil, tiers, "promise-test", nil)
	listeners := make([]*Listener, 0, len(tiers))

	for i := range tiers {
		listeners = append(listeners, NewListener(a, tiers[i].Label, &fakeSession{}, s.listenerOpts(nil)...))
	}

	return a, listeners
}

// RELEASING IDLE ESCROW NEVER TOUCHES A LEASE PROMISED TO AN ACQUISITION.
//
// A promise is capacity an AcquireJobs call may already have claimed at GitHub;
// handing it back would let another tier take room this one owes a job. Only
// `held` is ever surplus. The rule predates #140 and outlives it: with no idle
// escrow the surplus release runs on every poll, which makes keeping its hands
// off `acquiring` matter more, not less.
func TestReleasingIdleEscrowNeverTouchesAPromisedLease(t *testing.T) {
	t.Parallel()

	a, listeners := wiredListeners(t, []config.Tier{tier("a-work")}, 3*tierVCPU)
	l := listeners[0]

	if err := l.refillEscrowUngated(t.Context(), 2, 2); err != nil {
		t.Fatal(err)
	}

	if got := l.reserve([]resolvedJob{{job: Job{RequestID: 11}}}); !slices.Equal(got, []int64{11}) {
		t.Fatalf("reserved %v, want request 11", got)
	}

	promised := l.acquiring[11].lease

	l.releaseIdleEscrowAbove(t.Context(), l.targetCapacity())

	if l.idleEscrow() != 0 || len(l.acquiring) != 1 {
		t.Fatalf("after the surplus release: held %d, promises %d; want 0, 1",
			l.idleEscrow(), len(l.acquiring))
	}

	if err := a.Heartbeat(t.Context(), promised.ID, promised.Epoch); err != nil {
		t.Fatalf("the surplus release handed back a lease promised to an acquisition: %v", err)
	}
}

// AN AMBIGUOUS ACQUISITION KEEPS ITS PROMISE AND ITS CHARGE, SO NO OTHER TIER CAN
// TAKE THE ROOM IT MAY ALREADY OWE.
//
// A lost AcquireJobs response, or one naming a request billet never asked for,
// cannot say whether GitHub assigned the job. Could-not-tell keeps the
// capacity: the promise stays, the lease stays charged, and a peer that wants
// the only slot in the fleet does not get it.
func TestAnAmbiguousAcquisitionKeepsItsPromiseAndItsCharge(t *testing.T) {
	t.Parallel()

	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost response", true: "unrequested response"}[malformed], func(t *testing.T) {
			t.Parallel()

			// ROOM FOR ONE.
			a, listeners := wiredListeners(t, []config.Tier{tier("a-donor"), tier("b-work")}, tierVCPU)
			donor, peer := listeners[0], listeners[1]

			lost := errors.New("acquisition response lost")
			want := lost

			if malformed {
				want = ErrUntrustworthySession
			}

			donor.session = &fakeSession{onAcquire: func([]int64) ([]int64, error) {
				if malformed {
					return []int64{99}, nil
				}

				return nil, lost
			}}

			// THE OFFER PATH'S OWN PURCHASE: escrow for exactly the offer in hand.
			if err := donor.refillEscrowTo(t.Context(), donor.targetCapacityFor(1)); err != nil {
				t.Fatal(err)
			}

			if err := donor.acquire(t.Context(), []Job{{RequestID: 11, RunID: 101}}); !errors.Is(err, want) {
				t.Fatalf("acquire = %v, want %v", err, want)
			}

			donor.releaseIdleEscrowAbove(t.Context(), donor.targetCapacity())

			if donor.idleEscrow() != 0 || len(donor.acquiring) != 1 {
				t.Fatalf("ambiguous acquisition has held %d, promises %d; want 0, 1",
					donor.idleEscrow(), len(donor.acquiring))
			}

			if err := a.Heartbeat(t.Context(), donor.acquiring[11].lease.ID,
				donor.acquiring[11].lease.Epoch); err != nil {
				t.Fatalf("ambiguous acquisition lost its charge: %v", err)
			}

			if err := peer.backPoolSlot(t.Context()); err != nil {
				t.Fatal(err)
			}

			if peer.capacity() != 0 {
				t.Fatal("a peer bought capacity still owed by the ambiguous request")
			}
		})
	}
}

// AN OFFER CANCELLED IN ITS OWN BATCH MUST NOT BE ACQUIRED, AND MUST NOT KEEP THE
// SLOT CHARGED. The escrow the offer path bought for it goes back, and a peer
// can buy that room.
func TestCancelledOfferDoesNotCreateAPromise(t *testing.T) {
	_, listeners := wiredListeners(t, []config.Tier{tier("a-work"), tier("b-idle")}, tierVCPU)
	work, idle := listeners[0], listeners[1]
	session := &fakeSession{}
	work.session = session

	if err := work.handle(t.Context(), &Message{
		MessageID: 1,
		Available: []Job{{RequestID: 11, RunID: 101}},
		Completed: []Job{{RequestID: 11, RunID: 101}},
	}); err != nil {
		t.Fatal(err)
	}

	if ids := session.acquiredIDs(); len(ids) != 0 {
		t.Fatalf("acquired cancelled request %v", ids)
	}

	work.releaseIdleEscrowAbove(t.Context(), work.targetCapacity())

	if err := idle.backPoolSlot(t.Context()); err != nil {
		t.Fatal(err)
	}

	if work.committedCapacity() != 0 || idle.capacity() != 1 {
		t.Fatalf("cancelled offer kept capacity: work %d, idle %d",
			work.committedCapacity(), idle.capacity())
	}

	for _, unrelated := range []bool{false, true} {
		t.Run(map[bool]string{false: "pooled completion", true: "unrelated offer shares launch id"}[unrelated], func(t *testing.T) {
			tiers := []config.Tier{tier("a-work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			session := &fakeSession{}
			var destroyed []int64
			work := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{
				onDestroy: func(requestID int64) error {
					destroyed = append(destroyed, requestID)
					return nil
				},
			}), WithRunnerRegistry(&fakeRunnerRegistry{}))
			if err := work.refillEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := work.handle(t.Context(), &Message{
				MessageID: 1, Statistics: &Statistics{TotalAssignedJobs: 1},
			}); err != nil {
				t.Fatal(err)
			}
			members, err := a.PoolRunners(t.Context(), work.tier)
			if err != nil || len(members) != 1 || members[0].LaunchRequestID >= 0 {
				t.Fatalf("anonymous pool runner = %+v, err %v", members, err)
			}
			member := members[0]
			actual := Job{RequestID: 12, RunID: 101, JobID: "completed-job",
				RunnerID: 77, RunnerName: member.RunnerName, Result: "Succeeded"}
			available := []Job{{RequestID: actual.RequestID, RunID: actual.RunID, JobID: actual.JobID}}
			var want []int64
			if unrelated {
				available = append(available, Job{RequestID: member.LaunchRequestID, RunID: 102, JobID: "unfinished-job"})
				want = []int64{member.LaunchRequestID}
			}
			if err := work.handle(t.Context(), &Message{
				MessageID: 2, Started: []Job{actual}, Completed: []Job{actual}, Available: available,
			}); err != nil {
				t.Fatal(err)
			}
			if ids := session.acquiredIDs(); !slices.Equal(ids, want) {
				t.Errorf("acquired %v, want %v; completion must filter actual request 12 only", ids, want)
			}
			if !slices.Equal(destroyed, []int64{member.LaunchRequestID}) {
				t.Errorf("destroyed %v, want launch request %d", destroyed, member.LaunchRequestID)
			}
			if work.Running() != 0 || work.Acquiring() != len(want) {
				t.Errorf("after completion: running %d, promises %d; want 0, %d",
					work.Running(), work.Acquiring(), len(want))
			}
		})
	}

	for _, tc := range []struct {
		name           string
		keepJobID      bool
		offerRequestID int64
	}{
		{name: "runner name only", offerRequestID: 11},
		{name: "job id without request id", keepJobID: true, offerRequestID: 11},
		{name: "runner name only with zero-request offer"},
		{name: "job id without request id with zero-request offer", keepJobID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tiers := []config.Tier{tier("a-work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			session := &fakeSession{}
			var destroyed []int64
			work := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{
				onDestroy: func(requestID int64) error {
					destroyed = append(destroyed, requestID)
					return nil
				},
			}), WithRunnerRegistry(&fakeRunnerRegistry{}))
			if err := work.refillEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			actual := Job{RequestID: 11, RunID: 101, JobID: "completed-job"}
			if err := work.handle(t.Context(), &Message{MessageID: 1, Assigned: []Job{actual}}); err != nil {
				t.Fatal(err)
			}
			members, err := a.PoolRunners(t.Context(), work.tier)
			if err != nil || len(members) != 1 || members[0].LaunchRequestID != 11 {
				t.Fatalf("launched runner = %+v, err %v; want request 11", members, err)
			}
			actual.RunnerID, actual.RunnerName = 77, members[0].RunnerName
			if err := work.handle(t.Context(), &Message{MessageID: 2, Started: []Job{actual}}); err != nil {
				t.Fatal(err)
			}
			binding, err := a.PoolRunnerByName(t.Context(), actual.RunnerName)
			if err != nil || binding.Status != alloc.PoolRunnerBusy || binding.ActualRequestID != 11 || binding.JobID != actual.JobID {
				t.Fatalf("busy binding = %+v, err %v; want actual request 11", binding, err)
			}
			completion := Job{RunnerName: actual.RunnerName, Result: "Cancelled"}
			if tc.keepJobID {
				completion.JobID = actual.JobID
			}
			if err := work.handle(t.Context(), &Message{
				MessageID: 3, Completed: []Job{completion},
				Available: []Job{{RequestID: tc.offerRequestID, RunID: actual.RunID, JobID: actual.JobID}},
			}); err != nil {
				t.Fatal(err)
			}
			if ids := session.acquiredIDs(); len(ids) != 0 {
				t.Errorf("acquired completed request %v after its wire identity was omitted", ids)
			}
			if !slices.Equal(destroyed, []int64{11}) || work.Running() != 0 || work.Acquiring() != 0 {
				t.Errorf("destroyed %v, running %d, promises %d; want [11], 0, 0",
					destroyed, work.Running(), work.Acquiring())
			}
			if _, exists, err := a.DirectJobIdentity(t.Context(), actual.JobID); err != nil {
				t.Fatal(err)
			} else if exists != (tc.offerRequestID == 0) {
				t.Errorf("direct identity exists = %t; only a zero-request offer may mint it", exists)
			}
		})
	}
}

// A DIRECT ASSIGNMENT ESTABLISHES IDENTITY BEFORE ITS SAME-BATCH CANCELLATION.
// Held escrow and absent statistics leave the completion as the only launch guard.
func TestAssignedAndCancelledDirectJobNeverLaunches(t *testing.T) {
	tiers := []config.Tier{tier("a-work")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
	var launched []int64
	work := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{
		onLaunch: func(requestID int64) error {
			launched = append(launched, requestID)
			return nil
		},
	}))
	if err := work.refillEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if work.idleEscrow() != 1 {
		t.Fatalf("held escrow = %d, want 1", work.idleEscrow())
	}
	if _, exists, err := a.DirectJobIdentity(t.Context(), "J"); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Fatal("fixture already has a direct identity for J")
	}
	err := work.handle(t.Context(), &Message{
		MessageID: 1,
		Assigned:  []Job{{JobID: "J"}},
		Completed: []Job{{JobID: "J", Result: "Cancelled"}},
	})
	if len(launched) != 0 || work.Running() != 0 || work.Acquiring() != 0 {
		t.Errorf("cancelled direct job launched %v, running %d, promises %d; want none",
			launched, work.Running(), work.Acquiring())
	}
	if err != nil {
		t.Fatalf("handle assigned-and-cancelled direct job: %v", err)
	}
	if work.idleEscrow() != 1 {
		t.Errorf("cancelled direct job consumed held escrow: %d remain, want 1", work.idleEscrow())
	}
}
