package server

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
)

// Every representation reaches handle with real escrow and an observable launcher.
func TestBatchCompletionUsesEveryActualAlias(t *testing.T) {
	for _, tc := range []struct {
		name      string
		assigned  Job
		offer     Job
		completed Job
		direct    bool
	}{
		{name: "translated offer coalesces assignment aliases", assigned: Job{RequestID: 11, JobID: "J", RunID: 101}, offer: Job{RequestID: 12, JobID: "J", RunID: 101}, completed: Job{RequestID: 11, JobID: "J", RunID: 101}},
		{name: "positive request", assigned: Job{RequestID: 11}, offer: Job{RequestID: 11}, completed: Job{RequestID: 11}},
		{name: "positive assignment establishes job alias", assigned: Job{RequestID: 11, JobID: "J"}, offer: Job{RequestID: 11, JobID: "J"}, completed: Job{JobID: "J"}},
		{name: "zero assignment establishes direct alias", assigned: Job{JobID: "J"}, offer: Job{JobID: "J"}, completed: Job{JobID: "J"}},
		{name: "negative request recovers job alias", direct: true, assigned: Job{RequestID: -1}, offer: Job{RequestID: -1}, completed: Job{JobID: "J"}},
		{name: "negative completion recovers job alias", direct: true, assigned: Job{RequestID: 11, JobID: "J"}, offer: Job{RequestID: 11, JobID: "J"}, completed: Job{RequestID: -1}},
		{name: "zero assignment matches positive completion", assigned: Job{JobID: "J"}, offer: Job{JobID: "J"}, completed: Job{RequestID: 11, JobID: "J"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tiers := []config.Tier{tier("work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			if tc.direct {
				id, err := a.IdentifyDirectJob(t.Context(), "J")
				if err != nil || id != -1 {
					t.Fatalf("direct identity = %d, %v; want -1", id, err)
				}
			}
			session := &fakeSession{}
			var launched []int64
			l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{
				onLaunch: func(id int64) error { launched = append(launched, id); return nil },
			}))
			if err := l.refillEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if l.idleEscrow() != 1 {
				t.Fatal("fixture has no launchable escrow")
			}
			tc.completed.Result = "Cancelled"
			if err := l.handle(t.Context(), &Message{MessageID: 1,
				Assigned: []Job{tc.assigned}, Available: []Job{tc.offer}, Completed: []Job{tc.completed},
			}); err != nil {
				t.Fatal(err)
			}
			if len(launched) != 0 || len(session.acquiredIDs()) != 0 || l.Running() != 0 || l.Acquiring() != 0 || l.idleEscrow() != 1 {
				t.Fatalf("cancelled job: launched %v, acquired %v, running %d, promises %d, held %d",
					launched, session.acquiredIDs(), l.Running(), l.Acquiring(), l.idleEscrow())
			}
		})
	}
}

// The reused launch ID case proves dispatch past the completion filter, not a
// successful replacement: its old pool row remains reserved until acknowledgement.
func TestBusyPoolCompletionFiltersAssignmentDispatchByActualJob(t *testing.T) {
	for _, requestID := range []int64{11, 0, -1} {
		name := map[int64]string{11: "positive actual", 0: "zero actual", -1: "unfinished launch alias"}[requestID]
		t.Run(name, func(t *testing.T) {
			tiers := []config.Tier{tier("work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 2 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			var launched, destroyed []int64
			l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunnerRegistry(&fakeRunnerRegistry{}),
				WithRunner(&fakeRunner{
					onLaunch: func(id int64) error {
						launched = append(launched, id)
						if len(launched) > 1 {
							// Prove dispatch without re-registering a launch ID whose
							// completion acknowledgement is still pending.
							return errors.New("injected replacement launch failure")
						}
						return nil
					},
					onDestroy: func(id int64) error { destroyed = append(destroyed, id); return nil },
				}))
			if err := l.refillEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := l.handle(t.Context(), &Message{MessageID: 1, Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
				t.Fatal(err)
			}
			members, err := a.PoolRunners(t.Context(), l.tier)
			if err != nil || len(members) != 1 || members[0].LaunchRequestID != -1 || !slices.Equal(launched, []int64{-1}) {
				t.Fatalf("pool fixture: members %+v, launches %v, err %v", members, launched, err)
			}
			actual := Job{RequestID: 11, RunID: 101, JobID: "J", RunnerID: 77, RunnerName: members[0].RunnerName}
			if err := l.handle(t.Context(), &Message{MessageID: 2, Started: []Job{actual}}); err != nil {
				t.Fatal(err)
			}
			binding, err := a.PoolRunnerByName(t.Context(), actual.RunnerName)
			if err != nil || binding.ActualRequestID != 11 || binding.Status != alloc.PoolRunnerBusy {
				t.Fatalf("busy binding = %+v, %v", binding, err)
			}
			if err := l.refillEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if l.idleEscrow() != 1 {
				t.Fatalf("replacement escrow = %d, want 1", l.idleEscrow())
			}
			assigned := Job{RequestID: requestID, RunID: 101, JobID: "J"}
			wantLaunches := []int64{-1}
			wantRunning := 0
			if requestID == -1 {
				assigned.RunID, assigned.JobID = 102, "unfinished"
				wantLaunches = append(wantLaunches, -1)
			}
			if err := l.handle(t.Context(), &Message{MessageID: 3, Assigned: []Job{assigned},
				Completed: []Job{{RunnerName: actual.RunnerName, Result: "Cancelled"}},
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(launched, wantLaunches) || !slices.Equal(destroyed, []int64{-1}) || l.Running() != wantRunning {
				t.Fatalf("launches %v, destroys %v, running %d; want %v, [-1], %d",
					launched, destroyed, l.Running(), wantLaunches, wantRunning)
			}
		})
	}
}

func TestKnownIncarnationContradictsAnOfferAlias(t *testing.T) {
	for _, tc := range []struct {
		name  string
		offer Job
	}{
		{name: "different run sharing job", offer: Job{RequestID: 12, RunID: 102, JobID: "J"}},
		{name: "different job sharing request", offer: Job{RequestID: 11, RunID: 101, JobID: "K"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, listeners := arbitrationListeners(t, []config.Tier{tier("work")}, tierVCPU)
			l := listeners[0]
			session := &fakeSession{}
			l.session = session
			if err := l.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := l.handle(t.Context(), &Message{MessageID: 1, Available: []Job{tc.offer},
				Completed: []Job{{RequestID: 11, RunID: 101, JobID: "J", Result: "Cancelled"}},
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(session.acquiredIDs(), []int64{tc.offer.RequestID}) || l.Acquiring() != 1 {
				t.Fatalf("unfinished offer acquired %v, promises %d; want [%d], 1",
					session.acquiredIDs(), l.Acquiring(), tc.offer.RequestID)
			}
		})
	}
}

func TestCompletionPreservesDemandForAnotherKnownRun(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("a-work"), tier("b-work")}, tierVCPU)
	first, second := listeners[0], listeners[1]
	first.observed = &Statistics{TotalAssignedJobs: 1}
	if err := first.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := second.handle(t.Context(), &Message{MessageID: 1,
		Available: []Job{{RequestID: 12, RunID: 102, JobID: "J"}},
	}); err != nil {
		t.Fatal(err)
	}
	if len(second.waitingOffers) != 1 {
		t.Fatal("fixture did not retain the refused offer")
	}
	if err := second.handle(t.Context(), &Message{MessageID: 2,
		Completed: []Job{{RequestID: 11, RunID: 101, JobID: "J", Result: "Cancelled"}},
	}); err != nil {
		t.Fatal(err)
	}
	_, turn := first.admissionPoll()
	first.finishAdmissionTurn(turn)
	if len(second.waitingOffers) != 1 || second.waitingOffers[0].run != 102 || !second.arbiter.permits(second.tier) {
		t.Fatalf("other run lost its demand or priority: %+v", second.waitingOffers)
	}
}

func TestCompletedRunnerDropsOnlyItsDischargedCommitment(t *testing.T) {
	for _, outcome := range []string{"acquired", "offer refused", "destroy failed"} {
		t.Run(outcome, func(t *testing.T) {
			a, listeners := arbitrationListeners(t, []config.Tier{tier("work")}, 2*tierVCPU)
			l := listeners[0]
			session := &fakeSession{}
			if outcome == "offer refused" {
				session.onAcquire = func([]int64) ([]int64, error) { return nil, nil }
			}
			l.session = session
			var destroyed []int64
			l.runner = &fakeRunner{onDestroy: func(id int64) error {
				destroyed = append(destroyed, id)
				if outcome == "destroy failed" {
					return errors.New("injected destroy failure")
				}
				return nil
			}}
			if err := l.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := l.refillEscrowUngated(t.Context(), 2, 2); err != nil {
				t.Fatal(err)
			}
			job := Job{RequestID: 11, JobID: "J", RunID: 101}
			if err := l.handle(t.Context(), &Message{MessageID: 1, Assigned: []Job{job}}); err != nil {
				t.Fatal(err)
			}
			lease := l.running[11]
			if lease == nil {
				t.Fatal("fixture did not launch request 11")
			}
			if l.idleEscrow() != 1 {
				t.Fatalf("fixture has %d spare leases, want 1", l.idleEscrow())
			}
			offer := Job{RequestID: 13, JobID: "J", RunID: 101}
			if err := l.handle(t.Context(), &Message{MessageID: 2, Available: []Job{offer},
				Completed: []Job{{RequestID: 12, JobID: "K", RunID: 102,
					RunnerName: provider.InstanceName(lease.ID), Result: "Succeeded"}},
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(destroyed, []int64{11}) {
				t.Fatalf("destroyed %v, want the runner launched for 11", destroyed)
			}
			if outcome == "destroy failed" {
				if len(session.acquiredIDs()) != 0 || l.Running() != 1 || l.Acquiring() != 0 {
					t.Fatalf("undischarged ownership: acquired %v, running %d, promises %d",
						session.acquiredIDs(), l.Running(), l.Acquiring())
				}
				return
			}
			if !slices.Equal(session.acquiredIDs(), []int64{13}) || l.Running() != 0 {
				t.Fatalf("unfinished J: acquired %v, running %d", session.acquiredIDs(), l.Running())
			}
			if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
				t.Fatalf("completed lease still open: %v", err)
			}
			if outcome == "offer refused" {
				if l.Acquiring() != 0 || len(l.waitingOffers) != 1 || l.waitingOffers[0].job != "J" || l.waitingOffers[0].run != 101 {
					t.Fatalf("refused J lost demand: promises %d, waiting %+v", l.Acquiring(), l.waitingOffers)
				}
			} else if p := l.acquiring[13]; p == nil || p.lease.ID == lease.ID || len(l.waitingOffers) != 0 {
				t.Fatalf("J was not backed by fresh escrow: promise %+v, waiting %+v", p, l.waitingOffers)
			}
		})
	}
}

func TestCompletionRequestDisambiguatesBothCandidateOrdersThroughHandle(t *testing.T) {
	for _, assignedID := range []int64{11, 12} {
		name := map[int64]string{11: "matching offer second", 12: "matching assignment first"}[assignedID]
		t.Run(name, func(t *testing.T) {
			tiers := []config.Tier{tier("work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 2 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			session := &fakeSession{}
			var launched, destroyed []int64
			l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{
				onLaunch:  func(id int64) error { launched = append(launched, id); return nil },
				onDestroy: func(id int64) error { destroyed = append(destroyed, id); return nil },
			}))
			if err := l.refillEscrowTo(t.Context(), 2); err != nil {
				t.Fatal(err)
			}
			offerID := 23 - assignedID
			if err := l.handle(t.Context(), &Message{MessageID: 1,
				Assigned:  []Job{{RequestID: assignedID, JobID: "J", RunID: assignedID + 90}},
				Available: []Job{{RequestID: offerID, JobID: "J", RunID: offerID + 90}},
				Completed: []Job{{RequestID: 12, JobID: "J", Result: "Cancelled"}},
			}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(destroyed, []int64{12}) {
				t.Fatalf("destroyed %v, want [12]", destroyed)
			}
			if assignedID == 11 {
				if !slices.Equal(launched, []int64{11}) || len(session.acquiredIDs()) != 0 || l.Running() != 1 {
					t.Fatalf("unfinished assignment: launches %v, acquisitions %v, running %d",
						launched, session.acquiredIDs(), l.Running())
				}
			} else if len(launched) != 0 || !slices.Equal(session.acquiredIDs(), []int64{11}) || l.Acquiring() != 1 {
				t.Fatalf("unfinished offer: launches %v, acquisitions %v, promises %d",
					launched, session.acquiredIDs(), l.Acquiring())
			}
		})
	}
}

func TestAmbiguousCompletionQuarantinesWithoutSelectingARun(t *testing.T) {
	for _, requestID := range []int64{0, 99} {
		for _, first := range []int64{11, 12} {
			tiers := []config.Tier{tier("work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 4 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			var launched, destroyed []int64
			var acknowledged bool
			session := &fakeSession{onDelete: func(int64) error {
				acknowledged = true
				return nil
			}}
			l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{
				onLaunch:  func(id int64) error { launched = append(launched, id); return nil },
				onDestroy: func(id int64) error { destroyed = append(destroyed, id); return nil },
			}))
			if err := l.refillEscrowTo(t.Context(), 4); err != nil {
				t.Fatal(err)
			}
			second := 23 - first
			err := l.handle(t.Context(), &Message{MessageID: 1, Statistics: &Statistics{TotalAssignedJobs: 3},
				Assigned: []Job{{RequestID: first, JobID: "J", RunID: first + 90},
					{RequestID: second, JobID: "J", RunID: second + 90},
					{RequestID: 21, JobID: "K", RunID: 201}},
				Available: []Job{{RequestID: first + 20, JobID: "J", RunID: first + 90},
					{RequestID: second + 20, JobID: "J", RunID: second + 90},
					{RequestID: 22, JobID: "L", RunID: 202}},
				Completed: []Job{{RequestID: requestID, JobID: "J", Result: "Cancelled"},
					{RequestID: first, JobID: "J", RunID: first + 90, Result: "Cancelled"}},
			})
			if poison, ok := errors.AsType[*poisonedMessageError](err); !ok || poison == nil || !errors.Is(err, errQuarantinableCompletion) {
				t.Fatalf("request %d, first %d: handle = %v, want quarantinable poison", requestID, first, err)
			}
			if len(destroyed) != 0 || acknowledged || !slices.Equal(launched, []int64{21}) {
				t.Fatalf("ambiguous completion acted: destroys %v, ack %v, launches %v", destroyed, acknowledged, launched)
			}
			if !slices.Equal(session.acquiredIDs(), []int64{22}) || l.Running() != 1 || l.Acquiring() != 1 || l.idleEscrow() != 2 {
				t.Fatalf("candidate work escaped hold: acquired %v, running %d, promises %d, held %d",
					session.acquiredIDs(), l.Running(), l.Acquiring(), l.idleEscrow())
			}
			if _, exists, err := a.DirectJobIdentity(t.Context(), "J"); err != nil || exists {
				t.Fatalf("completion minted identity: exists %v, err %v", exists, err)
			}
		}
	}
}

func TestAssignmentConsumesItsDirectPromiseAcrossRequestAliases(t *testing.T) {
	for _, capacity := range []int{1, 2} {
		name := map[int]string{1: "sole escrow", 2: "spare escrow"}[capacity]
		t.Run(name, func(t *testing.T) {
			tiers := []config.Tier{tier("work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: capacity * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			session := &fakeSession{}
			var launched []int64
			l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{
				onLaunch: func(id int64) error { launched = append(launched, id); return nil },
			}))
			if err := l.refillEscrowTo(t.Context(), capacity); err != nil {
				t.Fatal(err)
			}
			if err := l.handle(t.Context(), &Message{MessageID: 1, Available: []Job{{JobID: "J"}}}); err != nil {
				t.Fatal(err)
			}
			p := l.acquiring[-1]
			if p == nil || !slices.Equal(session.acquiredIDs(), []int64{0}) || l.idleEscrow() != capacity-1 {
				t.Fatalf("direct offer fixture: promise %+v, acquisitions %v, held %d", p, session.acquiredIDs(), l.idleEscrow())
			}
			if err := l.handle(t.Context(), &Message{MessageID: 2, Assigned: []Job{{RequestID: 11, JobID: "J"}}}); err != nil {
				t.Fatal(err)
			}
			if l.Acquiring() != 0 || l.running[11] != p.lease || l.idleEscrow() != capacity-1 || !slices.Equal(launched, []int64{11}) {
				t.Fatalf("assignment lost its promise: promises %d, running %+v, held %d, launches %v",
					l.Acquiring(), l.running[11], l.idleEscrow(), launched)
			}
			lease, err := a.Lease(t.Context(), p.lease.ID)
			if err != nil || lease.RequestID != 11 || lease.Phase != alloc.PhaseAssigned {
				t.Fatalf("promised lease = %+v, %v; want assigned request 11", lease, err)
			}
			// The running job keeps the same aliases even when redelivery changes
			// the request again. Spare escrow must remain unused.
			if err := l.handle(t.Context(), &Message{MessageID: 3, Assigned: []Job{{RequestID: 12, JobID: "J"}}}); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(launched, []int64{11}) || l.Running() != 1 || l.idleEscrow() != capacity-1 {
				t.Fatalf("redelivery spent another lease: launches %v, running %d, held %d", launched, l.Running(), l.idleEscrow())
			}
		})
	}
}

func TestAssignmentRequestSelectsItsPromiseAmongKnownRuns(t *testing.T) {
	for _, requestID := range []int64{11, 12} {
		tiers := []config.Tier{tier("work")}
		a := newAllocator(t, alloc.Limits{MaxVCPU: 2 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
		session := &fakeSession{}
		var launched []int64
		l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{
			onLaunch: func(id int64) error { launched = append(launched, id); return nil },
		}))
		if err := l.refillEscrowTo(t.Context(), 2); err != nil {
			t.Fatal(err)
		}
		if err := l.handle(t.Context(), &Message{MessageID: 1, Available: []Job{
			{RequestID: 11, JobID: "J", RunID: 101}, {RequestID: 12, JobID: "J", RunID: 102},
		}}); err != nil {
			t.Fatal(err)
		}
		p, other := l.acquiring[requestID], l.acquiring[23-requestID]
		if p == nil || other == nil || !slices.Equal(session.acquiredIDs(), []int64{11, 12}) {
			t.Fatalf("fixture promises: selected %+v, other %+v, acquired %v", p, other, session.acquiredIDs())
		}
		if err := l.handle(t.Context(), &Message{MessageID: 2, Assigned: []Job{{RequestID: requestID, JobID: "J"}}}); err != nil {
			t.Fatal(err)
		}
		if l.running[requestID] != p.lease || l.acquiring[23-requestID] != other || l.Acquiring() != 1 || !slices.Equal(launched, []int64{requestID}) {
			t.Fatalf("request %d consumed the wrong promise: running %+v, promises %+v, launches %v",
				requestID, l.running, l.acquiring, launched)
		}
	}
}

func TestConsumedPromiseRetainsItsRunAndRequestAliases(t *testing.T) {
	tiers := []config.Tier{tier("work")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 2 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
	var launched []int64
	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{
		onLaunch: func(id int64) error { launched = append(launched, id); return nil },
	}))
	if err := l.refillEscrowTo(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if err := l.handle(t.Context(), &Message{MessageID: 1, Available: []Job{{RequestID: 11, JobID: "J", RunID: 101}}}); err != nil {
		t.Fatal(err)
	}
	p := l.acquiring[11]
	if p == nil || l.idleEscrow() != 1 {
		t.Fatalf("fixture promise %+v, spare escrow %d", p, l.idleEscrow())
	}
	if err := l.handle(t.Context(), &Message{MessageID: 2, Assigned: []Job{{RequestID: 12, JobID: "J"}}}); err != nil {
		t.Fatal(err)
	}
	lease, err := a.Lease(t.Context(), p.lease.ID)
	if err != nil || lease.RequestID != 12 || lease.RunID != 0 {
		t.Fatalf("ledger assignment changed its authoritative fields: %+v, %v", lease, err)
	}
	// Without JobID, this redelivery can match only the consumed offer's alias.
	if err := l.handle(t.Context(), &Message{MessageID: 3, Assigned: []Job{{RequestID: 11, RunID: 101}}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(launched, []int64{12}) || l.idleEscrow() != 1 {
		t.Fatalf("offer alias lost: launches %v, spare escrow %d", launched, l.idleEscrow())
	}
	if err := l.handle(t.Context(), &Message{MessageID: 4, Assigned: []Job{{RequestID: 13, JobID: "J", RunID: 102}}}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(launched, []int64{12, 13}) || l.Running() != 2 || l.Acquiring() != 0 || l.running[12] != p.lease {
		t.Fatalf("distinct run suppressed: launches %v, running %d, promises %d", launched, l.Running(), l.Acquiring())
	}
}

func TestAmbiguousCompletionNeverAcknowledgesHeldAssignmentsAfterRetries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		shared      bool
		nextMessage bool
	}{
		{name: "standalone retries"},
		{name: "shared admission retries", shared: true},
		{name: "standalone different message", nextMessage: true},
		{name: "shared admission different message", shared: true, nextMessage: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tiers := []config.Tier{tier("work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 3 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			var deliveries, deletes, launches, reconciliations atomic.Int32
			observed := &Statistics{}
			session := &fakeSession{stats: observed,
				onDelete: func(int64) error { deletes.Add(1); return nil },
			}
			l := NewListener(a, tiers[0].Label, session, WithDrainGrace(notDrainingHere), stopsWithoutWaiting(),
				WithRunner(&fakeRunner{onLaunch: func(int64) error { launches.Add(1); return nil }}))
			if err := l.refillEscrowTo(t.Context(), 3); err != nil {
				t.Fatal(err)
			}
			if err := l.acquire(t.Context(), []Job{
				{RequestID: 11, JobID: "J", RunID: 101}, {RequestID: 12, JobID: "J", RunID: 102},
			}); err != nil {
				t.Fatal(err)
			}
			first, second := l.acquiring[11], l.acquiring[12]
			if first == nil || second == nil || l.idleEscrow() != 1 {
				t.Fatal("fixture needs two candidate promises and spare escrow")
			}
			spare := l.Held()[0]
			if tc.shared {
				l.arbiter = newDiscoveryArbiter(tiers)
			}
			checkHeld := func() {
				t.Helper()
				l.mu.Lock()
				intact := l.acquiring[11] == first && l.acquiring[12] == second && len(l.running) == 0
				l.mu.Unlock()
				if !intact || l.observed != observed || launches.Load() != 0 {
					t.Errorf("retry changed held ownership or statistics: intact %v, observed %+v, launches %d",
						intact, l.observed, launches.Load())
				}
				for _, lease := range []*alloc.Lease{first.lease, second.lease, spare} {
					current, err := a.Lease(t.Context(), lease.ID)
					if err != nil || current.Phase != alloc.PhaseCapacity || current.RequestID != 0 {
						t.Errorf("held lease changed before session close: %+v, %v", current, err)
					}
				}
			}
			polls := 0
			session.onGet = func() (*Message, error) {
				checkHeld()
				polls++
				if polls%2 == 0 {
					return nil, ErrNoMessage
				}
				if deliveries.Add(1) > poisonQuarantineAfter {
					return nil, errors.New("ambiguous completion exceeded its retry budget")
				}
				if tc.nextMessage && deliveries.Load() == 2 {
					return &Message{MessageID: 43, Statistics: &Statistics{TotalAssignedJobs: 2}}, nil
				}
				return &Message{MessageID: 42, Statistics: &Statistics{TotalAssignedJobs: 2},
					Assigned: []Job{{RequestID: 11, JobID: "J", RunID: 101},
						{RequestID: 12, JobID: "J", RunID: 102}},
					Completed: []Job{{JobID: "J", Result: "Cancelled"}},
				}, nil
			}
			session.onClose = func(context.Context) error {
				checkHeld()
				return nil
			}
			l.beforePoolReconcile = func() {
				if deliveries.Load() > 0 {
					reconciliations.Add(1)
				}
			}
			wantErr := errQuarantinableCompletion
			wantDeliveries := int32(poisonQuarantineAfter)
			if tc.nextMessage {
				wantErr = ErrUntrustworthySession
				wantDeliveries = 2
			}
			if err := l.Run(t.Context()); !errors.Is(err, wantErr) ||
				tc.nextMessage && errors.Is(err, errQuarantinableCompletion) {
				t.Fatalf("Run = %v, want %v", err, wantErr)
			}
			if deliveries.Load() != wantDeliveries || deletes.Load() != 0 || launches.Load() != 0 ||
				reconciliations.Load() != 0 || l.lastMessageID != 0 || session.closes() != 1 ||
				!slices.Equal(session.acquiredIDs(), []int64{11, 12}) {
				t.Fatalf("held message escaped retry hold: deliveries %d, deletes %d, launches %d, reconciliations %d, cursor %d, closes %d",
					deliveries.Load(), deletes.Load(), launches.Load(), reconciliations.Load(), l.lastMessageID, session.closes())
			}
		})
	}
}

func TestCancellationAfterAHoldDoesNotReconcileDuringDrain(t *testing.T) {
	tiers := []config.Tier{tier("work")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 5 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var launches, deletes, reconciliations atomic.Int32
	var destroyed []int64
	session := &fakeSession{stats: &Statistics{TotalAssignedJobs: 2}}
	l := NewListener(a, tiers[0].Label, session, WithDrainGrace(notDrainingHere),
		WithRunner(&fakeRunner{
			onLaunch: func(int64) error { launches.Add(1); return nil },
			onDestroy: func(id int64) error {
				destroyed = append(destroyed, id)
				return nil
			},
		}))
	if err := l.refillEscrowTo(t.Context(), 5); err != nil {
		t.Fatal(err)
	}
	if err := l.handle(t.Context(), &Message{MessageID: 1,
		Assigned: []Job{{RequestID: 21}, {RequestID: 22}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := l.acquire(t.Context(), []Job{
		{RequestID: 11, JobID: "J", RunID: 101}, {RequestID: 12, JobID: "J", RunID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	first, second := l.acquiring[11], l.acquiring[12]
	if first == nil || second == nil || l.Running() != 2 || l.idleEscrow() != 1 || launches.Load() != 2 {
		t.Fatal("fixture needs two running jobs, two candidate promises and spare escrow")
	}
	checkHeld := func() {
		t.Helper()
		l.mu.Lock()
		intact := l.acquiring[11] == first && l.acquiring[12] == second &&
			len(l.running) == 1 && l.running[21] != nil
		l.mu.Unlock()
		if !intact || launches.Load() != 2 || deletes.Load() != 0 || reconciliations.Load() != 0 ||
			!slices.Equal(destroyed, []int64{22}) || l.lastMessageID != 1 {
			t.Errorf("drain escaped hold: intact %v, launches %d, deletes %d, reconciliations %d, destroyed %v, cursor %d",
				intact, launches.Load(), deletes.Load(), reconciliations.Load(), destroyed, l.lastMessageID)
		}
		for _, p := range []*promise{first, second} {
			lease, err := a.Lease(t.Context(), p.lease.ID)
			if err != nil || lease.Phase != alloc.PhaseCapacity || lease.RequestID != 0 {
				t.Errorf("held promise changed during drain: %+v, %v", lease, err)
			}
		}
	}
	polls, cancellations := 0, 0
	l.beforeEscrowRefill = func() {
		if polls == 1 {
			cancellations++
			cancel()
		}
	}
	l.beforePoolReconcile = func() {
		if polls > 0 {
			reconciliations.Add(1)
		}
	}
	session.onDelete = func(int64) error { deletes.Add(1); return nil }
	session.onGet = func() (*Message, error) {
		polls++
		if polls == 1 {
			return &Message{MessageID: 42,
				Available: []Job{{RequestID: 31, JobID: "J", RunID: 101}},
				Completed: []Job{{JobID: "J", Result: "Cancelled"}, {RequestID: 22, Result: "Succeeded"}},
			}, nil
		}
		if !l.isDraining() || ctx.Err() == nil {
			t.Error("poll did not enter the drain after the cancelled refill")
		}
		checkHeld()
		if polls == 2 {
			return nil, ErrNoMessage
		}
		return nil, errors.New("end drain after the empty poll")
	}
	session.onClose = func(context.Context) error {
		checkHeld()
		return nil
	}
	if err := l.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want cancellation", err)
	}
	if polls != 3 || cancellations != 1 || session.closes() != 1 {
		t.Fatalf("drain path: polls %d, cancellations %d, closes %d",
			polls, cancellations, session.closes())
	}
}

func TestEquivalentOffersCannotHideAnAmbiguousProtocolID(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		name := map[bool]string{false: "positive alias first", true: "positive alias last"}[reverse]
		t.Run(name, func(t *testing.T) {
			tiers := []config.Tier{tier("work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 3 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			var acknowledged bool
			session := &fakeSession{
				onAcquire: func([]int64) ([]int64, error) { return []int64{0}, nil },
				onDelete:  func(int64) error { acknowledged = true; return nil },
			}
			l := NewListener(a, tiers[0].Label, session)
			if err := l.refillEscrowTo(t.Context(), 3); err != nil {
				t.Fatal(err)
			}
			offers := []Job{{RequestID: 11, JobID: "J"}, {JobID: "J"}, {JobID: "K"}}
			if reverse {
				slices.Reverse(offers)
			}
			err := l.handle(t.Context(), &Message{MessageID: 42, Available: offers})
			if !errors.Is(err, ErrUntrustworthySession) ||
				!strings.Contains(err.Error(), "under the same runner request id 0; the acquisition response cannot distinguish them") {
				t.Fatalf("handle = %v, want ambiguous wire request 0", err)
			}
			if len(session.acquiredIDs()) != 0 || acknowledged || l.Acquiring() != 0 ||
				l.Running() != 0 || l.idleEscrow() != 3 || l.lastMessageID != 0 {
				t.Fatalf("ambiguous offers acted: acquired %v, ack %v, promises %d, running %d, escrow %d, cursor %d",
					session.acquiredIDs(), acknowledged, l.Acquiring(), l.Running(), l.idleEscrow(), l.lastMessageID)
			}
		})
	}
}

func TestRequestOnlyEntriesInheritTheCandidateHoldFromPromises(t *testing.T) {
	for _, tc := range []struct {
		name       string
		assignment bool
		statistics *Statistics
	}{
		{name: "assignment without statistics", assignment: true},
		{name: "assignment with deficit", assignment: true, statistics: &Statistics{TotalAssignedJobs: 2}},
		{name: "assignment with zero deficit", assignment: true, statistics: &Statistics{}},
		{name: "completion"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tiers := []config.Tier{tier("work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 3 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			var launched, destroyed []int64
			var acknowledged bool
			session := &fakeSession{onDelete: func(int64) error { acknowledged = true; return nil }}
			l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{
				onLaunch:  func(id int64) error { launched = append(launched, id); return nil },
				onDestroy: func(id int64) error { destroyed = append(destroyed, id); return nil },
			}))
			if err := l.refillEscrowTo(t.Context(), 3); err != nil {
				t.Fatal(err)
			}
			if err := l.acquire(t.Context(), []Job{
				{RequestID: 11, JobID: "J", RunID: 101}, {RequestID: 12, JobID: "J", RunID: 102},
			}); err != nil {
				t.Fatal(err)
			}
			first, second := l.acquiring[11], l.acquiring[12]
			if first == nil || second == nil || l.idleEscrow() != 1 {
				t.Fatal("fixture needs two candidate promises and spare escrow")
			}
			msg := &Message{MessageID: 42, Statistics: tc.statistics,
				Available: []Job{{RequestID: 31, JobID: "J", RunID: 101}, {RequestID: 32, JobID: "J", RunID: 102}},
				Completed: []Job{{JobID: "J", Result: "Cancelled"}},
			}
			if tc.assignment {
				msg.Assigned = []Job{{RequestID: 11}}
			} else {
				msg.Completed = append(msg.Completed, Job{RequestID: 11, Result: "Cancelled"})
			}
			err := l.handle(t.Context(), msg)
			poison, ok := errors.AsType[*poisonedMessageError](err)
			if !ok || !poison.held || !errors.Is(err, errQuarantinableCompletion) {
				t.Fatalf("handle = %v, want held completion ambiguity", err)
			}
			if acknowledged || len(launched) != 0 || len(destroyed) != 0 || l.lastMessageID != 0 ||
				l.acquiring[11] != first || l.acquiring[12] != second || l.Acquiring() != 2 ||
				l.Running() != 0 || l.idleEscrow() != 1 || !slices.Equal(session.acquiredIDs(), []int64{11, 12}) {
				t.Fatalf("held entry acted: ack %v, launches %v, destroys %v, promises %d, running %d, escrow %d, acquisitions %v",
					acknowledged, launched, destroyed, l.Acquiring(), l.Running(), l.idleEscrow(), session.acquiredIDs())
			}
			for _, p := range []*promise{first, second} {
				lease, err := a.Lease(t.Context(), p.lease.ID)
				if err != nil || lease.Phase != alloc.PhaseCapacity || lease.RequestID != 0 {
					t.Fatalf("held promise lease changed: %+v, %v", lease, err)
				}
			}
		})
	}
}

func TestEquivalentOffersReserveAndConsumeOnePromise(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := map[bool]string{false: "one batch", true: "pre-existing equivalent promises"}[existing]
		t.Run(name, func(t *testing.T) {
			tiers := []config.Tier{tier("work")}
			a := newAllocator(t, alloc.Limits{MaxVCPU: 2 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			session := &fakeSession{}
			var launched []int64
			l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{
				onLaunch: func(id int64) error { launched = append(launched, id); return nil },
			}))
			if err := l.refillEscrowTo(t.Context(), 2); err != nil {
				t.Fatal(err)
			}
			offers := []Job{{RequestID: 11, JobID: "J", RunID: 101}, {RequestID: 12, JobID: "J", RunID: 101}}
			leases := l.Held()
			if len(leases) != 2 {
				t.Fatalf("fixture escrow = %d, want 2", len(leases))
			}
			if existing {
				l.mu.Lock()
				for i, job := range offers {
					l.acquiring[job.RequestID] = &promise{job: job, lease: leases[i]}
				}
				l.held = nil
				l.mu.Unlock()
			} else {
				if err := l.handle(t.Context(), &Message{MessageID: 1, Available: offers}); err != nil {
					t.Fatal(err)
				}
				if l.Acquiring() != 1 || l.idleEscrow() != 1 || !slices.Equal(session.acquiredIDs(), []int64{11}) {
					t.Fatalf("equivalent batch reserved twice: promises %d, escrow %d, acquired %v",
						l.Acquiring(), l.idleEscrow(), session.acquiredIDs())
				}
			}
			if err := l.handle(t.Context(), &Message{MessageID: 2, Assigned: []Job{offers[0]}}); err != nil {
				t.Fatalf("equivalent owners stopped the listener: %v", err)
			}
			if l.Acquiring() != 0 || l.Running() != 1 || l.idleEscrow() != 1 ||
				l.running[11] != leases[0] || l.Held()[0] != leases[1] || !slices.Equal(launched, []int64{11}) {
				t.Fatalf("equivalent owners consumed incorrectly: promises %d, running %d, escrow %d, launches %v",
					l.Acquiring(), l.Running(), l.idleEscrow(), launched)
			}
			for i, original := range leases {
				lease, err := a.Lease(t.Context(), original.ID)
				if err != nil {
					t.Fatal(err)
				}
				if i == 0 && (lease.Phase != alloc.PhaseAssigned || lease.RequestID != 11) ||
					i == 1 && (lease.Phase != alloc.PhaseCapacity || lease.RequestID != 0) {
					t.Fatalf("equivalent promise lease %d = %+v", i, lease)
				}
			}
			if err := l.handle(t.Context(), &Message{MessageID: 3, Assigned: []Job{{RequestID: 12, RunID: 101}}}); err != nil {
				t.Fatalf("listener refused the coalesced request alias: %v", err)
			}
			if !slices.Equal(launched, []int64{11}) || l.lastMessageID != 3 || l.idleEscrow() != 1 {
				t.Fatalf("redelivery lost its aliases: launches %v, cursor %d, escrow %d", launched, l.lastMessageID, l.idleEscrow())
			}
		})
	}
}

func TestAResolvedRedeliveryKeepsItsStatisticsForTheNextReconciliation(t *testing.T) {
	tiers := []config.Tier{tier("work")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 3 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
	var acknowledged []int64
	session := &fakeSession{onDelete: func(id int64) error { acknowledged = append(acknowledged, id); return nil }}
	l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{}))
	if err := l.refillEscrowTo(t.Context(), 3); err != nil {
		t.Fatal(err)
	}
	if err := l.acquire(t.Context(), []Job{
		{RequestID: 11, JobID: "J", RunID: 101}, {RequestID: 12, JobID: "J", RunID: 102},
	}); err != nil {
		t.Fatal(err)
	}
	stale := &Statistics{}
	l.observed = stale
	err := l.handle(t.Context(), &Message{MessageID: 42, Statistics: &Statistics{TotalAssignedJobs: 1},
		Completed: []Job{{JobID: "J", Result: "Cancelled"}}})
	if poison, ok := errors.AsType[*poisonedMessageError](err); !ok || !poison.held {
		t.Fatalf("first delivery = %v, want a hold", err)
	}
	if l.observed != stale || !l.messageHeld() {
		t.Fatalf("held delivery: observed %+v, held %v", l.observed, l.messageHeld())
	}

	l.unreserve([]int64{12})
	current := &Statistics{TotalAssignedJobs: 1}
	if err := l.handle(t.Context(), &Message{MessageID: 42, Statistics: current,
		Completed: []Job{{JobID: "J", Result: "Cancelled"}}}); err != nil {
		t.Fatalf("redelivery that no longer holds: %v", err)
	}
	if l.observed != current || l.messageHeld() || !slices.Equal(acknowledged, []int64{42}) || l.lastMessageID != 42 {
		t.Fatalf("resolved redelivery: observed %+v, held %v, acknowledged %v, cursor %d",
			l.observed, l.messageHeld(), acknowledged, l.lastMessageID)
	}
}
