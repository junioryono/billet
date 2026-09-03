package nodeplane

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

// registerAs registers the docker host n1 as one named process.
func registerAs(t *testing.T, p *Plane, incarnation string) {
	t.Helper()

	if _, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "n1", Provider: config.ProviderDocker,
		Deployment: deployment, Incarnation: incarnation, VCPU: 8, Memory: 32 * config.GiB,
	}); err != nil {
		t.Fatalf("register n1 as %s: %v", incarnation, err)
	}
}

// A NODE THAT WITHDRAWS IS OUT OF PLACEMENT AT ONCE, with no silence window.
//
// A stopped node used to stay pickable until expireStaleLocked forgot it, and
// every job aimed there in the meantime waited that window out. THE CLOCK DOES
// NOT MOVE in this test, so a plane that merely let expiry do the work fails
// it.
//
// AND THE LEDGER IS TOLD FIRST, with the registration's own fence — because
// placement escrows against the ledger's live set long before this map is
// consulted, and a withdrawal that only touched memory would leave the tier
// advertising the host.
func TestAWithdrawnNodeIsNotPickedAndNeedsNoSilence(t *testing.T) {
	t.Parallel()

	clock := newTestClock()
	ledger := newLedger()

	p := testPlane(t, WithClock(clock.now), WithRegistrar(ledger))
	registerAs(t, p, "p1")

	if _, err := p.PickForTest(testLease()); err != nil {
		t.Fatalf("nothing was placeable before the withdrawal, so this proves nothing: %v", err)
	}

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	if got := p.Nodes(); len(got) != 0 {
		t.Errorf("a node that withdrew is still in the fleet: %v", got)
	}

	if _, err := p.PickForTest(testLease()); !errors.Is(err, ErrNoNode) {
		t.Errorf("a lease was placed on a node that withdrew: %v", err)
	}

	w, told := ledger.withdrawalOf("n1")
	if !told {
		t.Fatal("the ledger was never told the host withdrew, so escrow still targets it")
	}

	if w.epoch != ledger.currentEpoch() || w.incarnation != "p1" {
		t.Errorf("the ledger was told epoch %d from %q, want epoch %d from %q",
			w.epoch, w.incarnation, ledger.currentEpoch(), "p1")
	}

	// NOT "GONE". Giving up on a host marks its jobs disrupted; a host saying it
	// is leaving observes nothing about anybody's job.
	if n := ledger.goneCount(); n != 0 {
		t.Errorf("a withdrawal was recorded as the plane giving up on %d host(s)", n)
	}
}

// ONLY THE CURRENT PROCESS MAY WITHDRAW THE NAME. A superseded process still
// holds the certificate; letting it withdraw would take its replacement out of
// the fleet. An absent incarnation is refused for the same reason once a
// process has claimed the name — the fourth place this needed saying.
func TestAWithdrawalFromASupersededProcessIsRefused(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		stale string
	}{
		{"the superseded process", "p1"},
		{"a request carrying no incarnation", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ledger := newLedger()
			p := testPlane(t, WithRegistrar(ledger))

			registerAs(t, p, "p1")
			registerAs(t, p, "p2")

			err := p.Withdraw(t.Context(), "n1", tc.stale)
			if !errors.Is(err, ErrSuperseded) {
				t.Fatalf("Withdraw from %q = %v, want ErrSuperseded", tc.stale, err)
			}

			if got := p.CurrentIncarnationForTest("n1"); got != "p2" {
				t.Errorf("the name resolves to %q after a refused withdrawal, want p2", got)
			}

			if _, err := p.PickForTest(testLease()); err != nil {
				t.Errorf("a refused withdrawal still took the replacement out of placement: %v", err)
			}

			// REFUSED BEFORE THE LEDGER. The ledger has its own fence, but a request
			// the plane can already see is not the current process should never
			// reach it.
			if _, told := ledger.withdrawalOf("n1"); told {
				t.Error("a superseded process's withdrawal reached the ledger")
			}
		})
	}
}

// A LEDGER THAT CANNOT RECORD THE WITHDRAWAL LEAVES THE NODE WHERE IT WAS.
//
// The order is ledger first, memory second, and this is the case that pins it:
// a node dropped from memory while the ledger still says live is a host that
// escrow keeps targeting and the plane can no longer deliver to. A refused
// withdrawal must change nothing, so the node can ask again.
func TestAWithdrawalTheLedgerCannotRecordLeavesTheNodePlaceable(t *testing.T) {
	t.Parallel()

	ledger := newLedger()
	ledger.withdrawFailures = 1

	p := testPlane(t, WithRegistrar(ledger))
	registerAs(t, p, "p1")

	err := p.Withdraw(t.Context(), "n1", "p1")
	if err == nil {
		t.Fatal("a withdrawal the ledger refused to record was reported as done")
	}

	if errors.Is(err, ErrSuperseded) || errors.Is(err, ErrUnregistered) {
		t.Fatalf("a ledger outage was reported as a verdict: %v", err)
	}

	if _, err := p.PickForTest(testLease()); err != nil {
		t.Errorf("the node was dropped from placement although the ledger never heard: %v", err)
	}

	// AND ASKING AGAIN WORKS, which is what the node does.
	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("the retry was refused: %v", err)
	}

	if got := p.Nodes(); len(got) != 0 {
		t.Errorf("the retried withdrawal left the node in the fleet: %v", got)
	}
}

// staleWithdrawals is a ledger whose fence has always moved.
type staleWithdrawals struct{ countingRegistrar }

func (staleWithdrawals) NodeWithdrawn(context.Context, string, int64, string) error {
	return alloc.ErrWithdrawalStale
}

// THE LEDGER'S FENCE IS A VERDICT, NOT AN OUTAGE. A withdrawal the ledger calls
// stale names a registration that has since been replaced, and the process
// asking has nothing to withdraw — so the node is told it was superseded and
// stops, rather than retrying against a fence that will never match again.
func TestAWithdrawalTheLedgerCallsStaleIsSuperseded(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(staleWithdrawals{}))
	registerAs(t, p, "p1")

	if err := p.Withdraw(t.Context(), "n1", "p1"); !errors.Is(err, ErrSuperseded) {
		t.Fatalf("Withdraw against a moved fence = %v, want ErrSuperseded", err)
	}

	if _, err := p.PickForTest(testLease()); err != nil {
		t.Errorf("a withdrawal the ledger refused still dropped the node from memory: %v", err)
	}
}

// A LAUNCH THE NODE NEVER TOOK IS ANSWERED "NOTHING STARTED", never custody.
//
// This is the answer that used to arrive after the silence window, arriving at
// once: the listener hands the capacity back and the job is reassigned. Custody
// here would be the opposite failure — a lease held for compute that was never
// created, on a host that has left.
func TestAWithdrawalAnswersQueuedLaunchesAsNeverStarted(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(time.Hour), WithRegistrar(newLedger()))
	registerAs(t, p, "p1")

	done := make(chan error, 1)

	go func() {
		done <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	waitFor(t, "the launch to be queued", func() bool { return p.QueuedForTest("n1") == 1 })

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a launch queued for a node that withdrew reported success")
		}

		if errors.Is(err, server.ErrCustody) {
			t.Errorf("a launch the node never took was answered with custody: %v", err)
		}

		if !strings.Contains(err.Error(), "nothing started") {
			t.Errorf("the answer does not say nothing started: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the launch waited on a node that had withdrawn")
	}
}

// A LAUNCH IN FLIGHT WHEN THE NODE WITHDRAWS IS ANSWERED WITH CUSTODY, as a
// re-registration answers one. The node took it and has said it will not
// report it, and a launch that was taken may have started something — so the
// listener is told the node has custody, which is the direction a timeout
// takes for the same reason. Reachable only when the node's own report was
// lost; the node's next Recover is what finds whatever started.
func TestAWithdrawalWithALaunchInFlightHandsTheNodeCustody(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(time.Hour), WithRegistrar(newLedger()))
	registerAs(t, p, "p1")

	done := make(chan error, 1)

	go func() {
		done <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	waitFor(t, "the launch to be queued", func() bool { return p.QueuedForTest("n1") == 1 })

	cmd, took, err := p.Poll(t.Context(), "n1", "p1")
	if err != nil || !took {
		t.Fatalf("the node did not take the launch: took=%v err=%v", took, err)
	}

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, server.ErrCustody) {
			t.Fatalf("a launch in flight when its node withdrew = %v, want custody", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the launch waited on a node that had withdrawn")
	}

	// AND THE LEASE STAYS ATTRIBUTED TO THE PROCESS THAT TOOK IT, so a node that
	// comes back holding the compute is the one entitled to release it.
	if !p.OwnsForTest(cmd.Lease.ID, "n1", "p1") {
		t.Error("a withdrawal dropped the ownership of a launch the process had taken")
	}
}

// OWNERSHIP OUTLIVES THE WITHDRAWAL, as it outlives expiry. The record is what
// lets a process finish what it was given, and a withdrawn process that comes
// back holding compute — a node stopped and started — must still be the one
// entitled to release it.
func TestAWithdrawalKeepsOwnership(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	p.AdoptOwnership("n1", "p1", []string{"l9"})

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	if !p.OwnsForTest("l9", "n1", "p1") {
		t.Error("a withdrawal dropped the ownership of a lease the process still holds")
	}
}

// A REGISTRATION THAT LANDS DURING THE LEDGER WRITE WINS, and the withdrawal
// that was overtaken must not remove it.
//
// The ledger is written outside the mutex, so a replacement can register in
// between — bumping the epoch, making the host live again — and a withdrawal
// that then dropped the node from memory would leave escrow targeting a host
// the plane can no longer dispatch to. The real ledger's fence refuses the
// overtaken write on its own; this stages the interleaving against a fake that
// does not, so the plane's own re-check is what is under test.
func TestAWithdrawalOvertakenByARegistrationIsRefused(t *testing.T) {
	t.Parallel()

	// HELD IN A LOCAL, because the fake clears its own reference once it has
	// parked, and the release below must close the channel that was parked on.
	hold := make(chan struct{})

	ledger := newLedger()
	ledger.holdWithdraw = hold
	ledger.withdrawEntered = make(chan struct{})

	// RELEASED WHATEVER HAPPENS, so a regression cannot turn this into a hang: a
	// withdrawal that held the mutex across the write would block the
	// registration below, and nothing would ever reach the release.
	var releaseOnce sync.Once

	release := func() { releaseOnce.Do(func() { close(hold) }) }

	t.Cleanup(release)

	p := testPlane(t, WithRegistrar(ledger))
	registerAs(t, p, "p1")

	done := make(chan error, 1)

	go func() { done <- p.Withdraw(t.Context(), "n1", "p1") }()

	// INSIDE THE WRITE, with the fence already read and the mutex released.
	select {
	case <-ledger.withdrawEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("the withdrawal never reached the ledger")
	}

	// THE REGISTRATION IS NOT ALLOWED TO HANG THE TEST EITHER: it runs on its own
	// goroutine and is bounded, so a plane holding its mutex across the write
	// fails here rather than deadlocking against the parked withdrawal.
	registered := make(chan error, 1)

	go func() {
		_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
			Version: nodeapi.Version, Node: "n1", Provider: config.ProviderDocker,
			Deployment: deployment, Incarnation: "p2", VCPU: 8, Memory: 32 * config.GiB,
		})
		registered <- err
	}()

	select {
	case err := <-registered:
		if err != nil {
			t.Fatalf("register the replacement during the write: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a registration could not land while a withdrawal was inside the ledger write")
	}

	release()

	select {
	case err := <-done:
		if !errors.Is(err, ErrSuperseded) {
			t.Fatalf("a withdrawal overtaken by a registration = %v, want ErrSuperseded", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the withdrawal never returned")
	}

	if got := p.CurrentIncarnationForTest("n1"); got != "p2" {
		t.Errorf("the name resolves to %q, want the registration that overtook, p2", got)
	}

	if _, err := p.PickForTest(testLease()); err != nil {
		t.Errorf("the overtaken withdrawal took the replacement out of placement: %v", err)
	}
}

// A DESTROY'S LATE SUCCESS STILL ENDS OWNERSHIP AFTER THE NODE HAS WITHDRAWN.
//
// The node reports before it withdraws, but a report cut on the client side by
// the stop signal can still be inside the handler when the withdrawal lands —
// and a late successful destroy is the only proof its compute is gone.
// Discarding it because the node record has been deleted leaves the ownership
// record alive after the process exits, which is the exact failure a
// re-registration keeps tombstones for; so the tombstones outlive the record.
func TestALateDestroyResultAfterAWithdrawalStillEndsOwnership(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(time.Hour), WithRegistrar(newLedger()))
	registerAs(t, p, "p1")

	// A launch this process took and reported, so it owns the compute.
	launched := make(chan error, 1)

	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	waitFor(t, "the launch to be queued", func() bool { return p.QueuedForTest("n1") == 1 })

	launch, took, err := p.Poll(t.Context(), "n1", "p1")
	if err != nil || !took {
		t.Fatalf("the node did not take the launch: took=%v err=%v", took, err)
	}

	if err := p.Result("n1", "p1", nodeapi.CommandResult{ID: launch.ID, OK: true}); err != nil {
		t.Fatalf("report the launch: %v", err)
	}

	if err := <-launched; err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !p.OwnsForTest("l1", "n1", "p1") {
		t.Fatal("the process that launched does not own the lease, so this proves nothing")
	}

	// Its destroy is taken and then the node withdraws before the report lands.
	destroyed := make(chan error, 1)

	go func() { destroyed <- p.NewRunner().Destroy(t.Context(), 7) }()

	waitFor(t, "the destroy to be queued", func() bool { return p.QueuedForTest("n1") == 1 })

	destroy, took, err := p.Poll(t.Context(), "n1", "p1")
	if err != nil || !took || destroy.Kind != nodeapi.CommandDestroy {
		t.Fatalf("the node did not take the destroy: kind=%s took=%v err=%v", destroy.Kind, took, err)
	}

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	select {
	case err := <-destroyed:
		// UNKNOWN, NOT DONE: the withdrawal cannot vouch for a command in flight.
		if err == nil {
			t.Fatal("a destroy in flight when its node withdrew was reported as done")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the destroy waited on a node that had withdrawn")
	}

	if !p.OwnsForTest("l1", "n1", "p1") {
		t.Fatal("the withdrawal itself ended ownership of compute nothing proved gone")
	}

	// THE LATE REPORT, from the process that withdrew.
	if err := p.Result("n1", "p1", nodeapi.CommandResult{ID: destroy.ID, OK: true}); err != nil {
		t.Fatalf("a late destroy result after the withdrawal was refused: %v", err)
	}

	if p.OwnsForTest("l1", "n1", "p1") {
		t.Error("a late successful destroy did not end ownership, so the plane reports " +
			"custody forever for compute that is gone")
	}

	// AND ONLY ONCE. The tombstone is spent; a second copy of the report is an
	// ordinary unknown-node answer.
	if err := p.Result("n1", "p1", nodeapi.CommandResult{ID: destroy.ID, OK: true}); !errors.Is(err, ErrUnregistered) {
		t.Errorf("a repeated late result = %v, want ErrUnregistered", err)
	}
}

// A DESTROY'S LATE SUCCESS STILL ENDS OWNERSHIP AFTER SILENCE FORGOT THE NODE,
// which is the same proof arriving through the other way out of the fleet.
//
// A destroy the plane stopped waiting for is no longer in flight, so the guard
// that keeps a busy node from expiring does not hold it; with a ten-minute
// command timeout against a silence window of four poll windows, a node can be
// forgotten between the timeout and the late answer. Expiry used to delete the
// tombstone with the record; the store that outlives it is shared with
// withdrawal.
func TestALateDestroyResultAfterExpiryStillEndsOwnership(t *testing.T) {
	t.Parallel()

	clock := newTestClock()

	p := testPlane(t, WithClock(clock.now), WithCommandTimeout(time.Hour), WithRegistrar(newLedger()))
	registerAs(t, p, "p1")

	// A launch this process TOOK, which is what records it as the owner. The
	// caller then stops waiting, which abandons the delivered command through
	// the same path a timeout takes — deterministically, where a short command
	// budget would race the node taking it.
	launchCtx, stopLaunch := context.WithCancel(t.Context())
	defer stopLaunch()

	launched := make(chan error, 1)

	go func() {
		launched <- p.NewRunner().Launch(launchCtx, testLease(), server.Job{RequestID: 7})
	}()

	waitFor(t, "the launch to be queued", func() bool { return p.QueuedForTest("n1") == 1 })

	if _, took, err := p.Poll(t.Context(), "n1", "p1"); err != nil || !took {
		t.Fatalf("the node did not take the launch: took=%v err=%v", took, err)
	}

	stopLaunch()

	select {
	case <-launched:
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned launch never returned")
	}

	if !p.OwnsForTest("l1", "n1", "p1") {
		t.Fatal("the process that took the launch does not own the lease, so this proves nothing")
	}

	// Its destroy is taken and then abandoned the same way, which tombstones it.
	destroyCtx, stopDestroy := context.WithCancel(t.Context())
	defer stopDestroy()

	destroyed := make(chan error, 1)

	go func() { destroyed <- p.NewRunner().Destroy(destroyCtx, 7) }()

	waitFor(t, "the destroy to be queued", func() bool { return p.QueuedForTest("n1") == 1 })

	destroy, took, err := p.Poll(t.Context(), "n1", "p1")
	if err != nil || !took || destroy.Kind != nodeapi.CommandDestroy {
		t.Fatalf("the node did not take the destroy: kind=%s took=%v err=%v", destroy.Kind, took, err)
	}

	stopDestroy()

	select {
	case <-destroyed:
	case <-time.After(10 * time.Second):
		t.Fatal("the abandoned destroy never returned")
	}

	// Silence forgets the node, with nothing in flight to hold it.
	clock.advancePastSilence()

	if got := p.Nodes(); len(got) != 0 {
		t.Fatalf("the node was not expired, so this proves nothing: %v", got)
	}

	if !p.OwnsForTest("l1", "n1", "p1") {
		t.Fatal("expiry itself ended ownership of compute nothing proved gone")
	}

	// THE LATE ANSWER, from the process the plane forgot.
	if err := p.Result("n1", "p1", nodeapi.CommandResult{ID: destroy.ID, OK: true}); err != nil {
		t.Fatalf("a late destroy result after expiry was refused: %v", err)
	}

	if p.OwnsForTest("l1", "n1", "p1") {
		t.Error("a late successful destroy after expiry did not end ownership, so the plane " +
			"reports custody forever for compute that is gone")
	}
}

// THE LATE-RESULT STORE IS BOUNDED AS A WHOLE, not per node: a control plane
// that replaces hosts for years must not keep a map per name it has ever seen.
// The oldest tombstone is the one evicted, because the newer ones are the
// answers still likely to arrive.
func TestLateResultsAreBoundedAcrossNodes(t *testing.T) {
	t.Parallel()

	p := testPlane(t)

	for i := range maxAbandoned + 1 {
		p.mu.Lock()
		p.rememberLateLocked(lateResult{node: "n" + strconv.Itoa(i), id: "c"},
			abandonedCmd{kind: nodeapi.CommandDestroy, at: time.Unix(int64(i), 0)})
		p.mu.Unlock()
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if got := len(p.lateResults); got != maxAbandoned {
		t.Fatalf("the store holds %d tombstones, want the bound of %d", got, maxAbandoned)
	}

	if _, kept := p.lateResults[lateResult{node: "n0", id: "c"}]; kept {
		t.Error("the oldest tombstone survived the eviction")
	}

	if _, kept := p.lateResults[lateResult{node: "n" + strconv.Itoa(maxAbandoned), id: "c"}]; !kept {
		t.Error("the newest tombstone was the one evicted")
	}

	// AND AN INCOMING TOMBSTONE OLDER THAN EVERYTHING HELD IS THE ONE THAT GOES,
	// not a newer one making room for it: a node forgotten after months carries
	// exactly such entries, and the first version evicted before inserting.
	p.rememberLateLocked(lateResult{node: "ancient", id: "c"},
		abandonedCmd{kind: nodeapi.CommandDestroy, at: time.Time{}})

	if got := len(p.lateResults); got != maxAbandoned {
		t.Fatalf("the store holds %d tombstones after an old arrival, want %d", got, maxAbandoned)
	}

	if _, kept := p.lateResults[lateResult{node: "ancient", id: "c"}]; kept {
		t.Error("an incoming tombstone older than the whole store displaced a newer one")
	}

	if _, kept := p.lateResults[lateResult{node: "n1", id: "c"}]; !kept {
		t.Error("the oldest surviving tombstone was evicted to make room for an older arrival")
	}
}

// A HOST THE PLANE DOES NOT KNOW HAS NOTHING TO WITHDRAW, and is told to
// register again rather than being refused as somebody else.
func TestAWithdrawalFromAnUnknownNodeIsUnregistered(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))

	if err := p.Withdraw(t.Context(), "ghost", "p1"); !errors.Is(err, ErrUnregistered) {
		t.Fatalf("Withdraw for an unknown node = %v, want ErrUnregistered", err)
	}
}
