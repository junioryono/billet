package scripts_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// THE RUNNER-USAGE SAMPLER IS EXECUTED AGAINST A FAKE /proc, NOT READ. What it
// reports sizes a CI job's runner, so each number is known before it runs: the
// busy cores of each interval from /proc/stat's aggregate line (guest time is
// already in user, so counting it again would double it), the memory held as
// MemTotal less MemAvailable, and the mean over the whole job.

// usageProc writes one reading into a fake /proc: the aggregate cpu line, two
// per-CPU lines, and the two meminfo lines the sampler reads.
func usageProc(t *testing.T, proc, cpu string, memTotalKiB, memAvailKiB int) {
	t.Helper()

	stat := "cpu " + cpu + "\ncpu0 0 0 0 0 0 0 0 0 0 0\ncpu1 0 0 0 0 0 0 0 0 0 0\nintr 1\n"
	if err := os.WriteFile(filepath.Join(proc, "stat"), []byte(stat), 0o600); err != nil {
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
	cmd.Env = append(os.Environ(), env...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ci-usage.sh %v: %v\n%s", args, err, out)
	}

	return string(out)
}

func TestTheUsageSamplerReportsCoresAndMemoryFromProc(t *testing.T) {
	t.Parallel()

	proc, dir := t.TempDir(), t.TempDir()
	summary := filepath.Join(t.TempDir(), "summary.md")
	env := []string{"CI_USAGE_PROC=" + proc, "GITHUB_STEP_SUMMARY=" + summary}

	const gib = 1 << 20 // KiB

	// 2 CPUs, 8 GiB. Fields: user nice system idle iowait irq softirq steal guest guest_nice.
	usageProc(t, proc, "100 0 100 700 100 0 0 0 0 0", 8*gib, 6*gib)
	runUsage(t, env, "sample", dir)
	// Busy +400 of +600 (iowait counts as idle): 1.33 cores. Guest +900, which
	// is already inside user and must not be counted again. 4 GiB held.
	usageProc(t, proc, "400 0 200 800 200 0 0 0 900 0", 8*gib, 4*gib)
	runUsage(t, env, "sample", dir)
	// Busy +100 of +600: 0.33 cores. 1 GiB held.
	usageProc(t, proc, "500 0 200 1200 300 0 0 0 900 0", 8*gib, 7*gib)
	runUsage(t, env, "sample", dir)

	// report takes one more sample, unchanged, which adds no interval.
	out := runUsage(t, env, "report", dir, "host-lifecycle (guard)")

	body, err := os.ReadFile(filepath.Join(dir, "usage.json"))
	if err != nil {
		t.Fatalf("report wrote no usage.json: %v\n%s", err, out)
	}

	var got struct {
		Label        string  `json:"label"`
		VCPU         int     `json:"vcpu"`
		MemTotalGiB  float64 `json:"mem_total_gib"`
		CPUMeanCores float64 `json:"cpu_mean_cores"`
		CPUPeakCores float64 `json:"cpu_peak_cores"`
		MemPeakGiB   float64 `json:"mem_peak_gib"`
		Samples      int     `json:"samples"`
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("usage.json is not JSON: %v\n%s", err, body)
	}

	if got.Label != "host-lifecycle (guard)" || got.VCPU != 2 || got.MemTotalGiB != 8 || got.Samples != 4 {
		t.Errorf("usage = %+v, want the label, 2 vCPU, 8 GiB and 4 samples", got)
	}

	// Mean: busy 500 of 1200 jiffies across 2 CPUs.
	if got.CPUMeanCores != 0.83 || got.CPUPeakCores != 1.33 || got.MemPeakGiB != 4 {
		t.Errorf("usage = %+v, want a mean of 0.83 cores, a peak of 1.33 and 4 GiB held at most "+
			"(1.67 and 1.80 would mean guest time was counted twice)", got)
	}

	row, err := os.ReadFile(summary)
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(row), "| host-lifecycle (guard) | 2 vCPU, 8.0 GiB | 0.83 cores | 1.33 cores | 4.00 GiB |") {
		t.Errorf("the step summary does not carry the measurement:\n%s", row)
	}
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
}

// THE SAMPLER STARTED IN THE BACKGROUND SAMPLES, AND report STOPS IT: a loop
// left running would outlive its step and sample a runner the job has left.
func TestTheUsageSamplerStopsWhenItReports(t *testing.T) {
	t.Parallel()

	proc, dir := t.TempDir(), t.TempDir()
	env := []string{"CI_USAGE_PROC=" + proc, "CI_USAGE_INTERVAL=0.05"}

	usageProc(t, proc, "100 0 100 800 0 0 0 0 0 0", 1<<20, 1<<19)
	runUsage(t, env, "start", dir)

	raw, err := os.ReadFile(filepath.Join(dir, "pid"))
	if err != nil {
		t.Fatalf("start wrote no pid: %v", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("the pid file holds %q", raw)
	}

	// THE LOOP SAMPLES ON ITS OWN: more lines than start's first.
	deadline := time.Now().Add(30 * time.Second)
	for {
		body, err := os.ReadFile(filepath.Join(dir, "samples"))
		if err == nil && strings.Count(string(body), "\n") >= 3 {
			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("the background sampler took no sample past start's own: %q, %v", body, err)
		}

		time.Sleep(20 * time.Millisecond)
	}

	runUsage(t, env, "report", dir, "lint")

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
	if after, err := os.ReadFile(filepath.Join(dir, "samples")); err != nil || string(after) != string(reported) {
		t.Errorf("the sampler wrote after report stopped it: %q, then %q (%v)", reported, after, err)
	}
}
