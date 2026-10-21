package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"
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
	ID string
	// Database is current_database() in the snapshot, the one the dump is of.
	Database      string
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

	return peekPostgres(ctx, dsn, func(be *postgresBackend, tx *sql.Tx) error {
		var snap LedgerSnapshot

		//billet:ignore rawsql // exports a snapshot and asks which database, schema and major answer, not a query over billet's tables
		if err := tx.QueryRowContext(ctx,
			`SELECT pg_export_snapshot(), current_database(), coalesce(current_schema(), ''), `+
				`current_setting('server_version_num')::int`,
		).Scan(&snap.ID, &snap.Database, &snap.Schema, &snap.ServerVersion); err != nil {
			return fmt.Errorf("state: export a snapshot of the ledger: %w", err)
		}

		if snap.Schema == "" {
			return errors.New("state: the PostgreSQL data source's search_path names no schema that " +
				"exists, so there is no ledger there to copy")
		}

		// A DUMP A RESTORE COULD NOT READ IS REFUSED HERE, not discovered there:
		// pg_restore's list quotes nothing, so a table whose name has whitespace
		// in it makes the dump's tables ambiguous to every restore of it.
		tables, err := be.userTables(ctx, tx)
		if err != nil {
			return err
		}

		if err := refuseSpacedTables(snap.Schema, tables); err != nil {
			return err
		}

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

// RestoreInto is where a restore writes, and what the ledger there must be when
// the write begins: the database and schema the caller proved, and the
// deployment its rows are bound to (empty for none, or no ledger at all).
//
// CHECKED ON THE CONNECTION THAT WRITES, inside its transaction, because a
// proof read on another connection is a proof about wherever that one went.
type RestoreInto struct {
	Database string
	Schema   string
	Binding  string
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
// THE TARGET IS CHECKED, not assumed: inside the transaction that drops, the
// connection must reach into's database and schema and find the ledger there
// bound to into's deployment, or this refuses before dropping anything. lock_timeout bounds the wait for a
// table another session holds, so a stray transaction is a refusal naming the
// wait rather than a command that never returns.
//
// NOTHING OUTSIDE THE DROPPED TABLES GOES WITH THEM. The copies a caller holds
// are dumps of this schema alone, so a view, a foreign key or an inheriting
// table elsewhere that a CASCADE took could never be put back. The drop is one
// RESTRICT statement over every table at once (dependencies among them are
// allowed, a dependent anywhere else refuses the whole transaction), and a
// table in a hierarchy is refused outright: a partitioned table's partitions go
// with it whatever the drop says, and a partition or an inheriting child takes
// its rows out of a parent that may live anywhere.
func DropTablesOutside(ctx context.Context, dsn DSN, into RestoreInto, keep []string) ([]string, error) {
	schema := into.Schema

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

	var database, current string

	//billet:ignore rawsql // asks the server which database and schema answer, not a query over billet's tables
	if err := tx.QueryRowContext(ctx, `SELECT current_database(), coalesce(current_schema(), '')`).
		Scan(&database, &current); err != nil {
		return nil, fmt.Errorf("state: ask the server which schema this ledger is in: %w", err)
	}

	switch {
	case database != into.Database:
		return nil, fmt.Errorf("state: the connection reaches database %q and the restore is into "+
			"%q, so billet drops nothing", database, into.Database)
	case current != schema:
		return nil, fmt.Errorf("state: the connection resolves to schema %q and the restore is "+
			"into %q, so billet drops nothing", current, schema)
	}

	tables, err := be.userTables(ctx, tx)
	if err != nil {
		return nil, err
	}

	binding := ""

	if slices.Contains(tables, "deployment_binding") {
		row, err := ReadQueries(tx).ReadDeploymentBinding(ctx)

		switch {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, fmt.Errorf("state: read the deployment binding of the ledger a restore replaces: %w", err)
		default:
			binding = row.DeploymentID
		}
	}

	if binding != into.Binding {
		return nil, fmt.Errorf("state: the ledger in %s.%s is bound to %q and the restore expected "+
			"%q, so billet drops nothing", database, schema, binding, into.Binding)
	}

	if err := refuseSpacedTables(schema, tables); err != nil {
		return nil, err
	}

	// EVERY TABLE IN THE SCHEMA, the kept ones included: pg_restore --clean
	// drops those itself, so a hierarchy among them reaches outside the schema
	// just the same.
	inHierarchy, err := hierarchyTables(ctx, tx)
	if err != nil {
		return nil, err
	}

	if len(inHierarchy) > 0 {
		return nil, fmt.Errorf("state: %s.%s is partitioned, a partition or an inheriting table, "+
			"which billet's ledger never holds, and replacing it would take rows outside it "+
			"wherever they are; billet drops nothing", schema, inHierarchy[0])
	}

	var dropped []string

	for _, table := range tables {
		if !slices.Contains(keep, table) {
			dropped = append(dropped, table)
		}
	}

	if len(dropped) > 0 {
		quoted := make([]string, 0, len(dropped))

		for _, table := range dropped {
			quoted = append(quoted, quoteIdent(schema)+"."+quoteIdent(table))
		}

		//nolint:gosec // G202: an identifier cannot be a bind parameter; each comes from the catalogue, quoted with every " doubled.
		stmt := `DROP TABLE ` + strings.Join(quoted, ", ") + ` RESTRICT`

		//billet:ignore rawsql // the TABLES are the variable, discovered from the catalogue at run time
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return nil, fmt.Errorf("state: drop %s, which the restored ledger does not have: something "+
				"outside them depends on them, or they could not be dropped, so billet drops nothing: %w",
				strings.Join(dropped, ", "), err)
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("state: commit dropping what a restore replaces: %w", err)
	}

	return dropped, nil
}

// refuseSpacedTables refuses a ledger schema holding a table with whitespace in
// its name, which billet never creates and which pg_restore's list cannot name
// unambiguously (pgdump.tablesOf reads a table's name as one field).
func refuseSpacedTables(schema string, tables []string) error {
	for _, table := range tables {
		if strings.ContainsFunc(table, unicode.IsSpace) {
			return fmt.Errorf("state: schema %s holds a table named %q, and billet's ledger has no "+
				"table with whitespace in its name; a dump of it could not be restored unambiguously, "+
				"so move that table out of the ledger's schema", schema, table)
		}
	}

	return nil
}

// hierarchyTables lists the tables in the connection's schema that are
// partitioned, are partitions, or inherit from another table.
func hierarchyTables(ctx context.Context, tx *sql.Tx) ([]string, error) {
	//billet:ignore rawsql // pg_class and pg_inherits are not in sqlc's catalogue, and this asks which tables sit in a hierarchy
	rows, err := tx.QueryContext(ctx,
		`SELECT c.relname FROM pg_catalog.pg_class c `+
			`JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace `+
			`WHERE n.nspname = current_schema() AND (c.relkind = 'p' OR c.relispartition `+
			`OR EXISTS (SELECT 1 FROM pg_catalog.pg_inherits i WHERE i.inhrelid = c.oid)) `+
			`ORDER BY c.relname`)
	if err != nil {
		return nil, fmt.Errorf("state: list the schema's tables in a hierarchy: %w", err)
	}

	defer rows.Close()

	var out []string

	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("state: scan the schema's tables in a hierarchy: %w", err)
		}

		out = append(out, name)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("state: list the schema's tables in a hierarchy: %w", err)
	}

	return out, nil
}

// quoteIdent quotes a PostgreSQL identifier, doubling every quote in it.
func quoteIdent(name string) string { return `"` + strings.ReplaceAll(name, `"`, `""`) + `"` }

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
