package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
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
}

// Held lists every operator-visible proof obligation, oldest first.
func (a *Allocator) Held(ctx context.Context) ([]HeldLease, error) {
	var out []HeldLease

	err := a.db.View(ctx, func(tx querier) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id, tier, COALESCE(node, target_node, ''), phase, vcpu, memory,
			        held_at, force_release
			   FROM leases
			  WHERE phase IN (?,?,?)
			  ORDER BY held_at, id`, PhaseCustody, PhaseTeardown, PhaseQuarantine)
		if err != nil {
			return fmt.Errorf("alloc: list held leases: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				h     HeldLease
				state string
				mem   int64
				force int
			)

			if err := rows.Scan(&h.ID, &h.Tier, &h.Node, &state, &h.VCPU, &mem,
				&h.Since, &force); err != nil {
				return fmt.Errorf("alloc: scan a held lease: %w", err)
			}

			h.State = Phase(state)
			h.Memory = config.ByteSize(mem)
			h.ForceRequested = force == 1
			out = append(out, h)
		}

		return rows.Err()
	})

	return out, err
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
		var epoch int64
		if err := tx.QueryRowContext(ctx, `SELECT epoch FROM leases WHERE id = ?`, leaseID).
			Scan(&epoch); err != nil {
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
			if _, err := tx.ExecContext(ctx,
				`UPDATE leases SET phase = ?, epoch = epoch + 1 WHERE id = ? AND epoch = ?`,
				string(PhaseFailed), leaseID, epoch); err != nil {
				return fmt.Errorf("alloc: force-release quarantined lease %s: %w", leaseID, err)
			}

			return a.archive(ctx, tx, lease, PhaseFailed)

		case PhaseCustody, PhaseTeardown:
			result.Pending = true
			if _, err := tx.ExecContext(ctx,
				`UPDATE leases SET force_release = 1 WHERE id = ? AND epoch = ?`,
				leaseID, epoch); err != nil {
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

		rows, err := tx.QueryContext(ctx,
			`SELECT id, tier, COALESCE(node, target_node, ''), vcpu, memory, heartbeat_at
			   FROM leases WHERE phase = ? ORDER BY heartbeat_at`, string(PhaseQuarantine))
		if err != nil {
			return fmt.Errorf("alloc: list quarantined leases: %w", err)
		}
		defer rows.Close()

		for rows.Next() {
			var (
				q   QuarantinedLease
				mem int64
			)

			if err := rows.Scan(&q.ID, &q.Tier, &q.Node, &q.VCPU, &mem, &q.Since); err != nil {
				return fmt.Errorf("alloc: scan a quarantined lease: %w", err)
			}

			q.Memory = config.ByteSize(mem)
			out = append(out, q)
		}

		return rows.Err()
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
		ids, err := quarantinedIDsOn(ctx, tx, node, "")
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

		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = ?, epoch = epoch + 1 WHERE id = ?`,
			string(outcome), leaseID); err != nil {
			return fmt.Errorf("alloc: resolve quarantined lease %s: %w", leaseID, err)
		}

		return a.archive(ctx, tx, lease, outcome)
	})
}

// ResolveQuarantineForCompletion settles one inventory-absent quarantined lease
// with the authoritative completion outcome. The node epoch fences the inventory
// snapshot; the stored lease epoch rejects a completion from an impossible future;
// an explicit failure marker distinguishes provisional inventory reconciliation
// from an independent terminal outcome. It reports whether capacity is already
// gone or was released by this call.
func (a *Allocator) ResolveQuarantineForCompletion(
	ctx context.Context,
	node, leaseID string,
	nodeEpoch, leaseEpoch int64,
	outcome Phase,
) (bool, error) {
	if !outcome.Terminal() {
		return false, fmt.Errorf("%w: %q is not terminal", ErrBadTransition, outcome)
	}

	settled := false
	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		var current int64
		switch err := tx.QueryRowContext(ctx,
			`SELECT epoch FROM nodes WHERE name = ?`, node).Scan(&current); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read the epoch of node %s: %w", node, err)
		}
		if current != nodeEpoch {
			return nil
		}

		var (
			phase         string
			currentEpoch  int64
			failureReason string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT phase, epoch, failure_reason FROM leases WHERE id = ?`, leaseID).
			Scan(&phase, &currentEpoch, &failureReason)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: inspect completion lease %s: %w", leaseID, err)
		}
		if errors.Is(err, sql.ErrNoRows) {
			// Capacity is gone, but without the lease row there is no provenance for
			// its history. Settled is safe; rewriting an unknown verdict is not.
			settled = true

			return nil
		}
		if currentEpoch < leaseEpoch {
			return nil
		}
		if Phase(phase).Terminal() {
			// ONLY INVENTORY'S PROVISIONAL VERDICT MAY BE CORRECTED. An operator or
			// node can independently fail a quarantined lease at a later epoch too,
			// so epoch order is a fence and never provenance.
			if failureReason == inventoryAbsenceFailureReason {
				if _, err := tx.ExecContext(ctx,
					`UPDATE leases SET phase = ?, failure_reason = '' WHERE id = ?`,
					string(outcome), leaseID); err != nil {
					return fmt.Errorf("alloc: correct completion lease %s: %w", leaseID, err)
				}
				if _, err := tx.ExecContext(ctx,
					`UPDATE job_history SET conclusion = ?, failure_reason = '' WHERE lease_id = ?`,
					string(outcome), leaseID); err != nil {
					return fmt.Errorf("alloc: correct completion history for lease %s: %w", leaseID, err)
				}
			}
			settled = true

			return nil
		}

		var eligible int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM leases
			 WHERE id = ? AND phase = ? AND COALESCE(node, target_node, '') = ?
			   AND heartbeat_at <= ?)`,
			leaseID, string(PhaseQuarantine), node,
			ts(a.now().UTC().Add(-quarantineGrace))).Scan(&eligible); err != nil {
			return fmt.Errorf("alloc: inspect quarantined completion lease %s: %w", leaseID, err)
		}
		if eligible == 0 {
			return nil
		}

		lease, err := quarantinedLeaseTx(ctx, tx, a, leaseID)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = ?, epoch = epoch + 1 WHERE id = ?`,
			string(outcome), leaseID); err != nil {
			return fmt.Errorf("alloc: resolve completion quarantine %s: %w", leaseID, err)
		}
		if err := a.archive(ctx, tx, lease, outcome); err != nil {
			return err
		}
		settled = true

		return nil
	})

	return settled, err
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
		var current int64

		switch err := tx.QueryRowContext(ctx,
			`SELECT epoch FROM nodes WHERE name = ?`, node).Scan(&current); {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read the epoch of node %s: %w", node, err)
		}

		if current != epoch {
			return nil
		}

		ids, err := quarantinedIDsOn(ctx, tx, node, ts(a.now().UTC().Add(-quarantineGrace)))
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
			if lease.FailureReason == "" {
				lease.FailureReason = inventoryAbsenceFailureReason
			}

			if _, err := tx.ExecContext(ctx,
				`UPDATE leases SET phase = 'failed', epoch = epoch + 1, failure_reason = ? WHERE id = ?`,
				lease.FailureReason, id); err != nil {
				return fmt.Errorf("alloc: resolve quarantined lease %s: %w", id, err)
			}

			if err := a.archive(ctx, tx, lease, PhaseFailed); err != nil {
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
	var epoch int64

	err := tx.QueryRowContext(ctx, `SELECT epoch FROM leases WHERE id = ?`, leaseID).Scan(&epoch)

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
func quarantinedIDsOn(ctx context.Context, tx querier, node, settled string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM leases
		  WHERE phase = ? AND COALESCE(node, target_node) = ?
		    AND (? = '' OR expires_at < ?)`,
		string(PhaseQuarantine), node, settled, settled)
	if err != nil {
		return nil, fmt.Errorf("alloc: list quarantined leases on %s: %w", node, err)
	}

	defer func() { _ = rows.Close() }()

	var ids []string

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("alloc: scan a quarantined lease: %w", err)
		}

		ids = append(ids, id)
	}

	return ids, rows.Err()
}

// ExpireForTest ages a lease out, so a test can drive the real reaper.
//
// A TEST THAT STAGES THE PHASE BY HAND PROVES NOTHING about the path it claims
// to protect: the reaper is what decides quarantine, and its rule about which
// phases keep their compute is exactly the thing worth testing. Moving the clock
// is the alternative, and it makes every helper take one.
func (a *Allocator) ExpireForTest(ctx context.Context, leaseID string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE leases SET expires_at = ? WHERE id = ?`,
			ts(a.now().UTC().Add(-time.Hour)), leaseID)
		if err != nil {
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
		return tx.QueryRowContext(ctx,
			`SELECT epoch FROM nodes WHERE name = ?`, node).Scan(&epoch)
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
		_, err := tx.ExecContext(ctx,
			`UPDATE leases SET expires_at = ? WHERE id = ?`,
			ts(a.now().UTC().Add(-2*quarantineGrace)), leaseID)
		if err != nil {
			return fmt.Errorf("alloc: age lease %s: %w", leaseID, err)
		}

		return nil
	})
}
