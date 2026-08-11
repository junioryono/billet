package alloc

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/junioryono/billet/internal/config"
)

// nodeRow is a registered host as the ledger holds it.
type nodeRow struct {
	name     string
	provider config.ProviderKind
	site     string
	vcpu     int
	memory   config.ByteSize
}

// eligibleNodes lists the live hosts a tier could actually be placed on.
//
// A FILTER, NOT A CHOICE. It answers "which machines may serve this tier at
// all", which is what the arithmetic and the placement both need; choosing among
// them is a separate question with its own ordering.
//
// ORDERED BY NAME, because Go map iteration is not and this list decides
// placement. An unordered candidate set makes the same fleet produce different
// answers on different runs, which is untestable and unexplainable in a log.
func (a *Allocator) eligibleNodes(ctx context.Context, tx *sql.Tx, t config.Tier) ([]nodeRow, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT name, provider, site, total_vcpu, total_memory
		   FROM nodes
		  WHERE live = 1 AND drained = 0
		  ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("alloc: list nodes for tier %s: %w", t.Label, err)
	}

	defer func() { _ = rows.Close() }()

	accepts := t.AcceptableProviders()

	var out []nodeRow

	for rows.Next() {
		var (
			n        nodeRow
			provider string
			memory   int64
		)

		if err := rows.Scan(&n.name, &provider, &n.site, &n.vcpu, &memory); err != nil {
			return nil, fmt.Errorf("alloc: read a node row: %w", err)
		}

		n.provider, n.memory = config.ProviderKind(provider), config.ByteSize(memory)

		// A PIN IS AN ALLOWLIST OF ONE, and it is checked first because it is the
		// operator's explicit instruction rather than an inference.
		if t.Node != "" && n.name != t.Node {
			continue
		}

		// A SITE CONFINES WITHOUT PINNING: the tier may run on any machine there,
		// which is what keeps the fallback a fleet buys.
		if t.Site != "" && n.site != t.Site {
			continue
		}

		// THE HOST'S REGISTERED BACKEND, not a catalogue's claim about it. A
		// firecracker lease cannot run on a Tart host whatever the config says.
		if !slicesContains(accepts, n.provider) {
			continue
		}

		if !a.allowsGuestOS(n.name, t.GuestOS) {
			continue
		}

		out = append(out, n)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alloc: list nodes for tier %s: %w", t.Label, err)
	}

	return out, nil
}

// slicesContains reports membership without importing slices for one call.
func slicesContains(haystack []config.ProviderKind, needle config.ProviderKind) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}

	return false
}

// headroomOn reports how many more of a tier fit on ONE host.
//
// THE SAME SHAPE AS headroom AND A DIFFERENT SUBJECT. Every limit that belongs
// to a MACHINE is applied here — its cores, its memory, and Apple's per-host
// guest licence. Limits that belong to a TIER — MaxConcurrent, an unmet floor —
// are not, because they bound the tier across the whole fleet and applying them
// once per candidate would deduct them as many times as there are machines.
//
// Capacity is a vector: enough cores says nothing about memory, and the smallest
// answer is the true one.
func (a *Allocator) headroomOn(
	ctx context.Context, tx *sql.Tx, n nodeRow, t config.Tier,
) (int, error) {
	used, err := a.usageOn(ctx, tx, n.name)
	if err != nil {
		return 0, err
	}

	byVCPU := (n.vcpu - used.VCPU) / t.VCPU
	byMemory := int((n.memory - used.Memory) / t.Memory)

	room := min(byVCPU, byMemory)

	// APPLE'S LIMIT BELONGS TO THE MAC, and it counts every macOS guest on that
	// host regardless of which tier asked for one. Two individually legal macOS
	// tiers on one machine still share one licence, so this is a property of the
	// node's headroom rather than of any tier's.
	//
	// A macOS tier always names its host — alloc.New refuses one that does not,
	// because the licence cannot be enforced without knowing whose it is — so the
	// host being counted is never in doubt.
	if t.GuestOS == config.GuestMacOS {
		guests, err := a.countOpenMacOSByNode(ctx, tx, n.name)
		if err != nil {
			return 0, err
		}

		room = min(room, a.macOSLimit(n.name)-guests)
	}

	return max(room, 0), nil
}

// usageOn is what one host has already committed.
//
// COALESCE(node, target_node) IS THE ATTRIBUTION, and it is the same expression
// countOpenMacOSByNode uses. A lease that has been bound is charged to the host
// running it; one that has only been AIMED at a host is charged there too,
// because capacity is escrowed before anything is advertised — a reservation
// that names a machine has committed that machine's room whether or not a
// container has started yet. Counting only bound leases would let a tier escrow
// against the same host repeatedly in the window before its first launch, which
// is the overcommit the escrow exists to prevent, moved from the deployment down
// to the machine.
func (a *Allocator) usageOn(ctx context.Context, tx *sql.Tx, node string) (Usage, error) {
	var (
		u    Usage
		vcpu sql.NullInt64
		mem  sql.NullInt64
	)

	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(vcpu), 0), COALESCE(SUM(memory), 0), COUNT(*)
		   FROM leases
		  WHERE phase NOT IN ('done','failed')
		    AND COALESCE(node, target_node) = ?`, node).
		Scan(&vcpu, &mem, &u.Leases)
	if err != nil {
		return u, fmt.Errorf("alloc: read usage on node %s: %w", node, err)
	}

	u.VCPU = int(vcpu.Int64)
	u.Memory = config.ByteSize(mem.Int64)

	return u, nil
}
