package alloc

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// quarantineFleet is one machine with room for exactly one lease of `small`.
func quarantineFleet(t *testing.T, now *time.Time) *Allocator {
	t.Helper()

	small := tier("small", 4, 16*config.GiB)
	small.Provider = config.ProviderDocker

	a := newBareAllocator(t, Limits{MaxVCPU: 1000, MaxMemory: 4000 * config.GiB},
		[]config.Tier{small},
		WithClock(func() time.Time { return *now }),
		WithLeaseTTL(30*time.Second))

	mustRegister(t, a, NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 4, Memory: 16 * config.GiB})

	return a
}

// nodeEpoch is the fencing token the ledger holds for a host, which a
// reconciliation has to present.
func nodeEpoch(t *testing.T, a *Allocator) int64 {
	t.Helper()

	const name = "epyc-1"

	var epoch int64

	if err := a.db.Reader().QueryRowContext(t.Context(),
		`SELECT epoch FROM nodes WHERE name = ?`, name).Scan(&epoch); err != nil {
		t.Fatalf("read the epoch of %s: %v", name, err)
	}

	return epoch
}

// busyLease drives a lease all the way to a running container on epyc-1.
func busyLease(t *testing.T, a *Allocator) *Lease {
	t.Helper()

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	if err := a.Bind(t.Context(), lease.ID, lease.Epoch, "epyc-1"); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	fresh, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	advanceTo(t, a, fresh, PhaseBusy)

	return fresh
}

// EXPIRY DOES NOT FREE CAPACITY A CONTAINER MAY STILL BE USING.
//
// The reaper terminalizes anything whose holder stopped heartbeating. For escrow
// nobody launched that is exactly right. For a lease with a container behind it
// it is a window: the capacity is free the instant the reaper commits, and the
// container keeps running until the node next sweeps — so another tier can
// escrow that slot in between and two jobs end up on a machine sized for one.
//
// Capacity reclaimed late is recoverable. Capacity handed out twice is not.
func TestAnExpiredRunningLeaseKeepsItsCapacityCharged(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	if got := headroom(t, a, "small"); got != 0 {
		t.Fatalf("the machine is full and offered %d more", got)
	}

	// The listener dies. Nothing heartbeats, and the lease ages out — while the
	// container carries on.
	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	after, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if after.Phase != PhaseQuarantine {
		t.Errorf("the reaper moved a running lease to %q; its capacity is free while its "+
			"container may still be on the host", after.Phase)
	}

	if got := headroom(t, a, "small"); got != 0 {
		t.Errorf("the machine offered %d slots while a container it cannot account for may "+
			"still be running; that slot is being sold twice", got)
	}
}

// Custody and teardown still mean compute may exist. If their holder vanishes,
// expiry must become quarantine rather than returning the same capacity twice.
func TestExpiredHeldPhasesBecomeQuarantine(t *testing.T) {
	for _, phase := range []Phase{PhaseCustody, PhaseTeardown} {
		t.Run(string(phase), func(t *testing.T) {
			now := time.Now().UTC()
			a := quarantineFleet(t, &now)
			lease := busyLease(t, a)

			if err := a.Advance(t.Context(), lease.ID, lease.Epoch, phase); err != nil {
				t.Fatalf("Advance: %v", err)
			}
			now = now.Add(31 * time.Second)

			if reaped, err := a.Reap(t.Context()); err != nil || reaped != 0 {
				t.Fatalf("Reap = %d, %v; held compute must not be terminalized", reaped, err)
			}
			got, err := a.Lease(t.Context(), lease.ID)
			if err != nil || got.Phase != PhaseQuarantine {
				t.Fatalf("expired %s lease = %+v, %v; want quarantine", phase, got, err)
			}
		})
	}
}

// Quarantine has no live holder to notify, so an operator's force assertion
// resolves it immediately rather than leaving a request nobody can observe.
func TestForceReleaseResolvesQuarantineWithoutALiveHolder(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	now = now.Add(31 * time.Second)
	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	result, err := a.ForceRelease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("ForceRelease: %v", err)
	}
	if result.Pending {
		t.Fatalf("ForceRelease = %+v; quarantine has no holder to wait for", result)
	}
	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, ErrLeaseNotFound) {
		t.Fatalf("forced quarantine is still open: %v", err)
	}
	if got := headroom(t, a, "small"); got != 1 {
		t.Fatalf("Headroom = %d, want the quarantined capacity returned", got)
	}
}

// AND THE REAPER DOES NOT KEEP RE-REAPING IT. A quarantined lease is expired by
// definition and stays that way until it is resolved, so a reaper that counted
// it again would spin on the same rows forever and bump their epochs each pass.
func TestAQuarantinedLeaseIsNotReapedAgain(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("first reap: %v", err)
	}

	first, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("second reap: %v", err)
	}

	second, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("Lease: %v", err)
	}

	if second.Epoch != first.Epoch {
		t.Errorf("a second reap bumped the epoch from %d to %d, so the reaper spins on this "+
			"lease every pass", first.Epoch, second.Epoch)
	}
}

// ESCROW NOBODY LAUNCHED STILL ENDS AT EXPIRY, which is the other half of the
// rule and the far more common one: a listener that died holding capacity has no
// container behind it, and holding that capacity would strand it.
func TestExpiredEscrowStillTerminalizes(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	lease, err := a.Reserve(t.Context(), "small")
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}

	now = now.Add(31 * time.Second)

	reaped, err := a.Reap(t.Context())
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}

	if reaped != 1 {
		t.Errorf("the reaper reclaimed %d leases; abandoned escrow must come back at once", reaped)
	}

	if _, err := a.Lease(t.Context(), lease.ID); err == nil {
		t.Error("the lease is still open, so its capacity is held by nothing")
	}

	if got := headroom(t, a, "small"); got != 1 {
		t.Errorf("the machine offers %d slots after its abandoned escrow was reclaimed", got)
	}
}

// CAPACITY COMES BACK ON PROOF THE CONTAINER IS GONE, which is what the node
// provides when it destroys one.
func TestResolvingAQuarantinedLeaseReturnsItsCapacity(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	held, err := a.Quarantined(t.Context())
	if err != nil {
		t.Fatalf("Quarantined: %v", err)
	}

	if len(held) != 1 || held[0].ID != lease.ID || held[0].Node != "epyc-1" {
		t.Fatalf("quarantined leases are %+v; an operator cannot see what is holding the "+
			"capacity", held)
	}

	if err := a.ResolveQuarantine(t.Context(), lease.ID, PhaseDone); err != nil {
		t.Fatalf("ResolveQuarantine: %v", err)
	}

	if got := headroom(t, a, "small"); got != 1 {
		t.Errorf("the machine offers %d slots after its container was confirmed gone; the "+
			"capacity never came back", got)
	}
}

// A NODE THAT COMES BACK REPORTING WHAT IT RUNS IS ALSO PROOF.
//
// The sweep only fires for a container that still exists. A host that rebooted
// has none, so nothing would ever confirm the destroy and the capacity would sit
// quarantined forever. Registration carries the inventory, and a quarantined
// lease missing from it has no container by definition.
func TestANodeThatComesBackWithoutTheContainerFreesIt(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	// PAST THE GRACE, because an absence taken moments after a lost launch is a
	// snapshot rather than a result — the daemon may still be creating the
	// container. Its own test is below.
	now = now.Add(2 * quarantineGrace)

	// It still runs SOMETHING, just not this.
	resolved, err := a.ResolveQuarantineFor(t.Context(), "epyc-1", []string{"some-other-lease"}, nodeEpoch(t, a))
	if err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	if resolved != 1 {
		t.Fatalf("a node reporting it is not running the container freed %d leases", resolved)
	}

	if got := headroom(t, a, "small"); got != 1 {
		t.Errorf("the machine offers %d slots after its host said the container is gone", got)
	}

	_ = lease
}

// AND A NODE STILL RUNNING IT KEEPS IT CHARGED. This is the direction that must
// never be wrong: a host that reports the container is alive is the one case
// where freeing the capacity sells the same slot twice.
func TestANodeStillRunningTheContainerKeepsItQuarantined(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)
	lease := busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	now = now.Add(2 * quarantineGrace)

	resolved, err := a.ResolveQuarantineFor(t.Context(), "epyc-1", []string{lease.ID}, nodeEpoch(t, a))
	if err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	if resolved != 0 {
		t.Errorf("freed %d leases whose containers the host says are still running", resolved)
	}

	if got := headroom(t, a, "small"); got != 0 {
		t.Errorf("the machine offered %d slots while its host says the container is still "+
			"there", got)
	}
}

// A QUARANTINED LEASE PINS ITS HOST'S BACKEND, and it is the LAST phase that
// should not.
//
// The reaper reaching this verdict is billet saying, in the ledger, that a
// container may still be running on that machine and nothing has confirmed
// otherwise. Letting the host change its provider on the strength of that turns
// the reaper's own conclusion into the thing that unlocks the move — after
// which the new backend cannot enumerate the old container, the host reports an
// inventory without it, and the quarantine is resolved by a machine that cannot
// see what it is vouching for.
func TestAQuarantinedLeasePinsItsHostsBackend(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	moved := NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderTart, VCPU: 4, Memory: 16 * config.GiB,
	}

	if _, err := a.RegisterNode(t.Context(), moved); !errors.Is(err, ErrWrongProvider) {
		t.Fatalf("a host holding quarantined compute changed its backend: %v", err)
	}
}

// AN ABSENCE TAKEN TOO SOON IS NOT PROOF.
//
// The rule custody already lives by: when a launch loses its response the daemon
// may still be creating the container, and a listing issued straight afterwards
// can overtake it and see nothing. Freeing the capacity on that hands the slot
// back moments before the container appears, and a second job lands on top of
// the first.
func TestAFreshQuarantineIsNotResolvedByAnEmptyInventory(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	// The node comes straight back and reports nothing running.
	resolved, err := a.ResolveQuarantineFor(t.Context(), "epyc-1", nil, nodeEpoch(t, a))
	if err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	if resolved != 0 {
		t.Errorf("a listing taken %s after the lease expired freed %d lease(s); the container "+
			"may not have appeared yet", 31*time.Second, resolved)
	}

	if got := headroom(t, a, "small"); got != 0 {
		t.Errorf("the machine offered %d slots on the strength of one early snapshot", got)
	}

	// Once the container has had time to appear and still has not, the absence
	// means what it says.
	now = now.Add(2 * quarantineGrace)

	resolved, err = a.ResolveQuarantineFor(t.Context(), "epyc-1", nil, nodeEpoch(t, a))
	if err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	if resolved != 1 {
		t.Errorf("a settled absence freed %d leases; capacity that can never come back is the "+
			"other half of this failure", resolved)
	}
}

// AN OVERTAKEN REGISTRATION CHANGES NOTHING, including this.
//
// Two registrations can be in flight — a node restarting twice, or a duplicate
// host. The one that arrives second is current, and the first must not
// terminalize a lease the second has just vouched for using a listing taken
// before that container was visible.
func TestAnOvertakenRegistrationDoesNotResolveQuarantine(t *testing.T) {
	now := time.Now().UTC()
	a := quarantineFleet(t, &now)

	busyLease(t, a)

	now = now.Add(31 * time.Second)

	if _, err := a.Reap(t.Context()); err != nil {
		t.Fatalf("Reap: %v", err)
	}

	stale := nodeEpoch(t, a)

	now = now.Add(2 * quarantineGrace)

	// A newer registration supersedes it.
	if _, err := a.RegisterNode(t.Context(), NodeRegistration{
		Name: "epyc-1", Provider: config.ProviderDocker, VCPU: 4, Memory: 16 * config.GiB,
	}); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	resolved, err := a.ResolveQuarantineFor(t.Context(), "epyc-1", nil, stale)
	if err != nil {
		t.Fatalf("ResolveQuarantineFor: %v", err)
	}

	if resolved != 0 {
		t.Errorf("an overtaken registration freed %d lease(s) using its own stale inventory",
			resolved)
	}
}

// THE HISTORY RECORDS WHAT ACTUALLY HAPPENED, not how the capacity came back.
//
// A job can finish and have its release lose the race to the reaper: the lease
// is quarantined, and the listener then resolves it with the destroy as proof.
// Archiving that as `failed` because of the route it took would record a job
// GitHub reported completed as one that did not — the same objection the launch
// path makes in the other direction, where calling an unstarted job `done` is
// the lie.
func TestResolvingAQuarantineKeepsTheCallersOutcome(t *testing.T) {
	for _, outcome := range []Phase{PhaseDone, PhaseFailed} {
		t.Run(string(outcome), func(t *testing.T) {
			now := time.Now().UTC()
			a := quarantineFleet(t, &now)
			lease := busyLease(t, a)

			now = now.Add(31 * time.Second)

			if _, err := a.Reap(t.Context()); err != nil {
				t.Fatalf("Reap: %v", err)
			}

			if err := a.ResolveQuarantine(t.Context(), lease.ID, outcome); err != nil {
				t.Fatalf("ResolveQuarantine: %v", err)
			}

			// The id advanceTo assigns; the lease was read before that happened, so
			// its own copy is still zero.
			got, err := a.HistoryOutcomesForRequest(t.Context(), 1)
			if err != nil {
				t.Fatalf("history: %v", err)
			}

			if len(got) != 1 || got[0] != string(outcome) {
				t.Errorf("job history records %v for a job that ended as %q", got, outcome)
			}
		})
	}
}
