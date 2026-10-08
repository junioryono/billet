package app

import (
	"context"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/metrics"
	"github.com/junioryono/billet/internal/state"
)

// MetricsSource is what a role serves beside the runtime's metrics: gauges it
// reads afresh at every scrape, and observers it attaches to what it runs.
type MetricsSource struct {
	name     string
	families []metrics.Family
	read     func(context.Context) (metrics.Snapshot, error)
	// attach registers the source's event metrics and hands them to what
	// reports them, once the endpoint is serving.
	attach func(*metrics.Registry) error
}

// The ledger's write metrics report through state.Observer, which the ledger
// declares and internal/metrics satisfies without either importing the other.
var _ state.Observer = (*metrics.LedgerObserver)(nil)

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
			"idle and running runners, cleanup, or unknown (a lease whose state could not be established: " +
			"capacity the listener's report does not classify, or a runner whose record names another tier " +
			"or a status this does not know)."},
	{Name: familyTierFloor, Labels: []string{"tier"},
		Help: "The tier's configured floor: capacity held for it whatever else is running."},
	{Name: familyTierHeadroom, Labels: []string{"tier"},
		Help: "How many more of the tier's runners the allocator would grant right now."},
	{Name: familyTierAdvertised, Labels: []string{"tier"},
		Help: "The capacity the tier's listener last told GitHub, as its last completed exchange recorded it."},
	{Name: familyTierWaiting, Labels: []string{"tier"},
		Help: "How much of GitHub's assigned work the tier could not buy capacity for, as its listener last reported."},
	{Name: familyTierWaitingFor, Labels: []string{"tier"},
		Help: "How long the tier has been waiting for capacity, while it is."},
	{Name: familyTierReportAge, Labels: []string{"tier"},
		Help: "How old the tier listener's last published report is; a stopped listener's report only ages."},
	{Name: familyNodes, Labels: []string{"state"},
		Help: "Registered hosts by state: live, offline, or decommissioned."},
}

// LedgerMetrics is the control plane's ledger as metrics: each tier's capacity
// report and the registered hosts, read at every scrape through the reader
// pool, never the writer slot scheduling uses; and how the writes themselves
// went, told by the ledger as they happen.
func (cp *ControlPlane) LedgerMetrics() MetricsSource {
	return ledgerSource(cp.allocator, cp.db, cp.cfg.Tiers)
}

func ledgerSource(a *alloc.Allocator, db *state.DB, tiers []config.Tier) MetricsSource {
	return MetricsSource{
		name:     "ledger",
		families: ledgerFamilies,
		read:     ledgerSnapshot(a, tiers, time.Now),
		attach: func(reg *metrics.Registry) error {
			o, err := reg.LedgerObserver()
			if err != nil {
				return err
			}

			db.Observe(o)

			return nil
		},
	}
}

func ledgerSnapshot(a *alloc.Allocator, tiers []config.Tier, now func() time.Time,
) func(context.Context) (metrics.Snapshot, error) {
	return func(ctx context.Context) (metrics.Snapshot, error) {
		snap := metrics.Snapshot{}

		add := func(family string, value float64, labels ...string) {
			snap[family] = append(snap[family], metrics.Sample{Labels: labels, Value: value})
		}

		// ONE SNAPSHOT FOR EVERY TIER, reading the fleet's leases once rather
		// than once per tier.
		reports, err := a.CapacityReports(ctx)
		if err != nil {
			return nil, err
		}

		// AFTER THE READ, so a report a listener published while it ran is not
		// younger than the clock it is compared with; one from a clock ahead of
		// this one is age zero, never negative.
		at := now()

		age := func(since time.Time) float64 { return max(at.Sub(since).Seconds(), 0) }

		for i := range tiers {
			label := tiers[i].Label

			report, ok := reports[label]
			if !ok {
				return nil, fmt.Errorf("tier %s has no capacity report", label)
			}

			// AN OBSERVATION THAT COULD NOT BE READ IS NOT ONE THAT SAYS NOTHING:
			// the source fails rather than report its tier idle.
			if report.ObservationError != "" {
				return nil, fmt.Errorf("tier %s: the listener's report could not be read: %s", label, report.ObservationError)
			}

			for state, n := range map[string]int{
				"discovery": report.Discovery, "pending": report.Pending, "launching": report.Launching,
				"idle": report.Idle, "running": report.Running, "cleanup": report.Cleanup, "unknown": report.Unknown,
			} {
				add(familyTierLeases, float64(n), label, state)
			}

			add(familyTierFloor, float64(report.Floor), label)
			add(familyTierHeadroom, float64(report.Headroom), label)

			// WHAT WAS NEVER RECORDED IS LEFT OUT, NOT REPORTED AS ZERO: a
			// listener that has published no report has said nothing about a
			// wait, and one that has completed no exchange has told GitHub
			// nothing billet knows of.
			observed, reported := parseLedgerTime(report.ObservedAt)
			if !reported {
				continue
			}

			add(familyTierReportAge, age(observed), label)
			add(familyTierWaiting, float64(report.Listener.Waiting), label)

			if report.Listener.Confirmed != nil {
				add(familyTierAdvertised, float64(*report.Listener.Confirmed), label)
			}

			if since, ok := parseLedgerTime(report.Listener.WaitingSince); ok && report.Listener.Waiting > 0 {
				add(familyTierWaitingFor, age(since), label)
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
