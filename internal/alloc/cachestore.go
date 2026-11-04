package alloc

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
)

// RecordCacheStoreState keeps what a node's breaker says about its site's store, under
// the node's epoch now. TELEMETRY, like what a host says it is running: nothing but an
// operator reads it, and a breaker decides nothing about capacity, custody or teardown.
func (a *Allocator) RecordCacheStoreState(ctx context.Context, node string, report CacheStoreReport) error {
	if err := report.Validate(); err != nil {
		return err
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		written, err := state.WriteQueries(tx).UpsertNodeCacheStore(ctx, ledgerdb.UpsertNodeCacheStoreParams{
			Store:      report.Store,
			State:      string(report.State),
			Since:      ts(report.Since.UTC()),
			Failures:   int64(report.Failures),
			Reason:     string(report.Reason),
			ReceivedAt: ts(a.now().UTC()),
			Node:       node,
		})
		if err != nil {
			return fmt.Errorf("alloc: record what node %s says of its store: %w", node, err)
		}
		if written == 0 {
			return fmt.Errorf("alloc: record what node %s says of its store: no such node", node)
		}

		return nil
	})
}

// NodeCacheStore is what one host last said about its site's store.
type NodeCacheStore struct {
	Node string
	// Live is what the deployment believes about the host right now.
	Live bool
	CacheStoreReport
	// ReceivedAt is when the report arrived, on the control plane's clock.
	ReceivedAt time.Time
}

// NodeCacheStores is every host's last word about its store, under the registration
// in force.
func (a *Allocator) NodeCacheStores(ctx context.Context) ([]NodeCacheStore, error) {
	var out []NodeCacheStore

	err := a.db.View(ctx, func(q querier) error {
		rows, err := state.ReadQueries(q).ListNodeCacheStores(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list what the fleet says of its stores: %w", err)
		}
		for _, row := range rows {
			out = append(out, NodeCacheStore{
				Node: row.Name, Live: row.Live == 1, ReceivedAt: parseStamp(row.ReceivedAt),
				CacheStoreReport: CacheStoreReport{
					Store: row.Store, State: CacheStoreState(row.State), Since: parseStamp(row.Since),
					Failures: int(row.Failures), Reason: CacheStoreReason(row.Reason),
				},
			})
		}

		return nil
	})

	return out, err
}
