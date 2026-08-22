package alloc

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/config"
)

// RegisteredNode is the durable placement identity of one compute host.
type RegisteredNode struct {
	Name     string
	Provider config.ProviderKind
	Site     string
	Live     bool
}

// RegisteredNodes lists every host the deployment has recorded, including an
// offline host whose placement identity is still part of the ledger.
func (a *Allocator) RegisteredNodes(ctx context.Context) ([]RegisteredNode, error) {
	var out []RegisteredNode
	err := a.db.View(ctx, func(q querier) error {
		rows, err := q.QueryContext(ctx,
			`SELECT name, provider, site, live
			   FROM nodes
			  ORDER BY name`)
		if err != nil {
			return fmt.Errorf("alloc: list registered nodes: %w", err)
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var node RegisteredNode
			var provider string
			var live int
			if err := rows.Scan(&node.Name, &provider, &node.Site, &live); err != nil {
				return fmt.Errorf("alloc: read registered node: %w", err)
			}
			node.Provider = config.ProviderKind(provider)
			if !node.Provider.Valid() {
				return fmt.Errorf("alloc: registered node %q has unknown provider %q", node.Name, provider)
			}
			if live != 0 && live != 1 {
				return fmt.Errorf("alloc: registered node %q has invalid liveness %d", node.Name, live)
			}
			node.Live = live == 1
			out = append(out, node)
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("alloc: list registered nodes: %w", err)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
