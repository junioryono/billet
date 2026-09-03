package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ErrControllerHeld means another process is already this deployment's
// controller.
//
// ITS OWN ERROR because the remedy is specific and nothing else in this package
// implies it: stop the other controller, or fix the configuration that pointed
// two of them at one ledger. An ordinary failure to open would send an operator
// looking at the database.
var ErrControllerHeld = errors.New("state: another billet process is this deployment's controller")

// ErrLeadershipLost means this process WAS this deployment's controller and no
// longer is: a successor has claimed, and every write from here is refused.
//
// ITS OWN ERROR, AND DISTINCT FROM ErrControllerHeld, because the two arrive at
// opposite moments and ask opposite things of a caller. ErrControllerHeld is a
// process that never started; this is one that has been running, may have
// compute in flight, and must now act on nothing. It must also be
// distinguishable from alloc.ErrFenced and alloc.ErrLeaseNotFound, which are
// statements about ONE LEASE that a listener answers by dropping it — this is a
// statement about the whole deployment, and dropping anything on it would be a
// fenced controller taking one last authoritative decision.
var ErrLeadershipLost = errors.New("state: this process is no longer this deployment's controller")

// ErrStandby means this handle was opened as a standby and has not yet claimed
// the controller, so it may not write.
//
// ITS OWN ERROR, AND THE THIRD IN THIS FAMILY, because all three arrive at
// different moments and none of the other two describes this one. ErrControllerHeld
// is a process that could not start; ErrLeadershipLost is one that was replaced;
// this is one that has not started YET and is waiting on purpose. A standby that
// reported either of the others would look like a fault rather than a design.
var ErrStandby = errors.New(
	"state: this process is a standby and has not claimed this deployment's controller")

// controllerPollInterval paces a standby's attempts to take the claim.
//
// SHORT, BECAUSE THE ONLY THING IT COSTS IS ONE pg_try_advisory_lock AGAINST A
// LEDGER NOBODY IS WRITING TO, and what it buys is how long a deployment has no
// controller after the incumbent's session ends. It is not a lease and it is not
// a timeout: nothing here decides that a controller is dead — PostgreSQL does,
// by ending the session — so this number cannot make billet wrong, only slow.
const controllerPollInterval = 2 * time.Second

// ControllerClaim is proof that this process may make scheduling decisions for
// this deployment.
type ControllerClaim struct {
	// Holder is whatever the claiming process called itself. It is a
	// DIAGNOSTIC, not an identity: it decides nothing, and nothing compares it.
	// A refusal quotes it so an operator knows which machine to look at.
	Holder string

	// Epoch goes up by one every time the claim is taken, and never down.
	//
	// IT IS THE FENCE. Every write transaction this handle opens re-reads the
	// recorded epoch and refuses if it has moved — see checkLeadership — so a
	// controller that lost its exclusion without noticing is REFUSED rather than
	// detected. That is what makes it a fencing token rather than a diagnostic,
	// and it is why the value is computed by the ledger rather than supplied by a
	// caller: two controllers agreeing on a number is the one thing a fencing
	// token must never allow.
	//
	// WHAT STILL HAS NO ELECTION BEHIND IT is the promotion. Nothing here decides
	// that a leader is dead or that a follower should take over, and the
	// controller election is where that lands; this is the half it has to be built
	// on.
	Epoch int64
}

// ClaimController takes the deployment's controller claim, or refuses.
//
// IT IS A SEPARATE STEP FROM Open, DELIBERATELY. Open is what every operator
// command and every test uses, and claiming there would mean an ordinary `billet
// nodes approve` announced itself as the controller. What this is for is the one
// process that is about to poll GitHub and dispatch, and it is called BEFORE
// either of those happens — a claim taken afterwards is a claim taken after the
// damage.
//
// THE EXCLUSION IS THE BACKEND'S AND THE RECORD IS SHARED. On SQLite the
// exclusive hold on the state directory has already excluded a second control
// plane, and there is no second machine to worry about because the ledger is a
// file. On PostgreSQL the ledger is reachable from anywhere, so the backend
// takes a session-scoped advisory lock the server releases when the connection
// dies — no lease, no clock, and no stale row that could refuse a correct
// restart.
//
// THE EXCLUSION STOPS A SECOND CONTROLLER STARTING; THE EPOCH IS WHAT FENCES ONE
// THAT WAS ALREADY RUNNING. A controller that loses its session while still
// running — a partition rather than a crash — releases the lock without
// noticing, and a replacement can then legitimately claim. Nothing can stop the
// first one writing up to that moment; what closes the hole is that its next
// write AFTER the successor's claim is refused, because every write transaction
// re-reads this epoch. See checkLeadership, and the PostgreSQL backend's
// claimController for why detection could never have done it.
//
// THE ROW IS WRITTEN AFTER THE EXCLUSION IS HELD, never before and never
// instead. Deciding from the row would be deciding from what is present rather
// than from what is proved, and a crashed controller leaves its row exactly as
// it was.
//
// THE DEPLOYMENT IS BOUND IN THE SAME TRANSACTION AS THE EPOCH, and it is the
// ONE transaction rather than the order within it that matters. Becoming this
// deployment's controller and recording which deployment these rows are is one
// decision: either both land or neither does, so a claim can never leave a
// ledger carrying a generation of a controller whose identity it does not name.
//
// SPLITTING THEM LOOKS HARMLESS AND IS NOT — measured by doing it. With the bind
// in a second transaction after the claim, a process pointed at another
// deployment's rows advances the epoch, is then refused by the binding, and has
// FENCED THE REAL CONTROLLER OUT OF ITS OWN LEDGER on the way past. The
// misconfiguration that should have changed nothing takes the deployment down.
func (db *DB) ClaimController(
	ctx context.Context, holder, deployment string,
) (ControllerClaim, error) {
	if err := db.backend.claimController(ctx, db); err != nil {
		return ControllerClaim{}, err
	}

	// THE EXCLUSION IS WHAT MAKES THIS PROCESS THE CONTROLLER, so the standby
	// latch comes off HERE and not before: holding it is the proof, and every
	// step below is a write that the latch would otherwise refuse — including the
	// migration, which is this handle's right for the first time.
	//
	// AND IT GOES BACK ON IF ANY OF THEM FAILS. A claim that cleared the latch and
	// then could not finish would leave a process that is not the controller and
	// can write anyway, which is the exact hole the latch exists to close.
	wasStandby := db.standby.Swap(false)

	restore := func(err error) (ControllerClaim, error) {
		if wasStandby {
			db.standby.Store(true)
		}

		return ControllerClaim{}, errors.Join(err, db.backend.releaseController())
	}

	// A STANDBY MIGRATES AT PROMOTION, WHICH IS THE FIRST MOMENT IT IS ENTITLED
	// TO. It opened without the exclusion and therefore without the right to
	// change the schema, and it was allowed to wait beside a leader whose schema
	// was older than its own binary — which is the whole shape of a
	// follower-first upgrade. This is where that gets resolved.
	if wasStandby {
		// THE PER-TRANSACTION SCHEMA RE-CHECK COMES OFF FIRST, AND IT HAS TO.
		// Nothing can migrate underneath a handle that holds the exclusion, so it
		// is no longer necessary — and while it is set it REFUSES the migration
		// this handle has just earned the right to make, because verifySchemaIn's
		// whole job is to stop a caller writing against a schema it did not apply.
		// Measured: leaving it on until after the migrate makes a standby unable to
		// promote onto a ledger no controller has ever migrated.
		db.revalidate.Store(false)

		if err := db.migrate(ctx); err != nil {
			db.revalidate.Store(true)

			return restore(fmt.Errorf("state: migrate on promotion: %w", err))
		}
	}

	var epoch int64

	if err := db.Tx(ctx, func(tx *sql.Tx) error {
		if err := bindDeployment(ctx, tx, deployment); err != nil {
			return err
		}

		var err error

		epoch, err = WriteQueries(tx).ClaimController(ctx, ledgerdb.ClaimControllerParams{
			Holder:    holder,
			ClaimedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})

		return err
	}); err != nil {
		// THE EXCLUSION IS GIVEN BACK. A claim that held the lock and failed to
		// record itself would leave the deployment excluded by a process that
		// does not believe it is the controller, and the only way out would be
		// restarting something whose logs say it never started.
		return restore(fmt.Errorf("state: record the controller claim: %w", err))
	}

	// RECORDED AFTER THE COMMIT, NEVER BEFORE. The transaction above goes through
	// DB.Tx and is fenced by checkLeadership like every other write, so a handle
	// that stored its epoch first would read a row that does not yet carry the
	// value it is about to write, and refuse its own claim.
	db.claimedEpoch.Store(epoch)

	// AND THE RELEASE WATERMARK MOVES HERE, AFTER THE BINDING AND THE CLAIM, for
	// every control plane and never at open. This is the first moment the process
	// is the control plane proper: a standby beside an older leader has just
	// taken over, and a process pointed at another deployment's ledger has just
	// been refused by the binding with the mark untouched. A promoted standby
	// records too, which is what makes a follower-first upgrade of an
	// active-passive pair leave the mark at the newer release.
	if db.recordsRelease || wasStandby {
		if err := db.enforceReleaseWatermark(ctx, db.runningRelease, true); err != nil {
			return restore(err)
		}
	}

	return ControllerClaim{Holder: holder, Epoch: epoch}, nil
}

// AwaitController waits until this process can become the deployment's
// controller, and then becomes it.
//
// THIS IS THE ELECTION, AND THERE IS NOTHING ELSE TO IT. PostgreSQL releases a
// session advisory lock when the session ends, so a controller is dead when its
// session is — decided by the database rather than by billet — and a standby
// that keeps asking for that lock becomes the controller at the moment the
// incumbent's goes away. No lease, no renewal, no timeout, and no failure
// detector: a lease would need a number that decides whether a live controller
// is declared dead, and nothing here has one.
//
// ONLY ErrControllerHeld IS WAITED OUT, and that distinction is the whole
// safety content. A held claim is the ordinary state a standby exists for and
// resolves by itself. Everything else — a ledger bound to another deployment, a
// schema this binary cannot read, a database that will not answer — does NOT
// resolve by waiting, and retrying it forever would turn a misconfiguration into
// a process that sits there looking healthy. Those are returned.
//
// onWaiting IS CALLED BEFORE EACH WAIT, with whatever the ledger says about the
// holder, so a caller can log it and report it to a service manager. It is best
// effort by construction: the row is a diagnostic and the exclusion is the
// authority, so a standby that cannot read the holder still waits correctly.
func (db *DB) AwaitController(
	ctx context.Context, holder, deployment string, onWaiting func(ControllerClaim),
) (ControllerClaim, error) {
	for {
		claim, err := db.ClaimController(ctx, holder, deployment)
		if err == nil {
			return claim, nil
		}

		if !errors.Is(err, ErrControllerHeld) {
			return ControllerClaim{}, err
		}

		if onWaiting != nil {
			// THE ERROR IS NOT CONSULTED FOR THE HOLDER. describeHolder already
			// folded it into the refusal's text, and re-parsing that text would be a
			// second reader of a sentence written for a person.
			held, holderErr := db.ControllerHolder(ctx)
			if holderErr != nil {
				held = ControllerClaim{}
			}

			onWaiting(held)
		}

		wait := time.NewTimer(controllerPollInterval)

		select {
		case <-ctx.Done():
			wait.Stop()

			return ControllerClaim{}, fmt.Errorf("state: waiting for the controller claim: %w", ctx.Err())
		case <-wait.C:
		}
	}
}

// LeadershipLost reports whether a write from this handle has been refused
// because another process has become this deployment's controller.
//
// A LATCH THAT NEVER CLEARS. A fenced process cannot win leadership back by
// trying again — the successor holds the exclusion — so a caller that reads true
// must never read false afterwards and act on it.
//
// WHAT IT IS FOR is the control plane's own teardown, which must act on nothing:
// destroy no compute, close no message session, hand back no capacity. Each of
// those is an authoritative act this process no longer has the right to perform,
// and the successor performs every one of them correctly. Deriving it from an
// error would tell the ONE caller that saw the refusal while its siblings
// unwound none the wiser; this is set synchronously inside Tx, before the
// refusal reaches anybody, so it is already true when anything begins tearing
// down.
func (db *DB) LeadershipLost() bool { return db.fenced.Load() }

// LeadershipLostSignal is closed the first time a write is refused because a
// successor claimed. It never carries a value and is never closed twice.
//
// A SIGNAL AS WELL AS A FLAG, BECAUSE REFUSING THE WRITE IS NOT STOPPING THE
// PROCESS. Every background writer in the control plane is deliberately patient
// with an error it cannot classify — a heartbeat keeps its lease, a reap logs and
// tries again, a cleanup retry backs off — because the alternative is a database
// blip dropping leases and failing builds. So a replaced controller whose writes
// are all being refused would carry on polling GitHub, holding its message
// session and running its cleanup loop until something unrelated happened to
// return an error out of a poll. That loop calls Runner.Destroy, which does not
// go through the ledger and is therefore not fenced by anything.
//
// The control plane selects on this and cancels itself, which is what turns a
// refusal into a stop. Polling LeadershipLost on a timer would work and would be
// a second clock deciding how long a replaced controller keeps touching the
// fleet; this fires at the instant the refusal happens.
//
// NIL FOR A HANDLE openDir DID NOT BUILD, which blocks forever in a select. That
// is the right answer for a handle that never claimed and can never be fenced.
func (db *DB) LeadershipLostSignal() <-chan struct{} { return db.fencedCh }

// markFenced records that this handle has been refused, for both consumers, in
// one place so they cannot disagree.
//
// THE FLAG IS SET BEFORE THE CHANNEL CLOSES. Whatever wakes on the signal asks
// the flag immediately afterwards — the listener teardown does exactly that — so
// the other order leaves a window where a woken goroutine reads false and tears
// down as though nothing had happened.
func (db *DB) markFenced() {
	db.fenced.Store(true)

	db.fencedOnce.Do(func() {
		if db.fencedCh != nil {
			close(db.fencedCh)
		}
	})
}

// checkLeadership refuses a write from a process that is no longer this
// deployment's controller.
//
// IT RUNS INSIDE THE WRITE TRANSACTION AND ON THAT TRANSACTION, and both halves
// are why it is exact rather than advisory. Every writer serializes — SQLite
// takes its write lock at BEGIN IMMEDIATE, PostgreSQL takes
// pg_advisory_xact_lock in beginWrite — and a successor's claim advances the
// epoch through DB.Tx, so it takes that same lock. If the row still names our
// epoch here, no successor committed a claim before us, and none can commit
// between this read and our COMMIT.
//
// WHAT IT PROVES IS NARROWER THAN "ONLY ONE CONTROLLER EVER WROTE", and saying
// so is the point: it cannot stop a predecessor writing BEFORE a successor
// claims, because nothing can. What it guarantees is that once a successor HAS
// claimed, the predecessor writes nothing more, which is what makes a handover
// safe.
//
// A HANDLE THAT NEVER CLAIMED IS NOT ASKED AT ALL. migrate runs before any claim
// exists, and every operator command opens through OpenAdmin deliberately
// without one — fencing those would refuse `billet leases release --force`
// against exactly the live deployment it exists for.
func (db *DB) checkLeadership(ctx context.Context, tx *sql.Tx) error {
	claimed := db.claimedEpoch.Load()
	if claimed == 0 {
		return nil
	}

	row, err := ReadQueries(tx).ReadControllerClaim(ctx)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// NOT PERMISSION. This handle claimed, so the row existed; its absence is
		// billet unable to say who the controller is, and a could-not-tell must
		// never resolve to yes.
		db.markFenced()

		return fmt.Errorf("%w: it claimed at epoch %d and the ledger no longer records "+
			"any controller at all", ErrLeadershipLost, claimed)
	case err != nil:
		// REPORTED AS THE FAULT IT IS, and the transaction does not proceed. A
		// database error is evidence about leadership in neither direction:
		// answering it with ErrLeadershipLost would stop a healthy control plane
		// over a blip, and answering it with nil would let a fenced one write.
		return fmt.Errorf("state: read the controller claim to fence this write: %w", err)
	case row.Epoch != claimed:
		db.markFenced()

		return fmt.Errorf("%w: it claimed at epoch %d and %s now holds it at epoch %d",
			ErrLeadershipLost, claimed, row.Holder, row.Epoch)
	}

	return nil
}

// ControllerHolder reports who the ledger says holds the claim.
//
// FOR A DIAGNOSTIC AND FOR `billet status`, never for a decision. An absent row
// is an ordinary state — a deployment nothing has ever claimed — and is reported
// as an empty holder rather than as an error.
func (db *DB) ControllerHolder(ctx context.Context) (ControllerClaim, error) {
	var out ControllerClaim

	err := db.View(ctx, func(q Querier) error {
		row, err := ReadQueries(q).ReadControllerClaim(ctx)
		if err != nil {
			return err
		}

		out = ControllerClaim{Holder: row.Holder, Epoch: row.Epoch}

		return nil
	})

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ControllerClaim{}, nil
	case err != nil:
		return ControllerClaim{}, fmt.Errorf("state: read the controller claim: %w", err)
	}

	return out, nil
}

// describeHolder turns whatever the ledger knows into a clause a refusal can
// carry.
//
// BEST EFFORT ON PURPOSE. The claim has already been refused by the time this
// runs, and a failure to read the row must not replace the refusal with a
// different error — the operator needs to know they are not the controller, and
// who has it is the nicety.
func describeHolder(ctx context.Context, db *DB) string {
	claim, err := db.ControllerHolder(ctx)
	if err != nil || claim.Holder == "" {
		return ""
	}

	return fmt.Sprintf(" (the ledger says %s holds it, at epoch %d)", claim.Holder, claim.Epoch)
}
