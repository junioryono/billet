package state

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// openStandby opens a standby handle over a schema of the caller's own.
func openStandby(t *testing.T, dsn DSN) *DB {
	t.Helper()

	db, err := OpenPostgresStandby(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgresStandby: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

// A STANDBY WRITES NOTHING UNTIL IT HAS CLAIMED, AND THEN WRITES.
//
// THE HOLE THIS CLOSES IS NOT THE OBVIOUS ONE. checkLeadership exempts a handle
// whose claimedEpoch is zero — deliberately, so migrations and operator commands
// work — which means the epoch fence protects a controller that has been
// REPLACED and does nothing at all for one that has never claimed. A standby is
// exactly that, so without a refusal of its own it would write to a live
// deployment's ledger completely unfenced.
func TestAStandbyHandleRefusesEveryWriteUntilItClaims(t *testing.T) {
	dsn := requirePostgres(t)

	// THE LEDGER IS MIGRATED BY A CONTROLLER FIRST, because a standby does not
	// migrate and this test is about writing rather than about schema.
	leader, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := leader.ClaimController(t.Context(), "leader", testDeployment); err != nil {
		t.Fatalf("the leader could not claim: %v", err)
	}

	standby := openStandby(t, dsn)

	err = standby.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the callback must not run for a standby that has not claimed")

		return nil
	})
	if !errors.Is(err, ErrStandby) {
		t.Fatalf("a standby's write was not refused with ErrStandby; got %v", err)
	}

	// READS ARE NOT REFUSED. A standby has to be able to see the ledger to report
	// on itself and on the claim it is waiting for.
	if _, err := standby.ControllerHolder(t.Context()); err != nil {
		t.Errorf("a standby could not read the claim it is waiting for: %v", err)
	}

	// AND THE CLAIM IS STILL REFUSED WHILE THE LEADER HOLDS IT.
	if _, err := standby.ClaimController(
		t.Context(), "standby", testDeployment); !errors.Is(err, ErrControllerHeld) {
		t.Fatalf("a standby claimed a held ledger; got %v", err)
	}

	// AND IT STILL CANNOT WRITE. This one is cheap and proves less than it looks:
	// a claim refused by the EXCLUSION returns before the latch is ever cleared,
	// so it exercises no unwind at all. What covers the unwind is
	// TestAClaimThatFailsAfterTakingTheExclusionPutsTheLatchBack, which is the
	// only path that reaches it — measured, because this assertion alone left the
	// unwind's mutant alive.
	if err := standby.Tx(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(
		err, ErrStandby) {
		t.Fatalf("a standby whose claim was refused could write; got %v", err)
	}

	if err := leader.Close(); err != nil {
		t.Fatalf("closing the leader: %v", err)
	}

	// AND ONCE THE INCUMBENT IS GONE IT PROMOTES AND WRITES.
	claim, err := standby.AwaitController(promoteCtx(t), "standby", testDeployment, nil)
	if err != nil {
		t.Fatalf("the standby could not promote: %v", err)
	}

	if claim.Epoch != 2 {
		t.Errorf("the promoted standby claimed at epoch %d, want 2", claim.Epoch)
	}

	if err := standby.Tx(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("a promoted standby could not write: %v", err)
	}
}

// promoteCtx bounds a wait for the claim so a lock that never comes free fails
// with a sentence rather than hanging the suite.
func promoteCtx(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// A STANDBY WAITS OUT A HELD CLAIM AND NOTHING ELSE.
//
// A held claim is the ordinary state a standby exists for and resolves by
// itself. A ledger bound to another deployment does not resolve by waiting, ever
// — so retrying it would turn a misconfiguration into a process that sits there
// reporting itself healthy while a deployment has no standby at all.
func TestAStandbyDoesNotWaitOutARefusalThatWillNeverResolve(t *testing.T) {
	dsn := requirePostgres(t)

	leader, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := leader.ClaimController(t.Context(), "leader", testDeployment); err != nil {
		t.Fatalf("the leader could not claim: %v", err)
	}

	if err := leader.Close(); err != nil {
		t.Fatalf("closing the leader: %v", err)
	}

	standby := openStandby(t, dsn)

	// The claim is free, so nothing here is waiting on a lock; what refuses is
	// the binding, and it must come back rather than be retried forever.
	_, err = standby.AwaitController(
		promoteCtx(t), "standby", "a-different-deployment", nil)
	if !errors.Is(err, ErrForeignLedger) {
		t.Fatalf("AwaitController should return a refusal that cannot resolve; got %v", err)
	}
}

// A CLAIM THAT FAILS AFTER TAKING THE EXCLUSION PUTS THE LATCH BACK.
//
// THE ONLY PATH THAT REACHES THE UNWIND, and finding that out cost a surviving
// mutant: a claim refused by the EXCLUSION returns before the latch is ever
// cleared, so the obvious assertion — "a standby whose claim was refused still
// cannot write" — is satisfied by a function that never unwound anything.
//
// What gets past the exclusion and then fails is a ledger bound to another
// deployment. At that point the latch is off and this process is not the
// controller, so without the unwind it would be a standby that can write to a
// live deployment's ledger, which is the exact hole the latch exists to close.
func TestAClaimThatFailsAfterTakingTheExclusionPutsTheLatchBack(t *testing.T) {
	dsn := requirePostgres(t)

	// A BOUND LEDGER WITH THE CLAIM FREE, so the exclusion is takeable and the
	// binding is what refuses.
	leader, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := leader.ClaimController(t.Context(), "leader", testDeployment); err != nil {
		t.Fatalf("the leader could not claim: %v", err)
	}

	if err := leader.Close(); err != nil {
		t.Fatalf("closing the leader: %v", err)
	}

	standby := openStandby(t, dsn)

	if _, err := standby.ClaimController(
		t.Context(), "standby", "a-different-deployment"); !errors.Is(err, ErrForeignLedger) {
		t.Fatalf("expected ErrForeignLedger past the exclusion; got %v", err)
	}

	if err := standby.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the callback must not run for a standby whose claim did not complete")

		return nil
	}); !errors.Is(err, ErrStandby) {
		t.Fatalf("a standby whose claim failed after taking the exclusion could write; got %v", err)
	}

	// AND THE EXCLUSION WENT BACK TOO, so the deployment is not left excluded by a
	// process that does not believe it is the controller.
	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("a controller could not start after a failed claim released nothing: %v", err)
	}

	t.Cleanup(func() { _ = plane.Close() })
}

// AND IT REPORTS WHO IT IS WAITING FOR.
//
// The callback is what a standby's log line and its systemd STATUS are built
// from, so an operator can see which machine holds the deployment rather than
// only that this one is not serving.
func TestAWaitingStandbyReportsTheHolder(t *testing.T) {
	dsn := requirePostgres(t)

	leader, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := leader.ClaimController(t.Context(), "the-incumbent", testDeployment); err != nil {
		t.Fatalf("the leader could not claim: %v", err)
	}

	standby := openStandby(t, dsn)

	seen := make(chan ControllerClaim, 1)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	go func() {
		// The wait is cancelled by this test, so its error is the cancellation and
		// says nothing; what is being observed is the callback.
		//nolint:errcheck // see above: this call is ended by the deferred cancel.
		_, _ = standby.AwaitController(ctx, "standby", testDeployment, func(held ControllerClaim) {
			select {
			case seen <- held:
			default:
			}
		})
	}()

	select {
	case held := <-seen:
		if held.Holder != "the-incumbent" || held.Epoch != 1 {
			t.Errorf("the standby reported %q at epoch %d, want the-incumbent at 1",
				held.Holder, held.Epoch)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a waiting standby never reported the holder")
	}

	cancel()

	if err := leader.Close(); err != nil {
		t.Errorf("closing the leader: %v", err)
	}
}

// A STANDBY MIGRATES AT PROMOTION AND NOT BEFORE.
//
// It opened without the controller exclusion and therefore without the right to
// change the schema. Promotion is the first moment it has that right, which is
// what lets a NEWER standby wait beside an OLDER leader — the shape a
// follower-first upgrade needs and the one a stricter check would refuse.
func TestAStandbyMigratesOnlyAtPromotion(t *testing.T) {
	dsn := requirePostgres(t)

	standby := openStandby(t, dsn)

	// NOTHING WAS CREATED. The ledger is untouched: no controller has ever run
	// here, and a standby is not one.
	exists, err := standby.backend.bookkeepingTableExists(t.Context(), standby.Reader())
	if err != nil {
		t.Fatalf("inspect the ledger's schema: %v", err)
	}

	if exists {
		t.Fatal("a standby migrated a ledger it holds no exclusion for")
	}

	if _, err := standby.AwaitController(
		promoteCtx(t), "standby", testDeployment, nil); err != nil {
		t.Fatalf("the standby could not promote: %v", err)
	}

	// AND NOW IT HAS, because it is the controller.
	exists, err = standby.backend.bookkeepingTableExists(t.Context(), standby.Reader())
	if err != nil {
		t.Fatalf("inspect the ledger's schema: %v", err)
	}

	if !exists {
		t.Fatal("a promoted standby did not migrate the ledger it now holds")
	}
}

// A LEDGER AHEAD OF THIS BINARY IS REFUSED AT OPEN, NOT AT THE FAILOVER.
//
// The two schema questions are not the same question. "The ledger carries a
// version I have never heard of" says this binary could not serve this
// deployment at all, and a standby that discovered it at PROMOTION would
// discover it at the worst possible moment with nothing it could do. "The ledger
// has not applied a migration I carry" says only that the leader is older, which
// is the ordinary state of a follower-first upgrade.
func TestAStandbyRefusesALedgerWrittenByANewerBillet(t *testing.T) {
	dsn := requirePostgres(t)

	leader, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	// A VERSION THIS BINARY HAS NEVER HEARD OF, which is what a newer billet
	// leaves behind.
	if err := leader.Tx(t.Context(), func(tx *sql.Tx) error {
		return WriteQueries(tx).RecordMigration(t.Context(), ledgerdb.RecordMigrationParams{
			Version:   9999,
			Name:      "from-the-future",
			Checksum:  "x",
			AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}); err != nil {
		t.Fatalf("record a future migration: %v", err)
	}

	if err := leader.Close(); err != nil {
		t.Fatalf("closing the leader: %v", err)
	}

	db, err := OpenPostgresStandby(t.Context(), t.TempDir(), dsn)
	if err == nil {
		t.Cleanup(func() { _ = db.Close() })
		t.Fatal("a standby opened a ledger written by a newer billet")
	}
}
