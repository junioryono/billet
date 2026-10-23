package power

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// A RECORD THAT NAMES NO POLICY, OR CARRIES MORE THAN ONE RECORD, IS NOT ONE A
// CAP WROTE: restoring "nothing" from it would call a capped host full.
func TestAnIncompleteRecordIsNotARestore(t *testing.T) {
	for _, body := range []string{
		`{"version":1}`,
		`{"version":1,"policies":{}}`,
		`{"version":1,"policies":{"policy0":{"scaling_max_freq":"2450000"}}} {"version":1}`,
	} {
		t.Run(body, func(t *testing.T) {
			h := newFakeHost(t, 1)
			c, stateDir := newController(t, h, capBoth, nil)
			h.write(h.policyFile(0, "scaling_max_freq"), lowKHz)
			record := filepath.Join(stateDir, RecordName)
			if err := os.WriteFile(record, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}

			err := c.Restore(t.Context(), "start")
			if err == nil || (!strings.Contains(err.Error(), "records no cpufreq policy") &&
				!strings.Contains(err.Error(), "more than the one record")) {
				t.Fatalf("an incomplete record answered %v, want a refusal naming what is missing", err)
			}
			if state(c.st.Load()) != stateUnknown {
				t.Errorf("state %d after an unreadable record, want unknown", c.st.Load())
			}
			if _, err := os.Stat(record); err != nil {
				t.Errorf("the incomplete record was removed: %v", err)
			}
			if got := h.maxFreq(0); got != lowKHz {
				t.Errorf("scaling_max_freq = %s, want it untouched", got)
			}
		})
	}
}

// lateWrites applies every ceiling written a moment after the write returns, as
// a kernel does when the store only updates a QoS request and schedules the
// policy's update.
func lateWrites(h *fakeHost, c *Controller, after time.Duration) {
	write := writeSysfs
	c.write = func(path, value string) error {
		if strings.HasSuffix(path, "scaling_max_freq") {
			time.AfterFunc(after, func() {
				if err := write(path, value); err != nil {
					h.t.Errorf("the late write failed: %v", err)
				}
			})

			return nil
		}

		return write(path, value)
	}
}

// A CEILING THAT READS BACK LATE IS WAITED FOR, not taken for a failed write;
// one that never lands is still a failure once the settle bound is spent.
func TestACeilingThatAppliesLateIsWaitedFor(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 2)
		o := &observed{}
		c, stateDir := newController(t, h, capBoth, o)
		lateWrites(h, c, 50*time.Millisecond)

		capNow(t, c)
		h.requireCapped(stateDir)

		c.RestoreBeforeLaunch(t.Context(), "lease-1")
		synctest.Wait()
		h.requireFull(stateDir)
		if o.count("cap/failed") != 0 || o.count("restore/failed") != 0 {
			t.Errorf("a late but correct write was counted as a failure: %v", o.changes)
		}

		capNow(t, c)
		c.write = func(path, value string) error {
			if strings.HasSuffix(path, "scaling_max_freq") {
				return nil // accepted, and never applied
			}

			return writeSysfs(path, value)
		}
		c.RestoreBeforeLaunch(t.Context(), "lease-2")
		synctest.Wait()
		if o.count("restore/failed") != 1 {
			t.Errorf("a ceiling that never read back was not a failed restore: %v", o.changes)
		}
		if _, err := os.Stat(filepath.Join(stateDir, RecordName)); err != nil {
			t.Errorf("the record went with a restore that did not hold: %v", err)
		}
	})
}

// A RECORD WHOSE BASELINE HAD BOOST OFF RESTORES WITHOUT WRITING A POLICY'S OWN
// SWITCH, which the kernel refuses while the global one is off. No cap is taken
// from boost off any more, but a record an earlier release wrote can hold one.
func TestABaselineWithBoostOffRestores(t *testing.T) {
	h := newFakeHost(t, 2)
	h.write(filepath.Join(cpufreqRel, "boost"), "0")
	rec := record{Version: recordVersion, Boost: "0", Policies: map[string]policyRecord{}}
	for i := range h.n {
		h.write(h.policyFile(i, "boost"), "0")
		h.write(h.policyFile(i, "scaling_max_freq"), lowKHz)
		rec.Policies[policyName(i)] = policyRecord{MaxFreq: fullKHz, Boost: "0"}
	}
	c, stateDir := newController(t, h, capBoth, nil)
	if err := c.installRecord(rec); err != nil {
		t.Fatal(err)
	}

	c.write = func(path, value string) error {
		if filepath.Base(path) == "boost" && filepath.Base(filepath.Dir(path)) != "cpufreq" && h.boost() == "0" {
			return errors.New("injected: EINVAL, the global switch is off")
		}

		return writeSysfs(path, value)
	}

	if err := c.Restore(t.Context(), "stop"); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for i := range h.n {
		if got := h.maxFreq(i); got != fullKHz {
			t.Errorf("%s scaling_max_freq = %s, want %s", policyName(i), got, fullKHz)
		}
	}
	if h.boost() != "0" {
		t.Error("the restore turned on a boost switch that was off before the cap")
	}
	if _, err := os.Stat(filepath.Join(stateDir, RecordName)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the record is still there: %v", err)
	}
}

// A LOOP WAITING BEHIND A RESTORE THAT NEVER FINISHES STILL RETURNS when its
// node stops.
func TestALoopWaitingOnAStuckRestoreReturnsOnItsStop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 1)
		c, _ := newController(t, h, capBoth, nil)
		c.sem <- struct{}{} // held by a write the kernel never finishes

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			defer close(done)
			c.Run(ctx, func() bool { return true })
		}()
		time.Sleep(time.Minute)
		synctest.Wait()

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("the loop is still waiting for the controller after its context ended")
		}
		c.release()
		<-done
	})
}

// THE SECOND QUESTION IS ASKED: a node that becomes busy between the two is
// not capped, and nothing is written or recorded.
func TestANodeBusyAtTheSecondQuestionIsNotCapped(t *testing.T) {
	h := newFakeHost(t, 2)
	c, stateDir := newController(t, h, capBoth, nil)
	c.step(t.Context(), func() bool { return true })
	c.mu.Lock()
	c.busyAt = time.Now().Add(-time.Hour)
	c.mu.Unlock()

	asked := 0
	c.step(t.Context(), func() bool { asked++; return asked == 1 })

	if asked != 2 {
		t.Fatalf("the idle question was asked %d times, want twice", asked)
	}
	h.requireFull(stateDir)
	if state(c.st.Load()) != stateFull {
		t.Errorf("state %d, want full", c.st.Load())
	}
}

// A LAUNCH THAT ARRIVES AS THE CAP ASKS ITS SECOND QUESTION waits for the cap
// it cannot see yet: the state reads unknown before that question, so the
// launch takes the slow path and restores after it.
func TestALaunchAtTheSecondQuestionWaitsForTheCap(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 1)
		c, stateDir := newController(t, h, capBoth, nil)
		c.step(t.Context(), func() bool { return true })
		c.mu.Lock()
		c.busyAt = time.Now().Add(-time.Hour)
		c.mu.Unlock()

		// THE LAUNCH ASKS WHILE THE SECOND QUESTION IS BEING ANSWERED, and its
		// hold is not seen: it is placed too late for this cap, which is the
		// latest a cap can still begin. Its restore must wait for the cap and
		// land after it, not read the state as full and return.
		asked := 0
		c.step(t.Context(), func() bool {
			asked++
			if asked == 2 {
				c.RestoreBeforeLaunch(t.Context(), "lease-1")
			}

			return true
		})
		synctest.Wait()

		if asked != 2 {
			t.Fatalf("the idle question was asked %d times, want twice", asked)
		}
		h.requireFull(stateDir)
	})
}

// A LAUNCH ON A FULL HOST TAKES NO LOCK and waits for nothing: a cap in
// progress elsewhere never delays it.
func TestALaunchOnAFullHostWaitsForNothing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 1)
		c, _ := newController(t, h, capBoth, nil)
		c.step(t.Context(), func() bool { return false })
		if state(c.st.Load()) != stateFull {
			t.Fatalf("state %d, want full", c.st.Load())
		}

		c.sem <- struct{}{}
		started := time.Now()
		c.RestoreBeforeLaunch(t.Context(), "lease-1")
		if waited := time.Since(started); waited != 0 {
			t.Errorf("a launch on a full host waited %s", waited)
		}
		c.release()
	})
}

// CLOSE RETRIES A RESTORE THAT FAILS until it holds, and gives up with the
// record's name after its bound.
func TestCloseRetriesWithinItsBound(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 1)
		c, stateDir := newController(t, h, capBoth, nil)
		capNow(t, c)

		failures := 2
		c.write = func(path, value string) error {
			if failures > 0 {
				failures--

				return errors.New("injected: EBUSY")
			}

			return writeSysfs(path, value)
		}
		if err := c.Close(t.Context()); err != nil {
			t.Fatalf("Close: %v", err)
		}
		h.requireFull(stateDir)
	})

	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 1)
		c, _ := newController(t, h, capBoth, nil)
		capNow(t, c)

		c.write = func(string, string) error { return errors.New("injected: EIO") }
		started := time.Now()
		err := c.Close(t.Context())
		if err == nil || !strings.Contains(err.Error(), "the next node process restores it from") {
			t.Fatalf("Close answered %v, want the refusal naming the record", err)
		}
		if took := time.Since(started); took < DefaultStopBound || took > DefaultStopBound+2*time.Second {
			t.Errorf("Close gave up after %s, want its %s bound", took, DefaultStopBound)
		}

		// THE ATTEMPT IN FLIGHT AT THE BOUND ends within its settle bound.
		time.Sleep(2 * DefaultSettle)
		synctest.Wait()
	})
}

// stuckWrites holds every write until the returned release is called, as a
// kernel that never finishes one would.
func stuckWrites(c *Controller) (release func()) {
	gate := make(chan struct{})
	c.write = func(path, value string) error {
		<-gate

		return writeSysfs(path, value)
	}

	return func() { close(gate) }
}

// CLOSE IS BOUNDED BY ITS STOP BOUND even when the restore it starts is stuck
// inside a write, and the restore still lands once the write returns.
func TestCloseIsBoundedByAStuckRestore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 1)
		c, stateDir := newController(t, h, capBoth, nil)
		capNow(t, c)

		release := stuckWrites(c)
		started := time.Now()
		err := c.Close(t.Context())
		if err == nil || !strings.Contains(err.Error(), "did not finish within the stop's bound") {
			t.Fatalf("Close answered %v, want the bound's refusal", err)
		}
		if took := time.Since(started); took < DefaultStopBound || took > DefaultStopBound+2*time.Second {
			t.Errorf("Close returned after %s, want its %s bound", took, DefaultStopBound)
		}

		release()
		synctest.Wait()
		h.requireFull(stateDir)
	})
}

// LAUNCHES BEHIND A STUCK RESTORE SHARE IT: each returns after its bound, and
// none leaves a goroutine of its own behind.
func TestLaunchesBehindAStuckRestoreShareIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newFakeHost(t, 1)
		c, stateDir := newController(t, h, capBoth, nil)
		capNow(t, c)

		inflight := func() *restoreRun {
			c.mu.Lock()
			defer c.mu.Unlock()

			return c.inflight
		}

		release := stuckWrites(c)
		c.RestoreBeforeLaunch(t.Context(), "lease-0")
		first := inflight()
		if first == nil {
			t.Fatal("no restore is in flight behind the stuck write, so this test proves nothing")
		}
		for i := range 5 {
			started := time.Now()
			c.RestoreBeforeLaunch(t.Context(), "lease-"+itoa(i+1))
			if waited := time.Since(started); waited != 2*time.Second {
				t.Errorf("launch %d waited %s, want its 2s bound", i+1, waited)
			}
			if run := inflight(); run != first {
				t.Errorf("launch %d started a restore of its own behind the stuck one", i+1)
			}
		}

		release()
		synctest.Wait()
		h.requireFull(stateDir)
	})
}
