package scripts_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// THE RUNNER-USAGE SAMPLER IS EXECUTED AGAINST A FAKE /proc, NOT READ. What it
// reports sizes a CI job's runner, so each number is known before it runs: the
// cores the job ran on over each interval from /proc/stat's aggregate line (guest
// time is already in user, so counting it again would double it, and stolen time
// is reported beside the cores, not in them), the memory held as MemTotal less
// MemAvailable, and the mean over the whole job.

// usageProc writes one reading into a fake /proc: the aggregate cpu line, cpus
// per-CPU lines, and the two meminfo lines the sampler reads.
func usageProc(t *testing.T, proc, cpu string, cpus, memTotalKiB, memAvailKiB int) {
	t.Helper()

	stat := "cpu " + cpu + "\n"
	for i := range cpus {
		stat += fmt.Sprintf("cpu%d 0 0 0 0 0 0 0 0 0 0\n", i)
	}

	if err := os.WriteFile(filepath.Join(proc, "stat"), []byte(stat+"intr 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	meminfo := "MemTotal:       " + strconv.Itoa(memTotalKiB) + " kB\nMemFree:        1 kB\n" +
		"MemAvailable:   " + strconv.Itoa(memAvailKiB) + " kB\n"
	if err := os.WriteFile(filepath.Join(proc, "meminfo"), []byte(meminfo), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runUsage(t *testing.T, env []string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "bash", append([]string{"ci-usage.sh"}, args...)...)
	// Never the step summary of the CI job running this suite.
	cmd.Env = append(append(os.Environ(), "GITHUB_STEP_SUMMARY="), env...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ci-usage.sh %v: %v\n%s", args, err, out)
	}

	return string(out)
}

// usageReport is usage.json.
type usageReport struct {
	Label            string  `json:"label"`
	VCPU             int     `json:"vcpu"`
	MemTotalGiB      float64 `json:"mem_total_gib"`
	CPUMeanCores     float64 `json:"cpu_mean_cores"`
	CPUPeakCores     float64 `json:"cpu_peak_cores"`
	CPUStealCores    float64 `json:"cpu_steal_cores"`
	MemPeakGiB       float64 `json:"mem_peak_gib"`
	Samples          int     `json:"samples"`
	SkippedIntervals int     `json:"skipped_intervals"`
	MaxGapSeconds    int     `json:"max_gap_seconds"`
	Partial          bool    `json:"partial"`
	Seconds          int     `json:"seconds"`
}

func readUsage(t *testing.T, dir, out string) usageReport {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatalf("report wrote no usage.json: %v\n%s", err, out)
	}

	var got usageReport
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("usage.json is not JSON: %v\n%s", err, body)
	}

	return got
}

const gib = 1 << 20 // KiB

func TestTheUsageSamplerReportsCoresAndMemoryFromProc(t *testing.T) {
	t.Parallel()

	proc, dir := t.TempDir(), t.TempDir()
	summary := filepath.Join(t.TempDir(), "summary.md")
	env := []string{"CI_USAGE_PROC=" + proc, "GITHUB_STEP_SUMMARY=" + summary}

	// 2 CPUs, 8 GiB. Fields: user nice system idle iowait irq softirq steal guest guest_nice.
	usageProc(t, proc, "100 0 100 700 100 0 0 0 0 0", 2, 8*gib, 6*gib)
	runUsage(t, env, "sample", dir)
	// Busy +400 of +600 (iowait counts as idle): 1.33 cores. Guest +900, which
	// is already inside user and must not be counted again. 4 GiB held.
	usageProc(t, proc, "400 0 200 800 200 0 0 0 900 0", 2, 8*gib, 4*gib)
	runUsage(t, env, "sample", dir)
	// Busy +100 of +1100: 0.18 cores. 1 GiB held.
	usageProc(t, proc, "500 0 200 1700 300 0 0 0 900 0", 2, 8*gib, 7*gib)
	runUsage(t, env, "sample", dir)

	// report takes one more sample, unchanged, which adds no interval.
	out := runUsage(t, env, "report", dir, "host-lifecycle (guard)")
	got := readUsage(t, dir, out)

	if got.Label != "host-lifecycle (guard)" || got.VCPU != 2 || got.MemTotalGiB != 8 || got.Samples != 4 ||
		got.SkippedIntervals != 0 || got.Partial {
		t.Errorf("usage = %+v, want the label, 2 vCPU, 8 GiB, 4 samples, none skipped, not partial", got)
	}

	// Mean: busy 500 of 1700 jiffies across 2 CPUs, weighted by time: 0.59, where
	// the two intervals' plain average would be 0.76.
	if got.CPUMeanCores != 0.59 || got.CPUPeakCores != 1.33 || got.CPUStealCores != 0 || got.MemPeakGiB != 4 {
		t.Errorf("usage = %+v, want a mean of 0.59 cores, a peak of 1.33, none stolen and 4 GiB held at most "+
			"(more would mean guest time was counted twice)", got)
	}

	row, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(row), "| host-lifecycle (guard) | 2 vCPU, 8.0 GiB | 0.59 cores (0.00 stolen) | 1.33 cores | 4.00 GiB |") {
		t.Errorf("the step summary does not carry the measurement:\n%s", row)
	}
}

// AN INTERVAL THE COUNTERS CANNOT ACCOUNT FOR IS LEFT OUT, NEVER READ AS A
// NUMBER, and the rest are still measured: iowait going backwards (which the
// kernel allows) so that more time was busy than passed, the CPU count changing,
// a read that failed and a line cut short. Stolen time is reported on its own.
func TestTheUsageSamplerLeavesOutWhatTheCountersCannotAccountFor(t *testing.T) {
	t.Parallel()

	proc, dir := t.TempDir(), t.TempDir()
	env := []string{"CI_USAGE_PROC=" + proc}

	// busy 200 of 1000.
	usageProc(t, proc, "100 0 100 700 100 0 0 0", 2, 8*gib, 6*gib)
	runUsage(t, env, "sample", dir)
	// iowait -50: busy +100 against total +50, which would read as 4 cores on 2.
	usageProc(t, proc, "200 0 100 700 50 0 0 0", 2, 8*gib, 6*gib)
	runUsage(t, env, "sample", dir)

	// A read that fails appends nothing.
	if err := os.Remove(filepath.Join(proc, "meminfo")); err != nil {
		t.Fatal(err)
	}

	runUsage(t, env, "sample", dir)

	// The CPU count changes: 4 CPUs now.
	usageProc(t, proc, "300 0 100 1100 50 0 0 0", 4, 8*gib, 6*gib)
	runUsage(t, env, "sample", dir)

	// A line cut short by a write that stopped.
	f, err := os.OpenFile(filepath.Join(dir, "samples"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.WriteString("1700000000 4 30\n"); err != nil {
		t.Fatal(err)
	}

	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	// busy -100, which a counter cannot do.
	usageProc(t, proc, "200 0 100 1250 50 0 0 0", 4, 8*gib, 6*gib)
	runUsage(t, env, "sample", dir)

	// busy +100 and steal +100 of +300 across 4 CPUs: 1.33 cores run, 1.33 stolen.
	usageProc(t, proc, "300 0 100 1350 50 0 0 100", 4, 8*gib, 5*gib)
	runUsage(t, env, "sample", dir)

	out := runUsage(t, env, "report", dir, "policy")
	got := readUsage(t, dir, out)

	if got.Samples != 6 || got.SkippedIntervals != 3 || got.VCPU != 4 {
		t.Errorf("usage = %+v, want 6 whole samples (the failed read and the cut line are none) and 3 intervals left out", got)
	}

	if got.CPUMeanCores != 1.33 || got.CPUPeakCores != 1.33 || got.CPUStealCores != 1.33 || got.MemPeakGiB != 3 {
		t.Errorf("usage = %+v, want 1.33 cores run and 1.33 stolen over the one interval that adds up, and 3 GiB held", got)
	}
}

// A SAMPLER THAT STOPPED EARLY IS VISIBLE: the longest gap between two samples
// is reported, and over three intervals the peaks are called partial, because a
// peak over a long gap is an average.
func TestTheUsageSamplerCallsAReportWithALongGapPartial(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		times   []int
		gap     int
		partial bool
	}{
		{[]int{0, 5, 10, 15}, 5, false},
		{[]int{0, 5, 10, 100}, 90, true},
	} {
		dir := t.TempDir()

		var samples strings.Builder
		for i, at := range tc.times {
			fmt.Fprintf(&samples, "%d 2 %d %d 0 %d %d\n", 1700000000+at, 100*i, 1000*(i+1), 8*gib, 6*gib)
		}

		if err := os.WriteFile(filepath.Join(dir, "samples"), []byte(samples.String()), 0o600); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, "interval"), []byte("5\n"), 0o600); err != nil {
			t.Fatal(err)
		}

		// No /proc, so report's own last sample adds nothing.
		summary := filepath.Join(dir, "summary.md")
		out := runUsage(t, []string{"CI_USAGE_PROC=" + filepath.Join(dir, "absent"), "GITHUB_STEP_SUMMARY=" + summary},
			"report", dir, "guard")
		got := readUsage(t, dir, out)

		last := tc.times[len(tc.times)-1]
		if got.MaxGapSeconds != tc.gap || got.Partial != tc.partial || got.Seconds != last || got.Samples != len(tc.times) {
			t.Errorf("samples at %v: %+v; want a longest gap of %d, partial %v, %d s and %d samples", tc.times, got, tc.gap,
				tc.partial, last, len(tc.times))
		}

		peak := "| 0.20 cores |"
		if tc.partial {
			peak = "| 0.20 cores (partial) |"
		}

		row := readFile(t, summary)
		for _, want := range []string{
			"| job | runner | mean CPU | peak CPU | peak memory | sampled |\n|---|---|---|---|---|---|\n",
			peak,
			fmt.Sprintf("| %d s, %d samples, longest gap %d s |", last, len(tc.times), tc.gap),
		} {
			if !strings.Contains(row, want) {
				t.Errorf("samples at %v: the step summary does not say %q:\n%s", tc.times, want, row)
			}
		}
	}
}

// A READING WITH A VALUE MISSING IS NO SAMPLE, never one with a zero in it: no
// MemAvailable would read as the whole runner's memory held, and no per-CPU
// line as a runner of no CPUs.
func TestTheUsageSamplerWritesNothingForAReadingWithAValueMissing(t *testing.T) {
	t.Parallel()

	for name, files := range map[string][2]string{
		"no MemAvailable": {"cpu 1 0 1 8 0 0 0 0\ncpu0 0 0 0 0 0 0 0 0\n", "MemTotal: 8388608 kB\n"},
		"no MemTotal":     {"cpu 1 0 1 8 0 0 0 0\ncpu0 0 0 0 0 0 0 0 0\n", "MemAvailable: 1 kB\n"},
		"no per-CPU line": {"cpu 1 0 1 8 0 0 0 0\n", "MemTotal: 8388608 kB\nMemAvailable: 1 kB\n"},
		"no cpu time":     {"cpu 0 0 0 0 0 0 0 0\ncpu0 0 0 0 0 0 0 0 0\n", "MemTotal: 8388608 kB\nMemAvailable: 1 kB\n"},
	} {
		proc, dir := t.TempDir(), t.TempDir()
		for file, body := range map[string]string{"stat": files[0], "meminfo": files[1]} {
			if err := os.WriteFile(filepath.Join(proc, file), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
		}

		runUsage(t, []string{"CI_USAGE_PROC=" + proc}, "sample", dir)

		if body, err := os.ReadFile(filepath.Join(dir, "samples")); !os.IsNotExist(err) {
			t.Errorf("%s: sample wrote %q (%v), want nothing", name, body, err)
		}
	}
}

// START TAKES ITS OWN FIRST SAMPLE, into a samples file it starts empty, so a
// job shorter than one interval is still measured from its start and a file a
// previous run left is not read as this one's.
func TestTheUsageSamplerSamplesOnceAsItStarts(t *testing.T) {
	t.Parallel()

	proc, dir := t.TempDir(), t.TempDir()
	usageProc(t, proc, "100 0 100 800 0 0 0 0 0 0", 2, 8*gib, 6*gib)

	if err := os.WriteFile(filepath.Join(dir, "samples"), []byte("1 2 3 4 5 6 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	startUsage(t, []string{"CI_USAGE_PROC=" + proc, "CI_USAGE_INTERVAL=600"}, dir)

	body := readFile(t, filepath.Join(dir, "samples"))
	if lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n"); len(lines) != 1 ||
		!strings.HasSuffix(lines[0], " 2 200 1000 0 8388608 6291456") {
		t.Errorf("start left samples %q, want its own one reading alone", body)
	}

	runUsage(t, []string{"CI_USAGE_PROC=" + proc}, "report", dir, "x")
}

// A SAMPLER THAT GOT NOTHING SAYS SO AND FAILS NOTHING: the job it watched is
// not the sampler's to fail, and a missing number is not a zero.
func TestTheUsageSamplerSaysWhenItHasNoMeasurement(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	summary := filepath.Join(t.TempDir(), "summary.md")
	env := []string{"CI_USAGE_PROC=" + filepath.Join(t.TempDir(), "absent"), "GITHUB_STEP_SUMMARY=" + summary}

	out := runUsage(t, env, "report", dir, "replay")
	if !strings.Contains(out, "no measurement for replay") {
		t.Errorf("report with no samples said %q, want that it has no measurement", out)
	}

	if _, err := os.Stat(filepath.Join(dir, "usage.json")); !os.IsNotExist(err) {
		t.Errorf("report with no samples wrote usage.json (%v)", err)
	}

	if row, err := os.ReadFile(summary); err != nil || !strings.Contains(string(row), "| replay | no measurement |") {
		t.Errorf("the step summary does not say there is no measurement: %q, %v", row, err)
	}

	// Nor does a directory it cannot write fail the step that started it.
	runUsage(t, env, "start", filepath.Join(summary, "not-a-directory"))
}

// startUsage runs `start` in a process group of its own, which the sampler it
// leaves behind stays in, and kills that group when the test ends, so a test
// that fails before report cannot leave the loop running.
func startUsage(t *testing.T, env []string, dir string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "bash", "ci-usage.sh", "start", dir)
	cmd.Env = append(append(os.Environ(), "GITHUB_STEP_SUMMARY="), env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}

	pgid := cmd.Process.Pid
	t.Cleanup(func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })

	if err := cmd.Wait(); err != nil {
		t.Fatalf("ci-usage.sh start: %v", err)
	}
}

// THE SAMPLER STARTED IN THE BACKGROUND SAMPLES, AND report STOPS IT: a loop
// left running would outlive its step and sample a runner the job has left.
func TestTheUsageSamplerSamplesInTheBackgroundAndStopsWhenItReports(t *testing.T) {
	t.Parallel()

	proc, dir := t.TempDir(), t.TempDir()
	env := []string{"CI_USAGE_PROC=" + proc, "CI_USAGE_INTERVAL=0.05"}

	usageProc(t, proc, "100 0 100 800 0 0 0 0 0 0", 2, 8*gib, 6*gib)
	startUsage(t, env, dir)

	raw, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatalf("start wrote no pid: %v", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("the pid file holds %q", raw)
	}

	// THE LOOP SAMPLES ON ITS OWN: a reading written after start returned
	// reaches the samples, whole.
	usageProc(t, proc, "300 0 100 1000 0 0 0 0 0 0", 2, 8*gib, 4*gib)

	deadline := time.Now().Add(30 * time.Second)
	for {
		body, err := os.ReadFile(filepath.Join(dir, "samples"))
		if err == nil && strings.Contains(string(body), " 2 400 1400 0 8388608 4194304\n") {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("the background sampler never read the second reading: %q, %v", body, err)
		}

		time.Sleep(20 * time.Millisecond)
	}

	out := runUsage(t, env, "report", dir, "lint")
	// Busy +200 of +400 across 2 CPUs: one core, over the one interval in which
	// the counters moved; the readings that repeat add no interval.
	if got := readUsage(t, dir, out); got.CPUPeakCores != 1 || got.CPUMeanCores != 1 || got.MemPeakGiB != 4 {
		t.Errorf("usage = %+v, want the background reading's one core and 4 GiB", got)
	}

	if _, err := os.Stat(filepath.Join(dir, "pid")); !os.IsNotExist(err) {
		t.Errorf("report left the pid file (%v)", err)
	}

	reported, err := os.ReadFile(filepath.Join(dir, "samples"))
	if err != nil {
		t.Fatal(err)
	}

	// THE LOOP ENDS, within its interval: its TERM trap runs once the sleep it
	// is in returns. The bound guards a hang, nothing more.
	deadline = time.Now().Add(30 * time.Second)
	for syscall.Kill(pid, 0) == nil {
		if time.Now().After(deadline) {
			t.Fatalf("the sampler %d is still running 30s after report stopped it", pid)
		}

		time.Sleep(20 * time.Millisecond)
	}

	// AND IT SAMPLED NOTHING ON ITS WAY OUT.
	if after, err := os.ReadFile(filepath.Join(dir, "samples")); err != nil || !bytes.Equal(after, reported) {
		t.Errorf("the sampler wrote after report stopped it: %q, then %q (%v)", reported, after, err)
	}
}

// AN INTERVAL THAT IS NOT A POSITIVE NUMBER IS FIVE SECONDS, not a loop that
// spins or one that never samples.
func TestTheUsageSamplerTakesFiveSecondsForABadInterval(t *testing.T) {
	t.Parallel()

	for _, interval := range []string{"", "0", "-1", "abc", "1.2.3", ".5", "5."} {
		proc, dir := t.TempDir(), t.TempDir()
		usageProc(t, proc, "100 0 100 800 0 0 0 0 0 0", 2, 8*gib, 6*gib)
		startUsage(t, []string{"CI_USAGE_PROC=" + proc, "CI_USAGE_INTERVAL=" + interval}, dir)

		if got, err := os.ReadFile(filepath.Join(dir, "interval")); err != nil || string(got) != "5\n" {
			t.Errorf("CI_USAGE_INTERVAL=%q: the sampler runs every %q (%v), want 5", interval, got, err)
		}

		runUsage(t, []string{"CI_USAGE_PROC=" + proc}, "report", dir, "x")
	}
}

// THE SAMPLER'S STEPS FAIL NOTHING IN CI: every step that starts, reports or
// uploads it continues on error, so a measurement that broke (an upload refused,
// a disk full) never fails the job it watched or the groups that need it; and
// every job that starts it reports and uploads it.
func TestEveryUsageStepInCIContinuesOnError(t *testing.T) {
	t.Parallel()

	var workflow struct {
		Jobs map[string]struct {
			Steps []struct {
				Run             string         `yaml:"run"`
				If              string         `yaml:"if"`
				ContinueOnError bool           `yaml:"continue-on-error"`
				With            map[string]any `yaml:"with"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join("..", ".github", "workflows", "ci.yml"))), &workflow); err != nil {
		t.Fatal(err)
	}

	jobs := 0

	for name, job := range workflow.Jobs {
		var start, report, upload int

		for _, step := range job.Steps {
			uploads := strings.HasPrefix(fmt.Sprint(step.With["name"]), "ci-usage-")

			switch {
			case strings.Contains(step.Run, "scripts/ci-usage.sh start"):
				start++
			case strings.Contains(step.Run, "scripts/ci-usage.sh report"):
				report++
			case uploads:
				upload++
			default:
				continue
			}

			if !step.ContinueOnError {
				t.Errorf("job %s: a ci-usage step does not continue on error: %q %v", name, step.Run, step.With)
			}

			if !strings.Contains(step.Run, " start ") && step.If != "always()" {
				t.Errorf("job %s: a ci-usage report or upload runs only on success (if: %q)", name, step.If)
			}
		}

		if start+report+upload == 0 {
			continue
		}

		jobs++

		if start != 1 || report != 1 || upload != 1 {
			t.Errorf("job %s starts the sampler %d times, reports %d and uploads %d; want one of each", name, start, report, upload)
		}
	}

	if jobs < 2 {
		t.Errorf("%d jobs sample their runner, want gate-binaries and host-lifecycle", jobs)
	}
}
