package guestreport

// bounds is how many entries each section of a Batch or a Report may hold.
type bounds struct {
	samples, processes, steps, suites, failedNames int
}

var (
	batchBounds = bounds{MaxBatchSamples, MaxBatchProcessSamples, MaxBatchSteps,
		MaxBatchSuites, MaxBatchSuites * MaxFailedNames}
	reportBounds = bounds{MaxReportSamples, MaxReportProcessSamples, MaxReportSteps,
		MaxReportSuites, MaxReportFailedNames}
)

// sections is what a Batch and a Report both carry.
type sections struct {
	samples   []Sample
	processes []ProcessSample
	steps     []StepMark
	tests     []SuiteResult
}

func validateBatch(b *Batch) error {
	if b.Seq == 0 {
		return refuse(ErrEmpty, "seq")
	}

	if b.Seq > MaxValue {
		return refuse(ErrOutOfRange, "seq")
	}

	if err := validateHeader(b.AgentVersion, b.TicksPerSecond, b.AgentCPUTicks); err != nil {
		return err
	}

	return validateSections(sections{b.Samples, b.Processes, b.Steps, b.Tests}, batchBounds)
}

func validateReport(r *Report) error {
	if err := validateHeader(r.AgentVersion, r.TicksPerSecond, r.AgentCPUTicks); err != nil {
		return err
	}

	if err := validateAccount(r); err != nil {
		return err
	}

	if r.Stride < 1 || r.Stride > MaxStride {
		return refuse(ErrOutOfRange, "stride")
	}

	for _, d := range []struct {
		n     int64
		where string
	}{
		{r.Dropped.Steps, "dropped.steps"},
		{r.Dropped.Suites, "dropped.suites"},
		{r.Dropped.FailedNames, "dropped.failed_names"},
	} {
		if err := checkValue(d.n, d.where); err != nil {
			return err
		}
	}

	return validateSections(sections{r.Samples, r.Processes, r.Steps, r.Tests}, reportBounds)
}

func validateHeader(version string, ticks, agentCPU int64) error {
	if err := checkText(version, MaxNameBytes, "agent_version"); err != nil {
		return err
	}

	if ticks < 1 || ticks > MaxTicksPerSecond {
		return refuse(ErrOutOfRange, "ticks_per_second")
	}

	return checkValue(agentCPU, "agent_cpu_ticks")
}

// validateAccount holds a Report's account of its sequence numbers to arithmetic:
// every number from FirstSeq to LastSeq was kept, refused or missed, and the gaps
// are the missing ones in order, all of them unless MaxReportGaps cut the list.
func validateAccount(r *Report) error {
	if r.FirstSeq == 0 {
		return refuse(ErrEmpty, "first_seq")
	}

	if r.LastSeq > MaxValue {
		return refuse(ErrOutOfRange, "last_seq")
	}

	if r.LastSeq < r.FirstSeq {
		return refuse(ErrInconsistent, "last_seq")
	}

	if r.Batches < 1 {
		return refuse(ErrEmpty, "batches")
	}

	for _, c := range []struct {
		n     int64
		where string
	}{
		{r.Batches, "batches"}, {r.Refused, "refused"}, {r.Missing, "missing"},
		{r.Duplicates, "duplicates"}, {r.Conflicts, "conflicts"},
	} {
		if err := checkValue(c.n, c.where); err != nil {
			return err
		}
	}

	// Each of the three is at most 2^53, so the sum cannot overflow.
	if uint64(r.Batches+r.Refused+r.Missing) != r.LastSeq-r.FirstSeq+1 {
		return refuse(ErrInconsistent, "missing")
	}

	if len(r.Gaps) > MaxReportGaps {
		return refuse(ErrTooMany, "gaps")
	}

	var named uint64

	next := r.FirstSeq + 1 // the lowest number a gap may start at

	for i, g := range r.Gaps {
		where := at("gaps", i)
		if g.FirstSeq < next || g.LastSeq < g.FirstSeq || g.LastSeq >= r.LastSeq {
			return refuse(ErrInconsistent, where)
		}

		named += g.LastSeq - g.FirstSeq + 1
		next = g.LastSeq + 2
	}

	switch {
	case named > uint64(r.Missing):
		return refuse(ErrInconsistent, "gaps")
	case named < uint64(r.Missing) && len(r.Gaps) < MaxReportGaps:
		return refuse(ErrInconsistent, "gaps")
	}

	return nil
}

func validateSections(s sections, b bounds) error {
	if len(s.samples) > b.samples {
		return refuse(ErrTooMany, "samples")
	}

	for i := range s.samples {
		var prev *Sample
		if i > 0 {
			prev = &s.samples[i-1]
		}

		if err := validateSample(&s.samples[i], prev, at("samples", i)); err != nil {
			return err
		}
	}

	if len(s.processes) > b.processes {
		return refuse(ErrTooMany, "processes")
	}

	for i := range s.processes {
		var prevAt int64
		if i > 0 {
			prevAt = s.processes[i-1].AtMillis
		}

		if err := validateProcessSample(&s.processes[i], prevAt, at("processes", i)); err != nil {
			return err
		}
	}

	if len(s.steps) > b.steps {
		return refuse(ErrTooMany, "steps")
	}

	for i, m := range s.steps {
		where := at("steps", i)
		if err := checkText(m.Name, MaxNameBytes, where+".name"); err != nil {
			return err
		}

		if err := checkTime(m.AtSeconds, where+".at_s"); err != nil {
			return err
		}

		// Several steps may start in one second.
		if i > 0 && m.AtSeconds < s.steps[i-1].AtSeconds {
			return refuse(ErrNotMonotonic, where+".at_s")
		}
	}

	return validateTests(s.tests, b)
}

func validateSample(s, prev *Sample, where string) error {
	if err := checkTime(s.AtMillis, where+".at_ms"); err != nil {
		return err
	}

	if prev != nil && s.AtMillis <= prev.AtMillis {
		return refuse(ErrNotMonotonic, where+".at_ms")
	}

	for _, f := range sampleFields {
		if err := checkValue(f.get(s), where+"."+f.name); err != nil {
			return err
		}

		if f.counter && prev != nil && f.get(s) < f.get(prev) {
			return refuse(ErrNotMonotonic, where+"."+f.name)
		}
	}

	if s.CPUBusyTicks > s.CPUTotalTicks {
		return refuse(ErrInconsistent, where+".cpu_busy_ticks")
	}

	return nil
}

// sampleFields is Sample's numbers other than its time, and which are counters.
var sampleFields = []struct {
	name    string
	counter bool
	get     func(*Sample) int64
}{
	{"cpu_busy_ticks", true, func(s *Sample) int64 { return s.CPUBusyTicks }},
	{"cpu_total_ticks", true, func(s *Sample) int64 { return s.CPUTotalTicks }},
	{"mem_used_bytes", false, func(s *Sample) int64 { return s.MemUsedBytes }},
	{"mem_available_bytes", false, func(s *Sample) int64 { return s.MemAvailableBytes }},
	{"mem_cached_bytes", false, func(s *Sample) int64 { return s.MemCachedBytes }},
	{"disk_read_bytes", true, func(s *Sample) int64 { return s.DiskReadBytes }},
	{"disk_write_bytes", true, func(s *Sample) int64 { return s.DiskWriteBytes }},
	{"net_rx_bytes", true, func(s *Sample) int64 { return s.NetRxBytes }},
	{"net_tx_bytes", true, func(s *Sample) int64 { return s.NetTxBytes }},
}

func validateProcessSample(p *ProcessSample, prevAt int64, where string) error {
	if err := checkTime(p.AtMillis, where+".at_ms"); err != nil {
		return err
	}

	if p.AtMillis <= prevAt {
		return refuse(ErrNotMonotonic, where+".at_ms")
	}

	if len(p.Rows) > MaxProcessRows {
		return refuse(ErrTooMany, where+".rows")
	}

	seen := make(map[string]struct{}, len(p.Rows))

	for i := range p.Rows {
		row := &p.Rows[i]
		rowAt := at(where+".rows", i)

		if err := checkText(row.Name, MaxNameBytes, rowAt+".name"); err != nil {
			return err
		}

		if _, dup := seen[row.Name]; dup {
			return refuse(ErrDuplicateName, rowAt+".name")
		}

		seen[row.Name] = struct{}{}

		if row.Procs < 1 {
			return refuse(ErrEmpty, rowAt+".procs")
		}

		if err := checkUsage(&row.ProcessUsage, rowAt); err != nil {
			return err
		}
	}

	if err := checkUsage(&p.Other, where+".other"); err != nil {
		return err
	}

	if p.Other.Procs == 0 && p.Other != (ProcessUsage{}) {
		return refuse(ErrInconsistent, where+".other")
	}

	return checkValue(p.ResidualCPUTicks, where+".residual_cpu_ticks")
}

func checkUsage(u *ProcessUsage, where string) error {
	for _, f := range []struct {
		v    int64
		name string
	}{
		{u.Procs, "procs"}, {u.CPUTicks, "cpu_ticks"}, {u.RSSBytes, "rss_bytes"},
		{u.ReadBytes, "read_bytes"}, {u.WriteBytes, "write_bytes"},
	} {
		if err := checkValue(f.v, where+"."+f.name); err != nil {
			return err
		}
	}

	return nil
}

func validateTests(tests []SuiteResult, b bounds) error {
	if len(tests) > b.suites {
		return refuse(ErrTooMany, "tests")
	}

	names := 0

	for i := range tests {
		t := &tests[i]
		where := at("tests", i)

		if err := checkText(t.Name, MaxNameBytes, where+".name"); err != nil {
			return err
		}

		for _, c := range []struct {
			v    int64
			name string
		}{
			{t.Tests, "tests"}, {t.Failures, "failures"}, {t.Errors, "errors"},
			{t.Skipped, "skipped"},
		} {
			if err := checkValue(c.v, where+"."+c.name); err != nil {
				return err
			}
		}

		if len(t.Failed) > MaxFailedNames {
			return refuse(ErrTooMany, where+".failed")
		}

		names += len(t.Failed)
		if names > b.failedNames {
			return refuse(ErrTooMany, where+".failed")
		}

		for j, name := range t.Failed {
			if err := checkText(name, MaxFailedNameBytes, at(where+".failed", j)); err != nil {
				return err
			}
		}
	}

	return nil
}

func checkValue(v int64, where string) error {
	if v < 0 || v > MaxValue {
		return refuse(ErrOutOfRange, where)
	}

	return nil
}

// checkTime refuses a time that is unset as well as one out of range.
func checkTime(v int64, where string) error {
	if v == 0 {
		return refuse(ErrEmpty, where)
	}

	return checkValue(v, where)
}
