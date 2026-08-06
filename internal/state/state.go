// Package state is billet's durable control-plane store.
//
// The store is SQLite, deliberately. A billet deployment has one authoritative
// server process, and the data it keeps — the capacity ledger, job history, and
// advisory pointers to node-local volumes — is small and hot. SQLite gives that
// ACID semantics with no daemon to operate.
//
// Three constraints are load-bearing rather than stylistic, and all three are
// enforced here rather than merely documented:
//
//   - ONE authoritative process. An exclusive lock on the state directory is held
//     for the lifetime of DB, so a second server exits immediately instead of
//     taking turns writing conflicting scheduling decisions. SQLite's own
//     single-writer rule prevents simultaneous writes; it does not prevent two
//     control planes.
//   - ONE writer within the process. Mutations go through a single-connection
//     pool so an allocation decision serializes inside SQLite. Reads go through a
//     separate pool exposed only as a query interface, so a caller cannot write
//     through it by accident.
//   - Durability actually verified. PRAGMA journal_mode=WAL returns the mode that
//     is in effect, which may not be the mode requested — notably on a network
//     filesystem, where WAL cannot work at all because its shared-memory
//     coordination assumes a single host. Open reads the pragmas back from the
//     writer connection and fails closed on any mismatch.
//
// What this store is NOT: authoritative for cache generation pointers. Those live
// on the node that owns the underlying volume, because a commit here cannot be
// made atomic with a snapshot on a remote machine. What is kept here is advisory
// metadata used for scheduling affinity.
package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps billet a single static binary
)

// ErrConflict is returned when a compare-and-swap loses. Callers should re-read
// and decide, never blindly retry — a lost cache publication is correct
// behaviour, not a transient failure.
var ErrConflict = errors.New("state: compare-and-swap conflict")

// ErrLocked means another billet process already owns this state directory.
var ErrLocked = errors.New("state: another billet process holds this state directory")

// Querier is the read surface. It is deliberately narrower than *sql.DB: handing
// out the pool would let any caller issue writes on a connection that is supposed
// to be read-only, which is exactly the invariant this package exists to hold.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB wraps two connection pools over one SQLite file: a single-connection writer
// so mutations serialize, and a query-only pool so status reads never queue
// behind an allocation.
type DB struct {
	w    *sql.DB
	r    *sql.DB
	lock *dirLock
}

// Open prepares the state directory, takes the exclusive process lock, opens the
// database, and verifies that the durability pragmas actually took effect.
//
// The caller's context bounds startup only; it does not own the returned DB.
func Open(ctx context.Context, stateDir string) (*DB, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, and this directory will
	// hold the mTLS CA key. Tighten it rather than inheriting whatever was there.
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tighten state dir %s: %w", stateDir, err)
	}

	lock, err := lockDir(stateDir)
	if err != nil {
		return nil, err
	}

	path := filepath.Join(stateDir, "billet.db")

	// synchronous=FULL, not NORMAL: NORMAL can lose recently committed
	// transactions on power loss, and the capacity ledger is exactly the thing
	// that must survive an unclean shutdown for restart reconciliation to work.
	//
	// busy_timeout is a backstop. With a single writer it should never fire; if
	// it does, something is holding a write transaction far too long.
	writerDSN := dsn(path,
		"journal_mode(WAL)",
		"synchronous(FULL)",
		"busy_timeout(5000)",
		"foreign_keys(ON)",
	)
	// The reader stays read-write at the OS level on purpose. mode=ro cannot open
	// a WAL database unless it can also map the -shm file, so forcing it trades
	// one safety property for a startup failure. query_only plus the narrow
	// Querier interface is what keeps reads read-only.
	readerDSN := dsn(path,
		"busy_timeout(5000)",
		"foreign_keys(ON)",
		"query_only(ON)",
	)

	w, err := sql.Open("sqlite", writerDSN)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open state db: %w", err), lock.release())
	}
	// The single most important line in this file.
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	r, err := sql.Open("sqlite", readerDSN)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open state db (reader): %w", err), w.Close(), lock.release())
	}
	r.SetMaxOpenConns(4)

	db := &DB{w: w, r: r, lock: lock}

	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	// Every failure here closes both pools and releases the lock. The cleanup
	// error is joined rather than dropped: a lock that failed to release turns
	// the next start into a confusing "another billet process holds this state
	// directory", and the reason needs to survive to say otherwise.
	if err := db.PingContext(startupCtx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	if err := db.migrate(startupCtx); err != nil {
		return nil, errors.Join(fmt.Errorf("migrate state db: %w", err), db.Close())
	}
	return db, nil
}

// startupTimeout bounds the pragma verification, integrity check and migrations.
// Generous, because a first run creates the database and an integrity check
// scans it; anything slower than this is a sick disk, not a slow one.
const startupTimeout = 30 * time.Second

// PingContext proves the database is reachable AND configured as promised.
func (db *DB) PingContext(ctx context.Context) error {
	if err := db.w.PingContext(ctx); err != nil {
		return fmt.Errorf("ping state db: %w", err)
	}
	if err := db.verifyWriterPragmas(ctx); err != nil {
		return err
	}
	return db.integrityCheck(ctx)
}

// dsn builds a file: URI with the given pragmas, escaping the path so a state
// directory containing spaces, '?' or '#' does not silently truncate the DSN.
func dsn(path string, pragmas ...string) string {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{}
	for _, p := range pragmas {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// verifyWriterPragmas reads the settings back from the writer connection. This
// is not paranoia: `PRAGMA journal_mode=WAL` reports the mode actually in
// effect, and on a network filesystem — where WAL is unsupported because its
// shared-memory index assumes one host — the request silently degrades to
// DELETE. Failing closed here turns "your ledger was never durable" into a
// startup error.
func (db *DB) verifyWriterPragmas(ctx context.Context) error {
	var errs []error

	var journal string
	if err := db.w.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journal); err != nil {
		return fmt.Errorf("read journal_mode: %w", err)
	}
	if !strings.EqualFold(journal, "wal") {
		errs = append(errs, fmt.Errorf(
			"journal_mode is %q, not WAL — the state directory is most likely on a network "+
				"filesystem, where SQLite's WAL cannot work; move it to local storage", journal))
	}

	var synchronous int
	if err := db.w.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		return fmt.Errorf("read synchronous: %w", err)
	}
	if synchronous != 2 { // 2 == FULL
		errs = append(errs, fmt.Errorf("synchronous is %d, want 2 (FULL)", synchronous))
	}

	var foreignKeys int
	if err := db.w.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
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
func (db *DB) integrityCheck(ctx context.Context) error {
	var result string
	if err := db.w.QueryRowContext(ctx, `PRAGMA quick_check(1)`).Scan(&result); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if !strings.EqualFold(result, "ok") {
		return fmt.Errorf("state database failed integrity check: %s", result)
	}
	return nil
}

// Close releases both pools and the process lock.
func (db *DB) Close() error {
	return errors.Join(db.closeDBs(), db.lock.release())
}

func (db *DB) closeDBs() error {
	return errors.Join(db.r.Close(), db.w.Close())
}

// Reader returns the query-only pool.
func (db *DB) Reader() Querier { return db.r }

// Tx runs fn inside a single write transaction. Every mutation goes through here
// so that an allocation decision — read current usage, decide, record it — is one
// atomic step rather than a read followed by a hopeful write.
func (db *DB) Tx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := db.w.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin write tx: %w", err)
	}
	defer func() {
		// Rollback after a successful Commit is a documented no-op returning
		// sql.ErrTxDone; this only does real work on the error and panic paths,
		// where fn's error is the one worth reporting.
		//nolint:errcheck // see above: no-op after Commit, and fn's error wins otherwise.
		_ = tx.Rollback()
	}()
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write tx: %w", err)
	}
	return nil
}

// migration is identified by an explicit, immutable Version. Counting applied
// rows is not a schema version: a deleted row reruns a migration, a forged row
// skips one, and inserting a migration in the middle silently reruns the tail.
// The checksum additionally catches an edited migration, which would otherwise
// leave two deployments believing they share a schema they do not.
type migration struct {
	Version int
	Name    string
	Stmts   []string
}

func (m migration) checksum() string {
	h := sha256.New()
	for _, s := range m.Stmts {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// migrations are append-only. Never edit or reorder an existing entry; add a new
// one. Rolling a CI control plane backwards is a restore-from-backup operation,
// not a schema operation.
var migrations = []migration{
	{
		Version: 1,
		Name:    "nodes",
		Stmts: []string{
			// A node is a compute host. Nodes dial the server, so the server learns
			// of them here rather than from configuration.
			`CREATE TABLE nodes (
				name          TEXT PRIMARY KEY,
				provider      TEXT NOT NULL,
				-- Fencing epoch, bumped every time a node re-registers. Responses
				-- carrying a stale epoch are from an instance the server has already
				-- written off and must be ignored.
				epoch         INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0),
				total_vcpu    INTEGER NOT NULL DEFAULT 0 CHECK (total_vcpu >= 0),
				total_memory  INTEGER NOT NULL DEFAULT 0 CHECK (total_memory >= 0),
				last_seen_at  TEXT NOT NULL,
				drained       INTEGER NOT NULL DEFAULT 0 CHECK (drained IN (0, 1))
			) STRICT`,
		},
	},
	{
		Version: 2,
		Name:    "leases",
		Stmts: []string{
			// The capacity ledger. A row exists from the moment capacity is escrowed
			// — before a scale-set listener advertises it to GitHub — not from the
			// moment a VM boots. Reserving any later lets concurrent tier listeners
			// each advertise their own maximum and collectively overcommit the host.
			`CREATE TABLE leases (
				id           TEXT PRIMARY KEY,
				tier         TEXT NOT NULL,
				node         TEXT REFERENCES nodes(name) ON DELETE SET NULL,
				phase        TEXT NOT NULL CHECK (phase IN
					('capacity','assigned','launching','online','busy','done','failed')),
				vcpu         INTEGER NOT NULL CHECK (vcpu > 0),
				memory       INTEGER NOT NULL CHECK (memory > 0),
				run_id       INTEGER,
				job_id       INTEGER,
				epoch        INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0),
				created_at   TEXT NOT NULL,
				heartbeat_at TEXT NOT NULL,
				expires_at   TEXT NOT NULL
			) STRICT`,
			`CREATE INDEX leases_open_idx ON leases(phase) WHERE phase NOT IN ('done','failed')`,
			`CREATE INDEX leases_node_idx ON leases(node)`,
		},
	},
	{
		Version: 3,
		Name:    "cache_generations",
		Stmts: []string{
			// Advisory only. Authoritative generation pointers live on the owning
			// node, because a commit here cannot be atomic with a remote snapshot.
			// This exists so the scheduler can prefer a node that already holds a
			// warm generation for the cache key a job will want.
			`CREATE TABLE cache_generations (
				node       TEXT NOT NULL,
				store      TEXT NOT NULL,
				cache_key  TEXT NOT NULL,
				generation TEXT NOT NULL,
				updated_at TEXT NOT NULL,
				PRIMARY KEY (node, store, cache_key)
			) STRICT`,
		},
	},
	{
		Version: 4,
		Name:    "job_history",
		Stmts: []string{
			// Retained after a lease is reaped, for the dashboard and for answering
			// "why did this queue".
			`CREATE TABLE job_history (
				lease_id    TEXT PRIMARY KEY,
				tier        TEXT NOT NULL,
				node        TEXT,
				run_id      INTEGER,
				job_id      INTEGER,
				repo        TEXT,
				conclusion  TEXT,
				queued_at   TEXT NOT NULL,
				started_at  TEXT,
				finished_at TEXT
			) STRICT`,
			`CREATE INDEX job_history_queued_idx ON job_history(queued_at)`,
		},
	},
}

const bootstrapSchemaMigrations = `CREATE TABLE IF NOT EXISTS schema_migrations (
	version    INTEGER PRIMARY KEY,
	name       TEXT NOT NULL,
	checksum   TEXT NOT NULL,
	applied_at TEXT NOT NULL
)`

// checkBookkeepingSchema verifies schema_migrations has the columns this binary
// expects. billet has had no released version, so the only way to hit this is a
// database left by a development build; the remedy is to delete it rather than
// to write a migration for the migration table.
func checkBookkeepingSchema(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `PRAGMA table_info(schema_migrations)`)
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

	var missing []string
	for _, col := range []string{"version", "name", "checksum", "applied_at"} {
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

// appliedMigration is the bookkeeping row for one already-applied migration.
type appliedMigration struct {
	name     string
	checksum string
}

// readAppliedMigrations exists as its own function so `defer rows.Close()` is
// usable: the caller runs inside a transaction and issues further statements, so
// the cursor has to be closed before it continues.
func readAppliedMigrations(ctx context.Context, tx *sql.Tx) (map[int]appliedMigration, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT version, name, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		// Propagated, not swallowed. A permission, corruption, or locking error must
		// not be mistaken for an empty database and answered by recreating the schema.
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	seen := make(map[int]appliedMigration)

	for rows.Next() {
		var (
			v int
			r appliedMigration
		)

		if err := rows.Scan(&v, &r.name, &r.checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}

		seen[v] = r
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}

	return seen, nil
}

func (db *DB) migrate(ctx context.Context) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		// Bootstrapping is idempotent and lives outside the versioned set, so the
		// bookkeeping table's existence is never itself a migration whose absence
		// has to be inferred from a failed query.
		if _, err := tx.ExecContext(ctx, bootstrapSchemaMigrations); err != nil {
			return fmt.Errorf("bootstrap schema_migrations: %w", err)
		}
		// IF NOT EXISTS leaves an existing table alone, including one written by a
		// billet whose bookkeeping format differed. Detect that explicitly: the
		// alternative is a bare "no such column" from the SELECT below, which
		// gives an operator nothing to act on.
		if err := checkBookkeepingSchema(ctx, tx); err != nil {
			return err
		}

		seen, err := readAppliedMigrations(ctx, tx)
		if err != nil {
			return err
		}

		applied := make(map[int]struct{}, len(seen))
		for v := range seen {
			applied[v] = struct{}{}
		}

		known := make(map[int]struct{}, len(migrations))
		for _, m := range migrations {
			known[m.Version] = struct{}{}
		}
		// A version this binary has never heard of means the database was written
		// by a newer billet. Running an older control plane against a newer schema
		// corrupts state slowly and confusingly; refuse instead.
		for v := range applied {
			if _, ok := known[v]; !ok {
				return fmt.Errorf(
					"state database has migration %d, which this billet does not know about; "+
						"it was written by a newer version", v)
			}
		}

		for _, m := range migrations {
			if rec, ok := seen[m.Version]; ok {
				if rec.checksum != m.checksum() {
					return fmt.Errorf(
						"migration %d (%s) was applied with different SQL than this binary contains; "+
							"migrations are append-only and must never be edited",
						m.Version, m.Name)
				}
				continue
			}
			for _, stmt := range m.Stmts {
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
				}
			}
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO schema_migrations (version, name, checksum, applied_at) VALUES (?, ?, ?, ?)`,
				m.Version, m.Name, m.checksum(), time.Now().UTC().Format(time.RFC3339Nano),
			); err != nil {
				return fmt.Errorf("record migration %d (%s): %w", m.Version, m.Name, err)
			}
		}
		return nil
	})
}
