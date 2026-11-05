package replay

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

// Policy is one admission order and placement a comparison replays a trace
// under, named as the table names it. An empty Placement keeps the fleet's.
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
	{Name: "hold", Order: config.AdmissionHold},
}

// RecordPolicies are what the record also measures, by name through
// BILLET_REPLAY_POLICY: each order with the placement it is judged beside.
var RecordPolicies = append(slices.Clone(DefaultPolicies),
	Policy{Name: "fair-tightest", Order: config.AdmissionFair, Placement: config.PlacementTightest},
	Policy{Name: "fill-tightest", Order: config.AdmissionFill, Placement: config.PlacementTightest},
	Policy{Name: "hold-tightest", Order: config.AdmissionHold, Placement: config.PlacementTightest},
)

// Comparison is one trace replayed under several policies, each report in the
// policies' order.
type Comparison struct {
	Policies []Policy
	Reports  []*Report
}

// Compare replays trace against fleet under each policy, one replay after
// another. Each replay is deterministic, so two comparisons of one trace give
// the same table (TestAComparisonIsReproducible).
//
// A REPLAY THE REPORT CANNOT SPEAK FOR FAILS THE COMPARISON rather than giving
// it numbers (unspeakable says which). A table built over a lost job would
// compare policies on the jobs that happened to survive.
func Compare(t *testing.T, fleet Fleet, trace Trace, policies []Policy, opts Options) Comparison {
	t.Helper()

	c := Comparison{Policies: policies}

	for _, p := range policies {
		f := fleet
		f.AdmissionOrder = p.Order

		if p.Placement != "" {
			f.Placement = p.Placement
		}

		r := Run(t, f, trace, opts)
		if why := r.unspeakable(); why != "" {
			t.Fatalf("replay: under %s %s", p.Name, why)
		}

		c.Reports = append(c.Reports, r)
	}

	return c
}

// unspeakable says why the report cannot speak for its replay, or "" when it
// can: an overcommit the ledger proves, a job the ledger lost, a start or a
// completion delivered and never recorded, or a job that never started or
// finished with no horizon to explain it.
func (r *Report) unspeakable() string {
	switch {
	case len(r.Violations) > 0:
		return fmt.Sprintf("the ledger proves an overcommit: %v", r.Violations)
	case len(r.Missing) > 0:
		return fmt.Sprintf("the ledger has no row for jobs %v", r.Missing)
	case len(r.LostStarts) > 0:
		return fmt.Sprintf("jobs %v were started by the scripted GitHub and the ledger never saw them start",
			r.LostStarts)
	case len(r.LostCompletions) > 0:
		return fmt.Sprintf("jobs %v were completed by the scripted GitHub and the ledger never saw them finish",
			r.LostCompletions)
	case r.Horizon.IsZero() && (len(r.Unstarted) > 0 || len(r.Unfinished) > 0):
		return fmt.Sprintf("jobs %v never started and %v never finished, with no horizon to say why",
			r.Unstarted, r.Unfinished)
	}

	return ""
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

		for vcpu := range r.QueuedByShape() {
			shapes[vcpu] = true
		}
	}

	b.WriteString("| Shape | Policy | Started | p50 | p90 | p99 | p99.99 | Max | Still queued | Oldest queued |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|---|\n")

	for _, vcpu := range slices.Sorted(maps.Keys(shapes)) {
		for i, r := range c.Reports {
			p := r.QueueWaitByShape()[vcpu]
			q := r.QueuedByShape()[vcpu]

			// A SHAPE THAT STARTED NOTHING HAS NO WAITS, not waits of zero.
			waits := "| — | — | — | — | —"
			if p.Count > 0 {
				waits = fmt.Sprintf("| %s | %s | %s | %s | %s", p.P50.Round(time.Second), p.P90.Round(time.Second),
					p.P99.Round(time.Second), p.P9999.Round(time.Second), p.Max.Round(time.Second))
			}

			oldest := "—"
			if q.Count > 0 {
				oldest = q.Oldest.Round(time.Second).String()
			}

			fmt.Fprintf(&b, "| %d vCPU | %s | %d %s | %d | %s |\n", vcpu, c.Policies[i].Name, p.Count, waits,
				q.Count, oldest)
		}
	}

	b.WriteString("\n| Policy | Started | Still queued | Jobs refused at least once | vCPU charged | " +
		"Memory charged | Empty host-hours | Empty while a job that fits waited | Over (hours) |\n")
	b.WriteString("|---|---|---|---|---|---|---|---|---|\n")

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

		// A REPLAY THAT CHARGED NOTHING HAS NO SHARES, not shares of zero.
		overTime := "| — | — | — | — | —"
		if _, _, ok := r.span(); ok {
			vcpu, memory := r.MeanUtilization()
			overTime = fmt.Sprintf("| %.0f%% | %.0f%% | %.1f | %.1f | %.2f", 100*vcpu, 100*memory, r.EmptyHostHours(),
				r.EmptyWhileQueuedHours(), r.SpanHours())
		}

		fmt.Fprintf(&b, "| %s | %d | %d | %d %s |\n", c.Policies[i].Name, started, len(r.StillQueued), declined,
			overTime)
	}

	b.WriteString("\nThe charged shares and the empty host-hours are over each policy's own span, from its first " +
		"charge to its last end (or the horizon): the same charged hours over a longer span are a smaller share.\n")

	return b.String()
}
