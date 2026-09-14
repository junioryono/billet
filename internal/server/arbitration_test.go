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
	for i := range tiers {
		listeners = append(listeners, NewListener(a, tiers[i].Label, &fakeSession{}, s.listenerOpts(nil)...))
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
	if err := donor.refillEscrowUngated(t.Context(), 2, 2); err != nil {
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
	_, listeners := arbitrationListeners(t, []config.Tier{tier("a-work"), tier("b-idle")}, 2*tierVCPU)
	work, idle := listeners[0], listeners[1]
	work.observed = &Statistics{TotalAvailableJobs: 1}
	if err := work.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, turn := work.admissionPoll()
	offer := &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}
	if err := work.handle(t.Context(), offer); err != nil {
		t.Fatal(err)
	}
	work.finishAdmissionTurn(turn)
	if err := work.handle(t.Context(), offer); err != nil {
		t.Fatal(err)
	}
	if err := idle.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if idle.capacity() != 1 || work.capacity() != 1 || len(work.waitingOffers) != 0 {
		t.Fatalf("repeated offer left idle %d, work %d, waiting %d; want 1, 1, 0",
			idle.capacity(), work.capacity(), len(work.waitingOffers))
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
	second.rememberAvailable(&Message{Available: []Job{{JobID: "job-a"}, {JobID: "job-b"}}})
	second.rememberAvailable(&Message{Completed: []Job{{JobID: "job-a"}}})
	second.observeDemand(nil)
	_, turn := first.admissionPoll()
	first.finishAdmissionTurn(turn)
	if len(second.waitingOffers) != 1 || !second.waitingOffers[offerIdentity{job: "job-b"}] ||
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

// A REFUSED POOL LAUNCH SPENDS ITS TURN BEFORE THE NEXT OFFER ARRIVES. The
// last assigned count may still request that runner, but each failed attempt
// must leave the next exchange able to acquire from its own backed turn.
func TestARefusedPoolLaunchReturnsItsTurnBeforeTheNextOffer(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("work")}, tierVCPU)
	l := listeners[0]
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	launches, polls := 0, 0
	var spent uint64
	l.runner = &fakeRunner{onLaunch: func(int64) error {
		launches++
		_, spent = l.admissionPoll()
		return errors.New("pool trust refused before launch")
	}}
	session := &fakeSession{stats: &Statistics{TotalAssignedJobs: 1}}
	l.session = session
	session.onPoll = func(sent int) {
		polls++
		if polls == 1 {
			_, next := l.admissionPoll()
			if spent == 0 || next == 0 || next == spent {
				t.Errorf("refused turn %d left next poll on turn %d", spent, next)
			}
			l.finishAdmissionTurn(spent)
			_, afterDuplicate := l.admissionPoll()
			if afterDuplicate != next {
				t.Errorf("duplicate return changed this tier's next turn %d to %d", next, afterDuplicate)
			}
			if launches != 1 || sent != 1 || l.idleEscrow() != 1 {
				t.Errorf("after refusal: launches %d, sent %d, held %d; want 1, 1, 1",
					launches, sent, l.idleEscrow())
			}
		} else {
			cancel()
		}
	}
	session.onGet = func() (*Message, error) {
		return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}},
			Statistics: &Statistics{TotalAvailableJobs: 1}}, nil
	}
	if err := l.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want cancellation after the offer", err)
	}
	if got := session.acquiredIDs(); !slices.Equal(got, []int64{11}) {
		t.Fatalf("acquired %v after a refused pool launch, want request 11", got)
	}
	if polls != 2 || launches != 1 {
		t.Fatalf("polls %d, launches %d; want 2, 1", polls, launches)
	}
}

// QUARANTINING A POISONED COMPLETION RETURNS ITS GRANTED WORK TURN. Otherwise
// repeated poison keeps the grant forever and starves every waiting tier.
func TestQuarantiningAPoisonedCompletionReturnsItsGrantedWorkTurn(t *testing.T) {
	_, listeners := arbitrationListeners(t, []config.Tier{tier("a"), tier("b")}, tierVCPU)
	first, second := listeners[0], listeners[1]
	for _, l := range listeners {
		l.observed = &Statistics{TotalAvailableJobs: 1}
		l.observeDemand(l.observed)
	}
	for _, l := range listeners {
		if err := l.prepareEscrow(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	advertised, turn := first.admissionPoll()
	arbiter := first.arbiter
	arbiter.mu.Lock()
	work := arbiter.work
	arbiter.mu.Unlock()
	if advertised != 1 || turn == 0 || !work || second.capacity() != 0 {
		t.Fatalf("fixture has advertisement %d, turn %d, work %t, peer capacity %d; want A's granted work turn with B waiting",
			advertised, turn, work, second.capacity())
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	deliveries := 0
	var acked []int64
	checked := false
	session := &fakeSession{stats: first.observed}
	first.session = session
	session.onPoll = func(int) {
		if deliveries < poisonQuarantineAfter {
			_, current := first.admissionPoll()
			if current != turn {
				t.Errorf("before poison delivery %d, granted turn %d became %d",
					deliveries+1, turn, current)
			}
			return
		}
		// Observe before cancellation or an empty exchange can return the turn.
		arbiter.mu.Lock()
		owner, generation, granted := arbiter.owner, arbiter.generation, arbiter.granted
		arbiter.mu.Unlock()
		if owner != second.tier || generation <= turn || granted {
			t.Errorf("quarantine left owner %q, generation %d, granted %t; want B's ungranted turn after A's generation %d",
				owner, generation, granted, turn)
		}
		checked = true
		cancel()
	}
	session.onGet = func() (*Message, error) {
		deliveries++
		return &Message{MessageID: 42, Completed: []Job{{
			RunnerName: "not-a-billet-runner", Result: "succeeded",
		}}}, nil
	}
	session.onDelete = func(id int64) error {
		acked = append(acked, id)
		if deliveries != poisonQuarantineAfter {
			t.Errorf("acknowledged message %d after %d deliveries, want %d",
				id, deliveries, poisonQuarantineAfter)
		}
		return nil
	}
	if err := first.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want cancellation after quarantine", err)
	}
	if !checked || deliveries != poisonQuarantineAfter || !slices.Equal(acked, []int64{42}) || first.lastMessageID != 42 {
		t.Fatalf("checked %t, deliveries %d, acknowledgements %v, cursor %d; want one quarantine after %d deliveries",
			checked, deliveries, acked, first.lastMessageID, poisonQuarantineAfter)
	}
}

// RETURNING A CONSUMED TURN DOES NOT RELEASE ITS LEASE OR SPEND ITS SUCCESSOR.
// Running compute and custody remain charged; only a conclusive refusal frees
// the old placement. Every outcome gives the next known contender its turn.
func TestConsumedPoolTurnsAdvanceOnceAndKeepTheirComputeCharged(t *testing.T) {
	for _, outcome := range []struct {
		name string
		err  error
		kept bool
	}{
		{name: "launched", kept: true},
		{name: "refused", err: errors.New("refused before launch")},
		{name: "custody", err: ErrCustody, kept: true},
	} {
		t.Run(outcome.name, func(t *testing.T) {
			a, listeners := arbitrationListeners(t, []config.Tier{tier("a"), tier("b")}, 2*tierVCPU)
			first, second := listeners[0], listeners[1]
			for _, l := range listeners {
				l.observed = &Statistics{TotalAssignedJobs: 1}
				l.observeDemand(l.observed)
			}
			first.runner = &fakeRunner{onLaunch: func(int64) error { return outcome.err }}
			if err := first.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			held := first.Held()
			if len(held) != 1 {
				t.Fatalf("first turn has %d held leases, want 1", len(held))
			}
			lease := held[0]
			_, spent := first.admissionPoll()
			if err := first.reconcileAdmissionPool(t.Context(), 1); err != nil {
				t.Fatal(err)
			}
			if first.idleEscrow() != 0 || !second.arbiter.permits(second.tier) {
				t.Fatal("consumed turn did not yield to the next known contender")
			}
			if err := second.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, next := second.admissionPoll()
			if next == 0 || next == spent || second.idleEscrow() != 1 {
				t.Fatalf("successor has turn %d, held %d; spent turn was %d",
					next, second.idleEscrow(), spent)
			}
			first.finishAdmissionTurn(spent)
			_, afterDuplicate := second.admissionPoll()
			if afterDuplicate != next {
				t.Fatalf("duplicate return changed successor turn %d to %d", next, afterDuplicate)
			}
			_, err := a.Lease(t.Context(), lease.ID)
			if outcome.kept && err != nil {
				t.Fatalf("returning the turn lost the compute's lease: %v", err)
			}
			if !outcome.kept && !errors.Is(err, alloc.ErrLeaseNotFound) {
				t.Fatalf("refused lease read = %v, want lease not found", err)
			}
		})
	}
}

// RECONCILIATION MUST FINISH AND CONSUME THE BACKING BEFORE IT ENDS A TURN.
// An unused grant still needs its poll, and a failed read proves no progress.
func TestPoolReconciliationKeepsUnusedOrUnconfirmedTurns(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "unused backing", true: "failed reconciliation"}[cancelled], func(t *testing.T) {
			_, listeners := arbitrationListeners(t, []config.Tier{tier("a"), tier("b")}, tierVCPU)
			l := listeners[0]
			l.observed = &Statistics{TotalAssignedJobs: 1}
			if err := l.prepareEscrow(t.Context()); err != nil {
				t.Fatal(err)
			}
			_, before := l.admissionPoll()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if cancelled {
				cancel()
			}
			err := l.reconcileAdmissionPool(ctx, 0)
			if cancelled && !errors.Is(err, context.Canceled) {
				t.Fatalf("reconciliation = %v, want context canceled", err)
			}
			if !cancelled && err != nil {
				t.Fatal(err)
			}
			_, after := l.admissionPoll()
			if before == 0 || after != before || l.idleEscrow() != 1 {
				t.Fatalf("turn %d became %d with %d held, want unchanged backing",
					before, after, l.idleEscrow())
			}
		})
	}
}

// LOSING HELD BACKING TO A HEARTBEAT DOES NOT SERVE THE WAITING JOB. The same
// owner must replace that backing without giving its unused turn to its peer.
// A RECONCILIATION THAT TOOK NO HELD BACKING KEEPS ITS TURN AND BUYS NO SECOND
// LEASE under it. Zero idle escrow cannot distinguish backing the heartbeat lost
// from backing that moved into a live promise, so the grant is never cleared on
// that reading; the next handled exchange ends the turn instead.
func TestPoolReconciliationKeepsATurnWhoseBackingTheHeartbeatLost(t *testing.T) {
	a, listeners := arbitrationListeners(t, []config.Tier{tier("a"), tier("b")}, tierVCPU)
	first, second := listeners[0], listeners[1]
	for _, l := range listeners {
		l.observed = &Statistics{TotalAssignedJobs: 1}
		l.observeDemand(l.observed)
	}
	if err := first.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	held := first.Held()
	if len(held) != 1 {
		t.Fatalf("fixture has %d held leases, want 1", len(held))
	}
	_, before := first.admissionPoll()
	launches, losses := 0, 0
	first.runner = &fakeRunner{onLaunch: func(int64) error {
		launches++
		return nil
	}}
	first.beforePoolReconcile = func() {
		losses++
		if err := a.Release(t.Context(), held[0].ID, held[0].Epoch, alloc.PhaseDone); err != nil {
			t.Fatal(err)
		}
		first.heartbeatPass(t.Context())
		if first.idleEscrow() != 0 {
			t.Fatal("heartbeat did not drop the lost backing before reconciliation")
		}
	}
	if err := first.reconcileAdmissionPool(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	_, after := first.admissionPoll()
	if losses != 1 || launches != 0 || before == 0 || after != before {
		t.Fatalf("losses %d, launches %d, turn %d became %d; want one loss and the unused turn",
			losses, launches, before, after)
	}
	if err := first.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := second.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if bought := first.Held(); len(bought) != 0 || second.capacity() != 0 {
		t.Fatalf("held %+v, peer capacity %d; want no second purchase under a grant already spent",
			bought, second.capacity())
	}
	_, still := first.admissionPoll()
	if still != before {
		t.Fatalf("the refill changed turn %d to %d", before, still)
	}
}

// A STALE TARGET CANNOT BUY TWO LEASES IN ONE TURN. The heartbeat removes a
// promise after its capacity contributed to the target but before the purchase.
func TestArbitratedRefillBuysOneLeaseAfterCommittedCapacityDisappears(t *testing.T) {
	a, listeners := arbitrationListeners(t, []config.Tier{tier("work")}, 2*tierVCPU)
	l := listeners[0]
	l.observed = &Statistics{TotalAssignedJobs: 2}
	if err := l.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, spent := l.admissionPoll()
	if got := l.reserve([]Job{{RequestID: 11}}); !slices.Equal(got, []int64{11}) {
		t.Fatalf("reserved %v, want request 11", got)
	}
	promised := l.acquiring[11].lease
	l.finishAdmissionTurn(spent)
	if target := l.targetCapacity(); target != 2 {
		t.Fatalf("fixture target = %d, want 2", target)
	}
	losses := 0
	l.beforeEscrowRefill = func() {
		losses++
		if l.committedCapacity() != 1 {
			t.Fatal("promise disappeared before the refill captured its target")
		}
		if err := a.Release(t.Context(), promised.ID, promised.Epoch, alloc.PhaseDone); err != nil {
			t.Fatal(err)
		}
		l.heartbeatPass(t.Context())
		if l.capacity() != 0 {
			t.Fatal("heartbeat did not remove committed capacity before the purchase")
		}
	}
	if err := l.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if losses != 1 || l.idleEscrow() != 1 || usage.Leases != 1 || usage.VCPU != tierVCPU {
		t.Fatalf("losses %d, held %d, usage %+v; want one purchase despite the stale target",
			losses, l.idleEscrow(), usage)
	}
	if err := l.prepareEscrow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if losses != 1 || l.idleEscrow() != 1 {
		t.Fatalf("same grant refilled again: losses %d, held %d", losses, l.idleEscrow())
	}
}
