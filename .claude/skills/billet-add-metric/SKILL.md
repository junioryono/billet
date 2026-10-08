---
name: billet-add-metric
description: "Load when adding a Prometheus metric to billet: where a gauge read at scrape time lives (a MetricsSource in internal/app) and where an event counter lives (an observer interface the owning package declares), why only internal/metrics imports the client, what a label may never carry, the could-not-tell rule for a failed read, and the docs and tests a metric needs."
---

# Adding a metric

A checklist. The endpoint's rules are in ADR-016 and `docs/operating/metrics.md`; the security rules in `billet-security` (`references/secrets.md`); the layering in `billet-checks-and-lint` (`references/layering-and-bans.md`).

1. **Gauge or event.** A value the ledger or a role already holds (a count, a phase, an age) is a gauge read at scrape time. Something that happens (a heartbeat, an escrow, a request) is an event counted where it happens. Prefer the gauge: it instruments no hot path and cannot drift from what `billet status` reports.
2. **A gauge** is a `metrics.Family` in a `MetricsSource` (`internal/app/metricsledger.go` is the control plane's), read through the reader pool (`DB.View`, which every allocator read uses), never the writer slot. Read the same report `billet status` reads where one exists, so the two never disagree. Leave out what was never recorded rather than reporting zero.
3. **An event** is counted through a small interface the owning package declares in plain types, with a no-op default, which `internal/metrics` satisfies without importing that package and `internal/app` connects. Only `internal/metrics` imports the Prometheus client (the `metrics` depguard rule), so a ledger writer never imports a metrics library.
4. **Names and labels.** `billet_<area>_<what>`, a unit suffix where there is one (`_seconds`, `_bytes`, `_total` for a counter). A label is a bounded set: a tier, a node, a state from a closed vocabulary. Never a lease or job id (unbounded cardinality), never anything a guest or a job wrote, never a credential or a path that could hold one.
5. **Could not tell is not zero.** A source whose read fails reports none of its families, and `billet_scrape_up{source}` says so; a dashboard sees a gap, never an empty fleet.
6. **Tests.** Assert the value from a real ledger (`ledgertest.Dir`), not that a sample exists. Every sample names a family its source declares, with that family's label count, or the collector drops it silently (`TestTheLedgerGaugesReadTheCapacityReportAndTheHosts` holds this). A metric wired into a role is proved used: `cmd/billet/metricswiring_test.go` holds which sources each role passes to `app.ServeMetrics`.
7. **Docs.** Add the metric to the table in `docs/operating/metrics.md`, with what it means and when it is absent.
