package power

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// THE CONTROLLER ON A REAL HOST'S cpufreq, end to end: it caps an idle host
// through the real sysfs, a launch's restore puts every value back, and each
// cycle writes what it read and how long the restore took to a results file.
//
// It skips unless BILLET_TEST_REAL_POWER_OUT names that file and the test runs
// as root. It writes the host's live cpufreq ceiling, so it is meant for a
// drained node; whatever happens, its cleanup writes back what it read first.
//
// BILLET_TEST_REAL_POWER_RUNS (default 5), BILLET_TEST_REAL_POWER_DISABLE_BOOST
// (1 to turn boost off) and BILLET_TEST_REAL_POWER_MAX_KHZ (a ceiling) choose the
// cap.
func TestRealCapAndRestoreOnThisHost(t *testing.T) {
	out := os.Getenv("BILLET_TEST_REAL_POWER_OUT")
	if out == "" {
		t.Skip("set BILLET_TEST_REAL_POWER_OUT to a results file to run the controller on this host")
	}
	if os.Geteuid() != 0 {
		t.Skip("not root: the controller writes cpufreq")
	}

	runs := 5
	if raw := os.Getenv("BILLET_TEST_REAL_POWER_RUNS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("BILLET_TEST_REAL_POWER_RUNS is %q", raw)
		}
		runs = n
	}
	settings := Settings{IdleAfter: time.Second, DisableBoost: os.Getenv("BILLET_TEST_REAL_POWER_DISABLE_BOOST") == "1"}
	if raw := os.Getenv("BILLET_TEST_REAL_POWER_MAX_KHZ"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("BILLET_TEST_REAL_POWER_MAX_KHZ is %q", raw)
		}
		settings.MaxFreqKHz = n
	}
	if !settings.caps() {
		t.Fatal("set BILLET_TEST_REAL_POWER_DISABLE_BOOST=1, BILLET_TEST_REAL_POWER_MAX_KHZ, or both")
	}

	const sysfs = "/sys"
	before := readCeiling(t, sysfs)
	// WHATEVER HAPPENS, the host leaves with the ceiling it came with.
	t.Cleanup(func() {
		for path, value := range before {
			if now, err := readSysfs(path); err == nil && now == value {
				continue
			}
			if err := writeSysfs(path, value); err != nil {
				t.Errorf("restore %s to %s by hand: %v", path, value, err)
			}
		}
	})

	file, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open the results file: %v", err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)

	for run := range runs {
		c, err := New(Options{SysfsRoot: sysfs, StateDir: t.TempDir(), Settings: settings,
			Tick: 100 * time.Millisecond})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := c.CheckHost(); err != nil {
			t.Fatalf("CheckHost: %v", err)
		}

		var idle atomic.Bool
		idle.Store(true)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, idle.Load)
		}()

		started := time.Now()
		capped := false
		for time.Since(started) < 30*time.Second {
			if state(c.st.Load()) == stateCapped {
				capped = true

				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		capTook := time.Since(started)
		during := readCeiling(t, sysfs)

		idle.Store(false)
		asked := time.Now()
		c.RestoreBeforeLaunch(ctx, "realpower")
		restoreTook := time.Since(asked)
		after := readCeiling(t, sysfs)

		cancel()
		<-done
		if err := c.Close(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Close: %v", err)
		}

		mismatched := 0
		for path, value := range before {
			if after[path] != value {
				mismatched++
			}
		}
		policy0 := filepath.Join(sysfs, "devices", "system", "cpu", "cpufreq", "policy0", "scaling_max_freq")
		boost := filepath.Join(sysfs, "devices", "system", "cpu", "cpufreq", "boost")
		line := map[string]any{
			"run": run, "disable_boost": settings.DisableBoost, "max_khz": settings.MaxFreqKHz,
			"capped": capped, "cap_seconds": capTook.Seconds(), "restore_seconds": restoreTook.Seconds(),
			"boost_before": before[boost], "boost_capped": during[boost], "boost_after": after[boost],
			"policy0_max_before": before[policy0], "policy0_max_capped": during[policy0],
			"policy0_max_after": after[policy0], "values": len(before), "mismatched_after": mismatched,
		}
		if err := encoder.Encode(line); err != nil {
			t.Fatalf("write the results: %v", err)
		}
		if !capped {
			t.Errorf("run %d: the controller did not cap the idle host within 30 s", run)
		}
		if mismatched > 0 {
			t.Errorf("run %d: %d of %d cpufreq values differ after the launch's restore", run, mismatched,
				len(before))
		}
	}
}

// readCeiling reads the global boost switch and every policy's
// scaling_max_freq, keyed by path.
func readCeiling(t *testing.T, sysfs string) map[string]string {
	t.Helper()

	cpufreq := filepath.Join(sysfs, "devices", "system", "cpu", "cpufreq")
	values := map[string]string{}
	if v, err := readSysfs(filepath.Join(cpufreq, "boost")); err == nil {
		values[filepath.Join(cpufreq, "boost")] = v
	}
	entries, err := os.ReadDir(cpufreq)
	if err != nil {
		t.Fatalf("read %s: %v", cpufreq, err)
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "policy") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		path := filepath.Join(cpufreq, name, "scaling_max_freq")
		v, err := readSysfs(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		values[path] = v
	}

	return values
}
