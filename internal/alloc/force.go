package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// forceDestroyPhases are the phases whose compute a LISTENER is responsible for,
// and the only ones a force-destroy targets.
//
// WRITTEN OUT RATHER THAN DERIVED, for the reason quiescencePhases is: adding a
// phase has to be a decision about whether an operator may destroy it, not a
// silent inclusion in the one operation that fails somebody's build.
//
// CUSTODY, TEARDOWN AND QUARANTINE ARE DELIBERATELY ABSENT. Their compute is held
// by a NODE rather than by a listener, and billet already has the operation for
// them — `billet leases release --force` sets the request the holder observes on
// its next heartbeat, so the ledger never changes underneath a process that still
// believes it owns the proof obligation. Reaching into those from here would be a
// second mechanism for the same situation, and the one that skips the handoff.
// They are REPORTED beside a force so an operator can see them; they are not
// targeted.
//
// ASSIGNED IS INCLUDED even though its compute may not exist yet. The lease is in
// the listener's `running` map from the moment it is assigned, so nothing else
// returns it, and Destroy is required to be idempotent — asking about compute
// that was never created costs a no-op, while leaving it out strands the lease.
var forceDestroyPhases = []Phase{
	PhaseAssigned, PhaseLaunching, PhaseOnline, PhaseBusy,
}

// ForceCandidate is one lease a force-destroy could target.
type ForceCandidate struct {
	ID    string
	Tier  string
	Node  string
	Phase Phase
	// RunID is GitHub's workflow run, where the lease carries one. It is what
	// makes the diagnostic recognisable to the person whose build is about to be
	// failed — a lease id tells them nothing.
	RunID string
	// SchedulerRequest is billet's numeric scheduler identity, which is how a
	// listener finds this lease in its own escrow. It can be negative on the
	// direct-assignment path, so zero is the only value that means "none".
	SchedulerRequest int64
	Since            string
}

// ForceDestroyCandidates lists the running compute an operator could destroy,
// oldest first.
//
// ON THE READ-ONLY POOL. It is asked by a command with a person waiting, and
// again by that command to build the diagnostic; neither may reserve the single
// writer slot while the control plane is scheduling.
func (a *Allocator) ForceDestroyCandidates(
	ctx context.Context, tier, node string,
) ([]ForceCandidate, error) {
	var out []ForceCandidate

	err := a.db.View(ctx, func(q querier) error {
		out = nil

		rows, err := state.ReadQueries(q).ListForceDestroyCandidates(ctx,
			ledgerdb.ListForceDestroyCandidatesParams{Tier: tier, Node: node})
		if err != nil {
			return fmt.Errorf("alloc: list force-destroy candidates: %w", err)
		}

		for _, row := range rows {
			out = append(out, ForceCandidate{
				ID:               row.ID,
				Tier:             row.Tier,
				Node:             row.Node,
				Phase:            Phase(row.Phase),
				RunID:            row.RunID,
				SchedulerRequest: row.RequestID,
				Since:            row.Since,
			})
		}

		return nil
	})

	return out, err
}

// ErrForceHeld means a forced lease has acquired a live holder, so its capacity
// is not this caller's to return.
//
// A DESTROY THAT ENDED IN CUSTODY IS NOT A DESTROY THAT FINISHED. The node asked
// its backend to stop the guest and did not get proof it stopped, so it is
// holding the lease until the compute is provably gone — and terminalising it
// here would free a slot whose container may still be on the host, which is the
// overcommit the whole ordering exists to prevent. `billet leases release --force`
// is the operation for that case, because it goes THROUGH the holder rather than
// underneath it.
var ErrForceHeld = errors.New("alloc: a node has taken custody of this lease")

// forceDestroyFailureReason is what a forced lease records in its history.
//
// FAILED, NOT DONE. A job whose runner was destroyed while it was executing did
// not finish, and recording `done` puts a lie in the history of somebody's build
// — the same objection the launch path makes about a runner that never ran. The
// marker says WHY, so the row is distinguishable from an ordinary failure.
const forceDestroyFailureReason = "billet:force-destroy"

// ForceTerminate archives a lease whose compute an operator destroyed.
//
// THE ONE TERMINALISER FOR BOTH KINDS OF FORCED LEASE, and that is why it takes
// an id rather than a *Lease. A listener holds an object for the work it launched
// itself, and holds nothing at all for compute a restart re-adopted — but the
// durable force record names the lease either way, so a path keyed on the id
// reaches both and a path keyed on the in-memory object silently skips every job
// that outlived a control plane.
//
// THE PHASE IS READ INSIDE THE TRANSACTION rather than taken from the destroy
// pass. Between asking a backend to stop a guest and returning its capacity, that
// lease can acquire a live holder — and a decision made from the caller's memory
// would free capacity the node had just taken responsibility for, seconds after
// the handoff.
func (a *Allocator) ForceTerminate(ctx context.Context, leaseID string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		epoch, err := q.ReadLeaseEpoch(ctx, leaseID)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			// GONE IS THE OUTCOME THIS WANTED. The reaper or a concurrent settlement
			// got there first; nothing is left holding capacity, which is what the
			// caller is trying to achieve.
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read forced lease %s: %w", leaseID, err)
		}

		lease, err := a.loadAny(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		// IDEMPOTENT, because a listener that restarts re-observes the durable force
		// request and may act on a target a previous incarnation already settled.
		if lease.Phase.Terminal() {
			return nil
		}

		// AN ALLOWLIST, AND THE DENYLIST IT REPLACED WAS A P0.
		//
		// This refused custody and teardown and let everything else through, on the
		// reasoning that it is only reached after a confirmed destroy. That
		// reasoning was wrong twice. `destroyAll` reports a CUSTODY answer as
		// "done" — deliberately, because the node has taken responsibility — so the
		// caller's map is not proof of absence at all. And even where it is, the
		// reaper can move a lease into QUARANTINE between the destroy and this
		// transaction, which the denylist permitted: capacity freed for compute
		// nobody proved gone, which is the overcommit the whole ordering exists to
		// prevent.
		//
		// So the question is not "is this phase forbidden" but "is this still one
		// of the phases the operator approved". Anything else means the lease
		// acquired a holder or lost one while the force was in flight, and both
		// have their own operation: `billet leases release --force` goes THROUGH a
		// holder where one exists and resolves a quarantine where none does.
		if !slices.Contains(forceDestroyPhases, lease.Phase) {
			return fmt.Errorf("%w: lease %s moved to %s on node %s while the force was in "+
				"flight; `billet leases release --force` is what resolves it once you know "+
				"the compute is gone", ErrForceHeld, leaseID, lease.Phase, lease.Node)
		}

		// EMPTY ONLY. A lease that already records why it was going to fail keeps
		// that reason: the earlier fact is the cause, and this teardown is what
		// followed from it.
		if lease.FailureReason == "" {
			lease.FailureReason = forceDestroyFailureReason
		}

		if err := q.TerminateForcedLease(ctx, ledgerdb.TerminateForcedLeaseParams{
			Phase:         string(PhaseFailed),
			FailureReason: lease.FailureReason,
			ID:            leaseID,
			Epoch:         epoch,
		}); err != nil {
			return fmt.Errorf("alloc: terminate forced lease %s: %w", leaseID, err)
		}

		return a.archive(ctx, tx, lease, PhaseFailed)
	})
}

// RequestForceDestroy records an operator's decision to destroy running compute.
func (a *Allocator) RequestForceDestroy(
	ctx context.Context, req state.ForceDestroyRequest,
) (state.ForceDestroy, error) {
	return a.db.RequestForceDestroy(ctx, req)
}

// OpenForceDestroy reads the force-destroy request that has not finished, if any.
func (a *Allocator) OpenForceDestroy(ctx context.Context) (state.ForceDestroy, bool, error) {
	return a.db.OpenForceDestroy(ctx)
}

// PendingForceTargets lists what one tier still owes a destroy for.
func (a *Allocator) PendingForceTargets(
	ctx context.Context, generation int64, tier string,
) ([]state.ForceTarget, error) {
	return a.db.PendingForceTargets(ctx, generation, tier)
}

// ForceTargets lists every lease one force-destroy covers.
func (a *Allocator) ForceTargets(
	ctx context.Context, generation int64,
) ([]state.ForceTarget, error) {
	return a.db.ForceTargets(ctx, generation)
}

// SettleForceTarget records what became of one forced lease.
func (a *Allocator) SettleForceTarget(
	ctx context.Context, generation int64, leaseID, disposition, detail string,
) error {
	return a.db.SettleForceTarget(ctx, generation, leaseID, disposition, detail)
}

// LatestForceDestroy reads the most recent force-destroy, for the report.
func (a *Allocator) LatestForceDestroy(ctx context.Context) (state.ForceDestroy, bool, error) {
	return a.db.LatestForceDestroy(ctx)
}
