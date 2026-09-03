package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// QuarantinedLease is capacity held because compute could not be confirmed gone.
type QuarantinedLease struct {
	ID     string
	Tier   string
	Node   string
	VCPU   int
	Memory config.ByteSize
	Since  string
}

// HeldLease is capacity retained while compute has not been confirmed gone.
// State distinguishes a live node tending it from a lease whose holder vanished.
type HeldLease struct {
	ID             string
	Tier           string
	Node           string
	State          Phase
	VCPU           int
	Memory         config.ByteSize
	Since          string
	ForceRequested bool
	// Holder is the node process that took the obligation, beside what the
	// deployment knows about that host now.
	Holder Holder
}

// Holder names the node process a lease was given to, and whether it is still
// the process the deployment talks to.
//
// A REPORT, NOT A VERDICT. A different incarnation says the process that took
// the work is not the one this host's commands reach — dead, or superseded and
// still draining what it holds. Which of those it is shows in whether the lease
// keeps being renewed, and that is the reaper's to decide: a lease nobody renews
// is quarantined within a TTL, and quarantine is what an operator can release.
//
// INCARNATIONS, NOT EPOCHS. A node process mints one incarnation for its life;
// the registration epoch moves on every registration, and the same process
// registers again whenever a control plane restarts or forgets it. A report
// built on the epoch would call every surviving lease's holder replaced after an
// ordinary restart, which is exactly the accusation that gets a report ignored.
type Holder struct {
	// Incarnation is the process that took the work, or empty when no process
	// recorded one.
	Incarnation string
	// NodeIncarnation is the process the host registered with most recently,
	// empty for a host that presented none, and NodeKnown says the ledger has a
	// row for the host at all.
	NodeIncarnation string
	NodeKnown       bool
	// NodeLive is whether this deployment can reach the host right now.
	NodeLive bool
	// NodeSeenAt is when the host's current registration was recorded — for a
	// replaced holder, when its replacement arrived.
	NodeSeenAt string
}

// Replaced reports whether a different process has registered under the host's
// name since the holder took the work. False when either side is unknown,
// because "cannot tell" must not read as "replaced".
func (h Holder) Replaced() bool {
	return h.Incarnation != "" && h.NodeKnown && h.NodeIncarnation != "" &&
		h.NodeIncarnation != h.Incarnation
}

// Held lists every operator-visible proof obligation, oldest first.
func (a *Allocator) Held(ctx context.Context) ([]HeldLease, error) {
	var out []HeldLease

	err := a.db.View(ctx, func(tx querier) error {
		rows, err := state.ReadQueries(tx).ListHeldLeases(ctx, ledgerdb.ListHeldLeasesParams{
			Custody:    string(PhaseCustody),
			Teardown:   string(PhaseTeardown),
			Quarantine: string(PhaseQuarantine),
		})
		if err != nil {
			return fmt.Errorf("alloc: list held leases: %w", err)
		}

		for i := range rows {
			row := &rows[i]
			out = append(out, HeldLease{
				ID:             row.ID,
				Tier:           row.Tier,
				Node:           row.Node,
				State:          Phase(row.Phase),
				VCPU:           int(row.Vcpu),
				Memory:         config.ByteSize(row.Memory),
				Since:          row.HeldAt,
				ForceRequested: row.ForceRelease == 1,
				Holder: Holder{
					Incarnation:     row.HolderIncarnation,
					NodeIncarnation: row.NodeIncarnation.String,
					NodeKnown:       row.NodeIncarnation.Valid,
					NodeLive:        row.NodeLive.Valid && row.NodeLive.Int64 == 1,
					NodeSeenAt:      row.NodeSeenAt.String,
				},
			})
		}

		return nil
	})

	return out, err
}

// ReplacedHolderLease is a lease in a running phase whose host has registered
// again since the process holding it was given the work.
//
// THE SHAPE AN OPERATOR ONCE HAD NOTHING TO READ ABOUT. Such a lease is charged
// and is listed as held by nobody: its process is dead or superseded, and until
// it stops being renewed the reaper cannot quarantine it. It is reported so an
// operator can see WHY a slot is taken, and so that "stops being renewed" can
// be watched for rather than inferred.
type ReplacedHolderLease struct {
	ID          string
	Tier        string
	Node        string
	State       Phase
	VCPU        int
	Memory      config.ByteSize
	LastRenewed string
	Holder      Holder
}

// RunningWithReplacedHolder lists every running-phase lease whose holding
// process is not the one its host registered with, oldest renewal first.
//
// ON THE READ-ONLY POOL, because it answers a question an operator asks while
// the control plane is working, and a report must not reserve the writer slot.
func (a *Allocator) RunningWithReplacedHolder(ctx context.Context) ([]ReplacedHolderLease, error) {
	var out []ReplacedHolderLease

	err := a.db.View(ctx, func(tx querier) error {
		rows, err := state.ReadQueries(tx).ListRunningLeasesWithReplacedHolder(ctx,
			ledgerdb.ListRunningLeasesWithReplacedHolderParams{
				Launching: string(PhaseLaunching),
				Online:    string(PhaseOnline),
				Busy:      string(PhaseBusy),
			})
		if err != nil {
			return fmt.Errorf("alloc: list running leases whose holder was replaced: %w", err)
		}

		for i := range rows {
			row := &rows[i]
			out = append(out, ReplacedHolderLease{
				ID:          row.ID,
				Tier:        row.Tier,
				Node:        row.Node,
				State:       Phase(row.Phase),
				VCPU:        int(row.Vcpu),
				Memory:      config.ByteSize(row.Memory),
				LastRenewed: row.HeartbeatAt,
				Holder: Holder{
					Incarnation:     row.HolderIncarnation,
					NodeIncarnation: row.NodeIncarnation,
					NodeKnown:       true,
					NodeLive:        row.NodeLive == 1,
					NodeSeenAt:      row.NodeSeenAt,
				},
			})
		}

		return nil
	})

	return out, err
}

// SettleCompletionOnTerminalLease records GitHub's outcome against a lease that
// has ALREADY been settled by something else, and reports whether it was.
//
// THE ONLY COMPLETION PATH THAT DOES NOT REACH THE HOLDER, and the
// narrowness is the safety argument. An earlier path let a completion
// terminalize an open quarantine from the plane's cached inventory snapshot,
// and that cache has no ordering relationship to the quarantine: a snapshot
// taken before a build became visible, never refreshed because every later
// listing failed, would settle the lease under a running build. So a
// completion whose holder is gone — replaced, or never replaced at all —
// settles NOTHING itself. It waits for the host's ordinary inventory
// reconciliation — a fresh observation, taken after the grace — or an
// operator's forced release to settle the lease, and then corrects a
// provisional verdict to the outcome GitHub reported. An open lease answers
// false, however old it is.
func (a *Allocator) SettleCompletionOnTerminalLease(
	ctx context.Context, leaseID string, leaseEpoch int64, outcome Phase,
) (bool, error) {
	if !outcome.Terminal() {
		return false, fmt.Errorf("%w: %q is not terminal", ErrBadTransition, outcome)
	}

	settled := false
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		settlement, err := q.ReadLeaseSettlement(ctx, leaseID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: inspect completion lease %s: %w", leaseID, err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			// Capacity is gone, but without the lease row there is no provenance
			// for its history. Settled is safe; rewriting an unknown verdict is not.
			settled = true

			return nil
		}
		if settlement.Epoch < leaseEpoch {
			return nil
		}
		if !Phase(settlement.Phase).Terminal() {
			return nil
		}

		// ONLY INVENTORY'S PROVISIONAL VERDICT MAY BE CORRECTED, the same rule
		// ResolveQuarantineForCompletion applies: an operator or a node can fail
		// the lease independently, and that verdict stands.
		if settlement.FailureReason == inventoryAbsenceFailureReason {
			if err := q.CorrectProvisionalLease(ctx, ledgerdb.CorrectProvisionalLeaseParams{
				Phase: string(outcome),
				ID:    leaseID,
			}); err != nil {
				return fmt.Errorf("alloc: correct completion lease %s: %w", leaseID, err)
			}
			if err := q.CorrectProvisionalHistory(ctx,
				ledgerdb.CorrectProvisionalHistoryParams{
					Conclusion: sql.NullString{String: string(outcome), Valid: true},
					LeaseID:    leaseID,
				}); err != nil {
				return fmt.Errorf("alloc: correct completion history for lease %s: %w", leaseID, err)
			}
		}
		settled = true

		return nil
	})

	return settled, err
}

// ForceReleaseResult says whether capacity was returned immediately or the
// request was handed to a live custody holder.
type ForceReleaseResult struct {
	Node    string
	Pending bool
}

// ForceRelease records an operator's assertion that held compute is gone.
//
// A quarantined lease has no live holder, so it is resolved here. Custody and
// teardown do have one: setting force_release makes its next heartbeat return
// ErrForceRelease, after which that node drops the local custody record and
// releases the lease. This ordering avoids changing the ledger underneath a
// process that still believes it owns the proof obligation.
func (a *Allocator) ForceRelease(ctx context.Context, leaseID string) (ForceReleaseResult, error) {
	var result ForceReleaseResult

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		epoch, err := q.ReadLeaseEpoch(ctx, leaseID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("%w: %s", ErrLeaseNotFound, leaseID)
			}

			return fmt.Errorf("alloc: read held lease %s: %w", leaseID, err)
		}

		lease, err := a.loadAny(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}
		result.Node = lease.Node

		switch lease.Phase {
		case PhaseQuarantine:
			// THE OPERATOR'S ASSERTION IS THE REASON, unless something already
			// explained the failure — a Spot warning, a launch that never ran —
			// and that earlier fact stands. Written before the row is fenced so
			// the history copies it.
			if lease.FailureReason == "" {
				if err := q.MarkLeaseFailure(ctx, ledgerdb.MarkLeaseFailureParams{
					FailureReason: ForceReleasedReason,
					ID:            leaseID,
					Epoch:         epoch,
				}); err != nil {
					return fmt.Errorf("alloc: record the forced release of lease %s: %w", leaseID, err)
				}

				lease.FailureReason = ForceReleasedReason
			}

			if err := q.FenceQuarantinedLease(ctx, ledgerdb.FenceQuarantinedLeaseParams{
				Phase: string(PhaseFailed),
				ID:    leaseID,
				Epoch: epoch,
			}); err != nil {
				return fmt.Errorf("alloc: force-release quarantined lease %s: %w", leaseID, err)
			}

			return a.archive(ctx, tx, lease, PhaseFailed)

		case PhaseCustody, PhaseTeardown:
			result.Pending = true
			if err := q.RequestForceRelease(ctx, ledgerdb.RequestForceReleaseParams{
				Reason: ForceReleasedReason,
				ID:     leaseID,
				Epoch:  epoch,
			}); err != nil {
				return fmt.Errorf("alloc: request force-release of lease %s: %w", leaseID, err)
			}

			return nil

		default:
			return fmt.Errorf("%w: lease %s is %s, not held in custody, teardown, or quarantine",
				ErrBadTransition, leaseID, lease.Phase)
		}
	})

	return result, err
}

// Quarantined lists the leases holding capacity for compute nobody has accounted
// for, oldest first.
//
// WHAT AN OPERATOR LOOKS AT WHEN CAPACITY IS MISSING. A quarantined lease is the
// one thing that shrinks a fleet without anything having failed, so it has to be
// visible or the number is inexplicable.
func (a *Allocator) Quarantined(ctx context.Context) ([]QuarantinedLease, error) {
	var out []QuarantinedLease

	err := a.db.View(ctx, func(tx querier) error {
		out = nil

		rows, err := state.ReadQueries(tx).ListQuarantinedLeases(ctx, string(PhaseQuarantine))
		if err != nil {
			return fmt.Errorf("alloc: list quarantined leases: %w", err)
		}

		for _, row := range rows {
			out = append(out, QuarantinedLease{
				ID:     row.ID,
				Tier:   row.Tier,
				Node:   row.Node,
				VCPU:   int(row.Vcpu),
				Memory: config.ByteSize(row.Memory),
				Since:  row.HeartbeatAt,
			})
		}

		return nil
	})

	return out, err
}

// QuarantinedLeaseIDs reports the quarantined leases attributed to one node.
//
// SOMETHING IS STILL WAITING FOR THIS COMPUTE, which is the question the node is
// asking. LaunchedLeaseIDs answers it for leases a listener is managing, and
// deliberately does not include quarantine — the plane uses that set to decide
// ownership. But a quarantined lease is the case where the compute matters MOST:
// nobody is managing it, so a node that read only the launched set would see a
// running job as an orphan and destroy it.
func (a *Allocator) QuarantinedLeaseIDs(ctx context.Context, node string) (map[string]bool, error) {
	out := map[string]bool{}

	err := a.db.View(ctx, func(tx querier) error {
		// EVERY quarantined lease, whatever its age: the node is asking which of its
		// containers something is still waiting for, and a young quarantine is
		// exactly the one it must not destroy.
		ids, err := quarantinedIDsOn(ctx, state.ReadQueries(tx), node, "")
		if err != nil {
			return err
		}

		for _, id := range ids {
			out[id] = true
		}

		return nil
	})

	return out, err
}

// ResolveQuarantine terminalizes a quarantined lease, returning its capacity.
//
// PROOF, OR AN OPERATOR SAYING SO. The node calls this when it has destroyed the
// container, or when it re-registers reporting an inventory the lease is not in;
// both are evidence the compute is gone. `--force` exists for the case evidence
// can never arrive in — a machine that is not coming back — because otherwise its
// capacity would be missing from the deployment permanently, and the ceiling is
// deployment-wide.
// THE OUTCOME IS THE CALLER'S, because they are not all the same event. A
// listener resolving one after a completion knows the job finished; a node
// cleaning up compute it could not account for, and an operator forcing a
// machine that is never coming back, both know the opposite. Recording every
// one of them as `failed` puts a lie in the history of a job GitHub reported
// completed — the same objection the launch path makes in reverse.
func (a *Allocator) ResolveQuarantine(ctx context.Context, leaseID string, outcome Phase) error {
	if !outcome.Terminal() {
		return fmt.Errorf("%w: %q is not terminal", ErrBadTransition, outcome)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := quarantinedLeaseTx(ctx, tx, a, leaseID)
		if err != nil {
			return err
		}

		if err := state.WriteQueries(tx).TerminalizeQuarantinedLease(ctx,
			ledgerdb.TerminalizeQuarantinedLeaseParams{
				Phase: string(outcome),
				ID:    leaseID,
			}); err != nil {
			return fmt.Errorf("alloc: resolve quarantined lease %s: %w", leaseID, err)
		}

		return a.archive(ctx, tx, lease, outcome)
	})
}

// ResolveQuarantineFailed is ResolveQuarantine for a failure the caller can
// explain, recording the reason in the same transaction unless one is there.
//
// THE LISTENER'S PARKED LAUNCH FAILURE MEETS THE REAPER when it gets to the
// lease before the retry does — quarantined if it had reached `launching`,
// failed outright if it was escrow alone — and resolving it through
// ResolveQuarantine archived a failure with nothing to explain it. Both rows
// are explained here. Same rule as ReleaseFailed: an earlier reason stands.
func (a *Allocator) ResolveQuarantineFailed(ctx context.Context, leaseID, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("alloc: a failure reason must not be empty")
	}
	if reason == inventoryAbsenceFailureReason || reason == ForceReleasedReason {
		return fmt.Errorf("alloc: failure reason %q is reserved", reason)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		epoch, err := q.ReadLeaseEpoch(ctx, leaseID)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrLeaseNotFound, leaseID)
		case err != nil:
			return fmt.Errorf("alloc: read lease %s: %w", leaseID, err)
		}

		lease, err := a.loadAny(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		// THE REAPER MAY HAVE FAILED IT OUTRIGHT rather than quarantining it: an
		// escrow-only lease has no compute to hold, so its expiry is a plain
		// failed row at a moved epoch, which the caller's fenced release could
		// not reach. The failure is settled; what it lacks is the explanation,
		// and the same rule as ReleaseFailed's terminal branch applies.
		if lease.Phase == PhaseFailed {
			if lease.FailureReason != "" {
				return nil
			}
			if err := q.BackfillFailureReason(ctx,
				ledgerdb.BackfillFailureReasonParams{FailureReason: reason, ID: leaseID}); err != nil {
				return fmt.Errorf("alloc: explain the failure of lease %s: %w", leaseID, err)
			}
			if err := q.BackfillLeaseFailureReason(ctx,
				ledgerdb.BackfillLeaseFailureReasonParams{FailureReason: reason, ID: leaseID}); err != nil {
				return fmt.Errorf("alloc: explain the failure of lease %s: %w", leaseID, err)
			}

			return nil
		}

		if lease.Phase != PhaseQuarantine {
			return fmt.Errorf(
				"%w: lease %s is %s rather than quarantined, so there is nothing to resolve",
				ErrLeaseNotFound, leaseID, lease.Phase)
		}

		if lease.FailureReason == "" {
			if err := a.markFailureTx(ctx, tx, lease, reason); err != nil {
				return err
			}
		}

		if err := q.TerminalizeQuarantinedLease(ctx,
			ledgerdb.TerminalizeQuarantinedLeaseParams{
				Phase: string(PhaseFailed),
				ID:    leaseID,
			}); err != nil {
			return fmt.Errorf("alloc: resolve quarantined lease %s: %w", leaseID, err)
		}

		return a.archive(ctx, tx, lease, PhaseFailed)
	})
}

// ResolveQuarantineFor terminalizes every quarantined lease on a node that the
// node's own inventory does not mention, and reports how many.
//
// A NODE THAT COMES BACK IS THE EVIDENCE. It enumerates what it is actually
// running as part of registering; a quarantined lease missing from that list has
// no container, whether the host rebooted or the sweep already removed it.
// quarantineGrace is how long a lease stays quarantined before an ABSENCE from a
// node's inventory is believed.
//
// AN ABSENCE IS A SNAPSHOT, NOT A CAUSAL RESULT — the same rule custody's
// strayGrace exists for, and it applies here for the same reason. A launch whose
// response was lost may have left the daemon still creating the container, and a
// listing issued afterwards can overtake it and see nothing. Freeing the
// capacity on that hands the slot back moments before the container appears.
//
// Measured from when the lease EXPIRED, so a lease has already gone a full TTL
// without a heartbeat before this clock starts.
//
// GENEROUS ON PURPOSE, because the two errors are not symmetric. Too short sells
// a running machine's slot twice, which nothing recovers; too long returns the
// capacity later than it could have, which the next sweep fixes by itself and an
// operator can force. So this is sized for the slowest plausible create rather
// than the typical one: the docker provider launches with `docker run --detach`,
// which PULLS THE IMAGE INLINE, and a multi-gigabyte runner image on a slow link
// takes minutes rather than seconds.
const quarantineGrace = 5 * time.Minute

// inventoryAbsenceFailureReason marks a terminal outcome that inventory inferred
// from absence rather than one an operator or node reported independently. A
// later GitHub completion may correct this provisional verdict and no other.
const inventoryAbsenceFailureReason = "billet:provisional:inventory-absence"

func (a *Allocator) ResolveQuarantineFor(
	ctx context.Context, node string, running []string, epoch int64,
) (int, error) {
	held := make(map[string]bool, len(running))
	for _, id := range running {
		held[id] = true
	}

	var resolved int

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		resolved = 0

		// READ FULLY BEFORE WRITING. The caller runs more statements inside this
		// transaction, so the cursor has to be closed first — which is why the ids
		// are collected in their own function rather than iterated in place.
		// FENCED ON THE REGISTRATION THAT REPORTED IT. Two registrations can be in
		// flight — a node restarting twice, or a duplicate host — and the one that
		// arrives second is current. Letting an overtaken registration act on its
		// own stale inventory would terminalize a lease a newer one had just
		// vouched for, using a listing taken before that container was visible.
		q := state.WriteQueries(tx)

		current, err := q.ReadNodeEpoch(ctx, node)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read the epoch of node %s: %w", node, err)
		}

		if current != epoch {
			return nil
		}

		// RECORDED IN THE SAME TRANSACTION AS THE FENCE THAT ADMITS IT. This is
		// the one place both inventory paths converge — a registration and the
		// periodic /reconcile — and the epoch has just been proved current here.
		// Writing it anywhere else would be writing an observation whose fence was
		// checked separately, which is the shape this file exists to prevent.
		//
		// EVIDENCE, NOT PROOF. The node lists its provider and then posts, so
		// this records when the report ARRIVED and never when the snapshot was
		// taken, and nothing may treat it as clearance.
		if err := recordInventoryTx(ctx, q, node, epoch, len(running), a.now); err != nil {
			return err
		}

		ids, err := quarantinedIDsOn(ctx, q, node, ts(a.now().UTC().Add(-quarantineGrace)))
		if err != nil {
			return err
		}

		for _, id := range ids {
			if held[id] {
				continue
			}

			lease, err := quarantinedLeaseTx(ctx, tx, a, id)
			if err != nil {
				return err
			}

			// GITHUB'S OWN WORD, IF IT ALREADY ARRIVED, DECIDES THE OUTCOME. The
			// listener records a completion's result before it asks anything to
			// destroy the compute, so a lease whose teardown holder died with the
			// completion delivered carries the result and no longer needs a
			// provisional verdict: the job finished, the guest is gone, and the
			// lease's lifecycle ended tidily — `done`, exactly what the completion's
			// own correction would have written later. A reason something else
			// recorded still stands, because that earlier fact explains a failure
			// this absence does not.
			outcome := PhaseFailed
			if lease.FailureReason == "" {
				result, err := q.ReadJobResult(ctx, id)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return fmt.Errorf("alloc: read the recorded result of lease %s: %w", id, err)
				}

				if result != "" {
					outcome = PhaseDone
				} else {
					lease.FailureReason = inventoryAbsenceFailureReason
				}
			}

			// THE GUEST IS GONE AND BILLET DID NOT REMOVE IT, which is the
			// strongest thing an inventory can say — and it is recorded BEFORE the
			// row is terminalized below, because the phase filter in the guard
			// only admits a lease that still has compute behind it.
			//
			// Written here rather than derived from failure_reason later:
			// ResolveQuarantineForCompletion CLEARS that marker when GitHub's own
			// result corrects the provisional verdict, and the guest having
			// vanished is a fact that correction does not undo.
			//
			// THE IN-MEMORY LEASE IS ONLY UPDATED IF THE WRITE LANDED. The guard
			// keeps the FIRST observation, so a lease already carrying `reclaimed`
			// keeps it — and overwriting the loaded value unconditionally would
			// hand archive a token the row does not have, replacing a spot
			// interruption with an inventory absence in the history.
			observed := a.now().UTC()

			marked, err := markLeaseDisruptedTx(ctx, tx, id, DisruptionGuestAbsent, observed)
			if err != nil {
				return err
			}
			if marked {
				lease.Disruption, lease.DisruptedAt = DisruptionGuestAbsent, ts(observed)
			}

			if outcome == PhaseDone {
				if err := q.TerminalizeQuarantinedLease(ctx,
					ledgerdb.TerminalizeQuarantinedLeaseParams{
						Phase: string(PhaseDone),
						ID:    id,
					}); err != nil {
					return fmt.Errorf("alloc: resolve quarantined lease %s: %w", id, err)
				}
			} else if err := q.TerminalizeQuarantinedLeaseWithReason(ctx,
				ledgerdb.TerminalizeQuarantinedLeaseWithReasonParams{
					FailureReason: lease.FailureReason,
					ID:            id,
				}); err != nil {
				return fmt.Errorf("alloc: resolve quarantined lease %s: %w", id, err)
			}

			if err := a.archive(ctx, tx, lease, outcome); err != nil {
				return err
			}

			resolved++
		}

		return nil
	})

	return resolved, err
}

// quarantinedLeaseTx reads a lease and refuses one that is not quarantined.
func quarantinedLeaseTx(ctx context.Context, tx *sql.Tx, a *Allocator, leaseID string) (*Lease, error) {
	epoch, err := state.WriteQueries(tx).ReadLeaseEpoch(ctx, leaseID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s", ErrLeaseNotFound, leaseID)
	case err != nil:
		return nil, fmt.Errorf("alloc: read lease %s: %w", leaseID, err)
	}

	lease, err := a.loadAny(ctx, tx, leaseID, epoch)
	if err != nil {
		return nil, err
	}

	if lease.Phase != PhaseQuarantine {
		return nil, fmt.Errorf(
			"%w: lease %s is %s rather than quarantined, so there is nothing to resolve",
			ErrLeaseNotFound, leaseID, lease.Phase)
	}

	return lease, nil
}

// quarantinedIDsOn lists the quarantined leases attributed to one host, or only
// those that expired before `settled` when one is given.
func quarantinedIDsOn(
	ctx context.Context, q state.ReadOps, node, settled string,
) ([]string, error) {
	ids, err := q.ListQuarantinedLeaseIDsOn(ctx, ledgerdb.ListQuarantinedLeaseIDsOnParams{
		Phase:   string(PhaseQuarantine),
		Node:    node,
		Settled: settled,
	})
	if err != nil {
		return nil, fmt.Errorf("alloc: list quarantined leases on %s: %w", node, err)
	}

	return ids, nil
}

// ExpireForTest ages a lease out, so a test can drive the real reaper.
//
// A TEST THAT STAGES THE PHASE BY HAND PROVES NOTHING about the path it claims
// to protect: the reaper is what decides quarantine, and its rule about which
// phases keep their compute is exactly the thing worth testing. Moving the clock
// is the alternative, and it makes every helper take one.
func (a *Allocator) ExpireForTest(ctx context.Context, leaseID string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).ExpireLease(ctx, ledgerdb.ExpireLeaseParams{
			ExpiresAt: ts(a.now().UTC().Add(-time.Hour)),
			ID:        leaseID,
		}); err != nil {
			return fmt.Errorf("alloc: expire lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// Reconcile frees capacity held for compute a host says it is not running.
//
// THE IN-PROCESS SIDE OF THE NODE WIRE'S /reconcile, so a colocated node reaches
// the same code as a remote one. It reads the node's current epoch itself
// because there is no registration in flight to carry one — the caller IS the
// current incarnation by construction.
func (a *Allocator) Reconcile(ctx context.Context, node string, running []string) (int, error) {
	var epoch int64

	err := a.db.View(ctx, func(tx querier) error {
		var err error
		epoch, err = state.ReadQueries(tx).ReadNodeEpoch(ctx, node)

		return err
	})

	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Not in the fleet, so it holds nothing to free.
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("alloc: read the epoch of node %s: %w", node, err)
	}

	return a.ResolveQuarantineFor(ctx, node, running, epoch)
}

// AgeQuarantineForTest pushes a quarantined lease past the grace, so a test can
// reach the settled case without a clock.
func (a *Allocator) AgeQuarantineForTest(ctx context.Context, leaseID string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if err := state.WriteQueries(tx).ExpireLease(ctx, ledgerdb.ExpireLeaseParams{
			ExpiresAt: ts(a.now().UTC().Add(-2 * quarantineGrace)),
			ID:        leaseID,
		}); err != nil {
			return fmt.Errorf("alloc: age lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// recordInventoryTx stores what a host last said it was running.
//
// It is reached only on the path where the node VOUCHED for its list: the
// quarantine resolver refuses to act on an inventory a host could not read, so
// every count written here is one a host stood behind. A host that cannot see
// its provider posts nothing at all, and its previous row simply ages.
func recordInventoryTx(
	ctx context.Context,
	q state.WriteOps,
	node string,
	epoch int64,
	running int,
	now func() time.Time,
) error {
	if err := q.UpsertNodeInventory(ctx, ledgerdb.UpsertNodeInventoryParams{
		Node:       node,
		NodeEpoch:  epoch,
		ReceivedAt: ts(now().UTC()),
		Running:    int64(running),
	}); err != nil {
		return fmt.Errorf("alloc: record what node %s reports running: %w", node, err)
	}

	return nil
}

// NodeInventory is one host's last word about what it was running.
//
// IT IS TELEMETRY, NOT A VERDICT, and every field is shaped to keep it that
// way. The node lists its provider and THEN posts, so ReceivedAt is when the
// report arrived and never when the snapshot was taken, and a launch can be
// dispatched to that host immediately afterwards. A zero here is one host's
// opinion, already stale, about a question the ledger cannot answer.
type NodeInventory struct {
	Node string
	// Live is what the deployment believes about the host right now, which is a
	// different fact from anything in the report.
	Live bool
	// Report is what this host said under the registration the deployment is
	// talking to NOW, or nil if it has said nothing under it.
	//
	// A POINTER SO THE ABSENT CASE CANNOT BE READ AS A COUNT. An earlier shape
	// paired a Current bool with a Running int, and the natural thing to write
	// against that is `Current && Running == 0` — which is precisely the
	// clearance this must never be. There is no zero to reach for here without
	// first admitting the host said something.
	Report *InventoryReport
}

// InventoryReport is one host's own account of what it was running.
//
// EVERY FIELD IS ABOUT A MOMENT THAT HAS PASSED. The node lists its provider and
// THEN posts, so ReceivedAt is when this control plane learned of a snapshot
// taken some time before — and a launch can be handed to that host immediately
// afterwards, which is why this cannot be turned into proof.
type InventoryReport struct {
	// ReportedRunning is how many billet instances that snapshot contained.
	ReportedRunning int
	// ReceivedAt is when the CONTROL PLANE got the report, never when the host
	// took it.
	ReceivedAt string
}

// NodeInventories reports what every known host last said it was running.
//
// ON THE READ-ONLY POOL, because this answers a question an operator asks while
// the control plane is working, and a report must not reserve the writer slot.
func (a *Allocator) NodeInventories(ctx context.Context) ([]NodeInventory, error) {
	var out []NodeInventory

	err := a.db.View(ctx, func(q querier) error {
		// NULLABLE RATHER THAN COALESCED TO A SENTINEL. A host with no row and a
		// host reporting a number must not arrive here as the same type, or the
		// distinction has to be recovered by comparing against a magic value —
		// which is how an absent report becomes a zero somewhere downstream.
		rows, err := state.ReadQueries(q).ListNodeInventories(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list what the fleet reports running: %w", err)
		}

		for _, row := range rows {
			inv := NodeInventory{Node: row.Name, Live: row.Live == 1}

			// ONLY UNDER THE REGISTRATION IN FORCE. A report from before the host
			// reconnected is not this host's current word, and saying nothing is
			// not saying nothing is running.
			if row.NodeEpoch.Valid && row.Running.Valid && row.NodeEpoch.Int64 == row.Epoch {
				inv.Report = &InventoryReport{
					ReportedRunning: int(row.Running.Int64),
					ReceivedAt:      row.ReceivedAt.String,
				}
			}

			out = append(out, inv)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
