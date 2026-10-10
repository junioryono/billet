package guestreport

import (
	"cmp"
	"reflect"
	"slices"
)

// Merge is a job's batches as one Report, in sequence order. batches is in the order
// the node received them, each one DecodeBatch returned; one that would not decode
// is refused with the reason.
//
// A number received twice keeps the first one received and counts the other as a
// duplicate or, when it differs, a conflict. The lowest number's batch sets the
// agent version and the tick rate; a later batch that disagrees with it, or whose
// times or counters go back past the batches kept before it, is refused and counted.
// A number between the lowest and the highest that never arrived is counted as
// missing and named in Gaps. Steps, suites and failed names past the report's bounds
// are dropped and counted. The report is at full resolution: encode it through
// Downsample.
func Merge(batches []Batch) (Report, error) {
	if len(batches) == 0 {
		return Report{}, refuse(ErrNoBatches, "")
	}

	var m merger

	index := make(map[uint64]int, len(batches))
	distinct := make([]*Batch, 0, len(batches))

	for i := range batches {
		b := &batches[i]
		if err := validateBatch(b); err != nil {
			return Report{}, err
		}

		j, seen := index[b.Seq]
		switch {
		case !seen:
			index[b.Seq] = len(distinct)
			distinct = append(distinct, b)
		case reflect.DeepEqual(distinct[j], b):
			m.r.Duplicates++
		default:
			m.r.Conflicts++
		}
	}

	slices.SortFunc(distinct, func(a, b *Batch) int { return cmp.Compare(a.Seq, b.Seq) })

	first := distinct[0]
	m.r.AgentVersion, m.r.TicksPerSecond = first.AgentVersion, first.TicksPerSecond
	m.r.FirstSeq, m.r.LastSeq = first.Seq, distinct[len(distinct)-1].Seq
	m.r.Stride = 1

	for i, b := range distinct {
		if i > 0 {
			m.account(distinct[i-1].Seq, b.Seq)
		}

		if !m.follows(b) {
			m.r.Refused++

			continue
		}

		m.keep(b)
	}

	return m.r, nil
}

// merger is a Report being merged, and what it has seen that the report may not
// keep: the last step's time, which a step dropped past the bound still sets, and
// how many failed names the kept suites hold.
type merger struct {
	r        Report
	lastStep int64
	names    int
}

// account counts the numbers between two received ones as missing.
func (m *merger) account(prev, next uint64) {
	if next == prev+1 {
		return
	}

	m.r.Missing += int64(next - prev - 1)
	if len(m.r.Gaps) < MaxReportGaps {
		m.r.Gaps = append(m.r.Gaps, Gap{FirstSeq: prev + 1, LastSeq: next - 1})
	}
}

// follows reports whether b continues what was kept: the same agent, and no time
// or counter going back, a step dropped past the bound included.
func (m *merger) follows(b *Batch) bool {
	r := &m.r
	if b.AgentVersion != r.AgentVersion || b.TicksPerSecond != r.TicksPerSecond ||
		b.AgentCPUTicks < r.AgentCPUTicks {
		return false
	}

	if len(r.Samples) > 0 && len(b.Samples) > 0 &&
		validateSample(&b.Samples[0], &r.Samples[len(r.Samples)-1], "") != nil {
		return false
	}

	if len(r.Processes) > 0 && len(b.Processes) > 0 &&
		b.Processes[0].AtMillis <= r.Processes[len(r.Processes)-1].AtMillis {
		return false
	}

	return len(b.Steps) == 0 || b.Steps[0].AtSeconds >= m.lastStep
}

// keep appends b to the report, within its bounds for what is kept whole.
func (m *merger) keep(b *Batch) {
	r := &m.r
	r.Batches++
	r.AgentCPUTicks = b.AgentCPUTicks
	r.Samples = append(r.Samples, b.Samples...)

	for _, p := range b.Processes {
		p.Rows = slices.Clone(p.Rows)
		r.Processes = append(r.Processes, p)
	}

	for _, s := range b.Steps {
		m.lastStep = s.AtSeconds

		if len(r.Steps) == MaxReportSteps {
			r.Dropped.Steps++

			continue
		}

		r.Steps = append(r.Steps, s)
	}

	for _, t := range b.Tests {
		if len(r.Tests) == MaxReportSuites {
			r.Dropped.Suites++

			continue
		}

		room := MaxReportFailedNames - m.names
		if len(t.Failed) > room {
			r.Dropped.FailedNames += int64(len(t.Failed) - room)
			t.Failed = t.Failed[:room]
		}

		t.Failed = slices.Clone(t.Failed)
		if len(t.Failed) == 0 {
			t.Failed = nil
		}

		m.names += len(t.Failed)
		r.Tests = append(r.Tests, t)
	}
}
