package alloc

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ListenerCapacity is a dated observation, never permission to spend or release.
//
// CAPACITY PHASE DOES NOT MEAN IDLE. Discovery and pending name exact lease IDs
// from the listener's ownership sets; an unclassified row is reported as unknown.
// Sent is the outstanding attempt, Confirmed the last completed exchange. Neither
// is inferred from headroom, and an ambiguous exchange leaves Confirmed unchanged.
type ListenerCapacity struct {
	Discovery []string `json:"discovery"`
	Pending   []string `json:"pending"`
	Sent      *int     `json:"sent"`
	Confirmed *int     `json:"confirmed"`
	Exchange  string   `json:"exchange"`
	// Waiting is how much of GitHub's assigned work this tier could not buy
	// capacity for, and WaitingSince when it first could not. Zero and empty
	// mean nothing is waiting.
	//
	// THE ONLY RECORD OF A QUEUE THAT IS NOT BILLET'S. Nothing is reserved for
	// an idle tier any more, so a tier waiting for room is indistinguishable in
	// the ledger from a tier nobody wants: both hold nothing (#140). An
	// observation written by a control plane that predates these carries
	// neither, which reads as not waiting, so this needs no migration.
	Waiting      int    `json:"waiting,omitempty"`
	WaitingSince string `json:"waiting_since,omitempty"`
	// WaitingProgress is when this tier's listener last made admission
	// progress. A waiter holds other tiers back only while that is recent, and a
	// stalled listener cannot say it has stalled, so the reader compares this
	// with its own clock.
	WaitingProgress string `json:"waiting_progress,omitempty"`
}

// RecordListenerCapacity publishes the listener's ownership for another process.
func (a *Allocator) RecordListenerCapacity(ctx context.Context, tier string, report ListenerCapacity) error {
	body, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("alloc: encode listener capacity: %w", err)
	}
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		return state.WriteQueries(tx).RecordListenerCapacity(ctx, ledgerdb.RecordListenerCapacityParams{
			Tier: tier, ObservedAt: ts(a.now()), Snapshot: string(body),
		})
	})
}

// TierCapacity separates charged leases from guarantees and additional headroom.
// The observation's age is always shown: a stopped listener leaves a last report,
// not a claim that another process can infer GitHub's present advertisement.
//
// Idle is compute whose runner is registered with no job, Running compute whose
// runner GitHub says started one; together they are what a node reports running.
type TierCapacity struct {
	Discovery, Pending, Launching, Idle, Running, Cleanup, Unknown int
	Floor, Headroom                                                int
	ObservedAt                                                     string
	ObservationError                                               string
	Listener                                                       ListenerCapacity
}

// CapacityReport reads ownership and charged phases in one ledger snapshot.
func (a *Allocator) CapacityReport(ctx context.Context, tier string) (TierCapacity, error) {
	t, ok := a.tiers[tier]
	if !ok {
		return TierCapacity{}, fmt.Errorf("%w: %s", ErrUnknownTier, tier)
	}
	out := TierCapacity{Floor: t.Reserved}
	err := a.db.View(ctx, func(tx querier) error {
		row, err := state.ReadQueries(tx).ReadListenerCapacity(ctx, tier)
		// A FAILED READ IS NOT AN ABSENT OBSERVATION. A malformed snapshot can
		// leave ownership unknown, but a failed transaction cannot report phases.
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("alloc: read listener capacity: %w", err)
		}
		if err == nil {
			if err := json.Unmarshal([]byte(row.Snapshot), &out.Listener); err != nil {
				out.ObservationError = err.Error()
				out.Listener = ListenerCapacity{}
			} else {
				out.ObservedAt = row.ObservedAt
			}
		}
		rows, err := state.ReadQueries(tx).ListOutstandingLeases(ctx)
		if err != nil {
			return err
		}
		members, err := state.ReadQueries(tx).ListPoolRunnersInTier(ctx, tier)
		if err != nil {
			return fmt.Errorf("alloc: read the pool runners of tier %s: %w", tier, err)
		}
		pool := make(map[string]string, len(members))
		for i := range members {
			pool[members[i].LeaseID] = members[i].Status
		}
		for _, lease := range rows {
			if lease.Tier != tier {
				continue
			}
			switch Phase(lease.Phase) {
			case PhaseCapacity:
				switch {
				case slices.Contains(out.Listener.Pending, lease.ID):
					out.Pending++
				case slices.Contains(out.Listener.Discovery, lease.ID):
					out.Discovery++
				default:
					out.Unknown++
				}
			case PhaseAssigned:
				out.Pending++
			case PhaseLaunching, PhaseOnline, PhaseBusy:
				out.countLaunched(Phase(lease.Phase), pool, lease.ID)
			case PhaseCustody, PhaseTeardown, PhaseQuarantine:
				out.Cleanup++
			}
		}
		out.Headroom, err = a.headroom(ctx, tx, t)

		return err
	})

	return out, err
}

// countLaunched classifies a lease that has entered launch.
//
// A POOLED RUNNER'S PROGRESS IS ON ITS POOL RECORD, NOT ITS LEASE. Every launch
// registers a pool member, idle, and JobStarted marks it busy; the lease stays
// in `launching` throughout (StartPoolRunner), so the phase alone reported a
// busy fleet as one still starting up. A lease with no record is a launch the
// listener has not yet seen return, and a status this does not know is unknown.
func (out *TierCapacity) countLaunched(phase Phase, pool map[string]string, leaseID string) {
	status, member := pool[leaseID]
	switch {
	case !member && phase == PhaseLaunching:
		out.Launching++
	case !member && phase == PhaseOnline:
		out.Idle++
	case !member:
		out.Running++
	case status == PoolRunnerIdle:
		out.Idle++
	case status == PoolRunnerBusy:
		out.Running++
	case status == PoolRunnerRetiring || status == PoolRunnerRetired:
		out.Cleanup++
	default:
		out.Unknown++
	}
}
