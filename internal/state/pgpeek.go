package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// PostgresLedger is what a read-only look at a PostgreSQL ledger can say about
// it, for a restore deciding whether the database may be written.
//
// THE COUNTERPART OF LedgerContents, with what a database adds: which schema the
// connection resolves to (pg_restore installs into the schema the dump names,
// so the two must agree), which server major answers, what has been applied,
// and which deployment the rows say they belong to.
type PostgresLedger struct {
	// Database is current_database(), the name a recovery journals beside its
	// copy so that a resume or an abandon can refuse a connection that has since
	// been pointed somewhere else.
	Database string
	// Schema is current_schema(), or empty when the connection's search_path
	// names no schema that exists.
	Schema string
	// ServerVersion is server_version_num, e.g. 180006.
	ServerVersion int
	// Tables are every table in Schema. Empty means nothing is there to replace.
	Tables []string
	// Populated is true when any table a deployment writes to holds a row, and
	// NonEmpty names those, as PeekLedger reports them.
	Populated bool
	NonEmpty  []string
	// Migrations is what schema_migrations records, ordered by version.
	Migrations []AppliedMigration
	// Binding is the deployment the rows are bound to, or empty for none.
	Binding string
}

// PeekPostgresLedger reports what a PostgreSQL ledger holds, without a lock, a
// claim, a migration, a fence check or a write.
//
// ON THE INSPECTION POOL, whose every session is read-only, and in one
// REPEATABLE READ READ ONLY transaction, so the answers describe one moment.
// "Could not tell" is an error, never an empty answer: what a restore does with
// an empty answer is write a ledger into the schema.
func PeekPostgresLedger(ctx context.Context, dsn DSN) (PostgresLedger, error) {
	var out PostgresLedger

	err := peekPostgres(ctx, dsn, func(be *postgresBackend, tx *sql.Tx) error {
		//billet:ignore rawsql // asks the server which database, schema and major answer, not a query over billet's tables
		if err := tx.QueryRowContext(ctx,
			`SELECT current_database(), coalesce(current_schema(), ''), current_setting('server_version_num')::int`,
		).Scan(&out.Database, &out.Schema, &out.ServerVersion); err != nil {
			return fmt.Errorf("state: ask the server which schema this ledger is in: %w", err)
		}

		if out.Schema == "" {
			return nil
		}

		tables, err := be.userTables(ctx, tx)
		if err != nil {
			return err
		}

		out.Tables = tables

		contents, err := countLedger(ctx, be, tx, tables)
		if err != nil {
			return err
		}

		out.Populated, out.NonEmpty = contents.Populated, contents.NonEmpty

		if slices.Contains(tables, "schema_migrations") {
			if out.Migrations, err = appliedIn(ctx, tx); err != nil {
				return err
			}
		}

		if slices.Contains(tables, "deployment_binding") {
			row, err := ReadQueries(tx).ReadDeploymentBinding(ctx)

			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("state: read this ledger's deployment binding: %w", err)
			default:
				out.Binding = row.DeploymentID
			}
		}

		return nil
	})

	return out, err
}

// PeekPostgresAdmission reads a PostgreSQL ledger's admission row the way
// PeekAdmission reads a file's: no lock, no migration, no fence.
//
// FOR THE ONE CALLER THAT MUST ASK BEHIND ITS OWN FENCE: a recovery about to
// lift it, which needs positive proof the ledger it put back is sealed.
func PeekPostgresAdmission(ctx context.Context, dsn DSN) (Admission, error) {
	var out Admission

	err := peekPostgres(ctx, dsn, func(_ *postgresBackend, tx *sql.Tx) error {
		a, err := ReadAdmission(ctx, tx)
		out = a

		return err
	})

	return out, err
}

// peekPostgres runs fn in one read-only snapshot on a connection that can
// write nothing.
func peekPostgres(ctx context.Context, dsn DSN, fn func(*postgresBackend, *sql.Tx) error) error {
	be := newPostgresBackend(dsn)

	pools, err := be.inspectDataSources()
	if err != nil {
		return err
	}

	db, err := sql.Open(be.driverName(), pools.reader)
	if err != nil {
		return fmt.Errorf("state: open the PostgreSQL ledger to look at it: %w", err)
	}

	defer func() { _ = db.Close() }()

	db.SetMaxOpenConns(1)

	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
	if err != nil {
		return fmt.Errorf("state: begin a read of the PostgreSQL ledger: %w", err)
	}

	defer func() {
		//nolint:errcheck // a read transaction has nothing to lose on rollback.
		_ = tx.Rollback()
	}()

	return fn(be, tx)
}

// countLedger is PeekLedger's judgement over tables already listed, on either
// engine: every row counts except the bookkeeping and the admission singleton
// its migration inserts.
//
// THE EXACT ROW THAT MIGRATION INSERTS, not merely one row. Counting to one
// exempts a SEALED admission, an advanced generation, or a provenance and reason
// an operator wrote — all of them deployment state, and all of them exempted by
// a count. A drain taken on a host somebody then decided to restore over would
// have read as pristine.
func countLedger(ctx context.Context, be backend, q Querier, tables []string) (LedgerContents, error) {
	var out LedgerContents

	for _, table := range tables {
		n, err := be.countRows(ctx, q, table)
		if err != nil {
			return LedgerContents{}, fmt.Errorf("state: count %s: %w", table, err)
		}

		switch {
		case table == "schema_migrations":
		case table == "admission":
			pristine, err := admissionIsPristine(ctx, q)
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

// appliedIn reads the applied-migration set in order.
func appliedIn(ctx context.Context, q Querier) ([]AppliedMigration, error) {
	seen, err := readAppliedMigrations(ctx, ReadQueries(q))
	if err != nil {
		return nil, err
	}

	out := make([]AppliedMigration, 0, len(seen))
	for v, rec := range seen {
		out = append(out, AppliedMigration{Version: v, Name: rec.name, Checksum: rec.checksum})
	}

	sortMigrations(out)

	return out, nil
}

// LedgerSnapshot is one exported PostgreSQL snapshot and what the ledger held in
// it.
//
// THE MIGRATIONS AND THE BINDING ARE READ IN THE SNAPSHOT THE DUMP IMPORTS, so
// they describe the dump exactly rather than a moment near it. That is the
// property LedgerFacts asks of a SQLite archive (read back from the completed
// snapshot), reached the only way a database allows.
type LedgerSnapshot struct {
	// ID is pg_export_snapshot()'s answer, for pg_dump --snapshot. Importing it
	// fails on any database but this one ("cannot import a snapshot from a
	// different database", measured 2026-10-20), which is what proves the dump
	// is of this ledger rather than of whatever the client program reached.
	ID            string
	Schema        string
	ServerVersion int
	Migrations    []AppliedMigration
	Binding       string
}

// ExportPostgresSnapshot exports a snapshot of a PostgreSQL ledger and calls fn
// while it is held.
//
// ON THE INSPECTION POOL, in a REPEATABLE READ READ ONLY transaction, so it
// holds no writer lock and delays no scheduling write for as long as fn runs:
// MVCC is what makes the copy consistent, where SQLite's VACUUM INTO needs the
// writer slot (measured 2026-10-20: a snapshot exported this way under
// default_transaction_read_only is imported by pg_dump --snapshot, and the dump
// omits a row committed after the export).
//
// fenceDir IS THE DIRECTORY WHOSE FENCE THIS HONOURS, as SnapshotInto honours
// its handle's: a backup must not copy a ledger an upgrade or a restore has
// closed. It is empty for the one caller that holds the fence itself — a
// recovery taking the superseded copy of the ledger it is about to replace.
//
// fn MUST NOT OUTLIVE THIS CALL: the snapshot is gone when the transaction ends.
func ExportPostgresSnapshot(ctx context.Context, dsn DSN, fenceDir string,
	fn func(LedgerSnapshot) error,
) error {
	if fenceDir != "" {
		if err := refuseMaintenance(fenceDir); err != nil {
			return err
		}
	}

	return peekPostgres(ctx, dsn, func(_ *postgresBackend, tx *sql.Tx) error {
		var snap LedgerSnapshot

		//billet:ignore rawsql // exports a snapshot and asks which schema and major answer, not a query over billet's tables
		if err := tx.QueryRowContext(ctx,
			`SELECT pg_export_snapshot(), coalesce(current_schema(), ''), current_setting('server_version_num')::int`,
		).Scan(&snap.ID, &snap.Schema, &snap.ServerVersion); err != nil {
			return fmt.Errorf("state: export a snapshot of the ledger: %w", err)
		}

		if snap.Schema == "" {
			return errors.New("state: the PostgreSQL data source's search_path names no schema that " +
				"exists, so there is no ledger there to copy")
		}

		var err error

		if snap.Migrations, err = appliedIn(ctx, tx); err != nil {
			return err
		}

		row, err := ReadQueries(tx).ReadDeploymentBinding(ctx)

		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return fmt.Errorf("state: read this ledger's deployment binding: %w", err)
		default:
			snap.Binding = row.DeploymentID
		}

		return fn(snap)
	})
}

// PostgresWriterBarrier proves that no billet write transaction against a
// PostgreSQL ledger is in flight, ON ANY HOST.
//
// THE WRITER'S OWN LOCK, taken and given back. Every billet write begins by
// taking pg_advisory_xact_lock on the ledger's writer key (beginWrite), so
// holding it for an instant proves the one that held it before has committed or
// rolled back. That is WriterBarrier's argument on SQLite, and it is stronger
// here: the key is the ledger's, read from the server, so the proof covers every
// process that can reach these rows rather than one machine.
//
// IT IS NOT THE FENCE. It says nothing about a write that starts afterwards; on
// this backend the local fence reaches only this host's handles, and every other
// controller is the operator's to have stopped (--old-controller-fenced).
func PostgresWriterBarrier(ctx context.Context, dsn DSN) error {
	be := newPostgresBackend(dsn)

	pools, err := be.dataSources()
	if err != nil {
		return err
	}

	w, err := sql.Open(be.driverName(), pools.writer)
	if err != nil {
		return fmt.Errorf("state: open the PostgreSQL ledger to take the writer barrier: %w", err)
	}

	defer func() { _ = w.Close() }()

	w.SetMaxOpenConns(1)

	barrierCtx, cancel := context.WithTimeout(ctx, writerBarrierLimit)
	defer cancel()

	if err := be.prepare(barrierCtx, w); err != nil {
		return err
	}

	for {
		err := holdWriterLock(barrierCtx, be, w)
		if err == nil {
			return nil
		}

		if !be.isContention(err) {
			return fmt.Errorf("state: take the writer barrier on the PostgreSQL ledger: %w", err)
		}

		wait := time.NewTimer(busyRetryInterval)

		select {
		case <-barrierCtx.Done():
			wait.Stop()

			return fmt.Errorf("state: a billet write transaction on the PostgreSQL ledger was still "+
				"running %s after the maintenance fence went up, so billet cannot prove the "+
				"ledger is quiet: %w", writerBarrierLimit, barrierCtx.Err())
		case <-wait.C:
		}
	}
}

// holdWriterLock takes the writer's advisory lock in a transaction and rolls it
// back, which is how the lock is given back.
func holdWriterLock(ctx context.Context, be *postgresBackend, w *sql.DB) error {
	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}

	defer func() {
		//nolint:errcheck // nothing was written; the rollback is how the lock is released.
		_ = tx.Rollback()
	}()

	return be.beginWrite(ctx, tx)
}

// DropTablesOutside drops every table in a PostgreSQL ledger's schema that keep
// does not name, in one transaction, and reports what it dropped.
//
// WHAT MAKES A RESTORE A REPLACEMENT RATHER THAN AN OVERLAY. pg_restore --clean
// drops only the objects its dump holds, so restoring a ledger from before a
// migration over one after it leaves that migration's tables, and their rows,
// beside the restored schema_migrations that says the migration never ran: the
// next start re-runs it and fails on a table that already exists, or worse,
// serves a mix of the two ledgers. billet's migrations create tables and their
// indexes and nothing else, so dropping the tables the dump does not name is
// the whole difference.
//
// ONLY A CALLER HOLDING A DURABLE COPY OF WHAT IS THERE, OR PROOF NOTHING IS,
// may call this: a recovery after its superseded dump is journaled, an abandon
// putting that dump back, a restore into a schema the planner proved empty or
// pristine. It is its own transaction, before the restore's, so a failure
// between the two is recovered from that copy.
//
// THE SCHEMA IS CHECKED, not assumed: the connection must resolve to schema,
// or this refuses before dropping anything. lock_timeout bounds the wait for a
// table another session holds, so a stray transaction is a refusal naming the
// wait rather than a command that never returns.
func DropTablesOutside(ctx context.Context, dsn DSN, schema string, keep []string) ([]string, error) {
	be := newPostgresBackend(dsn)

	pools, err := be.dataSources()
	if err != nil {
		return nil, err
	}

	w, err := sql.Open(be.driverName(), pools.writer)
	if err != nil {
		return nil, fmt.Errorf("state: open the PostgreSQL ledger: %w", err)
	}

	defer func() { _ = w.Close() }()

	w.SetMaxOpenConns(1)

	tx, err := w.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("state: begin dropping what a restore replaces: %w", err)
	}

	defer func() {
		//nolint:errcheck // after a commit this is a no-op; on failure nothing was dropped.
		_ = tx.Rollback()
	}()

	//billet:ignore rawsql // a transaction-local server setting, not a query over billet's tables
	if _, err := tx.ExecContext(ctx, `SET LOCAL lock_timeout = '30s'`); err != nil {
		return nil, fmt.Errorf("state: bound the wait for a lock: %w", err)
	}

	var current string

	//billet:ignore rawsql // asks the server which schema answers, not a query over billet's tables
	if err := tx.QueryRowContext(ctx, `SELECT coalesce(current_schema(), '')`).Scan(&current); err != nil {
		return nil, fmt.Errorf("state: ask the server which schema this ledger is in: %w", err)
	}

	if current != schema {
		return nil, fmt.Errorf("state: the connection resolves to schema %q and the restore is "+
			"into %q, so billet drops nothing", current, schema)
	}

	tables, err := be.userTables(ctx, tx)
	if err != nil {
		return nil, err
	}

	var dropped []string

	for _, table := range tables {
		if slices.Contains(keep, table) {
			continue
		}

		//nolint:gosec // G202: an identifier cannot be a bind parameter; it comes from the catalogue, quoted with every " doubled.
		stmt := `DROP TABLE IF EXISTS "` + strings.ReplaceAll(table, `"`, `""`) + `" CASCADE`

		//billet:ignore rawsql // the TABLE is the variable, discovered from the catalogue at run time
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("state: drop table %s, which the restored ledger does not have: %w",
				table, err)
		}

		dropped = append(dropped, table)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("state: commit dropping what a restore replaces: %w", err)
	}

	return dropped, nil
}

// OpenPostgresRecovery opens a PostgreSQL ledger for `billet local recover` to
// seal what it has just put back, across the fence that recovery raised.
//
// THE COUNTERPART OF OpenMaintenance FOR A DATABASE, and it has to be its own
// entry because the PostgreSQL maintenance open is the upgrade probe's, which
// may write nothing. A recovery must write exactly one thing behind its fence —
// the operator's seal on the restored admission row — and nothing else can,
// because every other open honours the fence that keeps a control plane off a
// ledger whose admission is still the archive's.
//
// AN OPERATOR HANDLE OTHERWISE: no controller claim is kept, a schema behind
// this binary is migrated only when no controller holds the exclusion, which is
// what the next `billet server` start would do to it anyway, and nothing is
// recorded in the release watermark.
func OpenPostgresRecovery(ctx context.Context, stateDir string, dsn DSN, opts ...OpenOption) (*DB, error) {
	return openDir(ctx, stateDir, newPostgresBackend(dsn),
		openMode{admin: true, maintenanceProbe: true}.with(opts))
}
