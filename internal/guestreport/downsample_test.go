package guestreport

import (
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// processTotals is every process figure a report's process samples add up to.
type processTotals struct {
	cpu, read, write, residual int64
}

func sumProcesses(ps []ProcessSample) processTotals {
	var s processTotals

	for _, p := range ps {
		for _, row := range append(slices.Clone(p.Rows), ProcessRow{ProcessUsage: p.Other}) {
			s.cpu += row.CPUTicks
			s.read += row.ReadBytes
			s.write += row.WriteBytes
		}

		s.residual += p.ResidualCPUTicks
	}

	return s
}

// busyBatches is n batches of a guest running many processes: forty names a sample,
// so every sample folds some into Other, and every halving folds again.
func busyBatches(n uint64) []Batch {
	rng := rand.New(rand.NewPCG(1, 2))

	var out []Batch

	for seq := uint64(1); seq <= n; seq++ {
		b := batchAt(seq)
		for i := range b.Processes {
			var rows []ProcessRow
			for j := range 40 {
				rows = append(rows, ProcessRow{Name: fmt.Sprintf("proc-%02d", j), ProcessUsage: ProcessUsage{
					Procs: 1 + rng.Int64N(3), CPUTicks: rng.Int64N(200), RSSBytes: rng.Int64N(1 << 30),
					ReadBytes: rng.Int64N(1 << 20), WriteBytes: rng.Int64N(1 << 20),
				}})
			}

			b.Processes[i].Rows, b.Processes[i].Other = mustFold(rows)
		}

		out = append(out, b)
	}

	return out
}

// DOWNSAMPLING FITS THE BOUND, LOSES NO TOTAL, AND LEAVES THE WHOLE SECTIONS WHOLE.
func TestDownsampleKeepsTheTotalsAndTheWholeSections(t *testing.T) {
	t.Parallel()

	r := mustMerge(t, busyBatches(120))

	const bound = 24 << 10

	if data, err := Encode(r); err == nil && len(data) <= bound {
		t.Fatalf("the twenty minutes already fit %d bytes in %d, so nothing here halves it", bound, len(data))
	}

	got, data, err := Downsample(r, bound)
	if err != nil {
		t.Fatal(err)
	}

	if len(data) > bound {
		t.Fatalf("%d bytes, over %d", len(data), bound)
	}

	decoded, err := Decode(data)
	if err != nil || !reflect.DeepEqual(decoded, got) {
		t.Fatalf("the encoding is not the report returned (%v)", err)
	}

	if got.Stride < 2 || len(got.Samples) >= len(r.Samples) || len(got.Processes) >= len(r.Processes) {
		t.Fatalf("stride %d kept %d of %d samples and %d of %d process samples",
			got.Stride, len(got.Samples), len(r.Samples), len(got.Processes), len(r.Processes))
	}

	// THE COUNTERS' TOTALS: the first and last samples are kept, and every kept
	// sample is one of the agent's.
	if got.Samples[0] != r.Samples[0] || got.Samples[len(got.Samples)-1] != r.Samples[len(r.Samples)-1] {
		t.Fatal("the first or last sample was not kept")
	}

	for _, s := range got.Samples {
		if !slices.Contains(r.Samples, s) {
			t.Fatalf("sample at %d is not one the agent took", s.AtMillis)
		}
	}

	if want, have := sumProcesses(r.Processes), sumProcesses(got.Processes); want != have {
		t.Fatalf("the process totals were %+v and are %+v", want, have)
	}

	if got.Processes[len(got.Processes)-1].AtMillis != r.Processes[len(r.Processes)-1].AtMillis {
		t.Fatal("the process samples no longer end where the agent's did")
	}

	for i, p := range got.Processes {
		if len(p.Rows) > KeepProcessesByName {
			t.Fatalf("process sample %d holds %d names", i, len(p.Rows))
		}
	}

	whole := r
	whole.Samples, whole.Processes, whole.Stride = got.Samples, got.Processes, got.Stride

	if !reflect.DeepEqual(got, whole) {
		t.Fatal("something other than the samples changed: the steps, the tests, the agent's CPU or the account")
	}
}

// A REPORT THAT FITS IS RETURNED AS IT IS, at stride 1.
func TestDownsampleLeavesAFittingReportAlone(t *testing.T) {
	t.Parallel()

	r := mustMerge(t, batchesThrough(6))

	got, data, err := Downsample(r, MaxReportBytes)
	if err != nil || !reflect.DeepEqual(got, r) || got.Stride != 1 {
		t.Fatalf("stride %d, changed %v (%v)", got.Stride, !reflect.DeepEqual(got, r), err)
	}

	if want, err := Encode(r); err != nil || !slices.Equal(data, want) {
		t.Fatalf("the encoding returned is not Encode's (%v)", err)
	}
}

// A MERGE PAST THE SAMPLE BOUNDS is halved below them before it is encoded: a job
// longer than nine hours of one-second samples.
func TestDownsampleBringsALongJobUnderTheSampleBounds(t *testing.T) {
	t.Parallel()

	n := uint64(MaxReportSamples/10 + 10)
	r := mustMerge(t, batchesThrough(n))

	if len(r.Samples) <= MaxReportSamples {
		t.Fatalf("%d samples is not past the bound", len(r.Samples))
	}

	if _, err := Encode(r); !errors.Is(err, ErrTooMany) {
		t.Fatalf("a report past the bound encoded: %v", err)
	}

	got, _, err := Downsample(r, MaxReportBytes)
	if err != nil {
		t.Fatal(err)
	}

	if len(got.Samples) > MaxReportSamples || got.Samples[len(got.Samples)-1] != r.Samples[len(r.Samples)-1] {
		t.Fatalf("kept %d samples, or not the last", len(got.Samples))
	}
}

// WHAT CANNOT FIT IS REFUSED, NOT CUT: a bound no report can meet.
func TestDownsampleRefusesWhatCannotFit(t *testing.T) {
	t.Parallel()

	r := mustMerge(t, batchesThrough(6))

	for _, bound := range []int{0, -1, MaxReportBytes + 1} {
		if _, _, err := Downsample(r, bound); !errors.Is(err, ErrOutOfRange) {
			t.Fatalf("bound %d: %v", bound, err)
		}
	}

	if _, _, err := Downsample(r, 64); !errors.Is(err, ErrCannotFit) {
		t.Fatalf("a report fit 64 bytes: %v", err)
	}
}

// THE WHOLE SECTIONS FIT AT THEIR BOUNDS: steps, suites and failed names at every
// bound at once, every byte of them random, with twenty minutes of samples beside
// them. Downsample keeps them whole, so if their bounds did not fit MaxReportBytes
// together, a guest could make its report impossible to keep.
func TestTheWholeSectionsFitAtTheirBounds(t *testing.T) {
	t.Parallel()

	rng := rand.New(rand.NewPCG(3, 4))
	text := func(n int) string {
		var b strings.Builder
		for range n {
			b.WriteByte(byte(' ' + 1 + rng.IntN(94)))
		}

		return b.String()
	}

	r := mustMerge(t, busyBatches(120))
	r.Steps, r.Tests = nil, nil

	for i := range MaxReportSteps {
		r.Steps = append(r.Steps, StepMark{Name: text(MaxNameBytes), AtSeconds: MaxValue - MaxReportSteps + int64(i)})
	}

	names := 0
	for range MaxReportSuites {
		s := SuiteResult{Name: text(MaxNameBytes), Tests: MaxValue, Failures: MaxValue, Errors: MaxValue, Skipped: MaxValue}
		for ; names < MaxReportFailedNames && len(s.Failed) < MaxFailedNames; names++ {
			s.Failed = append(s.Failed, text(MaxFailedNameBytes))
		}

		r.Tests = append(r.Tests, s)
	}

	r.AgentVersion = text(MaxNameBytes)
	r.Dropped = Dropped{MaxValue, MaxValue, MaxValue}

	got, data, err := Downsample(r, MaxReportBytes)
	if err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(got.Steps, r.Steps) || !reflect.DeepEqual(got.Tests, r.Tests) {
		t.Fatal("the whole sections were not kept whole")
	}

	t.Logf("%d bytes at stride %d", len(data), got.Stride)
}

// A SUM PAST MaxValue IS HELD THERE AND COUNTED, never wrapped and never silent: two
// process samples that each report MaxValue of CPU and of residual, merged by the
// halving that a report one sample past the bound forces.
func TestDownsampleCountsASumItCannotHold(t *testing.T) {
	t.Parallel()

	r := Report{AgentVersion: "v1", TicksPerSecond: 100, FirstSeq: 1, LastSeq: 1, Batches: 1, Stride: 1}

	for i := range MaxReportProcessSamples + 1 {
		p := ProcessSample{AtMillis: start + int64(i)*2000}
		if i < 2 {
			p.Rows = []ProcessRow{{Name: "spin", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: MaxValue}}}
			p.ResidualCPUTicks = MaxValue
		}

		r.Processes = append(r.Processes, p)
	}

	got, _, err := Downsample(r, MaxReportBytes)
	if err != nil {
		t.Fatal(err)
	}

	first := got.Processes[0]
	if got.Saturated != 2 || first.Rows[0].CPUTicks != MaxValue || first.ResidualCPUTicks != MaxValue {
		t.Fatalf("saturated %d, CPU %d, residual %d", got.Saturated, first.Rows[0].CPUTicks, first.ResidualCPUTicks)
	}

	// A report that saturates nothing says so.
	if got, _, err := Downsample(mustMerge(t, busyBatches(120)), 24<<10); err != nil || got.Saturated != 0 {
		t.Fatalf("saturated %d (%v)", got.Saturated, err)
	}
}

// FOLD REFUSES WHAT THE DECODER WOULD, with where it is, and a sum it cannot hold.
func TestFoldRefusesRowsTheDecoderWould(t *testing.T) {
	t.Parallel()

	ok := ProcessRow{Name: "go", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: 5}}

	for _, c := range []struct {
		name  string
		row   ProcessRow
		err   error
		where string
	}{
		{"an empty name", ProcessRow{ProcessUsage: ok.ProcessUsage}, ErrEmpty, "rows[1].name"},
		{"a long name", ProcessRow{Name: strings.Repeat("n", MaxNameBytes+1), ProcessUsage: ok.ProcessUsage},
			ErrNameTooLong, "rows[1].name"},
		{"no processes", ProcessRow{Name: "x"}, ErrEmpty, "rows[1].procs"},
		{"a negative figure", ProcessRow{Name: "x", ProcessUsage: ProcessUsage{Procs: 1, ReadBytes: -1}},
			ErrOutOfRange, "rows[1].read_bytes"},
		{"a figure past MaxValue", ProcessRow{Name: "x", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: math.MaxInt64}},
			ErrOutOfRange, "rows[1].cpu_ticks"},
		{"a sum past MaxValue", ProcessRow{Name: "go", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: MaxValue}},
			ErrOutOfRange, "rows"},
	} {
		_, _, err := Fold([]ProcessRow{ok, c.row})
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			check(t, err, c.err, c.where)
		})
	}
}

func TestFoldKeepsTheBusiestNamesAndSumsTheRest(t *testing.T) {
	t.Parallel()

	var rows []ProcessRow

	for i := range 30 {
		u := ProcessUsage{Procs: 1, CPUTicks: int64(i), RSSBytes: 10, ReadBytes: 1, WriteBytes: 2}
		rows = append(rows, ProcessRow{Name: fmt.Sprintf("p%02d", i), ProcessUsage: u})
	}

	rows = append(rows,
		// Two processes of one name are one row, summed, and outrank everything.
		ProcessRow{Name: "p00", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: 100, RSSBytes: 5}},
		// A tie on CPU is broken by name, so the fold is the same whatever the order.
		ProcessRow{Name: "a-tie", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: 11}})
	slices.Reverse(rows)

	kept, other := mustFold(rows)

	if len(kept) != KeepProcessesByName {
		t.Fatalf("kept %d names", len(kept))
	}

	if kept[0].Name != "p00" || kept[0].Procs != 2 || kept[0].CPUTicks != 100 || kept[0].RSSBytes != 15 {
		t.Fatalf("the busiest row is %+v", kept[0])
	}

	var names []string
	for _, r := range kept {
		names = append(names, r.Name)
	}

	// p29..p12 by CPU, then a-tie, which ties p11 at eleven ticks and sorts first.
	want := []string{"p00"}
	for i := 29; i >= 12; i-- {
		want = append(want, fmt.Sprintf("p%02d", i))
	}

	want = append(want, "a-tie")

	if !slices.Equal(names, want) {
		t.Fatalf("kept %q, want %q", names, want)
	}

	// Left out: p11, p10 and p01..p09, one process each; p00's first row was not.
	if other.Procs != 11 || other.CPUTicks != 11+10+45 || other.RSSBytes != 110 || other.ReadBytes != 11 || other.WriteBytes != 22 {
		t.Fatalf("other is %+v", other)
	}

	if few, none := mustFold(rows[:3]); len(few) != 3 || none != (ProcessUsage{}) {
		t.Fatalf("three rows folded into %d and %+v", len(few), none)
	}

	// THE SAME ROWS IN ANY ORDER FOLD THE SAME, the tie at the cut included.
	rng := rand.New(rand.NewPCG(5, 6))
	for range 50 {
		shuffled := slices.Clone(rows)
		rng.Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })

		again, againOther := mustFold(shuffled)
		if !slices.Equal(again, kept) || againOther != other {
			t.Fatalf("a shuffle folded into %v and %+v", again, againOther)
		}
	}
}
