package guestreport

import (
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

	var r Report

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
			r.Duplicates++
		default:
			r.Conflicts++
		}
	}

	slices.SortFunc(distinct, func(a, b *Batch) int {
		switch {
		case a.Seq < b.Seq:
			return -1
		case a.Seq > b.Seq:
			return 1
		default:
			return 0
		}
	})

	first := distinct[0]
	r.AgentVersion, r.TicksPerSecond = first.AgentVersion, first.TicksPerSecond
	r.FirstSeq, r.LastSeq = first.Seq, distinct[len(distinct)-1].Seq
	r.Stride = 1

	for i, b := range distinct {
		if i > 0 {
			r.account(distinct[i-1].Seq, b.Seq)
		}

		if !r.follows(b) {
			r.Refused++

			continue
		}

		r.keep(b)
	}

	return r, nil
}

// account counts the numbers between two received ones as missing.
func (r *Report) account(prev, next uint64) {
	if next == prev+1 {
		return
	}

	r.Missing += int64(next - prev - 1)
	if len(r.Gaps) < MaxReportGaps {
		r.Gaps = append(r.Gaps, Gap{FirstSeq: prev + 1, LastSeq: next - 1})
	}
}

// follows reports whether b continues what r has kept: the same agent, and no time
// or counter going back.
func (r *Report) follows(b *Batch) bool {
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

	return len(r.Steps) == 0 || len(b.Steps) == 0 ||
		b.Steps[0].AtSeconds >= r.Steps[len(r.Steps)-1].AtSeconds
}

// keep appends b to r, within the report's bounds for what is kept whole.
func (r *Report) keep(b *Batch) {
	r.Batches++
	r.AgentCPUTicks = b.AgentCPUTicks
	r.Samples = append(r.Samples, b.Samples...)

	for _, p := range b.Processes {
		p.Rows = slices.Clone(p.Rows)
		r.Processes = append(r.Processes, p)
	}

	for _, m := range b.Steps {
		if len(r.Steps) == MaxReportSteps {
			r.Dropped.Steps++

			continue
		}

		r.Steps = append(r.Steps, m)
	}

	names := 0
	for _, t := range r.Tests {
		names += len(t.Failed)
	}

	for _, t := range b.Tests {
		if len(r.Tests) == MaxReportSuites {
			r.Dropped.Suites++

			continue
		}

		room := MaxReportFailedNames - names
		if len(t.Failed) > room {
			r.Dropped.FailedNames += int64(len(t.Failed) - room)
			t.Failed = t.Failed[:room]
		}

		t.Failed = slices.Clone(t.Failed)
		if len(t.Failed) == 0 {
			t.Failed = nil
		}

		names += len(t.Failed)
		r.Tests = append(r.Tests, t)
	}
}
