package state

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// backend is the part of the ledger that differs between engines, and nothing
// else.
//
// WHAT IS NOT HERE IS THE POINT. The process lock, the single writer, the busy
// retry and its asymmetric patience, the maintenance fence, the schema
// re-verification inside every transaction, and the migration runner itself all
// stay above this interface, because every one of them is an invariant about
// billet rather than about storage. An engine that could reimplement them is an
// engine that could get them wrong.
//
// WHAT IS HERE IS EVERYTHING A SECOND ENGINE CANNOT SHARE: how a connection is
// opened and proved durable, how the migration bookkeeping table is created and
// inspected, how a catalogue is read, how contention and cancellation are
// recognised, how a snapshot is taken, and what a write transaction must do
// before it does anything else.
//
// IT TAKES *sql.DB AND *sql.Tx RATHER THAN OWNING THEM. Both engines speak
// database/sql, so the handle types are shared and only the statements differ —
// which keeps the generated query set, the read/write split and the transaction
// boundaries identical on both.
type backend interface {
	// engine names this backend in a diagnostic. It is also the name that
	// appears in configuration.
	engine() string

	// driverName is the database/sql driver to open with.
	driverName() string

	// timeline is the migration set this engine applies. The two timelines
	// declare the same versions and names and their own checksums.
	timeline() *timeline

	// dataSources builds the connection strings for the two pools.
	//
	// TWO, NOT ONE, because the difference between them is a safety property
	// rather than a tuning knob: the reader must be unable to write. On SQLite
	// that is query_only; on PostgreSQL it is a read-only default transaction.
	dataSources() (ledgerPools, error)

	// prepare learns whatever the backend needs from the open connection, once,
	// before anything else uses it.
	//
	// SEPARATE FROM verifyDurability rather than folded into it, because they ask
	// opposite questions: that one proves the connection is what was asked for
	// and changes nothing, this one reads a fact off the server and keeps it. A
	// method named "verify" that also assigns is a method whose failures nobody
	// looks for in the right place.
	prepare(ctx context.Context, w *sql.DB) error

	// verifyDurability proves the settings that were asked for actually took
	// effect. On SQLite that is the WAL/synchronous/foreign-keys readback that
	// catches a state directory on a network filesystem.
	verifyDurability(ctx context.Context, w *sql.DB) error

	// integrityCheck refuses to serve from a corrupt ledger.
	integrityCheck(ctx context.Context, w *sql.DB) error

	// bootstrapSchema is the migration bookkeeping DDL, executed idempotently
	// outside the versioned set.
	bootstrapSchema() string

	// checkBookkeepingSchema names the columns schema_migrations must have, so a
	// database left by an older development build is refused with advice rather
	// than with "no such column".
	checkBookkeepingSchema(ctx context.Context, q Querier) error

	// bookkeepingTableExists answers whether anything has created the schema at
	// all. Separate from the check above because an empty state directory is not
	// a corrupt one, and telling an operator to delete a directory that is merely
	// new would destroy a running deployment's ledger.
	bookkeepingTableExists(ctx context.Context, q Querier) (bool, error)

	// userTables lists the ledger's own tables, excluding whatever the engine
	// reserves for itself.
	userTables(ctx context.Context, q Querier) ([]string, error)

	// countRows counts one table, named at run time. The TABLE is the variable,
	// which is why no generated query can express it.
	countRows(ctx context.Context, q Querier, table string) (int64, error)

	// isContention reports whether an error is the engine saying somebody else is
	// writing right now.
	//
	// CONTENTION IS A RACE, NOT A VERDICT, and this is the one classification
	// that must never be generous. An error folded in here that is not
	// contention becomes an infinite retry in the control plane; a real
	// contention left out becomes an escrow failure, which stops the listener,
	// which destroys every job on the host.
	isContention(err error) bool

	// isCancellation reports whether an error is the engine's own spelling of a
	// cancelled operation, so a caller testing for context.Canceled can see it.
	// Deliberately narrow: every other failure is a fact about the database that
	// must survive being reported.
	isCancellation(err error) bool

	// beginWrite runs inside a freshly begun write transaction, before anything
	// else, and is what makes that transaction the only writer.
	//
	// SQLite has already done it by the time this is called — BEGIN IMMEDIATE
	// takes the write lock at BEGIN — so its implementation is empty. PostgreSQL
	// has no such thing, and an isolation level plus a retry is the wrong answer
	// there, because retrying re-executes a caller's closure. See the PostgreSQL
	// backend for what it does instead.
	beginWrite(ctx context.Context, tx *sql.Tx) error

	// snapshotInto writes a consistent copy of the ledger to a local path, or
	// refuses when the engine has no such operation billet should own.
	snapshotInto(ctx context.Context, db *DB, path string) error

	// sharedLedger reports whether these rows are reachable from a machine other
	// than this one.
	//
	// IT DECIDES WHAT THE DIRECTORY LOCK PROVES, which is the whole of why it is
	// here. On SQLite the ledger is a file on local storage, so holding its
	// directory excludes every controller that could open it and claimController
	// takes nothing beyond that — releasing the exclusion would release nothing,
	// and paying for a per-transaction schema re-check afterwards would be a cost
	// with no fact behind it. On PostgreSQL the exclusion is a session advisory
	// lock held separately from any directory, so an operator command that took it
	// only to create the schema has something real to give back.
	sharedLedger() bool

	// claimController takes the exclusion that makes this process the
	// deployment's controller, or returns ErrControllerHeld.
	//
	// IDEMPOTENT. openDir takes it before migrating and DB.ClaimController takes
	// it again before recording the epoch, because the two answer different
	// halves — who may change the schema, and which generation of controller this
	// is. A second acquisition from a second connection would be a second SESSION
	// asking for a lock this process already holds, which the server correctly
	// refuses, so a backend that holds one already answers yes rather than
	// competing with itself.
	//
	// SESSION-SCOPED, NOT LEASED. Whatever holds it must be released by the
	// process dying, not by a clock: a lease needs a renewal, a renewal needs a
	// timeout, and a timeout is a number that decides whether a live controller
	// is declared dead. SQLite already has one in the directory lock; PostgreSQL
	// has one in a session advisory lock the server drops when the connection
	// goes.
	claimController(ctx context.Context, db *DB) error

	// releaseController gives the exclusion back. Called when the claim could
	// not be recorded, and when the store closes.
	releaseController() error
}

// ledgerPools is the two connection strings a backend opens.
//
// A STRUCT RATHER THAN A PAIR OF STRINGS, so the two cannot be swapped at a call
// site. Handing the reader's DSN to the writer would produce a control plane
// that starts, looks healthy, and refuses every scheduling write.
type ledgerPools struct {
	writer string
	reader string
}

// bookkeepingColumns is what schema_migrations must have, whatever engine holds
// it.
//
// SHARED, because it is a fact about billet's migrator rather than about
// storage: these four columns are what readAppliedMigrations selects and what
// the migrator records. Two backends asking two different questions here would
// be two definitions of what a recorded migration IS.
var bookkeepingColumns = []string{"version", "name", "checksum", "applied_at"}

// requireBookkeepingColumns turns a set of column names into the diagnostic.
//
// billet has had no released version, so the only way to reach this is a
// database left by a development build; the remedy is to delete it rather than
// to write a migration for the migration table. Said explicitly because the
// alternative is a bare "no such column" from the next SELECT, which gives an
// operator nothing to act on.
func requireBookkeepingColumns(found map[string]bool) error {
	var missing []string

	for _, col := range bookkeepingColumns {
		if !found[col] {
			missing = append(missing, col)
		}
	}

	if len(missing) > 0 {
		return fmt.Errorf(
			"state database has an incompatible schema_migrations table (missing %s); "+
				"it was created by an older development build of billet — delete the state "+
				"directory and start again",
			strings.Join(missing, ", "))
	}

	return nil
}
