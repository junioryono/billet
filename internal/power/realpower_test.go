package power

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
// through the real sysfs, the cap writes what the settings ask, a launch's
// restore puts every value back, and each cycle writes what it read and how long
// the restore took to a results file.
//
// It skips unless BILLET_TEST_REAL_POWER_OUT names that file and the test runs
// as root. It writes the host's live cpufreq ceiling, so it is meant for a
// drained node; whatever happens inside the test, the controller is stopped and
// the baseline the test read first is put back, in the order the kernel takes
// it, and read back. Only a crash of the test binary itself escapes that, which
// is why the session that runs it also restores cpufreq on exit.
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
	// THE BASELINE IS READ UNDER THE HOST'S CLAIM: a node running node.power
	// here could have a cap in force, which this test would take for the
	// baseline and write back.
	probe, err := New(Options{SysfsRoot: sysfs, StateDir: t.TempDir(), Settings: settings})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := probe.ClaimHost(); err != nil {
		t.Fatalf("claim the host's cpufreq: %v", err)
	}
	baseline, err := readBaseline(sysfs)
	if err != nil {
		probe.ReleaseHost()
		t.Fatalf("read the host's cpufreq baseline: %v", err)
	}
	// THE BASELINE IS PROVED FULL BEFORE ANYTHING CAN WRITE IT BACK, as the
	// controller proves it before it caps: a ceiling firmware is holding low for
	// a moment would otherwise be written back as a lasting cap.
	if err := baseline.requireFull(probe); err != nil {
		probe.ReleaseHost()
		t.Fatal(err)
	}
	probe.ReleaseHost()
	// REGISTERED FIRST, SO IT RUNS LAST, after every run's own shutdown: the host
	// leaves with the ceiling it came with, written in the kernel's order and read
	// back, under a claim of its own.
	t.Cleanup(func() {
		if err := withHostClaimed(sysfs, baseline.restore); err != nil {
			t.Errorf("put the host's cpufreq baseline back: %v", err)
		}
	})

	file, err := os.OpenFile(out, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open the results file: %v", err)
	}
	defer file.Close()
	encoder := json.NewEncoder(file)

	for run := range runs {
		line, err := capAndRestoreOnce(t, sysfs, settings, baseline)
		if line != nil {
			line["run"] = run
			if err := encoder.Encode(line); err != nil {
				t.Fatalf("write the results: %v", err)
			}
		}
		if err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
}

// capAndRestoreOnce runs one controller through one cap and one launch's
// restore, and always stops it and puts the baseline back before returning.
func capAndRestoreOnce(t *testing.T, sysfs string, settings Settings, baseline ceiling) (map[string]any, error) {
	t.Helper()

	// NOT t.TempDir: the controller's record must outlive a failed restore, so
	// the directory is removed only once the baseline is proved back.
	stateDir, err := os.MkdirTemp("", "billet-realpower-") //nolint:usetesting // t.TempDir's cleanup would delete the record before the baseline is proved back

	if err != nil {
		return nil, fmt.Errorf("make a state directory: %w", err)
	}

	c, err := New(Options{SysfsRoot: sysfs, StateDir: stateDir, Settings: settings, Tick: 100 * time.Millisecond})
	if err != nil {
		return nil, fmt.Errorf("New: %w", err)
	}
	if err := c.CheckHost(); err != nil {
		return nil, fmt.Errorf("CheckHost: %w", err)
	}
	if err := c.ClaimHost(); err != nil {
		return nil, fmt.Errorf("ClaimHost: %w", err)
	}

	var idle atomic.Bool
	idle.Store(true)
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	done := make(chan struct{})
	go func() {
		defer close(done)
		c.Run(ctx, idle.Load)
	}()

	// WHATEVER HAPPENS BELOW: the node turns busy, the loop is stopped and
	// joined, the controller restores and closes, and the baseline is proved
	// back before the record's directory goes.
	var result error
	defer func() {
		idle.Store(false)
		cancel()
		<-done
		closeErr := c.Close(context.WithoutCancel(t.Context()))
		restoreErr := withHostClaimed(sysfs, baseline.restore)
		if closeErr == nil && restoreErr == nil {
			_ = os.RemoveAll(stateDir)
		}
		if err := errors.Join(closeErr, restoreErr); err != nil {
			t.Errorf("after the run, the controller's record is kept in %s: %v", stateDir, err)
		}
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
	if !capped {
		return nil, errors.New("the controller did not cap the idle host within 30 s")
	}
	during, err := readBaseline(sysfs)
	if err != nil {
		return nil, fmt.Errorf("read the capped ceiling: %w", err)
	}
	// THE LOOP STOPS BEFORE THE LAUNCH, so the restore measured is the launch's
	// own: a running loop restores a busy host by itself on its next tick.
	cancel()
	<-done

	// THE CAP CHANGED THE HOST, and wrote what was asked, not merely a state
	// change: settings the host already had would pass with nothing written.
	if baseline.mismatches(during) == 0 {
		result = errors.Join(result, errors.New("the cap changed none of the host's cpufreq values"))
	}
	if settings.DisableBoost && during.boost != "0" {
		result = errors.Join(result, fmt.Errorf("boost reads %q while capped, want 0", during.boost))
	}
	if settings.MaxFreqKHz > 0 {
		want := strconv.Itoa(settings.MaxFreqKHz)
		for path, value := range during.ceilings {
			if value != want {
				result = errors.Join(result, fmt.Errorf("%s reads %s while capped, want %s", path, value, want))

				break
			}
		}
	}

	idle.Store(false)
	asked := time.Now()
	c.RestoreBeforeLaunch(context.WithoutCancel(t.Context()), "realpower")
	restoreTook := time.Since(asked)
	// RestoreBeforeLaunch may return at its bound while the restore goes on, so
	// the comparison waits out the kernel's own asynchronous read-back.
	after, err := baseline.waitFor(sysfs, 3*time.Second)
	if err != nil {
		result = errors.Join(result, err)
	}

	policy0 := filepath.Join(sysfs, "devices", "system", "cpu", "cpufreq", "policy0", "scaling_max_freq")
	return map[string]any{
		"disable_boost": settings.DisableBoost, "max_khz": settings.MaxFreqKHz,
		"cap_seconds": capTook.Seconds(), "restore_seconds": restoreTook.Seconds(),
		"boost_before": baseline.boost, "boost_capped": during.boost, "boost_after": after.boost,
		"policy0_max_before": baseline.ceilings[policy0], "policy0_max_capped": during.ceilings[policy0],
		"policy0_max_after": after.ceilings[policy0], "policies": len(baseline.ceilings),
		"mismatched_after": baseline.mismatches(after),
	}, result
}

// ceiling is a host's cpufreq ceiling: the global boost switch, each policy's
// own boost switch and each policy's scaling_max_freq, keyed by path.
type ceiling struct {
	root             string
	boostPath, boost string
	policyBoost      map[string]string
	ceilings         map[string]string
}

// withHostClaimed runs write under a claim of the host's cpufreq, and writes
// nothing when another process holds it, a stuck write of this test's own
// controller included.
func withHostClaimed(sysfs string, write func() error) error {
	holder, err := New(Options{SysfsRoot: sysfs, StateDir: os.TempDir()})
	if err != nil {
		return err
	}
	if err := holder.ClaimHost(); err != nil {
		return fmt.Errorf("nothing was written: %w", err)
	}
	defer holder.ReleaseHost()

	return write()
}

// requireFull refuses a baseline any of whose ceilings is below the full value
// the controller would restore it to.
func (c ceiling) requireFull(probe *Controller) error {
	paths := make([]string, 0, len(c.ceilings))
	for path := range c.ceilings {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		full, err := probe.fullCeiling(filepath.Base(filepath.Dir(path)))
		if err != nil {
			return fmt.Errorf("read %s's full ceiling: %w", filepath.Dir(path), err)
		}
		if c.ceilings[path] != strconv.Itoa(full) {
			return fmt.Errorf("%s reads %s, not its full %d: refusing to run on a host held below its ceiling",
				path, c.ceilings[path], full)
		}
	}

	return nil
}

func TestTheRealPowerTestRefusesABaselineBelowItsFullCeiling(t *testing.T) {
	h := newFakeHost(t, 3)
	probe, err := New(Options{SysfsRoot: h.root, StateDir: t.TempDir(), Settings: Settings{DisableBoost: true, IdleAfter: time.Minute}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	full, err := readBaseline(h.root)
	if err != nil {
		t.Fatalf("readBaseline: %v", err)
	}
	if err := full.requireFull(probe); err != nil {
		t.Fatalf("a baseline at its full ceiling was refused: %v", err)
	}

	h.write(h.policyFile(1, "scaling_max_freq"), midKHz+"\n")
	held, err := readBaseline(h.root)
	if err != nil {
		t.Fatalf("readBaseline: %v", err)
	}
	err = held.requireFull(probe)
	if err == nil || !strings.Contains(err.Error(), "policy1") || !strings.Contains(err.Error(), "not its full 2450000") {
		t.Fatalf("a baseline with policy1 at %s: got %v, want a refusal naming policy1's full ceiling", midKHz, err)
	}
}

func readBaseline(sysfs string) (ceiling, error) {
	cpufreq := filepath.Join(sysfs, "devices", "system", "cpu", "cpufreq")
	c := ceiling{root: sysfs, boostPath: filepath.Join(cpufreq, "boost"), policyBoost: map[string]string{},
		ceilings: map[string]string{}}
	if v, err := readSysfs(c.boostPath); err == nil {
		c.boost = v
	}
	entries, err := os.ReadDir(cpufreq)
	if err != nil {
		return ceiling{}, err
	}
	var names []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "policy") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		if v, err := readSysfs(filepath.Join(cpufreq, name, "boost")); err == nil {
			c.policyBoost[filepath.Join(cpufreq, name, "boost")] = v
		}
		path := filepath.Join(cpufreq, name, "scaling_max_freq")
		v, err := readSysfs(path)
		if err != nil {
			return ceiling{}, err
		}
		c.ceilings[path] = v
	}

	return c, nil
}

// restore writes the baseline back in the order the controller uses: the
// global switch, each policy's switch where it differs, then each ceiling; and
// reads it all back.
func (c ceiling) restore() error {
	var failures []error
	write := func(path, value string, always bool) {
		// A SWITCH ALREADY RIGHT IS LEFT ALONE, since the kernel refuses a
		// policy's boost while the global switch is off. A CEILING IS ALWAYS
		// WRITTEN, as the controller writes it: it can read the baseline while a
		// lower request is still waiting to be applied.
		if !always {
			if now, err := readSysfs(path); err == nil && now == value {
				return
			}
		}
		if err := writeSysfs(path, value); err != nil {
			failures = append(failures, fmt.Errorf("write %s: %w", path, err))
		}
	}
	if c.boost != "" {
		write(c.boostPath, c.boost, false)
	}
	for _, path := range sortedKeys(c.policyBoost) {
		write(path, c.policyBoost[path], false)
	}
	for _, path := range sortedKeys(c.ceilings) {
		write(path, c.ceilings[path], true)
	}
	if _, err := c.waitFor(c.root, 3*time.Second); err != nil {
		failures = append(failures, err)
	}

	return errors.Join(failures...)
}

// waitFor reads the host until it matches the baseline or the bound passes.
func (c ceiling) waitFor(sysfs string, bound time.Duration) (ceiling, error) {
	deadline := time.Now().Add(bound)
	for {
		now, err := readBaseline(sysfs)
		if err == nil && c.mismatches(now) == 0 {
			return now, nil
		}
		if time.Now().After(deadline) {
			if err != nil {
				return now, err
			}

			return now, fmt.Errorf("%d cpufreq values still differ from the baseline after %s", c.mismatches(now),
				bound)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (c ceiling) mismatches(other ceiling) int {
	n := 0
	if c.boost != other.boost {
		n++
	}
	for path, v := range c.policyBoost {
		if other.policyBoost[path] != v {
			n++
		}
	}
	for path, v := range c.ceilings {
		if other.ceilings[path] != v {
			n++
		}
	}

	return n
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}
