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

// Quarantined lists the leases holding capacity for compute nobody has accounted
// for, oldest first.
//
// WHAT AN OPERATOR LOOKS AT WHEN CAPACITY IS MISSING. A quarantined lease is the
// one thing that shrinks a fleet without anything having failed, so it has to be
// visible or the number is inexplicable.
func (a *Allocator) Quarantined(ctx context.Context) ([]QuarantinedLease, error) {
	var out []QuarantinedLease

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
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

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
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
func (a *Allocator) ResolveQuarantine(ctx context.Context, leaseID string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := quarantinedLeaseTx(ctx, tx, a, leaseID)
		if err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = 'failed', epoch = epoch + 1 WHERE id = ?`,
			leaseID); err != nil {
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
const quarantineGrace = time.Minute

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

			if _, err := tx.ExecContext(ctx,
				`UPDATE leases SET phase = 'failed', epoch = epoch + 1 WHERE id = ?`, id); err != nil {
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
func quarantinedIDsOn(ctx context.Context, tx *sql.Tx, node, settled string) ([]string, error) {
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
