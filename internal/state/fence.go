package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DirectoryLock is an exclusive hold on a state directory, for a caller that is
// not opening the ledger.
//
// EXPORTED FOR RESTORE, which needs the exclusion WITHOUT the rest of what Open
// does: Open migrates, integrity-checks and creates. A restore has to prove no
// control plane holds this directory before it publishes anything into it, and
// it must prove that before deciding whether the directory may be written at
// all.
type DirectoryLock struct{ inner *dirLock }

// LockStateDir takes the same lock a control plane holds, or reports who has it.
//
// SUCCESS PROVES NO CONTROL PLANE HOLDS THIS DIRECTORY. It proves nothing about
// another host, another path, or an operator command that opened through
// OpenAdmin without the lock — which is why a restore needs the maintenance
// fence and a writer barrier beside it, and an explicit fencing assertion from
// the operator for the deployment-wide half. See ErrLocked.
func LockStateDir(stateDir string) (*DirectoryLock, error) {
	inner, err := lockDir(stateDir)
	if err != nil {
		return nil, err
	}

	return &DirectoryLock{inner: inner}, nil
}

// Release drops the lock.
func (l *DirectoryLock) Release() error {
	if l == nil {
		return nil
	}

	return l.inner.release()
}

// MaintenanceFencePath is where the fence lives, so a caller can name it in a
// diagnostic without knowing the filename.
func MaintenanceFencePath(stateDir string) string {
	return filepath.Join(stateDir, maintenanceFile)
}

// directoryLockFile is the exclusive lock's name inside the state directory.
const directoryLockFile = "billet.lock"

// DirectoryLockPath is where LockStateDir puts its lock.
//
// EXPORTED SO NOBODY COPIES THE NAME. A privileged `billet local restore` takes
// this lock as root and thereby CREATES the file inside a directory the service
// account owns, so what hands it back afterwards has to name the same file — and
// a second literal elsewhere is a control plane that cannot take its own lock,
// discovered on the first start after a restore.
func DirectoryLockPath(stateDir string) string {
	return filepath.Join(stateDir, directoryLockFile)
}

// MaintenanceFenceReason reports what a fence says it is for, and whether there
// is one.
//
// A READER, BECAUSE THE ALTERNATIVE IS A HAND-PARSE. WriteMaintenanceFence
// already compares this body exactly — that comparison is what stops one
// operation replacing another's fence — so a caller that needs to know WHOSE
// fence it found would otherwise open the file and trim it themselves, which is
// a second reading of a format this package owns. It answers three states rather
// than two: present with a reason, absent, or unreadable, because "billet could
// not tell" must never become "there is no fence" for a caller about to decide
// whether a directory is safe to act on.
func MaintenanceFenceReason(stateDir string) (string, bool, error) {
	body, err := os.ReadFile(MaintenanceFencePath(stateDir))

	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("state: read the maintenance fence %s: %w",
			MaintenanceFencePath(stateDir), err)
	}

	return strings.TrimSpace(string(body)), true, nil
}

// WriteMaintenanceFence closes this ledger to every handle, including ones that
// are already open.
//
// THE FILE IS THE FENCE, and that is what makes it reach a handle somebody else
// is holding: Tx and View consult it on entry, so an operator command that
// opened through OpenAdmin before this was written finds the fence on its next
// transaction rather than committing into a ledger being replaced underneath it.
// The directory lock cannot do that — OpenAdmin deliberately proceeds without
// it.
//
// IDEMPOTENT ON ITS OWN REASON AND REFUSING ON ANYBODY ELSE'S. A fence already
// standing belongs to whoever wrote it; overwriting it would let a restore
// silently adopt an Ansible host upgrade's fence and then CLEAR it at the end,
// reopening a ledger mid-upgrade.
//
// IT REPORTS WHETHER THIS CALL CREATED THE FENCE, and the caller needs that to
// know what it may undo. An operation that fences a ledger, fails before
// changing anything, and leaves the fence standing has taken a healthy control
// plane offline over an operation that did nothing — so a caller clears only a
// fence it established, and never one that predated it.
//
// ALL OR NOTHING. A write or sync that fails partway would otherwise leave an
// empty or truncated fence, which is worse than either state: it closes the
// ledger and no caller can recognise it as its own to clear. The file is
// removed on any failure after creation — safe by the one argument this package
// accepts for a pathname removal, that the O_EXCL open is what created the name.
func WriteMaintenanceFence(stateDir, reason string) (bool, error) {
	if strings.TrimSpace(reason) == "" {
		return false, errors.New("state: a maintenance fence must say what it is for; whoever " +
			"finds the ledger fenced has only this to go on")
	}

	path := MaintenanceFencePath(stateDir)
	body := strings.TrimSpace(reason) + "\n"

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if !os.IsExist(err) {
			return false, fmt.Errorf("state: write the maintenance fence %s: %w", path, err)
		}

		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return false, fmt.Errorf("state: %s already exists and could not be read, so billet "+
				"cannot tell whose fence it is: %w", path, readErr)
		}

		if string(existing) == body {
			// Already ours — a resumed operation re-establishing its own fence.
			// NOT created by this call, so this call may not clear it either.
			return false, nil
		}

		return false, fmt.Errorf("state: %s already fences this ledger for %q; billet will not "+
			"replace somebody else's fence with %q — whatever established that one is entitled "+
			"to clear it", path, strings.TrimSpace(string(existing)), strings.TrimSpace(reason))
	}

	if err := finishFence(f, stateDir, path, body); err != nil {
		// Created by the O_EXCL above and never completed, so removing it is the
		// one pathname removal this package permits.
		if rmErr := os.Remove(path); rmErr != nil {
			return false, fmt.Errorf("%w; and the incomplete fence could not be removed (%v), so "+
				"this ledger is closed to every billet until %s is deleted by hand",
				err, rmErr, path)
		}

		return false, err
	}

	return true, nil
}

// finishFence writes and flushes the body of a fence this call created.
func finishFence(f *os.File, stateDir, path, body string) error {
	defer func() { _ = f.Close() }()

	if _, err := f.WriteString(body); err != nil {
		return fmt.Errorf("state: write the maintenance fence %s: %w", path, err)
	}

	if err := f.Sync(); err != nil {
		return fmt.Errorf("state: sync the maintenance fence %s: %w", path, err)
	}

	return fsyncDir(stateDir)
}

// ClearMaintenanceFence reopens the ledger, and only for the reason that fenced
// it.
//
// THE REASON IS CHECKED, for the same argument admission provenance makes one
// layer up: clearing a fence somebody else established reopens a ledger in the
// middle of their operation, and the evidence is a write landing during a
// window that was supposed to be closed.
func ClearMaintenanceFence(stateDir, reason string) error {
	path := MaintenanceFencePath(stateDir)

	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}

		return fmt.Errorf("state: read the maintenance fence %s: %w", path, err)
	}

	if strings.TrimSpace(string(existing)) != strings.TrimSpace(reason) {
		return fmt.Errorf("state: %s fences this ledger for %q, not %q; billet will not clear "+
			"somebody else's fence", path, strings.TrimSpace(string(existing)),
			strings.TrimSpace(reason))
	}

	if err := os.Remove(path); err != nil {
		return fmt.Errorf("state: clear the maintenance fence %s: %w", path, err)
	}

	return fsyncDir(stateDir)
}

// writerBarrierLimit bounds how long WriterBarrier waits for the write lock.
//
// A var so a test can shorten it. Longer than adminBusyLimit deliberately: what
// this waits out is a transaction that began BEFORE the fence, and the caller is
// about to replace credential files on the strength of it having finished.
var writerBarrierLimit = 30 * time.Second

// WriterBarrier proves that every write transaction which began before the
// fence has finished.
//
// THE FENCE IS NOT ENOUGH BY ITSELF. It is consulted when a transaction STARTS,
// so a handle that got past that check a moment earlier is still free to commit.
// Taking the write lock is the proof: BEGIN IMMEDIATE acquires it up front, so
// holding it for an instant means nobody else is mid-write.
//
// IT WILL NOT CREATE A LEDGER. A caller asking this about a directory with no
// billet.db must not be handed one — sql.Open would create the file, and the
// next thing a restore does is decide whether a ledger is already there.
func WriterBarrier(ctx context.Context, stateDir string) error {
	path := filepath.Join(stateDir, "billet.db")

	if _, err := os.Lstat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("state: inspect %s: %w", path, err)
	}

	if err := requireRegularFile(path); err != nil {
		return err
	}

	db, err := sql.Open("sqlite", dsnWith(path, map[string]string{"_txlock": "immediate"},
		"busy_timeout(50)", "foreign_keys(ON)"))
	if err != nil {
		return fmt.Errorf("state: open %s to take the writer barrier: %w", path, err)
	}

	db.SetMaxOpenConns(1)

	defer func() { _ = db.Close() }()

	barrierCtx, cancel := context.WithTimeout(ctx, writerBarrierLimit)
	defer cancel()

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := barrierCtx.Err(); err != nil {
				return fmt.Errorf(
					"state: a write transaction on %s was still running %s after the maintenance "+
						"fence went up, so billet cannot prove the ledger is quiet: %w",
					path, writerBarrierLimit, err)
			}
		}

		tx, err := db.BeginTx(barrierCtx, nil)
		if err == nil {
			// NOTHING IS WRITTEN. Having held the lock is the entire result; the
			// rollback is how it is given back.
			//nolint:errcheck // a transaction that did nothing has nothing to lose on rollback.
			_ = tx.Rollback()

			return nil
		}

		if !isBusy(err) {
			return fmt.Errorf("state: take the writer barrier on %s: %w", path, err)
		}

		wait := time.NewTimer(busyRetryInterval)

		select {
		case <-barrierCtx.Done():
			wait.Stop()

			return fmt.Errorf(
				"state: a write transaction on %s was still running %s after the maintenance "+
					"fence went up, so billet cannot prove the ledger is quiet: %w",
				path, writerBarrierLimit, barrierCtx.Err())
		case <-wait.C:
		}
	}
}

// LedgerContents is what a read-only peek can say about a ledger file.
//
// Populated is THREE-VALUED in effect: a caller gets an error when billet could
// not look, a false when every table a deployment writes to is empty, and a true
// with the tables named otherwise. "Could not tell" must never collapse into
// "empty", because what is done with an empty answer is deleting the file.
type LedgerContents struct {
	// Populated is true when any table a deployment writes to holds a row.
	Populated bool
	// NonEmpty names the tables that made it true, for a diagnostic an operator
	// can act on.
	NonEmpty []string
}

// PeekLedger reports whether a ledger file holds anything a deployment wrote.
//
// WHAT IT EXISTS FOR: `billet check` creates billet.db and its schema on a host
// nobody has commissioned yet, so the presence of the FILE cannot be what stops
// a restore. The deployment identity and the CA marker are what prove a
// directory is committed; this is what proves the ledger beside them is the
// preflight's and not somebody's capacity record.
//
// EVERY TABLE, DISCOVERED FROM sqlite_master RATHER THAN LISTED. A hand-written
// list goes stale the next time a migration adds a table, and the direction it
// goes stale in is the dangerous one: a new table full of rows would be invisible
// and the ledger would read as empty.
//
// Two tables are exempt and both are schema rather than content:
// schema_migrations is the bookkeeping every ledger has, and admission is a
// singleton its own migration INSERTS, so a pristine ledger has exactly one row
// in it — which is checked rather than assumed.
// PeekAdmission reads a ledger's admission row without taking the directory
// lock, without migrating and without consulting the fence.
//
// FOR THE ONE CALLER THAT HOLDS THE LOCK ALREADY AND MUST STILL ASK. A restore
// or a recovery finishes with the directory lock in hand and the fence still up,
// and the last thing it has to establish is that the ledger it is about to
// unfence will not admit work. Every ordinary route is closed to it: OpenAdmin
// HONOURS the fence, which is its whole job, and OpenMaintenance crosses the
// fence but takes the directory lock — which a second descriptor in the same
// process is refused. Reaching for OpenMaintenance before taking the lock is
// what this replaces, and that had three costs: it MIGRATED a ledger before the
// caller had established the operation was even its own, it left a window
// between the answer and the lock in which admission could change, and it made
// the check impossible to run at the moment it is acted on.
//
// IT DOES NOT MIGRATE, AND IT DOES NOT VERIFY THE SCHEMA, which is worth saying
// out loud rather than leaving to be assumed: what comes back is whatever this
// build's admission query reads out of that file, and a ledger from a NEWER
// billet can answer it perfectly well. The caller's schema story has to come
// from somewhere else — for `billet local recover` it is the OpenMaintenance
// this runs behind, which migrates the restored ledger before anything asks it
// this question. Do not read a successful answer here as "billet understands
// this ledger".
func PeekAdmission(ctx context.Context, stateDir string) (Admission, error) {
	dbPath := filepath.Join(stateDir, "billet.db")

	if err := requireRegularFile(dbPath); err != nil {
		return Admission{}, err
	}

	db, err := sql.Open("sqlite",
		dsn(dbPath, "busy_timeout(2000)", "query_only(ON)", "foreign_keys(ON)"))
	if err != nil {
		return Admission{}, fmt.Errorf("state: open %s to read its admission: %w", dbPath, err)
	}

	defer func() { _ = db.Close() }()

	return ReadAdmission(ctx, db)
}

func PeekLedger(ctx context.Context, dbPath string) (LedgerContents, error) {
	if err := requireRegularFile(dbPath); err != nil {
		return LedgerContents{}, err
	}

	db, err := sql.Open("sqlite", dsn(dbPath, "busy_timeout(2000)", "query_only(ON)", "foreign_keys(ON)"))
	if err != nil {
		return LedgerContents{}, fmt.Errorf("state: open %s to inspect it: %w", dbPath, err)
	}

	defer func() { _ = db.Close() }()

	tables, err := userTables(ctx, db)
	if err != nil {
		return LedgerContents{}, err
	}

	var out LedgerContents

	for _, table := range tables {
		n, err := countTableRows(ctx, db, table)
		if err != nil {
			return LedgerContents{}, fmt.Errorf("state: count %s in %s: %w", table, dbPath, err)
		}

		switch {
		case table == "schema_migrations":
		case table == "admission":
			// THE EXACT ROW ITS MIGRATION INSERTS, not merely one row. Counting
			// to one exempts a SEALED admission, an advanced generation, or a
			// provenance and reason an operator wrote — all of them deployment
			// state, and all of them exempted by a count. A drain taken on a host
			// somebody then decided to restore over would have read as pristine.
			pristine, err := admissionIsPristine(ctx, db)
			if err != nil {
				return LedgerContents{}, err
			}

			if !pristine {
				out.Populated = true
				out.NonEmpty = append(out.NonEmpty, "admission (changed)")
			}
		case n > 0:
			out.Populated = true
			out.NonEmpty = append(out.NonEmpty, fmt.Sprintf("%s (%d)", table, n))
		}
	}

	return out, nil
}

// countTableRows counts one table, named at run time.
//
// RAW SQL, AND UNAVOIDABLY SO: the TABLE is the variable. sqlc generates a call
// per named statement, and there is no statement to name when the identifier is
// discovered from sqlite_master at run time — which is the whole point, because
// a hand-written list of tables goes stale on the next migration and a table
// this misses is one whose rows are never counted. The identifier is quoted, so
// there is no caller input in it at all.
func countTableRows(ctx context.Context, q Querier, table string) (int64, error) {
	stmt := `SELECT count(*) FROM "` + strings.ReplaceAll(table, `"`, `""`) + `"`

	var n int64

	//billet:ignore rawsql // the TABLE is the variable, discovered from sqlite_master at run time
	if err := q.QueryRowContext(ctx, stmt).Scan(&n); err != nil {
		return 0, err
	}

	return n, nil
}

// admissionIsPristine reports whether the admission table holds exactly the row
// its migration inserted and nothing else.
//
// THE VALUES ARE THE ONES admissionMigration WRITES: one row, id 1, mode open,
// generation 0, and empty provenance, reason, actor and timestamp. Anything
// else — a seal, a resume, a generation that has moved — is a decision somebody
// made about this deployment, and a caller asking "is this ledger untouched" is
// deciding whether to delete it.
func admissionIsPristine(ctx context.Context, q Querier) (bool, error) {
	rows, err := ReadQueries(q).ListAdmissionRows(ctx)
	if err != nil {
		return false, fmt.Errorf("state: read the admission row: %w", err)
	}

	for _, r := range rows {
		if r.ID != 1 || r.Mode != "open" || r.Generation != 0 ||
			r.Provenance != "" || r.Reason != "" || r.Actor != "" || r.ChangedAt != "" {
			return false, nil
		}
	}

	return len(rows) == 1, nil
}

// userTables lists the tables a deployment could have written to.
//
// THE UNDERSCORE IS ESCAPED, AND THAT IS NOT PEDANTRY. In LIKE, `_` matches any
// single character, so `'sqlite_%'` excludes `sqliteX…` as well as SQLite's own
// reserved `sqlite_…` names — and a table this omits is a table PeekLedger never
// counts rows in, which makes a populated ledger read as pristine and lets a
// restore replace a live deployment's capacity record. billet has no table named
// that way today; the predicate is written to mean what it says rather than to be
// right by luck.
func userTables(ctx context.Context, q Querier) ([]string, error) {
	// RAW SQL: sqlc's catalogue is built from billet's migrations, and
	// sqlite_master is not in them — measured, it answers "relation sqlite_master
	// does not exist". See internal/state/queries/README.md.
	//billet:ignore rawsql // sqlite_master is not in sqlc's catalogue (measured: relation "sqlite_master" does not exist)
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table'
		    AND name NOT LIKE 'sqlite\_%' ESCAPE '\'
		  ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("state: list the ledger's tables: %w", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("state: scan the ledger's tables: %w", err)
		}

		out = append(out, name)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list the ledger's tables: %w", err)
	}

	return out, nil
}
