package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ErrForceDestroyOpen means another force-destroy request has not finished.
//
// A SENTINEL BECAUSE THE REMEDY IS THE CALLER'S. Two concurrent forces would
// each enumerate a target set the other was midway through destroying, and
// neither diagnostic would describe what happened — but only the command knows
// how to say "watch the one that is running with `billet status`".
var ErrForceDestroyOpen = errors.New("state: a force-destroy request is already open")

// ErrForceDestroyNotSealed means the deployment is still admitting work.
//
// A FORCE ENUMERATES A SET AND THEN ASKS A PERSON ABOUT IT, so admission has to
// be closed before the enumeration or a job admitted in between is destroyed
// without ever appearing in the diagnostic the operator approved — or, worse,
// starts just after the destroy pass and survives a force that reported success.
var ErrForceDestroyNotSealed = errors.New("state: this deployment is still admitting work")

// Where one force-destroy request has got to.
const (
	ForceRequested = "requested"
	ForceCompleted = "completed"
)

// The disposition of one lease inside a force-destroy request.
const (
	// ForceTargetPending is a lease no listener has acted on yet.
	ForceTargetPending = "pending"
	// ForceTargetDestroyed is compute a listener confirmed gone.
	ForceTargetDestroyed = "destroyed"
	// ForceTargetFailed is a lease whose destroy did not confirm.
	//
	// NOT "UNKNOWN", AND THE DISTINCTION IS THE POINT. A failed destroy is not
	// proof the container survived, so nothing here releases capacity on it: the
	// lease stays charged and the row says so, which is what an operator reads
	// when a force reports that it did not finish.
	ForceTargetFailed = "failed"
)

// ForceDestroy is one operator decision to destroy running compute.
type ForceDestroy struct {
	Generation int64
	// AdmissionGeneration is the seal this was authorised against, so a reader
	// can see afterwards that the seal did not move underneath the request.
	AdmissionGeneration int64
	State               string
	Reason              string
	Actor               string
	RequestedAt         string
	CompletedAt         string
}

// ForceTarget is one lease an operator authorised destroying.
//
// EVERY FIELD IS HERE SO THE DIAGNOSTIC SURVIVES THE COMMAND. A force must name
// every affected job, lease and node, and a listener acting on this a poll later
// — or a second control plane after a restart — has no other record of what the
// person actually approved.
type ForceTarget struct {
	Generation       int64
	LeaseID          string
	Tier             string
	Node             string
	RunID            string
	SchedulerRequest int64
	Phase            string
	State            string
	Detail           string
}

// ForceDestroyRequest is one decision to destroy running compute.
type ForceDestroyRequest struct {
	// ExpectAdmission is the admission generation the caller enumerated its
	// targets against. A seal that moved in between means the set was taken from
	// a deployment in a different state, so the request is refused rather than
	// acted on.
	ExpectAdmission int64
	Reason          string
	Actor           string
	Targets         []ForceTarget
}

// RequestForceDestroy records an operator's decision and the exact leases it
// covers.
//
// THE PRECONDITIONS ARE CHECKED INSIDE THE WRITE TRANSACTION, against the rows
// the write acts on. Checking them in the command and writing afterwards proves
// nothing about the state at the moment of the write: a resume committing in
// between would have this authorise destruction on a deployment that is admitting
// work again.
func (db *DB) RequestForceDestroy(
	ctx context.Context, req ForceDestroyRequest,
) (ForceDestroy, error) {
	if len(req.Targets) == 0 {
		return ForceDestroy{}, errors.New(
			"state: a force-destroy request with no targets would destroy nothing")
	}

	// REQUIRED, because this is the one record of why somebody's build was failed
	// on purpose. An unattributed force is one nobody can answer for.
	if req.Reason == "" {
		return ForceDestroy{}, errors.New("state: a force-destroy request needs a reason")
	}

	var out ForceDestroy

	err := db.Tx(ctx, func(tx *sql.Tx) error {
		admission, err := ReadAdmission(ctx, tx)
		if err != nil {
			return err
		}

		// SEALED BY AN OPERATOR, not merely sealed. A local-down seal is cleared by
		// the next `billet local up`, so authorising destruction against one would
		// let a routine restart reopen admission into a deployment somebody was
		// midway through forcing.
		if admission.Mode != AdmissionSealed || admission.Provenance != ProvenanceOperator {
			return fmt.Errorf("%w: admission reads %s%s, and a force-destroy has to be taken "+
				"against a deployment an operator has already sealed",
				ErrForceDestroyNotSealed, admission.Mode, sealedBy(admission))
		}

		if admission.Generation != req.ExpectAdmission {
			return fmt.Errorf("%w: expected %d, found %d. Admission moved between enumerating "+
				"what would be destroyed and authorising it, so that list no longer describes "+
				"this deployment",
				ErrAdmissionGeneration, req.ExpectAdmission, admission.Generation)
		}

		q := WriteQueries(tx)

		open, err := q.CountForceDestroyInState(ctx, ForceRequested)
		if err != nil {
			return fmt.Errorf("state: look for an open force-destroy: %w", err)
		}

		if open > 0 {
			return ErrForceDestroyOpen
		}

		// COALESCE'd to 0 in the query, so an empty table makes the first request
		// generation 1 exactly as a NULL scanned into sql.NullInt64 used to.
		highest, err := q.HighestForceDestroyGeneration(ctx)
		if err != nil {
			return fmt.Errorf("state: read the force-destroy generation: %w", err)
		}

		out = ForceDestroy{
			Generation:          highest + 1,
			AdmissionGeneration: admission.Generation,
			State:               ForceRequested,
			Reason:              req.Reason,
			Actor:               req.Actor,
			RequestedAt:         time.Now().UTC().Format(time.RFC3339Nano),
		}

		if err := q.InsertForceDestroy(ctx, ledgerdb.InsertForceDestroyParams{
			Generation:          out.Generation,
			AdmissionGeneration: out.AdmissionGeneration,
			State:               out.State,
			Reason:              out.Reason,
			Actor:               out.Actor,
			RequestedAt:         out.RequestedAt,
		}); err != nil {
			return fmt.Errorf("state: record a force-destroy: %w", err)
		}

		for i := range req.Targets {
			t := &req.Targets[i]

			if err := q.InsertForceDestroyTarget(ctx, ledgerdb.InsertForceDestroyTargetParams{
				Generation:       out.Generation,
				LeaseID:          t.LeaseID,
				Tier:             t.Tier,
				Node:             t.Node,
				RunID:            t.RunID,
				SchedulerRequest: t.SchedulerRequest,
				Phase:            t.Phase,
				State:            ForceTargetPending,
			}); err != nil {
				return fmt.Errorf("state: record force-destroy target %s: %w", t.LeaseID, err)
			}
		}

		return nil
	})
	if err != nil {
		return ForceDestroy{}, err
	}

	return out, nil
}

func sealedBy(a Admission) string {
	if a.Actor == "" {
		return ""
	}

	return " (held by " + a.Actor + ")"
}

// OpenForceDestroy reads the force-destroy request that has not finished, if any.
//
// ON THE READ-ONLY POOL, because every listener asks this on its poll while the
// control plane is doing real work, and a question must not reserve the single
// writer slot to answer itself.
func (db *DB) OpenForceDestroy(ctx context.Context) (ForceDestroy, bool, error) {
	var (
		f     ForceDestroy
		found bool
	)

	err := db.View(ctx, func(q Querier) error {
		row, err := ReadQueries(q).ForceDestroyInState(ctx, ForceRequested)

		// EXISTENCE IS READ FROM THE ERROR, never from the returned value: a :one
		// query hands back a zero-value struct alongside sql.ErrNoRows, and a
		// caller that tested the struct would report generation 0 as a real
		// request.
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("state: read the open force-destroy: %w", err)
		}

		f = forceDestroyFrom(row)
		found = true

		return nil
	})

	return f, found, err
}

// forceDestroyFrom maps the generated row, so the two readers of force_destroy
// cannot disagree about what a column means.
func forceDestroyFrom(row ledgerdb.ForceDestroy) ForceDestroy {
	return ForceDestroy{
		Generation:          row.Generation,
		AdmissionGeneration: row.AdmissionGeneration,
		State:               row.State,
		Reason:              row.Reason,
		Actor:               row.Actor,
		RequestedAt:         row.RequestedAt,
		CompletedAt:         row.CompletedAt,
	}
}

// PendingForceTargets lists the leases one tier still owes a destroy for.
//
// SCOPED TO A TIER because a listener may only act on its own escrow. A listener
// destroying another tier's compute would be tearing down a lease it never held
// and cannot release.
func (db *DB) PendingForceTargets(
	ctx context.Context, generation int64, tier string,
) ([]ForceTarget, error) {
	var out []ForceTarget

	err := db.View(ctx, func(q Querier) error {
		out = nil

		rows, err := ReadQueries(q).PendingForceTargets(ctx, ledgerdb.PendingForceTargetsParams{
			Generation: generation,
			Tier:       tier,
			State:      ForceTargetPending,
		})
		if err != nil {
			return fmt.Errorf("state: list force-destroy targets: %w", err)
		}

		for _, r := range rows {
			out = append(out, ForceTarget{
				Generation:       generation,
				LeaseID:          r.LeaseID,
				Tier:             r.Tier,
				Node:             r.Node,
				RunID:            r.RunID,
				SchedulerRequest: r.SchedulerRequest,
				Phase:            r.Phase,
				State:            r.State,
				Detail:           r.Detail,
			})
		}

		return nil
	})

	return out, err
}

// ForceTargets lists every lease one request covers, whatever became of it.
func (db *DB) ForceTargets(ctx context.Context, generation int64) ([]ForceTarget, error) {
	var out []ForceTarget

	err := db.View(ctx, func(q Querier) error {
		out = nil

		rows, err := ReadQueries(q).ForceTargets(ctx, generation)
		if err != nil {
			return fmt.Errorf("state: list force-destroy targets: %w", err)
		}

		for _, r := range rows {
			out = append(out, ForceTarget{
				Generation:       generation,
				LeaseID:          r.LeaseID,
				Tier:             r.Tier,
				Node:             r.Node,
				RunID:            r.RunID,
				SchedulerRequest: r.SchedulerRequest,
				Phase:            r.Phase,
				State:            r.State,
				Detail:           r.Detail,
			})
		}

		return nil
	})

	return out, err
}

// SettleForceTarget records what became of one lease, and completes the request
// once nothing is pending.
//
// COMPLETION IS DECIDED IN THE SAME TRANSACTION AS THE LAST SETTLEMENT. Deciding
// it from a separate read would let two listeners settling their last target
// concurrently both see work outstanding, and leave a request open that nothing
// will ever finish — which reads to an operator as a force that hung.
//
// A FAILED TARGET STILL COMPLETES THE REQUEST. The alternative is a request that
// stays open forever against compute nothing can confirm, blocking the next
// force; the row keeps saying `failed`, which is what an operator acts on.
func (db *DB) SettleForceTarget(
	ctx context.Context, generation int64, leaseID, state, detail string,
) error {
	if state != ForceTargetDestroyed && state != ForceTargetFailed {
		return fmt.Errorf("state: %q is not a settled force-destroy disposition", state)
	}

	return db.Tx(ctx, func(tx *sql.Tx) error {
		q := WriteQueries(tx)

		res, err := q.SettleForceTarget(ctx, ledgerdb.SettleForceTargetParams{
			State:      state,
			Detail:     detail,
			Generation: generation,
			LeaseID:    leaseID,
			WasState:   ForceTargetPending,
		})
		if err != nil {
			return fmt.Errorf("state: settle force-destroy target %s: %w", leaseID, err)
		}

		// ALREADY SETTLED IS NOT AN ERROR. A listener that restarts re-observes the
		// request and may act on a target another incarnation already finished; the
		// destroy underneath is idempotent, and so is this.
		if n, err := res.RowsAffected(); err == nil && n == 0 {
			return nil
		}

		pending, err := q.CountForceTargetsInState(ctx, ledgerdb.CountForceTargetsInStateParams{
			Generation: generation,
			State:      ForceTargetPending,
		})
		if err != nil {
			return fmt.Errorf("state: count pending force-destroy targets: %w", err)
		}

		if pending > 0 {
			return nil
		}

		if err := q.CompleteForceDestroy(ctx, ledgerdb.CompleteForceDestroyParams{
			State:       ForceCompleted,
			CompletedAt: time.Now().UTC().Format(time.RFC3339Nano),
			Generation:  generation,
			WasState:    ForceRequested,
		}); err != nil {
			return fmt.Errorf("state: complete force-destroy %d: %w", generation, err)
		}

		return nil
	})
}

// LatestForceDestroy reads the most recent request, open or finished, for the
// report an operator reads after the fact.
func (db *DB) LatestForceDestroy(ctx context.Context) (ForceDestroy, bool, error) {
	var (
		f     ForceDestroy
		found bool
	)

	err := db.View(ctx, func(q Querier) error {
		row, err := ReadQueries(q).LatestForceDestroy(ctx)

		// Existence from the error, for the reason OpenForceDestroy gives.
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("state: read the last force-destroy: %w", err)
		}

		f = forceDestroyFrom(row)
		found = true

		return nil
	})

	return f, found, err
}
