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

	// unlocked marks a handle opened by an operator command while a control
	// plane held the directory. Such a handle did not migrate and cannot assume
	// the schema it verified at open is still the one it is writing against, so
	// every transaction re-checks. See OpenAdmin and verifySchemaIn.
	unlocked bool
}

// Open prepares the state directory, takes the exclusive process lock, opens the
// database, and verifies that the durability pragmas actually took effect.
//
// The caller's context bounds startup only; it does not own the returned DB.
func Open(ctx context.Context, stateDir string) (*DB, error) {
	return openDir(ctx, stateDir, false)
}

// openDir is the shared body of Open and OpenAdmin.
//
// admin says the caller is a ONE-SHOT OPERATOR COMMAND rather than a control
// plane, which changes exactly two things and nothing else: it may proceed
// without the exclusive directory lock when a control plane already holds it,
// and in that case it VERIFIES the schema instead of migrating it. See
// OpenAdmin for why both halves are necessary.
func openDir(ctx context.Context, stateDir string, admin bool) (*DB, error) {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, and this directory will
	// hold the mTLS CA key. Tighten it rather than inheriting whatever was there.
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tighten state dir %s: %w", stateDir, err)
	}

	// A NIL LOCK MEANS "SOMEBODY ELSE IS THE CONTROL PLANE HERE", and it is
	// reachable only for an admin caller. Everything downstream branches on this
	// one value rather than re-deriving the situation.
	lock, err := lockDir(stateDir)

	switch {
	case err == nil:
	case admin && errors.Is(err, ErrLocked):
		lock = nil
	default:
		return nil, err
	}

	path := filepath.Join(stateDir, "billet.db")

	// synchronous=FULL, not NORMAL: NORMAL can lose recently committed
	// transactions on power loss, and the capacity ledger is exactly the thing
	// that must survive an unclean shutdown for restart reconciliation to work.
	//
	// busy_timeout covers a writer waiting for another writer, which is now
	// REACHABLE rather than theoretical: an operator command opens this same
	// database while the control plane is running. See OpenAdmin.
	//
	// _txlock=immediate IS LOAD-BEARING, and it is the reason that is safe.
	//
	// database/sql's BeginTx issues a plain BEGIN, which in WAL mode is DEFERRED:
	// the transaction takes a read snapshot at its first read and only asks for
	// the write lock later. Every allocation decision here is read-current,
	// decide, record — so if anything commits in between, SQLite cannot promote
	// the now-stale snapshot and fails the write with SQLITE_BUSY_SNAPSHOT (517).
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
	// here because there is one writer connection per process already.
	writerDSN := dsnWith(path, map[string]string{"_txlock": "immediate"},
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

	db := &DB{w: w, r: r, lock: lock, unlocked: lock == nil}

	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	// Every failure here closes both pools and releases the lock. The cleanup
	// error is joined rather than dropped: a lock that failed to release turns
	// the next start into a confusing "another billet process holds this state
	// directory", and the reason needs to survive to say otherwise.
	if err := db.PingContext(startupCtx); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	// MIGRATING IS THE LOCK HOLDER'S JOB. Without the lock another billet is
	// already using this schema, and upgrading it underneath a process that is
	// mid-transaction against it is the one thing an operator command must never
	// do — so it checks instead, and refuses if it would have had to write.
	if lock == nil {
		if err := db.verifySchema(startupCtx); err != nil {
			return nil, errors.Join(err, db.Close())
		}

		return db, nil
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

	// RE-CHECKED INSIDE THE TRANSACTION, for a handle that never held the lock.
	// The check at open time is only true of the control plane that was running
	// then; this one is true of the schema this transaction is actually writing
	// against, because BEGIN IMMEDIATE already holds the write lock a migration
	// would need.
	if db.unlocked {
		if err := verifySchemaIn(ctx, tx); err != nil {
			return err
		}
	}

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

// placementMigration is appended to migrations in init. Kept as its own value
// only so this long comment does not sit inside the main slice literal.
var placementMigration = migration{
	Version: 5,
	Name:    "lease_placement",
	Stmts: []string{
		// A reservation's PLACEMENT must be durable on the row, not derived from
		// whatever the tier catalog happens to say later.
		//
		// target_node is the node a lease is constrained to by its tier's config
		// at reserve time; `node` remains the node that actually bound it. They
		// are different questions and were previously conflated: target_node has
		// no foreign key because it may name a host that has not registered yet,
		// while `node` keeps its FK because binding proves the node exists.
		//
		// macos_slot records whether the lease consumes one of its host's macOS
		// guest licences. Counting that by walking the current tier map meant
		// renaming a tier, changing its guest_os, or restarting with a different
		// catalog silently reclassified leases already in flight.
		`ALTER TABLE leases ADD COLUMN target_node TEXT`,
		`ALTER TABLE leases ADD COLUMN macos_slot INTEGER NOT NULL DEFAULT 0 CHECK (macos_slot IN (0, 1))`,
		// Reap scans by expiry; without this it is a full table scan holding the
		// only writer connection.
		`CREATE INDEX leases_expiry_idx ON leases(expires_at) WHERE phase NOT IN ('done','failed')`,
		// job_history gains the queue timestamp so queue duration is measured
		// rather than fabricated from the terminalization time.
		`ALTER TABLE job_history ADD COLUMN assigned_at TEXT`,
	},
}

// guestOSMigration records what a lease actually boots, for the same reason
// placement is recorded: a host may be restricted to a subset of guest
// operating systems, and the check happens at bind time — long after the tier
// catalog that produced the lease may have changed underneath it.
//
// 'linux' is the column default because it is the overwhelming majority case
// and a real guest OS: an empty default would match no allowlist and strand
// every lease written before the column existed.
var guestOSMigration = migration{
	Version: 6,
	Name:    "lease_guest_os",
	Stmts: []string{
		`ALTER TABLE leases ADD COLUMN guest_os TEXT NOT NULL DEFAULT 'linux'`,
	},
}

// placementFactsMigration corrects migration 6's backfill and records the
// remaining placement fact.
//
// Migration 6 defaulted EVERY pre-existing lease to 'linux', including macOS
// ones. That is not a safe default in the direction its own comment claimed:
// an unbound macOS lease relabelled Linux would be PERMITTED onto a Linux-only
// host, even though its durable macos_slot proves what it is.
//
// The backfill reads macos_slot, which is authoritative only for leases written
// after migration 5 — that migration added the column defaulting to 0 and did
// not backfill it either, so a macOS lease predating it is indistinguishable
// from a Linux one and this UPDATE cannot repair it. Nothing here can: the
// information was never recorded. What protects those rows is the allocator
// refusing to place any lease with no recorded provider, which every pre-v7
// lease is; they fail closed rather than being guessed at.
//
// Corrected by appending rather than by editing migration 6: the checksum guard
// exists precisely to stop an applied migration changing underneath a database,
// and "nobody has run it yet" is the argument that erodes that discipline.
//
// provider joins target_node, macos_slot and guest_os as a placement fact
// recorded on the row. A Firecracker lease cannot run on a Tart host, so Bind
// has to be able to compare — and re-deriving it from the live catalog is what
// lets a tier redefined mid-flight reclassify a lease that is already running.
var placementFactsMigration = migration{
	Version: 7,
	Name:    "lease_placement_facts",
	Stmts: []string{
		`UPDATE leases SET guest_os = 'macos' WHERE macos_slot = 1`,
		`ALTER TABLE leases ADD COLUMN provider TEXT NOT NULL DEFAULT ''`,
	},
}

// requestIDMigration renames job_id to what it actually holds.
//
// The column was written before anything consumed GitHub's scale-set API, and
// the API disagrees with it twice over. What billet needs to record is
// RunnerRequestID — that is the identity AcquireJobs claims work by, and the
// only one that makes a redelivered message idempotent. GitHub's own JobID is a
// separate field and is a STRING, so a column named job_id holding an int64 was
// storing the wrong value under a name that would later look right to whoever
// tried to correlate a lease with GitHub's API.
//
// Renamed rather than left alone because the trap is silent: `job_id INTEGER`
// accepts the request id without complaint, and SQLite's affinity rules would
// even accept the GUID. The failure surfaces only when a human reads it.
//
// RENAME COLUMN needs SQLite 3.25+; modernc.org/sqlite is far past that.
var requestIDMigration = migration{
	Version: 8,
	Name:    "lease_request_id",
	Stmts: []string{
		`ALTER TABLE leases RENAME COLUMN job_id TO request_id`,
		`ALTER TABLE job_history RENAME COLUMN job_id TO request_id`,
	},
}

// providerListMigration splits "which backends may this lease run on" from
// "which one is it on".
//
// The single `provider` column answered both, which quietly made a tier's
// backend a property of its reservation: a lease was pinned before anything knew
// where it would run, so one `runs-on` label could never span a machine at home
// and a cloud. `providers` is the ordered list the tier accepted, copied at
// reserve; `chosen_provider` is filled in at bind.
//
// Existing rows carry their single provider into BOTH columns. That is the
// honest reading of an old row: it was reserved for exactly one backend, and if
// it is already bound, that is the backend it is on. A bound row would be
// indistinguishable from an unbound one otherwise, and the placement check fails
// closed on a lease it cannot verify.
var providerListMigration = migration{
	Version: 9,
	Name:    "lease_provider_list",
	Stmts: []string{
		`ALTER TABLE leases ADD COLUMN providers TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE leases ADD COLUMN chosen_provider TEXT NOT NULL DEFAULT ''`,
		`UPDATE leases SET providers = provider WHERE providers = ''`,
		// Only rows that are actually placed get a chosen backend. An unbound row
		// has not chosen anything, and writing one would make the column mean
		// "was reserved for" again — the exact conflation being undone.
		`UPDATE leases SET chosen_provider = provider WHERE node IS NOT NULL AND chosen_provider = ''`,
	},
}

// A NODE IS SOMEWHERE, and until there was a word for where, a cache had no
// address and every host looked equally close to every other one.
//
// Empty is "unsited", which is what every existing row is and what a
// single-machine deployment stays. It is a real value rather than a missing one:
// one place is still one place, and a deployment that never names it should not
// have to.
var nodeSiteMigration = migration{
	Version: 10,
	Name:    "node_site",
	Stmts: []string{
		`ALTER TABLE nodes ADD COLUMN site TEXT NOT NULL DEFAULT ''`,
	},
}

// WHETHER A HOST IS REACHABLE IS NOW A FACT THE LEDGER NEEDS, because capacity
// is counted here and a machine that is gone must stop backing advertisements.
//
// SEPARATE FROM `drained`, which is a different state: draining is a host that
// is finishing its work and taking no more, and it is still there. This is the
// plane's judgement about whether it is there at all.
//
// DEFAULTS TO 0, so a ledger written by an older billet trusts nothing until
// each node registers again — which is the same conservative start a restart
// gets, and the correct one: liveness is the plane's judgement and a plane that
// has just started has not formed one.
var nodeLivenessMigration = migration{
	Version: 11,
	Name:    "node_liveness",
	Stmts: []string{
		`ALTER TABLE nodes ADD COLUMN live INTEGER NOT NULL DEFAULT 0 CHECK (live IN (0, 1))`,
	},
}

// A CERTIFICATE CAN BE TAKEN BACK, which until now it could not.
//
// The wire's whole admission decision is "an operator issued this host a
// certificate", and it lasts a year. A decommissioned machine, or one whose key
// leaked, could rejoin and be handed work — including a JIT credential that
// registers a runner against the organisation — and the only remedy was to
// rotate the CA, which invalidates every node at once.
//
// KEYED ON SERIAL, not on node name. A name can be re-issued to a replacement
// machine deliberately, and revoking the name would refuse the replacement too.
// The serial identifies the one credential being withdrawn.
var certRevocationMigration = migration{
	Version: 12,
	Name:    "cert_revocation",
	Stmts: []string{
		`CREATE TABLE revoked_certs (
			serial     TEXT PRIMARY KEY,
			node       TEXT NOT NULL,
			reason     TEXT NOT NULL DEFAULT '',
			revoked_at TEXT NOT NULL
		)`,
		`CREATE INDEX idx_revoked_certs_node ON revoked_certs (node)`,
	},
}

// A NODE ASKS TO JOIN AND AN OPERATOR SAYS YES, which is a decision that needs
// somewhere to sit between the two.
//
// Admission used to be entirely out of band: an operator ran `billet ca issue`
// and copied a bundle to the machine. That works and it is not discoverable — a
// node that is powered on and pointed at a control plane appears nowhere, so the
// operator has to already know it exists, and the thing they compare to decide
// it is the right machine is a file they copied rather than anything the node
// proved.
//
// KEYED ON NAME, WITH THE FINGERPRINT AS THE FACT BEING APPROVED. The name is
// how an operator refers to a host; the fingerprint is what makes approving it
// mean something, because it is the one value both ends can display and a human
// can compare out of band.
var nodeEnrollmentMigration = migration{
	Version: 13,
	Name:    "node_enrollment",
	Stmts: []string{
		`CREATE TABLE node_enrollments (
			name         TEXT PRIMARY KEY,
			fingerprint  TEXT NOT NULL,
			csr_pem      TEXT NOT NULL,
			cert_pem     TEXT NOT NULL DEFAULT '',
			state        TEXT NOT NULL CHECK (state IN ('pending','approved','denied')),
			requested_at TEXT NOT NULL,
			decided_at   TEXT NOT NULL DEFAULT ''
		)`,
	},
}

// ENROLLING HAS TO COST SOMETHING TO ATTEMPT, or the request endpoint is open
// to anyone who can reach the port.
//
// Approval still cannot be tricked — an operator matches a fingerprint against
// what the node printed — but an unauthenticated endpoint lets a stranger fill
// the pending list with plausible entries, and take a NAME before the real
// machine asks for it. "First key claims the name" protects an operator from
// approving a substitute; without a credential in front of it, it also lets
// somebody deny a machine its own name.
//
// HASHED, NEVER STORED. A join token is a credential, so the ledger keeps only
// what is needed to recognise one — the same reason a password is not stored.
var joinTokenMigration = migration{
	Version: 14,
	Name:    "join_tokens",
	Stmts: []string{
		`CREATE TABLE join_tokens (
			token_sha256   TEXT PRIMARY KEY,
			note           TEXT NOT NULL DEFAULT '',
			uses_remaining INTEGER NOT NULL,
			created_at     TEXT NOT NULL,
			expires_at     TEXT NOT NULL
		)`,
		// A certificate handed out by `billet ca issue` is an admission too, and
		// it left no record: nothing could answer "what has been let into this
		// deployment, and when".
		`ALTER TABLE node_enrollments ADD COLUMN source TEXT NOT NULL DEFAULT 'enrolled'`,
	},
}

// A CREDENTIAL CAN ONLY BE TAKEN BACK IF BILLET KNOWS IT EXISTS.
//
// Revocation is by serial, and a serial names one certificate. That is the right
// granularity — a node name is legitimately re-issued to a replacement machine,
// so revoking the name would refuse the replacement too — but it only works if
// every serial in circulation is written down. Renewal minted a fresh key and
// serial, returned it, and recorded nothing, so a node that had renewed once
// held a credential the control plane could not name. An operator revoking the
// bundle they issued took back a serial nobody was presenting, was told it would
// be refused on its next request, and the host carried on registering, binding
// leases and asking for JIT runner registrations.
var issuedCertMigration = migration{
	Version: 15,
	Name:    "issued_certs",
	Stmts: []string{
		`CREATE TABLE issued_certs (
			serial     TEXT PRIMARY KEY,
			node       TEXT NOT NULL,
			-- enrolled | issued | renewed: how this credential came to exist,
			-- which is what an operator reads when deciding what to take back.
			source     TEXT NOT NULL,
			not_after  TEXT NOT NULL,
			issued_at  TEXT NOT NULL
		) STRICT`,
		`CREATE INDEX idx_issued_certs_node ON issued_certs (node)`,
	},
}

// CAPACITY IS NOT FREED BY A LEASE EXPIRING, only by its compute being gone.
//
// The reaper terminalizes anything whose holder stopped heartbeating, which is
// right for escrow nobody launched and wrong for a lease with a container behind
// it: terminalizing frees the capacity immediately, while the container keeps
// running until the node next sweeps. Another tier can escrow that slot in
// between, and two jobs end up on a machine sized for one.
//
// So an expired RUNNING lease moves to a phase that still charges the host, and
// leaves it only on proof the compute is gone. The phase list is a CHECK
// constraint and SQLite cannot alter one, so the table is rebuilt.
var quarantineMigration = migration{
	Version: 16,
	Name:    "lease_quarantine",
	Stmts: []string{
		// Columns are named rather than SELECT *: the order has to survive every
		// earlier migration for a star to be correct, and nothing checks that.
		`CREATE TABLE leases_new (
			id              TEXT PRIMARY KEY,
			tier            TEXT NOT NULL,
			node            TEXT REFERENCES nodes(name) ON DELETE SET NULL,
			phase           TEXT NOT NULL CHECK (phase IN
				('capacity','assigned','launching','online','busy','quarantine','done','failed')),
			vcpu            INTEGER NOT NULL CHECK (vcpu > 0),
			memory          INTEGER NOT NULL CHECK (memory > 0),
			run_id          INTEGER,
			request_id      INTEGER,
			epoch           INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0),
			created_at      TEXT NOT NULL,
			heartbeat_at    TEXT NOT NULL,
			expires_at      TEXT NOT NULL,
			target_node     TEXT,
			macos_slot      INTEGER NOT NULL DEFAULT 0 CHECK (macos_slot IN (0, 1)),
			guest_os        TEXT NOT NULL DEFAULT 'linux',
			provider        TEXT NOT NULL DEFAULT '',
			providers       TEXT NOT NULL DEFAULT '',
			chosen_provider TEXT NOT NULL DEFAULT ''
		) STRICT`,
		`INSERT INTO leases_new
		   (id, tier, node, phase, vcpu, memory, run_id, request_id, epoch, created_at,
		    heartbeat_at, expires_at, target_node, macos_slot, guest_os, provider,
		    providers, chosen_provider)
		 SELECT id, tier, node, phase, vcpu, memory, run_id, request_id, epoch, created_at,
		        heartbeat_at, expires_at, target_node, macos_slot, guest_os, provider,
		        providers, chosen_provider
		   FROM leases`,
		`DROP TABLE leases`,
		`ALTER TABLE leases_new RENAME TO leases`,
		// EVERY index the old table carried, not the ones that came to mind. A
		// rebuild drops them all, and a missing one is invisible until the table is
		// large enough for the scan to matter — leases_expiry_idx is what keeps the
		// reaper from scanning the whole lease history on the single writer
		// connection every listener is waiting for.
		`CREATE INDEX leases_open_idx ON leases(phase) WHERE phase NOT IN ('done','failed')`,
		`CREATE INDEX leases_node_idx ON leases(node)`,
		`CREATE INDEX leases_expiry_idx ON leases(expires_at) WHERE phase NOT IN ('done','failed')`,
	},
}

// STRICT, LIKE EVERY OTHER TABLE. SQLite's default typing accepts a string where
// an integer belongs and stores it as one, so a bug that writes the wrong type
// is discovered by a later reader rather than by the write. Three tables added
// during the trust work were declared without it, and consistency here is worth
// more than the tables are large: they hold credentials and admission decisions,
// which are exactly the rows worth being strict about.
//
// A rebuild, because STRICT is a property of the table declaration.
var strictTrustTablesMigration = migration{
	Version: 17,
	Name:    "strict_trust_tables",
	Stmts: []string{
		`CREATE TABLE revoked_certs_new (
			serial     TEXT PRIMARY KEY,
			node       TEXT NOT NULL,
			reason     TEXT NOT NULL DEFAULT '',
			revoked_at TEXT NOT NULL
		) STRICT`,
		`INSERT INTO revoked_certs_new (serial, node, reason, revoked_at)
		 SELECT serial, node, reason, revoked_at FROM revoked_certs`,
		`DROP TABLE revoked_certs`,
		`ALTER TABLE revoked_certs_new RENAME TO revoked_certs`,
		`CREATE INDEX idx_revoked_certs_node ON revoked_certs (node)`,

		`CREATE TABLE node_enrollments_new (
			name         TEXT PRIMARY KEY,
			fingerprint  TEXT NOT NULL,
			csr_pem      TEXT NOT NULL,
			cert_pem     TEXT NOT NULL DEFAULT '',
			state        TEXT NOT NULL CHECK (state IN ('pending','approved','denied')),
			requested_at TEXT NOT NULL,
			decided_at   TEXT NOT NULL DEFAULT '',
			source       TEXT NOT NULL DEFAULT 'enrolled'
		) STRICT`,
		`INSERT INTO node_enrollments_new
		   (name, fingerprint, csr_pem, cert_pem, state, requested_at, decided_at, source)
		 SELECT name, fingerprint, csr_pem, cert_pem, state, requested_at, decided_at, source
		   FROM node_enrollments`,
		`DROP TABLE node_enrollments`,
		`ALTER TABLE node_enrollments_new RENAME TO node_enrollments`,

		`CREATE TABLE join_tokens_new (
			token_sha256   TEXT PRIMARY KEY,
			note           TEXT NOT NULL DEFAULT '',
			uses_remaining INTEGER NOT NULL,
			created_at     TEXT NOT NULL,
			expires_at     TEXT NOT NULL
		) STRICT`,
		`INSERT INTO join_tokens_new
		   (token_sha256, note, uses_remaining, created_at, expires_at)
		 SELECT token_sha256, note, uses_remaining, created_at, expires_at FROM join_tokens`,
		`DROP TABLE join_tokens`,
		`ALTER TABLE join_tokens_new RENAME TO join_tokens`,
	},
}

// A NODE CAN BE REVOKED WITHOUT ENUMERATING WHAT IT HOLDS.
//
// Revocation by serial reaches only credentials billet wrote down, and there are
// two ways for one to exist outside that set: a deployment upgraded from a
// version that did not record serials, and a name that was issued more than once
// before it did — the admission trail keeps one row per node and overwrites it,
// so the earlier certificate is unrecoverable.
//
// A CUTOFF NEEDS NO LIST. Revoking a node records the moment; any certificate
// for that name minted before it is refused on sight, whether or not billet has
// ever seen it. A replacement issued afterwards has a later NotBefore and works,
// which is what keeps this a revocation rather than a ban on the name.
var nodeRevocationMigration = migration{
	Version: 18,
	Name:    "node_revocations",
	Stmts: []string{
		`CREATE TABLE node_revocations (
			node           TEXT PRIMARY KEY,
			-- Every certificate for this node valid from before this instant is
			-- refused. Stored as the certificate's own NotBefore basis, so the
			-- comparison is against a fact of the credential rather than a clock.
			revoked_before TEXT NOT NULL,
			reason         TEXT NOT NULL DEFAULT '',
			revoked_at     TEXT NOT NULL
		) STRICT`,
	},
}

func init() {
	migrations = append(migrations,
		placementMigration, guestOSMigration, placementFactsMigration, requestIDMigration,
		providerListMigration, nodeSiteMigration, nodeLivenessMigration,
		certRevocationMigration, nodeEnrollmentMigration, joinTokenMigration,
		issuedCertMigration, quarantineMigration, strictTrustTablesMigration,
		nodeRevocationMigration)
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
func checkBookkeepingSchema(ctx context.Context, q Querier) error {
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
func readAppliedMigrations(ctx context.Context, q Querier) (map[int]appliedMigration, error) {
	rows, err := q.QueryContext(ctx,
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

		if err := refuseUnknownVersions(seen); err != nil {
			return err
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
