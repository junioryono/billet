package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// nodeRow is a registered host as the ledger holds it.
type nodeRow struct {
	name     string
	provider config.ProviderKind
	site     string
	vcpu     int
	memory   config.ByteSize
	shapes   []config.EC2InstanceType
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
func (a *Allocator) eligibleNodes(ctx context.Context, tx querier, t config.Tier) ([]nodeRow, error) {
	rows, err := state.ReadQueries(tx).ListPlaceableNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("alloc: list nodes for tier %s: %w", t.Label, err)
	}

	accepts := t.AcceptableProviders()

	var out []nodeRow

	for i := range rows {
		n, err := nodeRowFrom(&rows[i])
		if err != nil {
			return nil, err
		}

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

		if _, ok := n.cost(t); !ok {
			continue
		}

		out = append(out, n)
	}

	return out, nil
}

// nodeRowFrom maps one placeable-host row onto what the arithmetic works with.
//
// SHARED BY eligibleNodes AND liveNodes, which read the same statement and
// differ only in what they do with the rows.
func nodeRowFrom(row *ledgerdb.ListPlaceableNodesRow) (nodeRow, error) {
	n := nodeRow{
		name:     row.Name,
		provider: config.ProviderKind(row.Provider),
		site:     row.Site,
		vcpu:     int(row.TotalVcpu),
		memory:   config.ByteSize(row.TotalMemory),
	}

	shapes, err := decodeRemoteShapes(n.provider, row.Ec2Shapes)
	if err != nil {
		return nodeRow{}, fmt.Errorf("alloc: read shapes for node %s: %w", n.name, err)
	}

	n.shapes = shapes

	return n, nil
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

// usageOn is what one host has already committed.
//
// COALESCE(node, target_node) IS THE ATTRIBUTION, and it is the same expression
// usageByNode uses for the whole fleet. A lease that has been bound is charged to the host
// running it; one that has only been AIMED at a host is charged there too,
// because capacity is escrowed before anything is advertised — a reservation
// that names a machine has committed that machine's room whether or not a
// container has started yet. Counting only bound leases would let a tier escrow
// against the same host repeatedly in the window before its first launch, which
// is the overcommit the escrow exists to prevent, moved from the deployment down
// to the machine.
func (a *Allocator) usageOn(ctx context.Context, tx querier, node string) (Usage, error) {
	row, err := state.ReadQueries(tx).UsageOnNode(ctx, node)
	if err != nil {
		return Usage{}, fmt.Errorf("alloc: read usage on node %s: %w", node, err)
	}

	return Usage{
		VCPU:   int(row.Vcpu),
		Memory: config.ByteSize(row.Memory),
		Leases: int(row.Leases),
	}, nil
}

// Stranded reports which of these leases their target machine can no longer
// honour — because it is gone, or because it is no longer big enough.
//
// FOR CAPACITY THAT HAS BEEN ADVERTISED BUT NOT USED. A listener holds escrow
// and tells GitHub about it; if the host those reservations name cannot keep
// them, the number is a promise nothing will honour, and the listener only ever
// ADDS to it. These are the ones it can safely take back.
//
// TWO WAYS TO BE STRANDED, and only the first was handled. A host that
// DISAPPEARS is the obvious one. A host that SHRINKS is the same failure with a
// quieter cause: capacity is deliberately overwritten on re-registration, so an
// operator who halves node.max_vcpu and restarts leaves the ledger recording a
// machine smaller than the escrow already aimed at it. It stays perfectly live,
// so a liveness question returns nothing, and billet goes on advertising slots
// that will fail to launch on arrival.
//
// ONLY THE EXCESS. Shedding every lease on an overcommitted host would give back
// capacity it can still honour, and the listener would immediately re-escrow it
// — advertisement flapping once per poll for as long as the host stayed small.
//
// A lease with no target is NOT stranded. Every reservation names a machine now,
// so an empty target is a row from before that was true — and guessing that such
// a lease is worthless is exactly the kind of cleanup that deletes something
// real. It fails closed by being left alone.
func (a *Allocator) Stranded(ctx context.Context, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var out []string

	err := a.db.View(ctx, func(tx querier) error {
		out = out[:0]

		// Candidates that survive the liveness question, grouped by the host they
		// are aimed at, so the overcommit below is measured per machine.
		onNode := make(map[string][]strandedCandidate)

		q := state.ReadQueries(tx)

		for _, id := range ids {
			// LEFT JOIN, because a target naming a host the ledger has never heard
			// of is the same situation as one it has forgotten: there is nowhere for
			// that reservation to go.
			row, err := q.ReadLeaseTargetSize(ctx, id)

			switch {
			case errors.Is(err, sql.ErrNoRows):
				// Already gone from the ledger; the caller will notice by other means.
				continue
			case err != nil:
				return fmt.Errorf("alloc: read the target of lease %s: %w", id, err)
			}

			if row.TargetNode.String == "" {
				continue
			}

			// live IS NULLABLE BECAUSE THE JOIN IS OUTER: a NULL is a target host
			// the ledger has no row for at all, which is the same situation as a
			// host it can no longer reach.
			if !row.Live.Valid || row.Live.Int64 != 1 {
				out = append(out, id)

				continue
			}

			onNode[row.TargetNode.String] = append(onNode[row.TargetNode.String],
				strandedCandidate{
					id:     id,
					vcpu:   int(row.Vcpu),
					memory: config.ByteSize(row.Memory),
				})
		}

		return a.shedOvercommit(ctx, tx, onNode, &out)
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// strandedCandidate is a held reservation on a live host, and what it costs that
// host if it stays.
type strandedCandidate struct {
	id     string
	vcpu   int
	memory config.ByteSize
}

// shedOvercommit adds enough of each host's candidates to the stranded list to
// bring it back inside what it now says it has.
//
// THE FEWEST LEASES THAT SETTLE IT, because every one shed is a slot billet stops
// advertising.
//
// SCORED AGAINST WHAT IS STILL MISSING, not against a fixed dimension. A host runs
// out of vCPU and memory independently, so no single ordering is right for both:
// the widest lease by vCPU may free almost no memory, and leading by either one
// when BOTH are short can take a lease that settles half the deficit and then have
// to take the other anyway. So each step asks what fraction of the REMAINING
// shortfall a candidate covers in both dimensions at once, re-asked each time as
// the deficits shrink. The id breaks ties, because Go map iteration is randomised
// and an advertisement that depends on it cannot be reproduced from a log.
//
// IT SEES ONLY ONE LISTENER'S LEASES, and that is safe in the direction that
// matters: a host overcommitted across several tiers may be trimmed by more than
// one of them and give back a little too much. Over-shedding costs a re-escrow on
// the next poll; under-shedding leaves billet advertising a slot that will fail on
// arrival. It converges — the second caller measures the ledger the first one
// already corrected.
func (a *Allocator) shedOvercommit(
	ctx context.Context, tx querier, onNode map[string][]strandedCandidate, out *[]string,
) error {
	nodes := make([]string, 0, len(onNode))
	for name := range onNode {
		nodes = append(nodes, name)
	}

	slices.Sort(nodes)

	for _, name := range nodes {
		var total nodeRow

		size, err := state.ReadQueries(tx).ReadNodeSize(ctx, name)
		if err != nil {
			return fmt.Errorf("alloc: read the size of node %s: %w", name, err)
		}

		total.vcpu = int(size.TotalVcpu)
		total.memory = config.ByteSize(size.TotalMemory)

		used, err := a.usageOn(ctx, tx, name)
		if err != nil {
			return err
		}

		overVCPU := used.VCPU - total.vcpu
		overMemory := used.Memory - total.memory

		if overVCPU <= 0 && overMemory <= 0 {
			continue
		}

		// BY ID FIRST, so that when two candidates score identically — the common
		// case, since a tier's leases are all the same shape — the one taken is
		// the same on every run.
		candidates := slices.Clone(onNode[name])
		slices.SortFunc(candidates, func(x, y strandedCandidate) int {
			return strings.Compare(x.id, y.id)
		})

		for len(candidates) > 0 && (overVCPU > 0 || overMemory > 0) {
			best := 0

			for i := 1; i < len(candidates); i++ {
				if candidates[i].covers(overVCPU, overMemory) >
					candidates[best].covers(overVCPU, overMemory) {
					best = i
				}
			}

			c := candidates[best]
			candidates = slices.Delete(candidates, best, best+1)

			*out = append(*out, c.id)
			overVCPU -= c.vcpu
			overMemory -= c.memory
		}
	}

	return nil
}

// covers is how much of a host's remaining shortfall this reservation would
// settle: the fraction of each deficit it closes, added together.
//
// A FRACTION RATHER THAN AN AMOUNT, because vCPU and bytes cannot be added.
// Normalising each dimension by what is still missing makes them comparable and
// says the useful thing directly — a lease that closes a deficit entirely scores
// 1 for it, whether that deficit is 2 vCPU or 200 GiB — so a candidate that
// settles both dimensions outscores every candidate that settles one.
//
// Surplus does not count. A 200 GiB reservation against a 10 GiB shortfall is
// worth exactly as much as a 10 GiB one, and letting it score higher would take
// back far more than the host needed.
func (c strandedCandidate) covers(overVCPU int, overMemory config.ByteSize) float64 {
	score := 0.0

	if overVCPU > 0 {
		score += float64(min(c.vcpu, overVCPU)) / float64(overVCPU)
	}

	if overMemory > 0 {
		score += float64(min(c.memory, overMemory)) / float64(overMemory)
	}

	return score
}

// liveNodes lists every reachable host, whatever any tier can use.
//
// THE WHOLE FLEET, because tiers compete for the same machines: a floor
// belonging to one tier is held on the hosts IT could use, which the asking
// tier may not share. Measuring only one tier's candidates left no entry for a
// machine it cannot use, so another tier's reservation had nowhere correct to go.
func (a *Allocator) liveNodes(ctx context.Context, tx querier) ([]nodeRow, error) {
	rows, err := state.ReadQueries(tx).ListPlaceableNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("alloc: list the fleet: %w", err)
	}

	var out []nodeRow

	for i := range rows {
		n, err := nodeRowFrom(&rows[i])
		if err != nil {
			return nil, err
		}

		out = append(out, n)
	}

	return out, nil
}
