package state

import (
	"errors"
	"strings"
	"testing"
)

// testDeployment is the identity these tests claim under.
//
// ONE VALUE, SO A MISMATCH IN A TEST IS DELIBERATE. Claiming binds the ledger to
// the identity it was given, so a test that passed a different string each time
// would be exercising the refusal by accident and could not tell it from a bug.
const testDeployment = "deployment-under-test"

// THE CLAIM IS RECORDED AND THE GENERATION GOES UP.
//
// True on every backend, because the row is shared code: the exclusion differs
// and the bookkeeping does not.
func TestClaimingTheControllerAdvancesTheEpoch(t *testing.T) {
	db := open(t)

	first, err := db.ClaimController(t.Context(), "host-a", testDeployment)
	if err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	if first.Epoch != 1 {
		t.Errorf("the first claim is epoch %d, want 1; zero is what an unwritten row reads as, "+
			"so the column refuses it", first.Epoch)
	}

	second, err := db.ClaimController(t.Context(), "host-b", testDeployment)
	if err != nil {
		t.Fatalf("re-claiming: %v", err)
	}

	if second.Epoch != 2 {
		t.Errorf("the second claim is epoch %d, want 2", second.Epoch)
	}

	held, err := db.ControllerHolder(t.Context())
	if err != nil {
		t.Fatalf("ControllerHolder: %v", err)
	}

	if held.Holder != "host-b" || held.Epoch != 2 {
		t.Errorf("the ledger says %q at epoch %d, want host-b at 2", held.Holder, held.Epoch)
	}
}

// AN UNCLAIMED LEDGER IS AN ORDINARY STATE, not an error.
//
// A fresh deployment has never been claimed, and a reader that reported that as
// a failure would make `billet status` fail on exactly the deployment an
// operator is setting up.
func TestAnUnclaimedLedgerReportsNoHolder(t *testing.T) {
	db := open(t)

	held, err := db.ControllerHolder(t.Context())
	if err != nil {
		t.Fatalf("ControllerHolder on a fresh ledger: %v", err)
	}

	if held.Holder != "" || held.Epoch != 0 {
		t.Errorf("a fresh ledger reports %q at epoch %d, want no holder", held.Holder, held.Epoch)
	}
}

// A SECOND CONTROLLER ON ONE POSTGRESQL LEDGER IS REFUSED.
//
// THE PROPERTY THE WHOLE CLAIM EXISTS FOR, and the one a process-local flock
// cannot give: two controllers on two machines both take their own directory
// lock happily, and the only thing that can see both of them is the ledger they
// share. Two state directories here stand for two hosts.
func TestASecondControllerIsRefusedOnOnePostgresLedger(t *testing.T) {
	dsn := requirePostgres(t)

	first, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = first.Close() })

	if _, err := first.ClaimController(t.Context(), "host-a", testDeployment); err != nil {
		t.Fatalf("the first controller could not claim: %v", err)
	}

	// REFUSED BY THE OPEN, NOT BY A LATER CLAIM, and the difference is a schema.
	// Opening as a control plane takes the exclusion before it migrates, because
	// the alternative is a process that upgrades the shared schema on its way to
	// being told it is not the controller.
	second, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err == nil {
		t.Cleanup(func() { _ = second.Close() })
		t.Fatal("a second control plane opened a ledger another one holds")
	}

	if !errors.Is(err, ErrControllerHeld) {
		t.Fatalf("a second controller opened a held ledger; got %v", err)
	}

	// AND THE REFUSAL NAMES WHO HAS IT. An operator meeting this has two
	// machines and needs to know which one to stop; "already claimed" sends them
	// to look at both.
	if !strings.Contains(err.Error(), "host-a") {
		t.Errorf("the refusal should name the holder the ledger recorded; got: %v", err)
	}
}

// AND THE CLAIM IS RELEASED BY THE PROCESS ENDING, not by a clock.
//
// No lease, no renewal, no timeout. A timeout is a number that decides whether a
// live controller is declared dead, and the whole point of a session-scoped lock
// is that nobody has to make that decision: the server drops it when the
// connection goes.
func TestClosingAControllerReleasesItsClaim(t *testing.T) {
	dsn := requirePostgres(t)

	first, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := first.ClaimController(t.Context(), "host-a", testDeployment); err != nil {
		t.Fatalf("the first controller could not claim: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	second, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("opening a second handle: %v", err)
	}

	t.Cleanup(func() { _ = second.Close() })

	claim, err := second.ClaimController(t.Context(), "host-b", testDeployment)
	if err != nil {
		t.Fatalf("a replacement controller could not claim a released ledger: %v", err)
	}

	// AND THE GENERATION CARRIED FORWARD. A replacement that restarted the epoch
	// would be indistinguishable from the controller it replaced, which is
	// exactly what a fencing token must never be.
	if claim.Epoch != 2 {
		t.Errorf("the replacement claimed at epoch %d, want 2", claim.Epoch)
	}
}

// AN OPERATOR COMMAND CANNOT CLAIM TO BE THE CONTROLLER.
//
// OpenAdmin deliberately proceeds WITHOUT the directory lock when a control
// plane holds it, which is what lets `billet leases release --force` reach a
// running deployment's ledger at all. Such a handle holds no exclusion, so a
// claim from it would write a row saying it is the controller while the real one
// carries on — and the row is what a refusal quotes.
//
// NOTHING IN BILLET DOES THIS, and the exported API allowed it.
func TestAnUnlockedHandleCannotClaimTheController(t *testing.T) {
	dir := t.TempDir()

	plane, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	t.Cleanup(func() { _ = plane.Close() })

	command, err := OpenAdmin(t.Context(), dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = command.Close() })

	if _, err := command.ClaimController(t.Context(), "an-operator-command", testDeployment); !errors.Is(
		err, ErrControllerHeld) {
		t.Fatalf("a handle without the directory lock claimed the controller; got %v", err)
	}
}
