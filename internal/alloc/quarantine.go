package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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
func (a *Allocator) ResolveQuarantineFor(
	ctx context.Context, node string, running []string,
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
		ids, err := quarantinedIDsOn(ctx, tx, node)
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

// quarantinedIDsOn lists the quarantined leases attributed to one host.
func quarantinedIDsOn(ctx context.Context, tx *sql.Tx, node string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM leases WHERE phase = ? AND COALESCE(node, target_node) = ?`,
		string(PhaseQuarantine), node)
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
