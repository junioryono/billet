package node

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/server"
)

// A running job survives a controller restart.
//
// THE CASE THE PREVIOUS VERSION GOT WRONG, and it was wrong on a point of fact
// rather than of judgement: I justified destroying it by saying GitHub would
// requeue the job, and the scale-set documentation promises that only for a job
// "not acquired by a runner in time" — nothing about one a runner has already
// started. The runner in this container is talking to GitHub directly and may
// well finish; killing it is a deliberate job failure.
func TestRecoverAdoptsAJobThatIsStillRunning(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// A restart: empty maps, the container still running.
	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatalf("killed a job that was running fine: %v", p.live)
	}

	if !restarted.heldLeases()[lease.ID] {
		t.Error("the adopted job's lease is not held, so its capacity can be resold underneath it")
	}
}

// An adopted job's lease keeps being heartbeated, so the reaper leaves it alone.
//
// Without this the adoption is worse than useless: the container survives, the
// lease expires, the reaper hands the capacity back, and billet admits
// replacement work onto vCPU that is still busy.
func TestAnAdoptedLeaseIsKeptAlive(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	before, err := a.Lease(t.Context(), lease.ID)
	if err != nil {
		t.Fatalf("read the adopted lease: %v", err)
	}

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	// Still open, still running, still held.
	if _, err := a.Lease(t.Context(), before.ID); err != nil {
		t.Fatalf("the adopted lease went away while its container was still running: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatalf("Tend destroyed a running adopted job: %v", p.live)
	}
}

// When an adopted container stops, its lease is released and the container goes.
func TestAnAdoptedJobIsCleanedUpWhenItFinishes(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The job finishes on its own — the runner talked to GitHub without billet.
	p.stop(provider.InstanceName(lease.ID))

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("the finished container was not cleaned up: %v", p.live)
	}

	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("the finished job's lease still holds capacity: %v", err)
	}

	if restarted.heldLeases()[lease.ID] {
		t.Error("the lease is still in custody after being released")
	}
}

// An adopted container is destroyed once GitHub says its job is over.
//
// The ordinary end of an adoption. The listener receives JobCompleted, releases
// the lease, and the next Tend finds the heartbeat refused — which is the signal
// that nothing is waiting for this container any more.
func TestAnAdoptedJobIsDestroyedWhenItsLeaseIsReleased(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// GitHub reported the job complete and the listener released the lease. The
	// container is STILL RUNNING as far as the backend is concerned.
	if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("a container whose lease is gone was left running: %v", p.live)
	}
}

// A container that is NOT running and whose lease is gone is destroyed at
// startup rather than adopted. Nothing is waiting for its result.
func TestRecoverDestroysWhatNothingIsWaitingFor(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	p.add(&provider.Instance{
		ID: "leftover", Name: provider.InstanceName("deadbeef"), Running: true,
	})

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("adopted a container whose lease does not exist: %v", p.live)
	}
}

// An exited container is never adopted, even with an open lease. There is no
// work left to protect, only a name and a disk.
func TestRecoverDoesNotAdoptAnExitedContainer(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	p.stop(provider.InstanceName(lease.ID))

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("kept an exited container: %v", p.live)
	}

	if restarted.heldLeases()[lease.ID] {
		t.Error("took custody of a lease whose container had already exited")
	}
}

// ============================================================================
// The other half: a launch that could not confirm its own cleanup.
// ============================================================================

// An unconfirmed cleanup keeps the capacity, and says so to the caller.
//
// The listener releases the lease on an ordinary launch error, so returning one
// here handed the capacity back while a container might still be running on it.
// ErrCustody is how the runner says "do not".
func TestAnUnconfirmedCleanupHoldsTheCapacity(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		kind:         config.ProviderDocker,
		launchErr:    errors.New("context deadline exceeded"),
		startsAnyway: true,
		findErr:      errors.New("cannot connect to the docker daemon"),
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"})
	if err == nil {
		t.Fatal("a launch that failed reported success")
	}

	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("the caller was not told the capacity is held, so it will release it: %v", err)
	}

	if !r.heldLeases()[lease.ID] {
		t.Error("the lease was not taken into custody")
	}
}

// A CONFIRMED cleanup does not hold anything. The capacity should go straight
// back — holding it would strand the budget on every ordinary launch failure.
func TestAConfirmedCleanupHoldsNothing(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		kind:         config.ProviderDocker,
		launchErr:    errors.New("context deadline exceeded"),
		startsAnyway: true,
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"})
	if err == nil {
		t.Fatal("a launch that failed reported success")
	}

	if errors.Is(err, server.ErrCustody) {
		t.Error("held capacity for compute that was confirmed destroyed")
	}

	if len(r.heldLeases()) != 0 {
		t.Error("took custody after a successful cleanup")
	}
}

// Custody ends when the daemon comes back and the stray is destroyed.
func TestCustodyEndsWhenTheStrayIsFinallyDestroyed(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		kind:         config.ProviderDocker,
		launchErr:    errors.New("context deadline exceeded"),
		startsAnyway: true,
		findErr:      errors.New("cannot connect to the docker daemon"),
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err == nil {
		t.Fatal("a launch that failed reported success")
	}

	// While the daemon is unreachable, Tend cannot make progress and must not
	// pretend otherwise — the capacity stays held.
	if err := r.Tend(t.Context()); err == nil {
		t.Error("Tend reported success while it could not reach the backend")
	}

	if !r.heldLeases()[lease.ID] {
		t.Fatal("dropped custody while the stray was still unaccounted for")
	}

	p.findErr = nil

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("the stray survived: %v", p.live)
	}

	if len(r.heldLeases()) != 0 {
		t.Error("kept holding capacity after the stray was destroyed")
	}

	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("the held lease was never released: %v", err)
	}
}

// A sweep must not touch what Tend is holding.
//
// Both run on the same tick. Without the exclusion the sweep would destroy an
// adopted container the instant its lease went terminal, before Tend had the
// chance to release the capacity in the right order.
func TestSweepLeavesHeldComputeAlone(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The lease goes terminal, so the sweep would otherwise call this an orphan.
	if err := a.Release(t.Context(), lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		t.Fatalf("Release: %v", err)
	}

	if err := restarted.Sweep(t.Context()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatal("the sweep destroyed compute that Tend was responsible for")
	}
}

// The custody entry records when it started, so a stuck one is diagnosable.
func TestCustodyRecordsWhenItStarted(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	frozen := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return frozen }

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	restarted.now = func() time.Time { return frozen }

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	for _, c := range restarted.custodySnapshot() {
		if c.since != frozen {
			t.Errorf("custody started at %v, want the clock's %v", c.since, frozen)
		}
	}
}

// A release that FAILS keeps the lease in custody.
//
// The subtlest of the custody rules and the one that had no test until the
// ledger became injectable. If the release fails, the capacity is still recorded
// as held — dropping the entry then leaves nothing to retry it, and the lease
// sits until the reaper eventually notices a heartbeat that stopped arriving.
// Meanwhile the runner has already destroyed the compute, so the host has
// capacity it believes is busy and is not.
func TestCustodyIsKeptWhenTheReleaseFails(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)

	// Releases fail; everything else works.
	store := &brittleStore{LeaseStore: a, releaseErr: errors.New("database is locked")}

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(store, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The job finishes, so Tend destroys the container and tries to release.
	p.stop(provider.InstanceName(lease.ID))

	if err := restarted.Tend(t.Context()); err == nil {
		t.Fatal("Tend reported success though the lease could not be released")
	}

	if !restarted.heldLeases()[lease.ID] {
		t.Fatal("dropped custody after a failed release, so nothing will ever retry it")
	}

	// And it retries: once the ledger recovers, the next tick finishes the job.
	store.releaseErr = nil

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the ledger recovered: %v", err)
	}

	if len(restarted.heldLeases()) != 0 {
		t.Error("custody was never resolved even after the release succeeded")
	}
}

// brittleStore is a LeaseStore whose operations can be made to fail, so the
// runner's refusal paths are reachable from a test.
type brittleStore struct {
	LeaseStore

	releaseErr   error
	heartbeatErr error
}

func (b *brittleStore) Release(
	ctx context.Context, leaseID string, epoch int64, outcome alloc.Phase,
) error {
	if b.releaseErr != nil {
		return b.releaseErr
	}

	return b.LeaseStore.Release(ctx, leaseID, epoch, outcome)
}

func (b *brittleStore) Heartbeat(ctx context.Context, leaseID string, epoch int64) error {
	if b.heartbeatErr != nil {
		return b.heartbeatErr
	}

	return b.LeaseStore.Heartbeat(ctx, leaseID, epoch)
}

// A ledger that cannot be reached is reported, not treated as a lease that went
// away — the difference between "nobody wants this any more" and "I could not
// ask", which decide opposite things about a running container.
func TestTendReportsAnUnreachableLedger(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	store := &brittleStore{LeaseStore: a, heartbeatErr: errors.New("database is locked")}

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(store, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if err := restarted.Tend(t.Context()); err == nil {
		t.Fatal("an unreachable ledger was treated as a successful tick")
	}

	if len(p.live) != 1 {
		t.Fatal("destroyed a running job because the ledger could not be reached")
	}
}
