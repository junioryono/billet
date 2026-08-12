package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrSchemaBehind means the ledger needs a migration this process is not allowed
// to apply, because another billet is already using it.
//
// Its own error rather than a string, so an operator command can tell "the
// running control plane is older than this binary" — which is fixed by
// restarting it — from an ordinary failure to open the database.
var ErrSchemaBehind = errors.New("state: the ledger needs migrating and another billet process is using it")

// OpenAdmin opens the ledger for a ONE-SHOT OPERATOR COMMAND.
//
// THE DIRECTORY LOCK EXISTS TO STOP TWO CONTROL PLANES, not to stop two
// processes. Its whole argument is that SQLite's single-writer rule prevents
// simultaneous writes but does not prevent two billets both long-polling GitHub
// and taking turns writing conflicting scheduling decisions. A command that
// approves an enrollment or forces one quarantined lease back is not a second
// control plane: it makes no scheduling decisions, holds nothing open, and
// commits one bounded transaction that SQLite serialises against the server's
// own.
//
// Opening through Open instead meant every such command failed against a live
// deployment — `nodes pending|approve|revoke`, `ca token|issue|revoke|
// revocations`, `leases quarantined|release`, and `check`. The sharpest case was
// `leases release --force`, whose entire purpose is reclaiming capacity a
// quarantine has stranded on a RUNNING deployment: the only documented remedy
// required stopping the thing that was holding the capacity.
//
// It still takes the lock WHEN IT IS FREE, which matters on a fresh control
// plane: an operator runs `billet ca issue` before the server has ever started,
// so whoever gets there first has to create the schema, and two commands racing
// to create it must not both try.
func OpenAdmin(ctx context.Context, stateDir string) (*DB, error) {
	return openDir(ctx, stateDir, true)
}

// verifySchema proves the ledger is already at the schema this binary expects,
// for a caller that is not allowed to migrate it.
//
// READ-ONLY, through the reader pool: it must not take a write transaction on a
// database a control plane is actively writing to, and query_only makes that
// structural rather than a promise.
func (db *DB) verifySchema(ctx context.Context) error {
	// THE BOOKKEEPING TABLE MAY NOT EXIST AT ALL. An empty state directory that
	// something else has locked has no schema yet, and checkBookkeepingSchema
	// would report that as a corrupt table and tell the operator to delete the
	// directory — advice that would destroy a running deployment's ledger.
	var name string

	err := db.Reader().QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).
		Scan(&name)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: it has no schema yet", ErrSchemaBehind)
	case err != nil:
		return fmt.Errorf("inspect the ledger's schema: %w", err)
	}

	if err := checkBookkeepingSchema(ctx, db.Reader()); err != nil {
		return err
	}

	seen, err := readAppliedMigrations(ctx, db.Reader())
	if err != nil {
		return err
	}

	// The SAME rule migrate applies, from the same function: a database carrying
	// a version this binary has never heard of was written by a newer billet.
	if err := refuseUnknownVersions(seen); err != nil {
		return err
	}

	for _, m := range migrations {
		rec, ok := seen[m.Version]
		if !ok {
			return fmt.Errorf(
				"%w: migration %d (%s) has not been applied. The billet holding this directory "+
					"is older than the one you are running — restart the control plane with this "+
					"binary and it will migrate, then run this command again",
				ErrSchemaBehind, m.Version, m.Name)
		}

		// The same guard migrate applies, and it must not be skipped here: an
		// edited migration means the two processes disagree about what the schema
		// IS, which is worse for a writer than for the process that applied it.
		if rec.checksum != m.checksum() {
			return fmt.Errorf(
				"migration %d (%s) was applied with different SQL than this binary contains; "+
					"migrations are append-only and must never be edited",
				m.Version, m.Name)
		}
	}

	return nil
}

// refuseUnknownVersions rejects a ledger written by a newer billet.
//
// ONE IMPLEMENTATION, called by both the migrating and the verifying path.
// Written twice, these drift — and the failure would be that an operator command
// silently tolerates a database its own control plane refuses to start against.
func refuseUnknownVersions(seen map[int]appliedMigration) error {
	known := make(map[int]struct{}, len(migrations))
	for _, m := range migrations {
		known[m.Version] = struct{}{}
	}

	for v := range seen {
		if _, ok := known[v]; !ok {
			return fmt.Errorf(
				"state database has migration %d, which this billet does not know about; "+
					"it was written by a newer version", v)
		}
	}

	return nil
}
