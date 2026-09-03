package state

import (
	"context"
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
// control plane: it makes no scheduling decisions and holds nothing open, and
// the writes it does make are ordinary transactions SQLite serialises against
// the server's own. Some commands commit more than one — `nodes revoke` records
// each older serial before withdrawing them — which is why the give-up
// diagnostic says what already stands rather than claiming a no-op.
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
	return openDir(ctx, stateDir, newSQLiteBackend(stateDir), openMode{admin: true})
}

// verifySchema proves the ledger is already at the schema this binary expects,
// for a caller that is not allowed to migrate it.
//
// READ-ONLY, through the reader pool: it must not take a write transaction on a
// database a control plane is actively writing to, and query_only makes that
// structural rather than a promise.
func (db *DB) verifySchema(ctx context.Context) error {
	return verifySchemaIn(ctx, db.backend, db.Reader())
}

// verifySchemaNotAhead is the weaker half of verifySchemaIn, for a STANDBY.
//
// TWO DIFFERENT QUESTIONS, AND ONLY ONE OF THEM IS ABOUT THIS PROCESS. "The
// ledger carries a version I have never heard of" says this binary could not
// serve this deployment at all, so a standby that met it at PROMOTION would
// discover it at the worst possible moment — the failover — and there is nothing
// it could do then. "The ledger has not applied a migration I carry" says only
// that the leader is older, which is the ordinary state of a follower-first
// upgrade and the reason a newer standby is staged in the first place.
//
// So this refuses the first and tolerates the second, and it tolerates an absent
// schema entirely: a standby may legitimately be started before the deployment's
// first controller has ever run.
//
// THE CHECKSUM CHECK IS KEPT, because an edited migration means the two
// processes disagree about what the schema IS, and that is not a version
// question at all.
func verifySchemaNotAhead(ctx context.Context, be backend, q Querier) error {
	exists, err := be.bookkeepingTableExists(ctx, q)
	if err != nil || !exists {
		return err
	}

	if err := be.checkBookkeepingSchema(ctx, q); err != nil {
		return err
	}

	seen, err := readAppliedMigrations(ctx, ReadQueries(q))
	if err != nil {
		return err
	}

	if err := be.timeline().refuseUnknownVersions(seen); err != nil {
		return err
	}

	for _, m := range be.timeline().migrations {
		rec, ok := seen[m.Version]
		if !ok {
			continue
		}

		if rec.checksum != m.checksum() {
			return fmt.Errorf(
				"migration %d (%s) was applied with different SQL than this binary contains; "+
					"migrations are append-only and must never be edited",
				m.Version, m.Name)
		}
	}

	return nil
}

// verifySchemaIn is the check itself, against whatever is asking.
//
// TAKEN AGAINST A TRANSACTION AS WELL AS THE READER, because the open-time check
// alone is a time-of-check-to-time-of-use: an admin handle verifies against the
// control plane it found, that plane exits, a NEWER one acquires the lock and
// migrates, and the still-running command then writes against a schema it never
// checked — defeating refuseUnknownVersions during exactly the restart it exists
// to protect. Re-running it inside each transaction closes that, and it is sound
// precisely because the writer begins IMMEDIATE: holding the write lock is what
// makes a migration unable to interleave between the check and the work.
func verifySchemaIn(ctx context.Context, be backend, q Querier) error {
	// THE BOOKKEEPING TABLE MAY NOT EXIST AT ALL. An empty state directory that
	// something else has locked has no schema yet, and checkBookkeepingSchema
	// would report that as a corrupt table and tell the operator to delete the
	// directory — advice that would destroy a running deployment's ledger.
	exists, err := be.bookkeepingTableExists(ctx, q)
	if err != nil {
		return err
	}

	if !exists {
		// NOT "your control plane is older". Nothing has created the schema yet,
		// and the likeliest reason is that another billet is holding this ledger
		// and is still initialising it — two operator commands on a fresh install
		// race exactly here. Telling someone to restart a control plane that does
		// not exist would send them looking for the wrong thing.
		//
		// "THIS LEDGER" RATHER THAN "THIS DIRECTORY", because the two stopped being
		// the same thing once a ledger could live in a database. On a shared one
		// this is reached by a command whose own directory nothing else holds — it
		// is the CONTROLLER EXCLUSION that is held, on another machine — and a
		// sentence naming the directory would send an operator to look at a lock
		// file that is not the reason.
		return fmt.Errorf(
			"%w: nothing has created it yet, and another billet process is holding this ledger — "+
				"it is most likely still initialising. Try again in a moment",
			ErrSchemaBehind)
	}

	if err := be.checkBookkeepingSchema(ctx, q); err != nil {
		return err
	}

	// ReadQueries, not the transaction itself: verifying a schema is a read even
	// when it happens inside a write transaction, and binding it to the read-only
	// adapter means this path cannot write whatever it is handed.
	seen, err := readAppliedMigrations(ctx, ReadQueries(q))
	if err != nil {
		return err
	}

	// The SAME rule migrate applies, from the same function: a database carrying
	// a version this binary has never heard of was written by a newer billet.
	if err := be.timeline().refuseUnknownVersions(seen); err != nil {
		return err
	}

	for _, m := range be.timeline().migrations {
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
