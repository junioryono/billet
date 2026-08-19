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
	"log/slog"
	"net/url"
	"os"
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

	// admin marks a handle belonging to a ONE-SHOT OPERATOR COMMAND, whether or
	// not it managed to take the directory lock. It decides how patient the
	// handle is: a command has a person waiting on it and gives up, a control
	// plane never does.
	admin bool

	// unlocked marks a handle that did NOT take the directory lock, because
	// something else holds it. Such a handle did not migrate and cannot assume
	// the schema it verified at open is still the one it is writing against, so
	// every transaction re-checks. See OpenAdmin and verifySchemaIn.
	//
	// SEPARATE FROM admin, and conflating them was a bug: an operator command on
	// a STOPPED server takes the lock, so a single flag made it a control plane
	// for the purposes of patience — and it would then retry forever against
	// another command that had beaten it to SQLite's writer slot, which is
	// exactly the hang the bound exists to prevent.
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

	db := &DB{w: w, r: r, lock: lock, admin: admin, unlocked: lock == nil}

	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	// Every failure here closes both pools and releases the lock. The cleanup
	// error is joined rather than dropped: a lock that failed to release turns
	// the next start into a confusing "another billet process holds this state
	// directory", and the reason needs to survive to say otherwise.
	if err := db.PingContext(startupCtx); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	// THE SCAN BELONGS TO THE PROCESS THAT WILL SCHEDULE AGAINST THIS LEDGER.
	//
	// quick_check reads the whole file, and job_history is unbounded, so its cost
	// grows with the deployment's age. Running it on every operator command put
	// that growing scan in front of `nodes approve`, `leases release --force` and
	// `check` — under the shared thirty-second startup budget, so a large or
	// loaded deployment could lose EVERY live administration command, including
	// the emergency one. A control plane opens the ledger once and is about to
	// make scheduling decisions against it; a command is neither.
	if !admin {
		if err := db.integrityCheck(startupCtx); err != nil {
			return nil, errors.Join(err, db.Close())
		}
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
//
// The integrity SCAN is deliberately not part of this. It is a whole-file read
// whose cost grows with job_history, and it answers a question only a control
// plane about to schedule against the ledger has to ask. See IntegrityCheck.
func (db *DB) PingContext(ctx context.Context) error {
	if err := db.w.PingContext(ctx); err != nil {
		return fmt.Errorf("ping state db: %w", err)
	}

	return db.verifyWriterPragmas(ctx)
}

// IntegrityCheck refuses to serve from a corrupt ledger.
//
// EXPORTED so `billet check` can ask for it explicitly, because that command
// exists to prove a deployment is sane and this is most of what that means.
// Nothing else should: it reads the whole file, and doing it on every operator
// command put a growing scan in front of `leases release --force`, which is the
// command an operator runs when capacity is already missing.
func (db *DB) IntegrityCheck(ctx context.Context) error { return db.integrityCheck(ctx) }

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
	tx, err := db.beginWrite(ctx)
	if err != nil {
		return err
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

// View runs fn inside a READ-ONLY transaction on the query-only pool.
//
// THE COMPANION TO Tx, AND NOT AN OPTIMISATION. Every write transaction now
// begins IMMEDIATE, which takes SQLite's single writer slot at BEGIN — so a
// read-only operation routed through Tx reserves the right to write while it
// scans, and can delay a scheduling decision in the control plane. That was
// harmless when one process wrote; it is not now that operator commands share
// the ledger, and `billet leases quarantined` scanning a large table is exactly
// the shape that would do it.
//
// The reader pool is query_only, so a write attempted in here fails rather than
// quietly succeeding on a connection nobody expected to mutate anything — the
// same reason Reader hands back a Querier rather than the pool.
//
// A TRANSACTION rather than bare queries, so a caller issuing several of them
// sees one consistent snapshot instead of rows from either side of a commit.
func (db *DB) View(ctx context.Context, fn func(Querier) error) error {
	// Deferred, deliberately: the reader takes no write lock, which is the whole
	// point, and nothing here can be promoted.
	tx, err := db.r.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin read tx: %w", err)
	}

	defer func() {
		//nolint:errcheck // a read transaction has nothing to lose on rollback.
		_ = tx.Rollback()
	}()

	// Re-checked for the same reason Tx does it, and it matters here too: a read
	// against a schema a newer billet has since rebuilt would report rows that no
	// longer mean what this binary thinks they mean.
	if db.unlocked {
		if err := verifySchemaIn(ctx, tx); err != nil {
			return err
		}
	}

	return fn(tx)
}

// busyRetryInterval paces re-attempts at starting a write transaction.
//
// Short, because it only ever runs while another process holds the write lock,
// and the point is to be waiting when that process lets go.
const busyRetryInterval = 50 * time.Millisecond

// stallWarnAfter is how long a write may wait for the lock before saying so, and
// stallWarnEvery paces the repeats.
//
// WAITING FOREVER IS THE DESIGN; WAITING SILENTLY IS NOT. A control plane never
// gives up, because an escrow error stops the listener and destroys every job on
// the host — but that means an operator command stalled inside an open
// transaction (a suspended shell, a debugger, a wedged disk) takes every
// scheduling write down with it, indefinitely, with nothing anywhere saying why.
// The plane just goes quiet.
//
// Before this package admitted a second writer that was unreachable. It is the
// new failure mode the capability brought, so it gets the one thing that makes it
// diagnosable.
const (
	stallWarnAfter = time.Second
	stallWarnEvery = 15 * time.Second
)

// onBusyRetry observes a writer about to wait out a busy BEGIN. Nil in
// production; a test sets it to synchronise on the state it needs.
var onBusyRetry func()

// adminBusyLimit bounds how long an OPERATOR COMMAND waits for the write lock
// before reporting that the control plane is busy.
//
// A var so a test can shorten it; nothing outside this package writes it. The
// control plane itself has no such bound — see beginWrite for why the two sides
// are deliberately asymmetric.
var adminBusyLimit = 15 * time.Second

// beginWrite starts a write transaction, WAITING OUT a writer in another process
// rather than failing.
//
// CONTENTION IS A RACE, NOT A BROKEN CONTRACT, and this codebase already draws
// that line: an assignment with no escrow behind it declines and carries on,
// while a scale-set response that cannot be true stops the listener. SQLITE_BUSY
// belongs firmly on the first side — it means somebody else is writing right
// now, which is ordinary once operator commands share the ledger.
//
// Treating it as fatal would be severe out of all proportion. Escrow's error
// reaches refillEscrow, which stops the listener, which cancels every other
// listener, whose shutdowns destroy every job on the host — so a `billet leases
// quarantined` that happened to scan for longer than busy_timeout would fail
// every build on the machine. Retrying costs a delay; not retrying costs the
// deployment.
//
// Bounded by the CALLER'S CONTEXT rather than by an attempt count, because there
// is no number of attempts that is right for both a poll loop and a one-shot
// command: each already carries a deadline that says how long it is willing to
// wait.
func (db *DB) beginWrite(ctx context.Context) (*sql.Tx, error) {
	// THE TWO CALLERS DESERVE OPPOSITE ANSWERS, which is why the bound is not a
	// single number. A control plane must never give up — stopping is the
	// catastrophe this retry exists to prevent — so it waits as long as its
	// context allows. An operator command has a HUMAN WAITING AT A TERMINAL, and
	// a command that hangs silently with no output is its own kind of failure, so
	// it gives up and says to try again.
	//
	// Not expressed as a derived context, deliberately: the transaction that
	// BeginTx returns stays bound to the context it was given, so cancelling one
	// here would roll the transaction back the moment this function returned.
	var errBusy error

	var deadline time.Time
	if db.admin {
		deadline = time.Now().Add(adminBusyLimit)
	}

	started := time.Now()
	warned := time.Time{}

	for attempt := 0; ; attempt++ {
		// CHECKED BEFORE STARTING, never only after. An attempt that begins inside
		// the bound can still block for busy_timeout, so testing afterwards let the
		// effective wait run past the number this promises. Refusing to START a
		// late attempt keeps the overshoot to one busy_timeout rather than
		// unbounded — and a transaction that IS won is never thrown away, because
		// having the lock is strictly better than reporting that we could not get
		// it.
		if attempt > 0 {
			if err := ctx.Err(); err != nil {
				return nil, fmt.Errorf("begin write tx: %w", err)
			}

			if !deadline.IsZero() && time.Now().After(deadline) {
				// IT DOES NOT PROMISE THAT NOTHING CHANGED, and an earlier draft did.
				// Several commands commit more than one transaction — `nodes revoke`
				// records each legacy serial before withdrawing them, `ca issue`
				// records the serial and the admission separately — so a later one
				// failing leaves the earlier ones committed. Claiming otherwise would
				// send an operator away believing a half-done command was a no-op.
				return nil, fmt.Errorf(
					"another process held this ledger's write lock for longer than %s, so this "+
						"command stopped rather than waiting silently. Anything it had already "+
						"committed stands; run it again in a moment: %w",
					adminBusyLimit, errBusy)
			}
		}

		tx, err := db.w.BeginTx(ctx, nil)
		if err == nil {
			return tx, nil
		}

		if !isBusy(err) {
			// ONLY AN INTERRUPT IS CANCELLATION WEARING THE DRIVER'S CLOTHES.
			//
			// modernc can interrupt a BEGIN and surface SQLITE_INTERRUPT rather
			// than a context error, so a caller testing for Canceled would see
			// nothing — that one code is translated.
			//
			// NOTHING ELSE IS, and the two rejected alternatives are why. Asking the
			// context first and returning its error threw away SQLITE_CORRUPT and
			// SQLITE_IOERR whenever cancellation raced the return. Joining the two
			// kept both identities structurally and still lost the fault in
			// practice: callers filter on errors.Is(err, context.Canceled) and treat
			// a match as a clean shutdown, so a joined error is discarded exactly
			// like a pure cancellation — by nodeplane's request handler and by the
			// server's own shutdown classifier.
			//
			// A storage fault must stay a storage fault. It is the actionable half.
			if isInterrupt(err) {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return nil, fmt.Errorf("begin write tx: %w", ctxErr)
				}
			}

			return nil, fmt.Errorf("begin write tx: %w", err)
		}

		errBusy = err

		// SAID OUT LOUD ONCE IT STOPS LOOKING LIKE CONTENTION AND STARTS LOOKING
		// LIKE A STALL. Rate-limited, because this runs on every writer and a wedged
		// holder would otherwise produce a line per retry.
		if waited := time.Since(started); waited > stallWarnAfter &&
			(warned.IsZero() || time.Since(warned) > stallWarnEvery) {
			warned = time.Now()

			// THE CONSEQUENCE DEPENDS ON WHO IS WAITING, so it is not asserted for
			// both. A stalled control plane really does have scheduling queued
			// behind it; a command waiting its turn proves only that something else
			// is writing, and telling an operator their `ca token` has stopped the
			// deployment would send them after the wrong thing.
			if db.admin {
				slog.Default().Warn("still waiting for the ledger's write lock; another billet "+
					"process is holding it", "waited", waited.Round(time.Second))
			} else {
				slog.Default().Warn("the control plane is still waiting for the ledger's write "+
					"lock, so scheduling writes are queued behind it; an operator command is "+
					"holding it", "waited", waited.Round(time.Second))
			}
		}

		// A TEST HOOK, nil in production. The cancellation branch below is only
		// reachable once a caller is genuinely waiting out a busy BEGIN, and a test
		// that cancels on a guess exercises it on some runs and not others — which
		// is indistinguishable from a test that does not exercise it at all.
		if onBusyRetry != nil {
			onBusyRetry()
		}

		// time.After would leak its timer until it fired, and forbidigo bans it
		// for exactly that reason.
		wait := time.NewTimer(busyRetryInterval)

		select {
		case <-ctx.Done():
			wait.Stop()

			// ctx.Err(), NOT the busy error that happened to be last. Cancellation
			// is what ended this wait, and a caller testing for context.Canceled or
			// DeadlineExceeded has to be able to see it — the guarantee two lines up
			// is worthless if this branch quietly returns something else.
			return nil, fmt.Errorf("begin write tx: %w", ctx.Err())
		case <-wait.C:
		}
	}
}

// isBusy reports whether an error is SQLite saying somebody else is writing.
//
// MATCHED ON THE CODE, never on the message: "database is locked" is the text
// for both plain SQLITE_BUSY and SQLITE_BUSY_SNAPSHOT, and a driver or locale
// that phrases it differently would silently turn a retry into a fatal error.
// The primary code is the low byte, so this covers the extended forms —
// BUSY_RECOVERY, BUSY_SNAPSHOT and BUSY_TIMEOUT — without listing them.
func isBusy(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}

	const sqliteBusy = 5

	return serr.Code()&0xff == sqliteBusy
}

// isInterrupt reports whether an error is SQLite saying the operation was
// interrupted, which is how a cancelled BEGIN can arrive.
//
// Narrow deliberately: this is the ONLY driver code translated into the caller's
// context error, because every other failure is a fact about the database that
// must survive being reported.
// A VAR SO A TEST CAN REACH THE BRANCH IT GUARDS. modernc's *sqlite.Error has
// unexported fields, so a code-9 error cannot be fabricated from here and the
// translation below it would otherwise be unreachable from any test — which is
// how it came to be written on an unverified premise in the first place. The
// same reason adminBusyLimit is a var; nothing outside this package writes
// either.
var isInterrupt = func(err error) bool {
	var serr *sqlite.Error
	if !errors.As(err, &serr) {
		return false
	}

	const sqliteInterrupt = 9

	return serr.Code()&0xff == sqliteInterrupt
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

// AN EC2 BUDGET IS CHARGED FOR WHAT THE NODE MAY BUY, not merely what a tier
// requested. The ordered shape list belongs to the node row because placement
// happens on the control plane, while requested_* stays immutable on the lease
// so a fallback can change the charged size without forgetting what must fit.
var ec2ShapeAccountingMigration = migration{
	Version: 19,
	Name:    "ec2_shape_accounting",
	Stmts: []string{
		`ALTER TABLE nodes ADD COLUMN ec2_shapes TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE leases ADD COLUMN requested_vcpu INTEGER NOT NULL DEFAULT 0 CHECK (requested_vcpu >= 0)`,
		`ALTER TABLE leases ADD COLUMN requested_memory INTEGER NOT NULL DEFAULT 0 CHECK (requested_memory >= 0)`,
		`ALTER TABLE leases ADD COLUMN instance_type TEXT NOT NULL DEFAULT ''`,
		`UPDATE leases SET requested_vcpu = vcpu, requested_memory = memory
		  WHERE requested_vcpu = 0 OR requested_memory = 0`,
	},
}

// Custody is durable operator-visible state even though the node's detailed
// tending record remains local. force_release is a request TO that holder: the
// node observes it through heartbeat, drops its local proof obligation, and
// terminalizes the lease itself rather than having the control plane release
// capacity underneath a process that still believes it owns it.
var custodyVisibilityMigration = migration{
	Version: 20,
	Name:    "custody_visibility",
	Stmts: []string{
		`ALTER TABLE leases ADD COLUMN held_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE leases ADD COLUMN force_release INTEGER NOT NULL DEFAULT 0 CHECK (force_release IN (0,1))`,
		`CREATE TABLE leases_new (
			id               TEXT PRIMARY KEY,
			tier             TEXT NOT NULL,
			node             TEXT REFERENCES nodes(name) ON DELETE SET NULL,
			phase            TEXT NOT NULL CHECK (phase IN
				('capacity','assigned','launching','online','busy','custody','teardown','quarantine','done','failed')),
			vcpu             INTEGER NOT NULL CHECK (vcpu > 0),
			memory           INTEGER NOT NULL CHECK (memory > 0),
			run_id           INTEGER,
			request_id       INTEGER,
			epoch            INTEGER NOT NULL DEFAULT 0 CHECK (epoch >= 0),
			created_at       TEXT NOT NULL,
			heartbeat_at     TEXT NOT NULL,
			expires_at       TEXT NOT NULL,
			target_node      TEXT,
			macos_slot       INTEGER NOT NULL DEFAULT 0 CHECK (macos_slot IN (0, 1)),
			guest_os         TEXT NOT NULL DEFAULT 'linux',
			provider         TEXT NOT NULL DEFAULT '',
			providers        TEXT NOT NULL DEFAULT '',
			chosen_provider  TEXT NOT NULL DEFAULT '',
			requested_vcpu   INTEGER NOT NULL DEFAULT 0 CHECK (requested_vcpu >= 0),
			requested_memory INTEGER NOT NULL DEFAULT 0 CHECK (requested_memory >= 0),
			instance_type    TEXT NOT NULL DEFAULT '',
			held_at          TEXT NOT NULL DEFAULT '',
			force_release    INTEGER NOT NULL DEFAULT 0 CHECK (force_release IN (0,1))
		) STRICT`,
		`INSERT INTO leases_new
		   (id, tier, node, phase, vcpu, memory, run_id, request_id, epoch, created_at,
		    heartbeat_at, expires_at, target_node, macos_slot, guest_os, provider,
		    providers, chosen_provider, requested_vcpu, requested_memory, instance_type,
		    held_at, force_release)
		 SELECT id, tier, node, phase, vcpu, memory, run_id, request_id, epoch, created_at,
		        heartbeat_at, expires_at, target_node, macos_slot, guest_os, provider,
		        providers, chosen_provider, requested_vcpu, requested_memory, instance_type,
		        held_at, force_release
		   FROM leases`,
		`DROP TABLE leases`,
		`ALTER TABLE leases_new RENAME TO leases`,
		`UPDATE leases SET held_at = heartbeat_at WHERE phase = 'quarantine'`,
		`CREATE INDEX leases_open_idx ON leases(phase) WHERE phase NOT IN ('done','failed')`,
		`CREATE INDEX leases_node_idx ON leases(node)`,
		`CREATE INDEX leases_expiry_idx ON leases(expires_at) WHERE phase NOT IN ('done','failed')`,
	},
}

// An external reclaim is known before the guest disappears. Record that fact on
// the live lease so a node restart between the warning and teardown cannot turn
// a known failed build into an unattributed completion.
var leaseFailureReasonMigration = migration{
	Version: 21,
	Name:    "lease_failure_reason",
	Stmts: []string{
		`ALTER TABLE leases ADD COLUMN failure_reason TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE job_history ADD COLUMN failure_reason TEXT NOT NULL DEFAULT ''`,
	},
}

// A completed scale-set message is acknowledged only after its authoritative
// result can survive the control plane stopping before node teardown succeeds.
var pendingCompletionsMigration = migration{
	Version: 22,
	Name:    "pending_completions",
	Stmts: []string{
		`CREATE TABLE pending_completions (
			tier       TEXT NOT NULL CHECK (length(trim(tier)) > 0),
			request_id INTEGER NOT NULL CHECK (request_id > 0),
			run_id     INTEGER NOT NULL DEFAULT 0 CHECK (run_id >= 0),
			result     TEXT NOT NULL CHECK (length(trim(result)) > 0),
			PRIMARY KEY (tier, request_id)
		) STRICT`,
	},
}

// A result alone can replay node teardown, but it cannot return capacity when
// the control plane stops after teardown succeeds and before the lease release
// settles. Keep the fenced lease identity beside the result so restart recovery
// can finish both halves of completion in their required order.
var pendingCompletionLeaseMigration = migration{
	Version: 23,
	Name:    "pending_completion_lease",
	Stmts: []string{
		`ALTER TABLE pending_completions ADD COLUMN lease_id TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE pending_completions ADD COLUMN lease_epoch INTEGER NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0)`,
		`ALTER TABLE pending_completions ADD COLUMN outcome TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','done','failed'))`,
		`ALTER TABLE pending_completions ADD COLUMN release_only INTEGER NOT NULL DEFAULT 0 CHECK (release_only IN (0,1))`,
	},
}

// Completion recovery needs both the bound host and a monotonic delivery phase.
// The host keeps restart reconciliation from treating an unrelated live fleet as
// proof of absence. The message id distinguishes a redelivery from a later job
// that reuses GitHub's request id, and retired is the durable tombstone that makes
// a failed row deletion harmless.
var pendingCompletionRecoveryMigration = migration{
	Version: 24,
	Name:    "pending_completion_recovery",
	Stmts: []string{
		`ALTER TABLE pending_completions ADD COLUMN lease_node TEXT NOT NULL DEFAULT ''`,
		`UPDATE pending_completions
		    SET lease_node = COALESCE((SELECT COALESCE(node, target_node, '') FROM leases
		                               WHERE leases.id = pending_completions.lease_id), '')
		  WHERE lease_id != ''`,
		`ALTER TABLE pending_completions ADD COLUMN message_id INTEGER NOT NULL DEFAULT 0 CHECK (message_id >= 0)`,
		`ALTER TABLE pending_completions ADD COLUMN retired INTEGER NOT NULL DEFAULT 0 CHECK (retired IN (0,1))`,
	},
}

// A retired completion must outlive its source message, but not every later
// restart. Persisting acknowledgement lets either ordering of settlement and
// source deletion remove the tombstone only after both facts are durable.
var pendingCompletionAcknowledgementMigration = migration{
	Version: 25,
	Name:    "pending_completion_acknowledgement",
	Stmts: []string{
		`ALTER TABLE pending_completions ADD COLUMN acknowledged INTEGER NOT NULL DEFAULT 0 CHECK (acknowledged IN (0,1))`,
	},
}

// GitHub assigns runnerRequestId 0 when it sends JobAssigned directly instead
// of first offering JobAvailable. A durable negative identity, keyed by jobId,
// keeps those jobs distinct without colliding with GitHub's positive request ids.
// Pending completions accept either namespace but continue to refuse zero, which
// remains the unusable wire value rather than a scheduler identity.
var directAssignmentIdentityMigration = migration{
	Version: 26,
	Name:    "direct_assignment_identity",
	Stmts: []string{
		`CREATE TABLE job_identities (
			job_id      TEXT PRIMARY KEY CHECK (length(trim(job_id)) > 0),
			internal_id INTEGER NOT NULL UNIQUE CHECK (internal_id < 0)
		) STRICT`,
		`CREATE TABLE pending_completions_new (
			tier         TEXT NOT NULL CHECK (length(trim(tier)) > 0),
			request_id   INTEGER NOT NULL CHECK (request_id != 0),
			run_id       INTEGER NOT NULL DEFAULT 0 CHECK (run_id >= 0),
			result       TEXT NOT NULL CHECK (length(trim(result)) > 0),
			lease_id     TEXT NOT NULL DEFAULT '',
			lease_epoch  INTEGER NOT NULL DEFAULT 0 CHECK (lease_epoch >= 0),
			outcome      TEXT NOT NULL DEFAULT '' CHECK (outcome IN ('','done','failed')),
			release_only INTEGER NOT NULL DEFAULT 0 CHECK (release_only IN (0,1)),
			lease_node   TEXT NOT NULL DEFAULT '',
			message_id   INTEGER NOT NULL DEFAULT 0 CHECK (message_id >= 0),
			retired      INTEGER NOT NULL DEFAULT 0 CHECK (retired IN (0,1)),
			acknowledged INTEGER NOT NULL DEFAULT 0 CHECK (acknowledged IN (0,1)),
			PRIMARY KEY (tier, request_id)
		) STRICT`,
		`INSERT INTO pending_completions_new
		   (tier, request_id, run_id, result, lease_id, lease_epoch, outcome,
		    release_only, lease_node, message_id, retired, acknowledged)
		 SELECT tier, request_id, run_id, result, lease_id, lease_epoch, outcome,
		        release_only, lease_node, message_id, retired, acknowledged
		   FROM pending_completions`,
		`DROP TABLE pending_completions`,
		`ALTER TABLE pending_completions_new RENAME TO pending_completions`,
	},
}

func init() {
	migrations = append(migrations,
		placementMigration, guestOSMigration, placementFactsMigration, requestIDMigration,
		providerListMigration, nodeSiteMigration, nodeLivenessMigration,
		certRevocationMigration, nodeEnrollmentMigration, joinTokenMigration,
		issuedCertMigration, quarantineMigration, strictTrustTablesMigration,
		nodeRevocationMigration, ec2ShapeAccountingMigration, custodyVisibilityMigration,
		leaseFailureReasonMigration, pendingCompletionsMigration, pendingCompletionLeaseMigration,
		pendingCompletionRecoveryMigration, pendingCompletionAcknowledgementMigration,
		directAssignmentIdentityMigration)
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
