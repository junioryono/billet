package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// ImageCache is what the guest's Docker image-store clone did, as the node saw
// it.
//
// A CLOSED VOCABULARY AND AN OBSERVATION, like Disruption: it says what the
// cache did for one job and decides nothing. Recorded from what the node saw
// at the moment the guest asked, never from what the tier intended, because a
// field that says "warm" without saying who observed it is the could-not-tell
// collapse this repository keeps removing. The empty string means nothing was
// observed.
type ImageCache string

const (
	// ImageCacheWarm means the guest's image store was cloned from a published
	// generation, which CacheObservation.CacheGeneration names.
	ImageCacheWarm ImageCache = "warm"
	// ImageCacheCold means no generation existed for the store and a fresh volume
	// was created for the guest.
	ImageCacheCold ImageCache = "cold"
	// ImageCacheUnavailable means the site store failed and the job continued
	// with no image store at all.
	ImageCacheUnavailable ImageCache = "unavailable"
	// ImageCacheUnused means the cache session closed without the guest ever
	// asking for an image store.
	ImageCacheUnused ImageCache = "unused"
)

// Valid reports whether this is a token billet may write.
func (c ImageCache) Valid() bool {
	switch c {
	case ImageCacheWarm, ImageCacheCold, ImageCacheUnavailable, ImageCacheUnused:
		return true
	}

	return false
}

// ActionsCache is what the Actions cache interception did for the FIRST
// CacheService request a job made, as the node saw it.
//
// RECORDED WHEN THE DISPOSITION IS FINAL, not when it is intended: a call the
// site store failed to answer is retried through GitHub, and what the guest
// got was a splice, whatever billet set out to do.
type ActionsCache string

const (
	// ActionsCacheServed means the request was answered from the site store.
	ActionsCacheServed ActionsCache = "served"
	// ActionsCacheSpliced means interception was on and the request went
	// upstream to GitHub for a reason other than the kill switch: the policy
	// could not be read, or the client was not one billet serves locally.
	ActionsCacheSpliced ActionsCache = "spliced"
	// ActionsCacheDisabled means the central kill switch refused the request.
	ActionsCacheDisabled ActionsCache = "disabled"
	// ActionsCacheUnavailable means the request was bound to a reservation
	// billet holds, failed locally, and could not go upstream because GitHub
	// has nothing to answer it with; the guest was told the cache was
	// unavailable.
	ActionsCacheUnavailable ActionsCache = "unavailable"
	// ActionsCacheOff means the session had no interception at all: the tier does
	// not intercept, or the work was untrusted.
	ActionsCacheOff ActionsCache = "off"
	// ActionsCacheUnused means interception was on and the session closed
	// without a CacheService request ever arriving.
	ActionsCacheUnused ActionsCache = "unused"
)

// Valid reports whether this is a token billet may write.
func (c ActionsCache) Valid() bool {
	switch c {
	case ActionsCacheServed, ActionsCacheSpliced, ActionsCacheDisabled,
		ActionsCacheUnavailable, ActionsCacheOff, ActionsCacheUnused:
		return true
	}

	return false
}

// CacheObservation is what a node saw the cache do for one job. Either half
// may be empty, meaning that half has not been observed yet; a generation
// travels only with a warm image store.
type CacheObservation struct {
	ImageCache      ImageCache   `json:"image_cache,omitempty"`
	CacheGeneration string       `json:"cache_generation,omitempty"`
	ActionsCache    ActionsCache `json:"actions_cache,omitempty"`
}

// Validate refuses an observation billet must not write: an unknown token, a
// generation with no warm store to attribute it to, or nothing at all.
func (o CacheObservation) Validate() error {
	if o.ImageCache == "" && o.ActionsCache == "" {
		return errors.New("alloc: a cache observation must observe something")
	}
	if o.ImageCache != "" && !o.ImageCache.Valid() {
		return fmt.Errorf("alloc: %q is not an image-cache outcome billet records", o.ImageCache)
	}
	if o.ActionsCache != "" && !o.ActionsCache.Valid() {
		return fmt.Errorf("alloc: %q is not an Actions cache outcome billet records", o.ActionsCache)
	}
	if o.CacheGeneration != "" && o.ImageCache != ImageCacheWarm {
		return fmt.Errorf("alloc: a cache generation is recorded only beside a warm image store, not %q",
			o.ImageCache)
	}
	if o.ImageCache == ImageCacheWarm && o.CacheGeneration == "" {
		return errors.New("alloc: a warm image store must name the generation it was cloned from")
	}

	return nil
}

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
			ID:              lease.ID,
			Epoch:           lease.Epoch,
		}); err != nil {
			return fmt.Errorf("alloc: record the cache observation of lease %s: %w", leaseID, err)
		}

		if err := q.RecordHistoryCacheObservation(ctx, ledgerdb.RecordHistoryCacheObservationParams{
			ImageCache:      string(obs.ImageCache),
			CacheGeneration: obs.CacheGeneration,
			ActionsCache:    string(obs.ActionsCache),
			LeaseID:         lease.ID,
		}); err != nil {
			return fmt.Errorf("alloc: record the cache observation in the history of lease %s: %w",
				leaseID, err)
		}

		return nil
	})
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
		}

		return nil
	})

	return out, err
}
