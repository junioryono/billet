package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// A SECOND WRITING PROCESS MUST NEVER MAKE THE CONTROL PLANE THE CASUALTY.
//
// Admitting operator commands to the ledger while the server runs introduces a
// second writer, and in WAL mode database/sql's BeginTx issues a DEFERRED
// transaction: it takes a read snapshot at its first read and asks for the write
// lock later. Every allocation decision here is read-current, decide, record, so
// if anything commits in between, SQLite cannot promote the stale snapshot and
// fails the write with SQLITE_BUSY_SNAPSHOT (517) — which busy_timeout does not
// rescue, because waiting cannot make an old snapshot current.
//
// The consequence is not local. Escrow's error reaches refillEscrow, which stops
// the listener, which cancels every other listener, whose shutdowns destroy every
// job on the host. One badly-timed `billet ca token` would have taken the
// deployment down. Measured before the fix: error 517, exactly here.
//
// WHAT IS ASSERTED IS ASYMMETRIC, AND DELIBERATELY SO. With BEGIN IMMEDIATE the
// server takes the write lock up front, so a concurrent operator write can no
// longer slip in behind its read — it QUEUES instead. That means the operator
// command may fail to get in while a transaction is open, and that is the correct
// trade: a command an operator can re-run is recoverable, and a control plane
// that stops is not. So this asserts only that the SERVER's transaction
// completes, and deliberately does not require the admin write to succeed.
func TestAnOperatorWriteIsNeverTheServersProblem(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if _, err := admin.w.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatalf("disable the operator handle's busy timeout: %v", err)
	}

	err = server.Tx(ctx, func(tx *sql.Tx) error {
		// THE READ COMES FIRST, which is what took the snapshot before the fix.
		var open int

		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM leases WHERE phase NOT IN ('done','failed')`).Scan(&open); err != nil {
			return fmt.Errorf("the server reads headroom: %w", err)
		}

		// An operator command tries to commit in the window between the server's
		// read and its write.
		//
		// A BLOCKED BEGIN DOES NOT OBSERVE CONTEXT CANCELLATION: modernc arms
		// sqlite3_interrupt, but SQLite's busy handler sleeps and retries without
		// consulting it, so this attempt sat for the full five-second busy_timeout
		// and the context bound did nothing. Switching SQLite's own waiting off for
		// this handle makes it report contention at once — the same outcome, minus
		// five seconds of suite.
		attempt, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()

		done := make(chan error, 1)

		go func() {
			done <- admin.Tx(attempt, func(atx *sql.Tx) error {
				_, err := atx.ExecContext(attempt,
					`INSERT INTO join_tokens (token_sha256, uses_remaining, created_at, expires_at)
					 VALUES (?, ?, ?, ?)`,
					"deadbeef", 1,
					time.Now().UTC().Format(time.RFC3339Nano),
					time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))

				return err
			})
		}()

		// Its outcome is READ but not asserted: whether it got in or queued is
		// timing, and neither answer is a defect. Waiting for it is what makes the
		// interleaving deterministic rather than hopeful.
		<-done

		// AND NOW THE SERVER WRITES. Before the fix this failed with 517 whenever
		// the operator command had got in first.
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (name, provider, last_seen_at) VALUES (?, ?, ?)`,
			"epyc-1", "docker", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("the server records its decision: %w", err)
		}

		return nil
	})
	if err != nil {
		t.Fatalf("the server's transaction must survive a concurrent operator write: %v", err)
	}

	// The server's decision is durable, which is the thing the listener would
	// have died over.
	var provider string

	if err := server.Reader().QueryRowContext(ctx,
		`SELECT provider FROM nodes WHERE name = ?`, "epyc-1").Scan(&provider); err != nil {
		t.Fatalf("the server's write should have committed: %v", err)
	}
}

// AND THE ORDINARY CASE STILL WORKS: with no transaction open on the server, an
// operator command writes without waiting on anything.
//
// The companion to the test above, because that one tolerates the admin write
// failing — so on its own it would stay green if operator writes NEVER got in,
// which is the whole defect this branch exists to fix.
func TestAnOperatorWriteSucceedsWhenTheServerIsNotMidTransaction(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if err := admin.Tx(ctx, func(atx *sql.Tx) error {
		_, err := atx.ExecContext(ctx,
			`INSERT INTO join_tokens (token_sha256, uses_remaining, created_at, expires_at)
			 VALUES (?, ?, ?, ?)`,
			"cafebabe", 1,
			time.Now().UTC().Format(time.RFC3339Nano),
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano))

		return err
	}); err != nil {
		t.Fatalf("an operator command must be able to write while the server is idle: %v", err)
	}

	var uses int

	if err := server.Reader().QueryRowContext(ctx,
		`SELECT uses_remaining FROM join_tokens WHERE token_sha256 = ?`, "cafebabe").Scan(&uses); err != nil {
		t.Fatalf("the server should see the operator's write: %v", err)
	}

	if uses != 1 {
		t.Errorf("uses_remaining = %d, want 1", uses)
	}
}

// THE REVERSE ORDERING IS THE ONE THAT KILLS. Both tests above give the server
// the write lock first; this gives it to the OPERATOR.
//
// Taking the lock up front removed the snapshot failure but not the contention:
// if an operator command holds the write lock for longer than busy_timeout, the
// server's own BeginTx fails with plain SQLITE_BUSY — and that error reaches
// refillEscrow, stops the listener, cancels the others, and destroys every job on
// the host. Same catastrophe as the snapshot bug, one ordering along.
//
// Admin work is not demonstrably short: listing scans unbounded tables and
// revocation loops over rows. So contention must be WAITED OUT rather than
// treated as a verdict.
//
// The server's busy_timeout is set to zero here so its first attempt fails
// immediately, which is what puts the retry loop — rather than SQLite's own
// waiting — under test.
func TestTheServerWaitsOutAnOperatorHoldingTheWriteLock(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	// WITHOUT SQLite's OWN WAITING, so what is measured is billet's retry. The
	// writer pool holds one connection, so this pragma sticks to it.
	if _, err := server.w.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatalf("disable the server's busy timeout: %v", err)
	}

	holding := make(chan struct{})
	release := make(chan struct{})
	adminDone := make(chan error, 1)

	go func() {
		adminDone <- admin.Tx(ctx, func(atx *sql.Tx) error {
			if _, err := atx.ExecContext(ctx,
				`INSERT INTO join_tokens (token_sha256, uses_remaining, created_at, expires_at)
				 VALUES (?, ?, ?, ?)`,
				"held", 1,
				time.Now().UTC().Format(time.RFC3339Nano),
				time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)); err != nil {
				return err
			}

			// The lock is now genuinely held, which is what the server has to
			// survive. Holding it until told to let go is what makes the
			// ordering deterministic rather than raced.
			close(holding)
			<-release

			return nil
		})
	}()

	<-holding

	serverDone := make(chan error, 1)

	go func() {
		serverDone <- server.Tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx,
				`INSERT INTO nodes (name, provider, last_seen_at) VALUES (?, ?, ?)`,
				"epyc-1", "docker", time.Now().UTC().Format(time.RFC3339Nano))

			return err
		})
	}()

	// The server must still be trying rather than have failed. Nothing observable
	// says "it is mid-retry", so this asserts the outcome instead: it has not
	// come back with an error while the lock was held.
	settling := time.NewTimer(2 * busyRetryInterval)
	defer settling.Stop()

	select {
	case err := <-serverDone:
		close(release)
		t.Fatalf("the server gave up while an operator held the write lock: %v", err)
	case <-settling.C:
	}

	close(release)

	if err := <-adminDone; err != nil {
		t.Fatalf("the operator's transaction: %v", err)
	}

	if err := <-serverDone; err != nil {
		t.Fatalf("the server must get in once the operator lets go: %v", err)
	}
}

// AN OPERATOR COMMAND GIVES UP RATHER THAN HANGING SILENTLY.
//
// The control plane waits out contention for as long as its context allows,
// because stopping is the catastrophe. A command has a person waiting at a
// terminal, and the same unbounded patience there is a process that prints
// nothing and never returns — so it stops and says to run it again.
func TestAnOperatorWriteGivesUpRatherThanHanging(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	// Shortened so the test measures the BEHAVIOUR rather than the constant.
	restore := adminBusyLimit
	adminBusyLimit = 150 * time.Millisecond

	t.Cleanup(func() { adminBusyLimit = restore })

	if _, err := admin.w.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatalf("disable the operator handle's busy timeout: %v", err)
	}

	var adminErr error

	// The server holds the writer slot throughout, so the operator write can
	// never get in — which is the situation that used to hang.
	if err := server.Tx(ctx, func(*sql.Tx) error {
		adminErr = admin.Tx(ctx, func(atx *sql.Tx) error {
			_, err := atx.ExecContext(ctx, `DELETE FROM join_tokens`)

			return err
		})

		return nil
	}); err != nil {
		t.Fatalf("the server's transaction: %v", err)
	}

	if adminErr == nil {
		t.Fatal("the operator write should not have got in while the server held the lock")
	}

	// THE MESSAGE IS THE POINT: it has to tell an operator that nothing changed
	// and that running it again is the answer.
	if got := adminErr.Error(); !strings.Contains(got, "run it again") {
		t.Errorf("the diagnostic should tell the operator to run the command again, got: %v", got)
	}
}

// A READ MUST NOT QUEUE BEHIND A WRITE IT DOES NOT NEED.
//
// Every write transaction now begins IMMEDIATE, so anything routed through Tx
// takes SQLite's single writer slot — including operations that only read. That
// turned `billet leases quarantined` and `ca revocations` into writers: they
// would wait for the control plane's transaction, and hold the writer slot
// against it while scanning. View exists so they do not.
func TestAReadOnlyOperatorCommandDoesNotWaitForTheWriteLock(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	// No waiting anywhere: if the read needed the write lock it fails rather than
	// quietly succeeding after a pause, which is what makes this assertion mean
	// something.
	if _, err := admin.r.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatalf("disable the operator reader's busy timeout: %v", err)
	}

	// The control plane is mid-decision and holding the writer slot.
	if err := server.Tx(ctx, func(*sql.Tx) error {
		return admin.View(ctx, func(q Querier) error {
			var n int

			return q.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM leases WHERE phase = 'quarantine'`).Scan(&n)
		})
	}); err != nil {
		t.Fatalf("a read-only operator command must not need the write lock: %v", err)
	}
}

// A write that genuinely cannot get in reports contention rather than something
// misleading, so an operator who hits it can simply run the command again.
func TestAQueuedOperatorWriteReportsContention(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()

	server, err := Open(ctx, dir)
	if err != nil {
		t.Fatalf("open the server's ledger: %v", err)
	}

	t.Cleanup(func() { _ = server.Close() })

	admin, err := OpenAdmin(ctx, dir)
	if err != nil {
		t.Fatalf("OpenAdmin: %v", err)
	}

	t.Cleanup(func() { _ = admin.Close() })

	if _, err := admin.w.ExecContext(ctx, `PRAGMA busy_timeout = 0`); err != nil {
		t.Fatalf("disable the operator handle's busy timeout: %v", err)
	}

	var adminErr error

	if err := server.Tx(ctx, func(*sql.Tx) error {
		attempt, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		defer cancel()

		adminErr = admin.Tx(attempt, func(atx *sql.Tx) error {
			_, err := atx.ExecContext(attempt, `DELETE FROM join_tokens`)

			return err
		})

		return nil
	}); err != nil {
		t.Fatalf("the server's transaction: %v", err)
	}

	if adminErr == nil {
		t.Fatal("an operator write during an open server transaction should not have got in; " +
			"BEGIN IMMEDIATE is what stops it slipping in behind the server's read")
	}

	// NOT a schema error and NOT a corruption: it has to read as "try again".
	if !errors.Is(adminErr, context.DeadlineExceeded) &&
		!strings.Contains(adminErr.Error(), "locked") &&
		!strings.Contains(adminErr.Error(), "busy") {
		t.Errorf("a queued operator write should report contention, got: %v", adminErr)
	}
}
