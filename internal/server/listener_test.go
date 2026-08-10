package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
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
		WithRunner(&fakeRunner{}))

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
		WithRunner(&fakeRunner{}))

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
// process; they sit until the pickup deadline and are then reassigned, and from
// the outside billet looks like it silently dropped them.
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
		WithRunner(&fakeRunner{}))

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

	// Restarted the way the control plane would restart it.

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

	l := NewListener(a, tiers[0].Label, session)

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

	l = NewListener(a, tiers[0].Label, session)

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

	l := NewListener(a, tiers[0].Label, session)

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
			&slog.HandlerOptions{Level: slog.LevelWarn}))))

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
		WithRunner(&fakeRunner{}))

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

	l := NewListener(a, tiers[0].Label, session, WithRunner(runner))

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
	stats     *Statistics
	// acquired records every request id the listener asked GitHub to claim, so a
	// test can assert WHICH class of message drove the acquisition.
	acquiredMu sync.Mutex
	acquired   []int64
}

func (f *fakeSession) GetMessage(ctx context.Context, _ int64, maxCapacity int) (*Message, error) {
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

func (f *fakeSession) Close(context.Context) error { return nil }

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

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	a := newAllocator(t, alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)

	// A destroy that fails immediately the first time, then blocks the way an
	// unreachable node does.
	blocked := make(chan struct{})
	entered := make(chan struct{}, 1)

	var attempts atomic.Int32

	runner := &fakeRunner{onDestroy: func(int64) error {
		if attempts.Add(1) == 1 {
			return errors.New("the node is not answering")
		}

		select {
		case entered <- struct{}{}:
		default:
		}

		<-blocked

		return nil
	}}

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

	t.Cleanup(func() { close(blocked) })

	holdRunning(t, l, a, tiers[0].Label, 7)

	// A completion whose destroy failed, so there is something to retry.
	if err := l.complete(t.Context(), Job{RequestID: 7}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// retryCleanup directly rather than cleanupLoop: the question is whether a
	// stuck retry holds anything renewal needs, not how long the loop's ticker
	// waits before the first attempt.
	go l.retryCleanup(ctx)

	// Wait until the retry is genuinely stuck inside the provider, which is the
	// state the whole test is about.
	select {
	case <-entered:
	case <-time.After(30 * time.Second):
		t.Fatal("the retry never reached the runner")
	}

	// AND RENEWAL STILL RUNS. Called directly rather than waited for, because the
	// question is whether the cleanup holds a lock or a goroutine that renewal
	// needs — not how fast the ticker is. If retryCleanup were on the heartbeat's
	// tick, this work would simply not be happening while that destroy is stuck.
	done := make(chan struct{})

	go func() {
		defer close(done)

		l.mu.Lock()
		l.heartbeatHeld(ctx)
		l.mu.Unlock()
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("renewal could not run while a cleanup retry was stuck in the provider; one " +
			"unreachable host delays every renewal on this listener and expires the leases " +
			"it is protecting")
	}
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

	l := NewListener(a, tiers[0].Label, &fakeSession{}, WithRunner(runner))

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
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()

	if err := l.complete(cancelled, Job{RequestID: 7}); err == nil {
		t.Fatal("a release that could not run reported success")
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
	if err := l.complete(t.Context(), Job{RequestID: 7}); err != nil {
		t.Fatalf("complete: %v", err)
	}

	l.mu.Lock()
	pending = len(l.cleanup)
	running := len(l.running)
	l.mu.Unlock()

	if pending != 0 || running != 0 {
		t.Errorf("after a successful release: %d pending, %d running, want 0 and 0",
			pending, running)
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
func holdRunning(t *testing.T, l *Listener, a *alloc.Allocator, tier string, requestID int64) {
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
	l.cleanup = map[int64]Job{11: {RequestID: 11}}
	l.mu.Unlock()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	// The retry is what ends the test. Without the loop, nothing does, and the
	// context bound is what turns a missing goroutine into a failure.
	go func() {
		select {
		case <-destroyed:
			cancel()
		case <-ctx.Done():
		}
	}()

	if err := l.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("Run: %v", err)
	}

	select {
	case <-destroyed:
	default:
		t.Fatal("Run never retried a pending cleanup, so nothing started cleanupLoop: a " +
			"completion whose destroy failed holds its capacity for the life of the " +
			"process, because GitHub does not redeliver a completion it has acknowledged")
	}
}
