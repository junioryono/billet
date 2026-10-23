package power

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ONE CONTROLLER HOLDS A HOST'S CPUFREQ AT A TIME, across processes and
// deployments, and one that has not claimed it writes nothing.
func TestOneControllerHoldsAHostsCPUFreqAtATime(t *testing.T) {
	h := newFakeHost(t, 2)
	first, _ := newController(t, h, capBoth, nil)

	second, err := New(Options{SysfsRoot: h.root, StateDir: t.TempDir(), Settings: capBoth,
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if err := second.ClaimHost(); !errors.Is(err, ErrHostHeld) {
		t.Fatalf("a second controller's claim: %v, want ErrHostHeld", err)
	}

	// UNCLAIMED, IT CAPS NOTHING, however idle the node is.
	stateDir := second.stateDir
	second.step(t.Context(), func() bool { return true })
	second.mu.Lock()
	second.busyAt = second.busyAt.Add(-2 * capBoth.IdleAfter)
	second.mu.Unlock()
	second.step(t.Context(), func() bool { return true })
	if state(second.st.Load()) == stateCapped {
		t.Fatal("a controller that has not claimed the host capped it")
	}
	h.requireFull(stateDir)

	first.ReleaseHost()
	if err := second.ClaimHost(); err != nil {
		t.Fatalf("the claim once the first released it: %v", err)
	}
	second.ReleaseHost()
}

// NOTHING IS CAPPED FROM BOOST ALREADY OFF, globally or on one policy: that is
// another cap or an operator's setting, and recording it would keep it.
func TestNothingIsCappedFromBoostAlreadyOff(t *testing.T) {
	for name, off := range map[string]string{
		"global": "devices/system/cpu/cpufreq/boost",
		"policy": filepath.Join(cpufreqRel, policyName(1), "boost"),
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t, 2)
			h.write(off, "0\n")
			c, stateDir := newController(t, h, capBoth, nil)

			c.step(t.Context(), func() bool { return true })
			c.mu.Lock()
			c.busyAt = c.busyAt.Add(-2 * capBoth.IdleAfter)
			c.mu.Unlock()
			c.step(t.Context(), func() bool { return true })

			if state(c.st.Load()) == stateCapped {
				t.Fatal("capped a host whose boost was already off")
			}
			if _, err := os.Stat(filepath.Join(stateDir, RecordName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("a record was installed: %v", err)
			}
			for i := range 2 {
				if got := h.maxFreq(i); got != fullKHz {
					t.Fatalf("policy%d's ceiling is %s, want it left at %s", i, got, fullKHz)
				}
			}
		})
	}
}

// A CAP WHOSE RECORD VANISHES OR ROTS WHILE IT IS IN FORCE IS STILL RESTORED,
// from what the process that made it holds, rather than called full.
func TestACapWhoseRecordIsLostIsRestoredFromWhatTheProcessHolds(t *testing.T) {
	for name, lose := range map[string]func(path string) error{
		"removed": os.Remove,
		"garbled": func(path string) error { return os.WriteFile(path, []byte("{not a record"), 0o600) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newFakeHost(t, 2)
			c, stateDir := newController(t, h, capBoth, nil)
			capNow(t, c)
			h.requireCapped(stateDir)

			if err := lose(filepath.Join(stateDir, RecordName)); err != nil {
				t.Fatal(err)
			}
			if err := c.Restore(t.Context(), "launch"); err != nil {
				t.Fatalf("Restore: %v", err)
			}
			if h.boost() != "1" {
				t.Fatalf("boost reads %q after the restore, want 1", h.boost())
			}
			for i := range 2 {
				if got := h.maxFreq(i); got != fullKHz {
					t.Fatalf("policy%d's ceiling is %s after the restore, want %s", i, got, fullKHz)
				}
			}
			if _, err := os.Stat(filepath.Join(stateDir, RecordName)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the record outlived the restore: %v", err)
			}
		})
	}
}

// A NODE'S CLAIM NAMES THE DIRECTORY THAT IS LOCKED, so an operator can find
// the other holder.
func TestARefusedClaimSaysWhatIsLocked(t *testing.T) {
	h := newFakeHost(t, 1)
	newController(t, h, capBoth, nil)

	other, err := New(Options{SysfsRoot: h.root, StateDir: t.TempDir(), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	err = other.ClaimHost()
	if err == nil || !strings.Contains(err.Error(), filepath.Join(h.root, cpufreqRel)) {
		t.Fatalf("the refused claim says %v; want it to name %s", err, filepath.Join(h.root, cpufreqRel))
	}
}
