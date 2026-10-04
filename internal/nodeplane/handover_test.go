package nodeplane

import (
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/server"
)

// A PROCESS THAT WITHDREW HANDS WHAT IT OWNED TO THE NODE'S NEXT PROCESS (#374).
//
// A node handing over leaves its compute running and exits. Its successor adopts
// that compute, and only the successor can act on a completion's destroy; left
// with the withdrawn owner, the destroy answered "holder unavailable" for as long
// as the plane ran and a finished job's VM was never torn down.
func TestAProcessThatWithdrewHandsItsLeasesToTheNextOne(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	p.AdoptOwnership("n1", "p1", []string{"l9"})

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	registerAs(t, p, "p2")
	p.AdoptOwnership("n1", "p2", []string{"l9"})

	if !p.OwnsForTest("l9", "n1", "p2") {
		t.Fatal("the process that adopted the withdrawn process's lease does not own it")
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
	p.AdoptOwnership("n1", "p1", []string{"l9"})

	registerAs(t, p, "p2")
	p.AdoptOwnership("n1", "p2", []string{"l9"})

	if !p.OwnsForTest("l9", "n1", "p1") {
		t.Error("a replacement took a lease its superseded, still-draining process owns")
	}
}

// ONLY WHAT THE SUCCESSOR HOLDS MOVES. A withdrawn process's lease the next
// process did not find in its inventory stays where it was, for the ordinary
// reconciliation to settle.
func TestOnlyALeaseTheSuccessorAdoptedMoves(t *testing.T) {
	t.Parallel()

	p := testPlane(t, WithRegistrar(newLedger()))
	registerAs(t, p, "p1")
	p.AdoptOwnership("n1", "p1", []string{"l8", "l9"})

	if err := p.Withdraw(t.Context(), "n1", "p1"); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	registerAs(t, p, "p2")
	p.AdoptOwnership("n1", "p2", []string{"l9"})

	if !p.OwnsForTest("l9", "n1", "p2") {
		t.Error("the adopted lease did not move to the successor")
	}
	if p.OwnsForTest("l8", "n1", "p2") {
		t.Error("a lease the successor never reported moved to it")
	}
}
