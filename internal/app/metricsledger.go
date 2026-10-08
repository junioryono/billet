package app

import (
	"context"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/metrics"
)

// MetricsSource is a set of gauges a role reads afresh at every scrape.
type MetricsSource struct {
	name     string
	families []metrics.Family
	read     func(context.Context) (metrics.Snapshot, error)
}

// ledgerScrapeTimeout bounds one scrape's reads of the ledger.
const ledgerScrapeTimeout = 5 * time.Second

// The control plane's gauges. Every one is a count or an age; no lease or job
// id is a label, so a series' cardinality is the tier list's and the fleet's.
const (
	familyTierLeases     = "billet_tier_leases"
	familyTierFloor      = "billet_tier_floor"
	familyTierHeadroom   = "billet_tier_headroom"
	familyTierAdvertised = "billet_tier_advertised"
	familyTierWaiting    = "billet_tier_waiting"
	familyTierWaitingFor = "billet_tier_waiting_seconds"
	familyTierReportAge  = "billet_tier_report_age_seconds"
	familyNodes          = "billet_nodes"
)

var ledgerFamilies = []metrics.Family{
	{Name: familyTierLeases, Labels: []string{"tier", "state"},
		Help: "Open leases by tier and what they are doing: discovery and pending capacity, launching, " +
			"idle and running runners, cleanup, or unknown (a capacity lease the listener's report does not classify)."},
	{Name: familyTierFloor, Labels: []string{"tier"},
		Help: "The tier's configured floor: capacity held for it whatever else is running."},
	{Name: familyTierHeadroom, Labels: []string{"tier"},
		Help: "How many more of the tier's runners the allocator would grant right now."},
	{Name: familyTierAdvertised, Labels: []string{"tier"},
		Help: "The capacity the tier's listener last told GitHub, as its last completed exchange recorded it."},
	{Name: familyTierWaiting, Labels: []string{"tier"},
		Help: "How much of GitHub's assigned work the tier could not buy capacity for."},
	{Name: familyTierWaitingFor, Labels: []string{"tier"},
		Help: "How long the tier has been waiting for capacity, while it is."},
	{Name: familyTierReportAge, Labels: []string{"tier"},
		Help: "How old the tier listener's last published report is; a stopped listener's report only ages."},
	{Name: familyNodes, Labels: []string{"state"},
		Help: "Registered hosts by state: live, offline, or decommissioned."},
}

// LedgerMetrics is the control plane's gauges, read from its ledger at every
// scrape: each tier's capacity report and the registered hosts. The reads go
// through the reader pool, never the writer slot scheduling uses.
func (cp *ControlPlane) LedgerMetrics() MetricsSource {
	return MetricsSource{
		name:     "ledger",
		families: ledgerFamilies,
		read:     ledgerSnapshot(cp.allocator, cp.cfg.Tiers, time.Now),
	}
}

func ledgerSnapshot(a *alloc.Allocator, tiers []config.Tier, now func() time.Time,
) func(context.Context) (metrics.Snapshot, error) {
	return func(ctx context.Context) (metrics.Snapshot, error) {
		snap := metrics.Snapshot{}
		at := now()

		add := func(family string, value float64, labels ...string) {
			snap[family] = append(snap[family], metrics.Sample{Labels: labels, Value: value})
		}

		for i := range tiers {
			label := tiers[i].Label

			report, err := a.CapacityReport(ctx, label)
			if err != nil {
				return nil, fmt.Errorf("tier %s: %w", label, err)
			}

			for state, n := range map[string]int{
				"discovery": report.Discovery, "pending": report.Pending, "launching": report.Launching,
				"idle": report.Idle, "running": report.Running, "cleanup": report.Cleanup, "unknown": report.Unknown,
			} {
				add(familyTierLeases, float64(n), label, state)
			}

			add(familyTierFloor, float64(report.Floor), label)
			add(familyTierHeadroom, float64(report.Headroom), label)
			add(familyTierWaiting, float64(report.Listener.Waiting), label)

			// WHAT WAS NEVER RECORDED IS LEFT OUT, NOT REPORTED AS ZERO: a
			// listener that has completed no exchange has told GitHub nothing
			// billet knows of.
			if report.Listener.Confirmed != nil {
				add(familyTierAdvertised, float64(*report.Listener.Confirmed), label)
			}

			if since, ok := parseLedgerTime(report.Listener.WaitingSince); ok && report.Listener.Waiting > 0 {
				add(familyTierWaitingFor, at.Sub(since).Seconds(), label)
			}

			if observed, ok := parseLedgerTime(report.ObservedAt); ok {
				add(familyTierReportAge, at.Sub(observed).Seconds(), label)
			}
		}

		nodes, err := a.RegisteredNodes(ctx)
		if err != nil {
			return nil, fmt.Errorf("registered nodes: %w", err)
		}

		counts := map[string]int{"live": 0, "offline": 0, "decommissioned": 0}

		for _, n := range nodes {
			switch {
			case n.Decommissioned != "":
				counts["decommissioned"]++
			case n.Live:
				counts["live"]++
			default:
				counts["offline"]++
			}
		}

		for state, n := range counts {
			add(familyNodes, float64(n), state)
		}

		return snap, nil
	}
}

// parseLedgerTime reads a timestamp the ledger or a listener recorded.
func parseLedgerTime(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}

	t, err := time.Parse(time.RFC3339Nano, s)

	return t, err == nil
}
