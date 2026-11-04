package alloc

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// holdLeaseBound is how many open leases one choice of a host reads. Far past any
// deployment measured; one past it has no candidates and says why, rather than
// choosing from the part it read.
const holdLeaseBound = 100000

// HoldCandidate is a host that could hold a waiting shape once enough of its
// running work ends.
type HoldCandidate struct {
	Node string
	// ReadyAt is when the shape is predicted to fit there.
	ReadyAt time.Time
}

// heldLease is what choosing a host needs of one open lease.
type heldLease struct {
	cost placementCost
	end  time.Time
	// stuck is a lease in custody or quarantine, which ends only on proof the compute
	// is gone or by an operator: room no prediction can promise.
	stuck bool
}

// HoldCandidates lists the hosts a tier could be held one of, the soonest ready
// first (ties by name), or none when its shape fits now on a host outside avoid other
// than own, the host it already holds, or is not one a hold can help.
//
// THE PREDICTION CHOOSES THE HOST AND NOTHING ELSE (ADR-023). Each host's leases are
// sorted by predicted end and their room added up until the shape fits. A wrong
// prediction picks a worse host and never a wrong outcome; a history that cannot be
// read predicts from what is left of it or the default, and picks a host all the
// same, because holding one still beats holding none.
//
// A HOST ALREADY RUNNING A LEASE AS LARGE AS THE SHAPE IS NOT A CANDIDATE: that lease
// ending is room the shape fits in at once, and holding it would close the host to
// everyone for work that frees it anyway. A waiter with no candidate holds the line
// instead, as under fair, so what it fits in is not sold from under it. A host that
// needs a lease in custody or quarantine to end is no candidate either. A remote
// backend buys its shapes and has no room to accumulate. A macOS slot is a licence
// rather than room, which a lease of another size frees all the same. A floor elsewhere
// is charged as a purchase's is.
func (a *Allocator) HoldCandidates(ctx context.Context, tier string, avoid []string,
	own string,
) ([]HoldCandidate, error) {
	t, ok := a.tiers[tier]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTier, tier)
	}

	if t.GuestOS == config.GuestMacOS {
		return nil, nil
	}

	priors, err := a.RuntimePriors(ctx)
	if err != nil && !errors.Is(err, ErrRuntimesStale) && ctx.Err() != nil {
		return nil, err
	}

	now := a.now()

	var out []HoldCandidate

	err = a.db.View(ctx, func(tx querier) error {
		place, _, err := a.placerWithFloors(ctx, tx, t)
		if err != nil {
			return err
		}

		for _, n := range place.order {
			if n.name != own && place.roomFor(n.name, t) > 0 && !slices.Contains(avoid, n.name) {
				return nil
			}
		}

		byNode, err := a.leasesByNode(ctx, tx, priors, now)
		if err != nil {
			return err
		}

		for _, n := range place.order {
			cost, placeable := place.cost[n.name]
			if !placeable || !n.provider.RunsOnHost() {
				continue
			}

			if ready, ok := readyAt(cost, place.freeVCPU[n.name], place.freeMemory[n.name], byNode[n.name], now); ok {
				out = append(out, HoldCandidate{Node: n.name, ReadyAt: ready})
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	slices.SortFunc(out, func(x, y HoldCandidate) int {
		if c := x.ReadyAt.Compare(y.ReadyAt); c != 0 {
			return c
		}

		if x.Node < y.Node {
			return -1
		}

		return 1
	})

	return out, nil
}

// leasesByNode reads every open lease with its predicted end, by host.
func (a *Allocator) leasesByNode(ctx context.Context, tx querier, priors *RuntimePriors,
	now time.Time,
) (map[string][]heldLease, error) {
	rows, err := state.ReadQueries(tx).ListOpenLeasesForHold(ctx, holdLeaseBound)
	if err != nil {
		return nil, fmt.Errorf("alloc: read open leases for a hold: %w", err)
	}

	if len(rows) >= holdLeaseBound {
		return nil, fmt.Errorf("alloc: %d open leases fill the read a hold is chosen from", len(rows))
	}

	out := map[string][]heldLease{}

	for i := range rows {
		row := &rows[i]

		// NOT STARTED IS PREDICTED FROM NOW, as a lease not yet running is.
		start := parseStamp(row.StartedAt)
		if start.IsZero() {
			start = now
		}

		est := priors.Estimate(JobKeyOf(row.Tier, row.Repo, row.WorkflowRef, row.JobName))
		end, _ := PredictEnd(est, start, now)

		out[row.Node] = append(out[row.Node], heldLease{
			cost: placementCost{vcpu: int(row.Vcpu), memory: config.ByteSize(row.Memory)}, end: end,
			stuck: row.Phase == string(PhaseCustody) || row.Phase == string(PhaseQuarantine),
		})
	}

	return out, nil
}

// readyAt is when a shape costing cost fits on a host with the given room, as its
// leases are predicted to end, and false if it never would there or if a lease as
// large as the shape runs there. A host with room now is ready now: it is only asked
// of when that host is held for another tier.
func readyAt(cost placementCost, freeVCPU int, freeMemory config.ByteSize, leases []heldLease,
	now time.Time,
) (time.Time, bool) {
	for i := range leases {
		if leases[i].cost.vcpu >= cost.vcpu && leases[i].cost.memory >= cost.memory {
			return time.Time{}, false
		}
	}

	if freeVCPU >= cost.vcpu && freeMemory >= cost.memory {
		return now, true
	}

	sorted := slices.Clone(leases)
	slices.SortStableFunc(sorted, func(x, y heldLease) int { return x.end.Compare(y.end) })

	for i := range sorted {
		if sorted[i].stuck {
			continue
		}

		freeVCPU += sorted[i].cost.vcpu
		freeMemory += sorted[i].cost.memory

		if freeVCPU >= cost.vcpu && freeMemory >= cost.memory {
			return sorted[i].end, true
		}
	}

	return time.Time{}, false
}
