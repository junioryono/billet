package state

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

// A PARTITIONED CONTROLLER IS REPLACED BY A WAITING STANDBY, AND IS FENCED.
//
// THE WHOLE OF THE CONTROLLER ELECTION IN ONE TEST, and it simulates nothing. The
// incumbent's claim session is terminated on the server — which is what a
// partition, an idle_session_timeout, a pooling proxy or a database failover each
// do — and everything after that is production code:
//
//   - the standby is WAITING on AwaitController, not polling a flag a test set;
//   - it promotes because PostgreSQL released the lock, not because anything
//     decided the incumbent was dead;
//   - the incumbent's next write is REFUSED, and its stop signal fires, so the
//     process that has been replaced stops rather than carrying on;
//   - and the generation moved, so the two are distinguishable.
//
// WHAT IT DOES NOT PROVE, said rather than implied: that the promoted process
// serves the fleet. That needs the promoted host to hold this deployment's
// node-wire authority, which is internal/wireshare's subject and is tested there,
// and an address the nodes reach, which billet does not own.
func TestAStandbyPromotesWhenTheControllersSessionIsTerminated(t *testing.T) {
	dsn := requirePostgres(t)

	// TWO STATE DIRECTORIES STAND FOR TWO HOSTS. Each takes its own directory
	// lock happily, which is exactly why the ledger has to be what excludes them.
	incumbent, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	t.Cleanup(func() { _ = incumbent.Close() })

	if _, err := incumbent.ClaimController(
		t.Context(), incumbentHolder, testDeployment); err != nil {
		t.Fatalf("the incumbent could not claim: %v", err)
	}

	standby := openStandby(t, dsn)

	// A STANDBY WRITES NOTHING WHILE IT WAITS, which is the property the fence
	// cannot give it: checkLeadership exempts a handle that never claimed.
	if err := standby.Tx(t.Context(), func(*sql.Tx) error { return nil }); !errors.Is(
		err, ErrStandby) {
		t.Fatalf("a waiting standby could write; got %v", err)
	}

	promoted := make(chan ControllerClaim, 1)
	failed := make(chan error, 1)

	go func() {
		claim, err := standby.AwaitController(
			promoteCtx(t), replacementHolder, testDeployment, nil)
		if err != nil {
			failed <- err

			return
		}

		promoted <- claim
	}()

	// THE PARTITION. Terminating the backend that holds the claim IS the event
	// rather than a stand-in for it: the lock lives in that session, and the
	// server releases it when the session ends.
	terminateClaimSession(t, incumbent, dsn)

	var claim ControllerClaim

	select {
	case claim = <-promoted:
	case err := <-failed:
		t.Fatalf("the standby did not promote: %v", err)
	case <-time.After(25 * time.Second):
		t.Fatal("the standby never promoted after the incumbent's session was terminated")
	}

	if claim.Epoch != 2 {
		t.Errorf("the promoted standby claimed at epoch %d, want 2", claim.Epoch)
	}

	// AND IT CAN WRITE, which is what promotion is for.
	if err := standby.Tx(t.Context(), func(*sql.Tx) error { return nil }); err != nil {
		t.Errorf("a promoted standby could not write: %v", err)
	}

	// THE INCUMBENT'S WRITER POOL IS STILL PERFECTLY HEALTHY, which is the whole
	// difficulty: nothing about losing a session tells that process anything. The
	// epoch is what refuses it, and the refusal must not be reachable as an
	// ordinary lease error — a listener answers ErrFenced by dropping a lease,
	// and this is a statement about the whole deployment.
	err = incumbent.Tx(t.Context(), func(*sql.Tx) error {
		t.Error("the callback must not run for a controller that has been replaced")

		return nil
	})
	if !errors.Is(err, ErrLeadershipLost) {
		t.Fatalf("the replaced controller's write returned %v, want ErrLeadershipLost", err)
	}

	if !incumbent.LeadershipLost() {
		t.Error("the replaced controller does not report that it lost leadership, so its " +
			"teardown would destroy compute the successor now owns")
	}

	// AND THE SIGNAL FIRED, which is what turns a refused write into a stopped
	// process. Without it a replaced controller goes on polling GitHub and running
	// the cleanup loop that calls Runner.Destroy — which never touches the ledger
	// and is therefore fenced by nothing.
	select {
	case <-incumbent.LeadershipLostSignal():
	default:
		t.Error("the leadership-lost signal did not fire, so nothing would stop the " +
			"replaced control plane")
	}
}

// terminateClaimSession ends the backend holding a handle's controller claim.
//
// THROUGH THE CONNECTION THAT HOLDS THE LOCK, because that pool is
// MaxOpenConns(1) precisely so the lock lives in one session — reading the pid
// out of pg_locks instead would need the lock key, and two tests sharing a server
// could then terminate each other's controller.
func terminateClaimSession(t *testing.T, db *DB, dsn DSN) {
	t.Helper()

	be, ok := db.backend.(*postgresBackend)
	if !ok {
		t.Fatalf("a PostgreSQL ledger has a %T backend", db.backend)
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
}
