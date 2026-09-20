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
type TierCapacity struct {
	Discovery, Pending, Launching, Running, Cleanup, Unknown int
	Floor, Headroom                                          int
	ObservedAt                                               string
	ObservationError                                         string
	Listener                                                 ListenerCapacity
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
			case PhaseLaunching:
				out.Launching++
			case PhaseOnline, PhaseBusy:
				out.Running++
			case PhaseCustody, PhaseTeardown, PhaseQuarantine:
				out.Cleanup++
			}
		}
		out.Headroom, err = a.headroom(ctx, tx, t)

		return err
	})

	return out, err
}
