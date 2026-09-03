package server

import (
	"errors"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// forceFixture is a listener holding running work, on a sealed deployment, with
// a recording runner.
type forceFixture struct {
	a    *alloc.Allocator
	l    *Listener
	db   *state.DB
	tier string

	mu        sync.Mutex
	destroyed []int64
	failNext  map[int64]error
}

func (f *forceFixture) took() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]int64(nil), f.destroyed...)
}

// newForceFixture builds a sealed deployment with one tier and a listener.
func newForceFixture(t *testing.T) *forceFixture {
	t.Helper()

	tiers := []config.Tier{tier("billet-4vcpu-a")}
	db := openState(t)

	a, err := alloc.New(db, alloc.Limits{MaxVCPU: 64, MaxMemory: 512 * config.GiB},
		tiers, alloc.WithLeaseTTL(outlivesTheDrain))
	if err != nil {
		t.Fatalf("alloc.New: %v", err)
	}

	// A HOST WHOSE BACKEND MATCHES THE TIER, because capacity is per machine and
	// placement is per provider: a firecracker tier can never land on a docker
	// host, so a deployment with only the wrong backend registered advertises zero
	// and every Reserve here would fail for a reason that has nothing to do with
	// forcing.
	if _, err := a.RegisterNode(t.Context(), alloc.NodeRegistration{
		Name: "test-host", Provider: config.ProviderFirecracker,
		VCPU: 1 << 20, Memory: 1 << 20 * config.GiB,
	}); err != nil {
		t.Fatalf("registering the default host: %v", err)
	}

	f := &forceFixture{a: a, db: db, tier: tiers[0].Label, failNext: map[int64]error{}}

	runner := &fakeRunner{onDestroy: func(id int64) error {
		f.mu.Lock()
		err := f.failNext[id]
		if err == nil {
			f.destroyed = append(f.destroyed, id)
		}
		f.mu.Unlock()

		return err
	}}

	f.l = NewListener(a, f.tier, &fakeSession{}, WithRunner(runner),
		WithDrainGrace(notDrainingHere), stopsWithoutWaiting())

	return f
}

// seal puts the deployment where a force may be taken, and returns the
// generation.
func (f *forceFixture) seal(t *testing.T) int64 {
	t.Helper()

	current, err := f.db.Admission(t.Context())
	if err != nil {
		t.Fatalf("Admission: %v", err)
	}

	sealed, err := f.db.Seal(t.Context(), state.SealRequest{
		Expect: current.Generation, Provenance: state.ProvenanceOperator,
		Reason: "wedged", Actor: "ops",
	})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}

	return sealed.Generation
}

// request records a force covering the given leases.
func (f *forceFixture) request(t *testing.T, targets ...state.ForceTarget) state.ForceDestroy {
	t.Helper()

	recorded, err := f.a.RequestForceDestroy(t.Context(), state.ForceDestroyRequest{
		ExpectAdmission: f.seal(t), Reason: "wedged", Actor: "ops", Targets: targets,
	})
	if err != nil {
		t.Fatalf("RequestForceDestroy: %v", err)
	}

	return recorded
}

func targetFor(lease *alloc.Lease, tierLabel string, request int64) state.ForceTarget {
	return state.ForceTarget{
		LeaseID: lease.ID, Tier: tierLabel, Node: lease.Node, RunID: "9001",
		SchedulerRequest: request, Phase: string(alloc.PhaseBusy),
	}
}

// AN OPERATOR'S FORCE DESTROYS RUNNING COMPUTE AND HANDS BACK ITS CAPACITY.
//
// This is the half of the drain contract that removing the drain deadline
// deliberately left with no caller: `destroyAll(includeRunning: true)` existed
// and nothing could reach it, so the only running work billet could end was
// whatever a node's own teardown happened to remove. An operator with a wedged
// fleet had no orderly way out.
func TestAForceDestroysRunningComputeAndReturnsItsCapacity(t *testing.T) {
	f := newForceFixture(t)
	lease := holdRunning(t, f.l, f.a, f.tier, 7)

	recorded := f.request(t, targetFor(lease, f.tier, 7))

	f.l.forceDestroy(t.Context())

	if got := f.took(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("the force destroyed %v, want [7]", got)
	}

	// FAILED, NOT DONE. A job whose runner was destroyed while it was executing
	// did not finish, and recording `done` puts a lie in the history of somebody's
	// build.
	//
	// READ FROM THE HISTORY, because a terminal lease is deliberately not readable
	// as an open one — asserting on Lease's error would prove only that something
	// removed the row.
	outcome, err := f.a.HistoryOutcome(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the archived outcome after the force: %v", err)
	}

	if outcome != string(alloc.PhaseFailed) {
		t.Errorf("the forced lease is archived as %s, want failed", outcome)
	}

	targets, err := f.a.ForceTargets(t.Context(), recorded.Generation)
	if err != nil {
		t.Fatalf("ForceTargets: %v", err)
	}

	if len(targets) != 1 || targets[0].State != state.ForceTargetDestroyed {
		t.Errorf("the record reads %+v, want one destroyed", targets)
	}

	// AND THE REQUEST IS CLOSED, so a later force is not blocked by one that has
	// already done its work.
	if _, open, err := f.a.OpenForceDestroy(t.Context()); err != nil {
		t.Fatalf("OpenForceDestroy: %v", err)
	} else if open {
		t.Error("the force stayed open after settling every target")
	}
}

// A FORCE DESTROYS ONLY WHAT THE OPERATOR APPROVED.
//
// The target set is enumerated, shown to a person, and recorded before anything
// is destroyed. A job that started between the diagnostic and the confirmation
// was never approved — destroying it would be exactly the implicit teardown this
// whole mechanism exists to refuse.
func TestAForceDestroysOnlyTheApprovedLeases(t *testing.T) {
	f := newForceFixture(t)
	approved := holdRunning(t, f.l, f.a, f.tier, 8)
	// Arrived after the operator read the list.
	holdRunning(t, f.l, f.a, f.tier, 7)

	f.request(t, targetFor(approved, f.tier, 8))

	f.l.forceDestroy(t.Context())

	got := f.took()
	if len(got) != 1 || got[0] != 8 {
		t.Fatalf("the force destroyed %v, want only the approved [8]", got)
	}
}

// A DESTROY THAT DID NOT CONFIRM RELEASES NOTHING.
//
// A failed teardown is not proof the container survived, and it is not proof it
// is gone either. Returning the capacity on it would sell a slot whose guest may
// still be on the host, which is the overcommit the whole ordering prevents.
func TestAForceThatCannotDestroyLeavesTheCapacityCharged(t *testing.T) {
	f := newForceFixture(t)
	lease := holdRunning(t, f.l, f.a, f.tier, 7)

	f.mu.Lock()
	f.failNext[7] = errors.New("the node did not answer")
	f.mu.Unlock()

	recorded := f.request(t, targetFor(lease, f.tier, 7))

	f.l.forceDestroy(t.Context())

	after, err := f.a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the lease after a failed force: %v", err)
	}

	if after.Phase.Terminal() {
		t.Errorf("a force whose destroy failed terminalised lease %s as %s; its container "+
			"may still be running and its capacity must stay charged", after.ID, after.Phase)
	}

	targets, err := f.a.ForceTargets(t.Context(), recorded.Generation)
	if err != nil {
		t.Fatalf("ForceTargets: %v", err)
	}

	if len(targets) != 1 || targets[0].State != state.ForceTargetFailed {
		t.Fatalf("the record reads %+v, want one failed", targets)
	}

	// AND IT SAYS WHY, because the operator's next question is whether to go and
	// look at the host.
	if targets[0].Detail == "" {
		t.Error("a failed force target recorded no detail")
	}
}

// COMPUTE A RESTART RE-ADOPTED IS STILL REACHABLE.
//
// After a control-plane restart, a lease that was online or busy is held by the
// NODE — it is adopted rather than managed, so it is not in the listener's
// `running` map at all. A force keyed on that map would silently do nothing after
// exactly the restart that most often precedes one, and would report success.
func TestAForceReachesComputeThatOutlivedAControlPlane(t *testing.T) {
	f := newForceFixture(t)

	// Staged as production leaves it: the lease is real and durable, and this
	// listener holds no in-memory record of it.
	lease, err := f.a.Reserve(t.Context(), f.tier)
	if err != nil {
		t.Fatalf("reserve: %v", err)
	}

	if err := f.a.Assign(t.Context(), lease.ID, lease.Epoch, 7, 7); err != nil {
		t.Fatalf("assign: %v", err)
	}

	f.l.mu.Lock()
	_, managed := f.l.running[7]
	f.l.mu.Unlock()

	if managed {
		t.Fatal("the fixture put the lease in `running`; this test is about the case where " +
			"it is not")
	}

	f.request(t, targetFor(lease, f.tier, 7))

	f.l.forceDestroy(t.Context())

	if got := f.took(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("the force destroyed %v, want [7] — adopted compute must be reachable", got)
	}
}

// A FORCED LEASE STOPS BEING ADVERTISED IMMEDIATELY.
//
// destroyAll does not clear `running` — on a shutdown, releaseAll is what walks
// it — so a forced lease used to stay in that map pointing at a row the force had
// just terminalised, and capacity() went on counting it. The listener then
// advertised a slot it did not have until the next heartbeat happened to get
// ErrLeaseNotFound and drop it. That is an overcommit window created by a map
// entry nobody removed, and it is exactly the kind of thing the escrow ordering
// exists to prevent.
func TestAForcedLeaseStopsBeingCountedAsRunning(t *testing.T) {
	f := newForceFixture(t)
	lease := holdRunning(t, f.l, f.a, f.tier, 7)

	if f.l.Running() != 1 {
		t.Fatalf("the fixture is holding %d running lease(s), want 1", f.l.Running())
	}

	f.request(t, targetFor(lease, f.tier, 7))

	f.l.forceDestroy(t.Context())

	if got := f.l.Running(); got != 0 {
		t.Errorf("after a force the listener still counts %d running lease(s); it would "+
			"advertise capacity for a lease that no longer exists", got)
	}
}

// NOTHING HAPPENS WITHOUT A RECORDED REQUEST. The poll loop calls this every
// iteration, so an open request is the ONLY thing that may make it destroy
// anything at all.
func TestForceDestroyDoesNothingWithoutARequest(t *testing.T) {
	f := newForceFixture(t)
	holdRunning(t, f.l, f.a, f.tier, 7)

	f.l.forceDestroy(t.Context())

	if got := f.took(); len(got) != 0 {
		t.Errorf("a listener with no force request destroyed %v", got)
	}
}
