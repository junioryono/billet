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

// HoldLateFactor is how many times its kind's median a job runs before a hold on its host
// stops being the bound on a wait: a hold stays through ordinary noise in a prediction,
// and past this the waiter holds the line as under fair, whose bound is the first host
// that fits. Chosen from the record (docs/reference/records/replay-policies.md).
const HoldLateFactor = 6

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
	// Late says a job the shape needs gone has run HoldLateFactor times its kind's median.
	Late bool
}

// heldLease is what choosing a host needs of one open lease.
type heldLease struct {
	cost placementCost
	end  time.Time
	// stuck is a lease in custody or quarantine, which ends only on proof the compute
	// is gone or by an operator: room no prediction can promise.
	stuck bool
	// late is a job past HoldLateFactor times its kind's median.
	late bool
}

// HoldCandidates lists the hosts a tier could be held one of, the soonest ready first
// (ties by name), or none when its shape fits now on a host outside avoid or is not one
// a hold can help. When it fits now and holds own, own alone is listed: a pass the order
// refuses must not give up the room it accumulated.
//
// THE PREDICTION CHOOSES THE HOST AND NOTHING ELSE (ADR-027). Each host's leases are
// sorted by predicted end and their room added up until the shape fits. A wrong
// prediction picks a worse host and never a wrong outcome; a history that cannot be
// read predicts from what is left of it or the default, and picks a host all the
// same, because holding one still beats holding none.
//
// A HOST ALREADY RUNNING A LEASE AS LARGE AS THE SHAPE IS NOT A CANDIDATE: that lease
// ending is room the shape fits in at once, and holding it would close the host to
// everyone for work that frees it anyway. behindAsLarge reports that this is the whole
// reason there are none, on every host the tier could use, which is the one case where
// only a buyer as wide can keep the shape waiting. Otherwise a waiter with no candidate
// holds the line as under fair: a host that needs a lease in custody or quarantine to
// end is no candidate, a remote backend buys its shapes against a budget a hold does not
// keep, and a macOS slot is a licence rather than room, which a lease of another size
// frees all the same. A floor elsewhere is charged as a purchase's is.
func (a *Allocator) HoldCandidates(ctx context.Context, tier string, avoid map[string]string,
	own string,
) ([]HoldCandidate, bool, error) {
	t, ok := a.tiers[tier]
	if !ok {
		return nil, false, fmt.Errorf("%w: %q", ErrUnknownTier, tier)
	}

	if t.GuestOS == config.GuestMacOS {
		return nil, false, nil
	}

	priors, err := a.RuntimePriors(ctx)
	if err != nil && !errors.Is(err, ErrRuntimesStale) && ctx.Err() != nil {
		return nil, false, err
	}

	now := a.now()

	var out []HoldCandidate

	behindAsLarge := false

	err = a.db.View(ctx, func(tx querier) error {
		place, _, err := a.placerWithFloors(ctx, tx, t)
		if err != nil {
			return err
		}

		// READ OFF WHAT THE HOSTS HELD AGAINST IT LEAVE, as its purchase would be.
		open := place.clone()
		open.keepFor(avoid, a.tiers)

		fitsNow, fitsOwn := false, false

		for _, n := range place.order {
			switch {
			case open.roomFor(n.name, t) < 1:
			case n.name == own:
				fitsOwn = true
			default:
				fitsNow = true
			}
		}

		byNode, err := a.leasesByNode(ctx, tx, priors, now)
		if err != nil {
			return err
		}

		usable, asLarge := 0, 0

		for _, n := range place.order {
			cost, placeable := place.cost[n.name]
			if !placeable {
				continue
			}

			if !n.provider.RunsOnHost() {
				usable++

				continue
			}

			if !everFits(cost, place.freeVCPU[n.name], place.freeMemory[n.name], byNode[n.name]) {
				continue
			}

			usable++

			// ROOM ON ITS OWN HOST KEEPS IT, a lease as large beside that room or not.
			if (!fitsOwn || n.name != own) && runsAsLarge(cost, byNode[n.name]) {
				asLarge++

				continue
			}

			if fitsNow && n.name != own {
				continue
			}

			ready, late, ok := readyAt(cost, place.freeVCPU[n.name], place.freeMemory[n.name], byNode[n.name], now)
			if ok {
				out = append(out, HoldCandidate{Node: n.name, ReadyAt: ready, Late: late})
			}
		}

		behindAsLarge = !fitsNow && !fitsOwn && usable > 0 && usable == asLarge

		return nil
	})
	if err != nil {
		return nil, false, err
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

	return out, behindAsLarge, nil
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

		// NOT STARTED IS PREDICTED FROM NOW, as a lease not yet running is, and how late
		// it is runs from when it was bought, or one that never starts is never late.
		start, since := parseStamp(row.StartedAt), parseStamp(row.StartedAt)
		if start.IsZero() {
			start, since = now, parseStamp(row.CreatedAt)
		}

		est := priors.Estimate(JobKeyOf(row.Tier, row.Repo, row.WorkflowRef, row.JobName))
		end, _ := PredictEnd(est, start, now)

		out[row.Node] = append(out[row.Node], heldLease{
			cost: placementCost{vcpu: int(row.Vcpu), memory: config.ByteSize(row.Memory)}, end: end,
			stuck: row.Phase == string(PhaseCustody) || row.Phase == string(PhaseQuarantine),
			late:  !since.IsZero() && now.Sub(since) > HoldLateFactor*est.Median,
		})
	}

	return out, nil
}

// everFits reports that a shape costing cost would fit on a host with the given room once
// all its leases had gone: a host too small for it is not one it could use.
func everFits(cost placementCost, freeVCPU int, freeMemory config.ByteSize, leases []heldLease) bool {
	for i := range leases {
		freeVCPU += leases[i].cost.vcpu
		freeMemory += leases[i].cost.memory
	}

	return freeVCPU >= cost.vcpu && freeMemory >= cost.memory
}

// runsAsLarge reports that a lease as large as cost runs among these and will end soon
// enough to wait for. One in custody or quarantine may not end, and one run late may not
// for hours, so neither promises the shape anything: it may fit beside it instead.
func runsAsLarge(cost placementCost, leases []heldLease) bool {
	return slices.ContainsFunc(leases, func(l heldLease) bool {
		return !l.stuck && !l.late && l.cost.vcpu >= cost.vcpu && l.cost.memory >= cost.memory
	})
}

// readyAt is when a shape costing cost fits on a host with the given room, as its
// leases are predicted to end, whether a job it needs gone is late, and false if it
// never would there. A host with room now is ready now: it is only asked of when that
// host is held.
func readyAt(cost placementCost, freeVCPU int, freeMemory config.ByteSize, leases []heldLease,
	now time.Time,
) (time.Time, bool, bool) {
	if freeVCPU >= cost.vcpu && freeMemory >= cost.memory {
		return now, false, true
	}

	sorted := slices.Clone(leases)
	slices.SortStableFunc(sorted, func(x, y heldLease) int { return x.end.Compare(y.end) })

	late := false

	for i := range sorted {
		if sorted[i].stuck {
			continue
		}

		freeVCPU += sorted[i].cost.vcpu
		freeMemory += sorted[i].cost.memory
		late = late || sorted[i].late

		if freeVCPU >= cost.vcpu && freeMemory >= cost.memory {
			return sorted[i].end, late, true
		}
	}

	return time.Time{}, false, false
}
