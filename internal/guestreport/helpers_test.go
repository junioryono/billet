package guestreport

import (
	"bytes"
	"compress/flate"
	"encoding/json"
	"fmt"
	"testing"
)

// start is a plausible guest clock: 2026-10-10T02:04:20Z, in milliseconds.
const start int64 = 1_760_061_860_000

// batchAt is the batch an agent would send as its seq-th, ten seconds after the one
// before: ten samples, five process samples, a step and, every third batch, a suite.
// Its counters continue the previous batch's, so consecutive ones merge.
func batchAt(seq uint64) Batch {
	base := start + int64(seq-1)*10_000
	n := int64(seq-1) * 10

	b := Batch{Seq: seq, AgentVersion: "v0.14.0", TicksPerSecond: 100, AgentCPUTicks: int64(seq) * 3}

	for i := range int64(10) {
		k := n + i + 1
		b.Samples = append(b.Samples, Sample{
			AtMillis:      base + i*1000 + 1000,
			CPUBusyTicks:  k * 37,
			CPUTotalTicks: k * 800,
			MemUsedBytes:  2<<30 + k*4096, MemAvailableBytes: 30 << 30, MemCachedBytes: 1<<30 + k,
			DiskReadBytes: k * 65536, DiskWriteBytes: k * 4096,
			NetRxBytes: k * 1500, NetTxBytes: k * 900,
		})
	}

	for i := range int64(5) {
		k := n/2 + i + 1
		rows, other := mustFold([]ProcessRow{
			{Name: "go", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: 120 + k%7, RSSBytes: 900 << 20, ReadBytes: k * 10, WriteBytes: k}},
			{Name: "node", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: 30, RSSBytes: 200 << 20}},
			{Name: "node", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: 12, RSSBytes: 150 << 20, WriteBytes: 512}},
			{Name: "Runner.Worker", ProcessUsage: ProcessUsage{Procs: 1, CPUTicks: 2, RSSBytes: 90 << 20}},
		})
		b.Processes = append(b.Processes, ProcessSample{
			AtMillis: base + i*2000 + 2000, Rows: rows, Other: other, ResidualCPUTicks: k % 3,
		})
	}

	b.Steps = []StepMark{{Name: fmt.Sprintf("Run step %d", seq), AtSeconds: base/1000 + 1}}
	if seq%3 == 0 {
		b.Tests = []SuiteResult{{Name: "internal/guestreport", Tests: 40, Failures: 1, Errors: 0, Skipped: 2,
			Failed: []string{"TestRoundTrip/it's \"quoted\""}}}
	}

	return b
}

// mustFold is Fold for rows a test built to be valid.
func mustFold(rows []ProcessRow) ([]ProcessRow, ProcessUsage) {
	kept, other, err := Fold(rows)
	if err != nil {
		panic(fmt.Sprintf("the test's rows do not fold: %v", err))
	}

	return kept, other
}

func batchesThrough(last uint64) []Batch {
	var out []Batch
	for seq := uint64(1); seq <= last; seq++ {
		out = append(out, batchAt(seq))
	}

	return out
}

// deflated is raw compressed as the codec compresses it, whatever raw holds, so a
// test can hand the decoder JSON the encoder would never write.
func deflated(tb testing.TB, raw []byte) []byte {
	tb.Helper()

	var out bytes.Buffer

	w, err := flate.NewWriter(&out, flate.BestCompression)
	if err != nil {
		tb.Fatal(err)
	}

	if _, err := w.Write(raw); err != nil {
		tb.Fatal(err)
	}

	if err := w.Close(); err != nil {
		tb.Fatal(err)
	}

	return out.Bytes()
}

// replaced is raw with the first old replaced by with; an edit that matches nothing
// would leave the case testing the unedited value, so it fails instead.
func replaced(t *testing.T, raw, old, with []byte) []byte {
	t.Helper()

	if !bytes.Contains(raw, old) {
		t.Fatalf("%s is not in %s", old, raw)
	}

	return bytes.Replace(raw, old, with, 1)
}

func marshal(tb testing.TB, v any) []byte {
	tb.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		tb.Fatal(err)
	}

	return raw
}

func mustMerge(t *testing.T, batches []Batch) Report {
	t.Helper()

	r, err := Merge(batches)
	if err != nil {
		t.Fatal(err)
	}

	return r
}
