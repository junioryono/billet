package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// tierOf builds a tier of a given size, so a test can state the shapes that
// contend rather than inherit the file's one size.
func tierOf(label string, vcpu int) config.Tier {
	entry := tier(label)
	entry.VCPU = vcpu

	return entry
}

// launchLog records which tier each launch belonged to, in order. The lease
// carries the tier, so one runner serves every listener a control plane builds
// — which is also how production wires it (Server.listenerOpts).
type launchLog struct {
	mu      sync.Mutex
	started []string
}

func (l *launchLog) Launch(_ context.Context, lease *alloc.Lease, _ Job) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.started = append(l.started, lease.Tier)

	return nil
}

func (l *launchLog) Destroy(context.Context, int64) error { return nil }

func (l *launchLog) tiers() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]string(nil), l.started...)
}

// orderedListeners builds a control plane under one admission order, the way
// the CLI does, and returns its listeners in the order the tiers were given.
func orderedListeners(t *testing.T, tiers []config.Tier, vcpu int,
	order config.AdmissionOrder,
) (*alloc.Allocator, *launchLog, []*Listener) {
	t.Helper()

	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: vcpu, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	registerHost(t, a)

	log := &launchLog{}
	s := New(a, nil, tiers, "order-test", nil,
		WithNodeRunner(log), WithAdmissionOrder(order))

	listeners := make([]*Listener, 0, len(tiers))
	for i := range tiers {
		listeners = append(listeners, NewListener(a, tiers[i].Label, &fakeSession{}, s.listenerOpts(nil)...))
	}

	return a, log, listeners
}

// contenders is the fixture both policies are judged on: room for exactly two
// small jobs OR one large one, with both tiers wanting work at once. A fleet
// where everything fits cannot tell the policies apart.
func contenders() []config.Tier {
	return []config.Tier{tierOf("a-small", 4), tierOf("b-large", 8)}
}

// FAIR DOES NOT STARVE A LARGE SHAPE BEHIND A STREAM OF SMALL ONES.
//
// The small tier has standing demand and takes the fleet first. When one of its
// jobs finishes there is room for another small job and not for the large one,
// so first-come would give the room back to the small tier forever and the
// large job would never run — on a fleet that is never idle, never.
//
// Under `fair` the tier that has waited longest holds the line: the freed room
// goes to nobody until the large shape fits. That is head-of-line blocking, and
// it is the point rather than a defect — it is what "fair" buys, and `fill` is
// the policy for a deployment that would rather have the throughput.
func TestFairHoldsTheLineForALargeShape(t *testing.T) {
	t.Parallel()

	a, log, listeners := orderedListeners(t, contenders(), 8, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	// The small tier fills the fleet: two jobs, two runners.
	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	// The large tier asks, finds no room, and is now the waiting tier.
	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	if got := log.tiers(); len(got) != 2 {
		t.Fatalf("started %v, want both small runners and nothing large", got)
	}

	// One small job finishes. Four vCPU are free: enough for another small job,
	// not enough for the large one.
	freeOneLease(t, a)

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile after a slot freed: %v", err)
	}

	if got := log.tiers(); len(got) != 2 {
		t.Errorf("started %v: the small tier took room the longest-waiting tier is "+
			"holding, so the large shape can never run on a busy fleet", got)
	}

	// The second small job finishes. Now the large shape fits and must take it.
	freeOneLease(t, a)

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile once its shape fits: %v", err)
	}

	got := log.tiers()
	if len(got) != 3 || got[2] != "b-large" {
		t.Errorf("started %v, want the large tier to run once its shape fit", got)
	}
}

// FILL GIVES FREED ROOM TO WHATEVER FITS, which is the trade the other policy
// exists to offer: more work runs, and a large shape can wait indefinitely.
func TestFillGivesFreedRoomToWhateverFits(t *testing.T) {
	t.Parallel()

	a, log, listeners := orderedListeners(t, contenders(), 8, config.AdmissionFill)
	small, large := listeners[0], listeners[1]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	freeOneLease(t, a)

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile after a slot freed: %v", err)
	}

	got := log.tiers()
	if len(got) != 3 || got[2] != "a-small" {
		t.Errorf("started %v, want fill to give the freed room to the shape that fits", got)
	}
}

// THE ORDER GATES PURCHASES, NEVER ADVERTISEMENTS.
//
// This is the whole lesson of #140: a tier that is not advertising is a tier
// GitHub will not assign work to, and a scheduler that withholds advertisement
// to control admission cannot be corrected later, because the work never
// arrives. A waiting tier goes on advertising its ceiling; what it does not do
// is buy.
func TestWaitingForItsTurnNeverLowersATiersAdvertisement(t *testing.T) {
	t.Parallel()

	_, _, listeners := orderedListeners(t, contenders(), 8, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if got := small.steadyAdvertisement(); got != 2 {
		t.Errorf("a tier waiting its turn advertises %d, want its ceiling of 2", got)
	}

	if got := large.steadyAdvertisement(); got != 1 {
		t.Errorf("the waiting tier advertises %d, want its ceiling of 1", got)
	}
}

// A TIER WHOSE DEMAND GOES AWAY STOPS HOLDING THE LINE.
//
// A queued job can be cancelled at GitHub, and the only thing that says so is
// the next statistics count. A waiting record that outlived its demand would
// block every other tier for as long as the control plane ran, which is a
// worse outage than the one fairness prevents.
func TestATierWhoseDemandDisappearsStopsHoldingTheLine(t *testing.T) {
	t.Parallel()

	a, log, listeners := orderedListeners(t, contenders(), 8, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	// The large tier's job is cancelled before it ever ran: GitHub now counts
	// none assigned to it.
	if err := large.reconcilePool(t.Context(), 0); err != nil {
		t.Fatalf("large tier reconcile with no demand: %v", err)
	}

	freeOneLease(t, a)

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile after the wait cleared: %v", err)
	}

	got := log.tiers()
	if len(got) != 3 || got[2] != "a-small" {
		t.Errorf("started %v: nothing was waiting any more, so the freed room had to "+
			"go to the tier that could use it", got)
	}
}

// freeOneLease ends one of the small tier's open leases, as a finished job does.
// The small tier is the one that fills the fleet in every case here, so it is
// also the only one holding a lease to give back.
func freeOneLease(t *testing.T, a *alloc.Allocator) {
	t.Helper()

	tierLabel := contenders()[0].Label

	runners, err := a.PoolRunners(t.Context(), tierLabel)
	if err != nil {
		t.Fatalf("read the pool for %s: %v", tierLabel, err)
	}

	if len(runners) == 0 {
		t.Fatalf("%s has no pool member to finish", tierLabel)
	}

	lease, err := a.Lease(t.Context(), runners[0].LeaseID)
	if err != nil {
		t.Fatalf("read lease %s: %v", runners[0].LeaseID, err)
	}

	if err := a.ForgetPoolRunner(t.Context(), lease.ID); err != nil {
		t.Fatalf("forget pool runner %s: %v", lease.ID, err)
	}

	if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("release lease %s: %v", lease.ID, err)
	}
}

// THE FILE REACHES THE LISTENER, not just the config struct.
//
// An option that is validated and never wired is the failure this asserts
// against: `server.admission_order: fill` would parse, `billet check` would
// approve it, and every tier would go on holding the line. So this builds the
// control plane the way the command does, from a Config, and reads the policy
// off a listener it produced.
func TestTheConfiguredAdmissionOrderReachesTheListeners(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		set  config.AdmissionOrder
		want config.AdmissionOrder
	}{
		{name: "unset is fair", want: config.AdmissionFair},
		{name: "fill", set: config.AdmissionFill, want: config.AdmissionFill},
		{name: "fair", set: config.AdmissionFair, want: config.AdmissionFair},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			tiers := contenders()
			cfg := &config.Config{Tiers: tiers, Server: &config.ServerConfig{AdmissionOrder: tc.set}}

			opts, err := OptionsFromConfig(cfg)
			if err != nil {
				t.Fatalf("OptionsFromConfig: %v", err)
			}

			a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)
			if err != nil {
				t.Fatal(err)
			}

			s := New(a, nil, tiers, "order-config-test", nil, opts...)
			l := NewListener(a, tiers[0].Label, &fakeSession{}, s.listenerOpts(nil)...)

			if l.order == nil {
				t.Fatal("the listener has no admission order, so nothing the file says can reach it")
			}

			if l.order.policy != tc.want {
				t.Errorf("listener admission order = %q, want %q", l.order.policy, tc.want)
			}
		})
	}
}

// EVERY TIER'S LISTENER GETS THE SAME QUEUE, AND BUILDING IT RACES NOBODY.
//
// listenerOpts runs on each tier's own goroutine (runTier), so a queue built
// lazily there is two listeners racing to create it — and whichever won, the
// fleet would be split between two queues each being fair about half of it.
// Caught by -race on the e2e suite; this is the same thing at its own level,
// and it fails without the detector too, because the pointers differ.
func TestEveryListenerSharesOneQueueBuiltBeforeTheyStart(t *testing.T) {
	t.Parallel()

	tiers := contenders()

	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	s := New(a, nil, tiers, "shared-queue-test", nil)

	queues := make([]*admissionQueue, len(tiers))

	var wg sync.WaitGroup

	for i := range tiers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			l := NewListener(a, tiers[i].Label, &fakeSession{}, s.listenerOpts(nil)...)
			queues[i] = l.order
		}()
	}

	wg.Wait()

	for i, q := range queues {
		if q == nil {
			t.Fatalf("%s got no admission queue", tiers[i].Label)
		}

		if q != queues[0] {
			t.Errorf("%s got a different admission queue from %s, so each is fair about part "+
				"of the fleet", tiers[i].Label, tiers[0].Label)
		}
	}
}

// WAITING TIERS TAKE TURNS: A PLACE IS WON FOR ONE PURCHASE, NOT FOR A BACKLOG.
//
// Two tiers of one shape share a fleet and both have demand that never runs
// out, as a busy repository's CI does. The first to wait buys the first freed
// slot; the second freed slot is the other tier's even though the first still
// has demand and asks first. Before a purchase re-dated the buyer, the first
// waiter held the line for as long as its backlog lasted (2026-09-25: half an
// hour, with the other target's tier starting nothing).
//
// A third tier holds one slot throughout. Without it the first tier would hold
// the whole fleet, and a tier whose only way to grow is its own jobs ending is
// never recorded as waiting (#157), which is a different rule from this one.
func TestWaitingTiersTakeTurnsWhenBothHaveStandingDemand(t *testing.T) {
	t.Parallel()

	a, log, listeners := orderedListeners(t,
		[]config.Tier{tierOf("a-small", 4), tierOf("b-small", 4), tierOf("c-small", 4)},
		12, config.AdmissionFair)
	first, second, other := listeners[0], listeners[1], listeners[2]

	var mu sync.Mutex

	clock := time.Date(2026, 9, 25, 17, 21, 0, 0, time.UTC)
	first.order.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()

		clock = clock.Add(time.Second)

		return clock
	}

	if err := other.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("other tier reconcile: %v", err)
	}

	// The first tier fills the rest and still wants more, so it waits first.
	if err := first.reconcilePool(t.Context(), 10); err != nil {
		t.Fatalf("first tier reconcile: %v", err)
	}

	if err := second.reconcilePool(t.Context(), 10); err != nil {
		t.Fatalf("second tier reconcile: %v", err)
	}

	if _, _, waiting := first.order.waitingSince("a-small"); !waiting {
		t.Fatal("the first tier found no room for its demand and is not recorded as waiting")
	}

	if _, _, waiting := second.order.waitingSince("b-small"); !waiting {
		t.Fatal("the second tier found no room for its demand and is not recorded as waiting")
	}

	if got := log.tiers(); len(got) != 3 {
		t.Fatalf("started %v, want the other tier's runner and the first tier's two", got)
	}

	// A slot frees. The first tier has waited longest and takes it.
	freeOneLease(t, a)

	if err := first.reconcilePool(t.Context(), 10); err != nil {
		t.Fatalf("first tier reconcile after a slot freed: %v", err)
	}

	if got := log.tiers(); len(got) != 4 || got[3] != "a-small" {
		t.Fatalf("started %v, want the longest waiter to take the first freed slot", got)
	}

	// Another frees, and the first tier asks first again. Its turn is spent.
	freeOneLease(t, a)

	if err := first.reconcilePool(t.Context(), 10); err != nil {
		t.Fatalf("first tier reconcile after its turn: %v", err)
	}

	if got := log.tiers(); len(got) != 4 {
		t.Fatalf("started %v: the first tier bought again while the other tier waited, "+
			"so a backlog that never runs out holds the line forever", got)
	}

	if err := second.reconcilePool(t.Context(), 10); err != nil {
		t.Fatalf("second tier reconcile on its turn: %v", err)
	}

	if got := log.tiers(); len(got) != 5 || got[4] != "b-small" {
		t.Fatalf("started %v, want the other waiting tier to take the second freed slot", got)
	}
}
