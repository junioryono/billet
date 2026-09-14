package server

import (
	"errors"
	"slices"
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
			a, listeners := arbitrationListeners(t, []config.Tier{tier("work")}, tierVCPU)
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
			job := Job{RequestID: 11, JobID: "J", RunID: 101}
			if err := l.handle(t.Context(), &Message{MessageID: 1, Assigned: []Job{job}}); err != nil {
				t.Fatal(err)
			}
			lease := l.running[11]
			if lease == nil {
				t.Fatal("fixture did not launch request 11")
			}
			if err := l.handle(t.Context(), &Message{MessageID: 2, Available: []Job{job},
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
			if !slices.Equal(session.acquiredIDs(), []int64{11}) || l.Running() != 0 {
				t.Fatalf("unfinished J: acquired %v, running %d", session.acquiredIDs(), l.Running())
			}
			if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
				t.Fatalf("completed lease still open: %v", err)
			}
			if outcome == "offer refused" {
				if l.Acquiring() != 0 || len(l.waitingOffers) != 1 || l.waitingOffers[0].job != "J" || l.waitingOffers[0].run != 101 {
					t.Fatalf("refused J lost demand: promises %d, waiting %+v", l.Acquiring(), l.waitingOffers)
				}
			} else if p := l.acquiring[11]; p == nil || p.lease.ID == lease.ID || len(l.waitingOffers) != 0 {
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
			a := newAllocator(t, alloc.Limits{MaxVCPU: 2 * tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
			var launched, destroyed []int64
			var acknowledged bool
			l := NewListener(a, tiers[0].Label, &fakeSession{onDelete: func(int64) error {
				acknowledged = true
				return nil
			}}, WithRunner(&fakeRunner{
				onLaunch:  func(id int64) error { launched = append(launched, id); return nil },
				onDestroy: func(id int64) error { destroyed = append(destroyed, id); return nil },
			}))
			if err := l.refillEscrowTo(t.Context(), 2); err != nil {
				t.Fatal(err)
			}
			second := 23 - first
			err := l.handle(t.Context(), &Message{MessageID: 1,
				Assigned: []Job{{RequestID: first, JobID: "J", RunID: first + 90},
					{RequestID: second, JobID: "J", RunID: second + 90}},
				Completed: []Job{{RequestID: requestID, JobID: "J", Result: "Cancelled"}},
			})
			if _, ok := errors.AsType[*poisonedMessageError](err); !ok || !errors.Is(err, errQuarantinableCompletion) {
				t.Fatalf("request %d, first %d: handle = %v, want quarantinable poison", requestID, first, err)
			}
			if len(destroyed) != 0 || acknowledged || !slices.Equal(launched, []int64{first, second}) {
				t.Fatalf("ambiguous completion acted: destroys %v, ack %v, launches %v", destroyed, acknowledged, launched)
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
