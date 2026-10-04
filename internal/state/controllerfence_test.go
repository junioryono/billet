package state

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// The two hosts every test in this file is about: the controller that claimed
// first, and the replacement that takes the deployment from it.
//
// CONSTANTS RATHER THAN ARGUMENTS, because there is one story here and passing
// the same two strings in at every call site is a parameter that can never vary
// — which unparam says out loud, and which would also let two tests disagree
// about which name means which role.
const (
	incumbentHolder   = "host-a"
	replacementHolder = "host-b"
)

// stageSuccessorClaim writes the row a REPLACEMENT controller's claim writes,
// through a handle that is not the one under test.
//
// A SECOND HANDLE RATHER THAN THE FIRST ONE, because the property under test is
// what happens to a handle whose epoch moved underneath it. Staging the bump
// through that same handle would make the test depend on its own fence passing
// first, so an inverted comparison would fail in the setup rather than in the
// assertion — which is a mutation surviving in the place it matters most.
//
// DIRECTLY RATHER THAN THROUGH ClaimController, because on SQLite no second
// process can take the exclusion at all: the directory lock refuses one, and
// sqliteBackend.claimController refuses a handle that does not hold it. What a
// successor does to the LEDGER is exactly this statement, and the end-to-end
// article — a real session lost, a real replacement — is
// TestAControllerCannotWriteOnceAReplacementHasClaimed below.
func stageSuccessorClaim(t *testing.T, db *DB) {
	t.Helper()

	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := WriteQueries(tx).ClaimController(t.Context(), ledgerdb.ClaimControllerParams{
			Holder:    replacementHolder,
			ClaimedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})

		return err
	}); err != nil {
		t.Fatalf("stage a successor's claim: %v", err)
	}
}

// planeAndCommand opens the two handles one deployment has: the control plane,
// which holds the directory lock, and an operator command, which deliberately
// does not.
func planeAndCommand(t *testing.T) (*DB, *DB) {
	t.Helper()

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

	return plane, command
}

// claimIncumbent takes the controller claim for the host that holds it first.
func claimIncumbent(t *testing.T, db *DB) ControllerClaim {
	t.Helper()

	claim, err := db.ClaimController(t.Context(), incumbentHolder, testDeployment)
	if err != nil {
		t.Fatalf("ClaimController(%s): %v", incumbentHolder, err)
	}

	return claim
}

// A WRITE IS REFUSED ONCE A SUCCESSOR HAS CLAIMED, AND THE CALLBACK NEVER RUNS.
//
// THE PROPERTY THE EPOCH EXISTS FOR. A controller that lost its exclusion
// without noticing — a dropped PostgreSQL session, a pooling proxy, a partition
// — must be unable to write, not merely able to find out afterwards.
//
// THE CALLBACK IS THE HALF THAT MATTERS. Asserting only that an error came back
// would agree with a Tx that ran the caller's closure, let it make a scheduling
// decision and keep whatever it built, and refused only the COMMIT.
func TestAWriteIsRefusedOnceASuccessorHasClaimed(t *testing.T) {
	plane, command := planeAndCommand(t)

	claimIncumbent(t, plane)
	stageSuccessorClaim(t, command)

	err := plane.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the callback must not run for a process that is no longer the controller")

		return nil
	})
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("a fenced controller's write returned %v, want ErrLeadershipLost", err)
	}

	// AND THE REFUSAL NAMES WHO TOOK OVER. An operator meeting this has two
	// machines and needs to know which one is authoritative now; both epochs are
	// there because "somebody else has it" does not say how far behind this
	// process is.
	for _, want := range []string{replacementHolder, "epoch 1", "epoch 2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q; got: %v", want, err)
		}
	}
}

// AN OPERATOR COMMAND IS NEVER FENCED, AND WRITES WHILE THE CONTROLLER CANNOT.
//
// OpenAdmin exists so `billet leases release --force` can reach a live
// deployment's ledger, and such a handle has no claim to lose. Fencing it would
// refuse exactly the command an operator runs when capacity is already missing.
func TestAnOperatorCommandWritesWhileTheControllerIsFenced(t *testing.T) {
	plane, command := planeAndCommand(t)

	claimIncumbent(t, plane)
	stageSuccessorClaim(t, command)

	if err := plane.Tx(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(
		err, ErrLeadershipLost) {
		t.Fatalf("the control plane should be fenced; got %v", err)
	}

	ran := false

	if err := command.Tx(t.Context(), func(*sql.Tx) error {
		ran = true

		return nil
	}); err != nil {
		t.Fatalf("an operator command must still be able to write: %v", err)
	}

	if !ran {
		t.Error("the operator command's callback did not run")
	}
}

// READS ARE NOT FENCED.
//
// A read is not an authoritative act, and everything that reports on a
// deployment goes through View — including the diagnostic that would explain the
// refusal. A fence there would leave a fenced control plane unable to say why.
func TestAFencedControllerCanStillRead(t *testing.T) {
	plane, command := planeAndCommand(t)

	claimIncumbent(t, plane)
	stageSuccessorClaim(t, command)

	held, err := plane.ControllerHolder(t.Context())
	if err != nil {
		t.Fatalf("a fenced controller must still be able to read: %v", err)
	}

	if held.Holder != replacementHolder || held.Epoch != 2 {
		t.Errorf("the read reported %q at epoch %d, want host-b at 2", held.Holder, held.Epoch)
	}
}

// A CLAIM ROW THAT HAS GONE REFUSES THE WRITE; IT DOES NOT PERMIT IT.
//
// This handle claimed, so the row existed. Its absence is billet unable to say
// who the controller is, and a could-not-tell that resolves to yes is the
// collapse this codebase removes everywhere it appears.
func TestAVanishedClaimRowRefusesAWrite(t *testing.T) {
	plane, command := planeAndCommand(t)

	claimIncumbent(t, plane)

	if err := command.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DELETE FROM controller_claim WHERE id = 1`)

		return err
	}); err != nil {
		t.Fatalf("delete the claim row: %v", err)
	}

	err := plane.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the callback must not run when billet cannot say who the controller is")

		return nil
	})
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("a missing claim row returned %v, want ErrLeadershipLost", err)
	}
}

// A FAILED CLAIM READ IS REPORTED AS THE FAULT IT IS, AND STILL REFUSES.
//
// A database error is evidence about leadership in NEITHER direction. Reporting
// it as ErrLeadershipLost would stop a healthy control plane over a blip and
// send an operator looking for a second controller that does not exist;
// reporting it as nil would let a fenced one write. It refuses, and it refuses
// as the storage fault.
func TestAFailedClaimReadRefusesTheWriteWithoutClaimingLeadershipMoved(t *testing.T) {
	plane, command := planeAndCommand(t)

	claimIncumbent(t, plane)

	if err := command.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP TABLE controller_claim`)

		return err
	}); err != nil {
		t.Fatalf("drop the claim table: %v", err)
	}

	err := plane.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the callback must not run when the fence could not be read")

		return nil
	})
	if err == nil {
		t.Fatal("a write whose fence could not be read must be refused")
	}

	if errors.Is(err, ErrLeadershipLost) {
		t.Errorf("a read failure must not be reported as a lost leadership: %v", err)
	}

	if !strings.Contains(err.Error(), "controller_claim") {
		t.Errorf("the refusal should carry the database's own fault; got: %v", err)
	}

	if plane.LeadershipLost() {
		t.Error("a read failure must not latch the leadership-lost flag")
	}
}

// LeadershipLost IS FALSE UNTIL A WRITE IS REFUSED, AND NEVER FALSE AGAIN.
//
// It is what the control plane's teardown asks before it destroys compute,
// closes a session or hands back capacity, so a caller that reads true must
// never read false afterwards and act on it.
func TestLeadershipLostLatchesOnTheFirstRefusal(t *testing.T) {
	plane, command := planeAndCommand(t)

	claimIncumbent(t, plane)

	if plane.LeadershipLost() {
		t.Fatal("a controller that has just claimed has not lost leadership")
	}

	stageSuccessorClaim(t, command)

	// STILL FALSE UNTIL A WRITE IS ACTUALLY REFUSED. Nothing polls the row, and
	// saying otherwise would be a claim about a moment billet never observed.
	if plane.LeadershipLost() {
		t.Error("nothing has been refused yet, so nothing has been discovered yet")
	}

	if err := plane.Tx(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(
		err, ErrLeadershipLost) {
		t.Fatalf("the write should have been refused; got %v", err)
	}

	if !plane.LeadershipLost() {
		t.Fatal("a refused write must latch the flag the teardown reads")
	}

	// AND IT STAYS SET, including across a successful read that finds the same
	// row. Nothing may clear it.
	if _, err := plane.ControllerHolder(t.Context()); err != nil {
		t.Fatalf("ControllerHolder: %v", err)
	}

	if !plane.LeadershipLost() {
		t.Error("the flag cleared after a later read")
	}
}

// THE SIGNAL FIRES AT THE INSTANT THE WRITE IS REFUSED.
//
// THE FLAG IS NOT ENOUGH ON ITS OWN, which is why both exist. Nothing in the
// control plane treats an unclassified error as a reason to stop — a heartbeat
// keeps its lease, the reaper logs and retries, a cleanup retry backs off — so a
// replaced controller whose writes are all being refused would keep polling
// GitHub and keep running the loop that calls Runner.Destroy, which never
// touches the ledger. What stops it is this channel closing.
func TestTheLeadershipSignalFiresOnTheFirstRefusedWrite(t *testing.T) {
	plane, command := planeAndCommand(t)

	signal := plane.LeadershipLostSignal()
	if signal == nil {
		t.Fatal("a ledger opened for a control plane has no leadership signal to select on")
	}

	select {
	case <-signal:
		t.Fatal("the signal fired before anything was refused")
	default:
	}

	claimIncumbent(t, plane)
	stageSuccessorClaim(t, command)

	// STILL SILENT. Nothing polls the row, so the signal is a consequence of a
	// refusal rather than of the successor's claim — asserting otherwise would be
	// asserting a moment billet never observes.
	select {
	case <-signal:
		t.Fatal("the signal fired before this process had been refused anything")
	default:
	}

	if err := plane.Tx(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(
		err, ErrLeadershipLost) {
		t.Fatalf("the write should have been refused; got %v", err)
	}

	select {
	case <-signal:
	default:
		t.Fatal("a refused write did not fire the signal the control plane stops on")
	}

	// AND A SECOND REFUSAL DOES NOT CLOSE IT TWICE, which would panic. Reached
	// through the production path rather than by calling markFenced directly.
	if err := plane.Tx(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(
		err, ErrLeadershipLost) {
		t.Fatalf("the second write should have been refused too; got %v", err)
	}
}

// A FENCE THAT COULD NOT BE READ DOES NOT FIRE THE SIGNAL.
//
// The signal stops the control plane, so firing it on a database blip would take
// a healthy deployment offline over a query that timed out. The WRITE is still
// refused, which is where a could-not-tell has to be answered no; deciding the
// process must die is a different question with a different answer.
func TestAFailedClaimReadDoesNotFireTheLeadershipSignal(t *testing.T) {
	plane, command := planeAndCommand(t)

	claimIncumbent(t, plane)

	if err := command.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `DROP TABLE controller_claim`)

		return err
	}); err != nil {
		t.Fatalf("drop the claim table: %v", err)
	}

	if err := plane.Tx(t.Context(), func(*sql.Tx) error { return nil }); err == nil {
		t.Fatal("a write whose fence could not be read must be refused")
	}

	select {
	case <-plane.LeadershipLostSignal():
		t.Error("a read failure fired the signal that stops the control plane")
	default:
	}
}

// A HANDLE THAT NEVER CLAIMED IS NOT FENCED, WHICH IS WHAT LETS BILLET START.
//
// migrate runs inside DB.Tx before any claim exists, so a fence that engaged on
// an unclaimed handle would refuse the migration that creates the table it
// reads. Open having succeeded at all is most of this; the write afterwards is
// the rest.
func TestAnUnclaimedHandleWritesWithoutAFence(t *testing.T) {
	db := open(t)

	ran := false

	if err := db.Tx(t.Context(), func(*sql.Tx) error {
		ran = true

		return nil
	}); err != nil {
		t.Fatalf("a handle that never claimed must be able to write: %v", err)
	}

	if !ran {
		t.Error("the callback did not run")
	}

	if db.LeadershipLost() {
		t.Error("a handle that never claimed cannot have lost leadership")
	}
}

// A CONTROLLER IS NOT FENCED AGAINST ITS OWN CLAIM.
//
// ClaimController records its epoch AFTER the transaction that writes it. Doing
// it before would have the recording transaction read a row that does not yet
// carry the value it is about to write, and refuse the claim it is making — a
// control plane that cannot start, produced by the fence that exists to let one
// stop safely.
func TestClaimingIsNotFencedAgainstItsOwnClaim(t *testing.T) {
	db := open(t)

	first := claimIncumbent(t, db)

	// A SECOND CLAIM ON THE SAME HANDLE, which is the sharper case: the fence is
	// live by now, and the epoch this transaction reads is the one it is about to
	// replace.
	second := claimIncumbent(t, db)

	if second.Epoch != first.Epoch+1 {
		t.Errorf("re-claiming produced epoch %d, want %d", second.Epoch, first.Epoch+1)
	}

	if err := db.Tx(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("a controller must be able to write at the epoch it just claimed: %v", err)
	}
}

// A CONTROLLER THAT LOST ITS SESSION CANNOT WRITE ONCE A REPLACEMENT CLAIMS.
//
// THE FAILURE THAT WAS DOCUMENTED AS OPEN, END TO END AND WITHOUT SIMULATING
// ANY OF IT. The claim is a session-scoped advisory lock, so a PostgreSQL
// restart, a failover, an idle_session_timeout, a pooling proxy or a partition
// ends the session and releases the lock — and the holding process finds out
// about none of it, because nothing uses that connection again. Terminating the
// backend IS that event rather than a stand-in for it.
//
// TWO STATE DIRECTORIES STAND FOR TWO HOSTS. The directory lock is per machine
// and is not what is under test; the ledger is the only thing that can see both
// processes.
func TestAControllerCannotWriteOnceAReplacementHasClaimed(t *testing.T) {
	dsn := requirePostgres(t)

	first, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = first.Close() })

	claimIncumbent(t, first)

	// THE CLAIM'S OWN BACKEND, ASKED THROUGH THE CONNECTION THAT HOLDS THE LOCK.
	// That pool is MaxOpenConns(1) precisely because the lock lives in the
	// session, so this is the pid whose exit releases it — reading it out of
	// pg_locks instead would need the lock key, and two tests sharing a server
	// would then be able to terminate each other's controller.
	be, ok := first.backend.(*postgresBackend)
	if !ok {
		t.Fatalf("a PostgreSQL ledger has a %T backend", first.backend)
	}

	var pid int
	if err := be.claim.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatalf("read the claim session's backend pid: %v", err)
	}

	admin, err := sql.Open("pgx", string(dsn))
	if err != nil {
		t.Fatalf("open an administrative connection: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	var terminated bool
	if err := admin.QueryRowContext(t.Context(),
		`SELECT pg_terminate_backend($1)`, pid).Scan(&terminated); err != nil {
		t.Fatalf("terminate the claim session: %v", err)
	}

	if !terminated {
		t.Fatalf("pg_terminate_backend(%d) reported nothing to terminate", pid)
	}

	second, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("opening a replacement: %v", err)
	}

	t.Cleanup(func() { _ = second.Close() })

	// POLLED, BECAUSE pg_terminate_backend SIGNALS AND THE LOCK IS RELEASED WHEN
	// THE BACKEND ACTUALLY EXITS. Bounded, so a lock that never comes free fails
	// with a sentence rather than hanging.
	var claim ControllerClaim

	deadline := time.Now().Add(10 * time.Second)

	for {
		claim, err = second.ClaimController(t.Context(), replacementHolder, testDeployment)
		if err == nil {
			break
		}

		if !errors.Is(err, ErrControllerHeld) {
			t.Fatalf("the replacement's claim failed for a reason other than the lock: %v", err)
		}

		if time.Now().After(deadline) {
			t.Fatalf("the advisory lock was still held %s after its session was terminated",
				10*time.Second)
		}

		time.Sleep(25 * time.Millisecond)
	}

	// THE GENERATION CARRIED FORWARD. A replacement that restarted the epoch would
	// be indistinguishable from the controller it replaced, which is exactly what
	// a fencing token must never be.
	if claim.Epoch != 2 {
		t.Fatalf("the replacement claimed at epoch %d, want 2", claim.Epoch)
	}

	// AND THE FIRST ONE'S WRITER POOL IS STILL PERFECTLY HEALTHY, which is the
	// whole difficulty: nothing about losing the claim session tells this process
	// anything. The epoch is what refuses it.
	err = first.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the callback must not run for a controller that has been replaced")

		return nil
	})
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("the replaced controller's write returned %v, want ErrLeadershipLost", err)
	}

	if !strings.Contains(err.Error(), replacementHolder) {
		t.Errorf("the refusal should name the replacement; got: %v", err)
	}

	if !first.LeadershipLost() {
		t.Error("the refusal must latch the flag the teardown reads")
	}

	// AND THE REPLACEMENT WRITES NORMALLY. Without this the test passes against a
	// fence that refuses everybody.
	if err := second.Tx(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Fatalf("the replacement controller must be able to write: %v", err)
	}
}
