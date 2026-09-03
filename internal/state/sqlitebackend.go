package state

import (
	"context"
	"database/sql"
	_ "embed" // the bookkeeping DDL is read from schema_migrations.sql.
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	// The pure-Go driver keeps billet a single static binary. Named rather than
	// blank because contention has to be recognised by SQLite's own error CODE:
	// "database is locked" is the text for both SQLITE_BUSY and
	// SQLITE_BUSY_SNAPSHOT, and matching prose would turn a retry into a fatal
	// error the first time a driver reworded it. depguard confines this import to
	// this package.
	"modernc.org/sqlite"
)

// sqliteBackend is the ledger on local storage: one file, WAL, one writer.
//
// THE DEFAULT AND THE RECOMMENDED SHAPE for a laptop, one owned server, or the
// small controller instance ADR-001 describes. It is explicitly single
// controller: the exclusive lock on the state directory is what stops a second
// one, and there is no shared-storage story here at all, because SQLite's WAL
// cannot work on a network filesystem.
type sqliteBackend struct {
	// path is the ledger file. It lives inside the state directory, but the
	// backend is told the path rather than deriving it, so a configuration that
	// puts the ledger somewhere else has one place to say so.
	path string
}

// The whole contract, checked at compile time. A backend that has drifted from
// the interface fails here rather than at the one call site that reaches the
// method nobody implemented.
var _ backend = (*sqliteBackend)(nil)

func newSQLiteBackend(stateDir string) *sqliteBackend {
	return sqliteBackendAt(filepath.Join(stateDir, "billet.db"))
}

// sqliteBackendAt names the ledger file directly.
//
// FOR THE COLD READERS, which are handed a path rather than a state directory:
// internal/deployarchive's restore planner inspects a ledger nobody has opened,
// and there is no DB to ask. They are SQLite-only by construction — a file to
// peek at is not a concept PostgreSQL has — and what replaces them for an
// external ledger is the archive's own pairing, not this.
func sqliteBackendAt(path string) *sqliteBackend { return &sqliteBackend{path: path} }

func (*sqliteBackend) engine() string      { return "sqlite" }
func (*sqliteBackend) driverName() string  { return "sqlite" }
func (*sqliteBackend) timeline() *timeline { return sqliteTimeline }

// dataSources builds the two DSNs.
//
// synchronous=FULL, not NORMAL: NORMAL can lose recently committed transactions
// on power loss, and the capacity ledger is exactly the thing that must survive
// an unclean shutdown for restart reconciliation to work.
//
// busy_timeout covers a writer waiting for another writer, which is REACHABLE
// rather than theoretical: an operator command opens this same database while
// the control plane is running. See OpenAdmin.
//
// _txlock=immediate IS LOAD-BEARING, and it is the reason that is safe.
//
// database/sql's BeginTx issues a plain BEGIN, which in WAL mode is DEFERRED:
// the transaction takes a read snapshot at its first read and only asks for the
// write lock later. Every allocation decision here is read-current, decide,
// record — so if anything commits in between, SQLite cannot promote the
// now-stale snapshot and fails the write with SQLITE_BUSY_SNAPSHOT (517).
// busy_timeout does NOT rescue that: waiting cannot make an old snapshot
// current, so it fails immediately however long the caller is willing to wait.
//
// The blast radius is what makes this a correctness rule rather than a tuning
// one. Escrow's error reaches refillEscrow, which stops the listener, which
// cancels every other listener, whose shutdowns destroy every job on the host.
// One badly-timed `billet ca token` would have taken the deployment down.
//
// BEGIN IMMEDIATE takes the write lock up front, so there is no snapshot to
// promote and a second writer simply queues on busy_timeout. It costs nothing
// here because there is one writer connection per process already — and it is
// also why this backend's beginWrite has nothing left to do.
func (b *sqliteBackend) dataSources() (ledgerPools, error) {
	writer := dsnWith(b.path, map[string]string{"_txlock": "immediate"},
		"journal_mode(WAL)",
		"synchronous(FULL)",
		// SHORT ON PURPOSE, because beginWrite owns the waiting now. A five-second
		// timeout inside SQLite made both bounds a fiction: an attempt begun just
		// under adminBusyLimit could block for another five seconds past it, and a
		// blocked BEGIN does not observe context cancellation — modernc arms
		// sqlite3_interrupt but SQLite's busy handler sleeps without consulting it,
		// so a cancelled caller still waited out the full timeout. Returning
		// quickly and retrying in Go makes both the deadline and the context real.
		"busy_timeout(50)",
		"foreign_keys(ON)",
	)

	// The reader stays read-write at the OS level on purpose. mode=ro cannot open
	// a WAL database unless it can also map the -shm file, so forcing it trades
	// one safety property for a startup failure. query_only plus the narrow
	// Querier interface is what keeps reads read-only.
	reader := dsn(b.path,
		"busy_timeout(5000)",
		"foreign_keys(ON)",
		"query_only(ON)",
	)

	return ledgerPools{writer: writer, reader: reader}, nil
}

// dsn builds a file: URI with the given pragmas, escaping the path so a state
// directory containing spaces, '?' or '#' does not silently truncate the DSN.
func dsn(path string, pragmas ...string) string {
	return dsnWith(path, nil, pragmas...)
}

// dsnWith is dsn plus driver parameters that are not pragmas — _txlock, which
// decides whether BeginTx issues BEGIN or BEGIN IMMEDIATE, is one.
func dsnWith(path string, extra map[string]string, pragmas ...string) string {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}

	for k, v := range extra {
		q.Set(k, v)
	}

	u.RawQuery = q.Encode()

	return u.String()
}

// verifyDurability reads the settings back from the writer connection. This is
// not paranoia: `PRAGMA journal_mode=WAL` reports the mode actually in effect,
// and on a network filesystem — where WAL is unsupported because its
// shared-memory index assumes one host — the request silently degrades to
// DELETE. Failing closed here turns "your ledger was never durable" into a
// startup error.
func (*sqliteBackend) verifyDurability(ctx context.Context, w *sql.DB) error {
	var errs []error

	var journal string
	// A PRAGMA IS NOT A QUERY: sqlc has no catalogue entry for one, so there is
	// nothing for it to generate.
	//billet:ignore rawsql // a pragma is not a query; sqlc has no catalogue entry for one
	if err := w.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		return fmt.Errorf("read journal_mode: %w", err)
	}

	if !strings.EqualFold(journal, "wal") {
		errs = append(errs, fmt.Errorf(
			"journal_mode is %q, not WAL — the state directory is most likely on a network "+
				"filesystem, where SQLite's WAL cannot work; move it to local storage", journal))
	}

	var synchronous int
	//billet:ignore rawsql // a pragma is not a query; sqlc has no catalogue entry for one
	if err := w.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return fmt.Errorf("read synchronous: %w", err)
	}

	if synchronous != 2 { // 2 == FULL
		errs = append(errs, fmt.Errorf("synchronous is %d, want 2 (FULL)", synchronous))
	}

	var foreignKeys int
	//billet:ignore rawsql // a pragma is not a query; sqlc has no catalogue entry for one
	if err := w.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		return fmt.Errorf("read foreign_keys: %w", err)
	}

	if foreignKeys != 1 {
		errs = append(errs, errors.New("foreign_keys is off"))
	}

	return errors.Join(errs...)
}

// integrityCheck refuses to serve from a corrupt ledger. quick_check is cheap
// enough to run at every startup for a database this size, and scheduling
// against corrupt capacity data is worse than not starting.
func (*sqliteBackend) integrityCheck(ctx context.Context, w *sql.DB) error {
	var result string
	//billet:ignore rawsql // a pragma is not a query; sqlc has no catalogue entry for one
	if err := w.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&result); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}

	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("state database failed integrity check: %s", result)
	}

	return nil
}

// bootstrapSchemaMigrations creates the migration bookkeeping table.
//
// A FILE RATHER THAN A LITERAL, BECAUSE TWO THINGS READ IT. This statement is
// executed here and its table is also named by the generated queries in
// ledgerdb — and the bookkeeping table is deliberately not a migration, so sqlc
// would not otherwise have known it exists. sqlc.yaml lists its PostgreSQL twin
// beside the PostgreSQL migration directory, since generation runs once with
// that engine; the two files are the same statement under the same declared
// substitutions, so what runs against the ledger is what codegen was given.
//
// Unlike a migration's, these bytes carry no checksum anywhere, so they may be
// edited. checkBookkeepingSchema is what constrains them.
//
//go:embed schema_migrations.sql
var bootstrapSchemaMigrations string

func (*sqliteBackend) bootstrapSchema() string { return bootstrapSchemaMigrations }

// checkBookkeepingSchema verifies schema_migrations has the columns this binary
// expects. billet has had no released version, so the only way to hit this is a
// database left by a development build; the remedy is to delete it rather than
// to write a migration for the migration table.
func (*sqliteBackend) checkBookkeepingSchema(ctx context.Context, q Querier) error {
	//billet:ignore rawsql // a pragma is not a query; sqlc has no catalogue entry for one
	rows, err := q.QueryContext(ctx, `PRAGMA table_info(schema_migrations)`)
	if err != nil {
		return fmt.Errorf("inspect schema_migrations: %w", err)
	}

	defer rows.Close()

	found := make(map[string]bool)

	for rows.Next() {
		var cid int

		var name, colType string

		var notNull, pk int

		var dflt sql.NullString

		if err := rows.Scan(&cid, &name, &colType, &notNull, &dflt, &pk); err != nil {
			return fmt.Errorf("scan schema_migrations columns: %w", err)
		}

		found[name] = true
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("inspect schema_migrations: %w", err)
	}

	return requireBookkeepingColumns(found)
}

// bookkeepingTableExists answers whether anything has created the schema.
func (*sqliteBackend) bookkeepingTableExists(ctx context.Context, q Querier) (bool, error) {
	var name string

	//billet:ignore rawsql // sqlite_master is not in sqlc's catalogue (measured: relation "sqlite_master" does not exist)
	err := q.QueryRowContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'schema_migrations'`).
		Scan(&name)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("inspect the ledger's schema: %w", err)
	}

	return true, nil
}

// userTables and countRows delegate to the file-shaped helpers in fence.go,
// which the cold readers there also use. One definition rather than two: those
// callers are handed a path and have no DB to ask, and a second copy of a rule
// PeekLedger depends on is a second thing to get right.
func (*sqliteBackend) userTables(ctx context.Context, q Querier) ([]string, error) {
	return userTables(ctx, q)
}

func (*sqliteBackend) countRows(ctx context.Context, q Querier, table string) (int64, error) {
	return countTableRows(ctx, q, table)
}

// isContention reports whether an error is SQLite saying somebody else is
// writing.
//
// MATCHED ON THE CODE, never on the message: "database is locked" is the text
// for both plain SQLITE_BUSY and SQLITE_BUSY_SNAPSHOT, and a driver or locale
// that phrases it differently would silently turn a retry into a fatal error.
// The primary code is the low byte, so this covers the extended forms —
// BUSY_RECOVERY, BUSY_SNAPSHOT and BUSY_TIMEOUT — without listing them.
func (*sqliteBackend) isContention(err error) bool { return isBusy(err) }

func isBusy(err error) bool {
	serr, ok := errors.AsType[*sqlite.Error](err)
	if !ok {
		return false
	}

	const sqliteBusy = 5

	return serr.Code()&0xff == sqliteBusy
}

// isCancellation reports whether an error is SQLite saying the operation was
// interrupted, which is how a cancelled BEGIN can arrive.
//
// Narrow deliberately: this is the ONLY driver code translated into the caller's
// context error, because every other failure is a fact about the database that
// must survive being reported.
func (*sqliteBackend) isCancellation(err error) bool { return isInterrupt(err) }

// A VAR SO A TEST CAN REACH THE BRANCH IT GUARDS. modernc's *sqlite.Error has
// unexported fields, so a code-9 error cannot be fabricated from here and the
// translation it feeds would otherwise be unreachable from any test — which is
// how it came to be written on an unverified premise in the first place. The
// same reason adminBusyLimit is a var; nothing outside this package writes
// either.
//
// NO TEST THAT SWAPS IT MAY CALL t.Parallel — however many there are; a count
// here is a comment that goes stale the next time one is added, and this one
// already had. Sequential top-level tests do not overlap, so today every swapper
// is safe by construction rather than by argument. Made parallel, their Cleanups interleave: one restores the
// other's override, and a classifier that says yes to everything escapes into
// unrelated tests, where it turns every storage fault on a cancelled context
// into a cancellation. That is the exact defect asCancellation exists to refuse,
// arriving through the seam that proves it.
var isInterrupt = func(err error) bool {
	serr, ok := errors.AsType[*sqlite.Error](err)
	if !ok {
		return false
	}

	const sqliteInterrupt = 9

	return serr.Code()&0xff == sqliteInterrupt
}

// beginWrite has nothing to do, and that is a property of the DSN rather than an
// omission: _txlock=immediate means BeginTx has already issued BEGIN IMMEDIATE
// and taken SQLite's single write lock. There is no window between the BEGIN and
// this call in which another writer could be admitted.
func (*sqliteBackend) beginWrite(context.Context, *sql.Tx) error { return nil }

// snapshotInto is VACUUM INTO, waiting out a writer in another process on the
// same terms beginWrite does.
//
// THE CONSTRAINTS ARE MEASURED RATHER THAN READ, and three of them decide the
// shape of this: it cannot run inside a transaction ("cannot VACUUM from within
// a transaction"), it is refused on the query-only reader pool ("attempt to
// write a readonly database"), and it REFUSES an existing destination ("output
// file already exists") — which is a no-clobber install for free.
//
// The patience is the caller's, exactly as it is for a transaction: a control
// plane waits as long as its context allows, and an operator command gives up
// after adminBusyLimit because a person is waiting on it.
func (b *sqliteBackend) snapshotInto(ctx context.Context, db *DB, path string) error {
	var (
		errBusy  error
		deadline time.Time
	)

	if db.admin {
		deadline = time.Now().Add(adminBusyLimit)
	}

	for attempt := 0; ; attempt++ {
		if attempt > 0 {
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("state: snapshot the ledger: %w", err)
			}

			if !deadline.IsZero() && time.Now().After(deadline) {
				return fmt.Errorf(
					"state: another process held this ledger's write lock for longer than %s, so "+
						"the snapshot was not taken. Nothing was written to %s; run it again in "+
						"a moment: %w", adminBusyLimit, path, errBusy)
			}
		}

		//billet:ignore rawsql // sqlc cannot express VACUUM INTO, and its measured constraints are why this exists
		_, err := db.w.ExecContext(ctx, `VACUUM INTO ?`, path)
		if err == nil {
			return nil
		}

		if !b.isContention(err) {
			return fmt.Errorf("state: snapshot the ledger into %s: %w", path, err)
		}

		errBusy = err

		wait := time.NewTimer(busyRetryInterval)

		select {
		case <-ctx.Done():
			wait.Stop()

			return fmt.Errorf("state: snapshot the ledger: %w", ctx.Err())
		case <-wait.C:
		}
	}
}

// sharedLedger is false, and the state package's own durability check is what
// keeps it true: a SQLite ledger must be on local storage, because WAL
// coordinates through shared memory that assumes a single host, and Open reads
// the pragma back and fails closed otherwise. So there is no second machine that
// could hold these rows open, and the directory lock is the whole exclusion.
func (*sqliteBackend) sharedLedger() bool { return false }

// claimController has nothing to TAKE, because the exclusive hold on the state
// directory is already this deployment's controller exclusion and Open took it
// before this backend existed. There is no second host to worry about either: a
// SQLite ledger is reachable from exactly one machine, which is the same reason
// the state directory must be on local storage.
//
// SO WHAT IT DOES IS CHECK. A handle that did not take the directory lock holds
// no exclusion at all — OpenAdmin returns one deliberately, so an operator
// command can reach a ledger a running control plane owns — and letting that
// handle claim would write a row saying it is the controller while the actual
// controller carries on. Nothing in billet does this today; the API allows it,
// so this refuses it.
func (*sqliteBackend) claimController(_ context.Context, db *DB) error {
	if db.unlocked {
		return fmt.Errorf("%w: this handle does not hold %s, so it is an operator command "+
			"rather than a control plane", ErrControllerHeld, DirectoryLockPath(db.stateDir))
	}

	return nil
}

// prepare has nothing to learn: everything this backend needs to know about the
// ledger is the path it was constructed with.
func (*sqliteBackend) prepare(context.Context, *sql.DB) error { return nil }

func (*sqliteBackend) releaseController() error { return nil }
