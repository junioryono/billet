package power

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// The reference host as read on 2026-10-23 (ubuntu-01, AMD EPYC 7763, kernel
// 7.0): acpi-cpufreq under schedutil, three available frequencies, a ceiling at
// the highest of them, and boost on globally and per policy.
const (
	fullKHz  = "2450000"
	midKHz   = "2000000"
	lowKHz   = "1500000"
	availKHz = "2450000 2000000 1500000 "
)

// cpufreqRel is cpufreq's directory under a sysfs root.
var cpufreqRel = filepath.Join("devices", "system", "cpu", "cpufreq")

// fakeHost is a cpufreq tree under a temporary sysfs root.
type fakeHost struct {
	t    *testing.T
	root string
	n    int
}

func newFakeHost(t *testing.T, policies int) *fakeHost {
	t.Helper()

	h := &fakeHost{t: t, root: t.TempDir(), n: policies}
	h.write("devices/system/cpu/cpufreq/boost", "1\n")
	for i := range policies {
		p := policyName(i)
		for file, value := range map[string]string{
			"scaling_max_freq":              fullKHz,
			"scaling_min_freq":              lowKHz,
			"scaling_available_frequencies": availKHz,
			"cpuinfo_max_freq":              "3530493",
			"scaling_driver":                "acpi-cpufreq",
			"boost":                         "1",
		} {
			h.write(filepath.Join(cpufreqRel, p, file), value+"\n")
		}
	}

	return h
}

func policyName(i int) string { return "policy" + itoa(i) }

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}

	return string(b)
}

func (h *fakeHost) path(rel string) string { return filepath.Join(h.root, rel) }

func (h *fakeHost) write(rel, value string) {
	h.t.Helper()

	path := h.path(rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		h.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(value), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

func (h *fakeHost) remove(rel string) {
	h.t.Helper()

	if err := os.RemoveAll(h.path(rel)); err != nil {
		h.t.Fatal(err)
	}
	if _, err := os.Stat(h.path(rel)); !errors.Is(err, fs.ErrNotExist) {
		h.t.Fatalf("%s is still there: %v", rel, err)
	}
}

func (h *fakeHost) read(rel string) string {
	h.t.Helper()

	b, err := os.ReadFile(h.path(rel))
	if err != nil {
		h.t.Fatal(err)
	}

	return strings.TrimSpace(string(b))
}

func (h *fakeHost) boost() string { return h.read("devices/system/cpu/cpufreq/boost") }

func (h *fakeHost) policyFile(i int, file string) string {
	return filepath.Join(cpufreqRel, policyName(i), file)
}

func (h *fakeHost) maxFreq(i int) string { return h.read(h.policyFile(i, "scaling_max_freq")) }

// requireFull fails unless every value a cap changes is back where the host
// started.
func (h *fakeHost) requireFull(stateDir string) {
	h.t.Helper()

	if got := h.boost(); got != "1" {
		h.t.Errorf("boost = %s, want it restored to 1", got)
	}
	for i := range h.n {
		if got := h.maxFreq(i); got != fullKHz {
			h.t.Errorf("%s scaling_max_freq = %s, want it restored to %s", policyName(i), got, fullKHz)
		}
		if got := h.read(h.policyFile(i, "boost")); got != "1" {
			h.t.Errorf("%s boost = %s, want it restored to 1", policyName(i), got)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, RecordName)); !errors.Is(err, fs.ErrNotExist) {
		h.t.Errorf("the record is still there after a restore (%v)", err)
	}
}

// requireCapped fails unless the cap the settings ask for is in force, with
// the record of what was there.
func (h *fakeHost) requireCapped(stateDir string) {
	h.t.Helper()

	if got := h.boost(); got != "0" {
		h.t.Errorf("boost = %s, want 0 while capped", got)
	}
	for i := range h.n {
		if got := h.maxFreq(i); got != lowKHz {
			h.t.Errorf("%s scaling_max_freq = %s, want %s while capped", policyName(i), got, lowKHz)
		}
	}
	body, err := os.ReadFile(filepath.Join(stateDir, RecordName))
	if err != nil {
		h.t.Fatalf("no record of what the cap changed: %v", err)
	}
	if !bytes.Contains(body, []byte(`"scaling_max_freq": "`+fullKHz+`"`)) || !bytes.Contains(body, []byte(`"boost": "1"`)) {
		h.t.Errorf("the record does not hold what was there before the cap:\n%s", body)
	}
}

// observed records what an Observer is told.
type observed struct {
	mu       sync.Mutex
	changes  map[string]int
	launches []time.Duration
}

func (o *observed) PowerChanged(transition, outcome string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.changes == nil {
		o.changes = map[string]int{}
	}
	o.changes[transition+"/"+outcome]++
}

func (o *observed) PowerLaunchRestore(took time.Duration) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.launches = append(o.launches, took)
}

func (o *observed) count(key string) int {
	o.mu.Lock()
	defer o.mu.Unlock()

	return o.changes[key]
}

func (o *observed) launchRestores() []time.Duration {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]time.Duration(nil), o.launches...)
}

var capBoth = Settings{IdleAfter: time.Minute, DisableBoost: true, MaxFreqKHz: 1500000}

func newController(t *testing.T, h *fakeHost, s Settings, o Observer) (*Controller, string) {
	t.Helper()

	stateDir := t.TempDir()
	c, err := New(Options{SysfsRoot: h.root, StateDir: stateDir, Settings: s, Observer: o,
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}

	return c, stateDir
}

// capNow drives the controller through the loop's own step until the cap is
// in force: proved full, then idle for IdleAfter.
func capNow(t *testing.T, c *Controller) {
	t.Helper()

	c.step(t.Context(), func() bool { return true }) // unknown -> full: no record
	c.mu.Lock()
	c.busyAt = time.Now().Add(-2 * c.settings.IdleAfter)
	c.mu.Unlock()
	c.step(t.Context(), func() bool { return true })
	if state(c.st.Load()) != stateCapped {
		t.Fatalf("the controller did not cap an idle node; state %d", c.st.Load())
	}
}

// AN IDLE NODE IS CAPPED ONCE IT HAS BEEN IDLE FOR idle_after, AND NOT BEFORE,
// through the loop's own ticker.
func TestAnIdleNodeIsCappedAfterIdleAfterAndNotBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 4)
		c, stateDir := newController(t, h, capBoth, nil)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, func() bool { return true })
		}()

		time.Sleep(55 * time.Second)
		synctest.Wait()
		if h.boost() != "1" || h.maxFreq(0) != fullKHz {
			t.Fatalf("capped after 55s of a 60s idle_after: boost %s, ceiling %s", h.boost(), h.maxFreq(0))
		}

		time.Sleep(10 * time.Second)
		synctest.Wait()
		h.requireCapped(stateDir)

		cancel()
		<-done
	})
}

// A NODE THAT HOLDS ANYTHING IS NEVER CAPPED, however long it holds it.
func TestABusyNodeIsNeverCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 2)
		c, stateDir := newController(t, h, capBoth, nil)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, func() bool { return false })
		}()

		time.Sleep(time.Hour)
		synctest.Wait()
		h.requireFull(stateDir)

		cancel()
		<-done
	})
}

// A BURST OF WORK THAT FALLS BETWEEN TWO TICKS STILL RESETS THE IDLE CLOCK: a
// launch marks the node busy itself, so a node that ran a short job is not
// capped a moment after it ends because the loop never saw the job.
func TestALaunchBetweenTicksRestartsTheIdlePeriod(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 1)
		c, stateDir := newController(t, h, capBoth, nil)

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, func() bool { return true })
		}()

		time.Sleep(50 * time.Second)
		c.RestoreBeforeLaunch(t.Context(), "lease-1")
		time.Sleep(50 * time.Second)
		synctest.Wait()
		h.requireFull(stateDir)

		time.Sleep(15 * time.Second)
		synctest.Wait()
		if h.boost() != "0" {
			t.Error("not capped a full idle_after after the launch")
		}

		cancel()
		<-done
	})
}

// A LAUNCH RESTORES THE FULL CEILING BEFORE IT RETURNS, and says how long that
// took.
func TestALaunchRestoresTheFullCeilingBeforeItReturns(t *testing.T) {
	h := newFakeHost(t, 3)
	o := &observed{}
	c, stateDir := newController(t, h, capBoth, o)
	capNow(t, c)
	h.requireCapped(stateDir)

	c.RestoreBeforeLaunch(t.Context(), "lease-1")

	h.requireFull(stateDir)
	if got := o.launchRestores(); len(got) != 1 || got[0] <= 0 {
		t.Errorf("launch restores observed %v, want one with its latency", got)
	}
	if o.count("cap/ok") != 1 || o.count("restore/ok") != 1 {
		t.Errorf("observed %v, want one cap and one restore", o.changes)
	}

	// AND A LAUNCH ON A HOST AT FULL WRITES NOTHING AND RECORDS NO LATENCY.
	c.RestoreBeforeLaunch(t.Context(), "lease-2")
	if got := o.launchRestores(); len(got) != 1 {
		t.Errorf("a launch on a full host recorded a restore: %v", got)
	}
}

// THE RECORD IS DURABLE BEFORE THE FIRST WRITE, and holds what was there.
func TestTheRecordIsInstalledBeforeTheFirstWrite(t *testing.T) {
	h := newFakeHost(t, 2)
	c, stateDir := newController(t, h, capBoth, nil)

	var checked bool
	write := c.write
	c.write = func(path, value string) error {
		if !checked {
			checked = true
			body, err := os.ReadFile(filepath.Join(stateDir, RecordName))
			if err != nil {
				t.Errorf("the first sysfs write came before the record: %v", err)
			} else if !bytes.Contains(body, []byte(fullKHz)) {
				t.Errorf("the record at the first write does not hold the baseline:\n%s", body)
			}
		}

		return write(path, value)
	}

	capNow(t, c)
	if !checked {
		t.Fatal("the cap wrote nothing")
	}
	h.requireCapped(stateDir)
}

// A CAP THAT FAILS PART WAY IS PUT BACK AT ONCE, never left half applied.
func TestACapThatFailsPartWayIsRestoredAtOnce(t *testing.T) {
	h := newFakeHost(t, 3)
	o := &observed{}
	c, stateDir := newController(t, h, capBoth, o)

	failing := h.path(h.policyFile(1, "scaling_max_freq"))
	write := c.write
	c.write = func(path, value string) error {
		if path == failing && value == lowKHz {
			return errors.New("injected: EBUSY")
		}

		return write(path, value)
	}

	c.step(t.Context(), func() bool { return true })
	c.mu.Lock()
	c.busyAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	c.step(t.Context(), func() bool { return true })

	h.requireFull(stateDir)
	if state(c.st.Load()) != stateFull {
		t.Errorf("state %d after a failed cap was restored, want full", c.st.Load())
	}
	if o.count("cap/failed") != 1 || o.count("restore/ok") != 1 {
		t.Errorf("observed %v, want one failed cap and the restore that undid it", o.changes)
	}
}

// A RESTORE THAT FAILS IS RETRIED UNTIL IT HOLDS, and the launch that asked
// for it is not stopped by it.
func TestAFailedRestoreIsRetriedUntilItHolds(t *testing.T) {
	h := newFakeHost(t, 2)
	o := &observed{}
	c, stateDir := newController(t, h, capBoth, o)
	capNow(t, c)

	var broken bool
	var mu sync.Mutex
	write := c.write
	c.write = func(path, value string) error {
		mu.Lock()
		defer mu.Unlock()
		if broken {
			return errors.New("injected: EIO")
		}

		return write(path, value)
	}
	mu.Lock()
	broken = true
	mu.Unlock()

	c.RestoreBeforeLaunch(t.Context(), "lease-1")
	if h.boost() != "0" {
		t.Fatal("the injected failure did not hold, so this test proves nothing")
	}
	if _, err := os.Stat(filepath.Join(stateDir, RecordName)); err != nil {
		t.Fatalf("a failed restore lost the record it must retry from: %v", err)
	}
	if o.count("restore/failed") != 1 {
		t.Errorf("observed %v, want one failed restore", o.changes)
	}

	// THE LOOP RETRIES IT, busy or idle, and caps nothing in the meantime.
	c.step(t.Context(), func() bool { return true })
	if h.boost() != "0" {
		t.Fatal("a retry under the same failure reported success")
	}
	mu.Lock()
	broken = false
	mu.Unlock()
	c.step(t.Context(), func() bool { return true })
	h.requireFull(stateDir)
}

// A CAP AN EARLIER PROCESS LEFT IS RESTORED AT THE NEXT START, by a controller
// that caps nothing itself.
func TestALeftoverCapIsRestoredAtStart(t *testing.T) {
	h := newFakeHost(t, 2)
	first, stateDir := newController(t, h, capBoth, nil)
	capNow(t, first)
	h.requireCapped(stateDir)

	// THE FIRST PROCESS DIED HERE. Its successor has no node.power at all.
	next, err := New(Options{SysfsRoot: h.root, StateDir: stateDir, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := next.Restore(t.Context(), "start"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	h.requireFull(stateDir)
}

// A RECORD THAT CANNOT BE READ IS NOT ABSENCE: nothing is written from it,
// nothing is capped over it, and it stays for an operator.
func TestAnUnreadableRecordIsNotAbsence(t *testing.T) {
	h := newFakeHost(t, 1)
	c, stateDir := newController(t, h, capBoth, nil)
	h.write(h.policyFile(0, "scaling_max_freq"), lowKHz)
	record := filepath.Join(stateDir, RecordName)
	if err := os.WriteFile(record, []byte(`{"version":1,"policies":{"policy0":{"scaling_max_freq":"; reboot"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := c.Restore(t.Context(), "start")
	if err == nil || !strings.Contains(err.Error(), "is not a cpufreq policy's ceiling and switch") {
		t.Fatalf("a record holding something that is not a frequency answered %v, want its refusal", err)
	}

	// AND A CAP FROM FULL REFUSES TO RECORD OVER IT: the state is put at full
	// by hand, as a process that never read the record would have it.
	c.st.Store(int32(stateFull))
	c.mu.Lock()
	c.busyAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	c.step(t.Context(), func() bool { return true })
	if state(c.st.Load()) != stateUnknown {
		t.Errorf("state %d after a cap met a record it could not read, want unknown", c.st.Load())
	}

	if got := h.maxFreq(0); got != lowKHz {
		t.Errorf("scaling_max_freq = %s, want it untouched", got)
	}
	if h.boost() != "1" {
		t.Error("capped over a record nobody could read")
	}
	if _, err := os.Stat(record); err != nil {
		t.Errorf("the unreadable record was removed: %v", err)
	}
}

// A NODE'S STOP RESTORES, and nothing caps after it.
func TestCloseRestoresAndNothingCapsAfter(t *testing.T) {
	h := newFakeHost(t, 2)
	c, stateDir := newController(t, h, capBoth, nil)
	capNow(t, c)

	if err := c.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	h.requireFull(stateDir)

	c.mu.Lock()
	c.busyAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	c.step(t.Context(), func() bool { return true })
	h.requireFull(stateDir)
}

// A LAUNCH WAITS AT MOST ITS BOUND for a restore stuck behind a cap in
// progress, and the restore still lands after the cap.
func TestALaunchDuringACapWaitsItsBoundAndTheRestoreLandsAfter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 2)
		c, stateDir := newController(t, h, capBoth, nil)
		c.step(t.Context(), func() bool { return true })
		c.mu.Lock()
		c.busyAt = time.Now().Add(-time.Hour)
		c.mu.Unlock()

		gate := make(chan struct{})
		entered := make(chan struct{})
		var once sync.Once
		write := c.write
		c.write = func(path, value string) error {
			if value == lowKHz {
				once.Do(func() { close(entered) })
				<-gate
			}

			return write(path, value)
		}

		var busy sync.Mutex
		idle := true
		capping := make(chan struct{})
		go func() {
			defer close(capping)
			c.step(t.Context(), func() bool { busy.Lock(); defer busy.Unlock(); return idle })
		}()
		<-entered

		// THE LAUNCH ARRIVES MID-CAP: its hold is placed, then it asks.
		busy.Lock()
		idle = false
		busy.Unlock()
		started := time.Now()
		c.RestoreBeforeLaunch(t.Context(), "lease-1")
		if waited := time.Since(started); waited != 2*time.Second {
			t.Errorf("the launch waited %s for a restore stuck behind a cap, want its 2s bound", waited)
		}

		close(gate)
		<-capping
		synctest.Wait()
		h.requireFull(stateDir)
	})
}

// THE FULL CEILING IS EVERY POLICY'S, not only the ones the cap wrote: a
// driver that lowers a policy's ceiling when boost goes off has it put back.
func TestARestoreRewritesACeilingTheBoostSwitchMoved(t *testing.T) {
	h := newFakeHost(t, 2)
	c, stateDir := newController(t, h, Settings{IdleAfter: time.Minute, DisableBoost: true}, nil)

	write := c.write
	c.write = func(path, value string) error {
		if err := write(path, value); err != nil {
			return err
		}
		// A STAND-IN DRIVER THAT MOVES EVERY CEILING WITH THE SWITCH, both ways,
		// so a restore that writes the ceilings before the switch loses them.
		if path == h.path("devices/system/cpu/cpufreq/boost") {
			for i := range h.n {
				h.write(h.policyFile(i, "scaling_max_freq"), midKHz)
				h.write(h.policyFile(i, "boost"), value)
			}
		}

		return nil
	}

	capNow(t, c)
	if h.maxFreq(0) != midKHz {
		t.Fatal("the stand-in driver did not move the ceiling, so this test proves nothing")
	}
	c.RestoreBeforeLaunch(t.Context(), "lease-1")
	h.requireFull(stateDir)
}

// A CAP IS TAKEN ONLY FROM CEILINGS AT THEIR FULL. One a firmware limit or an
// operator holds lower reads as the effective limit, and writing it back as
// the user's request would keep that limit after it lifts, so nothing is
// capped, written or recorded until every ceiling is at its full again.
func TestACapIsTakenOnlyFromCeilingsAtTheirFull(t *testing.T) {
	h := newFakeHost(t, 2)
	h.write(h.policyFile(1, "scaling_max_freq"), midKHz) // a _PPC limit in force
	c, stateDir := newController(t, h, Settings{IdleAfter: time.Minute, MaxFreqKHz: 1500000}, nil)

	c.step(t.Context(), func() bool { return true })
	c.mu.Lock()
	c.busyAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()
	c.step(t.Context(), func() bool { return true })

	if got := h.maxFreq(0); got != fullKHz {
		t.Errorf("policy0 = %s, want it untouched while policy1 is held below its full", got)
	}
	if got := h.maxFreq(1); got != midKHz {
		t.Errorf("policy1 = %s, want it untouched", got)
	}
	if _, err := os.Stat(filepath.Join(stateDir, RecordName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a record was written from a ceiling below its full: %v", err)
	}
	if state(c.st.Load()) != stateFull {
		t.Errorf("state %d, want full", c.st.Load())
	}

	// THE LIMIT LIFTS, and the next idle period caps from the full ceiling.
	h.write(h.policyFile(1, "scaling_max_freq"), fullKHz)
	capNow(t, c)
	for i := range h.n {
		if got := h.maxFreq(i); got != lowKHz {
			t.Errorf("%s = %s, want capped to %s", policyName(i), got, lowKHz)
		}
	}
	if err := c.Restore(t.Context(), "stop"); err != nil {
		t.Fatal(err)
	}
	h.requireFull(stateDir)
}

// A CAP NEVER RAISES A CEILING: a setting at the full writes no ceiling.
func TestACapNeverRaisesACeiling(t *testing.T) {
	h := newFakeHost(t, 2)
	c, _ := newController(t, h, Settings{IdleAfter: time.Minute, MaxFreqKHz: 2450000, DisableBoost: true}, nil)

	var ceilings int
	c.write = func(path, value string) error {
		if strings.HasSuffix(path, "scaling_max_freq") {
			ceilings++
		}

		return writeSysfs(path, value)
	}
	capNow(t, c)
	if ceilings != 0 {
		t.Errorf("a ceiling at the setting was written %d times", ceilings)
	}
	if h.boost() != "0" {
		t.Error("boost was not turned off")
	}
}

// THE HOST MUST HONOUR THE SETTINGS EXACTLY, or the node refuses to start.
func TestCheckHostRefusesWhatTheHostCannotHonour(t *testing.T) {
	for _, tc := range []struct {
		name  string
		s     Settings
		patch func(h *fakeHost)
		want  string
	}{
		{"the reference host", capBoth, nil, ""},
		{"a ceiling outside the table", Settings{IdleAfter: time.Minute, MaxFreqKHz: 1800000}, nil,
			"not one of policy0's available frequencies"},
		{"a ceiling below the floor", Settings{IdleAfter: time.Minute, MaxFreqKHz: 1500000},
			func(h *fakeHost) { h.write(h.policyFile(1, "scaling_min_freq"), midKHz) }, "below policy1's scaling_min_freq"},
		{"a driver with no table", Settings{IdleAfter: time.Minute, MaxFreqKHz: 1500000},
			func(h *fakeHost) { h.remove(h.policyFile(0, "scaling_available_frequencies")) },
			"could not be read"},
		{"no boost switch", Settings{IdleAfter: time.Minute, DisableBoost: true},
			func(h *fakeHost) { h.remove("devices/system/cpu/cpufreq/boost") }, "billet writes no other boost switch"},
		{"a boost switch that is not one", Settings{IdleAfter: time.Minute, DisableBoost: true},
			func(h *fakeHost) { h.write("devices/system/cpu/cpufreq/boost", "on") }, "which is not 0 or 1"},
		{"boost alone on the reference host", Settings{IdleAfter: time.Minute, DisableBoost: true}, nil, ""},
		{"a ceiling alone on a host with no boost switch", Settings{IdleAfter: time.Minute, MaxFreqKHz: 1500000},
			func(h *fakeHost) { h.remove("devices/system/cpu/cpufreq/boost") }, ""},
		{"no cpufreq at all", capBoth,
			func(h *fakeHost) { h.remove("devices/system/cpu/cpufreq") }, "cpufreq policies"},
		{"an empty cpufreq directory", capBoth,
			func(h *fakeHost) {
				h.remove("devices/system/cpu/cpufreq")
				h.write("devices/system/cpu/cpufreq/boost", "1")
			}, "holds no cpufreq policy"},
		{"a restore-only controller checks nothing", Settings{},
			func(h *fakeHost) { h.remove("devices/system/cpu/cpufreq") }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFakeHost(t, 2)
			if tc.patch != nil {
				tc.patch(h)
			}
			c, _ := newController(t, h, tc.s, nil)
			err := c.CheckHost()
			switch {
			case tc.want == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Errorf("answered %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}
