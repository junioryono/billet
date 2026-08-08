package node

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
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
	leaseErr     error

	// heartbeats counts renewals, so a test can prove the keep-alive loop is
	// actually doing its one job.
	heartbeats atomic.Int64
}

func (b *brittleStore) Lease(ctx context.Context, leaseID string) (*alloc.Lease, error) {
	if b.leaseErr != nil {
		return nil, b.leaseErr
	}

	return b.LeaseStore.Lease(ctx, leaseID)
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
	b.heartbeats.Add(1)

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
	store := &brittleStore{LeaseStore: a}

	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(store, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	// Adoption succeeds; the ledger fails afterwards.
	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	store.heartbeatErr = errors.New("database is locked")

	if err := restarted.Tend(t.Context()); err == nil {
		t.Fatal("an unreachable ledger was treated as a successful tick")
	}

	if len(p.live) != 1 {
		t.Fatal("destroyed a running job because the ledger could not be reached")
	}

	if !restarted.heldLeases()[lease.ID] {
		t.Error("dropped custody because the ledger was briefly unreachable")
	}
}

// ADOPTION RENEWS THE LEASE IMMEDIATELY, and a failure to renew aborts recovery.
//
// Billet may have been down for longer than a lease TTL, so the lease being
// adopted can already be expired — and the control plane runs a reap BEFORE its
// first tend. Without the renewal, the reaper would terminalize the lease that
// had just been adopted, hand its capacity back, and let a listener advertise it
// while the container carried on running.
func TestRecoverRenewsTheLeaseItAdopts(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	store := &brittleStore{LeaseStore: a, heartbeatErr: errors.New("database is locked")}
	restarted := New(store, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err == nil {
		t.Fatal("recovery reported success though it could not renew the lease it adopted; " +
			"the reaper would take that capacity while the container still runs")
	}

	// And it did not destroy the job on its way out.
	if len(p.live) != 1 {
		t.Fatalf("a failed renewal destroyed a running job: %v", p.live)
	}
}

// A read failure while checking a surviving instance's lease is NOT a licence to
// destroy it.
//
// "Could not verify" and "confirmed gone" reach the same line of code unless
// they are told apart, and one of them force-kills a running job over a
// transient database error.
func TestRecoverDoesNotDestroyWhenItCannotReadTheLease(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	store := &brittleStore{LeaseStore: a, leaseErr: errors.New("disk I/O error")}
	restarted := New(store, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err == nil {
		t.Fatal("recovery reported success though it could not read the lease")
	}

	if len(p.live) != 1 {
		t.Fatalf("destroyed a running job because its lease could not be read: %v", p.live)
	}
}

// A completion for an adopted job ends its custody.
//
// The restarted listener has no record of the request, so its own completion
// handling releases nothing. Without this an adopted container whose job GitHub
// later reports finished is held until the hard bound — hours of capacity for a
// job that is over.
func TestACompletionEndsAnAdoption(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	const requestID = 4242

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// GitHub reports the job finished. This arrives at Destroy, whose in-memory
	// map is empty because a different process started the container.
	if err := restarted.Destroy(t.Context(), lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("an adopted job's container outlived its completion: %v", p.live)
	}

	if len(restarted.heldLeases()) != 0 {
		t.Error("capacity is still held for a job that has finished")
	}
}

// A discarded entry terminalizes as FAILED, not done.
//
// Durable history has to say what happened. "Done" for a runner that never
// started is a lie a later investigation would have to unpick.
func TestADiscardedCustodyRecordsAFailure(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		kind:         config.ProviderDocker,
		launchErr:    errors.New("context deadline exceeded"),
		startsAnyway: true,
		findErr:      errors.New("cannot connect to the docker daemon"),
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	frozen := time.Now()
	r.now = func() time.Time { return frozen }

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err == nil {
		t.Fatal("a launch that failed reported success")
	}

	for _, c := range r.custodySnapshot() {
		if c.outcome != alloc.PhaseFailed {
			t.Errorf("a failed launch would be recorded as %q, want %q", c.outcome, alloc.PhaseFailed)
		}
	}
}

// An absent stray is not believed straight away.
//
// A `docker ps` issued right after a lost create can overtake the daemon and see
// nothing. Releasing on that hands the capacity back moments before the
// container appears, which is the interleaving a single observation cannot rule
// out.
func TestAnAbsentStrayIsNotBelievedImmediately(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		kind:      config.ProviderDocker,
		launchErr: errors.New("context deadline exceeded"),
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	frozen := time.Now()
	r.now = func() time.Time { return frozen }

	lease := assignedLease(t, a)

	err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"})
	if err == nil {
		t.Fatal("a launch that failed reported success")
	}

	if !errors.Is(err, server.ErrCustody) {
		t.Fatalf("released the capacity on a single negative observation: %v", err)
	}

	// Still nothing there, but not yet long enough to be sure.
	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if !r.heldLeases()[lease.ID] {
		t.Fatal("stopped looking for a possible stray before the grace period was up")
	}

	// Past the grace period, an absence is believed and the capacity goes back.
	r.now = func() time.Time { return frozen.Add(strayGrace + time.Second) }

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if len(r.heldLeases()) != 0 {
		t.Error("held the capacity forever for a stray that never appeared")
	}
}

// Held compute is NOT destroyed on a timer by default.
//
// I gave this a 24-hour bound and it was wrong. Elapsed time is not evidence
// that a job has stopped making progress — billet imposes no job limit, and
// self-hosted runners are routinely configured past GitHub's six-hour default —
// so a legitimately long job restarted shortly after it began would have been
// killed a day later for no visible reason.
func TestHeldComputeIsNotDestroyedOnATimerByDefault(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	frozen := time.Now()

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	restarted.now = func() time.Time { return frozen }

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// A week later, with no bound configured.
	restarted.now = func() time.Time { return frozen.Add(7 * 24 * time.Hour) }

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatal("destroyed a running job on a timer; only a completion, an observed exit " +
			"or an operator may authorise that")
	}
}

// An operator who knows their longest job can set a bound, and a job killed by
// it is recorded as a FAILURE.
//
// Keeping the adopted entry's "done" would archive a teardown billet chose as a
// job that finished — a lie a later investigation would have to unpick.
func TestAConfiguredBoundDestroysAndRecordsAFailure(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	frozen := time.Now()

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	restarted.now = func() time.Time { return frozen }
	restarted.maxCustody = 2 * time.Hour

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Inside the bound: left alone.
	restarted.now = func() time.Time { return frozen.Add(time.Hour) }

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if len(p.live) != 1 {
		t.Fatal("destroyed a job that was still inside the configured bound")
	}

	// Past it: destroyed, and recorded as failed IN THE LEDGER.
	//
	// Asserted against job_history rather than the in-memory custody field, which
	// is what the first version checked — and which would stay green if finish()
	// hardcoded PhaseDone, since the field it read is only ever an input.
	restarted.now = func() time.Time { return frozen.Add(3 * time.Hour) }

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("held compute past the configured bound: %v", p.live)
	}

	if got := archivedOutcome(t, a, lease.ID); got != string(alloc.PhaseFailed) {
		t.Errorf("a job billet killed was archived as %q, want %q", got, alloc.PhaseFailed)
	}
}

// archivedOutcome reads how a finished lease was recorded, which is the only
// durable statement about what happened to a job.
func archivedOutcome(t *testing.T, a *alloc.Allocator, leaseID string) string {
	t.Helper()

	outcome, err := a.HistoryOutcome(t.Context(), leaseID)
	if err != nil {
		t.Fatalf("read job history for %s: %v", leaseID, err)
	}

	return outcome
}

// A redelivered assignment does not start a second runner for a job billet
// adopted.
//
// After a crash before an assignment was acknowledged, GitHub redelivers it and
// the restarted listener has empty maps. Without a guard it escrows another
// lease and starts another container while the adopted one carries on — and the
// extra runner is a live registration that can pick up unrelated work.
func TestAnAdoptedRequestIsNotLaunchedAgain(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	const requestID = 5150

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The redelivery: a brand-new lease for the SAME request.
	second := assignedLease(t, a)

	err := restarted.Launch(t.Context(), second, Job{RequestID: lease.RequestID, Event: "push"})
	if err == nil {
		t.Fatal("started a second runner for a job billet is already running")
	}

	if len(p.live) != 1 {
		t.Fatalf("two containers exist for one job: %v", p.live)
	}
}

// A completion clears BOTH the running entry and the custody entry.
//
// A request can have each — a redelivered assignment after a crash produces
// exactly that — and cleaning up only one leaves the other running with nothing
// left to notice it.
func TestACompletionClearsBothRunningAndHeldCompute(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	const requestID = 6260

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// Force the both-at-once state directly: the adopted entry plus a running
	// entry for the same request, which is what a redelivery would produce if the
	// launch guard were removed.
	extra := assignedLease(t, a)
	inst := &provider.Instance{
		ID: "second-" + extra.ID, Name: provider.InstanceName(extra.ID), Running: true,
	}
	p.add(inst)

	restarted.mu.Lock()
	restarted.running[lease.RequestID] = inst
	restarted.mu.Unlock()

	if err := restarted.Destroy(t.Context(), lease.RequestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("a completion left compute behind: %v", p.live)
	}

	if len(restarted.heldLeases()) != 0 {
		t.Error("capacity is still held after the job completed")
	}
}

// A custody entry that has been REPLACED must not be deleted by the old one.
//
// Tend works from a snapshot and calls into the backend between reads, so an
// entry can be replaced for the same lease while an older call is finishing. A
// delete keyed only on the lease id then drops the NEW entry — and with it, all
// knowledge of compute that exists. The lease goes back into the budget and the
// container runs on unaccounted for, which is the exact failure custody exists
// to prevent, reached from inside custody itself.
func TestFinishingAStaleEntryDoesNotDropItsReplacement(t *testing.T) {
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

	stale := restarted.custodySnapshot()
	if len(stale) != 1 {
		t.Fatalf("expected one custody entry, got %d", len(stale))
	}

	// A NEW entry lands for the same lease while the old call is in flight.
	replacement := &custody{
		leaseID: lease.ID,
		epoch:   lease.Epoch,
		name:    provider.InstanceName(lease.ID),
		outcome: alloc.PhaseDone,
		since:   time.Now(),
	}

	restarted.mu.Lock()
	restarted.custody[lease.ID] = replacement
	restarted.mu.Unlock()

	// The old call finishes. It must not remove the replacement.
	if err := restarted.finish(t.Context(), stale[0]); err != nil {
		t.Fatalf("finish: %v", err)
	}

	restarted.mu.Lock()
	held := restarted.custody[lease.ID]
	restarted.mu.Unlock()

	if held != replacement {
		t.Fatal("a stale custody entry deleted its replacement, so billet has forgotten " +
			"compute that still exists")
	}
}

// A completion arriving while the periodic tick runs must not race it.
//
// These are genuinely different goroutines in production: completions come from
// a listener, the tick from the reaper. Both end up in tendOne, which mutates
// the entry in place and issues backend calls between reads — so two of them on
// one entry destroy the same instance twice, and a delete can drop an entry that
// was just replaced.
//
// Serializing only the FLAG WRITE was not enough, which is what this exists to
// catch: it is the same bug the serialization was added for, moved one line
// down. Run under -race, which is how the suite runs.
func TestACompletionDoesNotRaceTheTick(t *testing.T) {
	t.Parallel()

	// The delay is what makes this a test rather than a coin flip: it holds both
	// callers inside the transition long enough to genuinely overlap.
	p := &fakeProvider{kind: config.ProviderDocker, findDelay: 20 * time.Millisecond}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	const requestID = 7777

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	var wg sync.WaitGroup

	start := make(chan struct{})

	wg.Add(2)

	go func() {
		defer wg.Done()

		<-start

		// The listener's completion.
		if err := restarted.Destroy(t.Context(), lease.RequestID); err != nil {
			t.Errorf("Destroy: %v", err)
		}
	}()

	go func() {
		defer wg.Done()

		<-start

		// The reaper's tick.
		if err := restarted.Tend(t.Context()); err != nil {
			t.Errorf("Tend: %v", err)
		}
	}()

	close(start)
	wg.Wait()

	// Whichever won, the outcome is the same: the compute is gone and nothing is
	// still holding capacity for it.
	if len(p.live) != 0 {
		t.Errorf("compute survived a completion: %v", p.live)
	}

	if len(restarted.heldLeases()) != 0 {
		t.Error("capacity is still held for a job that has finished")
	}
}

// KeepAlive renews held leases on its own clock, without touching the backend.
//
// THE SEPARATION IS THE POINT. Renewal used to happen inside Tend, which runs
// after the reaper on a shared tick and makes unbounded provider calls — so a
// slow `docker ps` delayed the next renewal without delaying the next reap, and
// anything longer than the lease TTL let the reaper reclaim capacity billet was
// holding on purpose while its container ran on.
func TestKeepAliveRenewsHeldLeasesWhileTendIsBlocked(t *testing.T) {
	t.Parallel()

	// A provider that blocks for an hour inside Find, which is the situation that
	// used to starve renewal: Tend calls Find, so anything sharing Tend's
	// schedule stops for as long as the backend does.
	p := &fakeProvider{kind: config.ProviderDocker, findDelay: time.Hour}
	a, host := newAllocatorWithHost(t)

	warm := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	if err := warm.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	store := &brittleStore{LeaseStore: a}
	r := New(store, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	r.ttl = func() time.Duration { return 30 * time.Millisecond }

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// TEND IS RUNNING AND STUCK, which is the whole point. Without it an
	// implementation that simply called Tend — heartbeating once and then
	// blocking in Find — would satisfy a test that only waits for one renewal.
	go func() {
		//nolint:errcheck // it is expected to block, not to return
		_ = r.Tend(ctx)
	}()

	// Counted from AFTER the blocked Tend has taken its own heartbeat, so the
	// renewals below are the keep-alive's and nothing else's. (The first version
	// of this test never started the keep-alive at all, and the single heartbeat
	// it saw was Tend's — which is exactly the confusion the count is here to
	// prevent.)
	time.Sleep(50 * time.Millisecond)

	before := store.heartbeats.Load()

	go r.KeepAlive(ctx)

	// SEVERAL renewals, not one. One proves the loop started; several prove it
	// keeps going while the backend is wedged.
	const want = 3

	deadline := time.Now().Add(15 * time.Second)

	for store.heartbeats.Load() < before+want {
		if time.Now().After(deadline) {
			t.Fatalf("the keep-alive renewed %d times while Tend was blocked, want at least %d; "+
				"renewal is sharing a schedule with provider I/O",
				store.heartbeats.Load()-before, want)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// KeepAlive stops when its context does, rather than outliving the process it
// belongs to.
func TestKeepAliveStopsWithItsContext(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	r.ttl = func() time.Duration { return 30 * time.Millisecond }

	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan struct{})

	go func() {
		r.KeepAlive(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("KeepAlive outlived its context")
	}
}

// A renewal that fails does not end custody or destroy anything.
//
// KeepAlive's one job is to renew; deciding that compute should go belongs to
// Tend. A keep-alive that tore something down on a transient ledger error would
// put teardown on a path that must stay cheap and must never block.
func TestKeepAliveDoesNotActOnARenewalFailure(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)

	warm := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	if err := warm.Launch(t.Context(), lease, Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	store := &brittleStore{LeaseStore: a}
	r := New(store, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	store.heartbeatErr = errors.New("database is locked")

	r.renewHeld(t.Context())

	if !r.heldLeases()[lease.ID] {
		t.Error("dropped custody because a single renewal failed")
	}

	if len(p.live) != 1 {
		t.Error("the keep-alive destroyed compute; only Tend may do that")
	}

	// A lease that is genuinely GONE is not an error worth reporting on this
	// path either — Tend is about to clean it up.
	store.heartbeatErr = alloc.ErrLeaseNotFound

	r.renewHeld(t.Context())

	if !r.heldLeases()[lease.ID] {
		t.Error("the keep-alive removed a custody entry; that is Tend's decision")
	}
}

// A custody error does not abandon the running instance.
//
// A completion for a request that has BOTH — which a redelivered assignment
// after a crash produces — used to return on the first error, so the normal
// running container was never destroyed. The listener treats that error as
// non-fatal and acknowledges the message, and nothing ever comes back for the
// abandoned half: its lease keeps being heartbeated and its container keeps
// running.
//
// Both are attempted; the errors are joined so the caller still learns cleanup
// was incomplete. What it must not do is choose which half to leak.
func TestACustodyErrorDoesNotAbandonTheRunningInstance(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)

	warm := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)
	lease := assignedLease(t, a)

	// THE LEASE'S OWN request id, not a constant. assignedLease writes its own
	// value into the ledger, and recovery reads the request id back from THERE —
	// so a test that launches under a different number files custody under one id
	// and then asks about another, and Destroy correctly finds nothing. That is
	// the same identity confusion this commit fixes in hold(), arriving from the
	// other direction, and it is why this test was green for a while against a
	// bug it was written to catch.
	requestID := lease.RequestID

	if err := warm.Launch(t.Context(), lease, Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Releases fail, so finishing the custody entry errors.
	store := &brittleStore{LeaseStore: a, releaseErr: errors.New("database is locked")}
	restarted := New(store, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The other half: a normal running entry for the same request.
	second := assignedLease(t, a)
	running := &provider.Instance{
		ID: "running-" + second.ID, Name: provider.InstanceName(second.ID), Running: true,
	}
	p.add(running)

	restarted.mu.Lock()
	restarted.running[requestID] = running
	restarted.mu.Unlock()

	// The completion arrives. It is EXPECTED to report an error, because the
	// custody half genuinely could not be released.
	if err := restarted.Destroy(t.Context(), requestID); err == nil {
		t.Fatal("reported success though the custody half could not be released")
	}

	// ...but the running half must be gone regardless.
	if _, stillThere := p.live[running.Name]; stillThere {
		t.Error("a custody error left the running container behind; the listener will " +
			"acknowledge the completion and nothing will come back for it")
	}
}

// A completion clears EVERY custody entry for its request, not the first found.
//
// One request should only ever have one entry — the launch guard sees to that —
// but "should" is doing a lot of work in a function whose whole job is cleaning
// up after states that should not exist. Stopping at the first match would
// destroy one container, acknowledge the completion, and leave the rest running
// with nothing that will ever look for them again.
func TestACompletionClearsEveryEntryForItsRequest(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, []config.Tier{dockerTier()}, nil)

	const requestID = 9090

	// Two entries for one request, built directly: reaching this state through
	// the normal path is exactly what the launch guard prevents.
	for range 2 {
		lease := assignedLease(t, a)

		inst := &provider.Instance{
			ID: "inst-" + lease.ID, Name: provider.InstanceName(lease.ID), Running: true,
		}
		p.add(inst)

		lease.RequestID = requestID

		r.adopt(lease, inst)
	}

	if len(r.heldLeases()) != 2 {
		t.Fatalf("expected two custody entries, got %d", len(r.heldLeases()))
	}

	if err := r.Destroy(t.Context(), requestID); err != nil {
		t.Fatalf("Destroy: %v", err)
	}

	if len(p.live) != 0 {
		t.Fatalf("a completion left %d container(s) running: %v", len(p.live), p.live)
	}

	if len(r.heldLeases()) != 0 {
		t.Errorf("capacity is still held for %d entries after the job completed",
			len(r.heldLeases()))
	}
}
