package main

import (
	"fmt"
	"io"
	"slices"
)

// verdict is three-valued: a metric billet did not measure is UNMEASURED,
// never PASS, and neither is one whose comparison could not be made.
type verdict string

const (
	pass       verdict = "PASS"
	fail       verdict = "FAIL"
	unmeasured verdict = "UNMEASURED"
)

const mib = 1 << 20

// metric is one comparison between what a job expected and what billet
// measured, with its tolerance stated once, here, for the report and the
// runbook alike (docs/operating/measurement-validation.md says why each is
// what it is).
type metric struct {
	kind  string
	name  string // the expectation's key
	unit  string // "s" or "B"
	group string // the usage group billet must have measured
	// against is the kind of the same run whose record is the reference: the
	// job that did everything this one did except its load.
	against string
	value   func(job, ref *usage) float64
	bounds  func(expected, seconds float64, ref *usage) (low, high float64)
	rule    string
}

func cpuSeconds(u *usage) float64 { return float64(u.CPUUserMicros+u.CPUSystemMicros) / 1e6 }

var metrics = []metric{
	{
		kind: kindIdle, name: "cpu_seconds", unit: "s", group: "cpu", against: kindBaseline,
		value: func(job, ref *usage) float64 { return cpuSeconds(job) - cpuSeconds(ref) },
		bounds: func(_, seconds float64, _ *usage) (float64, float64) {
			return -3, 0.02*seconds + 3
		},
		rule: "idle minus baseline CPU within [-3 s, 0.02 CPU x T + 3 s]",
	},
	{
		kind: kindCPU, name: "cpu_seconds", unit: "s", group: "cpu", against: kindIdle,
		value: func(job, ref *usage) float64 { return cpuSeconds(job) - cpuSeconds(ref) },
		bounds: func(e, _ float64, _ *usage) (float64, float64) {
			return 0.95*e - 2, 1.05*e + 2
		},
		rule: "cpu minus idle CPU within [0.95 N x T - 2 s, 1.05 N x T + 2 s]",
	},
	{
		kind: kindMemory, name: "memory_peak_bytes", unit: "B", group: "memory", against: kindIdle,
		value: func(job, _ *usage) float64 { return float64(job.MemoryPeakBytes) },
		bounds: func(e, _ float64, ref *usage) (float64, float64) {
			return e, float64(ref.MemoryPeakBytes) + 1.05*e + 64*mib
		},
		rule: "peak within [X, idle peak + 1.05 X + 64 MiB]",
	},
	{
		kind: kindNetwork, name: "net_rx_bytes", unit: "B", group: "net", against: kindIdle,
		value: func(job, ref *usage) float64 { return float64(job.NetRxBytes - ref.NetRxBytes) },
		bounds: func(e, _ float64, _ *usage) (float64, float64) {
			return e - 4*mib, 1.06*e + 4*mib
		},
		rule: "received minus idle within [S - 4 MiB, 1.06 S + 4 MiB]",
	},
	{
		kind: kindDisk, name: "disk_write_bytes", unit: "B", group: "io", against: kindIdle,
		value: func(job, ref *usage) float64 { return float64(job.DiskWriteBytes - ref.DiskWriteBytes) },
		bounds: func(e, _ float64, _ *usage) (float64, float64) {
			return e - 64*mib, 1.10*e + 64*mib
		},
		rule: "written minus idle within [X - 64 MiB, 1.10 X + 64 MiB]",
	},
}

func metricFor(kind string) (metric, bool) {
	i := slices.IndexFunc(metrics, func(m metric) bool { return m.kind == kind })
	if i < 0 {
		return metric{}, false
	}

	return metrics[i], true
}

// result is one metric of one job.
type result struct {
	run      runKey
	kind     string
	lease    string
	metric   metric
	verdict  verdict
	value    float64
	expected float64
	low      float64
	high     float64
	against  string
	reason   string
	jobID    string
}

// recordSource answers billet's record for a lease.
type recordSource func(lease string) (record, error)

// evaluate compares every loaded job of every run with its expectation. The
// baseline is only ever a reference, so it yields no result of its own.
func evaluate(exps []expectation, records recordSource) []result {
	byRun := map[runKey]map[string]*expectation{}
	for i := range exps {
		e := &exps[i]
		if byRun[e.run()] == nil {
			byRun[e.run()] = map[string]*expectation{}
		}
		byRun[e.run()][e.Kind] = e
	}
	var out []result
	for i := range exps {
		e := &exps[i]
		m, ok := metricFor(e.Kind)
		if !ok {
			continue
		}
		out = append(out, evaluateOne(e, m, byRun[e.run()], records))
	}

	return out
}

func evaluateOne(e *expectation, m metric, run map[string]*expectation, records recordSource) result {
	r := result{run: e.run(), kind: e.Kind, lease: e.Lease, metric: m,
		expected: float64(e.Expected[m.name]), verdict: unmeasured}

	job, v, why := measuredRecord(e, m.group, records)
	if v != "" {
		r.verdict, r.reason = v, why

		return r
	}
	r.jobID = jobIDNote(e.GitHubJobID, job.GitHubJobID)

	refExp, ok := run[m.against]
	if !ok {
		r.reason = fmt.Sprintf("no %s job in %s to subtract", m.against, e.run())

		return r
	}
	r.against = refExp.Lease
	ref, v, why := measuredRecord(refExp, m.group, records)
	if v != "" {
		// A REFERENCE THAT FAILS ITS IDENTITY FAILS THIS ONE TOO: what would be
		// subtracted is another job's.
		r.verdict, r.reason = v, fmt.Sprintf("the %s job %s: %s", m.against, refExp.Lease, why)

		return r
	}

	r.value = m.value(job.Usage, ref.Usage)
	r.low, r.high = m.bounds(r.expected, float64(e.Seconds), ref.Usage)
	r.verdict = fail
	if r.value >= r.low && r.value <= r.high {
		r.verdict = pass
	}

	return r
}

// measuredRecord reads a lease's record and, when it cannot be compared, says
// what that makes the result and why. A record naming another run is a FAIL:
// the lease is not the job the expectation describes, which is a finding
// rather than a gap. Everything else that stops a comparison is UNMEASURED.
func measuredRecord(e *expectation, group string, records recordSource) (record, verdict, string) {
	rec, err := records(e.Lease)
	if err != nil {
		return record{}, unmeasured, err.Error()
	}
	switch {
	case rec.RunID == 0:
		return record{}, unmeasured, "billet recorded no run id for this lease, so it cannot be tied to the job"
	case rec.RunID != e.RunID:
		return record{}, fail, fmt.Sprintf("billet says lease %s ran run %d, and the job says run %d",
			e.Lease, rec.RunID, e.RunID)
	case rec.Usage == nil:
		return record{}, unmeasured, "billet recorded no usage report for this lease"
	case !rec.Usage.Measured[group]:
		return record{}, unmeasured, fmt.Sprintf("billet did not measure %s for this lease", group)
	}

	return rec, "", ""
}

// jobIDNote compares GitHub's job id as the job saw it with the one billet
// recorded. It is a note and never a verdict: the lease ties the two together,
// and whether the scale-set message's job id is the check run id has not been
// measured.
func jobIDNote(seen, recorded string) string {
	switch {
	case seen == "" || recorded == "":
		return "github job id: not known to both sides"
	case seen == recorded:
		return "github job id " + seen + " agrees"
	}

	return fmt.Sprintf("github job id: the job saw %s, billet recorded %q", seen, recorded)
}

// discardRuns drops the first n runs (by run id, then attempt): the warmup.
func discardRuns(exps []expectation, n int) ([]expectation, []runKey) {
	var runs []runKey
	for i := range exps {
		if k := exps[i].run(); !slices.Contains(runs, k) {
			runs = append(runs, k)
		}
	}
	slices.SortFunc(runs, compareRuns)
	if n > len(runs) {
		n = len(runs)
	}
	dropped := runs[:n]
	kept := slices.DeleteFunc(slices.Clone(exps), func(e expectation) bool {
		return slices.Contains(dropped, e.run())
	})

	return kept, dropped
}

// overall is the worst verdict: any FAIL fails, and otherwise anything
// unmeasured makes the whole could-not-tell.
func overall(results []result) verdict {
	v := pass
	for i := range results {
		switch results[i].verdict {
		case fail:
			return fail
		case unmeasured:
			v = unmeasured
		case pass:
		}
	}

	return v
}

func formatValue(v float64, unit string) string {
	if unit == "B" {
		return fmt.Sprintf("%.1f MiB", v/mib)
	}

	return fmt.Sprintf("%.1f s", v)
}

// report writes each run's results and then, per metric, the spread across
// runs.
func report(w io.Writer, results []result, dropped []runKey) {
	for _, k := range dropped {
		fmt.Fprintf(w, "discarded %s (warmup)\n", k)
	}
	var current runKey
	for i := range results {
		r := &results[i]
		if i == 0 || r.run != current {
			current = r.run
			fmt.Fprintf(w, "\n%s\n", r.run)
		}
		fmt.Fprintf(w, "  %-8s %-17s %-10s lease %s\n", r.kind, r.metric.name, r.verdict, r.lease)
		if r.verdict == unmeasured || (r.verdict == fail && r.reason != "") {
			fmt.Fprintf(w, "           %s\n", r.reason)
		} else {
			fmt.Fprintf(w, "           measured %s, expected %s, accepted [%s, %s] (%s; reference %s)\n",
				formatValue(r.value, r.metric.unit), formatValue(r.expected, r.metric.unit),
				formatValue(r.low, r.metric.unit), formatValue(r.high, r.metric.unit), r.metric.rule, r.against)
		}
		if r.jobID != "" {
			fmt.Fprintf(w, "           %s\n", r.jobID)
		}
	}

	fmt.Fprintf(w, "\nsummary\n")
	for _, m := range metrics {
		var values, ratios []float64
		counts := map[verdict]int{}
		for i := range results {
			r := &results[i]
			if r.kind != m.kind {
				continue
			}
			counts[r.verdict]++
			if r.verdict == unmeasured {
				continue
			}
			values = append(values, r.value)
			if r.expected > 0 {
				ratios = append(ratios, r.value/r.expected)
			}
		}
		total := counts[pass] + counts[fail] + counts[unmeasured]
		if total == 0 {
			continue
		}
		fmt.Fprintf(w, "  %-8s %d run(s): %d PASS, %d FAIL, %d UNMEASURED\n", m.kind, total,
			counts[pass], counts[fail], counts[unmeasured])
		if len(values) > 0 {
			s := summarize(values)
			fmt.Fprintf(w, "           %s: mean %s, 95%% CI %s, CV %s, n %d\n", m.name,
				formatValue(s.mean, m.unit), s.ciText(func(v float64) string { return formatValue(v, m.unit) }),
				s.cvText(), s.n)
		}
		if len(ratios) > 0 {
			s := summarize(ratios)
			fmt.Fprintf(w, "           measured/expected: mean %.4f, 95%% CI %s, CV %s\n", s.mean,
				s.ciText(func(v float64) string { return fmt.Sprintf("%.4f", v) }), s.cvText())
		}
	}
	fmt.Fprintf(w, "\noverall %s\n", overall(results))
}
