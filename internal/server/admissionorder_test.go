package server

import (
	"context"
	"sync"
	"testing"

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
	freeOneLease(t, a, "a-small")

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile after a slot freed: %v", err)
	}

	if got := log.tiers(); len(got) != 2 {
		t.Errorf("started %v: the small tier took room the longest-waiting tier is "+
			"holding, so the large shape can never run on a busy fleet", got)
	}

	// The second small job finishes. Now the large shape fits and must take it.
	freeOneLease(t, a, "a-small")

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

	freeOneLease(t, a, "a-small")

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

	freeOneLease(t, a, "a-small")

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile after the wait cleared: %v", err)
	}

	got := log.tiers()
	if len(got) != 3 || got[2] != "a-small" {
		t.Errorf("started %v: nothing was waiting any more, so the freed room had to "+
			"go to the tier that could use it", got)
	}
}

// freeOneLease ends one of a tier's open leases, as a finished job does.
func freeOneLease(t *testing.T, a *alloc.Allocator, tierLabel string) {
	t.Helper()

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
