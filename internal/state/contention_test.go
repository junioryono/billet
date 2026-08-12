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

	err = server.Tx(ctx, func(tx *sql.Tx) error {
		// THE READ COMES FIRST, which is what took the snapshot before the fix.
		var open int

		if err := tx.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM leases WHERE phase NOT IN ('done','failed')`).Scan(&open); err != nil {
			return fmt.Errorf("the server reads headroom: %w", err)
		}

		// An operator command tries to commit in the window between the server's
		// read and its write. BOUNDED, so that when it correctly queues behind the
		// server it gives up in milliseconds rather than holding the suite for the
		// full busy_timeout.
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
