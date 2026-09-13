package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// UNREACHABILITY IS A PROPERTY OF THE ERROR, NOT OF THE STEP THAT PRODUCED IT.
// A caller that waits out an outage rather than refusing needs to know the
// database could not be reached; a ping that failed because the file is not a
// database is the database answering, and reading that as unreachability would
// have such a caller wait out a corrupt ledger for ever.
func TestUnreachableIsAskedOfTheErrorAndNotOfTheStep(t *testing.T) {
	// A LEDGER THAT IS NOT ONE: the ping fails, and it fails with positive
	// evidence about the file.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "billet.db"), []byte("this is not a database"), 0o600); err != nil {
		t.Fatalf("write the ledger: %v", err)
	}

	_, err := OpenAdmin(t.Context(), dir)
	if err == nil {
		t.Fatal("a file that is not a database opened")
	}

	if Unreachable(err) {
		t.Fatalf("a corrupt ledger reads as unreachable: %v", err)
	}

	// A POSTGRESQL DSN NOTHING LISTENS ON: the connection cannot be
	// established, which is what the word means.
	_, err = OpenPostgresCompletion(t.Context(), t.TempDir(),
		"postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable&connect_timeout=2")
	if err == nil {
		t.Fatal("a port nothing listens on opened")
	}

	if !Unreachable(err) {
		t.Fatalf("a refused connection does not read as unreachable: %v", err)
	}

	if Unreachable(nil) {
		t.Fatal("no error reads as unreachable")
	}

	// AND A REFUSAL THE LEDGER ITSELF COMPOSED IS NOT ONE.
	if Unreachable(ErrSchemaAhead) || Unreachable(ErrForeignLedger) {
		t.Fatal("a refusal the ledger answered with reads as unreachable")
	}
}

// EVERY ROW OF THE CLASSIFIER IS A MEASUREMENT, and this is where it is taken:
// pgx's shapes are not documented as a contract, and the two that matter most
// are the ones reading alone would get wrong — a wrong password and a database
// that does not exist arrive INSIDE a ConnectError, and a context deadline
// satisfies net.Error. A pgx upgrade that changes any of it fails here rather
// than silently turning a credential an operator must fix into a retirement
// that waits for ever.
func TestUnreachableIsMeasuredAgainstPgxsOwnShapes(t *testing.T) {
	base := requirePostgresSchema(t, "unreachable_shapes")

	cases := []struct {
		name string
		want bool
		run  func(t *testing.T) error
	}{{
		name: "a port nothing listens on", want: true,
		run: func(t *testing.T) error {
			t.Helper()

			_, err := OpenPostgresCompletion(t.Context(), t.TempDir(),
				"postgres://billet:billet@127.0.0.1:1/billet?sslmode=disable&connect_timeout=2")

			return err
		},
	}, {
		// INSIDE a ConnectError, which is why the server's own error is asked
		// about first.
		name: "a password the server rejects", want: false,
		run: func(t *testing.T) error {
			t.Helper()

			_, err := OpenPostgresCompletion(t.Context(), t.TempDir(), strings.Replace(base, ":billet@", ":wrong@", 1))

			return err
		},
	}, {
		name: "a database that does not exist", want: false,
		run: func(t *testing.T) error {
			t.Helper()

			_, err := OpenPostgresCompletion(t.Context(), t.TempDir(),
				strings.Replace(base, "/billet?", "/nosuchdb?", 1))

			return err
		},
	}, {
		name: "a relation that does not exist", want: false,
		run: func(t *testing.T) error {
			t.Helper()

			conn := openProbeConn(t, base)

			//billet:ignore rawsql // the measurement needs a statement the server refuses
			_, err := conn.ExecContext(t.Context(), `SELECT * FROM no_such_table`)

			return err
		},
	}, {
		// A SLOW QUERY IS A DATABASE ANSWERING, and the deadline is the
		// caller's own bound: pgx wraps it in errTimeout, which satisfies
		// net.Error.
		name: "a statement the caller's deadline cut off", want: false,
		run: func(t *testing.T) error {
			t.Helper()

			conn := openProbeConn(t, base)

			ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
			defer cancel()

			//billet:ignore rawsql // the measurement needs a statement that outlasts a deadline
			_, err := conn.ExecContext(ctx, `SELECT pg_sleep(3)`)

			return err
		},
	}, {
		// SQLSTATE 57P01: the server saying it is going away, which is an
		// availability answer and not a refusal to act on.
		name: "a backend the server terminated", want: true,
		run: func(t *testing.T) error {
			t.Helper()

			victim, other := openProbeConn(t, base), openProbeConn(t, base)
			victim.SetMaxOpenConns(1)

			var pid int

			//billet:ignore rawsql // the measurement needs the session's own pid
			if err := victim.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatalf("read the session's pid: %v", err)
			}

			go func() {
				time.Sleep(300 * time.Millisecond)

				//billet:ignore rawsql // the measurement terminates the session under its query
				//nolint:errcheck // the termination is best-effort: what is measured is what the victim's query answers.
				_, _ = other.ExecContext(context.WithoutCancel(t.Context()), `SELECT pg_terminate_backend($1)`, pid)
			}()

			//billet:ignore rawsql // the statement the termination lands under
			_, err := victim.ExecContext(t.Context(), `SELECT pg_sleep(3)`)

			return err
		},
	}}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.run(t)
			if err == nil {
				t.Fatal("the case produced no error, so it measured nothing")
			}

			if got := Unreachable(err); got != c.want {
				t.Fatalf("Unreachable is %v, want %v, for: %v", got, c.want, err)
			}
		})
	}
}

// openProbeConn is a connection through the same stack billet uses, closed
// with the test.
func openProbeConn(t *testing.T, dsn string) *sql.DB {
	t.Helper()

	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open a connection: %v", err)
	}

	t.Cleanup(func() { _ = conn.Close() })

	return conn
}

// A COMPLETION WRITES ONE ROW AND CLAIMS NOTHING. Its caller is a host whose
// controller has been retired: an admin open would take the deployment's
// controller exclusion and migrate the shared schema whenever the survivor
// happened to be down, which is the one moment a retired host must not become
// this deployment's control plane.
func TestOpenPostgresCompletionClaimsNothingAndMigratesNothing(t *testing.T) {
	dsn := requirePostgres(t)

	// A ledger a control plane has already migrated and left.
	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if _, err := plane.ClaimController(t.Context(), "control-a", "dddddddddddddddddddddddddddddddd"); err != nil {
		t.Fatalf("ClaimController: %v", err)
	}

	if err := plane.Close(); err != nil {
		t.Fatalf("close the plane: %v", err)
	}

	// The retired host's own directory is the archive: nothing else locks it,
	// so the controller exclusion is free for the taking — and is not taken.
	var db *DB

	ops := sideEffects(t, func() {
		db, err = OpenPostgresCompletion(t.Context(), t.TempDir(), dsn)
	})
	if err != nil {
		t.Fatalf("OpenPostgresCompletion: %v", err)
	}

	defer func() { _ = db.Close() }()

	if slices.Contains(ops, "claim") || slices.Contains(ops, "migrate") || slices.Contains(ops, "watermark") {
		t.Fatalf("a completion claimed or migrated: %v", ops)
	}

	// AND IT CAN WRITE, which is the whole reason it is not an inspection.
	if _, _, err := db.CompleteRetirement(t.Context(), RetirementCompletion{
		Deployment: "dddddddddddddddddddddddddddddddd", Retiring: "control-a", Survivor: "control-b",
		TransitionID: "0123456789abcdef0123456789abcdef", ReservedAt: "2026-09-11T10:00:00Z",
		CompletedBy: "control-a", At: time.Now(),
	}); err != nil {
		t.Fatalf("complete the row through a completion handle: %v", err)
	}
}

// AND IT REFUSES A SCHEMA THAT IS NOT EXACTLY THIS BINARY'S rather than
// migrating it, which is what lets the caller hand the write to the survivor:
// a ledger the survivor has already migrated past is ErrSchemaAhead, and one
// behind this binary is ErrSchemaBehind — neither is this host's to move.
func TestOpenPostgresCompletionRefusesASchemaItWouldHaveToMove(t *testing.T) {
	dsn := requirePostgresSchema(t, "completion_behind")

	plane, err := OpenPostgres(t.Context(), t.TempDir(), dsn)
	if err != nil {
		t.Fatalf("OpenPostgres: %v", err)
	}

	if err := plane.Close(); err != nil {
		t.Fatalf("close the plane: %v", err)
	}

	// ONE APPLIED VERSION REMOVED FROM THE RECORD is a ledger behind this
	// binary without touching a table: what the open compares is the recorded
	// set against its own.
	conn, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open a connection: %v", err)
	}

	//billet:ignore rawsql // the fixture rewinds the bookkeeping table this open reads
	if _, err := conn.ExecContext(t.Context(),
		`DELETE FROM schema_migrations WHERE version = (SELECT max(version) FROM schema_migrations)`); err != nil {
		t.Fatalf("rewind the recorded schema: %v", err)
	}

	if err := conn.Close(); err != nil {
		t.Fatalf("close the connection: %v", err)
	}

	_, err = OpenPostgresCompletion(t.Context(), t.TempDir(), dsn)
	if !errors.Is(err, ErrSchemaBehind) {
		t.Fatalf("a completion over a ledger behind this binary: %v", err)
	}
}
