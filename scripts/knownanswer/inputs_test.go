package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// THE RECORDS ARE READ AS billet jobs show --json WRITES THEM. The files under
// testdata/jobs-show are the command's own answers, written and compared by
// internal/ops/fleetops' TestTheKnownAnswerFixturesAreJobsShowsOwn, so a field
// renamed there fails here rather than reading as zero on the host.
func TestTheRecordsAreReadAsJobsShowWritesThem(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"measured", "partial", "no-report"} {
		body, err := os.ReadFile(filepath.Join("testdata", "jobs-show", name+".json"))
		if err != nil {
			t.Fatal(err)
		}
		// The fixtures share one lease; each is read under its own name here.
		body = []byte(strings.Replace(string(body), `"lease": "lease-cpu"`, `"lease": "`+name+`"`, 1))
		if err := os.WriteFile(filepath.Join(dir, name+".json"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	measured, err := loadRecord(dir, "measured")
	if err != nil {
		t.Fatal(err)
	}
	u := measured.Usage
	if measured.RunID != 18000000001 || measured.GitHubJobID != "52000000001" || measured.VCPU != 8 || u == nil {
		t.Fatalf("identity = %+v", measured)
	}
	if u.CPUUserMicros != 251_000_000 || u.CPUSystemMicros != 9_000_000 || u.MemoryPeakBytes != 1_200_000_000 ||
		u.DiskWriteBytes != 60_000_000 || u.NetRxBytes != 52_000_000 || u.EnergyActiveUJ != 9_500_000_000 ||
		u.EnergyIdleUJ != 600_000_000 || u.EnergySource != "rapl" || u.WindowMillis != 95_000 {
		t.Errorf("usage = %+v", *u)
	}
	for _, group := range []string{"cpu", "memory", "io", "net", "energy"} {
		if !u.Measured[group] {
			t.Errorf("%s reads as unmeasured in a fully measured record", group)
		}
	}

	partial, err := loadRecord(dir, "partial")
	if err != nil {
		t.Fatal(err)
	}
	for group, want := range map[string]bool{"cpu": true, "net": true, "memory": false, "io": false, "energy": false} {
		if partial.Usage.Measured[group] != want {
			t.Errorf("partial: %s measured = %v, want %v", group, partial.Usage.Measured[group], want)
		}
	}

	none, err := loadRecord(dir, "no-report")
	if err != nil {
		t.Fatal(err)
	}
	if none.Usage != nil {
		t.Errorf("a job with no report read as %+v", *none.Usage)
	}

	if _, err := loadRecord(dir, "absent"); !errors.Is(err, errNoRecord) {
		t.Errorf("a lease with no file = %v, want errNoRecord", err)
	}
	if _, err := loadRecord(dir, "../measured"); err == nil || !strings.Contains(err.Error(), "not a lease id") {
		t.Errorf("a lease naming another directory = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "other.json"), []byte(`{"lease":"measured"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadRecord(dir, "other"); err == nil || !strings.Contains(err.Error(), `holds lease "measured"`) {
		t.Errorf("a file holding another lease = %v", err)
	}
}

// THE EXPECTATIONS ARE READ AS THE JOB SCRIPT WRITES THEM. The files under
// testdata/expectations are scripts/known-answer-job.sh's own output, written
// and compared by scripts' TestTheKnownAnswerJobWritesWhatItRan.
func TestTheExpectationsAreReadAsTheJobScriptWritesThem(t *testing.T) {
	exps, err := loadExpectations(filepath.Join("testdata", "expectations"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]int64{
		kindBaseline: {}, kindIdle: {"cpu_seconds": 0}, kindCPU: {"cpu_seconds": 240},
		kindMemory: {"memory_peak_bytes": 2147483648}, kindNetwork: {"net_rx_bytes": 104857600},
		kindDisk: {"disk_write_bytes": 2147483648},
	}
	if len(exps) != len(want) {
		t.Fatalf("%d expectations, want %d", len(exps), len(want))
	}
	for _, e := range exps {
		if e.Lease != "lease-"+e.Kind || e.RunID != 18000000001 || e.RunAttempt != 2 ||
			e.GitHubJobID != "52000000077" || e.Repository != "acme/bench" {
			t.Errorf("%s: identity %+v", e.Kind, e)
		}
		if len(e.Expected) != len(want[e.Kind]) {
			t.Errorf("%s expects %v, want %v", e.Kind, e.Expected, want[e.Kind])
		}
		for k, v := range want[e.Kind] {
			if e.Expected[k] != v {
				t.Errorf("%s expects %s %d, want %d", e.Kind, k, e.Expected[k], v)
			}
		}
	}
}

func TestAnExpectationTheCheckerCouldMisreadIsRefused(t *testing.T) {
	good := `{"schema":1,"kind":"cpu","lease":"l1","repository":"a/b","run_id":5,"run_attempt":1,` +
		`"github_job_id":"","seconds":60,"expected":{"cpu_seconds":240}}`
	for _, tc := range []struct {
		name, from, to, says string
	}{
		{"another schema", `"schema":1`, `"schema":2`, "schema 2"},
		{"an unknown kind", `"kind":"cpu"`, `"kind":"gpu"`, `kind "gpu"`},
		{"a lease naming a path", `"lease":"l1"`, `"lease":"../l1"`, "not a lease id"},
		{"no run", `"run_id":5`, `"run_id":0`, "not a workflow run"},
		{"another kind's figure", `{"cpu_seconds":240}`, `{"net_rx_bytes":240}`, "not its figure"},
		{"no figure", `{"cpu_seconds":240}`, `{}`, "must expect cpu_seconds"},
		{"a negative figure", `{"cpu_seconds":240}`, `{"cpu_seconds":-1}`, "is -1; a load expects more than nothing"},
		{"a load expecting nothing", `{"cpu_seconds":240}`, `{"cpu_seconds":0}`, "is 0; a load expects more"},
		{"a null figure", `{"cpu_seconds":240}`, `{"cpu_seconds":null}`, "is 0; a load expects more"},
		{"an idle job expecting work", `"kind":"cpu"`, `"kind":"idle"`, "an idle job expects cpu_seconds 0"},
		{"a timed load that ran no time", `"seconds":60`, `"seconds":0`, "ran 0 seconds, which no job does"},
		{"a timed load past a job's life", `"seconds":60`, `"seconds":21601`, "ran 21601 seconds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(good, tc.from) {
				t.Fatalf("the fixture does not contain %q", tc.from)
			}
			dir := filepath.Join(t.TempDir(), "a")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			body := strings.Replace(good, tc.from, tc.to, 1)
			if err := os.WriteFile(filepath.Join(dir, "expectation.json"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := loadExpectations(filepath.Dir(dir))
			if err == nil {
				t.Fatalf("the expectation was accepted")
			}
			// THE PATH IS CUT OFF FIRST: it holds the subtest's name, which
			// would otherwise satisfy the assertion by itself.
			msg := strings.TrimPrefix(err.Error(), filepath.Join(dir, "expectation.json")+": ")
			if msg == err.Error() || !strings.Contains(msg, tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
		})
	}

	// AN UNTIMED LOAD DOES NOT CLAIM A DURATION.
	untimed := `{"schema":1,"kind":"network","lease":"l1","repository":"a/b","run_id":5,"run_attempt":1,` +
		`"github_job_id":"","seconds":60,"expected":{"net_rx_bytes":100}}`
	dir := filepath.Join(t.TempDir(), "a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "expectation.json"), []byte(untimed), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadExpectations(filepath.Dir(dir)); err == nil ||
		!strings.Contains(err.Error(), "is not timed and says it ran 60 seconds") {
		t.Errorf("err = %v", err)
	}

	// TWO OF A KIND IN ONE RUN, OR ONE LEASE TWICE, would make a subtraction pick
	// one of them silently.
	for _, tc := range []struct {
		name, second, says string
	}{
		{"two cpu jobs in one run", strings.Replace(good, `"lease":"l1"`, `"lease":"l2"`, 1), "two cpu jobs"},
		{"one lease twice", strings.Replace(good, `"run_id":5`, `"run_id":6`, 1), "lease l1 is claimed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for i, body := range []string{good, tc.second} {
				dir := filepath.Join(root, string(rune('a'+i)))
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "expectation.json"), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := loadExpectations(root); err == nil || !strings.Contains(err.Error(), tc.says) {
				t.Errorf("err = %v, want one saying %q", err, tc.says)
			}
		})
	}
}
