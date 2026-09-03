package alloc

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// ErrRemoteCostUnavailable means legacy registration data cannot support a fleet
// cost bound. Capacity and lease status remain valid.
var ErrRemoteCostUnavailable = errors.New("remote compute cost report unavailable")

// RemoteCostNodes returns the declarations behind every registered REMOTE node's
// compute-cost ceiling. Registration survives liveness loss because the cloud
// resources and their potential bill do too.
//
// EVERY REMOTE BACKEND, NOT JUST EC2, and the first version was scoped to
// `provider = 'ec2'`. A codebuild node declares ordered shapes with a price per hour
// for exactly the same reason an ec2 node does — placement charges the first that
// fits — so a deployment whose only cloud capacity was codebuild printed no cost line
// at all, which reads as a fleet that costs nothing. The set comes from
// config.RemoteProviders, so a third remote backend is included by the same allowlist
// that already decides how it is charged.
func (a *Allocator) RemoteCostNodes(ctx context.Context) ([]config.RemoteCostNode, error) {
	var out []config.RemoteCostNode
	err := a.db.View(ctx, func(tx querier) error {
		q := state.ReadQueries(tx)

		registered, err := q.ListRemoteCostNodes(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list registered remote nodes for cost reporting: %w", err)
		}

		prices := make(map[string]map[string]config.USDPerHour)
		nodeIndexes := make(map[string]int)
		for i := range registered {
			name := registered[i].Name
			node := config.RemoteCostNode{
				MaxVCPU:   int(registered[i].TotalVcpu),
				MaxMemory: config.ByteSize(registered[i].TotalMemory),
			}

			// THE ROW'S OWN PROVIDER, not a constant: decodeRemoteShapes names the
			// config field in its diagnostic, so passing ec2 for a codebuild row would
			// send an operator to node.ec2.instance_types, a key their config does not
			// have.
			node.Shapes, err = decodeRemoteShapes(
				config.ProviderKind(registered[i].Provider), registered[i].Ec2Shapes)
			if err != nil {
				return fmt.Errorf("alloc: read registered shapes for cost reporting: %w", err)
			}
			if len(node.Shapes) == 0 {
				return fmt.Errorf("%w: registered node %q has no shape catalogue; restart it with "+
					"the current billet version if it is still active", ErrRemoteCostUnavailable, name)
			}
			for i := range node.Shapes {
				if node.Shapes[i].PriceUSDPerHour <= 0 {
					return fmt.Errorf("%w: registered node %q has no price for shape %q; restart it "+
						"with the current billet version if it is still active", ErrRemoteCostUnavailable,
						name, node.Shapes[i].Type)
				}
			}
			prices[name] = make(map[string]config.USDPerHour, len(node.Shapes))
			for i := range node.Shapes {
				prices[name][node.Shapes[i].Type] = node.Shapes[i].PriceUSDPerHour
			}
			nodeIndexes[name] = len(out)
			out = append(out, node)
		}

		outstanding, err := q.ListOutstandingRemoteShapes(ctx)
		if err != nil {
			return fmt.Errorf("alloc: list outstanding remote shapes for cost reporting: %w", err)
		}

		for _, row := range outstanding {
			name, instanceType, count := row.Node.String, row.InstanceType, row.Outstanding
			price, ok := prices[name][instanceType]
			if !ok || price <= 0 {
				return fmt.Errorf("%w: outstanding work on node %q uses unknown shape %q",
					ErrRemoteCostUnavailable, name, instanceType)
			}
			if count > math.MaxInt64/int64(price) {
				return fmt.Errorf("alloc: outstanding remote price overflows the supported dollar amount")
			}
			nodeIndex := nodeIndexes[name]
			amount := int64(price) * count
			if int64(out[nodeIndex].Outstanding) > math.MaxInt64-amount {
				return fmt.Errorf("alloc: outstanding remote price overflows the supported dollar amount")
			}
			out[nodeIndex].Outstanding += config.USDPerHour(amount)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
