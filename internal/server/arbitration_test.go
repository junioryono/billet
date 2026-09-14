package server

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

func arbitrationListeners(t *testing.T, tiers []config.Tier, vcpu int) (*alloc.Allocator, []*Listener) {
	t.Helper()
	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: vcpu, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}
	registerHost(t, a)
	s := New(a, nil, tiers, "arbitration-test", nil)
	listeners := make([]*Listener, 0, len(tiers))
	for _, tr := range tiers {
		listeners = append(listeners, NewListener(a, tr.Label, &fakeSession{}, s.listenerOpts(nil)...))
	}

	return a, listeners
}

// WITHDRAWAL RETAINS BACKING THROUGH BOTH POLLS. The recipient reaches the real
// escrow path inside each exchange, so releasing before the lower response would
// give it a lease and fail here while the donor still owes its old advertisement.
func TestIdleDiscoveryYieldsOnlyAfterTheLowerExchange(t *testing.T) {
	for _, assignmentPoll := range []int{0, 1, 2} {
		t.Run([]string{"empty withdrawal", "outstanding poll assignment", "lower poll assignment"}[assignmentPoll], func(t *testing.T) {
			a, listeners := arbitrationListeners(t, []config.Tier{tier("a-idle"), tier("b-work")}, tierVCPU)
			donor, recipient := listeners[0], listeners[1]
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			session := &fakeSession{}
			donor.session = session
			donor.runner = &fakeRunner{}
			stopsWithoutWaiting()(donor)
			var advertised []int
			var held []int
			polls := 0
			session.onPoll = func(capacity int) {
				polls++
				advertised = append(advertised, capacity)
				held = append(held, donor.idleEscrow())
				if polls == 1 {
					report, err := a.CapacityReport(ctx, donor.tier)
					if err != nil {
						t.Error(err)
					} else if report.Discovery != 1 || report.Listener.Sent == nil ||
						*report.Listener.Sent != 1 || report.Listener.Exchange != "in flight" {
						t.Errorf("the running listener did not publish its discovery hold: %+v", report)
					}
					recipient.observed = &Statistics{TotalAssignedJobs: 1}
				}
				if err := recipient.prepareEscrow(ctx); err != nil {
					t.Error(err)
				}
				if polls <= 2 && recipient.capacity() != 0 {
					t.Errorf("recipient took %d leases before withdrawal completed", recipient.capacity())
				}
				if polls == 3 {
					want := 1
					if assignmentPoll != 0 {
						want = 0
						if donor.Running() != 1 {
							t.Errorf("old assignment has %d runners, want 1", donor.Running())
						}
					}
					if recipient.capacity() != want {
						t.Errorf("recipient after withdrawal = %d, want %d", recipient.capacity(), want)
					}
					usage, err := a.Usage(ctx)
					if err != nil {
						t.Error(err)
					} else if usage.VCPU != tierVCPU || usage.Leases != 1 {
						t.Errorf("charged after exchange = %+v, want one %d-vcpu lease", usage, tierVCPU)
					}
					cancel()
				}
			}
			session.onGet = func() (*Message, error) {
				if polls == assignmentPoll {
					return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
				}
				return nil, ErrNoMessage
			}
			if err := donor.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			wantSent, wantHeld := []int{1, 0}, []int{1, 1}
			if assignmentPoll == 1 {
				wantSent, wantHeld = []int{1, 1}, []int{1, 0}
			}
			if len(advertised) < 3 || !slices.Equal(advertised[:2], wantSent) ||
				!slices.Equal(held[:2], wantHeld) {
				t.Fatalf("withdrawal sent %v with held %v, want first polls %v with held %v",
					advertised, held, wantSent, wantHeld)
			}
		})
	}
}

// AN AMBIGUOUS LOWER EXCHANGE RELEASES NOTHING. A failed close leaves the same
// charge standing, so teardown cannot hide an arbitration release from this test.
func TestFailedDiscoveryWithdrawalRetainsItsCharge(t *testing.T) {
	for _, failure := range []error{errors.New("connection lost"), ErrUntrustworthySession} {
		t.Run(failure.Error(), func(t *testing.T) {
			a, listeners := arbitrationListeners(t, []config.Tier{tier("a-idle"), tier("b-work")}, tierVCPU)
			donor, recipient := listeners[0], listeners[1]
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			polls, releases := 0, 0
			donor.releaseCapacity = func(ctx context.Context, id string, epoch int64, phase alloc.Phase) error {
				releases++
				return a.Release(ctx, id, epoch, phase)
			}
			donor.session = &fakeSession{
				onPoll: func(capacity int) {
					polls++
					recipient.observed = &Statistics{TotalAssignedJobs: 1}
					if err := recipient.prepareEscrow(ctx); err != nil {
						t.Error(err)
					}
					if polls == 2 && (capacity != 0 || donor.idleEscrow() != 1) {
						t.Errorf("lower poll sent %d with %d held", capacity, donor.idleEscrow())
					}
				},
				onGet: func() (*Message, error) {
					if polls == 1 {
						return nil, ErrNoMessage
					}
					return nil, failure
				},
				onClose: func(context.Context) error { return errors.New("close unconfirmed") },
			}
			if err := donor.Run(ctx); !errors.Is(err, failure) {
				t.Fatalf("Run = %v, want %v", err, failure)
			}
			usage, err := a.Usage(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if polls != 2 || releases != 0 || usage.Leases != 1 || recipient.capacity() != 0 {
				t.Fatalf("polls %d, releases %d, usage %+v, recipient %d", polls, releases, usage, recipient.capacity())
			}
		})
	}
}

// A YIELDED TIER REJOINS DISCOVERY AFTER ITS PEER'S TURN. Asking repeatedly as
// the donor cannot retake the released slot before that peer's backed exchange.
func TestDiscoveryRotatesAndTheDonorCannotRetakeItsTurn(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("a-idle"), tier("b-idle")}, tierVCPU)
	first, second := listeners[0], listeners[1]
	if err := first.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, firstTurn := first.admissionPoll()
	first.finishAdmissionTurn(firstTurn)
	first.releaseIdleEscrowAbove(t.Context(), first.targetCapacity())
	for range 3 {
		if err := first.prepareEscrow(t.Context()); err != nil {
			t.Fatal(err)
		}
		if first.capacity() != 0 {
			t.Fatal("donor retook capacity before its peer's turn")
		}
	}
	if err := second.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second.capacity() != 1 {
		t.Fatalf("peer holds %d, want 1", second.capacity())
	}
	_, secondTurn := second.admissionPoll()
	second.finishAdmissionTurn(secondTurn)
	second.releaseIdleEscrowAbove(t.Context(), second.targetCapacity())
	if err := first.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if first.capacity() != 1 {
		t.Fatalf("yielded tier holds %d on its next turn, want 1", first.capacity())
	}
}

// A LARGE WINNER ACCUMULATES HEADROOM. A small stream repeatedly asking after
// each completion cannot consume any fragment until the large lease is backed.
func TestLargeDemandKeepsItsTurnWhileSmallJobsFinish(t *testing.T) {
	small, large := tier("a-small"), tier("b-large")
	small.VCPU, large.VCPU = 1, 4
	a, listeners := arbitrationListeners(t, []config.Tier{small, large}, 4)
	s, big := listeners[0], listeners[1]
	leases, err := a.Escrow(t.Context(), small.Label, 4)
	if err != nil || len(leases) != 4 {
		t.Fatalf("fixture escrow = %v, %v", leases, err)
	}
	for i, lease := range leases {
		if err := a.Bind(t.Context(), lease.ID, lease.Epoch, lease.TargetNode); err != nil {
			t.Fatal(err)
		}
		if err := a.Assign(t.Context(), lease.ID, lease.Epoch, 101, int64(i+1)); err != nil {
			t.Fatal(err)
		}
		if err := a.Advance(t.Context(), lease.ID, lease.Epoch, alloc.PhaseLaunching); err != nil {
			t.Fatal(err)
		}
	}
	big.observed = &Statistics{TotalAssignedJobs: 1}
	if err := big.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.observed = &Statistics{TotalAssignedJobs: 100}
	for i, lease := range leases {
		if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			t.Fatal(err)
		}
		if err := s.prepareEscrow(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s.capacity() != 0 {
			t.Fatalf("small stream took fragment %d before the large shape fit", i+1)
		}
		if err := big.prepareEscrow(t.Context()); err != nil {
			t.Fatal(err)
		}
		want := 0
		if i == 3 {
			want = 1
		}
		if big.capacity() != want {
			t.Fatalf("large holds %d after fragment %d, want %d", big.capacity(), i+1, want)
		}
	}
}

// ACQUIRING IS AN OBLIGATION EVEN IN CAPACITY PHASE. Arbitration may release a
// genuinely held sibling but never the lease reserve already promised to GitHub.
func TestDiscoveryWithdrawalDoesNotReleaseAnAcquiringCapacityLease(t *testing.T) {
	a, listeners := arbitrationListeners(t, []config.Tier{tier("a-donor"), tier("b-work")}, 3*tierVCPU)
	donor, recipient := listeners[0], listeners[1]
	if err := donor.refillEscrowUngated(t.Context(), 2); err != nil {
		t.Fatal(err)
	}
	if got := donor.reserve([]Job{{RequestID: 11}}); !slices.Equal(got, []int64{11}) {
		t.Fatalf("reserved %v, want request 11", got)
	}
	promised := donor.acquiring[11].lease
	recipient.observed = &Statistics{TotalAssignedJobs: 1}
	if err := recipient.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	donor.releaseIdleEscrowAbove(t.Context(), donor.targetCapacity())
	if donor.idleEscrow() != 0 || len(donor.acquiring) != 1 {
		t.Fatalf("held %d, acquiring %d", donor.idleEscrow(), len(donor.acquiring))
	}
	if err := a.Heartbeat(t.Context(), promised.ID, promised.Epoch); err != nil {
		t.Fatalf("arbitration released the acquiring capacity lease: %v", err)
	}
}

// THE TARGET BOUNDARY DOES NOT SPLIT THE CAPACITY QUEUE. Reversing catalogue
// construction and asking the repository tier first cannot win startup escrow.
func TestBothTargetsShareTheSameDeterministicAdmissionOrder(t *testing.T) {
	org, repo, tiers := twoTargets()
	slices.Reverse(tiers)
	a := newAllocator(t, alloc.Limits{MaxVCPU: tierVCPU, MaxMemory: 64 * config.GiB}, tiers)
	s := New(a, nil, tiers, "two-targets", nil, WithTargets(org, repo))
	orgListener := NewListener(a, "billet-4vcpu-a", &fakeSession{}, s.listenerOpts(org.Provisioner)...)
	repoListener := NewListener(a, "billet-4vcpu-b", &fakeSession{}, s.listenerOpts(repo.Provisioner)...)
	if err := repoListener.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := orgListener.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if repoListener.capacity() != 0 || orgListener.capacity() != 1 {
		t.Fatalf("startup gave org %d, repo %d; want sorted order 1, 0",
			orgListener.capacity(), repoListener.capacity())
	}
	repoListener.observed = &Statistics{TotalAssignedJobs: 1}
	if err := repoListener.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if orgListener.targetCapacity() != 0 || repoListener.targetCapacity() != 1 {
		t.Fatalf("cross-target demand did not request withdrawal: org %d, repo %d",
			orgListener.targetCapacity(), repoListener.targetCapacity())
	}
}

// REACHING A PER-TIER CEILING DURING HANDLING STILL SPENDS THAT TIER'S TURN.
// Losing the cursor update here lets two fast one-job tiers bypass a third.
func TestCappedDemandAdvancesTheWholeRoundRobin(t *testing.T) {
	tiers := []config.Tier{tier("a"), tier("b"), tier("c")}
	for i := range tiers {
		tiers[i].MaxConcurrent = 1
	}
	_, listeners := arbitrationListeners(t, tiers, 3*tierVCPU)
	for _, l := range listeners {
		l.observed = &Statistics{TotalAssignedJobs: 1}
		l.observeDemand(l.observed)
	}
	for i, l := range listeners {
		if err := l.prepareEscrow(t.Context()); err != nil {
			t.Fatal(err)
		}
		advertised, turn := l.admissionPoll()
		if advertised != 1 || turn == 0 {
			t.Fatalf("tier %s has advertisement %d, turn %d; want one backed turn", l.tier, advertised, turn)
		}
		id := int64(i + 1)
		if got := l.reserve([]Job{{RequestID: id}}); !slices.Equal(got, []int64{id}) {
			t.Fatalf("tier %s reserved %v, want %d", l.tier, got, id)
		}
		l.observeDemand(l.observed)
		l.finishAdmissionTurn(turn)
		if i < len(listeners)-1 && !l.arbiter.permits(listeners[i+1].tier) {
			t.Fatalf("tier %s did not pass its served turn to %s", l.tier, listeners[i+1].tier)
		}
	}
}

// A REVOKED TURN CANNOT MAKE A NEW PROMISE FROM OLD BACKING. The acquisition
// boundary must enforce this even when a handler already passed its offer guard.
func TestDiscoveryDonorCannotAcquireAfterItsTurnWasRevoked(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("a-idle"), tier("b-work")}, tierVCPU)
	donor, recipient := listeners[0], listeners[1]
	session := &fakeSession{}
	donor.session = session
	if err := donor.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	recipient.observed = &Statistics{TotalAssignedJobs: 1}
	if err := recipient.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := donor.acquire(t.Context(), []Job{{RequestID: 11, RunID: 101}}); err != nil {
		t.Fatal(err)
	}
	if got := session.acquiredIDs(); len(got) != 0 {
		t.Fatalf("revoked donor asked GitHub to acquire %v", got)
	}
	if donor.idleEscrow() != 1 || len(donor.acquiring) != 0 {
		t.Fatalf("revoked donor has held %d, pending %d; want 1 and 0",
			donor.idleEscrow(), len(donor.acquiring))
	}
}

// A REFUSED OFFER REMAINS KNOWN WORK WITHOUT ANOTHER STATISTICS MESSAGE. Once
// accepted it must stop outranking discovery merely because its old offer exists.
func TestAnOfferWithoutStatisticsKeepsItsPlaceUntilAccepted(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("a-work"), tier("b-work")}, 2*tierVCPU)
	first, second := listeners[0], listeners[1]
	first.observed = &Statistics{TotalAssignedJobs: 1}
	if err := first.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	offer := &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}
	if err := second.handle(t.Context(), offer); err != nil {
		t.Fatal(err)
	}
	if second.capacity() != 0 || len(second.waitingOffers) != 1 {
		t.Fatalf("refused offer has capacity %d, waiting %d; want 0 and 1",
			second.capacity(), len(second.waitingOffers))
	}
	_, turn := first.admissionPoll()
	first.finishAdmissionTurn(turn)
	first.releaseIdleEscrowAbove(t.Context(), first.targetCapacity())
	if err := second.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second.capacity() != 1 || !second.arbiter.permits(second.tier) {
		t.Fatal("refused offer lost its known-work turn without fresh statistics")
	}
	if err := second.handle(t.Context(), offer); err != nil {
		t.Fatal(err)
	}
	if second.committedCapacity() != 1 || len(second.waitingOffers) != 0 {
		t.Fatalf("accepted offer has commitments %d, waiting %d; want 1 and 0",
			second.committedCapacity(), len(second.waitingOffers))
	}
}

// ACCEPTANCE SPENDS AN AVAILABLE SNAPSHOT. A stale aggregate cannot keep its
// tier ahead of discovery after every job in that observation has been acquired.
func TestAcceptedWorkDoesNotKeepAStaleAvailablePriority(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("a-work"), tier("b-idle")}, 2*tierVCPU)
	work, idle := listeners[0], listeners[1]
	work.observed = &Statistics{TotalAvailableJobs: 1}
	if err := work.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, turn := work.admissionPoll()
	if err := work.handle(t.Context(), &Message{
		MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}},
	}); err != nil {
		t.Fatal(err)
	}
	work.finishAdmissionTurn(turn)
	if err := idle.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if idle.capacity() != 1 || work.committedCapacity() != 1 {
		t.Fatalf("stale available statistics kept idle discovery out: idle %d, committed %d",
			idle.capacity(), work.committedCapacity())
	}
}

// KNOWN CONTENDERS ARE SORTED BEFORE A TURN FIRST TRIES TO BUY CAPACITY.
// Reversing both declaration and observation cannot give the last label first use.
func TestKnownDemandOrderDoesNotFollowObservationArrival(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("c"), tier("b"), tier("a")}, tierVCPU)
	for _, l := range listeners {
		l.observed = &Statistics{TotalAssignedJobs: 1}
		l.observeDemand(l.observed)
	}
	for _, l := range listeners {
		if err := l.prepareEscrow(t.Context()); err != nil {
			t.Fatal(err)
		}
		want := 0
		if l.tier == "a" {
			want = 1
		}
		if l.capacity() != want {
			t.Fatalf("tier %s took %d leases, want %d despite reversed arrival", l.tier, l.capacity(), want)
		}
	}
}

// REDELIVERY CANNOT TURN AN EXISTING PROMISE INTO UNMET DEMAND. The spare slot
// must go to the idle peer even if GitHub repeats the already-acquired offer.
func TestARepeatedOfferDoesNotPreemptDiscovery(t *testing.T) {
	for _, tc := range []struct {
		name       string
		requestID  int64
		repeatedID int64
	}{
		{name: "request id", requestID: 11, repeatedID: 11},
		{name: "zero request id", requestID: 0},
		{name: "direct promise with positive offer", requestID: 0, repeatedID: 11},
		{name: "positive promise with zero offer", requestID: 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, listeners := arbitrationListeners(t, []config.Tier{tier("a-work"), tier("b-idle")}, 2*tierVCPU)
			work, idle := listeners[0], listeners[1]
			session := &fakeSession{}
			work.session = session
			work.observed = &Statistics{TotalAvailableJobs: 1}
			if err := work.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, turn := work.admissionPoll()
			offer := &Message{MessageID: 1, Available: []Job{{RequestID: tc.requestID, RunID: 101, JobID: "job-a"}}}
			if err := work.handle(t.Context(), offer); err != nil {
				t.Fatal(err)
			}
			if ids := session.acquiredIDs(); !slices.Equal(ids, []int64{tc.requestID}) || work.Acquiring() != 1 {
				t.Fatalf("initial acquisition = %v, promises %d; want [%d], 1", ids, work.Acquiring(), tc.requestID)
			}
			work.finishAdmissionTurn(turn)
			if err := idle.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			sent, idleTurn := idle.admissionPoll()
			if sent != 1 || idleTurn == 0 {
				t.Fatalf("peer discovery sent %d with turn %d; want one backed turn", sent, idleTurn)
			}
			offer.Available[0].RequestID = tc.repeatedID
			if err := work.handle(t.Context(), offer); err != nil {
				t.Fatal(err)
			}
			if sent, turn := idle.admissionPoll(); sent != 1 || turn != idleTurn {
				t.Fatalf("redelivery revoked peer discovery: sent %d, turn %d; want 1, %d", sent, turn, idleTurn)
			}
			if ids := session.acquiredIDs(); !slices.Equal(ids, []int64{tc.requestID}) {
				t.Fatalf("redelivery acquired the same job again: %v", ids)
			}
			if idle.capacity() != 1 || work.capacity() != 1 || len(work.waitingOffers) != 0 {
				t.Fatalf("repeated offer left idle %d, work %d, waiting %d; want 1, 1, 0",
					idle.capacity(), work.capacity(), len(work.waitingOffers))
			}
		})
	}
}

// A LOST ACQUISITION RESPONSE CANNOT MAKE ITS LEASE A DONATION CANDIDATE.
// Cancellation can enter the drain instead of ending Run immediately, so the
// acquiring set must retain the obligation through a later lower advertisement.
func TestAmbiguousAcquisitionCannotBeDonated(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "lost response", true: "unrequested response"}[malformed], func(t *testing.T) {
			a, listeners := arbitrationListeners(t, []config.Tier{tier("a-donor"), tier("b-work")}, tierVCPU)
			donor, recipient := listeners[0], listeners[1]
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
			if err := donor.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if err := donor.acquire(t.Context(), []Job{{RequestID: 11, RunID: 101}}); !errors.Is(err, want) {
				t.Fatalf("acquire = %v, want %v", err, want)
			}
			recipient.observed = &Statistics{TotalAssignedJobs: 1}
			if err := recipient.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			donor.releaseIdleEscrowAbove(t.Context(), donor.targetCapacity())
			if donor.idleEscrow() != 0 || len(donor.acquiring) != 1 {
				t.Fatalf("ambiguous acquisition has held %d, promises %d; want 0, 1",
					donor.idleEscrow(), len(donor.acquiring))
			}
			if err := a.Heartbeat(t.Context(), donor.acquiring[11].lease.ID, donor.acquiring[11].lease.Epoch); err != nil {
				t.Fatalf("ambiguous acquisition lost its charge: %v", err)
			}
			if err := recipient.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			if recipient.capacity() != 0 {
				t.Fatal("recipient acquired capacity still owed by the ambiguous request")
			}
		})
	}
}

// COMPLETING ONE DIRECT JOB DOES NOT ERASE ANOTHER ZERO-REQUEST OFFER.
// GitHub's stable job identity, not the shared zero, distinguishes their demand.
func TestDirectOfferDemandKeepsSeparateJobIdentities(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("a-work"), tier("b-work")}, tierVCPU)
	first, second := listeners[0], listeners[1]
	first.observed = &Statistics{TotalAssignedJobs: 1}
	if err := first.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := second.handle(t.Context(), &Message{MessageID: 1, Available: []Job{{JobID: "job-a"}, {JobID: "job-b"}}}); err != nil {
		t.Fatal(err)
	}
	if err := second.handle(t.Context(), &Message{MessageID: 2, Completed: []Job{{JobID: "job-a"}}}); err != nil {
		t.Fatal(err)
	}
	second.observeDemand(nil)
	_, turn := first.admissionPoll()
	first.finishAdmissionTurn(turn)
	if len(second.waitingOffers) != 1 || !containsActual(second.waitingOffers, actualJobIdentity{job: "job-b"}) ||
		!second.arbiter.permits(second.tier) {
		t.Fatalf("completion erased unrelated demand: %+v", second.waitingOffers)
	}
}

// AN OFFER CANCELLED IN ITS OWN BATCH MUST NOT BE ACQUIRED AGAIN. The peer's
// next turn proves that no impossible pending assignment keeps the slot charged.
func TestCancelledOfferDoesNotCreateAPromise(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("a-work"), tier("b-idle")}, tierVCPU)
	work, idle := listeners[0], listeners[1]
	session := &fakeSession{}
	work.session = session
	if err := work.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, turn := work.admissionPoll()
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
	work.finishAdmissionTurn(turn)
	work.releaseIdleEscrowAbove(t.Context(), work.targetCapacity())
	if err := idle.prepareEscrow(t.Context()); err != nil {
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

// AN UNUSED LABEL MUST NOT TRIGGER CLOUD FALLBACK. Even with deployment room
// for both placements, the recipient waits for the donor's lower exchange and
// then buys the preferred local slot that exchange released.
func TestDiscoveryWithdrawalPrecedesCloudFallback(t *testing.T) {
	idleTier, workTier := tier("a-idle"), tier("b-work")
	workTier.Provider = ""
	workTier.Providers = []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2}
	tiers := []config.Tier{idleTier, workTier}
	a := newBareAllocator(t, alloc.Limits{MaxVCPU: 2 * tierVCPU, MaxMemory: 16 * config.GiB}, tiers)
	for _, node := range []alloc.NodeRegistration{
		{Name: "local", Provider: config.ProviderFirecracker, VCPU: tierVCPU, Memory: 4 * config.GiB},
		{Name: "cloud", Provider: config.ProviderEC2, VCPU: tierVCPU, Memory: 4 * config.GiB,
			EC2Shapes: []config.RemoteShape{{Type: "c7i.xlarge", VCPU: tierVCPU,
				Memory: 4 * config.GiB, PriceUSDPerHour: 170000}}},
	} {
		if _, err := a.RegisterNode(t.Context(), node); err != nil {
			t.Fatal(err)
		}
	}
	s := New(a, nil, tiers, "local-before-cloud", nil)
	donor := NewListener(a, idleTier.Label, &fakeSession{}, s.listenerOpts(nil)...)
	recipient := NewListener(a, workTier.Label, &fakeSession{}, s.listenerOpts(nil)...)
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	polls := 0
	donor.session = &fakeSession{onPoll: func(sent int) {
		polls++
		recipient.observed = &Statistics{TotalAssignedJobs: 1}
		if err := recipient.prepareEscrow(ctx); err != nil {
			t.Error(err)
		}
		if polls <= 2 && recipient.capacity() != 0 {
			t.Error("recipient bought cloud capacity before the local withdrawal completed")
		}
		if polls == 2 && (sent != 0 || donor.idleEscrow() != 1) {
			t.Errorf("lower exchange sent %d with %d held; want 0, 1", sent, donor.idleEscrow())
		}
		if polls == 3 {
			held := recipient.Held()
			if len(held) != 1 || held[0].TargetNode != "local" {
				t.Errorf("recipient did not take the returned local placement: %+v", held)
			}
			cancel()
		}
	}}
	if err := donor.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if polls != 3 {
		t.Fatalf("observed %d polls, want the outstanding, lower and post-withdrawal polls", polls)
	}
}
