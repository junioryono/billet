package replay

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// Policy is one admission order and placement a comparison replays a trace
// under, named as the table names it.
type Policy struct {
	Name      string
	Order     config.AdmissionOrder
	Placement config.PlacementPolicy
}

// DefaultPolicies are the orders a deployment can choose today, each with the
// default placement.
var DefaultPolicies = []Policy{
	{Name: "fair", Order: config.AdmissionFair},
	{Name: "fill", Order: config.AdmissionFill},
}

// Comparison is one trace replayed under several policies, each report in the
// policies' order.
type Comparison struct {
	Policies []Policy
	Reports  []*Report
}

// Compare replays trace against fleet under each policy, one replay after
// another. Each replay is deterministic, so two comparisons of one trace give
// the same table (TestAComparisonIsReproducible).
func Compare(t *testing.T, fleet Fleet, trace Trace, policies []Policy, opts Options) Comparison {
	t.Helper()

	c := Comparison{Policies: policies}

	for _, p := range policies {
		f := fleet
		f.AdmissionOrder, f.Placement = p.Order, p.Placement

		c.Reports = append(c.Reports, Run(t, f, trace, opts))
	}

	return c
}

// Markdown is the comparison as tables a record can carry: per charged shape,
// each policy's queue-wait percentiles, then each policy's fleet-wide figures.
func (c Comparison) Markdown() string {
	var b strings.Builder

	shapes := map[int]bool{}
	for _, r := range c.Reports {
		for vcpu := range r.QueueWaitByShape() {
			shapes[vcpu] = true
		}
	}

	b.WriteString("| Shape | Policy | Jobs | p50 | p90 | p99 | p99.99 | Max |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|\n")

	for _, vcpu := range slices.Sorted(maps.Keys(shapes)) {
		for i, r := range c.Reports {
			p := r.QueueWaitByShape()[vcpu]
			fmt.Fprintf(&b, "| %d vCPU | %s | %d | %s | %s | %s | %s | %s |\n", vcpu, c.Policies[i].Name,
				p.Count, p.P50, p.P90, p.P99, p.P9999, p.Max)
		}
	}

	b.WriteString("\n| Policy | Started | Still queued | Purchases refused | vCPU charged | Memory charged | " +
		"Empty host-hours |\n")
	b.WriteString("|---|---|---|---|---|---|---|\n")

	for i, r := range c.Reports {
		started := 0
		for j := range r.Records {
			if !r.Records[j].StartedAt.IsZero() {
				started++
			}
		}

		declined := 0
		for _, n := range r.Declined {
			declined += n
		}

		vcpu, memory := r.MeanUtilization()
		fmt.Fprintf(&b, "| %s | %d | %d | %d | %.0f%% | %.0f%% | %.1f |\n", c.Policies[i].Name, started,
			len(r.StillQueued), declined, 100*vcpu, 100*memory, r.EmptyHostHours())
	}

	return b.String()
}
