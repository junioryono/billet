package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// notDrainingHere is the drain grace for tests that are NOT about draining.
//
// Cancelling with a job still running now means a drain: the listener waits for
// a completion before tearing anything down. These fake sessions were written
// before the drain existed and never send one, so on the real six-hour default
// they would wait it out — which is the drain working correctly, since the job
// genuinely is still running.
//
// A test about escrow, acquisition or teardown says here that it is not testing
// the drain. Shortening the drain for everybody to keep them passing would be
// the wrong fix, and would quietly delete the behaviour from production.
const notDrainingHere = 50 * time.Millisecond

// tierVCPU is the size of every tier in these tests. One number, so the capacity
// arithmetic in the assertions can be read without cross-referencing.
const tierVCPU = 4

// THE invariant of the listener plane, and the reason the allocator exists.
//
// **The sum of what every listener has advertised to GitHub at any instant never
// exceeds the global budget.**
//
// Each tier is its own scale set with its own `maxCapacity`, sent on every
// long-poll as X-ScaleSetMaxCapacity. If each listener computed its own maximum
// from its own tier's headroom, GitHub could fill all of them at once: three
// tiers each advertising "I can take 10" on a host with room for 12 is a promise
// billet cannot keep, and GitHub has already assigned the jobs by the time
// anyone notices. Reserving on ASSIGNMENT is too late for the same reason.
//
// So capacity is escrowed BEFORE it is advertised, and a listener may only
// advertise what the escrow actually returned. This test drives several
// listeners at once against one allocator and watches what they advertise.
//
// It is written against a fake session rather than GitHub because the property
// is about billet's arithmetic, not about the wire protocol.
func TestAdvertisedCapacityNeverExceedsTheBudget(t *testing.T) {
	const (
		budget  = 12       // vCPU
		perTier = tierVCPU // so at most 3 runners exist across all tiers at once
	)

	tiers := []config.Tier{
		tier("billet-4vcpu-a"),
		tier("billet-4vcpu-b"),
		tier("billet-4vcpu-c"),
	}

	a := newAllocator(t, alloc.Limits{MaxVCPU: budget, MaxMemory: 64 * config.GiB}, tiers)

	// advertised tracks what each listener currently has outstanding, and the
	// peak of their sum. A listener holds its advertisement for the duration of
	// a poll, which is exactly the window in which GitHub may act on it.
	var (
		mu          sync.Mutex
		outstanding = map[string]int{}
		peak        int
	)

	observe := func(tierLabel string, capacity int) {
		mu.Lock()
		defer mu.Unlock()

		outstanding[tierLabel] = capacity

		sum := 0
		for _, c := range outstanding {
			sum += c
		}

		if sum > peak {
			peak = sum
		}
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	var polls atomic.Int32

	var wg sync.WaitGroup

	for _, tr := range tiers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			session := &fakeSession{
				onPoll: func(maxCapacity int) {
					// Recorded and NOT cleared afterwards. An advertisement is live
					// from the moment it is sent until it is replaced — GitHub may
					// act on the last maxCapacity it saw at any point — so the
					// honest measure is the sum of each listener's most recent one.
					observe(tr.Label, maxCapacity)

					if polls.Add(1) >= 60 {
						cancel()
					}
				},
			}

			l := NewListener(a, tr.Label, session)
			if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
				!errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("listener %s: %v", tr.Label, err)
			}
		}()
	}

	wg.Wait()

	if polls.Load() == 0 {
		t.Fatal("no listener ever polled; this test proves nothing")
	}

	// vCPU, not runner count: the budget is a vector and this is the axis the
	// tiers here contend on.
	if got := peak * perTier; got > budget {
		t.Errorf("listeners advertised %d vCPU at once against a %d vCPU budget", got, budget)
	}
}

// Advertising zero is not the same as refusing work, and both are required.
//
// AdvertiseNothing stops billet asking for jobs. It does not stop GitHub
// delivering one that was already queued, or redelivering one never acknowledged
// — so a dry run that only sets the header would still acquire a job nothing can
// launch, which is the exact outcome it exists to prevent.
func TestAdvertisingNothingAlsoRefusesWork(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var delivered atomic.Bool

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if delivered.Swap(true) {
			return nil, ErrNoMessage
		}

		// Queued before the dry run started, which GitHub can legitimately do.
		return &Message{
			MessageID: 1,
			Available: []Job{{RequestID: 11, RunID: 101}},
			Assigned:  []Job{{RequestID: 11, RunID: 101}},
		}, nil
	}

	l := NewListener(a, tiers[0].Label, session, WithMaxCapacity(0))

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	if got := session.acquiredIDs(); len(got) != 0 {
		t.Errorf("a run advertising zero capacity acquired %v", got)
	}

	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.Leases != 0 {
		t.Errorf("%d leases escrowed while advertising nothing", usage.Leases)
	}
}

// tier builds a catalog entry. Every tier here is 4 vCPU so the arithmetic in
// the capacity assertions stays legible; the parameter exists so a test that
// needs an uneven catalog does not have to rewrite this.
//
// AVAILABLE is what gets acquired; ASSIGNED is what consumes escrow.
//
// This was wrong, and the comment explaining why it was right was wrong too:
// JobAvailable was dropped in translation as "pre-assignment noise" and
// AcquireJobs was called with the ids from JobAssigned. Available is the OFFER —
// it is the message whose RunnerRequestID claims work. Assigned is the later
// confirmation that a claim succeeded. Acquiring from Assigned asks GitHub to
// claim work it has already handed over, while every actual offer goes on the
// floor, so the scale set advertises capacity and never takes a job.
//
// It compiled, the tests passed, and it would have failed on first contact with
// a real organization in a way that looks like "GitHub never sends us work".
func TestAvailableIsAcquiredAndAssignedConsumesEscrow(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var delivered atomic.Bool

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if delivered.Swap(true) {
			// One batch, then idle until the test cancels.
			return nil, ErrNoMessage
		}

		return &Message{
			MessageID: 1,
			// Different id spaces on purpose: an implementation that acquires the
			// wrong class acquires the wrong NUMBER, which is what this asserts on.
			Available: []Job{{RequestID: 11, RunID: 101}, {RequestID: 12, RunID: 102}},
			Assigned:  []Job{{RequestID: 11, RunID: 101}},
		}, nil
	}

	l := NewListener(a, tiers[0].Label, session,
		// A runner that starts things, because these assertions are about escrow
		// rather than launching. The default fails closed, which correctly hands
		// the capacity back and would empty every count below.
		WithRunner(&fakeRunner{}),
		WithDrainGrace(notDrainingHere))

	// Observed DURING the run. Shutdown releases running leases as well as held
	// ones — correctly, since nothing can be executing them yet — so anything
	// asserted after Run returns would see zero either way and prove nothing.
	var running atomic.Int32

	// STOPS ON THE CONDITION, NOT ON A STOPWATCH. This used to sleep 150ms and
	// then cancel, which is a bet that a machine running fourteen instrumented
	// test binaries in parallel gets through an acquire, an assign and a launch in
	// that window. It usually does, and when it does not the failure reads as a
	// listener that acquired nothing — a real-looking bug that is only a slow
	// scheduler.
	//
	// The listener has done what this test is about once a poll observes the
	// running lease; anything after that is the shutdown path, which is a
	// different test.
	stop := sync.OnceFunc(cancel)

	session.onPoll = func(int) {
		if n := int32(l.Running()); n > 0 {
			running.Store(n)
			stop()
		}
	}

	// A ceiling, so a genuine failure to make progress ends the test rather than
	// waiting out the context — and a STOPPABLE one, because a sleeping goroutine
	// outlives the test that started it, holding its state for the full ten
	// seconds while the rest of the package runs in parallel.
	ceiling := time.AfterFunc(10*time.Second, stop)
	defer ceiling.Stop()

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	got := session.acquiredIDs()

	if len(got) != 2 {
		t.Fatalf("acquired %v, want both AVAILABLE ids (11, 12)", got)
	}

	for _, want := range []int64{11, 12} {
		if !slices.Contains(got, want) {
			t.Errorf("available id %d was never acquired; acquired %v", want, got)
		}
	}

	// Exactly one lease moved to running, for the one job that was ASSIGNED — not
	// two for the two that were merely offered.
	if got := running.Load(); got != 1 {
		t.Errorf("%d leases running after one assignment, want 1", got)
	}
}

// A redelivered Assigned message must not consume a second lease.
//
// DeleteMessage is the acknowledgement and an unacknowledged message comes back,
// so this is an ordinary event rather than a fault. Assigning the same request
// twice takes a second lease for one job — capacity nothing ever gives back,
// because only one completion will arrive for it.
func TestRedeliveredAssignmentDoesNotConsumeASecondLease(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var deliveries atomic.Int32

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		// The SAME assignment twice, as a redelivery looks.
		if deliveries.Add(1) <= 2 {
			return &Message{
				MessageID: 1,
				Assigned:  []Job{{RequestID: 11, RunID: 101}},
			}, nil
		}

		return nil, ErrNoMessage
	}

	l := NewListener(a, tiers[0].Label, session,
		// A runner that starts things, because these assertions are about escrow
		// rather than launching. The default fails closed, which correctly hands
		// the capacity back and would empty every count below.
		WithRunner(&fakeRunner{}),
		WithDrainGrace(notDrainingHere))

	var running atomic.Int32

	session.onPoll = func(int) { running.Store(int32(l.Running())) }

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	if got := running.Load(); got != 1 {
		t.Errorf("%d leases running after the same assignment twice, want 1", got)
	}
}

// A completed job gives its capacity back.
//
// Without this the cycle never closes: the lease stays open until the reaper
// expires it, holding capacity for a job that has finished.
func TestCompletionReleasesTheLease(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var stage atomic.Int32

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		switch stage.Add(1) {
		case 1:
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		case 2:
			return &Message{MessageID: 2, Completed: []Job{{RequestID: 11, RunID: 101}}}, nil
		default:
			return nil, ErrNoMessage
		}
	}

	l := NewListener(a, tiers[0].Label, session)

	var running atomic.Int32

	session.onPoll = func(int) { running.Store(int32(l.Running())) }

	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	if got := running.Load(); got != 0 {
		t.Errorf("%d leases still running after the job completed, want 0", got)
	}
}

// A backlog that predates the session is only visible in the session's own
// statistics: a restart does not replay messages for work already assigned.
func TestSessionStatisticsAreObserved(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	session := &fakeSession{
		stats:  &Statistics{TotalAssignedJobs: 7},
		onPoll: func(int) { cancel() },
	}

	l := NewListener(a, tiers[0].Label, session)
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Run: %v", err)
	}

	if l.Backlog() != 7 {
		t.Errorf("Backlog() = %d, want 7 from the session statistics", l.Backlog())
	}
}

func tier(label string) config.Tier {
	const vcpu = tierVCPU

	return config.Tier{
		Label:    label,
		Provider: config.ProviderFirecracker,
		GuestOS:  config.GuestLinux,
		VCPU:     vcpu,
		Memory:   4 * config.GiB,
		Image:    "ubuntu-2404-x64",
	}
}

// openState opens a throwaway control-plane database for one test.
func openState(t *testing.T) *state.DB {
	t.Helper()

	db, err := state.Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

func newAllocator(t *testing.T, limits alloc.Limits, tiers []config.Tier,
	opts ...alloc.Option,
) *alloc.Allocator {
	t.Helper()

	db := openState(t)

	a, err := alloc.New(db, limits, tiers, opts...)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	return a
}

// A poll that lasts longer than the lease TTL must not cost the listener its
// escrow.
//
// This is the failure that made heartbeats independent. A long poll is nominally
// ~50 seconds against a 90 second TTL, which reads like ample margin. Measured
// against a real organization, the first poll ever made ran ~88 seconds — but the
// vendor's HTTP client permits a request to run for minutes once slow responses
// and retries are counted, and heartbeats that happen only BETWEEN polls stop for
// as long as one poll lasts. The reaper then terminalises the leases, another
// tier escrows the capacity, and the poll returns an assignment backed by a lease
// this listener no longer holds.
func TestEscrowSurvivesAPollLongerThanTheLeaseTTL(t *testing.T) {
	// Raised from 150ms, and the honest version of why: this test failed twice
	// during full `make check` runs on a loaded machine and has never failed in
	// isolation — including at -count=10 under ten competing test binaries, at
	// BOTH constants. So the leading explanation is that renewal at ttl/3 fired
	// every 50ms, the same order as goroutine scheduling jitter under load, and
	// 600ms gives that four times the room.
	//
	// It is a hypothesis, not a demonstration. I could not reproduce the original
	// failure on demand, so I cannot claim this constant fixes it — only that the
	// margin it removes was the smallest one in the test. What is NOT in doubt is
	// that production is unaffected either way: the TTL there is 90s and renewal
	// has 60s of slack, so the RATIO was never the thing at risk. If this fails
	// again, the next step is instrumenting the renewal goroutine's actual wakeup
	// times rather than raising the constant a second time.
	const ttl = 600 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// The reaper has to be running, or nothing punishes a missed heartbeat and
	// this passes against an implementation with no heartbeats at all.
	go func() {
		tick := time.NewTicker(ttl / 5)
		defer tick.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if _, err := a.Reap(ctx); err != nil && ctx.Err() == nil {
					t.Errorf("Reap: %v", err)
				}
			}
		}
	}()

	var (
		polls    atomic.Int32
		resultMu sync.Mutex
		checked  []*alloc.Lease
		failures []error
	)

	l := NewListener(a, "billet-4vcpu-a", nil)

	l.session = &fakeSession{onPoll: func(int) {
		switch polls.Add(1) {
		case 1:
			// The escrow happens before the poll, so what is held now is what has
			// to survive the stall — FIVE TIMES the TTL inside a single
			// GetMessage. Every one of these leases expires during this call
			// unless something renews them on a clock of its own.
			held := l.Held()

			time.Sleep(5 * ttl)

			// Checked HERE, not after Run returns: shutdown releases the escrow,
			// so by then every lease is legitimately terminal and the assertion
			// would fire against correct behaviour. This is the only moment that
			// distinguishes "renewed through the long poll" from "reaped during
			// it" — one poll later, and refillEscrow has already replaced them.
			//
			// Identity, not count: a listener that loses its escrow re-escrows and
			// ends up holding the same NUMBER of leases. Renewability of these
			// specific leases is the property, and it is exactly what the listener
			// needs to be true — a reaped lease is terminal, so heartbeating one
			// reports ErrFenced or ErrLeaseNotFound.
			var errs []error

			for _, lease := range held {
				if err := a.Heartbeat(ctx, lease.ID, lease.Epoch); err != nil {
					errs = append(errs, fmt.Errorf("lease %s: %w", lease.ID, err))
				}
			}

			resultMu.Lock()
			checked, failures = held, errs
			resultMu.Unlock()
		case 3:
			cancel()
		}
	}}

	// Cancellation is how this listener is stopped, and Run reports the context
	// error rather than nil — Server.Run is what turns that into a clean exit.
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	resultMu.Lock()
	defer resultMu.Unlock()

	if len(checked) == 0 {
		t.Fatal("the listener escrowed nothing before the long poll; the test proves nothing")
	}

	for _, err := range failures {
		t.Errorf("could not renew an escrowed lease after a poll longer than the TTL (%v); "+
			"it was reaped mid-poll, so heartbeats are still bounded by the poll cadence", err)
	}
}

// A backlog GitHub already assigned has to be SAID, not merely stored.
//
// The session's statistics were being copied into a field that nothing read,
// which is indistinguishable from not collecting them. On a restart, GitHub goes
// on believing this scale set is running jobs whose runners died with the
// process. The ones no runner had picked up sit until the pickup deadline and
// are then reassigned; the ones already running are not reassigned at all and
// fail. From the outside billet looks like it silently dropped both.
//
// Recovering them needs a node runtime that can adopt a running instance, which
// does not exist. Reporting them is what makes the gap visible rather than
// mysterious, so that is what is tested.
func TestAnAlreadyAssignedBacklogIsReported(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var logged bytes.Buffer

	l := NewListener(a, "billet-4vcpu-a", nil,
		WithLogger(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelWarn}))))

	l.session = &fakeSession{
		// Seven jobs GitHub thinks are running, and no lease for any of them.
		stats:  &Statistics{TotalAssignedJobs: 7},
		onPoll: func(int) { cancel() },
	}

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	out := logged.String()
	if !strings.Contains(out, "assigned=7") {
		t.Errorf("a backlog of 7 already-assigned jobs was not reported; logs were:\n%s", out)
	}
}

// An acquisition is a PROMISE, and billet must not make one it cannot back.
//
// Advertising and acquiring answer different questions. maxCapacity is the scale
// set's total, so it counts assigned jobs too — but an offer can still arrive for
// more than this listener currently has free, and acquiring it tells GitHub the
// job will run here. Whatever cannot be backed is then stranded: the Assigned
// message arrives, there is no escrow, and the listener stops the control plane
// over a promise it should never have made.
func TestOffersAreAcquiredOnlyUpToFreeEscrow(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	// Room for exactly one runner, and it is already running a job.
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 16 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var delivered atomic.Int32

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			// Consume the only slot.
			return &Message{
				MessageID: 1,
				Available: []Job{{RequestID: 11, RunID: 101}},
				Assigned:  []Job{{RequestID: 11, RunID: 101}},
			}, nil
		case 2:
			// Two more offers with nothing free to back either.
			return &Message{
				MessageID: 2,
				Available: []Job{{RequestID: 12, RunID: 102}, {RequestID: 13, RunID: 103}},
			}, nil
		}

		cancel()

		return nil, ErrNoMessage
	}

	l := NewListener(a, tiers[0].Label, session,
		// A runner that starts things, because these assertions are about escrow
		// rather than launching. The default fails closed, which correctly hands
		// the capacity back and would empty every count below.
		WithRunner(&fakeRunner{}),
		WithDrainGrace(notDrainingHere))

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	// Only the first offer, which the one free lease backed. Acquiring 12 or 13
	// would be promising GitHub a job with no capacity set aside for it.
	if got := session.acquiredIDs(); !slices.Equal(got, []int64{11}) {
		t.Errorf("acquired %v; only request 11 had escrow behind it", got)
	}
}

// GitHub batches a job's completion with the offer of its replacement, because
// it considers the slot free the moment the job ends. Billet has to release
// before it acquires, or it claims the replacement while still holding the
// finished job's lease and then has nothing left to back the claim.
//
// The failure is not visible at acquisition time. It surfaces one message later,
// when the Assigned for the replacement arrives, finds no escrow, and stops the
// control plane — so this drives the sequence all the way through that message.
func TestACompletionFreesTheSlotItsReplacementNeeds(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	// One runner's worth of budget, so the replacement can only run on the slot
	// the completion gives back.
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 16 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var delivered atomic.Int32

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			return &Message{
				MessageID: 1,
				Available: []Job{{RequestID: 11, RunID: 101}},
				Assigned:  []Job{{RequestID: 11, RunID: 101}},
			}, nil
		case 2:
			// The completion of 11 and the offer of 12, together.
			return &Message{
				MessageID: 2,
				Available: []Job{{RequestID: 12, RunID: 102}},
				Completed: []Job{{RequestID: 11, RunID: 101}},
			}, nil
		case 3:
			// And the confirmation that the claim on 12 stuck.
			return &Message{
				MessageID: 3,
				Assigned:  []Job{{RequestID: 12, RunID: 102}},
			}, nil
		}

		cancel()

		return nil, ErrNoMessage
	}

	l := NewListener(a, tiers[0].Label, session)

	// Acquiring before releasing leaves nothing to back request 12, and this call
	// returns "assigned request 12 with no escrowed capacity".
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if got := session.acquiredIDs(); !slices.Equal(got, []int64{11, 12}) {
		t.Errorf("acquired %v, want both offers claimed in turn", got)
	}
}

// A redelivered batch must not consume a second lease for a job that is over.
//
// Billet acknowledges a message AFTER handling it — unlike the vendor's listener,
// which deletes first and so handles everything at most once. Acking last means a
// crash mid-handling redelivers rather than silently dropping a job, which is the
// safer trade for capacity, but it only holds if every handler is idempotent.
//
// `running` alone is not enough: complete() removes the entry, so on redelivery
// the request looks brand new. That is not a contrived case — a batch carrying
// Assigned and Completed for the SAME request is what an assigned-then-cancelled
// job looks like, which GitHub does to a job no runner picks up in time.
func TestARedeliveredCompletionDoesNotConsumeASecondLease(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	// One runner. A second lease for the same job cannot be quietly absorbed.
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 4, MaxMemory: 16 * config.GiB}, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var (
		delivered atomic.Int32
		acked     atomic.Int32
	)

	session := &fakeSession{}

	// The first acknowledgement fails, which is exactly what makes GitHub send the
	// batch again.
	session.onDelete = func(int64) error {
		if acked.Add(1) == 1 {
			return errors.New("acknowledgement lost in transit")
		}

		return nil
	}

	batch := func(id int64) *Message {
		return &Message{
			MessageID: id,
			Assigned:  []Job{{RequestID: 11, RunID: 101}},
			Completed: []Job{{RequestID: 11, RunID: 101}},
		}
	}

	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			return batch(1), nil
		case 2:
			// Redelivered, because the acknowledgement did not land.
			return batch(1), nil
		}

		cancel()

		return nil, ErrNoMessage
	}

	l := NewListener(a, tiers[0].Label, session)

	// A failed acknowledgement is an error, so Run returns on the first batch.
	// ASSERTED rather than discarded: if this run ever stops failing, the message
	// is acknowledged, GitHub never redelivers it, and everything below is testing
	// a redelivery that did not happen.
	if err := l.Run(ctx); err == nil || errors.Is(err, context.Canceled) {
		t.Fatalf("the first run was expected to fail on the lost acknowledgement, got %v; "+
			"without that failure there is no redelivery to be idempotent against", err)
	}

	// Restarted the way the control plane restarts it: a NEW listener on the same
	// session. Reusing the old one was neither what Server does — it builds one
	// per tier run — nor safe, since a listener that has shut down has sealed its
	// cleanup loop and closed its session.
	l = NewListener(a, tiers[0].Label, session)

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run after redelivery: %v", err)
	}

	// Counted in job_history, NOT in open leases.
	//
	// Asserting that no lease is left open proves nothing here: shutdown releases
	// running leases too, so a redelivery that consumed a second lease still ends
	// at zero and the test passes against the bug it exists to catch. Confirmed by
	// mutation — that is exactly what the first version of this test did.
	//
	// Every assignment writes a job_history row keyed by lease id, so a second
	// lease for one request leaves a second row carrying the same request id.
	// That evidence outlives the release.
	var leases int

	if err := db.Reader().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM job_history WHERE request_id = ?`, 11).Scan(&leases); err != nil {
		t.Fatalf("count job_history rows: %v", err)
	}

	if leases > 1 {
		t.Errorf("request 11 was assigned %d separate leases; the redelivered batch consumed "+
			"another one for a job that was already over", leases)
	}
}

// One lease cannot back two promises.
//
// An acquisition is an obligation that lasts until the Assigned message arrives,
// and capping acquisitions at an instantaneous count of free leases does not
// model that. The lease stayed in `held` while the claim was in flight, so the
// next batch's offer counted the very same lease as available and billet
// promised GitHub two jobs it had capacity for one of.
//
// Consecutive offers are the clearest form: nothing about the second batch tells
// the listener the first lease is already spoken for, unless the reservation is
// recorded when the promise is made.
func TestOneLeaseCannotBackTwoAcquisitions(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	// Room for exactly one runner.
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 16 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var delivered atomic.Int32

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
		case 2:
			// A second offer, with the only lease already promised to the first.
			return &Message{MessageID: 2, Available: []Job{{RequestID: 12, RunID: 102}}}, nil
		}

		cancel()

		return nil, ErrNoMessage
	}

	l := NewListener(a, tiers[0].Label, session, WithDrainGrace(notDrainingHere))

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if got := session.acquiredIDs(); !slices.Equal(got, []int64{11}) {
		t.Errorf("acquired %v; the single lease was promised to 11, so 12 had nothing behind it", got)
	}
}

// Escrow promised to an offer GitHub did not grant has to come back.
//
// AcquireJobs returns what it ACTUALLY gave, which can be fewer ids than were
// asked for, because another scale set can win the same offer. A reservation
// left standing for an assignment that is never coming strands the lease until
// the reaper takes it.
func TestEscrowIsReturnedWhenAnOfferIsNotGranted(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var (
		delivered atomic.Int32
		acquiring atomic.Int32
	)

	// Declared ahead of the session so the poll hook can read the listener's own
	// state while it is running.
	var l *Listener

	session := &fakeSession{}

	// Two offers, one granted.
	session.onAcquire = func(ids []int64) ([]int64, error) { return ids[:1], nil }

	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			return &Message{MessageID: 1, Available: []Job{
				{RequestID: 11, RunID: 101},
				{RequestID: 12, RunID: 102},
			}}, nil
		case 2:
			// Sampled after the acquisition settled, before shutdown releases it.
			acquiring.Store(int32(l.Acquiring()))

			cancel()
		}

		return nil, ErrNoMessage
	}

	l = NewListener(a, tiers[0].Label, session, WithDrainGrace(notDrainingHere))

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if got := acquiring.Load(); got != 1 {
		t.Errorf("%d leases still promised; only one offer was granted, so the other "+
			"reservation should have gone back to the free escrow", got)
	}
}

// An assignment with nothing behind it is declined, not fatal.
//
// This used to return an error, on the grounds that being assigned more than was
// advertised is a protocol violation rather than a race billet can absorb. That
// held when GitHub over-assigning was the only way to reach it. It is not any
// more: billet's own escrow can vanish underneath an acquisition — the heartbeat
// drops a fenced lease, a restart loses the promise — and a listener error takes
// the whole control plane down, stranding every tier's capacity over one job.
//
// The invariant that matters is preserved either way: nothing runs without
// escrow. What changes is the blast radius.
func TestAnUnbackedAssignmentIsDeclinedRatherThanFatal(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a"), tier("billet-4vcpu-b")}

	// The whole budget is escrowed by the OTHER tier, so tier a can hold nothing.
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 16 * config.GiB}, tiers)

	if _, err := a.Escrow(t.Context(), "billet-4vcpu-b", 1); err != nil {
		t.Fatalf("Escrow: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var delivered atomic.Int32

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if delivered.Add(1) == 1 {
			// Assigned with no offer, no promise, and no free escrow.
			return &Message{MessageID: 1, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		cancel()

		return nil, ErrNoMessage
	}

	l := NewListener(a, "billet-4vcpu-a", session)

	// Cancellation, not a protocol error. A listener that returns an error here
	// cancels every other listener with it.
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("an unbacked assignment stopped the listener: %v", err)
	}

	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	// And it stayed declined: the other tier's lease is the only one open.
	if usage.Leases != 1 {
		t.Errorf("%d leases open; declining must not have consumed capacity", usage.Leases)
	}
}

// A job cancelled before it was ever assigned still has to give its escrow back.
//
// GitHub cancels an assignment no runner picks up in time, and that cancellation
// arrives as a completion — for a request billet acquired but was never given.
// The lease sits in the promised state, which is neither free nor running, so a
// completion path that only looks at running leaves it reserved for an
// assignment that is never coming, until the reaper takes it.
//
// The discriminating assertion is the NEXT offer: escrow that did not come back
// is escrow the following request cannot use.
func TestACancelledOfferReturnsItsPromisedEscrow(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	// One runner, so request 12 can only be acquired if 11 gave its lease back.
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 16 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var (
		delivered atomic.Int32
		mu        sync.Mutex
		peak      int
	)

	session := &fakeSession{}

	// The advertisement is what actually catches this. Simply releasing the lease
	// in the ledger without dropping the promise leaves a phantom in the escrow:
	// the next refill escrows a REPLACEMENT, and the tier then advertises the
	// phantom and the replacement together — two runners against a one-runner
	// budget. Whether the following offer can be acquired does not discriminate,
	// because the replacement backs it either way. Confirmed by mutation.
	session.onPoll = func(capacity int) {
		mu.Lock()
		if capacity > peak {
			peak = capacity
		}
		mu.Unlock()
	}

	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
		case 2:
			// Cancelled before any assignment arrived.
			return &Message{MessageID: 2, Completed: []Job{{RequestID: 11, RunID: 101}}}, nil
		case 3:
			return &Message{MessageID: 3, Available: []Job{{RequestID: 12, RunID: 102}}}, nil
		}

		cancel()

		return nil, ErrNoMessage
	}

	l := NewListener(a, tiers[0].Label, session, WithDrainGrace(notDrainingHere))

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if got := session.acquiredIDs(); !slices.Equal(got, []int64{11, 12}) {
		t.Errorf("acquired %v; request 11 was cancelled, so its lease should have been free "+
			"for request 12", got)
	}

	mu.Lock()
	defer mu.Unlock()

	if peak > 1 {
		t.Errorf("advertised %d runners against a one-runner budget; the cancelled request's "+
			"promise was released in the ledger but never dropped from the escrow", peak)
	}
}

// A promise that goes unclaimed is REPORTED and KEPT, never reclaimed.
//
// This asserts the reverse of what it originally did, and the reversal is the
// point. Releasing the escrow when a promise aged out looked like the obvious
// fix for a lease nothing else could reclaim — and it was wrong, because an
// acquisition is a commitment made to GitHub that no local timer can revoke.
// AcquireJobs is one-way: the session client has no decline or release endpoint,
// and DeleteMessage acknowledges a notification rather than refusing a job. A
// timed release therefore hands nothing back. It only means billet has forgotten
// it owes a runner while GitHub still expects one, so the freed slot goes to
// another tier and the assignment, when it comes, has nothing behind it.
//
// Holding the lease is the lesser evil and the honest one: it is capacity billet
// genuinely still owes. What resolves it is the session ending.
func TestAStalePromiseIsReportedAndKept(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 16 * config.GiB}, tiers,
		alloc.WithLeaseTTL(300*time.Millisecond))

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var (
		delivered atomic.Int32
		stillHeld atomic.Bool
		reported  atomic.Bool
		logged    bytes.Buffer
		logMu     sync.Mutex
	)

	var l *Listener

	said := func() bool {
		logMu.Lock()
		defer logMu.Unlock()

		return strings.Contains(logged.String(), "gone unassigned")
	}

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		if delivered.Add(1) == 1 {
			// Acquired, and then GitHub never mentions it again.
			return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		// Paced, so the heartbeat goroutine actually gets to run between polls —
		// counting polls raced it, because an idle poll returns immediately and a
		// dozen of them elapse well inside one heartbeat interval.
		time.Sleep(20 * time.Millisecond)

		if said() {
			// Sampled at the moment it is reported: the promise must still be held.
			stillHeld.Store(l.Acquiring() == 1)
			reported.Store(true)

			cancel()
		}

		return nil, ErrNoMessage
	}

	l = NewListener(a, tiers[0].Label, session,
		WithStalePromiseAfter(100*time.Millisecond),
		WithLogger(slog.New(slog.NewTextHandler(&syncWriter{mu: &logMu, w: &logged},
			&slog.HandlerOptions{Level: slog.LevelWarn}))),
		WithDrainGrace(notDrainingHere))

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if !reported.Load() {
		logMu.Lock()
		defer logMu.Unlock()

		t.Fatalf("a promise sat unclaimed and nothing was reported; logs were:\n%s", logged.String())
	}

	if !stillHeld.Load() {
		t.Error("the stale promise was reclaimed; billet still owes GitHub a runner for that " +
			"job, and the slot must not be given to another tier")
	}
}

// A re-offered request must not tear down the promise it already has.
//
// reserve() used to return an id that was already being acquired, and acquire()
// unreserves whatever GitHub does not grant. So a second Available for a request
// billet had already acquired would, on a partial grant, delete the earlier
// successful promise — handing its lease to the next offer and leaving the
// original assignment with nothing. unreserve may only undo reservations the
// same call created.
func TestAReofferedRequestKeepsItsExistingPromise(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	// One runner, so the promise for 11 and any reservation for 12 compete for
	// the same lease.
	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 16 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	var (
		delivered atomic.Int32
		running   atomic.Int32
	)

	var l *Listener

	session := &fakeSession{}

	// The SECOND acquisition grants nothing, which the interface explicitly
	// permits ("may be fewer than asked for") and which is what turns the bad
	// unreserve into a lost promise.
	var grants atomic.Int32

	session.onAcquire = func(ids []int64) ([]int64, error) {
		if grants.Add(1) == 2 {
			return nil, nil
		}

		return ids, nil
	}

	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
		case 2:
			// The same offer again, which GitHub may redeliver.
			return &Message{MessageID: 2, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
		case 3:
			// A DIFFERENT request, and this is what makes the bug bite. Without it
			// the wrongly-freed lease is still sitting in held when the assignment
			// arrives, and assign's fallback picks it straight back up — so the
			// test passes against the bug. Something else has to take the lease
			// first. Confirmed by mutation.
			return &Message{MessageID: 3, Available: []Job{{RequestID: 12, RunID: 102}}}, nil
		case 4:
			// And the assignment billet was promised all along.
			return &Message{MessageID: 4, Assigned: []Job{{RequestID: 11, RunID: 101}}}, nil
		case 5:
			running.Store(int32(l.Running()))

			cancel()
		}

		return nil, ErrNoMessage
	}

	l = NewListener(a, tiers[0].Label, session,
		// A runner that starts things, because these assertions are about escrow
		// rather than launching. The default fails closed, which correctly hands
		// the capacity back and would empty every count below.
		WithRunner(&fakeRunner{}),
		WithDrainGrace(notDrainingHere))

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	if running.Load() != 1 {
		t.Errorf("request 11 is not running; the re-offer destroyed the promise its original "+
			"acquisition made, so the assignment had no lease behind it (running=%d)",
			running.Load())
	}
}

// syncWriter serialises writes from the heartbeat goroutine and the poll loop,
// which both log.
type syncWriter struct {
	mu *sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.w.Write(p)
}

// A scale-set response that is not a subset of its request stops the listener,
// and the reserved escrow is not left where the next offer can spend it.
//
// Logging and carrying on was not enough. Once a response contains an id nobody
// offered for, billet cannot tell which remote commitments are real: the
// unmatched id has no reservation, and a body wrong about that id may be wrong
// about the others. Continuing means spending reserved leases on later offers
// while GitHub may believe the original jobs are billet's.
//
// Deliberately harsher than an unbacked assignment, which declines and carries
// on. That one is reachable by ordinary races, so stopping the control plane
// over it is disproportionate. This one is not reachable by any race — it means
// the API broke — and stopping is itself the remedy, because the session is
// recreated and GitHub redelivers whatever was unacknowledged.
func TestAnAcquisitionOutsideItsRequestStopsTheListener(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	session := &fakeSession{}

	// An id that was never offered for.
	session.onAcquire = func([]int64) ([]int64, error) { return []int64{99}, nil }

	session.onGet = func() (*Message, error) {
		return &Message{MessageID: 1, Available: []Job{{RequestID: 11, RunID: 101}}}, nil
	}

	l := NewListener(a, tiers[0].Label, session)

	err := l.Run(ctx)
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the listener carried on after a response outside its own contract: %v", err)
	}

	if !strings.Contains(err.Error(), "did not offer for") {
		t.Errorf("the error does not say what went wrong: %v", err)
	}

	// And the escrow went back rather than being left promised to a request whose
	// status is unknown. Shutdown releases it either way, so this reads the ledger
	// after Run returned.
	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.Leases != 0 {
		t.Errorf("%d leases still open after the listener stopped", usage.Leases)
	}
}

// The message cursor must not move past a message that was never acknowledged.
//
// lastMessageID is a SERVER-SIDE cursor, not a local note of what was seen: the
// client sends it as ?lastMessageId= and the queue returns messages AFTER it.
// Advancing it before the work means any handling failure that does not end the
// session skips the message, and every job in it, with no trace.
//
// Driven through handle() directly, and that is the whole difficulty. Through
// Run() the bug is INVISIBLE: every failure in handle is currently fatal, so the
// listener stops before it can poll again and the bad cursor is never sent. A
// test at that level passes with the fix reverted — confirmed by mutation. The
// cursor is therefore only correct today as a side effect of an unrelated
// decision about error severity, which is exactly why it needs pinning here:
// the first non-fatal error path anyone adds would start dropping messages.
func TestTheCursorDoesNotAdvancePastAnUnacknowledgedMessage(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	session := &fakeSession{}
	session.onDelete = func(int64) error { return errors.New("acknowledgement lost in transit") }

	l := NewListener(a, tiers[0].Label, session)

	msg := &Message{MessageID: 42, Available: []Job{{RequestID: 11, RunID: 101}}}

	if err := l.handle(t.Context(), msg); err == nil {
		t.Fatal("handle reported success despite the acknowledgement failing")
	}

	if l.lastMessageID != 0 {
		t.Errorf("the cursor advanced to %d after a failed acknowledgement; the next poll "+
			"would ask github for messages after %d, skipping message 42 and every job it "+
			"carried", l.lastMessageID, l.lastMessageID)
	}

	// And it DOES advance once the acknowledgement lands, or the listener would
	// re-handle the same message forever.
	session.onDelete = nil

	if err := l.handle(t.Context(), msg); err != nil {
		t.Fatalf("handle: %v", err)
	}

	if l.lastMessageID != 42 {
		t.Errorf("the cursor is %d after a successful acknowledgement; the listener would "+
			"be redelivered message 42 forever", l.lastMessageID)
	}
}

// An assigned job is launched, and the compute is destroyed BEFORE its capacity
// is handed back.
//
// The ordering is the whole point. Releasing first frees the slot to another
// tier while this job's container is still on the host: the budget is satisfied
// on paper and overcommitted in fact. Same shape as closing the session before
// releasing the escrow, and the same reason.
func TestComputeIsDestroyedBeforeItsCapacityIsReleased(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// events records what happened in the order it happened, which is the only
	// thing that can catch an ordering bug.
	var (
		mu     sync.Mutex
		events []string
	)

	record := func(what string) {
		mu.Lock()
		defer mu.Unlock()

		events = append(events, what)
	}

	runner := &fakeRunner{
		onLaunch:  func(int64) error { record("launch"); return nil },
		onDestroy: func(int64) error { record("destroy"); return nil },
	}

	// Release is observed through the allocator by watching usage drop, so the
	// listener is given a hook rather than the test guessing at timing.
	var delivered atomic.Int32

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			return &Message{
				MessageID: 1,
				Available: []Job{{RequestID: 11, RunID: 101}},
				Assigned:  []Job{{RequestID: 11, RunID: 101}},
			}, nil
		case 2:
			return &Message{MessageID: 2, Completed: []Job{{RequestID: 11, RunID: 101}}}, nil
		case 3:
			cancel()
		}

		return nil, ErrNoMessage
	}

	l := NewListener(a, tiers[0].Label, session, WithRunner(runner))

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	if len(events) < 2 {
		t.Fatalf("expected a launch and a destroy, got %v", events)
	}

	if events[0] != "launch" {
		t.Errorf("the job was not launched first: %v", events)
	}

	if events[1] != "destroy" {
		t.Errorf("the compute was not destroyed on completion: %v", events)
	}

	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.Leases != 0 {
		t.Errorf("%d leases still open after the job finished", usage.Leases)
	}
}

// Capacity that could not be torn down is NOT handed back.
//
// A destroy failure means the container may still be running. Releasing the
// lease then lets another tier escrow a machine this job is still using, which
// is the overcommit the whole escrow exists to prevent. Capacity the reaper
// reclaims late is recoverable; capacity handed out twice is not.
func TestCapacityIsHeldWhenTheComputeWillNotDie(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	runner := &fakeRunner{
		onDestroy: func(int64) error { return errors.New("the host is not answering") },
	}

	var delivered atomic.Int32

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			return &Message{
				MessageID: 1,
				Available: []Job{{RequestID: 11, RunID: 101}},
				Assigned:  []Job{{RequestID: 11, RunID: 101}},
			}, nil
		case 2:
			return &Message{MessageID: 2, Completed: []Job{{RequestID: 11, RunID: 101}}}, nil
		}

		cancel()

		return nil, ErrNoMessage
	}

	l := NewListener(a, tiers[0].Label, session, WithRunner(runner), WithDrainGrace(notDrainingHere))

	// NOT fatal, and that is a deliberate separation of two questions. A listener
	// error cancels every other listener, and their shutdowns then destroy every
	// running job on the host — so one docker daemon hiccup while cleaning up a
	// single finished job would take down the fleet. The capacity is held; the
	// control plane keeps running.
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("a failed teardown of one job stopped the listener: %v", err)
	}

	usage, err := a.Usage(t.Context())
	if err != nil {
		t.Fatalf("Usage: %v", err)
	}

	if usage.Leases == 0 {
		t.Error("the capacity was handed back while the compute may still be running; another " +
			"tier can now escrow a machine this job is still on")
	}
}

// A job that cannot be started gives its capacity straight back.
//
// The opposite rule to the one above, and the difference is whether anything is
// running. A lease whose compute never started is backing nothing, so holding it
// withholds capacity from every other tier for no reason. GitHub reassigns the
// job when its pickup deadline passes.
func TestCapacityIsReturnedWhenTheComputeWillNotStart(t *testing.T) {
	tiers := []config.Tier{tier("billet-4vcpu-a")}

	a := newAllocator(t, alloc.Limits{MaxVCPU: 4, MaxMemory: 16 * config.GiB}, tiers)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	runner := &fakeRunner{
		onLaunch: func(int64) error { return errors.New("no space left on device") },
	}

	var (
		delivered atomic.Int32
		afterward atomic.Int32
	)

	var l *Listener

	session := &fakeSession{}
	session.onGet = func() (*Message, error) {
		switch delivered.Add(1) {
		case 1:
			return &Message{
				MessageID: 1,
				Available: []Job{{RequestID: 11, RunID: 101}},
				Assigned:  []Job{{RequestID: 11, RunID: 101}},
			}, nil
		case 2:
			// Sampled during the run: shutdown releases everything, so anything
			// asserted afterwards would look correct either way.
			afterward.Store(int32(l.Running()))

			cancel()
		}

		return nil, ErrNoMessage
	}

	l = NewListener(a, tiers[0].Label, session, WithRunner(runner))

	// NOT fatal. Failing the listener would take every tier down over one job
	// that GitHub will simply reassign.
	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("a job that failed to start stopped the listener: %v", err)
	}

	if afterward.Load() != 0 {
		t.Errorf("%d jobs still counted as running after their launch failed; that capacity "+
			"backs nothing and no other tier can use it", afterward.Load())
	}
}

// fakeRunner stands in for a host. Both hooks default to succeeding, so a test
// only says what it cares about.
type fakeRunner struct {
	onLaunch  func(requestID int64) error
	onDestroy func(requestID int64) error
}

func (f *fakeRunner) Launch(_ context.Context, _ *alloc.Lease, job Job) error {
	if f.onLaunch != nil {
		return f.onLaunch(job.RequestID)
	}

	return nil
}

func (f *fakeRunner) Destroy(_ context.Context, requestID int64) error {
	if f.onDestroy != nil {
		return f.onDestroy(requestID)
	}

	return nil
}

// fakeSession stands in for a scale-set message session. It never returns work,
// so the listener does nothing but escrow, advertise, and release — which is the
// whole of what this test is about.
type fakeSession struct {
	onPoll func(maxCapacity int)
	onGet  func() (*Message, error)
	// onDelete fails an acknowledgement, which is what makes GitHub redeliver the
	// message — the case every handler has to be idempotent against.
	onDelete func(id int64) error
	// onAcquire grants fewer ids than were asked for, which GitHub does when
	// another scale set wins the same offer.
	onAcquire func(ids []int64) ([]int64, error)
	// onClose fails the session close, which sends shutdown down the path that
	// deliberately skips the release. It receives the context so a test can
	// inspect the deadline the close was given.
	onClose func(ctx context.Context) error
	stats   *Statistics
	// acquired records every request id the listener asked GitHub to claim, so a
	// test can assert WHICH class of message drove the acquisition. polled counts
	// GetMessage calls, which is how a test tells "refused before starting" from
	// "started and then stopped".
	acquiredMu sync.Mutex
	acquired   []int64
	polled     int
	closed     int
}

func (f *fakeSession) closes() int {
	f.acquiredMu.Lock()
	defer f.acquiredMu.Unlock()

	return f.closed
}

func (f *fakeSession) polls() int {
	f.acquiredMu.Lock()
	defer f.acquiredMu.Unlock()

	return f.polled
}

func (f *fakeSession) GetMessage(ctx context.Context, _ int64, maxCapacity int) (*Message, error) {
	f.acquiredMu.Lock()
	f.polled++
	f.acquiredMu.Unlock()

	if f.onPoll != nil {
		f.onPoll(maxCapacity)
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}

	if f.onGet != nil {
		return f.onGet()
	}

	// A real long poll reports a timeout; the listener polls again.
	return nil, ErrNoMessage
}

func (f *fakeSession) Statistics() *Statistics { return f.stats }

func (f *fakeSession) DeleteMessage(_ context.Context, id int64) error {
	if f.onDelete != nil {
		return f.onDelete(id)
	}

	return nil
}

func (f *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	f.acquiredMu.Lock()
	f.acquired = append(f.acquired, ids...)
	f.acquiredMu.Unlock()

	if f.onAcquire != nil {
		return f.onAcquire(ids)
	}

	return ids, nil
}

// acquiredIDs returns what the listener asked to claim.
func (f *fakeSession) acquiredIDs() []int64 {
	f.acquiredMu.Lock()
	defer f.acquiredMu.Unlock()

	return append([]int64(nil), f.acquired...)
}

// Close HONOURS ITS CONTEXT, unlike the first version of this fake.
//
// Ignoring it made every shutdown test pass whatever context the close was
// handed, so a change that gave it an already-expired one was invisible — which
// is exactly the defect the separate teardown budgets exist to prevent.
func (f *fakeSession) Close(ctx context.Context) error {
	f.acquiredMu.Lock()
	f.closed++
	f.acquiredMu.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fake session close: %w", err)
	}

	if f.onClose != nil {
		return f.onClose(ctx)
	}

	return nil
}

// A COMPLETION WHOSE DESTROY FAILED IS RETRIED, which is what turns "capacity
// held" into something other than a leak.
//
// Holding it is deliberate: releasing while the compute may still be running is
// the overcommit the whole ordering exists to prevent. But nothing else was ever
// going to try again — GitHub's completion has been acknowledged so it will not
// be redelivered, the node may already have destroyed what it had, and the lease
// sits in `running` being heartbeated for the life of the process.
func TestACompletionWhoseDestroyFailedIsRetried(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var failures atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		if failures.Add(1) == 1 {
			return errors.New("the docker daemon is not answering")
		}

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

	holdRunning(t, l, a, tiers[0].Label, 7)

	// The first completion cannot destroy, so its capacity is held.
	if err := l.complete(t.Context(), Job{RequestID: 7}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	l.mu.Lock()
	held := len(l.cleanup)
	l.mu.Unlock()

	if held != 1 {
		t.Fatalf("a completion whose destroy failed was not recorded for retry: %d", held)
	}

	// The retry runs on the renewal clock and succeeds.
	l.retryCleanup(t.Context())

	l.mu.Lock()
	held = len(l.cleanup)
	l.mu.Unlock()

	if held != 0 {
		t.Errorf("a completion whose destroy later succeeded is still pending (%d); its "+
			"capacity is held for the life of the process", held)
	}
}

// RENEWAL IS NOT QUEUED BEHIND A SLOW DESTROY, which is why the retry has its
// own loop rather than a step in the heartbeat.
//
// Retrying a failed completion means broadcasting to nodes and waiting up to the
// command timeout. The heartbeat loop exists precisely so renewal is never
// behind something slow — its own comment says so — and the first version of the
// retry put a network call on that tick anyway. One unreachable host would then
// have delayed every renewal on the listener and expired the leases the loop was
// protecting: the exact failure the separate clock was introduced to prevent.
func TestASlowCleanupDoesNotStarveRenewal(t *testing.T) {
	t.Parallel()

	// DRIVEN THROUGH Run, because the property is about two loops and the earlier
	// version manufactured both of them itself.
	//
	// Starting retryCleanup in one goroutine and heartbeatHeld in another proves
	// only that a stuck destroy does not hold l.mu. It passes just as well against
	// a heartbeat loop that calls retryCleanup on its own tick — which is exactly
	// the regression this test is named for, and the shape production had before
	// the separate clock was introduced. The loops therefore have to be the ones
	// Run starts, and the detector has to be the consequence rather than the
	// mechanism: a lease that stops being renewed is reaped.
	// Long enough that renewal at ttl/3 gets three attempts before a lease would
	// expire, so descheduling the process for a beat or two does not fail a
	// correct implementation.
	const ttl = 1500 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// A destroy that blocks the way an unreachable node does.
	blocked := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(blocked) })

	t.Cleanup(unblock)
	entered := make(chan struct{}, 1)

	runner := &fakeRunner{onDestroy: func(int64) error {
		select {
		case entered <- struct{}{}:
		default:
		}

		<-blocked

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

	lease := holdRunning(t, l, a, tiers[0].Label, 7)

	// A completion whose destroy already failed, so the loop has something to
	// retry as soon as Run starts it.
	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{7: {job: Job{RequestID: 7}}}
	l.mu.Unlock()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	// Nothing below proves anything until the retry is genuinely stuck inside the
	// provider, which is the state the whole test is about.
	select {
	case <-entered:
	case <-runDone:
		t.Fatal("the listener stopped before its cleanup loop reached the runner")
	case <-time.After(30 * time.Second):
		t.Fatal("the retry never reached the runner, so nothing started the cleanup loop")
	}

	// AND RENEWAL OUTLIVES THE STUCK DESTROY. Long enough that a lease nobody
	// renewed has certainly expired, and then REAPED SYNCHRONOUSLY rather than by
	// a background ticker.
	//
	// A background reaper made the result depend on its own scheduling: if it was
	// delayed past this point, an expired-but-unreaped lease was still renewable,
	// and the assertion's own Heartbeat then renewed it — so a listener whose
	// renewal had stalled the whole time passed. Reaping here makes expiry
	// enforced at exactly the moment it is checked.
	time.Sleep(2 * ttl)

	if _, err := a.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if err := a.Heartbeat(ctx, lease.ID, lease.Epoch); err != nil {
		t.Fatalf("a running lease was lost while a cleanup retry was stuck in the provider "+
			"(%v); one unreachable host delays every renewal on this listener and expires "+
			"the leases it was protecting", err)
	}

	// Let the destroy finish, and WAIT FOR THE RETRY TO DRAIN before returning.
	// The blocked goroutine belongs to Run, so nothing else joins it — and if it
	// resumed after the test closed the database, its release would race that
	// close.
	unblock()

	for cleanupCount(l) > 0 {
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal("the retry never drained after the node started answering")
		}
	}

	cancel()
	<-runDone
}

// RUN DOES NOT RETURN WHILE A CLEANUP RETRY IS STILL IN THE PROVIDER.
//
// Cancelling the loops was mistaken for stopping them. A retry blocked in a
// remote Destroy — which can take the whole node command timeout — outlived Run
// and came back afterwards to call alloc.Release against a database the caller
// was entitled to have closed. Nothing joined the goroutine, so the only thing
// deciding whether that happened was how long the node took to answer.
//
// The request here is deliberately NOT in `running`, so the shutdown release has
// nothing to destroy: without the join, Run returns immediately while the retry
// is still inside the provider, which is exactly the state being ruled out.
func TestRunWaitsForACleanupStillInTheProvider(t *testing.T) {
	t.Parallel()

	const ttl = 300 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	blocked := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(blocked) })

	t.Cleanup(unblock)

	entered := make(chan struct{}, 1)

	// ONLY THE FIRST CALL BLOCKS, and that is what isolates the join.
	//
	// If every call blocked, the shutdown drain would block as well and Run would
	// wait THERE instead — so the test passed with the join deleted, because
	// something else happened to be slow in the same place. A mutation run found
	// that, not review. The first call is the cleanup loop's retry; the drain's
	// later call returns at once, leaving the join as the only thing that can
	// keep Run from returning while that retry is still inside the provider.
	var (
		firstCall atomic.Bool
		returned  atomic.Bool
	)

	runner := &fakeRunner{onDestroy: func(int64) error {
		if !firstCall.CompareAndSwap(false, true) {
			return nil
		}

		entered <- struct{}{}

		<-blocked
		returned.Store(true)

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{7: {job: Job{RequestID: 7}}}
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	// CAUSAL, not merely temporal. The timed check below can only ever say "Run
	// had not returned yet", which a descheduled goroutine satisfies too. This
	// records whether the destroy had come back at the moment Run returned.
	//
	// `returned` is set by the fake INSIDE the destroy, after the block clears —
	// setting it in the test just before unblocking left a window where Run could
	// resume, see it already true, and report nothing wrong while the destroy was
	// still in flight.
	var tooEarly atomic.Bool

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}

		if !returned.Load() {
			tooEarly.Store(true)
		}
	}()

	select {
	case <-entered:
	case <-runDone:
		t.Fatal("the listener stopped before its cleanup loop reached the runner")
	case <-time.After(30 * time.Second):
		t.Fatal("the retry never reached the runner")
	}

	cancel()

	// STILL RUNNING, because the destroy has not come back.
	select {
	case <-runDone:
		t.Fatal("Run returned while a cleanup retry was still inside the provider; that " +
			"retry comes back afterwards to release a lease against state the caller is " +
			"entitled to have torn down by then")
	case <-time.After(500 * time.Millisecond):
	}

	unblock()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned after the node answered")
	}

	if tooEarly.Load() {
		t.Fatal("Run returned before the cleanup retry had left the provider; the join is " +
			"what makes that impossible, and without it the retry outlives Run and " +
			"releases against state the caller has already torn down")
	}
}

// RENEWAL OUTLIVES THE SHUTDOWN RELEASE, which is why it is the last thing
// stopped rather than the first.
//
// The release destroys whatever is still running, and that is a remote call with
// no bound. Every lease it has not reached yet still has to be renewed while it
// works: stop renewing first and a slow teardown lets the reaper take leases
// whose compute is still being destroyed — another tier escrows that capacity
// and the host is overcommitted, which is the exact failure the destroy-then-
// release ordering exists to prevent.
//
// Ordering inside a deferred function is invisible at a glance and easy to
// "tidy" into the wrong order, so it gets a test rather than a comment.
func TestRenewalOutlivesTheShutdownRelease(t *testing.T) {
	t.Parallel()

	const ttl = 900 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	blocked := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(blocked) })

	t.Cleanup(unblock)
	entered := make(chan struct{}, 1)

	runner := &fakeRunner{onDestroy: func(int64) error {
		select {
		case entered <- struct{}{}:
		default:
		}

		<-blocked

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner), WithDrainGrace(notDrainingHere))

	lease := holdRunning(t, l, a, tiers[0].Label, 7)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	// Stop it almost immediately: the state under test is the shutdown release,
	// not anything the listener does while running.
	cancel()

	select {
	case <-entered:
	case <-runDone:
		t.Fatal("the listener returned without destroying the job it was running")
	case <-time.After(30 * time.Second):
		t.Fatal("the shutdown release never reached the runner")
	}

	// The teardown is stuck in the provider. Long enough that a lease nobody
	// renewed has expired, then reaped synchronously so expiry is enforced at the
	// moment it is checked rather than by a background ticker.
	//
	// t.Context() rather than ctx, which is cancelled — the allocator would refuse
	// both calls and the test would fail without proving anything.
	time.Sleep(2 * ttl)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if err := a.Heartbeat(t.Context(), lease.ID, lease.Epoch); err != nil {
		t.Fatalf("a lease was lost while the shutdown release was still destroying its "+
			"compute (%v); the reaper freed capacity for a container that is still on "+
			"the host, so another tier can escrow it", err)
	}

	unblock()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned after the node answered")
	}
}

// THE SHUTDOWN DESTROYS EACH REQUEST ONCE, NOT ONCE PER SET IT APPEARS IN.
//
// A completion whose destroy failed keeps its lease in `running` AND has a
// cleanup record, so the two sets overlap. Draining the records and then letting
// releaseAll destroy the running jobs meant a second remote round trip for every
// job in both — against the same shutdown grace, which is exactly what the grace
// was too small for.
func TestShutdownDestroysEachRequestOnce(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var attempts atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		attempts.Add(1)

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner), WithDrainGrace(notDrainingHere))

	// In BOTH sets: a running lease and a cleanup record for the same request,
	// which is the ordinary state after a completion whose destroy failed.
	holdRunning(t, l, a, tiers[0].Label, 7)

	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{
		7: {job: Job{RequestID: 7}, at: time.Now().Add(time.Hour)},
	}
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	if got := attempts.Load(); got != 1 {
		t.Errorf("shutdown destroyed request 7 %d times, want 1; each extra call is "+
			"another remote round trip against the same shutdown grace", got)
	}

	if cleanupCount(l) != 0 {
		t.Errorf("a discharged cleanup record survived the shutdown: %d", cleanupCount(l))
	}
}

// A NONSENSE BUDGET STOPS THE LISTENER RATHER THAN BEING CORRECTED.
//
// The first version of this substituted the default and carried on, on the
// reasoning that zero is what a caller passes by leaving a field unset. It is
// not — omitting the option already selects the default, so passing zero is an
// explicit instruction. Running for twelve quiet minutes instead of the -1s
// somebody's arithmetic produced is the same misconfiguration with the evidence
// removed, and these budgets decide when billet stops protecting capacity whose
// compute may still be running.
//
// The ceiling is the same argument from the other end: durations near MaxInt64
// sum to a NEGATIVE one, and saturating instead gives a watchdog that fires in
// about 292 years, which is not a watchdog.
func TestANonsenseBudgetRefusesToRun(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	for _, tc := range []struct {
		name string
		opt  Option
	}{
		{"zero shutdown grace", WithShutdownGrace(0)},
		{"negative shutdown grace", WithShutdownGrace(-time.Second)},
		{"absurd shutdown grace", WithShutdownGrace(time.Duration(1) << 62)},
		{"zero close grace", WithFinishGraces(0, time.Second)},
		{"negative release grace", WithFinishGraces(time.Second, -time.Second)},
		{"absurd release grace", WithFinishGraces(time.Second, time.Duration(1)<<62)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}), tc.opt)

			// SHORT, so a listener that does not refuse is caught by what it
			// returns rather than by how long this takes.
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()

			err := l.Run(ctx)
			if err == nil {
				t.Fatal("the listener started with a budget that cannot mean what it says")
			}

			// THE REASON, not merely an error. Asserting "some error" passed
			// against a listener that started happily and returned
			// DeadlineExceeded when the test's own context ran out — so every
			// silently tolerated misconfiguration looked like a refusal.
			if !strings.Contains(err.Error(), "misconfigured") {
				t.Fatalf("Run failed with %v; want a refusal naming the misconfiguration, "+
					"because anything else means it started and stopped for another reason",
					err)
			}
		})
	}

	// AND A SANE ONE STILL RUNS. Matching on a word cannot tell "refused for this
	// reason" from "refuses everything": a Run that returned a misconfiguration
	// error unconditionally would satisfy every case above. This half is what
	// makes those cases mean anything.
	session := &fakeSession{}

	l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{}),
		WithShutdownGrace(time.Second), WithFinishGraces(2*time.Second, 3*time.Second))

	if err := l.configError(); err != nil {
		t.Fatalf("a sane configuration was refused: %v", err)
	}

	polled := make(chan struct{})

	session.onPoll = func(int) {
		select {
		case polled <- struct{}{}:
		default:
		}
	}

	runCtx, stop := context.WithTimeout(t.Context(), 30*time.Second)

	t.Cleanup(stop)

	ran := make(chan error, 1)

	go func() { ran <- l.Run(runCtx) }()

	select {
	case <-polled:
	case err := <-ran:
		t.Fatalf("a sanely configured listener refused to start: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("a sanely configured listener never polled")
	}

	stop()
	<-ran

	// AND A REFUSED ONE NEVER TOUCHED ITS SESSION, so the refusal happened before
	// anything was advertised to GitHub.
	refused := &fakeSession{}
	bad := NewListener(a, tiers[0].Label, refused, WithRunner(&fakeRunner{}),
		WithShutdownGrace(0))

	if err := bad.Run(t.Context()); err == nil {
		t.Fatal("a listener with a zero grace started")
	}

	if refused.polls() != 0 {
		t.Errorf("a refused listener polled %d times; the refusal has to happen before "+
			"it advertises anything", refused.polls())
	}

	// Two destroy-length phases — the cleanup-loop join and the destroy pass —
	// then the close and the release.
	if got := l.teardownBudget(); got != 7*time.Second {
		t.Errorf("teardown budget is %v, want 7s (1s join + 1s destroy + 2s close + 3s "+
			"release); a budget that ignores its configuration bounds nothing an "+
			"operator asked for", got)
	}
}

// A LATER OPTION REPLACES AN EARLIER REFUSAL, because that is how every other
// option behaves.
//
// Validation errors were append-only while values were last-wins, so a layered
// configuration that set a bad default and then corrected it produced a listener
// refusing to start with the CORRECT value in place. Nothing about that is
// safer; it just makes the override unusable.
func TestALaterOptionReplacesAnEarlierRefusal(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	corrected := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}),
		WithShutdownGrace(0),
		WithShutdownGrace(30*time.Second))

	if err := corrected.configError(); err != nil {
		t.Errorf("a corrected option left the listener refusing to start: %v; a default "+
			"that a later option overrides is the ordinary way to layer configuration", err)
	}

	if corrected.shutdownGrace != 30*time.Second {
		t.Errorf("shutdown grace is %v, want 30s", corrected.shutdownGrace)
	}

	// AND THE OTHER ORDER STILL REFUSES. Replacing must not mean forgetting.
	spoiled := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}),
		WithShutdownGrace(30*time.Second),
		WithShutdownGrace(0))

	if err := spoiled.configError(); err == nil {
		t.Error("a valid value followed by a nonsense one was accepted; last-wins has to " +
			"apply to the refusal as well as the value")
	}

	// THE PAIRED OPTION TOO, which sets two fields at once. Testing only the
	// single-field option left room for a regression that kept append-only
	// behaviour in exactly the place where two errors can be live at the same
	// time — and each call has to clear the field it fixes without touching the
	// other's verdict.
	crossed := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}),
		WithFinishGraces(0, 5*time.Second),
		WithFinishGraces(5*time.Second, 0))

	err := crossed.configError()
	if err == nil {
		t.Fatal("a nonsense release grace was accepted because an earlier call had set a " +
			"good one")
	}

	if strings.Contains(err.Error(), "close grace") {
		t.Errorf("the corrected close grace is still being reported: %v", err)
	}

	if !strings.Contains(err.Error(), "release grace") {
		t.Errorf("the broken release grace is not being reported: %v", err)
	}

	// And correcting both clears both.
	fixed := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}),
		WithFinishGraces(0, 0),
		WithFinishGraces(5*time.Second, 6*time.Second))

	if err := fixed.configError(); err != nil {
		t.Errorf("correcting both graces left the listener refusing to start: %v", err)
	}

	if fixed.closeGrace != 5*time.Second || fixed.releaseGrace != 6*time.Second {
		t.Errorf("corrected graces are %v/%v, want 5s/6s",
			fixed.closeGrace, fixed.releaseGrace)
	}
}

// THE BUDGET SUM STILL CANNOT WRAP, even though the ceiling now makes it
// unreachable.
//
// Validation is what stops an absurd budget arriving, and this is the second
// line: nothing in the type system stops a future phase being added with a large
// constant, and int64 overflow turns the longest configuration into an
// already-expired deadline — the least cautious possible behaviour from the most
// cautious possible input.
func TestTheBudgetSumSaturatesRatherThanWrapping(t *testing.T) {
	t.Parallel()

	const huge = time.Duration(1) << 62

	if got := sumBudgets(huge, huge, huge, huge); got != time.Duration(math.MaxInt64) {
		t.Errorf("four large budgets summed to %v, want saturation at MaxInt64; anything "+
			"negative is a deadline that has already passed", got)
	}

	if got := sumBudgets(time.Second, 2*time.Second, 3*time.Second); got != 6*time.Second {
		t.Errorf("sumBudgets(1s, 2s, 3s) = %v, want 6s", got)
	}
}

// A PHASE THAT OVERRUNS DOES NOT PUSH THE NEXT ONE PAST THE WHOLE BUDGET.
//
// Renewal is bounded by the sum of the four phase budgets, so no phase may end
// after that sum — otherwise the close or the release runs with nothing renewing
// and leases expire while the session is still open. Giving each phase its own
// fresh budget did not establish that: the phases run in sequence, so a destroy
// that IGNORES its context and overruns leaves the close starting late with a
// full closeGrace still ahead of it, ending after the sum.
//
// Deriving every phase from one overall deadline makes each of them min(its own
// budget, what is left), which is the property this checks directly — what
// deadline the close was actually handed, not merely how long it took.
func TestAnOverrunningPhaseCannotPushTheNextPastTheBudget(t *testing.T) {
	t.Parallel()

	const (
		destroying = 300 * time.Millisecond
		overrun    = 2 * time.Second
		closing    = 10 * time.Second
		releasing  = 300 * time.Millisecond
	)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	// A destroy that ignores its context and overruns the destroy budget, which
	// nothing in the Runner contract forbids.
	runner := &fakeRunner{onDestroy: func(int64) error {
		time.Sleep(overrun)

		return nil
	}}

	deadlines := make(chan time.Time, 1)

	session := &fakeSession{onClose: func(ctx context.Context) error {
		d, ok := ctx.Deadline()
		if !ok {
			d = time.Time{}
		}

		select {
		case deadlines <- d:
		default:
		}

		return nil
	}}

	l := NewListener(a, tiers[0].Label, session, WithRunner(runner),
		WithShutdownGrace(destroying), WithFinishGraces(closing, releasing),
		WithDrainGrace(notDrainingHere))

	holdRunning(t, l, a, tiers[0].Label, 7)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	// The budget starts when the TEARDOWN does, not when Run does. Measuring from
	// before the run makes the bound tighter than the code ever promised, and
	// fails it by the microseconds between the two.
	cancel()

	teardown := time.Now()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	var closeDeadline time.Time

	select {
	case closeDeadline = <-deadlines:
	default:
		t.Fatal("the shutdown never closed the session")
	}

	if closeDeadline.IsZero() {
		t.Fatal("the session close was handed a context with no deadline at all")
	}

	// Slack for the gap between cancelling and the teardown actually beginning.
	// Small next to what this is distinguishing: without the overall deadline the
	// close gets a fresh ten seconds against roughly eight that remain, so the
	// overshoot to catch is well over a second.
	const slack = 500 * time.Millisecond

	budget := l.teardownBudget()

	if over := closeDeadline.Sub(teardown.Add(budget)); over > slack {
		t.Errorf("the session close was given until %v past the whole shutdown budget; "+
			"renewal stops at that budget, so the close and the release run with "+
			"nothing renewing and leases expire while the session is still open", over)
	}
}

// A BLOCKED REQUEST IS REPORTED ONCE, NOT ON EVERY OFFER.
//
// GitHub re-offers a job nobody acquires for as long as it stays queued, so a
// warning per offer turns one stuck obligation into a line every poll for as
// long as the node stays away — and an operator who learns to scroll past a
// message stops reading it. Once per obligation, and once more if the same
// request gets into the same state again.
func TestABlockedRequestIsReportedOnce(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var lines countingHandler

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}),
		WithLogger(slog.New(&lines)))

	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("refillEscrow: %v", err)
	}

	owe := func() {
		l.mu.Lock()
		l.cleanup = map[int64]*pendingCleanup{7: {job: Job{RequestID: 7}}}
		l.mu.Unlock()
	}

	owe()

	for range 5 {
		if got := l.reserve([]Job{{RequestID: 7}}); len(got) != 0 {
			t.Fatalf("reserved %v for a request whose container is still owed", got)
		}
	}

	if got := lines.count("previous run"); got != 1 {
		t.Errorf("five offers of one blocked request produced %d warnings, want 1; GitHub "+
			"re-offers for as long as the job is queued, so this repeats every poll for "+
			"as long as the node is away", got)
	}

	// AND EACH REQUEST GETS ITS OWN. Suppressing per LISTENER would satisfy
	// everything above while silencing every request after the first, which is the
	// version of this that hides an outage behind one line about an unrelated job.
	l.mu.Lock()
	l.cleanup[9] = &pendingCleanup{job: Job{RequestID: 9}}
	l.mu.Unlock()

	if got := l.reserve([]Job{{RequestID: 9}}); len(got) != 0 {
		t.Fatalf("reserved %v for a second blocked request", got)
	}

	if got := lines.count("previous run"); got != 2 {
		t.Errorf("two blocked requests produced %d warnings, want 2; suppressing per "+
			"listener rather than per obligation hides every request after the first", got)
	}

	// A NEW OBLIGATION IS NEW NEWS. Suppressing per entry rather than per request
	// is what makes that true, and silence here would be the opposite failure.
	l.mu.Lock()
	delete(l.cleanup, 7)
	delete(l.cleanup, 9)
	l.mu.Unlock()

	owe()

	if got := l.reserve([]Job{{RequestID: 7}}); len(got) != 0 {
		t.Fatalf("reserved %v for a freshly owed request", got)
	}

	if got := lines.count("previous run"); got != 3 {
		t.Errorf("a third, separate obligation produced %d warnings in total, want 3; "+
			"suppressing it would hide a request that had started failing again", got)
	}
}

// countingHandler counts log records whose message contains a substring.
type countingHandler struct {
	mu       sync.Mutex
	messages []string
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.messages = append(h.messages, r.Message)

	return nil
}

func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *countingHandler) WithGroup(string) slog.Handler      { return h }

func (h *countingHandler) count(substr string) int {
	h.mu.Lock()
	defer h.mu.Unlock()

	n := 0

	for _, m := range h.messages {
		if strings.Contains(m, substr) {
			n++
		}
	}

	return n
}

// A STUCK CLEANUP RETRY DOES NOT STARVE THE SHUTDOWN'S OWN DESTROYS.
//
// Waiting for the cleanup loop to return is waiting for a Destroy, so it can
// take exactly as long as a destroy can — and it shared the destroy phase's
// budget with the destroys themselves. One retry that stalled for the whole
// grace left destroyAll starting with an already-dead context: every remaining
// request reported as never attempted, its container still running and its lease
// left for the reaper. That is the close starving the release again, one phase
// further up, which is why the join is now its own phase.
func TestAStuckCleanupRetryDoesNotStarveTheDestroys(t *testing.T) {
	t.Parallel()

	const (
		ttl   = 300 * time.Millisecond
		grace = 300 * time.Millisecond
	)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	blocked := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(blocked) })

	t.Cleanup(unblock)

	entered := make(chan struct{}, 1)

	var (
		announced atomic.Bool
		destroyed atomic.Bool
		reissued  atomic.Int32
	)

	// Request 7 BLOCKS ON EVERY CALL, which is the point. The first version let
	// it through on the second, so the shutdown could re-issue the same destroy
	// and the test would not notice: with only one teardown slot, that second
	// attempt wins it and spends the whole destroy budget on work already in
	// flight, and request 9 — the unrelated job this is really about — is never
	// reached. Blocking throughout means the only way 9 gets destroyed is if the
	// shutdown declines to re-issue 7 at all.
	runner := &fakeRunner{onDestroy: func(id int64) error {
		if id == 7 {
			reissued.Add(1)

			if announced.CompareAndSwap(false, true) {
				entered <- struct{}{}
			}

			<-blocked

			return nil
		}

		if id == 9 {
			destroyed.Store(true)
		}

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithShutdownGrace(grace), WithFinishGraces(time.Second, time.Second),
		WithDrainGrace(notDrainingHere))

	holdRunning(t, l, a, tiers[0].Label, 9)

	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{7: {job: Job{RequestID: 7}}}
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	select {
	case <-entered:
	case <-runDone:
		t.Fatal("the listener stopped before its cleanup loop reached the runner")
	case <-time.After(30 * time.Second):
		t.Fatal("the cleanup retry never reached the runner")
	}

	// The retry is stuck. Shutting down now makes the join wait for it.
	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	if !destroyed.Load() {
		t.Error("the shutdown never tried to destroy the running job's compute: one stuck " +
			"cleanup retry consumed the destroy budget before the destroy pass began, so " +
			"that container is still on its host and its lease waits for the reaper")
	}

	// ORDER-INDEPENDENT, which the assertion above is not. With the skip removed
	// the destroy pass holds both requests and map order decides which takes the
	// single slot first, so request 9 still got destroyed about half the time —
	// the mutant survived on a coin toss. Request 7's call count does not depend
	// on that: the cleanup loop's attempt is the only one there should ever be.
	if got := reissued.Load(); got != 1 {
		t.Errorf("request 7 was handed to the runner %d times, want 1; the shutdown "+
			"re-issued a destroy that a retry was already inside, and with one slot "+
			"that second attempt spends the budget on work already in flight", got)
	}
}

// A RETRY THAT HAS FINISHED DOES NOT BLOCK THE SHUTDOWN'S DESTROY.
//
// Skipping requests a cleanup retry is already inside needs the mark CLEARED
// when that retry returns. Leaving it set is the quiet half of the bug it fixes:
// the shutdown would decline to destroy a request on the grounds that something
// else was handling it, when nothing was, and the container would outlive the
// process with nobody having said so.
func TestAFinishedRetryDoesNotBlockTheShutdownDestroy(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var destroys atomic.Int32

	// Never succeeds, so the record survives its retry and is still owed at
	// shutdown — which is exactly when the stale mark would hide it.
	runner := &fakeRunner{onDestroy: func(int64) error {
		destroys.Add(1)

		return errors.New("the node is not answering")
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithCleanupRetryPacing(0, 0),
		WithDrainGrace(notDrainingHere))

	holdRunning(t, l, a, tiers[0].Label, 7)

	if err := l.complete(t.Context(), Job{RequestID: 7}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	// A retry that runs to completion and leaves the obligation in place.
	l.retryCleanup(t.Context())

	before := destroys.Load()
	if before < 2 {
		t.Fatalf("the retry never reached the runner: %d destroys", before)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	if destroys.Load() <= before {
		t.Error("the shutdown skipped a request whose cleanup retry had already finished: " +
			"the in-flight mark outlived the attempt it stood for, so billet declined to " +
			"destroy that container on the grounds that something else was doing it")
	}
}

// A DESTROY THAT OUTRUNS THE WHOLE BUDGET STOPS THE TEARDOWN.
//
// Nothing obliges a Runner to honour its context, so a destroy can return only
// after the overall deadline has passed. Announcing that the budget is gone and
// then calling Close anyway is a contradiction: Session does not promise to
// honour a context either, so a phase just declared hopeless could block
// indefinitely — past the deadline announced as final.
func TestADestroyThatOutrunsTheBudgetStopsTheTeardown(t *testing.T) {
	t.Parallel()

	const (
		destroying = 200 * time.Millisecond
		finishing  = 200 * time.Millisecond
	)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var destroys atomic.Int32

	// Ignores its context and outlasts join + destroy + close + release together.
	runner := &fakeRunner{onDestroy: func(int64) error {
		destroys.Add(1)

		time.Sleep(2 * (2*destroying + 2*finishing))

		return nil
	}}

	session := &fakeSession{}

	l := NewListener(a, tiers[0].Label, session, WithRunner(runner),
		WithShutdownGrace(destroying), WithFinishGraces(finishing, finishing),
		WithDrainGrace(notDrainingHere))

	holdRunning(t, l, a, tiers[0].Label, 7)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})
	started := time.Now()

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	// THE PREMISE FIRST. A negative assertion passes for any reason the teardown
	// ended early, including the session close being removed entirely — so the
	// destroy has to be shown to have run and outlasted the budget.
	if destroys.Load() != 1 {
		t.Fatalf("the destroy ran %d times; this test means nothing unless it ran once "+
			"and outlasted the budget", destroys.Load())
	}

	if elapsed := time.Since(started); elapsed <= l.teardownBudget() {
		t.Fatalf("the teardown finished in %v, inside its %v budget; nothing overran, so "+
			"the check being tested was never reached", elapsed, l.teardownBudget())
	}

	if session.closes() != 0 {
		t.Errorf("the session was closed %d times after the whole shutdown budget had "+
			"already gone; a phase billet has just declared hopeless must not be started, "+
			"because nothing promises it will return", session.closes())
	}
}

// A SEALED CLEANUP LOOP STARTS NOTHING NEW, even mid-snapshot.
//
// retryCleanup walks a snapshot of due jobs, and cancellation does not stop it
// between items. So a loop that was inside a slow destroy for request 7 when
// shutdown began could finish it, move on, and start a BRAND NEW destroy for
// request 9 — which the teardown was destroying at that same moment. Two
// broadcasts to every node for one request, outside the single teardown slot,
// and possibly after Run had returned.
//
// Sealing is a fact settled under the mutex the shutdown snapshot reads;
// cancelling is only a request.
func TestASealedCleanupLoopStartsNothingNew(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var attempts atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		attempts.Add(1)

		return errors.New("the node is not answering")
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithCleanupRetryPacing(0, 0))

	// Two obligations, so a snapshot has somewhere to advance TO.
	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{
		7: {job: Job{RequestID: 7}},
		9: {job: Job{RequestID: 9}},
	}
	l.mu.Unlock()

	// The teardown has claimed them. Nothing the loop does from here may reach
	// the runner.
	l.seal()

	l.retryCleanup(t.Context())

	if got := attempts.Load(); got != 0 {
		t.Errorf("a sealed cleanup loop reached the runner %d times; the shutdown has "+
			"already decided which requests are its own, and a second broadcast for one "+
			"of them runs outside the single teardown slot", got)
	}

	// AND THE OBLIGATIONS SURVIVE. Sealing stops the loop; it does not discharge
	// anything, and the teardown still owes these containers a destroy.
	if cleanupCount(l) != 2 {
		t.Errorf("sealing dropped obligations: %d pending, want 2", cleanupCount(l))
	}
}

// AND THE SHUTDOWN ACTUALLY SEALS, which is a different property from sealing
// working.
//
// The test above calls seal() itself, so it passes against a Run that never
// does — the same gap that once let `go l.cleanupLoop` be deleted with a green
// suite. Here the loop is genuinely mid-snapshot when shutdown begins: it blocks
// inside the first destroy, the join gives up, the teardown destroys the other
// request, and only then does the first destroy return. An unsealed loop
// advances to the second request and destroys it again — after Run has returned,
// outside the teardown's single slot.
func TestRunSealsTheCleanupLoop(t *testing.T) {
	t.Parallel()

	const (
		// Roomy, because the teardown must get through its own destroy pass inside
		// this budget. At 200ms a scheduling hiccup after the join expired left
		// nothing for the destroy, and the test failed on correct code.
		grace = time.Second
		// The cleanup loop ticks at TTL/3, so the DEFAULT 90 second TTL means its
		// first pass is 30 seconds away. This test waited 30 seconds for that pass
		// and passed in isolation by about three seconds — then failed under the
		// full suite, which is the definition of a test that will flake in CI.
		ttl = 300 * time.Millisecond
	)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	blocked := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(blocked) })

	t.Cleanup(unblock)

	entered := make(chan struct{}, 1)

	var (
		destroys  atomic.Int32
		announced atomic.Bool
	)

	// resumed says the loop's blocked destroy has actually returned, which is the
	// causal anchor the final assertion needs.
	resumed := make(chan struct{})

	// Only the loop's FIRST destroy blocks; everything else returns at once, so
	// what distinguishes the behaviours is a count rather than a hang.
	runner := &fakeRunner{onDestroy: func(int64) error {
		destroys.Add(1)

		if announced.CompareAndSwap(false, true) {
			entered <- struct{}{}

			<-blocked
			close(resumed)
		}

		return errors.New("the node is not answering")
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithShutdownGrace(grace), WithFinishGraces(grace, grace),
		WithCleanupRetryPacing(0, 0))

	// Two obligations, so the loop's snapshot has somewhere to advance to.
	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{
		7: {job: Job{RequestID: 7}},
		9: {job: Job{RequestID: 9}},
	}
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	select {
	case <-entered:
	case <-runDone:
		t.Fatal("the listener stopped before its cleanup loop reached the runner")
	case <-time.After(30 * time.Second):
		t.Fatal("the cleanup retry never reached the runner")
	}

	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	// The loop's first destroy is still blocked and the teardown has been and
	// gone: its own call, plus the teardown's for the other request.
	afterRun := destroys.Load()
	if afterRun != 2 {
		t.Fatalf("%d destroys before releasing the stuck one, want 2 (the loop's and the "+
			"teardown's); the setup is not what this test describes", afterRun)
	}

	unblock()

	// WAIT FOR THE LOOP TO BE RUNNING AGAIN before judging what it does next.
	// Without this anchor, "no further destroys after a fixed sleep" also passes
	// when the orphaned goroutine simply never got scheduled — and the no-seal
	// mutation survives on that.
	select {
	case <-resumed:
	case <-time.After(30 * time.Second):
		t.Fatal("the blocked destroy never returned")
	}

	// From here an unsealed loop has one map lookup and a Destroy left, so this
	// waits for something that would happen immediately rather than guessing how
	// long a goroutine might take to be scheduled at all.
	time.Sleep(grace)

	if got := destroys.Load(); got != afterRun {
		t.Errorf("the cleanup loop issued %d more destroy(s) after Run returned; nothing "+
			"sealed it, so it walked on through a snapshot the teardown had already "+
			"claimed — outside the single slot, with no process left to own it",
			got-afterRun)
	}
}

// A LISTENER IS SINGLE USE, and says so rather than misbehaving quietly.
//
// Shutdown seals the cleanup loop permanently and closes the session, so a
// second Run gets a listener polling a closed session whose retries all return
// "sealed" — which retryCleanup reads as success, so it neither retries nor
// backs off nor complains. Every completion whose destroy fails after that is
// silently abandoned.
//
// Server builds a fresh listener per tier run, so nothing in production does
// this. A TEST did, with a comment claiming it was restarting the way the
// control plane restarts, which is exactly the misunderstanding an exported Run
// with no guard invites.
func TestAListenerIsSingleUse(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}))

	first, stop := context.WithTimeout(t.Context(), 30*time.Second)

	t.Cleanup(stop)

	go func() {
		time.Sleep(50 * time.Millisecond)
		stop()
	}()

	if err := l.Run(first); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("first run: %v", err)
	}

	err := l.Run(t.Context())
	if err == nil {
		t.Fatal("a listener that had already shut down ran again; its cleanup loop is " +
			"sealed and its session closed, so every failed destroy from here is " +
			"abandoned without a word")
	}

	if !strings.Contains(err.Error(), "already run") {
		t.Errorf("second run failed with %v; want a refusal saying so, because any other "+
			"error means it started and stopped for an unrelated reason", err)
	}
}

// A RETRY THAT PANICS STILL RELEASES ITS IN-FLIGHT MARK.
//
// The mark makes the shutdown skip a request on the grounds that a retry is
// already destroying it. An entry that outlives its attempt therefore hides that
// request from teardown permanently — a container nobody destroys and nobody
// mentions, which is strictly worse than the double-destroy the mark exists to
// prevent. Unmarking after the call rather than in a defer left that to luck:
// any panic under complete, in the runner or the allocator, and the mark stays.
func TestAPanickingRetryStillReleasesItsMark(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	runner := &fakeRunner{onDestroy: func(int64) error {
		panic("the provider exploded")
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

	holdRunning(t, l, a, tiers[0].Label, 7)

	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{7: {job: Job{RequestID: 7}}}
	l.mu.Unlock()

	// THROUGH retryCleanup, which is what the cleanup loop calls. Driving
	// `attempt` directly proved the method was exception-safe and nothing about
	// the path that uses it: a version that inlined the old mark/complete/unmark
	// sequence back into retryCleanup, never calling attempt at all, left that
	// test green.
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the runner did not panic, so this proves nothing")
			}
		}()

		l.retryCleanup(t.Context())
	}()

	l.mu.Lock()
	inFlight := len(l.destroying)
	l.mu.Unlock()

	if inFlight != 0 {
		t.Errorf("%d request(s) are still marked as being destroyed after the attempt "+
			"blew up; the shutdown will skip them believing a retry has them, and their "+
			"containers outlive the process with nothing said", inFlight)
	}
}

// A SLOW DESTROY DOES NOT COST THE RELEASE ITS BUDGET.
//
// Closing the session and releasing leases are fast and local, but they used to
// share the destroy pass's deadline. A destroy that outlasted the grace then
// handed the close an already-expired context; it failed, the early return
// skipped releaseAll, and leases whose compute had been destroyed SUCCESSFULLY
// were left for the reaper anyway. The expensive half failing must not throw
// away the cheap half's work.
func TestASlowDestroyDoesNotCostTheReleaseItsBudget(t *testing.T) {
	t.Parallel()

	const grace = 200 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	// Slower than the grace, and it IGNORES the context — nothing in the Runner
	// contract says it must not, and that is precisely when the grace matters.
	// It succeeds, so its lease is owed a release.
	runner := &fakeRunner{onDestroy: func(int64) error {
		time.Sleep(2 * grace)

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithShutdownGrace(grace),
		WithDrainGrace(notDrainingHere))

	holdRunning(t, l, a, tiers[0].Label, 7)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	// EVERY LEASE BACK. The escrowed ones and the running one whose destroy
	// succeeded: headroom returning to its full two is the allocator agreeing
	// nothing is still held.
	room, err := a.Headroom(t.Context(), tiers[0].Label)
	if err != nil {
		t.Fatalf("headroom: %v", err)
	}

	if room != 2 {
		t.Errorf("%d of 2 slots came back after shutdown; a destroy that outlasted the "+
			"grace left the session close and the release with an expired context, so "+
			"capacity billet had already freed stays held until the reaper", room)
	}
}

// A SLOW CLOSE DOES NOT COST THE RELEASE ITS BUDGET EITHER.
//
// Giving the local half its own budget fixed the remote half starving it and
// left a smaller version of the same bug inside: the session close and the
// releases shared that budget, so a close that used nearly all of it left the
// releases — sequential, against one SQLite writer — to fail on the remainder.
// A clean shutdown could then withhold the whole tier's capacity until the
// reaper. Each phase gets its own.
func TestASlowCloseDoesNotCostTheReleaseItsBudget(t *testing.T) {
	t.Parallel()

	const closing = 200 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	// A close that overruns its budget and then succeeds anyway — nothing in the
	// Session contract says it must honour the deadline, and that is exactly when
	// a shared budget hurts.
	session := &fakeSession{onClose: func(context.Context) error {
		time.Sleep(2 * closing)

		return nil
	}}

	l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{}),
		WithFinishGraces(closing, 10*time.Second),
		WithDrainGrace(notDrainingHere))

	holdRunning(t, l, a, tiers[0].Label, 7)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	room, err := a.Headroom(t.Context(), tiers[0].Label)
	if err != nil {
		t.Fatalf("headroom: %v", err)
	}

	if room != 2 {
		t.Errorf("%d of 2 slots came back after a slow session close; the releases were "+
			"left with what the close had not spent, so an otherwise clean shutdown "+
			"withholds capacity until the reaper", room)
	}
}

// A REQUEST WHOSE LAST CONTAINER IS STILL OWED IS NOT TAKEN AGAIN.
//
// Cleanup is addressed BY REQUEST ID, because that is all a completion gives us.
// So a request id with an undischarged container is not a free id: if GitHub
// re-offers it and this listener accepts, the old retry gains the power to
// destroy the NEW job's container and release the NEW job's lease. Request id
// alone cannot tell two incarnations apart, so the id stays occupied until the
// first one is discharged.
//
// This became reachable when a lost running lease started leaving a cleanup
// record behind — before that, a request in cleanup was always also in running,
// which already blocked reassignment.
func TestARequestWithComputeStillOwedIsNotTakenAgain(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}))

	if err := l.refillEscrow(t.Context()); err != nil {
		t.Fatalf("refillEscrow: %v", err)
	}

	// The state a lost running lease leaves: no lease, but a container this
	// listener still has to destroy.
	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{7: {job: Job{RequestID: 7}}}
	l.mu.Unlock()

	if got := l.reserve([]Job{{RequestID: 7}}); len(got) != 0 {
		t.Errorf("reserved %v for a request whose previous container is still owed; the "+
			"pending retry would destroy the new job's compute and release its lease", got)
	}

	lease, ok, err := l.assign(t.Context(), Job{RequestID: 7})
	if err != nil {
		t.Fatalf("assign: %v", err)
	}

	if ok || lease != nil {
		t.Error("assigned a request whose previous container is still owed; two runs now " +
			"share one request id, and the older one's cleanup can destroy the newer")
	}

	// AND IT IS TAKEN AGAIN ONCE THE OBLIGATION IS DISCHARGED. Blocking forever
	// would be its own bug — the id has to come back.
	l.mu.Lock()
	delete(l.cleanup, 7)
	l.mu.Unlock()

	if got := l.reserve([]Job{{RequestID: 7}}); len(got) != 1 {
		t.Errorf("reserved %v after the obligation was discharged, want one id; the "+
			"request would never be runnable again", got)
	}
}

// LOSING A RUNNING LEASE LEAVES THE CONTAINER OWED.
//
// The heartbeat used to delete a running job outright when its lease could not
// be renewed. That is right about the CAPACITY — fenced, it belongs to another
// holder — and wrong about the compute: this listener launched a container, it
// is still running, GitHub will not send the completion again, and with a runner
// that cannot sweep nothing else would ever remove it. The record moves to the
// cleanup set instead, where only a successful destroy discharges it.
func TestARunningLeaseLostLeavesItsComputeOwed(t *testing.T) {
	t.Parallel()

	const ttl = 300 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}))

	holdRunning(t, l, a, tiers[0].Label, 7)

	// Reaped, so the heartbeat sees the fence a real reap produces.
	time.Sleep(2 * ttl)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}

	l.mu.Lock()
	l.heartbeatHeld(t.Context())
	_, stillRunning := l.running[7]
	l.mu.Unlock()

	if stillRunning {
		t.Fatal("the listener kept a lease the reaper had taken, so this proves nothing")
	}

	if cleanupCount(l) != 1 {
		t.Fatalf("a running job whose lease was reaped left no cleanup obligation (%d "+
			"pending); its container is still on a host, GitHub will not redeliver the "+
			"completion, and only an optional Sweeper would ever notice", cleanupCount(l))
	}

	// THE RIGHT REQUEST. Counting the map only proves something was recorded: an
	// empty pendingCleanup{} satisfies a count and then destroys request 0, which
	// is nobody's container.
	l.mu.Lock()
	entry, recorded := l.cleanup[7]
	l.mu.Unlock()

	if !recorded {
		t.Fatal("a cleanup obligation was recorded under some other request id")
	}

	if entry.job.RequestID != 7 {
		t.Errorf("the obligation names request %d, want 7; a destroy addressed to the "+
			"wrong id leaves the real container running", entry.job.RequestID)
	}

	// AND THE SAME FOR A STALE LEASE, which is a different branch: `lost` is the
	// allocator saying the lease is not ours, `stale` is the allocator saying
	// nothing for longer than the lease could survive. Both end the claim; neither
	// ends the obligation.
	second := holdRunning(t, l, a, tiers[0].Label, 9)

	l.mu.Lock()
	l.confirmed[second.ID] = time.Now().Add(-2 * ttl)
	l.mu.Unlock()

	unreachable, cancel := context.WithCancel(t.Context())
	cancel()

	l.mu.Lock()
	l.heartbeatHeld(unreachable)
	_, stillRunningStale := l.running[9]
	staleEntry, owed := l.cleanup[9]
	l.mu.Unlock()

	if stillRunningStale {
		t.Fatal("a lease unconfirmed for longer than its TTL was still being advertised")
	}

	if !owed {
		t.Fatal("a running job dropped for staleness left no cleanup obligation; the " +
			"allocator never said the lease was lost, so there is even less reason to " +
			"assume the container is gone")
	}

	// The same payload check as the lost branch. Asserting only that the key
	// exists let an empty pendingCleanup{} through, whose retry then destroys
	// request 0 and leaves the real container running.
	if staleEntry.job.RequestID != 9 {
		t.Errorf("the stale branch's obligation names request %d, want 9",
			staleEntry.job.RequestID)
	}
}

// UNCERTAINTY IS NOT ALLOWED TO OUTLAST THE LEASE.
//
// A renewal that cannot reach the allocator says nothing about ownership, so the
// lease is kept — but only up to a point. Past the TTL with no confirmed
// renewal, the reaper can have taken it, and "no evidence it is lost" has become
// a reason to advertise capacity that is gone.
func TestALeaseUnconfirmedForLongerThanItsTTLIsDropped(t *testing.T) {
	t.Parallel()

	const ttl = 200 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}))

	holdRunning(t, l, a, tiers[0].Label, 7)

	// One good pass, so there is a confirmed renewal to measure from.
	l.mu.Lock()
	l.heartbeatHeld(t.Context())
	l.mu.Unlock()

	if _, ok := l.running[7]; !ok {
		t.Fatal("a healthy renewal dropped the lease")
	}

	// Now nothing gets through. A cancelled context is the cheapest stand-in for
	// an allocator that cannot be reached.
	unreachable, cancel := context.WithCancel(t.Context())
	cancel()

	l.mu.Lock()
	l.heartbeatHeld(unreachable)
	_, keptAtOnce := l.running[7]
	l.mu.Unlock()

	if !keptAtOnce {
		t.Fatal("a single unreachable renewal dropped the lease; a failure to ask is not " +
			"an answer, and dropping it removes the lease from the release path too")
	}

	time.Sleep(2 * ttl)

	l.mu.Lock()
	l.heartbeatHeld(unreachable)
	_, keptTooLong := l.running[7]
	l.mu.Unlock()

	if keptTooLong {
		t.Error("a lease went unconfirmed for longer than its TTL and was still being " +
			"advertised; the reaper can have reclaimed it by now, so the capacity may " +
			"already belong to another tier")
	}
}

// THE UNCERTAINTY CLOCK STARTS AT ESCROW, NOT AT THE FIRST FAILED RENEWAL.
//
// A lease that has NEVER been confirmed is the worst case, not an exempt one:
// escrowed at t=0 it expires at t=TTL whatever happens. Starting its clock at
// the first failure — around t=TTL/3 — bought it an extra TTL it had not earned
// and kept it advertised well past the point the reaper could have taken it.
//
// The distinguishing test is that no successful renewal ever happens here. The
// test above performs one first, so it cannot see this.
func TestALeaseNeverConfirmedIsStillBoundedByItsTTL(t *testing.T) {
	t.Parallel()

	const ttl = 300 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}))

	// ESCROWED WHILE THE MUTEX IS CONTENDED, which is the whole point.
	//
	// Without contention this test passed against a version that stamped the
	// clock AFTER taking l.mu — the lease was created and stamped in the same
	// instant, so the two were indistinguishable. A heartbeat pass holds l.mu for
	// up to its own deadline, so in production the gap is real: stamping after the
	// wait gave a lease created at t=0 a clock reading t=TTL/3 and kept it
	// advertisable to t=4TTL/3, most of the window the seeding exists to close.
	// The mutex is held for LONGER THAN THE TTL, so the two candidate clocks land
	// on opposite sides of the deadline: a lease stamped at creation is overdue by
	// the time it is checked, and one stamped after the wait is not.
	const contended = 3 * ttl

	l.mu.Lock()

	escrowed := make(chan error, 1)

	go func() { escrowed <- l.refillEscrow(t.Context()) }()

	// WAIT FOR THE LEASE TO EXIST, rather than sleeping and assuming it does.
	// Sleeping first made the test's meaning depend on the scheduler: on a loaded
	// machine the goroutine could reach Escrow only after the unlock, and then
	// correct code fails. Headroom dropping to zero is the allocator saying the
	// escrow has committed, which is the event the clock is supposed to date from.
	for {
		room, err := a.Headroom(t.Context(), tiers[0].Label)
		if err != nil {
			l.mu.Unlock()
			t.Fatalf("headroom: %v", err)
		}

		if room == 0 {
			break
		}

		time.Sleep(time.Millisecond)
	}

	// Only now does the contended wait begin, so it measures what it claims to.
	time.Sleep(contended)
	l.mu.Unlock()

	if err := <-escrowed; err != nil {
		t.Fatalf("refillEscrow: %v", err)
	}

	if len(l.Held()) == 0 {
		t.Fatal("nothing was escrowed; the test proves nothing")
	}

	unreachable, cancel := context.WithCancel(t.Context())
	cancel()

	// CHECKED IMMEDIATELY, with no further wait. The lease is already well past a
	// TTL measured from its creation and barely any time past one measured from
	// the unlock, so the two clocks give opposite answers with a full TTL of
	// margin either way. Sleeping here would eat into that margin for no gain —
	// waiting long enough puts BOTH clocks past the deadline and the test passes
	// against the defect it exists to catch, which is how its first version went
	// wrong.
	l.mu.Lock()
	l.heartbeatHeld(unreachable)
	held := len(l.held)
	l.mu.Unlock()

	if held != 0 {
		t.Errorf("a lease that was never once confirmed was still advertised a TTL after "+
			"it was escrowed (%d held); its clock started at the first failed renewal "+
			"rather than at the moment it became this listener's", held)
	}
}

// A LEASE THAT LEAVES TAKES ITS RENEWAL TIMESTAMP WITH IT.
//
// The confirmed map is keyed by lease id and a listener processes an unbounded
// number of leases over its life, so an entry that outlives its lease is a leak
// that grows with uptime rather than with load.
func TestRenewalTimestampsDoNotOutliveTheirLeases(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(&fakeRunner{}))

	holdRunning(t, l, a, tiers[0].Label, 7)

	l.mu.Lock()
	l.confirmed["a-lease-that-is-long-gone"] = time.Now()
	l.heartbeatHeld(t.Context())
	_, stale := l.confirmed["a-lease-that-is-long-gone"]
	tracked := len(l.confirmed)
	l.mu.Unlock()

	if stale {
		t.Error("a renewal timestamp survived the lease it was for; the map is keyed by " +
			"lease id and grows with every job this listener ever runs")
	}

	if tracked != 1 {
		t.Errorf("tracking %d leases, want 1 — the running lease and nothing else", tracked)
	}
}

// THE WAIT DOUBLES AND THEN STOPS AT THE CEILING.
//
// Tested directly on the entry, because the test below can only show that SOME
// wait was imposed: it would pass just as well against a fixed delay with no
// growth and no ceiling. Growth is what stops a node that is never coming back
// being asked every fifteen seconds forever; the ceiling is what keeps it being
// asked at all.
func TestTheRetryWaitDoublesToACeiling(t *testing.T) {
	t.Parallel()

	const (
		first   = time.Second
		ceiling = 4 * time.Second
	)

	now := time.Now()
	entry := &pendingCleanup{job: Job{RequestID: 7}}

	// Recorded ready to run: the first attempt after a failed completion is
	// immediate, because a node that was briefly busy is the common case.
	if !entry.due(now) {
		t.Fatal("a freshly recorded cleanup was not due immediately")
	}

	for i, want := range []time.Duration{first, 2 * first, ceiling, ceiling, ceiling} {
		entry.failed(now, first, ceiling)

		if entry.wait != want {
			t.Errorf("failure %d waits %v, want %v", i+1, entry.wait, want)
		}

		if entry.due(now) {
			t.Errorf("failure %d left the entry due immediately", i+1)
		}

		if !entry.due(now.Add(want)) {
			t.Errorf("failure %d was not due again after %v", i+1, want)
		}
	}

	// AND A DOUBLING THAT OVERSHOOTS IS CLAMPED. With 1s doubling to a 4s ceiling
	// the sequence lands on it exactly, so the clamp is never reached and a
	// version without it passes — which is what a mutation run found. Any pacing
	// whose ceiling is not a power of two multiple of the first wait needs it.
	overshoot := &pendingCleanup{job: Job{RequestID: 9}}

	overshoot.failed(now, 3*time.Second, 4*time.Second)
	overshoot.failed(now, 3*time.Second, 4*time.Second)

	if overshoot.wait != 4*time.Second {
		t.Errorf("a doubling that overshot the ceiling waits %v, want 4s", overshoot.wait)
	}
}

// A FAILED RETRY WAITS BEFORE THE NEXT ONE, so a node that is never coming back
// cannot occupy every pass.
//
// Retries run in one sequential pass and a single Destroy can wait the full node
// command timeout, so N hopeless records cost N timeouts before a node that has
// just recovered is tried at all. Keeping records until the destroy succeeds —
// which is correct — is what makes this matter, because ownership loss lets the
// count grow past the tier's capacity.
func TestAFailedRetryWaitsBeforeTheNextOne(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	var attempts atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		attempts.Add(1)

		return errors.New("the node is not answering")
	}}

	// A wait far longer than the test, so "waited" and "did not wait" cannot be
	// confused by timing.
	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithCleanupRetryPacing(time.Hour, time.Hour))

	holdRunning(t, l, a, tiers[0].Label, 7)

	if err := l.complete(t.Context(), Job{RequestID: 7}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if got := attempts.Load(); got != 1 {
		t.Fatalf("the completion's own destroy did not run: %d attempts", got)
	}

	// THE FIRST RETRY IS IMMEDIATE. A node that was briefly busy is the common
	// case by a wide margin, and making it wait would slow every ordinary recovery
	// to guard against the rare permanent one.
	l.retryCleanup(t.Context())

	if got := attempts.Load(); got != 2 {
		t.Fatalf("the first retry did not run immediately: %d attempts", got)
	}

	// Having failed, it waits.
	l.retryCleanup(t.Context())

	if got := attempts.Load(); got != 2 {
		t.Errorf("a retry that had just failed was attempted again on the very next pass "+
			"(%d attempts); with a node that is never coming back, every such record "+
			"costs a destroy timeout ahead of one whose node has recovered", got)
	}

	// And when it comes due it runs again — the pacing is a delay, not a give-up.
	l.mu.Lock()
	l.cleanup[7].at = time.Now().Add(-time.Second)
	l.mu.Unlock()

	l.retryCleanup(t.Context())

	if got := attempts.Load(); got != 3 {
		t.Errorf("a retry that had come due was not attempted (%d attempts); the backoff "+
			"is meant to pace the work, not abandon it", got)
	}
}

// SHUTDOWN DESTROYS COMPLETIONS WHOSE LEASE IS ALREADY GONE.
//
// releaseAll walks the lease maps — held, running, acquiring — so a record whose
// lease was reaped is in none of them, and the loop that would have retried it
// has just been stopped. Without a drain the obligation to destroy that
// container lasts exactly until the next shutdown, which is not what "only a
// successful destroy discharges it" can mean.
func TestShutdownDestroysCompletionsWhoseLeaseIsGone(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	destroyed := make(chan int64, 4)

	runner := &fakeRunner{onDestroy: func(id int64) error {
		destroyed <- id

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

	// A record with NO lease behind it, which is the whole point: nothing in the
	// lease maps refers to request 7.
	//
	// NOT DUE FOR AN HOUR, so the retry loop will not touch it however the test is
	// scheduled. Relying on the default TTL keeping the loop's ticker late was a
	// race: descheduled long enough, the loop could do the destroy and this would
	// pass with drainCleanup deleted. The drain ignores the backoff deliberately —
	// it is the last pass there will ever be — so making the entry ineligible for
	// the loop leaves the drain as the only possible source of a destroy.
	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{
		7: {job: Job{RequestID: 7}, at: time.Now().Add(time.Hour)},
	}
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	select {
	case id := <-destroyed:
		if id != 7 {
			t.Errorf("shutdown destroyed request %d, want 7", id)
		}
	default:
		t.Fatal("shutdown left a completed job's compute running: its lease was already " +
			"gone, so releaseAll never saw it and the retry loop had been stopped. " +
			"Nothing else will ever remove that container")
	}

	if cleanupCount(l) != 0 {
		t.Errorf("a discharged cleanup record survived the drain: %d", cleanupCount(l))
	}
}

// A SESSION THAT WILL NOT CLOSE STILL DESTROYS WHAT THIS LISTENER STARTED.
//
// That path returns early on purpose: a session billet could not close may still
// be handing this scale set work, so the capacity is left to expire rather than
// handed back while GitHub believes the tier has room. Releasing is what is
// skipped — destroying is not, and the drain used to skip records whose lease
// was still held on the grounds that releaseAll would deal with them. On this
// path releaseAll never runs, so nobody did.
func TestASessionThatWillNotCloseStillDestroysItsCompute(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	destroyed := make(chan int64, 4)

	runner := &fakeRunner{onDestroy: func(id int64) error {
		destroyed <- id

		return nil
	}}

	session := &fakeSession{onClose: func(context.Context) error {
		return errors.New("the scale set session is not answering")
	}}

	l := NewListener(a, tiers[0].Label, session, WithRunner(runner), WithDrainGrace(notDrainingHere))

	// A record whose lease this listener STILL HOLDS, which is the case the drain
	// used to skip. Not due for an hour, so the retry loop cannot be what destroys
	// it however the test is scheduled.
	holdRunning(t, l, a, tiers[0].Label, 7)

	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{
		7: {job: Job{RequestID: 7}, at: time.Now().Add(time.Hour)},
	}
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	cancel()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}

	select {
	case id := <-destroyed:
		if id != 7 {
			t.Errorf("shutdown destroyed request %d, want 7", id)
		}
	default:
		t.Fatal("a session that would not close left a completed job's compute running: " +
			"the release is skipped on that path deliberately, but the destroy is not " +
			"supposed to be, and nothing else will ever remove that container")
	}
}

// RENEWAL COVERS THE WHOLE SHUTDOWN BUDGET, not just the destroys.
//
// The watchdog used to fire when the destroy budget expired, and the session
// close and the releases then ran with nothing renewing. Held and promised
// leases could expire in that window while the session — and the positive
// maxCapacity GitHub last saw — was still open, so another tier could escrow
// capacity GitHub could still assign against. That is the close-before-release
// invariant broken by expiry rather than by a call, which is why releaseAll not
// having reached those leases is no defence.
func TestRenewalCoversTheWholeShutdownBudget(t *testing.T) {
	t.Parallel()

	const (
		ttl     = 300 * time.Millisecond
		grace   = 300 * time.Millisecond
		closing = 5 * time.Second
	)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	// A close that BLOCKS UNTIL THE TEST SAYS SO, rather than sleeping for a
	// guessed interval. Sleeping meant the assertion raced the close: if the test
	// goroutine lost 300ms to the scheduler, or the reap took it, the close
	// finished and releaseAll removed the lease before the check — and correct
	// code was reported as a renewal failure. Under t.Parallel on a loaded machine
	// that is not a remote possibility.
	closing0 := make(chan struct{})
	checked := make(chan struct{})

	var once sync.Once

	session := &fakeSession{onClose: func(context.Context) error {
		once.Do(func() { close(closing0) })

		<-checked

		return nil
	}}

	l := NewListener(a, tiers[0].Label, session, WithRunner(&fakeRunner{}),
		WithShutdownGrace(grace), WithFinishGraces(closing, closing),
		WithDrainGrace(notDrainingHere))

	lease := holdRunning(t, l, a, tiers[0].Label, 7)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	cancel()

	select {
	case <-closing0:
	case <-runDone:
		t.Fatal("the listener returned without closing its session")
	case <-time.After(30 * time.Second):
		t.Fatal("the shutdown never reached the session close")
	}

	// INSIDE THE CLOSE, and past both the destroy budget and a lease TTL. The
	// close cannot finish until this is done, so the only thing that can have kept
	// the lease alive is renewal.
	time.Sleep(grace + 2*ttl)

	_, reapErr := a.Reap(t.Context())
	beat := a.Heartbeat(t.Context(), lease.ID, lease.Epoch)

	close(checked)

	if reapErr != nil {
		t.Fatalf("reap: %v", reapErr)
	}

	if beat != nil {
		t.Fatalf("a lease expired while the shutdown was still closing its session (%v); "+
			"the old maxCapacity is still live at that moment, so another tier can "+
			"escrow capacity GitHub may still assign against", beat)
	}

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned")
	}
}

// A WEDGED TEARDOWN STOPS RENEWING, so the reaper can reclaim what it holds.
//
// Nothing can bound a Destroy that ignores its context, and the Runner interface
// does not forbid one — so Run can be wedged by a bad implementation whatever
// this listener does. What must not also happen is that it goes on renewing
// while wedged: renewal is exactly what stops the reaper reclaiming, so one
// stuck teardown would become capacity no operator gets back without killing the
// process.
func TestAWedgedTeardownStopsRenewing(t *testing.T) {
	t.Parallel()

	// The grace has to outlast the "still alive" check below, or the watchdog
	// fires before the test has established that renewal was ever running.
	const (
		ttl   = 300 * time.Millisecond
		grace = 1500 * time.Millisecond
	)

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	// A destroy that ignores its context entirely. Registered for cleanup at the
	// moment it is created, so a t.Fatal below cannot strand the goroutine.
	blocked := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(blocked) })

	t.Cleanup(unblock)

	entered := make(chan struct{}, 1)

	runner := &fakeRunner{onDestroy: func(int64) error {
		select {
		case entered <- struct{}{}:
		default:
		}

		<-blocked

		return nil
	}}

	// The finish phases are bounded too, and renewal has to outlast ALL of them —
	// so they are small here for the same reason the grace is: the test is about
	// what happens when the whole budget is spent, not about how long it is.
	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithShutdownGrace(grace), WithFinishGraces(grace/3, grace/3),
		WithDrainGrace(notDrainingHere))

	lease := holdRunning(t, l, a, tiers[0].Label, 7)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	cancel()

	select {
	case <-entered:
	case <-runDone:
		t.Fatal("the listener returned without destroying the job it was running")
	case <-time.After(30 * time.Second):
		t.Fatal("the shutdown release never reached the runner")
	}

	// ALIVE FIRST. Without this the test passes against a listener whose
	// heartbeat never started, or stopped the instant the caller cancelled — it
	// would be proving only that an unrenewed lease dies, which needs no
	// watchdog. Past a TTL into the teardown, still renewable, is what says
	// renewal was running when the grace began.
	time.Sleep(2 * ttl)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap before the grace: %v", err)
	}

	if err := a.Heartbeat(t.Context(), lease.ID, lease.Epoch); err != nil {
		t.Fatalf("the lease was already gone before the shutdown grace expired (%v); "+
			"renewal was not running during the teardown, so this proves nothing about "+
			"the watchdog", err)
	}

	// AND DEAD AFTER THE WHOLE BUDGET. Taken from the listener rather than
	// restated here: the teardown has four phases now, and a test that hardcodes
	// "past the grace" starts failing the next time one is added — which is
	// exactly how this one broke when the cleanup-loop join got its own.
	time.Sleep(l.teardownBudget() + 2*ttl)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}

	if err := a.Heartbeat(t.Context(), lease.ID, lease.Epoch); err == nil {
		t.Fatal("a listener wedged in its teardown was still renewing after the shutdown " +
			"grace; the reaper can never reclaim that capacity, so one stuck destroy " +
			"costs the deployment those vCPUs until the process is killed")
	}

	unblock()

	select {
	case <-runDone:
	case <-time.After(30 * time.Second):
		t.Fatal("Run never returned after the node answered")
	}
}

// cleanupCount reports how many completions are waiting to be retried.
func cleanupCount(l *Listener) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.cleanup)
}

// A TRANSIENT RELEASE FAILURE MUST NOT LOSE THE RETRY.
//
// The destroy is the slow, remote half and the release is the local one, so it
// is tempting to treat a successful destroy as the end of the job. It is not:
// if the release then fails, the lease stays in `running` being renewed
// forever, GitHub will not redeliver a completion it has already acknowledged,
// and deleting the retry record leaves nothing that will ever try again.
func TestATransientReleaseFailureKeepsTheRetry(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	// The destroy fails once so a retry record exists, then succeeds.
	var attempts atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		if attempts.Add(1) == 1 {
			return errors.New("the node is not answering")
		}

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithCleanupRetryPacing(0, 0))

	holdRunning(t, l, a, tiers[0].Label, 7)

	if err := l.complete(t.Context(), Job{RequestID: 7}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	l.mu.Lock()
	pending := len(l.cleanup)
	l.mu.Unlock()

	if pending != 1 {
		t.Fatalf("a completion whose destroy failed was not recorded: %d", pending)
	}

	// The retry destroys successfully, and the release fails: a cancelled context
	// is the cheapest transient failure that reaches the allocator.
	//
	// DRIVEN THROUGH retryCleanup, not complete. The dispatcher is half of the
	// property and calling complete directly skipped it, so every test on this
	// path passed equally against a retryCleanup that deleted the record whatever
	// error complete handed back.
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	l.retryCleanup(cancelled)

	if got := attempts.Load(); got != 2 {
		t.Fatalf("the retry never reached the runner: %d destroy attempts, want 2", got)
	}

	l.mu.Lock()
	pending = len(l.cleanup)
	l.mu.Unlock()

	if pending != 1 {
		t.Errorf("the retry record was dropped although the lease was never released (%d "+
			"pending); it stays in running being renewed forever and nothing will try "+
			"again", pending)
	}

	// And once the release can run, the job is finally over.
	l.retryCleanup(t.Context())

	l.mu.Lock()
	pending = len(l.cleanup)
	running := len(l.running)
	l.mu.Unlock()

	if pending != 0 || running != 0 {
		t.Errorf("after a successful release: %d pending, %d running, want 0 and 0",
			pending, running)
	}
}

// A RETRY RECORD OUTLIVES THE LEASE IT WAS FOR, AND IS DISCHARGED BY THE DESTROY.
//
// This asserted the opposite for one commit, and the reasoning that produced it
// is worth keeping visible. Losing the lease genuinely does end this listener's
// claim on the CAPACITY — so dropping the record looked like tidying up, and
// both shipped runners implement Sweeper, which destroys compute no lease is
// holding. But Sweeper is optional on the Runner interface. Reasoning from what
// the current runners happen to do makes correctness depend on which one is
// plugged in, and with a non-sweeping runner nothing destroys that container
// until the host restarts.
//
// The record exists because this listener launched something and could not
// destroy it. A fence or a reap changes who owns the capacity; it does not make
// the container disappear, and GitHub will not redeliver the completion that
// would ask again. So the obligation is to the compute, and only the destroy
// discharges it.
func TestALostLeaseKeepsItsPendingRetry(t *testing.T) {
	t.Parallel()

	const ttl = 300 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	var answering atomic.Bool

	runner := &fakeRunner{onDestroy: func(int64) error {
		if answering.Load() {
			return nil
		}

		return errors.New("the node is not answering")
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner),
		WithCleanupRetryPacing(0, 0))

	holdRunning(t, l, a, tiers[0].Label, 7)

	if err := l.complete(t.Context(), Job{RequestID: 7}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	if cleanupCount(l) != 1 {
		t.Fatalf("a completion whose destroy failed was not recorded: %d", cleanupCount(l))
	}

	// REAPED, not released. A direct release leaves the lease terminal at the same
	// epoch and the heartbeat sees ErrLeaseNotFound; a real reap bumps the epoch
	// and it sees ErrFenced. Production collapses the two, and a test that only
	// ever produces one cannot notice if that stops being true.
	time.Sleep(2 * ttl)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap: %v", err)
	}

	l.mu.Lock()
	l.heartbeatHeld(t.Context())
	_, stillRunning := l.running[7]
	l.mu.Unlock()

	if stillRunning {
		t.Fatal("the listener kept a lease the reaper had taken, so nothing below this " +
			"point is exercising a lost lease")
	}

	if cleanupCount(l) != 1 {
		t.Errorf("the retry record was dropped when the lease was reaped (%d pending); the "+
			"container is still running on a host, GitHub will not redeliver the "+
			"completion, and with a runner that cannot sweep nothing else would ever "+
			"ask again", cleanupCount(l))
	}

	// AND A RETRY THAT REDISCOVERS THE LOSS KEEPS IT TOO. The heartbeat is not the
	// only route to this state: a retry can be the thing that finds the lease
	// gone, and that path drops the record separately from the one above.
	l.retryCleanup(t.Context())

	if cleanupCount(l) != 1 {
		t.Errorf("a retry that found its lease gone dropped the record (%d pending); the "+
			"destroy had still not succeeded, so the container is still there",
			cleanupCount(l))
	}

	// AND THE DESTROY IS WHAT ENDS IT. There is no lease left to release, so a
	// retry that insisted on releasing would never finish; the obligation was to
	// the compute and it is discharged the moment the node answers.
	answering.Store(true)

	l.retryCleanup(t.Context())

	if cleanupCount(l) != 0 {
		t.Errorf("a retry whose destroy finally succeeded is still pending (%d); it would "+
			"be attempted on every pass for the life of the process", cleanupCount(l))
	}

	// AND THE PROMISED HALF, which is its own branch in the heartbeat. Without
	// this, restoring the deletion there alone would pass every assertion above.
	promised, err := a.Reserve(t.Context(), tiers[0].Label)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	l.mu.Lock()
	l.acquiring[9] = &promise{lease: promised, at: time.Now()}
	l.cleanup = map[int64]*pendingCleanup{9: {job: Job{RequestID: 9}}}
	l.mu.Unlock()

	time.Sleep(2 * ttl)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("reap the promised lease: %v", err)
	}

	l.mu.Lock()
	l.heartbeatHeld(t.Context())
	_, stillPromised := l.acquiring[9]
	l.mu.Unlock()

	if stillPromised {
		t.Fatal("the listener kept a promise the reaper had taken, so this proves nothing")
	}

	if cleanupCount(l) != 1 {
		t.Errorf("the retry record was dropped when a PROMISED lease was reaped (%d "+
			"pending); a job billet acquired and launched leaves compute behind exactly "+
			"like an assigned one", cleanupCount(l))
	}
}

// A COMPLETION THIS LISTENER DOES NOT HOLD IS NOT RECORDED FOR RETRY.
//
// GitHub can report a job completed that this listener never assigned — a
// restart loses the in-memory map while the lease lives on for the reaper — and
// recording those grows the map with entries whose retry can never accomplish
// anything, each one attempted on every pass.
func TestACompletionNotHeldIsNotRecorded(t *testing.T) {
	t.Parallel()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	runner := &fakeRunner{onDestroy: func(int64) error {
		return errors.New("the node is not answering")
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

	// No holdRunning: this listener knows nothing about request 7.
	if err := l.complete(t.Context(), Job{RequestID: 7}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	l.mu.Lock()
	pending := len(l.cleanup)
	l.mu.Unlock()

	if pending != 0 {
		t.Errorf("a completion for a job this listener never assigned was recorded for retry "+
			"(%d); those accumulate and are retried on every pass, forever", pending)
	}
}

// holdRunning gives a listener a lease it is actually running, which is what
// makes a completion its business.
//
// A completion for a request this listener never assigned is deliberately NOT
// recorded for retry — a restart can lose the in-memory map while the lease
// lives on for the reaper, and retrying those would grow the map with entries
// whose retry can never accomplish anything. So a test about the retry has to
// install the lease first, or it is testing the ignore path.
// It returns the lease so a test can ask the allocator about it directly, which
// is the only way to tell "still renewed" from "reaped" from outside.
func holdRunning(t *testing.T, l *Listener, a *alloc.Allocator, tier string,
	requestID int64,
) *alloc.Lease {
	t.Helper()

	lease, err := a.Reserve(t.Context(), tier)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	if err := a.Assign(t.Context(), lease.ID, lease.Epoch, requestID, requestID); err != nil {
		t.Fatalf("assign: %v", err)
	}

	l.mu.Lock()
	l.running[requestID] = lease
	l.mu.Unlock()

	return lease
}

// THE WIRING, WHICH IS A DIFFERENT PROPERTY FROM THE BEHAVIOUR.
//
// Every other test of the retry calls retryCleanup directly, so all of them pass
// against a Run that never starts the loop — the retry works perfectly and is
// simply never invoked, and in production nothing else invokes it. Deleting the
// `go l.cleanupLoop(beat)` line survived the entire suite.
//
// So this test refuses to touch retryCleanup or complete. It installs a pending
// cleanup, starts Run, and waits for a destroy that only the loop can produce.
func TestRunStartsTheCleanupLoop(t *testing.T) {
	t.Parallel()

	// The loop ticks at ttl/3, so this retries roughly every 100ms. The TTL is
	// short to make the ticker fire, not to test expiry: no reaper runs here, and
	// the listener renews its own running leases, so nothing expires underneath.
	const ttl = 300 * time.Millisecond

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers,
		alloc.WithLeaseTTL(ttl))

	destroyed := make(chan struct{})

	var once sync.Once

	runner := &fakeRunner{onDestroy: func(int64) error {
		once.Do(func() { close(destroyed) })

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

	holdRunning(t, l, a, tiers[0].Label, 11)

	// Installed DIRECTLY, not through complete(). Going through complete would
	// call Destroy on the way in, and this test would then pass on that call
	// rather than on anything the loop did.
	l.mu.Lock()
	l.cleanup = map[int64]*pendingCleanup{11: {job: Job{RequestID: 11}}}
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	runDone := make(chan struct{})

	go func() {
		defer close(runDone)

		if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()

	// OBSERVED WHILE THE LISTENER IS STILL RUNNING, which is the whole point and
	// was the hole in the first version of this test.
	//
	// That version let the context expire and then checked whether a destroy had
	// happened. Shutdown produces one: releaseAll destroys everything still in
	// `running`, and request 11 is. So it passed with `go l.cleanupLoop(beat)`
	// deleted — the mutant it exists to kill — and the only reason the mutation
	// run reported a kill was the incidental DeadlineExceeded from Run.
	//
	// The failure is recorded rather than raised so that cancel and join still
	// happen; a t.Fatal here would leave Run's goroutine logging into a finished
	// test.
	var failure string

	select {
	case <-destroyed:
	case <-runDone:
		failure = "the listener stopped before anything retried the pending cleanup"
	case <-time.After(10 * time.Second):
		failure = "Run never retried a pending cleanup, so nothing started cleanupLoop: a " +
			"completion whose destroy failed holds its capacity for the life of the " +
			"process, because GitHub does not redeliver a completion it has acknowledged"
	}

	cancel()
	<-runDone

	if failure != "" {
		t.Fatal(failure)
	}
}
