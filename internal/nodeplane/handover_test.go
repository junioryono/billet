package nodeplane

import (
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

// seedOwner records a lease as delivered to an incarnation, with the job it was
// launched for, the way a launch's delivery records it.
func seedOwner(p *Plane, lease, incarnation string, requestID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.owners == nil {
		p.owners = make(map[string]leaseOwner)
	}
	p.owners[lease] = leaseOwner{node: "n1", incarnation: incarnation, requestID: requestID}
}

func ownerOf(p *Plane, lease string) (leaseOwner, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	owner, ok := p.owners[lease]

	return owner, ok
}

// A PROCESS THAT WITHDREW HANDS WHAT IT OWNED TO THE NODE'S NEXT PROCESS (#374).
//
// A node handing over leaves its compute running and exits. Its successor adopts
// that compute, and only the successor can act on a completion's destroy; left
// with the withdrawn owner, the destroy answered "holder unavailable" for as long
// as the plane ran and a finished job's VM was never torn down. The request id
// moves with it, so the destroy that ends the job still ends the ownership.
func TestAProcessThatWithdrewHandsItsLeasesToTheNextOne(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	seedOwner(p, "l9", "p1", 7)

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	registerAs(t, p, "p2")
	p.AdoptOwnershipWithInventory("n1", "p2", []string{"l9"}, true)

	owner, ok := ownerOf(p, "l9")
	if !ok || owner.incarnation != "p2" || owner.requestID != 7 {
		t.Fatalf("after the handover l9 is owned by %+v (ok=%v), want p2 with request 7", owner, ok)
	}

	done := make(chan error, 1)
	go func() {
		done <- p.NewRunner().DestroyCompletedBound(
			t.Context(), 7, "Succeeded", "l9", "n1", 1, alloc.PhaseDone, server.CacheAuthority{})
	}()

	cmd, took, err := p.Poll(t.Context(), "n1", "p2")
	if err != nil || !took {
		t.Fatalf("the successor was not handed the destroy: took=%v err=%v", took, err)
	}
	if cmd.Kind != nodeapi.CommandDestroy || cmd.RequestID != 7 {
		t.Fatalf("the successor received %+v", cmd)
	}
	if err := p.Result("n1", "p2", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("complete destroy: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the completion's destroy through the successor: %v", err)
	}
}

// A SUPERSEDED PROCESS THAT HAS NOT WITHDRAWN KEEPS WHAT IT OWNS, because it may
// still be draining it; only a withdrawal says it never will.
func TestAProcessThatDidNotWithdrawKeepsItsLeases(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	seedOwner(p, "l9", "p1", 7)

	registerAs(t, p, "p2")
	p.AdoptOwnershipWithInventory("n1", "p2", []string{"l9"}, true)

	if owner, _ := ownerOf(p, "l9"); owner.incarnation != "p1" {
		t.Errorf("a replacement took a lease its superseded, still-draining process owns: %+v", owner)
	}
}

// ONLY WHAT THE SUCCESSOR REPORTS MOVES. A withdrawn process's lease the next
// process did not report stays with the withdrawn owner, for the ordinary
// reconciliation to settle, and is not dropped either.
func TestOnlyALeaseTheSuccessorReportedMoves(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	seedOwner(p, "l8", "p1", 8)
	seedOwner(p, "l9", "p1", 9)

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	registerAs(t, p, "p2")
	p.AdoptOwnershipWithInventory("n1", "p2", []string{"l9"}, true)

	if owner, _ := ownerOf(p, "l9"); owner.incarnation != "p2" {
		t.Errorf("the reported lease did not move to the successor: %+v", owner)
	}
	if owner, ok := ownerOf(p, "l8"); !ok || owner.incarnation != "p1" || owner.requestID != 8 {
		t.Errorf("a lease the successor never reported moved or was dropped: %+v (ok=%v)", owner, ok)
	}

	// AN EMPTY REPORT TAKES NOTHING: a host sharing the name that does not hold
	// the compute must never become what a destroy reaches.
	p.AdoptOwnershipWithInventory("n1", "p2", nil, true)
	if owner, _ := ownerOf(p, "l8"); owner.incarnation != "p1" {
		t.Errorf("an empty report took a lease: %+v", owner)
	}
}

// A WITHDRAWAL RECORD ENDS WITH THE LAST LEASE ITS PROCESS OWNS, whichever path
// removed it.
func TestAWithdrawalRecordIsForgottenWithItsLastLease(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	seedOwner(p, "l9", "p1", 7)

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	p.mu.Lock()
	recorded := p.withdrawn["n1"]["p1"]
	delete(p.owners, "l9")
	p.expireStaleLocked()
	left := len(p.withdrawn)
	p.mu.Unlock()

	if !recorded {
		t.Fatal("the withdrawal of a process that owned a lease was not recorded")
	}
	if left != 0 {
		t.Errorf("a withdrawal record outlived the last lease its process owned: %d left", left)
	}
}

// THE LEDGER'S LAUNCHED SET NEVER MOVES A LEASE, and a successor whose first
// inventory was unknown still takes over at its first known reconciliation.
func TestAHandoverWaitsForTheSuccessorsOwnReport(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	seedOwner(p, "l9", "p1", 7)

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	registerAs(t, p, "p2")
	p.AdoptOwnershipWithInventory("n1", "p2", []string{"l9"}, false)
	if owner, _ := ownerOf(p, "l9"); owner.incarnation != "p1" {
		t.Fatalf("a lease moved on something other than the process's own report: %+v", owner)
	}

	p.AdoptOwnershipWithInventory("n1", "p2", []string{"l9"}, true)
	if owner, _ := ownerOf(p, "l9"); owner.incarnation != "p2" || owner.requestID != 7 {
		t.Errorf("the successor's first known report did not take the lease over: %+v", owner)
	}
}

// A LATE FAILED LAUNCH FROM THE WITHDRAWN PROCESS DOES NOT ERASE THE SUCCESSOR'S
// RECORD. The withdrawn process took the launch and never reported; after the
// lease moved, its late failure is about a process that no longer owns anything.
func TestALateFailedLaunchDoesNotEraseTheSuccessorsOwnership(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithCommandTimeout(time.Hour), WithRegistrar(newLedger()))
	registerAs(t, p, "p1")

	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), server.Job{RequestID: 7})
	}()

	waitFor(t, "the launch to be queued", func() bool { return p.QueuedForTest("n1") == 1 })

	launch, took, err := p.Poll(t.Context(), "n1", "p1")
	if err != nil || !took {
		t.Fatalf("the node did not take the launch: took=%v err=%v", took, err)
	}

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}
	<-launched

	if !p.OwnsForTest("l1", "n1", "p1") {
		t.Fatal("the process that took the launch does not own the lease, so this proves nothing")
	}

	registerAs(t, p, "p2")
	p.AdoptOwnershipWithInventory("n1", "p2", []string{"l1"}, true)
	if !p.OwnsForTest("l1", "n1", "p2") {
		t.Fatal("the successor that reported l1 did not take it over")
	}

	// The late report from the process that withdrew: a clean failure.
	_ = p.Result("n1", "p1", nodeapi.CommandResult{ID: launch.ID, Error: "late failure"})

	if !p.OwnsForTest("l1", "n1", "p2") {
		t.Error("a late failed launch from the withdrawn process erased the successor's ownership")
	}
}
