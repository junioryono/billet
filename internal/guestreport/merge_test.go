package guestreport

import (
	"errors"
	"reflect"
	"slices"
	"testing"
)

// THE ORDER RECEIVED DOES NOT MATTER, A RETRIED BATCH COUNTS ONCE, AND WHAT NEVER
// ARRIVED IS NAMED. Batches 1 to 9 but 4 and 7, out of order, with 2 received twice.
func TestMergeOrdersDeduplicatesAndNamesGaps(t *testing.T) {
	t.Parallel()

	in := []Batch{batchAt(5), batchAt(2), batchAt(9), batchAt(1), batchAt(3), batchAt(2), batchAt(8), batchAt(6)}
	r := mustMerge(t, in)

	if r.FirstSeq != 1 || r.LastSeq != 9 || r.Batches != 7 || r.Missing != 2 || r.Refused != 0 {
		t.Fatalf("account: first %d last %d batches %d missing %d refused %d, want 1 9 7 2 0",
			r.FirstSeq, r.LastSeq, r.Batches, r.Missing, r.Refused)
	}

	if want := []Gap{{4, 4}, {7, 7}}; !slices.Equal(r.Gaps, want) {
		t.Fatalf("gaps %v, want %v", r.Gaps, want)
	}

	if r.Duplicates != 1 || r.Conflicts != 0 {
		t.Fatalf("duplicates %d conflicts %d, want 1 0", r.Duplicates, r.Conflicts)
	}

	// THE SAME REPORT AS THE BATCHES IN ORDER, each once.
	ordered := mustMerge(t, []Batch{batchAt(1), batchAt(2), batchAt(3), batchAt(5), batchAt(6), batchAt(8), batchAt(9)})
	ordered.Duplicates = 1
	if !reflect.DeepEqual(r, ordered) {
		t.Fatalf("out of order merged as\n%+v\nin order as\n%+v", r, ordered)
	}

	var steps []string
	for _, m := range r.Steps {
		steps = append(steps, m.Name)
	}

	if want := []string{"Run step 1", "Run step 2", "Run step 3", "Run step 5", "Run step 6", "Run step 8", "Run step 9"}; !slices.Equal(steps, want) {
		t.Fatalf("steps %q, want %q", steps, want)
	}

	if len(r.Samples) != 70 || len(r.Processes) != 35 || len(r.Tests) != 3 {
		t.Fatalf("kept %d samples, %d process samples, %d suites; want 70, 35, 3",
			len(r.Samples), len(r.Processes), len(r.Tests))
	}

	if r.AgentCPUTicks != batchAt(9).AgentCPUTicks || r.AgentVersion != "v0.14.0" || r.TicksPerSecond != 100 || r.Stride != 1 {
		t.Fatalf("header %q %d, agent CPU %d, stride %d", r.AgentVersion, r.TicksPerSecond, r.AgentCPUTicks, r.Stride)
	}

	if _, err := Encode(r); err != nil {
		t.Fatalf("the merge does not encode: %v", err)
	}
}

// A NUMBER RECEIVED AGAIN WITH OTHER CONTENT keeps the first received, whichever
// number it is.
func TestMergeKeepsTheFirstOfAConflict(t *testing.T) {
	t.Parallel()

	other := batchAt(2)
	other.Steps[0].Name = "the second one received"

	for _, in := range [][]Batch{
		{batchAt(1), batchAt(2), other},
		{other, batchAt(1), batchAt(2)},
	} {
		r := mustMerge(t, in)
		if r.Conflicts != 1 || r.Duplicates != 0 || r.Batches != 2 {
			t.Fatalf("conflicts %d duplicates %d batches %d, want 1 0 2", r.Conflicts, r.Duplicates, r.Batches)
		}

		want := in[0].Steps[0].Name
		if in[0].Seq != 2 {
			want = in[1].Steps[0].Name
		}

		if r.Steps[1].Name != want {
			t.Fatalf("kept %q for batch 2, want the first received, %q", r.Steps[1].Name, want)
		}
	}
}

// THE GAPS LIST IS BOUNDED AND THE COUNT IS NOT: every other number from 1 to 301 is
// missing, 150 of them, and the first MaxReportGaps runs are named.
func TestMergeCountsEveryMissingBatchPastTheGapsBound(t *testing.T) {
	t.Parallel()

	var in []Batch
	for seq := uint64(1); seq <= 301; seq += 2 {
		in = append(in, batchAt(seq))
	}

	r := mustMerge(t, in)
	if r.Missing != 150 || len(r.Gaps) != MaxReportGaps {
		t.Fatalf("missing %d with %d gaps named, want 150 and %d", r.Missing, len(r.Gaps), MaxReportGaps)
	}

	if last := r.Gaps[MaxReportGaps-1]; last != (Gap{2 * MaxReportGaps, 2 * MaxReportGaps}) {
		t.Fatalf("the last gap named is %v", last)
	}

	if _, _, err := Downsample(r, MaxReportBytes); err != nil {
		t.Fatalf("the merge does not encode: %v", err)
	}
}

// A BATCH THAT DOES NOT CONTINUE THE ONES BEFORE IT IS REFUSED AND COUNTED, and the
// ones after it still merge.
func TestMergeRefusesABatchThatGoesBack(t *testing.T) {
	t.Parallel()

	for _, c := range []struct {
		name string
		edit func(*Batch)
	}{
		{"another agent", func(b *Batch) { b.AgentVersion = "v0.13.0" }},
		{"another tick rate", func(b *Batch) { b.TicksPerSecond = 250 }},
		{"agent CPU that falls", func(b *Batch) { b.AgentCPUTicks = 0 }},
		{"a sample at the time of the last", func(b *Batch) { b.Samples[0].AtMillis = batchAt(2).Samples[9].AtMillis }},
		{"a counter that falls", func(b *Batch) { b.Samples[0].DiskWriteBytes = 0 }},
		{"a process sample that goes back", func(b *Batch) { b.Processes[0].AtMillis = batchAt(2).Processes[4].AtMillis }},
		{"a step that goes back", func(b *Batch) { b.Steps[0].AtSeconds = batchAt(2).Steps[0].AtSeconds - 1 }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			bad := batchAt(3)
			c.edit(&bad)

			// Batch 4 continues batch 2 in every check, so it merges.
			r := mustMerge(t, []Batch{batchAt(1), batchAt(2), bad, batchAt(4)})
			if r.Refused != 1 || r.Batches != 3 || r.Missing != 0 {
				t.Fatalf("refused %d batches %d missing %d, want 1 3 0", r.Refused, r.Batches, r.Missing)
			}

			for _, m := range r.Steps {
				if m.Name == "Run step 3" {
					t.Fatal("the refused batch's step was kept")
				}
			}

			if _, err := Encode(r); err != nil {
				t.Fatalf("the merge does not encode: %v", err)
			}
		})
	}
}

// A STEP AT THE SECOND THE LAST ONE STARTED continues it: the log has whole seconds.
func TestMergeKeepsAStepInTheSameSecond(t *testing.T) {
	t.Parallel()

	b := batchAt(2)
	b.Steps[0].AtSeconds = batchAt(1).Steps[0].AtSeconds

	if r := mustMerge(t, []Batch{batchAt(1), b}); r.Refused != 0 || len(r.Steps) != 2 {
		t.Fatalf("refused %d, kept %d steps", r.Refused, len(r.Steps))
	}
}

// WHAT IS KEPT WHOLE IS BOUNDED, AND WHAT THE BOUND LEAVES OUT IS COUNTED.
func TestMergeDropsAndCountsPastTheWholeSectionsBounds(t *testing.T) {
	t.Parallel()

	var in []Batch

	for seq := uint64(1); seq <= 12; seq++ {
		b := Batch{Seq: seq, AgentVersion: "v1", TicksPerSecond: 100}
		for i := range MaxBatchSteps {
			b.Steps = append(b.Steps, StepMark{Name: "s", AtSeconds: int64(seq)*1000 + int64(i)})
		}

		for range MaxBatchSuites {
			b.Tests = append(b.Tests, SuiteResult{Name: "suite", Tests: 60, Failures: 50,
				Failed: slices.Repeat([]string{"TestFails"}, 2)})
		}

		in = append(in, b)
	}

	r := mustMerge(t, in)

	if len(r.Steps) != MaxReportSteps || r.Dropped.Steps != 12*MaxBatchSteps-MaxReportSteps {
		t.Fatalf("kept %d steps and dropped %d", len(r.Steps), r.Dropped.Steps)
	}

	if len(r.Tests) != MaxReportSuites || r.Dropped.Suites != 12*MaxBatchSuites-MaxReportSuites {
		t.Fatalf("kept %d suites and dropped %d", len(r.Tests), r.Dropped.Suites)
	}

	names := 0
	for _, s := range r.Tests {
		names += len(s.Failed)
	}

	// 128 suites of two names would be 256, MaxReportFailedNames exactly.
	if names != MaxReportFailedNames || r.Dropped.FailedNames != 0 {
		t.Fatalf("kept %d failed names and dropped %d", names, r.Dropped.FailedNames)
	}

	// THREE NAMES A SUITE: the 86th suite has room for one, and every one after
	// for none.
	for i := range in {
		for j := range in[i].Tests {
			in[i].Tests[j].Failed = slices.Repeat([]string{"TestFails"}, 3)
		}
	}

	r = mustMerge(t, in)
	names = 0

	for _, s := range r.Tests {
		names += len(s.Failed)
	}

	if names != MaxReportFailedNames || r.Dropped.FailedNames != 3*MaxReportSuites-MaxReportFailedNames {
		t.Fatalf("kept %d failed names and dropped %d", names, r.Dropped.FailedNames)
	}

	if got := len(r.Tests[85].Failed); got != 1 || r.Tests[86].Failed != nil {
		t.Fatalf("suite 86 kept %d names, suite 87 %v", got, r.Tests[86].Failed)
	}

	if _, err := Encode(r); err != nil {
		t.Fatalf("the merge does not encode: %v", err)
	}
}

// MERGE TAKES ONLY WHAT DECODES, AND NOTHING IS NOT A REPORT.
func TestMergeRefusesWhatWouldNotDecode(t *testing.T) {
	t.Parallel()

	if _, err := Merge(nil); !errors.Is(err, ErrNoBatches) {
		t.Fatalf("no batches merged: %v", err)
	}

	bad := batchAt(2)
	bad.Seq = 0

	if _, err := Merge([]Batch{batchAt(1), bad}); !errors.Is(err, ErrEmpty) {
		t.Fatalf("a batch numbered zero merged: %v", err)
	}
}

// MERGE DOES NOT SHARE THE CALLER'S SLICES: a node that reuses a batch's buffers
// after merging changes nothing in the report.
func TestMergeCopiesWhatItKeeps(t *testing.T) {
	t.Parallel()

	in := []Batch{batchAt(3)}
	r := mustMerge(t, in)
	want := mustMerge(t, []Batch{batchAt(3)})

	in[0].Processes[0].Rows[0].Name = "changed"
	in[0].Tests[0].Failed[0] = "changed"

	if !reflect.DeepEqual(r, want) {
		t.Fatal("the report changed with the batch it was merged from")
	}
}
