package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// allGroups is a usage report that measured everything.
func allGroups() map[string]bool {
	return map[string]bool{"cpu": true, "threads": true, "memory": true, "oom": true, "io": true,
		"net": true, "pressure": true, "energy": true}
}

// fixtureRun is one run's expectations and billet's records for them: a
// baseline that spent 20 CPU-seconds preparing, an idle job that slept 60 s
// on top of it, and each loaded job measured exactly at its known answer over
// the idle job's figures.
func fixtureRun(runID int64) ([]expectation, map[string]record) {
	lease := func(kind string) string { return fmt.Sprintf("lease-%d-%s", runID, kind) }
	exp := func(kind string, seconds int64, expected map[string]int64) expectation {
		return expectation{Schema: 1, Kind: kind, Lease: lease(kind), Repository: "acme/bench", RunID: runID,
			RunAttempt: 1, GitHubJobID: "7" + lease(kind)[6:], Seconds: seconds, Expected: expected}
	}
	idle := usage{Measured: allGroups(), CPUUserMicros: 20_500_000, CPUSystemMicros: 1_000_000,
		MemoryPeakBytes: 700 * mib, NetRxBytes: 60 * mib, DiskWriteBytes: 90 * mib}
	rec := func(kind string, u usage) record {
		return record{Lease: lease(kind), RunID: runID, GitHubJobID: "7" + lease(kind)[6:], Usage: &u}
	}
	with := func(f func(*usage)) usage {
		u := idle
		u.Measured = allGroups()
		f(&u)
		return u
	}
	exps := []expectation{
		exp(kindBaseline, 0, map[string]int64{}),
		exp(kindIdle, 60, map[string]int64{"cpu_seconds": 0}),
		exp(kindCPU, 60, map[string]int64{"cpu_seconds": 240}),
		exp(kindMemory, 60, map[string]int64{"memory_peak_bytes": 2048 * mib}),
		exp(kindNetwork, 0, map[string]int64{"net_rx_bytes": 100 * mib}),
		exp(kindDisk, 0, map[string]int64{"disk_write_bytes": 2048 * mib}),
	}
	recs := map[string]record{
		lease(kindBaseline): rec(kindBaseline, with(func(u *usage) { u.CPUUserMicros = 20_000_000 })),
		lease(kindIdle):     rec(kindIdle, idle),
		lease(kindCPU):      rec(kindCPU, with(func(u *usage) { u.CPUUserMicros += 240_000_000 })),
		lease(kindMemory):   rec(kindMemory, with(func(u *usage) { u.MemoryPeakBytes = 2048*mib + 500*mib })),
		lease(kindNetwork):  rec(kindNetwork, with(func(u *usage) { u.NetRxBytes += 100 * mib * 103 / 100 })),
		lease(kindDisk):     rec(kindDisk, with(func(u *usage) { u.DiskWriteBytes += 2048*mib + 10*mib })),
	}

	return exps, recs
}

func fromMap(recs map[string]record) recordSource {
	return func(lease string) (record, error) {
		r, ok := recs[lease]
		if !ok {
			return record{}, fmt.Errorf("%w for lease %s", errNoRecord, lease)
		}
		return r, nil
	}
}

func resultFor(t *testing.T, results []result, kind string) result {
	t.Helper()
	for i := range results {
		if results[i].kind == kind {
			return results[i]
		}
	}
	t.Fatalf("no %s result in %+v", kind, results)

	return result{}
}

func TestJobsMeasuredAtTheirKnownAnswerPass(t *testing.T) {
	exps, recs := fixtureRun(100)
	results := evaluate(exps, fromMap(recs))
	if len(results) != 5 {
		t.Fatalf("%d results, want one per loaded kind (the baseline is only subtracted)", len(results))
	}
	for i := range results {
		r := &results[i]
		if r.verdict != pass {
			t.Errorf("%s: %s (%s) measured %.0f in [%.0f, %.0f]", r.kind, r.verdict, r.reason, r.value, r.low, r.high)
		}
	}
	// THE SUBTRACTION IS THE SAME RUN'S REFERENCE: the cpu job's value is its
	// CPU less the idle job's, and the idle job's less the baseline's.
	if got := resultFor(t, results, kindCPU); got.value != 240 || got.against != "lease-100-idle" {
		t.Errorf("cpu value %v against %s, want 240 against lease-100-idle", got.value, got.against)
	}
	if got := resultFor(t, results, kindIdle); got.value != 0.5 || got.against != "lease-100-baseline" {
		t.Errorf("idle value %v against %s, want 0.5 against lease-100-baseline", got.value, got.against)
	}
}

// EACH TOLERANCE IS HELD AT BOTH EDGES: a value just inside passes and one
// just outside fails, on both sides, so a bound that moved is caught.
func TestEachToleranceHoldsAtBothEdges(t *testing.T) {
	for _, tc := range []struct {
		kind     string
		low      float64
		high     float64
		expected float64
		seconds  float64
		refPeak  float64
	}{
		{kind: kindIdle, low: -3, high: 0.02*60 + 3, seconds: 60},
		{kind: kindCPU, expected: 240, low: 0.95*240 - 2, high: 1.05*240 + 2},
		{kind: kindMemory, expected: 2048 * mib, low: 2048 * mib, high: 700*mib + 1.05*2048*mib + 64*mib, refPeak: 700 * mib},
		{kind: kindNetwork, expected: 100 * mib, low: 100*mib - 4*mib, high: 1.06*100*mib + 4*mib},
		{kind: kindDisk, expected: 2048 * mib, low: 2048*mib - 64*mib, high: 1.10*2048*mib + 64*mib},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			m, ok := metricFor(tc.kind)
			if !ok {
				t.Fatalf("no metric for %s", tc.kind)
			}
			low, high := m.bounds(tc.expected, tc.seconds, &usage{MemoryPeakBytes: int64(tc.refPeak)})
			if low != tc.low || high != tc.high {
				t.Fatalf("bounds [%v, %v], want [%v, %v] (the doc states these)", low, high, tc.low, tc.high)
			}
		})
	}

	// And through evaluate, so the comparison uses the bounds the way the
	// report claims: inclusive at both ends.
	for _, tc := range []struct {
		name   string
		cpu    int64 // the cpu job's CPU above the idle job's, microseconds
		expect verdict
	}{
		{"just inside the low edge", int64((0.95*240-2)*1e6) + 1_000, pass},
		{"below the low edge", int64((0.95*240-2)*1e6) - 1_000, fail},
		{"just inside the high edge", int64((1.05*240+2)*1e6) - 1_000, pass},
		{"above the high edge", int64((1.05*240+2)*1e6) + 1_000, fail},
	} {
		t.Run("cpu "+tc.name, func(t *testing.T) {
			exps, recs := fixtureRun(100)
			r := recs["lease-100-cpu"]
			u := *r.Usage
			u.CPUUserMicros = recs["lease-100-idle"].Usage.CPUUserMicros + tc.cpu
			r.Usage = &u
			recs["lease-100-cpu"] = r
			if got := resultFor(t, evaluate(exps, fromMap(recs)), kindCPU); got.verdict != tc.expect {
				t.Errorf("verdict %s for %.3f s, want %s", got.verdict, got.value, tc.expect)
			}
		})
	}
}

// A METRIC BILLET DID NOT MEASURE IS UNMEASURED, NEVER PASS, and so is one
// whose reference could not be read: every way the comparison can be missing
// its input.
func TestAComparisonWithoutItsInputIsUnmeasured(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  string
		alter func(exps []expectation, recs map[string]record) []expectation
		says  string
	}{
		{"the group was not measured", kindDisk, func(e []expectation, recs map[string]record) []expectation {
			recs["lease-100-disk"].Usage.Measured["io"] = false
			return e
		}, "billet did not measure io"},
		{"the group is absent from the report", kindNetwork, func(e []expectation, recs map[string]record) []expectation {
			delete(recs["lease-100-network"].Usage.Measured, "net")
			return e
		}, "billet did not measure net"},
		{"the reference's group was not measured", kindCPU, func(e []expectation, recs map[string]record) []expectation {
			recs["lease-100-idle"].Usage.Measured["cpu"] = false
			return e
		}, "the idle job lease-100-idle: billet did not measure cpu"},
		{"no usage report", kindMemory, func(e []expectation, recs map[string]record) []expectation {
			r := recs["lease-100-memory"]
			r.Usage = nil
			recs["lease-100-memory"] = r
			return e
		}, "no usage report"},
		{"no record collected", kindCPU, func(e []expectation, recs map[string]record) []expectation {
			delete(recs, "lease-100-cpu")
			return e
		}, "no record was collected"},
		{"no run id recorded", kindCPU, func(e []expectation, recs map[string]record) []expectation {
			r := recs["lease-100-cpu"]
			r.RunID = 0
			recs["lease-100-cpu"] = r
			return e
		}, "recorded no run id"},
		{"no idle job to subtract", kindCPU, func(e []expectation, _ map[string]record) []expectation {
			return dropKind(e, kindIdle)
		}, "no idle job in run 100 attempt 1 to subtract"},
		{"no baseline to subtract", kindIdle, func(e []expectation, _ map[string]record) []expectation {
			return dropKind(e, kindBaseline)
		}, "no baseline job"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			exps, recs := fixtureRun(100)
			exps = tc.alter(exps, recs)
			results := evaluate(exps, fromMap(recs))
			got := resultFor(t, results, tc.kind)
			if got.verdict != unmeasured || !strings.Contains(got.reason, tc.says) {
				t.Errorf("verdict %s (%q), want UNMEASURED saying %q", got.verdict, got.reason, tc.says)
			}
			if overall(results) != unmeasured {
				t.Errorf("overall %s, want UNMEASURED", overall(results))
			}
		})
	}
}

func dropKind(exps []expectation, kind string) []expectation {
	var out []expectation
	for i := range exps {
		if exps[i].Kind != kind {
			out = append(out, exps[i])
		}
	}
	return out
}

// A RECORD FROM ANOTHER RUN IS A FINDING: the lease the job named ran
// something else, so neither it nor a job subtracting it can pass.
func TestARecordOfAnotherRunFails(t *testing.T) {
	exps, recs := fixtureRun(100)
	r := recs["lease-100-idle"]
	r.RunID = 99
	recs["lease-100-idle"] = r
	results := evaluate(exps, fromMap(recs))
	if got := resultFor(t, results, kindIdle); got.verdict != fail || !strings.Contains(got.reason, "ran run 99") {
		t.Errorf("idle: %s (%q), want FAIL naming run 99", got.verdict, got.reason)
	}
	if got := resultFor(t, results, kindCPU); got.verdict != fail || !strings.Contains(got.reason, "the idle job") {
		t.Errorf("cpu against a foreign idle: %s (%q), want FAIL", got.verdict, got.reason)
	}
	if overall(results) != fail {
		t.Errorf("overall %s, want FAIL", overall(results))
	}
}

// THE MEMORY PEAK IS COMPARED RAW, not net of the idle job's: a peak is a
// maximum, and the idle job's resident pages are not added to the stress
// allocation, only possibly reused by it. The idle job's peak bounds it above.
func TestTheMemoryPeakIsComparedRaw(t *testing.T) {
	exps, recs := fixtureRun(100)
	r := recs["lease-100-memory"]
	u := *r.Usage
	u.MemoryPeakBytes = 2048*mib - 1
	r.Usage = &u
	recs["lease-100-memory"] = r
	got := resultFor(t, evaluate(exps, fromMap(recs)), kindMemory)
	if got.verdict != fail || got.value != float64(2048*mib-1) {
		t.Errorf("a peak one byte under X: %s at %.0f, want FAIL at the raw peak", got.verdict, got.value)
	}
}

func TestTheJobIDIsANoteAndNeverAVerdict(t *testing.T) {
	exps, recs := fixtureRun(100)
	r := recs["lease-100-cpu"]
	r.GitHubJobID = "a-guid-like-id"
	recs["lease-100-cpu"] = r
	got := resultFor(t, evaluate(exps, fromMap(recs)), kindCPU)
	if got.verdict != pass || !strings.Contains(got.jobID, `billet recorded "a-guid-like-id"`) {
		t.Errorf("verdict %s, note %q", got.verdict, got.jobID)
	}
	if note := jobIDNote("123", "123"); note != "github job id 123 agrees" {
		t.Errorf("agreeing ids: %q", note)
	}
	if note := jobIDNote("", "123"); !strings.Contains(note, "not known") {
		t.Errorf("an unknown id: %q", note)
	}
}

func TestDiscardDropsTheEarliestRuns(t *testing.T) {
	var exps []expectation
	for _, id := range []int64{300, 100, 200} {
		e, _ := fixtureRun(id)
		exps = append(exps, e...)
	}
	kept, dropped := discardRuns(exps, 1)
	if len(dropped) != 1 || dropped[0] != (runKey{100, 1}) {
		t.Fatalf("dropped %v, want run 100", dropped)
	}
	for i := range kept {
		if kept[i].RunID == 100 {
			t.Fatalf("run 100 was kept")
		}
	}
	if len(kept) != 12 {
		t.Errorf("kept %d expectations, want 12", len(kept))
	}
	if _, dropped := discardRuns(exps, 9); len(dropped) != 3 {
		t.Errorf("discarding more runs than exist dropped %d", len(dropped))
	}
}

// writeRun lays a run out the way `gh run download` and collect do: one
// directory per artifact holding expectation.json, and <lease>.json records.
func writeRun(t *testing.T, expDir, recDir string, exps []expectation, recs map[string]record) {
	t.Helper()
	for i := range exps {
		e := &exps[i]
		dir := filepath.Join(expDir, fmt.Sprintf("known-answer-%d-%d-%s", e.RunID, e.RunAttempt, e.Kind))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "expectation.json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for lease := range recs {
		body, err := json.Marshal(recordJSON(recs[lease]))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(recDir, lease+".json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// recordJSON spells a record the way billet jobs show --json does, which the
// decoding test proves against the command's own fixtures.
func recordJSON(r record) map[string]any {
	out := map[string]any{"lease": r.Lease, "run_id": r.RunID, "github_job_id": r.GitHubJobID, "usage": nil}
	if r.Usage != nil {
		u := r.Usage
		out["usage"] = map[string]any{"measured": u.Measured, "samples": u.Samples,
			"cpu_user_us": u.CPUUserMicros, "cpu_system_us": u.CPUSystemMicros,
			"memory_peak_bytes": u.MemoryPeakBytes, "disk_write_bytes": u.DiskWriteBytes,
			"net_rx_bytes": u.NetRxBytes, "energy_active_uj": u.EnergyActiveUJ,
			"energy_idle_uj": u.EnergyIdleUJ, "energy_source": u.EnergySource, "window_ms": u.WindowMillis}
	}
	return out
}

// THE EXIT STATUS IS THE VERDICT, three ways, through the command a runbook
// runs.
func TestCheckExitsWithItsVerdict(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(map[string]record)
		code  int
		says  string
	}{
		{"every comparison passes", func(map[string]record) {}, exitPass, "overall PASS"},
		{"one fails", func(recs map[string]record) {
			recs["lease-200-network"].Usage.NetRxBytes = 0
		}, exitFail, "overall FAIL"},
		{"one is unmeasured", func(recs map[string]record) {
			recs["lease-200-disk"].Usage.Measured["io"] = false
		}, exitUnmeasured, "overall UNMEASURED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expDir, recDir := t.TempDir(), t.TempDir()
			for _, id := range []int64{100, 200} {
				exps, recs := fixtureRun(id)
				if id == 200 {
					tc.alter(recs)
				}
				writeRun(t, expDir, recDir, exps, recs)
			}
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), []string{"check", "--expectations", expDir, "--records", recDir},
				&stdout, &stderr)
			if code != tc.code || !strings.Contains(stdout.String(), tc.says) {
				t.Errorf("exit %d, want %d saying %q:\n%s%s", code, tc.code, tc.says, stdout.String(), stderr.String())
			}
			for _, want := range []string{"run 100 attempt 1", "run 200 attempt 1", "summary",
				"cpu      2 run(s)", "cpu minus idle CPU within [0.95 N x T - 2 s, 1.05 N x T + 2 s]"} {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("the report does not say %q:\n%s", want, stdout.String())
				}
			}
		})
	}

	var stdout, stderr bytes.Buffer
	if code := run(t.Context(), []string{"check", "--records", t.TempDir()}, &stdout, &stderr); code != exitUsage {
		t.Errorf("a check with no expectations exited %d, want %d", code, exitUsage)
	}
	if code := run(t.Context(), []string{"check", "--expectations", t.TempDir(), "--records", t.TempDir()},
		&stdout, &stderr); code != exitUsage || !strings.Contains(stderr.String(), "no expectation.json") {
		t.Errorf("an empty expectations directory exited %d: %s", code, stderr.String())
	}
}

func TestCheckDiscardsTheWarmupItIsTold(t *testing.T) {
	expDir, recDir := t.TempDir(), t.TempDir()
	for _, id := range []int64{100, 200} {
		exps, recs := fixtureRun(id)
		if id == 100 {
			recs["lease-100-cpu"].Usage.CPUUserMicros = 0 // the warmup failed
		}
		writeRun(t, expDir, recDir, exps, recs)
	}
	var stdout, stderr bytes.Buffer
	code := run(t.Context(), []string{"check", "--expectations", expDir, "--records", recDir, "--discard", "1"},
		&stdout, &stderr)
	if code != exitPass || !strings.Contains(stdout.String(), "discarded run 100 attempt 1 (warmup)") ||
		strings.Contains(stdout.String(), "\nrun 100 attempt 1\n") {
		t.Errorf("exit %d:\n%s%s", code, stdout.String(), stderr.String())
	}
}
