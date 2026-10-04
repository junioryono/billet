package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// RecordCacheObservation writes what a node saw the cache do for a lease's job.
//
// FENCED ON THE EPOCH, because the observation arrives from the process holding
// the compute and a holder declared dead must not go on writing to a lease
// somebody else owns. Refuses a terminal lease: the history is closed then, and
// an archive already copied whatever the lease held.
//
// THE FIRST OBSERVATION IS KEPT, and the statements decide it: each column is
// written only while it is empty, so a repeat from a node retrying a lost
// response changes nothing and a later, different observation cannot replace
// what the guest first saw. Both rows are written in one transaction, the
// history row NOW rather than at archive, for the reason a disruption is (see
// applyDisruptionTx). A lease that never reached Assign has no history row and
// that half updates nothing, which is correct: it ran no job.
func (a *Allocator) RecordCacheObservation(
	ctx context.Context, leaseID string, epoch int64, obs CacheObservation,
) error {
	if err := obs.Validate(); err != nil {
		return err
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		q := state.WriteQueries(tx)

		if err := q.RecordLeaseCacheObservation(ctx, ledgerdb.RecordLeaseCacheObservationParams{
			ImageCache:      string(obs.ImageCache),
			CacheGeneration: obs.CacheGeneration,
			ActionsCache:    string(obs.ActionsCache),
			StickyCache:     string(obs.Sticky),
			GitCache:        string(obs.Git),
			BazelCache:      string(obs.Bazel),
			GoCache:         string(obs.Go),
			ID:              lease.ID,
			Epoch:           lease.Epoch,
		}); err != nil {
			return fmt.Errorf("alloc: record the cache observation of lease %s: %w", leaseID, err)
		}

		if err := q.RecordHistoryCacheObservation(ctx, ledgerdb.RecordHistoryCacheObservationParams{
			ImageCache:      string(obs.ImageCache),
			CacheGeneration: obs.CacheGeneration,
			ActionsCache:    string(obs.ActionsCache),
			StickyCache:     string(obs.Sticky),
			GitCache:        string(obs.Git),
			BazelCache:      string(obs.Bazel),
			GoCache:         string(obs.Go),
			LeaseID:         lease.ID,
		}); err != nil {
			return fmt.Errorf("alloc: record the cache observation in the history of lease %s: %w",
				leaseID, err)
		}

		return nil
	})
}

// CacheOutcomes counts, per tier and per cache, what each cache did for the
// jobs assigned since a moment: tier, then cache ("image", "actions",
// "sticky", "git", "bazel", "go"), then outcome, with "" for a job whose
// outcome was not observed. Jobs is how many rows were read, at most limit.
//
// ON THE READ-ONLY POOL, like every report.
func (a *Allocator) CacheOutcomes(
	ctx context.Context, since time.Time, limit int,
) (map[string]map[string]map[string]int, int, error) {
	if limit <= 0 {
		return nil, 0, fmt.Errorf("alloc: a cache report needs a positive limit, got %d", limit)
	}
	var counts map[string]map[string]map[string]int
	var jobs int
	err := a.db.View(ctx, func(tx querier) error {
		counts, jobs = map[string]map[string]map[string]int{}, 0
		rows, err := state.ReadQueries(tx).ListCacheOutcomes(ctx, ledgerdb.ListCacheOutcomesParams{
			Since:   sql.NullString{String: ts(since.UTC()), Valid: true},
			MaxRows: int64(limit),
		})
		if err != nil {
			return fmt.Errorf("alloc: list what the caches did: %w", err)
		}
		for _, row := range rows {
			tier := counts[row.Tier]
			if tier == nil {
				tier = map[string]map[string]int{}
				counts[row.Tier] = tier
			}
			for cache, outcome := range map[string]string{
				"image": row.ImageCache, "actions": row.ActionsCache, "sticky": row.StickyCache,
				"git": row.GitCache, "bazel": row.BazelCache, "go": row.GoCache,
			} {
				if tier[cache] == nil {
					tier[cache] = map[string]int{}
				}
				tier[cache][outcome]++
			}
		}
		jobs = len(rows)

		return nil
	})

	return counts, jobs, err
}

// JobPlacement is what one lease was charged for and what the cache did,
// from the history row that outlives the lease.
type JobPlacement struct {
	// Provider is the backend the lease ran on, empty for one that never bound.
	Provider config.ProviderKind
	// InstanceType is the shape placement bought, empty for a host-backed lease.
	InstanceType string
	// VCPU and Memory are what the lease was CHARGED: the shape for a remote
	// lease, the tier request for a host-backed one.
	VCPU   int
	Memory config.ByteSize
	// Site is the placed host's registered site at escrow.
	Site string
	// PriceUSDPerHour is the shape's price when it was charged. ZERO IS NOT A
	// PRICE: it is a host-backed lease that bought nothing, or a remote row
	// written before the price was recorded, and InstanceType tells the two
	// apart. A reader renders the second as unknown, never as $0.
	PriceUSDPerHour config.USDPerHour
	// ImageCache, CacheGeneration and ActionsCache are what the node observed.
	// Empty means nothing was observed; a token this binary does not recognise
	// is a newer binary's observation and is carried verbatim.
	ImageCache      ImageCache
	CacheGeneration string
	ActionsCache    ActionsCache
	BuildCaches
}

// HistoryPlacement reads what a lease was charged for from its history row.
//
// THE ONLY DURABLE STATEMENT ABOUT WHAT A JOB COST, which is what makes it the
// right thing for a test to assert against; the lease row it was copied from is
// reaped. ErrLeaseNotFound when the lease never had a history row.
func (a *Allocator) HistoryPlacement(ctx context.Context, leaseID string) (JobPlacement, error) {
	var out JobPlacement

	err := a.db.View(ctx, func(tx querier) error {
		row, err := state.ReadQueries(tx).ReadJobPlacement(ctx, leaseID)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s has no job history", ErrLeaseNotFound, leaseID)
		case err != nil:
			return fmt.Errorf("alloc: read the placement of lease %s: %w", leaseID, err)
		}

		out = JobPlacement{
			Provider:        config.ProviderKind(row.ChosenProvider),
			InstanceType:    row.InstanceType,
			VCPU:            int(row.Vcpu),
			Memory:          config.ByteSize(row.Memory),
			Site:            row.Site,
			PriceUSDPerHour: config.USDPerHour(row.PriceMicrosPerHour),
			ImageCache:      ImageCache(row.ImageCache),
			CacheGeneration: row.CacheGeneration,
			ActionsCache:    ActionsCache(row.ActionsCache),
			BuildCaches: BuildCaches{Sticky: BuildCache(row.StickyCache), Git: BuildCache(row.GitCache),
				Bazel: BuildCache(row.BazelCache), Go: BuildCache(row.GoCache)},
		}

		return nil
	})

	return out, err
}
