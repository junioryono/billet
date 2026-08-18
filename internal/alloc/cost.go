package alloc

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/config"
)

// EC2CostNodes returns the declarations behind every registered EC2 node's
// compute-cost ceiling. Registration survives liveness loss because the cloud
// resources and their potential bill do too.
func (a *Allocator) EC2CostNodes(ctx context.Context) ([]config.EC2CostNode, error) {
	var out []config.EC2CostNode
	err := a.db.View(ctx, func(tx querier) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT total_vcpu, total_memory, ec2_shapes
			   FROM nodes
			  WHERE provider = ?
			  ORDER BY name`, config.ProviderEC2)
		if err != nil {
			return fmt.Errorf("alloc: list registered EC2 nodes for cost reporting: %w", err)
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var node config.EC2CostNode
			var memory int64
			var shapes string
			if err := rows.Scan(&node.MaxVCPU, &memory, &shapes); err != nil {
				return fmt.Errorf("alloc: read registered EC2 node for cost reporting: %w", err)
			}
			node.MaxMemory = config.ByteSize(memory)
			node.InstanceTypes, err = decodeEC2Shapes(shapes)
			if err != nil {
				return fmt.Errorf("alloc: read registered EC2 shapes for cost reporting: %w", err)
			}
			for i := range node.InstanceTypes {
				if node.InstanceTypes[i].PriceUSDPerHour <= 0 {
					return fmt.Errorf("alloc: a registered EC2 node has no price for shape %q; "+
						"restart it with the current billet version so it re-registers its prices",
						node.InstanceTypes[i].Type)
				}
			}
			out = append(out, node)
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("alloc: list registered EC2 nodes for cost reporting: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
