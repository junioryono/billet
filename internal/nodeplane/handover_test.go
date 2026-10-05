package nodeplane

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
)

// seedOwner records a lease as delivered to n1's process p1, with the job it was
// launched for, the way a launch's delivery records it.
func seedOwner(p *Plane, lease string, requestID int64) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.owners == nil {
		p.owners = make(map[string]leaseOwner)
	}
	p.owners[lease] = leaseOwner{node: "n1", incarnation: "p1", requestID: requestID}
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
	seedOwner(p, "l9", 7)

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
			t.Context(), 7, "Succeeded", "l9", "n1", 1, alloc.PhaseDone, dispatch.CacheAuthority{})
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
	seedOwner(p, "l9", 7)

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
	seedOwner(p, "l8", 8)
	seedOwner(p, "l9", 9)

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
	seedOwner(p, "l9", 7)

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
	seedOwner(p, "l9", 7)

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
		launched <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7})
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
	// Its answer is not the subject; what it leaves of ownership is.
	if err := p.Result("n1", "p1", nodeapi.CommandResult{ID: launch.ID, Error: "late failure"}); err != nil {
		t.Logf("the late report was answered %v", err)
	}

	if !p.OwnsForTest("l1", "n1", "p2") {
		t.Error("a late failed launch from the withdrawn process erased the successor's ownership")
	}
}

// A WITHDRAWN PROCESS'S STALE ADOPTION ENDS WITH THE LEDGER'S WORD. A process
// adopts a lease whose completion has just ended it, then withdraws; the next
// process's registration carries the ledger's launched set, which no longer holds
// the lease, and that is what prunes the record and the withdrawal with it.
func TestAWithdrawnProcesssEndedAdoptionIsPrunedByTheLedgersWord(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	p.AdoptOwnershipWithInventory("n1", "p1", []string{"l9"}, true)

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	registerAs(t, p, "p2")
	// The successor reports nothing, and the ledger's launched set no longer
	// holds l9.
	p.AdoptOwnershipWithInventory("n1", "p2", nil, true)
	if owner, ok := ownerOf(p, "l9"); !ok || owner.incarnation != "p1" {
		t.Fatalf("a report pruned another process's adoption: %+v (ok=%v)", owner, ok)
	}

	p.AdoptOwnership("n1", "p2", nil)
	if owner, ok := ownerOf(p, "l9"); ok {
		t.Errorf("an adoption the ledger says ended outlived the next registration: %+v", owner)
	}

	p.mu.Lock()
	left := len(p.withdrawn)
	p.mu.Unlock()
	if left != 0 {
		t.Errorf("the withdrawal record outlived the last lease its process owned: %d left", left)
	}
}

// A QUARANTINED LEASE IS NOT ENDED. The launched set leaves quarantine out, so a
// superseded process's adoption of quarantined compute is kept by the ledger's
// quarantined set, which is never adopted by the registering process.
func TestAnotherProcesssQuarantinedAdoptionIsKept(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	p.AdoptOwnershipWithInventory("n1", "p1", []string{"l7"}, true)

	registerAs(t, p, "p2")
	p.AdoptOwnershipKeeping("n1", "p2", nil, map[string]bool{"l7": true})

	if owner, ok := ownerOf(p, "l7"); !ok || owner.incarnation != "p1" {
		t.Errorf("a superseded process lost a quarantined lease it may still be draining: %+v (ok=%v)",
			owner, ok)
	}
}

// A KEPT ADOPTION IS FORGOTTEN WHEN ITS LEASE ENDS, without another registration.
// Registration keeps a superseded process's adoption of a quarantined lease; when a
// reconciliation later ends that lease, the adoption and the withdrawal record it
// keeps alive go with it, on the ledger's word and only on it.
func TestAKeptAdoptionIsForgottenWhenItsLeaseEnds(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		open        bool
		leaseErr    error
		stillDrains bool
		forgets     bool
	}{
		{name: "the lease ended", forgets: true},
		{name: "the lease is still open", open: true},
		{name: "the ledger could not tell", leaseErr: errors.New("ledger unavailable")},
		{name: "the process did not withdraw", stillDrains: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ledger := newLedger()
			p := testPlane(t, WithRegistrar(ledger))
			registerAs(t, p, "p1")
			p.AdoptOwnershipWithInventory("n1", "p1", []string{"l7"}, true)
			if !tc.stillDrains {
				if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
					t.Fatalf("withdraw p1: %v", err)
				}
			}

			registerAs(t, p, "p2")
			p.AdoptOwnershipKeeping("n1", "p2", nil, map[string]bool{"l7": true})
			if _, ok := ownerOf(p, "l7"); !ok {
				t.Fatal("registration did not keep the quarantined adoption")
			}

			ledger.mu.Lock()
			ledger.open = map[string]bool{"l7": tc.open}
			ledger.leaseErr = tc.leaseErr
			ledger.mu.Unlock()

			if _, err := p.ReconcileInventory(t.Context(), "n1", "p2", nil); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			_, owned := ownerOf(p, "l7")
			p.mu.Lock()
			recorded := p.withdrawn["n1"]["p1"]
			p.mu.Unlock()
			if owned == tc.forgets {
				t.Errorf("after reconciliation the adoption is owned=%v, want %v", owned, !tc.forgets)
			}
			if !tc.stillDrains && recorded == tc.forgets {
				t.Errorf("after reconciliation the withdrawal is recorded=%v, want %v", recorded, !tc.forgets)
			}
		})
	}
}
