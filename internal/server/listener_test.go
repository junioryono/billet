package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
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

	l := NewListener(a, tiers[0].Label, session)

	// Observed DURING the run. Shutdown releases running leases as well as held
	// ones — correctly, since nothing can be executing them yet — so anything
	// asserted after Run returns would see zero either way and prove nothing.
	var running atomic.Int32

	session.onPoll = func(int) {
		running.Store(int32(l.Running()))
	}

	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()

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

	l := NewListener(a, tiers[0].Label, session)

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

func newAllocator(t *testing.T, limits alloc.Limits, tiers []config.Tier) *alloc.Allocator {
	t.Helper()

	db := openState(t)

	a, err := alloc.New(db, limits, tiers)
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	return a
}

// A poll that lasts longer than the lease TTL must not cost the listener its
// escrow.
//
// This is the failure that made heartbeats independent. A long poll is nominally
// 50 seconds against a 90 second TTL, which reads like ample margin — but the
// vendor's HTTP client permits a request to run for minutes once slow responses
// and retries are counted, and heartbeats that happen only BETWEEN polls stop for
// as long as one poll lasts. The reaper then terminalises the leases, another
// tier escrows the capacity, and the poll returns an assignment backed by a lease
// this listener no longer holds.
func TestEscrowSurvivesAPollLongerThanTheLeaseTTL(t *testing.T) {
	const ttl = 150 * time.Millisecond

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

// fakeSession stands in for a scale-set message session. It never returns work,
// so the listener does nothing but escrow, advertise, and release — which is the
// whole of what this test is about.
type fakeSession struct {
	onPoll func(maxCapacity int)
	onGet  func() (*Message, error)
	stats  *Statistics
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

func (f *fakeSession) DeleteMessage(context.Context, int64) error { return nil }

func (f *fakeSession) AcquireJobs(_ context.Context, ids []int64) ([]int64, error) {
	f.acquiredMu.Lock()
	f.acquired = append(f.acquired, ids...)
	f.acquiredMu.Unlock()

	return ids, nil
}

// acquiredIDs returns what the listener asked to claim.
func (f *fakeSession) acquiredIDs() []int64 {
	f.acquiredMu.Lock()
	defer f.acquiredMu.Unlock()

	return append([]int64(nil), f.acquired...)
}

func (f *fakeSession) Close(context.Context) error { return nil }
