package alloc

import (
	"context"
	"fmt"
	"math"

	"github.com/junioryono/billet/internal/config"
)

// EC2CostNodes returns the declarations behind every registered EC2 node's
// compute-cost ceiling. Registration survives liveness loss because the cloud
// resources and their potential bill do too.
func (a *Allocator) EC2CostNodes(ctx context.Context) ([]config.EC2CostNode, error) {
	var out []config.EC2CostNode
	err := a.db.View(ctx, func(tx querier) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT name, total_vcpu, total_memory, ec2_shapes
			   FROM nodes
			  WHERE provider = ?
			  ORDER BY name`, config.ProviderEC2)
		if err != nil {
			return fmt.Errorf("alloc: list registered EC2 nodes for cost reporting: %w", err)
		}
		registeredRows := rows
		defer func() { _ = registeredRows.Close() }()

		prices := make(map[string]map[string]config.USDPerHour)
		nodeIndexes := make(map[string]int)
		for rows.Next() {
			var node config.EC2CostNode
			var name string
			var memory int64
			var shapes string
			if err := rows.Scan(&name, &node.MaxVCPU, &memory, &shapes); err != nil {
				return fmt.Errorf("alloc: read registered EC2 node for cost reporting: %w", err)
			}
			node.MaxMemory = config.ByteSize(memory)
			node.InstanceTypes, err = decodeEC2Shapes(shapes)
			if err != nil {
				return fmt.Errorf("alloc: read registered EC2 shapes for cost reporting: %w", err)
			}
			if len(node.InstanceTypes) == 0 {
				return fmt.Errorf("alloc: a registered EC2 node has no shape catalogue; " +
					"restart it with the current billet version so it re-registers its prices")
			}
			for i := range node.InstanceTypes {
				if node.InstanceTypes[i].PriceUSDPerHour <= 0 {
					return fmt.Errorf("alloc: a registered EC2 node has no price for shape %q; "+
						"restart it with the current billet version so it re-registers its prices",
						node.InstanceTypes[i].Type)
				}
			}
			prices[name] = make(map[string]config.USDPerHour, len(node.InstanceTypes))
			for i := range node.InstanceTypes {
				prices[name][node.InstanceTypes[i].Type] = node.InstanceTypes[i].PriceUSDPerHour
			}
			nodeIndexes[name] = len(out)
			out = append(out, node)
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("alloc: list registered EC2 nodes for cost reporting: %w", err)
		}
		if err := rows.Close(); err != nil {
			return fmt.Errorf("alloc: close registered EC2 nodes for cost reporting: %w", err)
		}
		rows, err = tx.QueryContext(ctx,
			`SELECT COALESCE(l.node, l.target_node), l.instance_type, COUNT(*)
			   FROM leases l
			   JOIN nodes n ON n.name = COALESCE(l.node, l.target_node)
			  WHERE n.provider = ? AND l.phase NOT IN ('done','failed')
			  GROUP BY COALESCE(l.node, l.target_node), l.instance_type`, config.ProviderEC2)
		if err != nil {
			return fmt.Errorf("alloc: list outstanding EC2 shapes for cost reporting: %w", err)
		}
		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var name, instanceType string
			var count int64
			if err := rows.Scan(&name, &instanceType, &count); err != nil {
				return fmt.Errorf("alloc: read outstanding EC2 shape for cost reporting: %w", err)
			}
			price, ok := prices[name][instanceType]
			if !ok || price <= 0 {
				return fmt.Errorf("alloc: outstanding EC2 work on node %q uses unknown shape %q",
					name, instanceType)
			}
			if count > math.MaxInt64/int64(price) {
				return fmt.Errorf("alloc: outstanding EC2 price overflows the supported dollar amount")
			}
			nodeIndex := nodeIndexes[name]
			amount := int64(price) * count
			if int64(out[nodeIndex].Outstanding) > math.MaxInt64-amount {
				return fmt.Errorf("alloc: outstanding EC2 price overflows the supported dollar amount")
			}
			out[nodeIndex].Outstanding += config.USDPerHour(amount)
		}

		if err := rows.Err(); err != nil {
			return fmt.Errorf("alloc: list outstanding EC2 shapes for cost reporting: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}
