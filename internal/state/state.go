// Package state is billet's durable control-plane store.
//
// The default store is SQLite, deliberately. A billet deployment has one
// authoritative server process, and the data it keeps — the capacity ledger, job
// history, and advisory pointers to node-local volumes — is small and hot.
// SQLite gives that ACID semantics with no daemon to operate.
//
// THE ENGINE IS A SEAM AND THE INVARIANTS ARE NOT. What differs between engines
// lives behind the backend interface in backend.go; everything below is billet's
// own and is enforced here, once, for all of them:
//
//   - ONE authoritative process. An exclusive lock on the state directory is held
//     for the lifetime of DB, so a second server exits immediately instead of
//     taking turns writing conflicting scheduling decisions. A database's own
//     ability to serialize writes prevents simultaneous writes; it does not
//     prevent two control planes.
//   - ONE writer within the process. Mutations go through a single-connection
//     pool so an allocation decision serializes. Reads go through a separate pool
//     exposed only as a query interface, so a caller cannot write through it by
//     accident.
//   - Durability actually verified. The settings that were asked for are read
//     back and the store fails closed on any mismatch — on SQLite that is what
//     catches a state directory on a network filesystem, where WAL cannot work at
//     all because its shared-memory coordination assumes a single host.
//   - Contention is a RACE, not a verdict, and is retried rather than returned,
//     with opposite patience for a control plane and for an operator command.
//
// What this store is NOT: authoritative for cache generation pointers. Those live
// on the node that owns the underlying volume, because a commit here cannot be
// made atomic with a snapshot on a remote machine. What is kept here is advisory
// metadata used for scheduling affinity.
package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ErrConflict is returned when a compare-and-swap loses. Callers should re-read
// and decide, never blindly retry — a lost cache publication is correct
// behaviour, not a transient failure.
var ErrConflict = errors.New("state: compare-and-swap conflict")

// ErrLocked means another billet process already owns this state directory.
var ErrLocked = errors.New("state: another billet process holds this state directory")

// ErrMaintenance means a host upgrade fenced the ledger against operator traffic.
var ErrMaintenance = errors.New("state: the ledger is fenced for host maintenance")

const maintenanceFile = "billet.maintenance"

// Querier is the read surface. It is deliberately narrower than *sql.DB: handing
// out the pool would let any caller issue writes on a connection that is supposed
// to be read-only, which is exactly the invariant this package exists to hold.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB wraps two connection pools over one ledger: a single-connection writer so
// mutations serialize, and a read-only pool so status reads never queue behind
// an allocation.
type DB struct {
	w    *sql.DB
	r    *sql.DB
	lock *dirLock

	// backend is the engine underneath, and everything in this file that is not
	// about billet's own invariants goes through it. See backend.go for what is
	// deliberately NOT behind it.
	backend backend

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

	// revalidate marks a handle that must re-check the schema inside every
	// transaction, because something else may migrate underneath it.
	//
	// SEPARATE FROM unlocked, AND THE TWO ARE ONLY EQUAL WHILE THE LEDGER IS A
	// FILE. What decides this is whether this handle holds the CONTROLLER
	// EXCLUSION for its lifetime, and on SQLite that is the directory lock — so
	// the two answers coincide and did not need telling apart. On a ledger
	// another machine can reach they come apart in both directions: a command on
	// a second host takes its own directory lock and holds no exclusion at all,
	// and one that took the exclusion only long enough to migrate has given it
	// back by the time it starts writing. Deriving this from `unlocked` there
	// would let a command write against a schema a control plane had migrated
	// underneath it since it looked.
	//
	// ATOMIC because a standby clears it at PROMOTION rather than at open, which
	// is after the handle has been read from — see ClaimController.
	revalidate atomic.Bool

	// standby marks a control plane that is waiting to become the controller, and
	// while it is set every write transaction is refused.
	//
	// THE FENCE PROTECTS A FORMER LEADER, NOT A NEVER-LEADER, and that asymmetry
	// is why this exists at all. checkLeadership exempts a handle whose
	// claimedEpoch is zero — deliberately, because migrate runs before any claim
	// and OpenAdmin holds none — so a standby, which by definition has never
	// claimed, would write completely unfenced.
	//
	// STRUCTURAL RATHER THAN A RULE THE RUNTIME REMEMBERS. The alternative is
	// auditing which goroutines a standby starts, which is a fence with a hole per
	// goroutine and a hole per future edit. This is the one choke point every
	// write crosses, which is the same argument readOnlyDBTX makes one seam over.
	//
	// READS ARE NOT REFUSED. A standby has to be able to see the ledger to report
	// on itself and to decide it could promote; what it may not do is change it.
	standby atomic.Bool

	stateDir          string
	maintenanceBypass bool

	// runningRelease is the billet this handle was opened by, and recordsRelease
	// whether this handle is one that records it: the control plane proper, at
	// the moment it claims. See ClaimController.
	runningRelease string
	recordsRelease bool

	// claimedEpoch is the controller generation this process claimed, or 0 for a
	// handle that is not a controller — a migration, an operator command, the
	// upgrade probe. It is what every write transaction is fenced against.
	//
	// ATOMIC BECAUSE Tx IS CONCURRENT WITH NOTHING ELSE IN PARTICULAR. It is
	// written once, by ClaimController, before the listeners exist; the reads come
	// from every goroutine that writes. A mutex would serialize the fence check
	// behind itself for no benefit.
	claimedEpoch atomic.Int64

	// fenced latches once a write has been refused because a successor claimed.
	// See LeadershipLost for why it never clears and what reads it.
	fenced atomic.Bool

	// fencedCh is closed exactly once, at the same instant fenced is set.
	//
	// A CHANNEL AS WELL AS A FLAG, BECAUSE THE TWO CONSUMERS ASK DIFFERENT
	// QUESTIONS. A teardown that is already unwinding asks "am I fenced" and a
	// boolean answers it. A control plane that is running normally has to be
	// WOKEN — its listeners are inside a long poll, its heartbeats keep the lease
	// they were told nothing about, and its cleanup loop is on its own clock, so
	// nothing on any of those paths would ever get round to asking. Polling a flag
	// would be a second clock deciding how long a replaced controller keeps
	// touching the fleet.
	//
	// Created by openDir, so a handle built by hand in a test has a nil channel —
	// which blocks forever in a select, and is the right answer for a handle that
	// can never be fenced because it never claimed.
	fencedCh   chan struct{}
	fencedOnce sync.Once
}

// Open prepares the state directory, takes the exclusive process lock, opens the
// database, and verifies that the durability pragmas actually took effect.
//
// The caller's context bounds startup only; it does not own the returned DB.
func Open(ctx context.Context, stateDir string, opts ...OpenOption) (*DB, error) {
	return openDir(ctx, stateDir, newSQLiteBackend(stateDir), openMode{}.with(opts))
}

// OpenMaintenance opens the control-plane store for a quiescent upgrade probe.
// It bypasses a host-upgrade fence without admitting operator or workload writes.
func OpenMaintenance(ctx context.Context, stateDir string, opts ...OpenOption) (*DB, error) {
	return openDir(ctx, stateDir, newSQLiteBackend(stateDir),
		openMode{maintenanceProbe: true}.with(opts))
}

// openMode is what kind of process is opening the ledger.
//
// A STRUCT RATHER THAN A LIST OF BOOLEANS, because the answers are not
// independent and a call site passing four bare `false`s says nothing about
// which question it answered. Every field is a different party with different
// rights, and the zero value is the control plane — the one that may do
// everything.
type openMode struct {
	// admin marks a ONE-SHOT OPERATOR COMMAND rather than a control plane. It
	// may proceed without the exclusive directory lock when a control plane
	// already holds it, and it may migrate only a ledger nothing else holds.
	admin bool

	// maintenanceProbe marks the quiescent upgrade probe, the one caller
	// entitled to cross a host-upgrade fence.
	maintenanceProbe bool

	// standby marks a control plane that is WAITING to become the controller.
	//
	// IT HOLDS NO EXCLUSION, SO IT MAY NOT WRITE, and the refusal is structural
	// rather than a rule the standby runtime has to remember: checkLeadership
	// exempts a handle that never claimed — deliberately, so migrations and
	// operator commands work — which means the fence protects a FORMER leader and
	// not a NEVER-leader. A standby's only protection is that Tx refuses it.
	standby bool

	// release is the billet opening the ledger, for the release watermark, or
	// empty for a caller that named none and gets neither the check nor the
	// record. See WithRunningRelease.
	release string
}

// records reports whether a handle opened in this mode is the one that moves the
// release watermark forward, which it does in ClaimController and never at open.
//
// THE CONTROL PLANE PROPER AND NOTHING ELSE. An operator command may be a newer
// binary run from a laptop; a standby has not earned a write; the upgrade probe
// proves a candidate can open what it inherited and leaves the recording to the
// service that then serves it, so a rollback that never reaches that service has
// nothing to undo.
func (m openMode) records() bool {
	return !m.admin && !m.maintenanceProbe && !m.standby
}

// with applies the caller's options to a mode.
func (m openMode) with(opts []OpenOption) openMode {
	for _, opt := range opts {
		opt(&m)
	}

	return m
}

// openDir is the shared body of every entry point.
func openDir(
	ctx context.Context, stateDir string, be backend, mode openMode,
) (*DB, error) {
	admin, maintenanceProbe := mode.admin, mode.maintenanceProbe
	// FIRST, so a binary that cannot read its own migration set leaves no
	// directory, no lock file and no database behind. See timeline.require for
	// why both branches at the bottom of this function would otherwise agree that
	// an empty set is a healthy schema.
	if err := be.timeline().require(); err != nil {
		return nil, err
	}

	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("create state dir %s: %w", stateDir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, and this directory will
	// hold the mTLS CA key. Tighten it rather than inheriting whatever was there.
	if err := os.Chmod(stateDir, 0o700); err != nil {
		return nil, fmt.Errorf("tighten state dir %s: %w", stateDir, err)
	}

	// ONLY THE TYPED ENTRY crosses the fence. This used to also honor
	// BILLET_MAINTENANCE=1, which made fence-crossing an ambient property of the
	// process environment: any command that inherited the variable — a shell
	// spawned from the upgrade transaction, a cron job configured with it once
	// and forgotten — silently wrote through a fence that exists to stop
	// exactly that. Authorization must come from code that KNOWS it is the
	// quiescent probe, which is what OpenMaintenance is for; `billet check
	// --maintenance-probe` is its one caller.
	maintenanceBypass := maintenanceProbe
	if !maintenanceBypass {
		if err := refuseMaintenance(stateDir); err != nil {
			return nil, err
		}
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

	pools, err := be.dataSources()
	if err != nil {
		return nil, errors.Join(err, lock.release())
	}

	w, err := sql.Open(be.driverName(), pools.writer)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open state db: %w", err), lock.release())
	}
	// THE SINGLE MOST IMPORTANT LINE IN THIS FILE, and deliberately not a
	// backend's to choose. One writer within the process is what makes an
	// allocation decision — read current usage, decide, record it — serialize,
	// and an engine that could widen this is an engine that could lose that.
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)

	r, err := sql.Open(be.driverName(), pools.reader)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("open state db (reader): %w", err), w.Close(), lock.release())
	}
	r.SetMaxOpenConns(4)

	db := &DB{
		w:                 w,
		r:                 r,
		lock:              lock,
		backend:           be,
		admin:             admin,
		unlocked:          lock == nil,
		stateDir:          stateDir,
		maintenanceBypass: maintenanceBypass,
		runningRelease:    mode.release,
		recordsRelease:    mode.records(),
		fencedCh:          make(chan struct{}),
	}

	db.revalidate.Store(lock == nil)
	db.standby.Store(mode.standby)

	startupCtx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	// Every failure here closes both pools and releases the lock. The cleanup
	// error is joined rather than dropped: a lock that failed to release turns
	// the next start into a confusing "another billet process holds this state
	// directory", and the reason needs to survive to say otherwise.
	if err := db.PingContext(startupCtx); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	// AFTER THE CONNECTION IS PROVED AND BEFORE ANYTHING USES IT. What a backend
	// learns here is load-bearing for the transactions below — on PostgreSQL it
	// is the key every writer serializes on — so a failure has to stop the open
	// rather than leave a handle whose exclusion is keyed on zero.
	if err := be.prepare(startupCtx, db.w); err != nil {
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
		if err := db.backend.integrityCheck(startupCtx, db.w); err != nil {
			return nil, errors.Join(err, db.Close())
		}
	}

	// MIGRATING IS AUTHORISED BY THE CONTROLLER EXCLUSION, AND THE DIRECTORY LOCK
	// IS ONLY THAT EXCLUSION WHILE THE LEDGER IS A FILE.
	//
	// Upgrading a schema underneath a process that is mid-transaction against it
	// is the one thing nothing here may do, and the rule used to be "the lock
	// holder migrates". That is exactly right for SQLite, where the ledger is
	// reachable from one machine and holding its directory excludes every
	// controller that could open it. On a ledger another machine can reach it
	// authorises the wrong process: a second host takes its OWN directory's lock
	// happily, so a server about to be refused as a second controller — and an
	// operator command that is not a controller at all — would migrate the shared
	// schema on the way to finding that out.
	//
	// So the exclusion is taken FIRST, and a handle that cannot have it verifies
	// instead. On SQLite claimController takes nothing and answers from the lock
	// already held, so this branch is what already happened; on PostgreSQL it is
	// the session advisory lock, which needs no schema to take — which is what
	// makes this ordering possible at all, since only the RECORD half of a claim
	// needs a table to write to.
	// A STANDBY TAKES NOTHING AND MIGRATES NOTHING, because it is not the
	// controller yet and the whole point of it is to be waiting rather than
	// competing. What it does check is that the schema is not AHEAD of this
	// binary: a process that does not know every applied version could not serve
	// this deployment if it were promoted, and finding that out at startup is
	// worth far more than finding it out at the failover.
	//
	// A SCHEMA BEHIND IT IS FINE AND IS NOT AN ERROR, which is the difference
	// between this and what an operator command asks. A newer standby waiting
	// beside an older leader is the whole shape of a follower-first upgrade, and
	// refusing it would make staging one impossible; the migration is the claim's
	// right, and it happens at promotion.
	if mode.standby {
		if err := verifySchemaNotAhead(startupCtx, be, db.Reader()); err != nil {
			return nil, errors.Join(err, db.Close())
		}

		db.revalidate.Store(true)

		// A STANDBY IS REFUSED AS A DOWNGRADE TOO, and records nothing. What it
		// would be promoted into is a ledger a newer release has served; that it
		// waits rather than serves changes nothing about which binary is older.
		if err := db.enforceReleaseWatermark(startupCtx, mode.release, false); err != nil {
			return nil, errors.Join(err, db.Close())
		}

		return db, nil
	}

	if lock != nil {
		if err := be.claimController(startupCtx, db); err != nil {
			// AN OPERATOR COMMAND IS NOT A SECOND CONTROLLER, so a held exclusion
			// puts it on the same footing as one that could not take the directory
			// lock: verify, re-verify inside every transaction, and refuse only if
			// it would have had to write. A control plane is refused outright.
			if !admin || !errors.Is(err, ErrControllerHeld) {
				return nil, errors.Join(err, db.Close())
			}

			db.revalidate.Store(true)
		}
	}

	if db.revalidate.Load() {
		if err := db.verifySchema(startupCtx); err != nil {
			return nil, errors.Join(err, db.Close())
		}

		if err := db.enforceReleaseWatermark(startupCtx, mode.release, false); err != nil {
			return nil, errors.Join(err, db.Close())
		}

		return db, nil
	}

	if err := db.migrate(startupCtx); err != nil {
		return nil, errors.Join(fmt.Errorf("migrate state db: %w", err), db.Close())
	}

	// AFTER THE MIGRATION, BECAUSE THE TABLE IT READS ARRIVED IN ONE. A proved
	// downgrade is refused here whichever handle this is. A proved upgrade is
	// recorded by nobody at open: the control plane proper records it in
	// ClaimController, after the deployment binding has agreed these rows are
	// its own. Recorded here, a newer binary pointed at another deployment's
	// ledger would raise that ledger's mark on the way to being refused, and
	// fence its real controller out of its own restart. See openMode.records.
	if err := db.enforceReleaseWatermark(startupCtx, mode.release, false); err != nil {
		return nil, errors.Join(err, db.Close())
	}

	// AND AN OPERATOR COMMAND GIVES THE EXCLUSION BACK THE MOMENT IT HAS FINISHED
	// MIGRATING, on a backend where holding it means something.
	//
	// It is taken because SOMEBODY has to create the schema on a deployment whose
	// server has never started — `billet check` and `billet ca issue` both run
	// before the first `billet server` — and it is released because a command is
	// not the controller and must not spend its lifetime refusing one. Held, it
	// would turn every `billet local backup` into a window in which a control
	// plane cannot start, and the refusal it met would be ErrControllerHeld
	// quoting the last recorded holder: a diagnostic naming a row rather than the
	// process actually in the way, which is worse than no diagnostic.
	//
	// Releasing it is what makes revalidate true from here on: once it is gone a
	// control plane can start and migrate, so nothing this handle verified at open
	// is still guaranteed at the moment it writes.
	if admin && be.sharedLedger() {
		if err := be.releaseController(); err != nil {
			return nil, errors.Join(
				fmt.Errorf("state: release the controller exclusion after migrating: %w", err),
				db.Close())
		}

		db.revalidate.Store(true)
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

	return db.backend.verifyDurability(ctx, db.w)
}

// IntegrityCheck refuses to serve from a corrupt ledger.
//
// EXPORTED so `billet check` can ask for it explicitly, because that command
// exists to prove a deployment is sane and this is most of what that means.
// Nothing else should: it reads the whole file, and doing it on every operator
// command put a growing scan in front of `leases release --force`, which is the
// command an operator runs when capacity is already missing.
func (db *DB) IntegrityCheck(ctx context.Context) error {
	return db.backend.integrityCheck(ctx, db.w)
}

// Close closes both pools, then releases the controller claim and the process
// lock.
//
// THE CLAIM GOES LAST, AND THE ORDER IS THE SAFETY CONTENT. Releasing it first
// lets a REPLACEMENT claim the deployment while this process's writer pool is
// still finishing a transaction — and the caller of that transaction may still
// go on to make the dispatch it recorded. Two controllers, briefly, produced by
// the shutdown of one.
//
// The first version of this had them the other way round, on the argument that
// the claim is the one thing that outlives the handle. It does, which is why it
// must be released — but AFTER the writes it was excluding have stopped.
//
// errors.Join evaluates its arguments in order, so the sequence is the source
// order and not an accident of how the results are combined.
//
// WHAT THIS STILL DOES NOT COVER is a caller that has already read a lease and
// is about to act on it outside any transaction. Closing the store cannot reach
// that; the scheduler's own shutdown is what does, and it runs first.
func (db *DB) Close() error {
	return errors.Join(db.closeDBs(), db.backend.releaseController(), db.lock.release())
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
	if err := db.checkMaintenance(); err != nil {
		return err
	}

	// A STANDBY WRITES NOTHING, AND THE REFUSAL IS HERE BECAUSE THIS IS WHERE
	// EVERY WRITE GOES. The fence below protects a controller that has been
	// REPLACED; it exempts a handle that never claimed, so a standby would be
	// unfenced by construction. Refused before the transaction is even begun, so
	// nothing takes the writer slot on the way to being told no.
	if db.standby.Load() {
		return ErrStandby
	}

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

	// RE-CHECKED INSIDE THE TRANSACTION, for a handle that does not hold the
	// controller exclusion. The check at open time is only true of the control
	// plane that was running then; this one is true of the schema this
	// transaction is actually writing against, because BEGIN IMMEDIATE already
	// holds the write lock a migration would need. The release watermark is
	// re-read for the same reason: a newer control plane can have claimed and
	// recorded since this handle opened, and two releases that share a schema
	// would pass the schema check alone.
	if db.revalidate.Load() {
		if err := db.checkMaintenance(); err != nil {
			return err
		}
		if err := verifySchemaIn(ctx, db.backend, tx); err != nil {
			return db.asCancellation(ctx, err)
		}
		if err := db.checkReleaseWatermarkIn(ctx, tx); err != nil {
			return err
		}
	}

	// THE FENCE, AND ITS POSITION IS THE WHOLE OF IT.
	//
	// Inside the transaction, so the answer cannot change before the commit — the
	// write lock this BEGIN already holds is the same one a successor's claim has
	// to take. Before fn, so a fenced controller's callback never runs at all: a
	// check after it would let a scheduling decision be made and its side effects
	// escape through whatever the caller keeps, with only the COMMIT refused.
	//
	// See checkLeadership. It costs one single-row read per write transaction and
	// nothing at all on a handle that never claimed.
	if err := db.checkLeadership(ctx, tx); err != nil {
		return db.asCancellation(ctx, err)
	}

	if err := fn(tx); err != nil {
		return db.asCancellation(ctx, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit write tx: %w", db.asCancellation(ctx, err))
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
	if err := db.checkMaintenance(); err != nil {
		return err
	}

	// Deferred, deliberately: the reader takes no write lock, which is the whole
	// point, and nothing here can be promoted.
	tx, err := db.r.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin read tx: %w", db.asCancellation(ctx, err))
	}

	defer func() {
		//nolint:errcheck // a read transaction has nothing to lose on rollback.
		_ = tx.Rollback()
	}()

	// Re-checked for the same reason Tx does it, and it matters here too: a read
	// against a schema a newer billet has since rebuilt would report rows that no
	// longer mean what this binary thinks they mean.
	if db.revalidate.Load() {
		if err := db.checkMaintenance(); err != nil {
			return err
		}
		if err := verifySchemaIn(ctx, db.backend, tx); err != nil {
			return db.asCancellation(ctx, err)
		}
	}

	return db.asCancellation(ctx, fn(tx))
}

// asCancellation substitutes the caller's context error for a driver's own
// spelling of "this was interrupted", and does nothing whatever else.
//
// ONLY AN INTERRUPT IS CANCELLATION WEARING THE DRIVER'S CLOTHES. modernc can
// interrupt a BEGIN and surface SQLITE_INTERRUPT rather than a context error,
// and PostgreSQL answers a cancelled statement with SQLSTATE 57014 — so a caller
// testing for context.Canceled would see nothing. That one code per engine is
// translated.
//
// NOTHING ELSE IS, and the two rejected alternatives are why. Asking the context
// first and returning its error threw away SQLITE_CORRUPT and SQLITE_IOERR
// whenever cancellation raced the return. Joining the two kept both identities
// structurally and still lost the fault in practice: callers filter on
// errors.Is(err, context.Canceled) and treat a match as a clean shutdown, so a
// joined error is discarded exactly like a pure cancellation — by nodeplane's
// request handler and by the server's own shutdown classifier.
//
// A storage fault must stay a storage fault. It is the actionable half.
//
// IT IS ONE FUNCTION BECAUSE IT WAS ONE BEGIN, AND THAT WAS THE BUG. The rule
// lived inline in beginWrite, so `View`'s begin did not have it: MEASURED in CI,
// the control plane's startup capacity refresh returned "begin read tx:
// interrupted (9)" from a listener whose context had just been cancelled,
// `onlyCancellation` correctly declined to call SQLITE_INTERRUPT a cancellation,
// and a deliberate `systemctl stop` exited non-zero — the clean-stop rule, broken
// through the one begin that had not been taught the driver's spelling. A BEGIN
// is also not the only place it arrives: PostgreSQL cancels the STATEMENT, so
// 57014 reaches the caller from a query inside fn, where nothing was translating
// it at all. Every error leaving Tx and View goes through here.
//
// A CALLER'S OWN ERROR IS SAFE HERE because the classifier matches a driver type
// or a SQLSTATE, never a message or a sentinel — ErrConflict, ErrLeadershipLost
// and every business failure pass through untouched.
//
// THE TEXT SURVIVES AND THE IDENTITY DOES NOT, which is the whole distinction the
// rejected errors.Join blurred. What made a join wrong is that the driver error
// stayed structurally discoverable, so `errors.Is` still matched it and every
// filter still discarded the result; what a bare substitution threw away was the
// only record of WHICH operation was in flight — a cancelled migration became
// "migrate state db: context deadline exceeded" and stopped naming the migration.
// Rendering the original with %s keeps the account and gives up the identity, so
// errors.Is answers exactly one thing: cancellation.
//
// A READ THAT DOES NOT GO THROUGH View OWES THIS CALL ITSELF. DB.Reader hands out
// a Querier with no transaction around it — see ScaleSets, the one production
// caller — so nothing on that path passes through here unless it asks.
func (db *DB) asCancellation(ctx context.Context, err error) error {
	if err == nil || !db.backend.isCancellation(err) {
		return err
	}

	ctxErr := ctx.Err()
	if ctxErr == nil {
		return err
	}

	// err.Error() RATHER THAN err, AND THAT IS THE POINT RATHER THAN A LINT
	// WORKAROUND. Passing the error would wrap it, which restores exactly the
	// structural discoverability the errors.Join shape was rejected for; taking
	// its TEXT keeps the account of what was in flight and leaves errors.Is
	// answering one thing.
	return fmt.Errorf("%s: %w", err.Error(), ctxErr)
}

func (db *DB) checkMaintenance() error {
	if db == nil || db.maintenanceBypass {
		return nil
	}

	return refuseMaintenance(db.stateDir)
}

func refuseMaintenance(stateDir string) error {
	_, err := os.Lstat(filepath.Join(stateDir, maintenanceFile))
	switch {
	case err == nil:
		return fmt.Errorf("%w in %s; wait for the transactional host upgrade to finish before running an operator command",
			ErrMaintenance, stateDir)
	case errors.Is(err, os.ErrNotExist):
		return nil
	default:
		return fmt.Errorf("inspect the maintenance fence in %s: %w", stateDir, err)
	}
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
			// TAKING THE WRITE LOCK IS PART OF BEGINNING, on every engine, and it
			// belongs INSIDE this loop rather than after it. SQLite has already
			// done it — _txlock=immediate — and has nothing to do here; an engine
			// that has not must be able to fail the way a busy BEGIN fails, and be
			// retried on the same terms, or its contention would reach the caller
			// as a scheduling error rather than as a wait.
			//
			// THE TRANSACTION IS ROLLED BACK BEFORE RETRYING, or each attempt would
			// leak one and the pool is a single connection: the second attempt
			// would wait for a transaction this loop is still holding open.
			if err = db.backend.beginWrite(ctx, tx); err == nil {
				return tx, nil
			}

			//nolint:errcheck // the transaction never did anything; err is what matters.
			_ = tx.Rollback()
		}

		if !db.backend.isContention(err) {
			// See asCancellation for why exactly one driver code is translated
			// here and nothing else is.
			return nil, fmt.Errorf("begin write tx: %w", db.asCancellation(ctx, err))
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

// appliedMigration is the bookkeeping row for one already-applied migration.
type appliedMigration struct {
	name     string
	checksum string
}

// readAppliedMigrations keys the bookkeeping rows by version, which is what both
// the migrator and the schema verifier ask about.
//
// It takes ReadOps rather than a transaction so the same read serves all three
// callers: migrate, inside the write transaction that is about to apply the rest
// of the set; verifySchemaIn, on a handle that must not write; and
// PeekMigrations, on a bare connection over a ledger file nobody has opened.
func readAppliedMigrations(ctx context.Context, q ReadOps) (map[int]appliedMigration, error) {
	rows, err := q.ListAppliedMigrations(ctx)
	if err != nil {
		// Propagated, not swallowed. A permission, corruption, or locking error must
		// not be mistaken for an empty database and answered by recreating the schema.
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}

	seen := make(map[int]appliedMigration, len(rows))

	for _, r := range rows {
		seen[int(r.Version)] = appliedMigration{name: r.Name, checksum: r.Checksum}
	}

	return seen, nil
}

func (db *DB) migrate(ctx context.Context) error {
	return db.Tx(ctx, func(tx *sql.Tx) error {
		// Bootstrapping is idempotent and lives outside the versioned set, so the
		// bookkeeping table's existence is never itself a migration whose absence
		// has to be inferred from a failed query.
		//billet:ignore rawsql // schema, not a query: this creates the table sqlc is told about
		if _, err := tx.ExecContext(ctx, db.backend.bootstrapSchema()); err != nil {
			return fmt.Errorf("bootstrap schema_migrations: %w", err)
		}
		// IF NOT EXISTS leaves an existing table alone, including one written by a
		// billet whose bookkeeping format differed. Detect that explicitly: the
		// alternative is a bare "no such column" from the SELECT below, which
		// gives an operator nothing to act on.
		if err := db.backend.checkBookkeepingSchema(ctx, tx); err != nil {
			return err
		}

		q := WriteQueries(tx)

		seen, err := readAppliedMigrations(ctx, q)
		if err != nil {
			return err
		}

		if err := db.backend.timeline().refuseUnknownVersions(seen); err != nil {
			return err
		}

		for _, m := range db.backend.timeline().migrations {
			if rec, ok := seen[m.Version]; ok {
				if rec.checksum != m.checksum() {
					return fmt.Errorf(
						"migration %d (%s) was applied with different SQL than this binary contains; "+
							"migrations are append-only and must never be edited",
						m.Version, m.Name)
				}
				continue
			}
			// THE ONE PLACE THIS PACKAGE STILL EXECUTES SQL IT DID NOT NAME, and it
			// cannot be otherwise: these are the published statement bytes, read from
			// a file at run time. Generating a call for them would mean knowing them
			// at build time, which is the opposite of what a migration is.
			for _, stmt := range m.Stmts {
				//billet:ignore rawsql // a migration's published bytes, read from a file at run time; knowing them at build time is the opposite of what a migration is
				if _, err := tx.ExecContext(ctx, stmt); err != nil {
					return fmt.Errorf("apply migration %d (%s): %w", m.Version, m.Name, err)
				}
			}
			if err := q.RecordMigration(ctx, ledgerdb.RecordMigrationParams{
				Version:   int64(m.Version),
				Name:      m.Name,
				Checksum:  m.checksum(),
				AppliedAt: time.Now().UTC().Format(time.RFC3339Nano),
			}); err != nil {
				return fmt.Errorf("record migration %d (%s): %w", m.Version, m.Name, err)
			}
		}
		return nil
	})
}
