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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// A restart: empty maps, the container still running.
	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	p.stop(provider.InstanceName(lease.ID))

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"})
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"})
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err == nil {
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	frozen := time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)
	r.now = func() time.Time { return frozen }

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)
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
	store := &brittleStore{LeaseStore: a}
	store.failRelease(errors.New("database is locked"))

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(store, host, &fakeJIT{setID: 7}, p, nil)

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
	store.failRelease(nil)

	if err := restarted.Tend(t.Context()); err != nil {
		t.Fatalf("Tend after the ledger recovered: %v", err)
	}

	if len(restarted.heldLeases()) != 0 {
		t.Error("custody was never resolved even after the release succeeded")
	}
}

// brittleStore is a LeaseStore whose operations can be made to fail, so the
// runner's refusal paths are reachable from a test.
//
// EVERY FIELD IS GUARDED, because KeepAlive reads this from its own goroutine
// while a test writes to it — and a test-only race is still a race, reported at
// whatever unlucky moment CI happens to schedule it. The setters exist so no
// caller has to remember.
type brittleStore struct {
	LeaseStore

	mu           sync.Mutex
	releaseErr   error
	heartbeatErr error
	leaseErr     error

	// releaseErrFor fails ONE lease's release, which is what makes "the loop
	// continues past a failure" testable. A store that fails every release cannot
	// distinguish a loop that stopped early from one that ran to the end and had
	// every entry fail.
	releaseErrFor map[string]error

	// heartbeats counts renewals, so a test can prove the keep-alive loop is
	// actually doing its one job.
	heartbeats atomic.Int64
}

func (b *brittleStore) failRelease(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.releaseErr = err
}

func (b *brittleStore) failHeartbeat(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.heartbeatErr = err
}

func (b *brittleStore) failLease(err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.leaseErr = err
}

func (b *brittleStore) failReleaseOf(leaseID string, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.releaseErrFor == nil {
		b.releaseErrFor = map[string]error{}
	}

	b.releaseErrFor[leaseID] = err
}

func (b *brittleStore) allowReleaseOf(leaseID string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.releaseErrFor, leaseID)
}

func (b *brittleStore) Release(
	ctx context.Context, leaseID string, epoch int64, outcome alloc.Phase,
) error {
	b.mu.Lock()
	err, forThis := b.releaseErrFor[leaseID]
	all := b.releaseErr
	b.mu.Unlock()

	if forThis {
		return err
	}

	if all != nil {
		return all
	}

	return b.LeaseStore.Release(ctx, leaseID, epoch, outcome)
}

func (b *brittleStore) Heartbeat(ctx context.Context, leaseID string, epoch int64) error {
	b.heartbeats.Add(1)

	b.mu.Lock()
	err := b.heartbeatErr
	b.mu.Unlock()

	if err != nil {
		return err
	}

	return b.LeaseStore.Heartbeat(ctx, leaseID, epoch)
}

func (b *brittleStore) Lease(ctx context.Context, leaseID string) (*alloc.Lease, error) {
	b.mu.Lock()
	err := b.leaseErr
	b.mu.Unlock()

	if err != nil {
		return nil, err
	}

	return b.LeaseStore.Lease(ctx, leaseID)
}

// A ledger that cannot be reached is reported, not treated as a lease that went
// away — the difference between "nobody wants this any more" and "I could not
// ask", which decide opposite things about a running container.
func TestTendReportsAnUnreachableLedger(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	store := &brittleStore{LeaseStore: a}

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(store, host, &fakeJIT{setID: 7}, p, nil)

	// Adoption succeeds; the ledger fails afterwards.
	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	store.failHeartbeat(errors.New("database is locked"))

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	store := &brittleStore{LeaseStore: a}
	store.failHeartbeat(errors.New("database is locked"))
	restarted := New(store, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if err := r.Launch(t.Context(), assignedLease(t, a), dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	store := &brittleStore{LeaseStore: a}
	store.failLease(errors.New("disk I/O error"))
	restarted := New(store, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	const requestID = 4242

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	frozen := time.Now()
	r.now = func() time.Time { return frozen }

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err == nil {
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	frozen := time.Now()
	r.now = func() time.Time { return frozen }

	lease := assignedLease(t, a)

	err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"})
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	frozen := time.Now()

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	frozen := time.Now()

	// THROUGH THE OPTION, not the private field. Writing maxCustody directly
	// tested the expiry logic while leaving the only way an operator can reach it
	// — WithMaxCustody — uncovered, which is how the previous version of this
	// capability shipped configurable in name only.
	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil,
		WithMaxCustody(2*time.Hour))
	restarted.now = func() time.Time { return frozen }

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	const requestID = 5150

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if err := restarted.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// The redelivery: a brand-new lease for the SAME request.
	second := assignedLease(t, a)

	err := restarted.Launch(t.Context(), second, dockerSpec(), Job{RequestID: lease.RequestID, Event: "push"})
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	const requestID = 6260

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
		name:    provider.InstanceName(lease.ID),
		outcome: alloc.PhaseDone,
		since:   time.Now(),
	}
	replacement.epoch.Store(lease.Epoch)

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
// These are different goroutines in production — completions from a listener, the
// tick from the reaper — and both reach tendOne, which mutates the entry in place
// and issues backend calls between reads. Two of them destroy the same instance
// twice, and a delete can drop an entry that was just replaced.
//
// Serializing only the FLAG WRITE is the same bug moved one line down, which is
// what this catches. Run under -race.
func TestACompletionDoesNotRaceTheTick(t *testing.T) {
	t.Parallel()

	// The delay is what makes this a test rather than a coin flip: it holds both
	// callers inside the transition long enough to genuinely overlap.
	p := &fakeProvider{kind: config.ProviderDocker, findDelay: 20 * time.Millisecond}
	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	const requestID = 7777

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	restarted := New(a, host, &fakeJIT{setID: 7}, p, nil)

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
	p := &fakeProvider{
		kind:        config.ProviderDocker,
		findDelay:   time.Hour,
		enteredFind: make(chan struct{}, 1),
	}

	a, host := newAllocatorWithHost(t)

	warm := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := warm.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	store := &brittleStore{LeaseStore: a}
	r := New(store, host, &fakeJIT{setID: 7}, p, nil)

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

	// WAIT FOR TEND TO BE GENUINELY STUCK rather than sleeping and hoping. Once
	// the provider says it is inside Find, Tend has already taken its own
	// heartbeat and cannot take another — so every renewal counted below is the
	// keep-alive's. (The first version of this test never started the keep-alive
	// at all and the single heartbeat it saw was Tend's, which is exactly the
	// confusion this synchronisation removes.)
	select {
	case <-p.enteredFind:
	case <-time.After(10 * time.Second):
		t.Fatal("Tend never reached the provider")
	}

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

// A LAUNCH IN PROGRESS IS RENEWED, because for its whole duration nobody else is
// doing it.
//
// Across the wire the control plane stops waiting after its command timeout and
// hands the listener custody, which stops the listener heartbeating — while the node
// is still inside provider.Launch and adopts nothing until it returns. Between
// those moments the lease has no owner: the reaper releases its capacity and the
// allocator sells it to another job.
func TestALaunchInProgressKeepsItsLeaseRenewed(t *testing.T) {
	t.Parallel()

	// A provider that blocks inside Launch, which is the whole situation.
	p := &fakeProvider{
		kind:          config.ProviderDocker,
		launchDelay:   time.Hour,
		enteredLaunch: make(chan struct{}, 1),
	}

	a, host := newAllocatorWithHost(t)

	store := &brittleStore{LeaseStore: a}
	r := New(store, host, &fakeJIT{setID: 7}, p, nil)
	r.ttl = func() time.Duration { return 30 * time.Millisecond }

	lease := assignedLease(t, a)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		//nolint:errcheck // it is expected to block, not to return
		_ = r.Launch(ctx, lease, dockerSpec(), Job{RequestID: 11, Event: "push"})
	}()

	select {
	case <-p.enteredLaunch:
	case <-time.After(10 * time.Second):
		t.Fatal("the provider was never asked to launch")
	}

	before := store.heartbeats.Load()

	go r.KeepAlive(ctx)

	// SEVERAL renewals, not one: one could be a coincidence of ordering.
	const want = 3

	deadline := time.Now().Add(15 * time.Second)

	for store.heartbeats.Load() < before+want {
		if time.Now().After(deadline) {
			t.Fatalf("a lease whose launch is still running was renewed %d times, want at "+
				"least %d; nothing holds it while the provider works, so the reaper "+
				"reclaims its capacity and the launch lands on hardware already resold",
				store.heartbeats.Load()-before, want)
		}

		time.Sleep(5 * time.Millisecond)
	}
}

// THE JANITOR FOLLOWS A RENEGOTIATED TTL, because the plane can shorten it.
//
// The TTL is agreed at registration, and a node re-registers whenever the control
// plane forgets it. A plane advertising a SHORTER one leaves a janitor built on the
// old value renewing too slowly, so the lease expires between heartbeats while its
// container runs. The janitor starts once — custody outlives any registration — so
// re-reading each cycle is the only place that correction can happen.
func TestKeepAliveFollowsAShortenedLeaseTTL(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}

	a, host := newAllocatorWithHost(t)

	warm := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := warm.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	store := &brittleStore{LeaseStore: a}
	r := New(store, host, &fakeJIT{setID: 7}, p, nil)

	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	// A CADENCE NO TEST COULD OUTLIVE, deliberately. If the janitor does not pull
	// its deadline in, the next renewal is a hundred seconds away and this test
	// simply never sees one — which makes the assertion below structural rather
	// than a race against a stopwatch. An earlier version asserted three renewals
	// inside 600ms and flaked under an instrumented parallel run, where a
	// goroutine can go unscheduled for longer than that.
	var (
		ttl   atomic.Int64
		reads atomic.Int64
	)

	ttl.Store(int64(300 * time.Second)) // renew every 100 seconds

	r.ttl = func() time.Duration {
		reads.Add(1)

		return time.Duration(ttl.Load())
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go r.KeepAlive(ctx)

	// WAIT UNTIL THE JANITOR HAS READ THE LONG TTL, which is the moment it commits to
	// that cadence and the only thing that makes shortening it a test of adaptation
	// rather than of scheduling order.
	//
	// Storing the short value straight after starting the goroutine lets it read the
	// SHORT one first; waiting for a heartbeat returns instantly because Recover already
	// heartbeated while adopting. Counting the janitor's own reads is the only signal
	// nothing else satisfies.
	armedBy := time.Now().Add(15 * time.Second)
	for reads.Load() == 0 {
		if time.Now().After(armedBy) {
			t.Fatal("the janitor never read the lease TTL, so it never armed")
		}

		time.Sleep(time.Millisecond)
	}

	before := store.heartbeats.Load()

	// The plane comes back with a much shorter TTL, as a restarted or
	// reconfigured control plane does.
	ttl.Store(int64(30 * time.Millisecond))

	// SEVERAL renewals, not one: one could be a single fire of a timer that then
	// went back to the hundred-second cadence.
	const want = 3

	// Generous, because the discrimination is structural: without the pull-in the
	// next renewal is a hundred seconds out, so no amount of waiting here would
	// produce one.
	deadline := time.Now().Add(10 * time.Second)

	for store.heartbeats.Load() < before+want {
		if time.Now().After(deadline) {
			t.Fatalf("the janitor renewed %d times in the ten seconds after the TTL was "+
				"shortened, want at least %d; its timer is still armed for the cadence it "+
				"held when the TTL changed, so a lease can expire between heartbeats while "+
				"its container runs", store.heartbeats.Load()-before, want)
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
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

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

	warm := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := warm.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	store := &brittleStore{LeaseStore: a}
	r := New(store, host, &fakeJIT{setID: 7}, p, nil)

	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	store.failHeartbeat(errors.New("database is locked"))

	r.renewHeld(t.Context())

	if !r.heldLeases()[lease.ID] {
		t.Error("dropped custody because a single renewal failed")
	}

	if len(p.live) != 1 {
		t.Error("the keep-alive destroyed compute; only Tend may do that")
	}

	// A lease that is genuinely GONE is not an error worth reporting on this
	// path either — Tend is about to clean it up.
	store.failHeartbeat(alloc.ErrLeaseNotFound)

	r.renewHeld(t.Context())

	if !r.heldLeases()[lease.ID] {
		t.Error("the keep-alive removed a custody entry; that is Tend's decision")
	}
}

// A custody failure does not strand the listener's own lease.
//
// A completion for a request that has BOTH — which a redelivered assignment after a
// crash produces — answers only "is MY compute gone". The listener reads any error
// as "it may still exist" and keeps heartbeating, so a custody-only failure strands
// a lease whose compute was just destroyed successfully.
//
// Custody does not need the help: its entry holds its own lease, and Tend retries
// every tick.
func TestACustodyFailureDoesNotStrandTheListenersLease(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)

	warm := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	// THE LEASE'S OWN request id, not a constant. assignedLease writes its own
	// value into the ledger, and recovery reads the request id back from THERE.
	requestID := lease.RequestID

	if err := warm.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: requestID, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	// Releases fail, so finishing the custody entry errors.
	store := &brittleStore{LeaseStore: a}
	store.failRelease(errors.New("database is locked"))
	restarted := New(store, host, &fakeJIT{setID: 7}, p, nil)

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

	// SUCCESS, because the listener's own compute is gone. Reporting the custody
	// failure here is what made the listener keep a lease for a container that no
	// longer existed.
	if err := restarted.Destroy(t.Context(), requestID); err != nil {
		t.Fatalf("a custody failure was reported to the listener, which will now keep "+
			"heartbeating a lease whose compute is gone: %v", err)
	}

	if _, stillThere := p.live[running.Name]; stillThere {
		t.Error("the running container was left behind")
	}

	// And custody kept its own entry, so Tend will retry it.
	if !restarted.heldLeases()[lease.ID] {
		t.Error("custody dropped an entry whose release failed; nothing will retry it")
	}
}

// A completion clears EVERY custody entry for its request, not the first found.
//
// One request should only ever have one entry — the launch guard sees to that —
// but "should" is doing a lot of work in a function whose whole job is cleaning
// up after states that should not exist. Stopping at the first match would
// destroy one container, acknowledge the completion, and leave the rest running
// with nothing that will ever look for them again.
//
// THE FIRST ENTRY FAILS, deliberately. Giving every entry a clean path leaves a
// regression that returns on the first error indistinguishable from correct
// behaviour, which is what the previous version of this test did.
func TestACompletionClearsEveryEntryForItsRequest(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)
	store := &brittleStore{LeaseStore: a}
	r := New(store, host, &fakeJIT{setID: 7}, p, nil)

	const requestID = 9090

	// Two entries for one request, built directly: reaching this state through
	// the normal path is exactly what the launch guard prevents.
	var leases []*alloc.Lease

	for range 2 {
		lease := assignedLease(t, a)

		inst := &provider.Instance{
			ID: "inst-" + lease.ID, Name: provider.InstanceName(lease.ID), Running: true,
		}
		p.add(inst)

		lease.RequestID = requestID

		r.adopt(lease, inst)

		leases = append(leases, lease)
	}

	// Custody iterates a map, so which entry is "first" is not knowable — both
	// are made to fail their release, then one is repaired, so whichever order
	// the loop takes it must reach the repaired one.
	for _, l := range leases {
		store.failReleaseOf(l.ID, errors.New("database is locked"))
	}

	if err := r.releaseRequest(t.Context(), requestID); err == nil {
		t.Fatal("reported success though both releases failed")
	}

	if len(r.heldLeases()) != 2 {
		t.Fatalf("dropped custody of an entry whose release failed: %d held", len(r.heldLeases()))
	}

	// Both containers were destroyed even though both releases failed — the
	// destroy comes first, and a release failure keeps the entry for a retry
	// rather than abandoning the compute.
	if len(p.live) != 0 {
		t.Fatalf("a release failure left containers running: %v", p.live)
	}

	// Now ONE recovers. The loop must reach it whichever order it visits.
	store.allowReleaseOf(leases[0].ID)

	if err := r.releaseRequest(t.Context(), requestID); err == nil {
		t.Fatal("reported success though one release still fails")
	}

	held := r.heldLeases()

	if held[leases[0].ID] {
		t.Error("the entry whose release succeeded was not removed; the loop stopped early")
	}

	if !held[leases[1].ID] {
		t.Error("the entry whose release failed was dropped, so nothing will retry it")
	}
}

// An adopted container that has VANISHED is released immediately, not after the
// stray grace period.
//
// The grace exists for compute that may never have started — a create whose
// response was lost can still be in flight inside the daemon. It has nothing to
// say about an instance billet watched running and then found gone: that one has
// genuinely finished, and making it wait held its capacity for a full minute
// after its job was over.
//
// The distinction is `observed`, and conflating it with `discard` was a bug my
// own test found: discard flips to true when a completion arrives, so checking
// it alone applied the stray grace to every adopted job at exactly the moment it
// finished.
func TestAVanishedAdoptedContainerIsReleasedWithoutWaiting(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}
	a, host := newAllocatorWithHost(t)

	warm := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := warm.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	frozen := time.Now()

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	r.now = func() time.Time { return frozen }

	if err := r.Recover(t.Context()); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	if !r.heldLeases()[lease.ID] {
		t.Fatal("the running container was not adopted")
	}

	// It disappears — somebody ran `docker rm`, or the daemon restarted. The
	// clock does NOT advance, so anything that waits for the grace period will
	// still be holding when this checks.
	delete(p.live, provider.InstanceName(lease.ID))

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if r.heldLeases()[lease.ID] {
		t.Error("held the capacity of a container billet had watched running and then " +
			"found gone, waiting out a grace period meant for compute that may never " +
			"have started")
	}

	if _, err := a.Lease(t.Context(), lease.ID); !errors.Is(err, alloc.ErrLeaseNotFound) {
		t.Errorf("the vanished job's lease still holds capacity: %v", err)
	}
}

// A DISCARDED entry billet has never seen still waits out the grace period.
//
// The other side of the same distinction: an absence here is a snapshot, not a
// causal result, because the daemon may still be acting on a create whose
// response was lost.
func TestAnUnseenStrayStillWaits(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{
		kind:      config.ProviderDocker,
		launchErr: errors.New("context deadline exceeded"),
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	frozen := time.Now()
	r.now = func() time.Time { return frozen }

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err == nil {
		t.Fatal("a launch that failed reported success")
	}

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if !r.heldLeases()[lease.ID] {
		t.Error("released a stray billet has never seen, on a single negative observation")
	}
}

// An entry billet has never seen becomes observed once Find succeeds.
//
// The transition in the middle: a stray that finally materialises is no longer a
// maybe, so a LATER absence needs no grace period. Without it, a container that
// appeared and was then destroyed ambiguously would make billet wait out the
// remaining grace before reclaiming capacity it already knows about.
func TestAStrayThatAppearsBecomesObserved(t *testing.T) {
	t.Parallel()

	// The launch fails and the stray is not visible yet, so custody starts unseen.
	p := &fakeProvider{
		kind:      config.ProviderDocker,
		launchErr: errors.New("context deadline exceeded"),
	}

	a, host := newAllocatorWithHost(t)
	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	frozen := time.Now()
	r.now = func() time.Time { return frozen }

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err == nil {
		t.Fatal("a launch that failed reported success")
	}

	for _, c := range r.custodySnapshot() {
		if c.observed {
			t.Fatal("custody started observed for compute billet never saw")
		}
	}

	// THE DAEMON CATCHES UP: the container the lost create asked for appears, but
	// destroying it fails, so the entry survives this tick having been seen.
	name := provider.InstanceName(lease.ID)
	p.add(&provider.Instance{ID: "late-" + lease.ID, Name: name, Running: true})
	p.destroyErr = errors.New("daemon is not responding")

	if err := r.Tend(t.Context()); err == nil {
		t.Fatal("Tend reported success though the destroy failed")
	}

	for _, c := range r.custodySnapshot() {
		if !c.observed {
			t.Error("billet saw the instance and did not record it")
		}
	}

	// Now it is gone, and the clock has NOT moved: an entry that waited out the
	// grace would still be holding here.
	p.destroyErr = nil
	delete(p.live, name)

	if err := r.Tend(t.Context()); err != nil {
		t.Fatalf("Tend: %v", err)
	}

	if len(r.heldLeases()) != 0 {
		t.Error("held capacity for an instance billet had seen and then found gone, " +
			"waiting out a grace period meant for compute that may never have started")
	}
}

// AN ORDINARY RUNNING JOB IS SOMETHING THIS NODE IS HOLDING.
//
// Custody is compute billet cannot account for; launching is compute it is
// creating. A job that started cleanly and is running right now is neither — and
// it is exactly as much this process's responsibility, because its completion
// will be routed here and nowhere else.
//
// Leaving it out meant a superseded node with a healthy running job saw "holding
// nothing", exited, and left a container whose completion now reaches a
// replacement that cannot see it: that Destroy finds nothing, reports success,
// and the lease is released under a running job.
func TestAnOrdinaryRunningJobCountsAsHolding(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}

	a, host := newAllocatorWithHost(t)

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)

	if r.Holding() {
		t.Fatal("a node with nothing running reported that it was holding something")
	}

	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !r.Holding() {
		t.Error("a node running a job it launched cleanly reported that it was holding " +
			"nothing; superseded, it would exit and leave that container's lease to a " +
			"replacement that cannot see it")
	}
}

// SUPERSESSION TURNS RUNNING WORK INTO CUSTODY, which is what lets a drain end.
//
// After supersession the control plane routes a job's completion to whichever
// process owns the node's name. For a container running HERE that is the
// replacement, which cannot see it, reports the destroy as done, and lets the
// lease be released under a running job. Nothing on this side would ever finish
// it either: Tend is what confirms compute gone and releases its lease, and Tend
// looks only at custody.
func TestSupersessionMovesRunningWorkIntoCustody(t *testing.T) {
	t.Parallel()

	p := &fakeProvider{kind: config.ProviderDocker}

	a, host := newAllocatorWithHost(t)

	r := New(a, host, &fakeJIT{setID: 7}, p, nil)
	lease := assignedLease(t, a)

	if err := r.Launch(t.Context(), lease, dockerSpec(), Job{RequestID: 11, Event: "push"}); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got := len(r.custodySnapshot()); got != 0 {
		t.Fatalf("a cleanly launched job was already in custody: %d", got)
	}

	r.Superseded()

	held := r.custodySnapshot()
	if len(held) != 1 {
		t.Fatalf("supersession left %d entries in custody, want the running job", len(held))
	}

	if held[0].leaseID != lease.ID {
		t.Errorf("custody holds lease %q, want %q", held[0].leaseID, lease.ID)
	}

	// DONE, not failed: the job launched cleanly and is running. What changed is
	// who may talk about it, not whether it worked — and writing "failed" into
	// the durable history for a job that ran is a lie a later investigation would
	// have to unpick.
	if held[0].outcome != alloc.PhaseDone {
		t.Errorf("custody records outcome %q for a job that launched cleanly, want done",
			held[0].outcome)
	}

	// And it is idempotent, because a drain may call it more than once.
	r.Superseded()

	if got := len(r.custodySnapshot()); got != 1 {
		t.Errorf("a second supersession produced %d custody entries, want 1", got)
	}

	// MOVED, NOT COPIED, which is the assertion that matters and the one the
	// first version of this test left out. Once Tend has confirmed the compute
	// gone and dropped the custody entry, this process must be holding nothing —
	// an entry left behind in `running` keeps Holding() true forever and the
	// drain can never end.
	r.mu.Lock()
	r.custody = map[string]*custody{}
	r.mu.Unlock()

	if r.Holding() {
		t.Error("after its custody was discharged the node still reported that it was " +
			"holding something; a superseded process in that state drains forever")
	}
}

// EVERY CUSTODY ENTRY RENEWS WITH ITS LEASE'S OWN EPOCH.
//
// The epoch is the fencing token: an entry carrying the wrong one is refused by
// the ledger on its first heartbeat, which custody reads as "this lease is
// gone" — and then destroys the compute it was holding. An entry that starts at
// zero therefore does the opposite of what custody is for.
//
// This is a structural property of every constructor rather than of one path,
// and it is checked that way because converting the field to an atomic silently
// dropped the assignment from three of them. Nothing failed: no test heartbeated
// an entry from those paths, so a zero epoch was invisible.
func TestEveryCustodyEntryCarriesItsLeasesEpoch(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		build func(r *Runner, lease *alloc.Lease, inst *provider.Instance)
	}{
		{"adopted after a restart", func(r *Runner, l *alloc.Lease, i *provider.Instance) {
			r.adopt(l, i)
		}},
		{"held when a launch could not be confirmed", func(r *Runner, l *alloc.Lease, i *provider.Instance) {
			r.hold(l, i.Name, l.RequestID)
		}},
		{"superseded by another process", func(r *Runner, l *alloc.Lease, i *provider.Instance) {
			r.mu.Lock()
			r.running[l.RequestID] = i
			r.runningLease[l.RequestID] = l
			r.mu.Unlock()

			r.Superseded()
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := &fakeProvider{kind: config.ProviderDocker}
			a, host := newAllocatorWithHost(t)
			r := New(a, host, &fakeJIT{setID: 7}, p, nil)

			lease := assignedLease(t, a)
			if err := a.Bind(t.Context(), lease.ID, lease.Epoch, host); err != nil {
				t.Fatalf("bind: %v", err)
			}

			// RUNNING, THEN QUARANTINED, so the lease's epoch is not zero. Only the reaper
			// moves an epoch, so an ordinary lease sits at 0 — and a test built on
			// one compares 0 against 0 and passes with the assignment removed. It is
			// also the case that matters: recovery adopts quarantined compute, and
			// that is the only path where the epoch has moved.
			for _, to := range []alloc.Phase{alloc.PhaseLaunching, alloc.PhaseOnline, alloc.PhaseBusy} {
				if err := a.Advance(t.Context(), lease.ID, lease.Epoch, to); err != nil {
					t.Fatalf("advance to %s: %v", to, err)
				}
			}

			if err := a.ExpireForTest(t.Context(), lease.ID); err != nil {
				t.Fatalf("expire: %v", err)
			}

			if _, err := a.Reap(t.Context()); err != nil {
				t.Fatalf("reap: %v", err)
			}

			bound, err := a.Lease(t.Context(), lease.ID)
			if err != nil {
				t.Fatalf("lease: %v", err)
			}

			if bound.Epoch == 0 {
				t.Fatal("this test cannot tell a missing epoch from a zero one")
			}

			inst := &provider.Instance{
				ID: "i1", Name: provider.InstanceName(bound.ID), Running: true,
			}

			tc.build(r, bound, inst)

			r.mu.Lock()
			entry, held := r.custody[bound.ID]
			r.mu.Unlock()

			if !held {
				t.Fatal("nothing was taken into custody")
			}

			if got := entry.epoch.Load(); got != bound.Epoch {
				t.Errorf("the entry renews with epoch %d and its lease is at %d; the first "+
					"heartbeat is fenced, custody reads that as the lease being gone, and "+
					"destroys the compute it was holding", got, bound.Epoch)
			}
		})
	}
}
