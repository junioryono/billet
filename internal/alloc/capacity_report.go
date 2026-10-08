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
// Idle is a launched lease whose runner's record says it has no job, Running one
// whose record says GitHub started a job on it.
type TierCapacity struct {
	Discovery, Pending, Launching, Idle, Running, Cleanup, Unknown int
	Floor, Headroom                                                int
	ObservedAt                                                     string
	ObservationError                                               string
	Listener                                                       ListenerCapacity
}

// CapacityReport reads ownership and charged phases in one ledger snapshot.
func (a *Allocator) CapacityReport(ctx context.Context, tier string) (TierCapacity, error) {
	if _, ok := a.tiers[tier]; !ok {
		return TierCapacity{}, fmt.Errorf("%w: %s", ErrUnknownTier, tier)
	}

	reports, err := a.capacityReports(ctx, []string{tier})

	return reports[tier], err
}

// CapacityReports is CapacityReport for every tier in the catalogue, from one
// ledger snapshot that reads the outstanding leases once rather than once per
// tier: what a scrape asks for, at a cost that grows with the fleet and not
// with the fleet times the tiers.
func (a *Allocator) CapacityReports(ctx context.Context) (map[string]TierCapacity, error) {
	labels := make([]string, 0, len(a.tiers))
	for label := range a.tiers {
		labels = append(labels, label)
	}

	slices.Sort(labels)

	return a.capacityReports(ctx, labels)
}

func (a *Allocator) capacityReports(ctx context.Context, labels []string) (map[string]TierCapacity, error) {
	out := make(map[string]*TierCapacity, len(labels))
	for _, label := range labels {
		out[label] = &TierCapacity{Floor: a.tiers[label].Reserved}
	}

	err := a.db.View(ctx, func(tx querier) error {
		for _, label := range labels {
			report := out[label]

			row, err := state.ReadQueries(tx).ReadListenerCapacity(ctx, label)
			// A FAILED READ IS NOT AN ABSENT OBSERVATION. A malformed snapshot can
			// leave ownership unknown, but a failed transaction cannot report phases.
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return fmt.Errorf("alloc: read listener capacity: %w", err)
			}
			if err == nil {
				if err := json.Unmarshal([]byte(row.Snapshot), &report.Listener); err != nil {
					report.ObservationError = err.Error()
					report.Listener = ListenerCapacity{}
				} else {
					report.ObservedAt = row.ObservedAt
				}
			}
		}

		rows, err := state.ReadQueries(tx).ListOutstandingLeases(ctx)
		if err != nil {
			return err
		}
		for _, lease := range rows {
			report, ok := out[lease.Tier]
			if !ok {
				continue
			}
			switch Phase(lease.Phase) {
			case PhaseCapacity:
				switch {
				case slices.Contains(report.Listener.Pending, lease.ID):
					report.Pending++
				case slices.Contains(report.Listener.Discovery, lease.ID):
					report.Discovery++
				default:
					report.Unknown++
				}
			case PhaseAssigned:
				report.Pending++
			case PhaseLaunching, PhaseOnline, PhaseBusy:
				member, found, err := poolRunnerByLease(ctx, state.ReadQueries(tx), lease.ID)
				if err != nil {
					return fmt.Errorf("alloc: read the pool runner of lease %s: %w", lease.ID, err)
				}
				report.countLaunched(Phase(lease.Phase), member, found, lease.Tier)
			case PhaseCustody, PhaseTeardown, PhaseQuarantine:
				report.Cleanup++
			}
		}

		for _, label := range labels {
			if out[label].Headroom, err = a.headroom(ctx, tx, a.tiers[label]); err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	reports := make(map[string]TierCapacity, len(out))
	for label, report := range out {
		reports[label] = *report
	}

	return reports, nil
}

// countLaunched classifies a lease of tier that has entered launch.
//
// A POOLED RUNNER'S PROGRESS IS ON ITS POOL RECORD, NOT ITS LEASE. Every launch
// registers a pool member, idle, and JobStarted marks it busy; the lease stays
// in `launching` throughout (StartPoolRunner), so the phase alone reported a
// busy fleet as one still starting up. A lease with no record is a launch the
// listener has not yet seen return. A record naming another tier, or a status
// this does not know, is unknown.
func (out *TierCapacity) countLaunched(phase Phase, member PoolRunner, found bool, tier string) {
	switch {
	case !found && phase == PhaseLaunching:
		out.Launching++
	case !found && phase == PhaseOnline:
		out.Idle++
	case !found:
		out.Running++
	case member.Tier != tier:
		out.Unknown++
	case member.Status == PoolRunnerIdle:
		out.Idle++
	case member.Status == PoolRunnerBusy:
		out.Running++
	case member.Status == PoolRunnerRetiring || member.Status == PoolRunnerRetired:
		out.Cleanup++
	default:
		out.Unknown++
	}
}
