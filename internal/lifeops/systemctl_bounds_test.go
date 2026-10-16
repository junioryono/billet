package lifeops

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// THE RUNNER UNDER A DEADLINE, against real processes: a systemctl that hangs
// ends when the caller's context expires, by TERM and then KILL; a descendant
// holding the output ends the wait at the delay with the answer refused as
// not read whole; output past the bound is an error and never a truncated
// value; a property read runs under the inspector's timeout; a stop and a
// start run under the caller's context and nothing shorter.

// runGuard is how long a test waits for a run whose bound under test is a second
// or so. It is a failure deadline, never a condition: the stand-ins here sleep
// for ten minutes, so a bounded run returns long before it however loaded the
// machine, and an unbounded one is still running at it. A correct run took 5 s
// to 12 s under a full local gate (2026-10-04 to 10-10, #188), almost all of it
// starting processes, so a bound of a few seconds measured the machine.
const runGuard = 2 * time.Minute

// within runs call and fails the test if it has not returned by guard.
func within[T any](t *testing.T, guard time.Duration, what string, call func() T) T {
	t.Helper()

	done := make(chan T, 1)
	go func() { done <- call() }()

	select {
	case v := <-done:
		return v
	case <-time.After(guard):
		t.Fatalf("%s had not returned after %s; nothing bounded it", what, guard)

		var zero T

		return zero
	}
}

// ranOut is what a run returned.
type ranOut struct {
	out []byte
	err error
}

// runningStandIn is a stand-in systemctl body that records its pid at path once
// whatever comes before it in the script has run, and then becomes body's last
// command; the pid is the one the run's TERM and KILL reach, since exec keeps it.
func runningStandIn(path, before, last string) string {
	return before + "printf '%s\\n' $$ > '" + path + ".tmp' && mv '" + path + ".tmp' '" + path + "'\n" + last
}

// cancelOnceRunning ends ctx once the stand-in has written its pid to path, so
// the run's cancellation reaches a process that is running and has set itself up,
// and never one still starting: a deadline that expired before the start was a
// refusal no TERM or KILL was needed for. The stand-in is killed when the test
// ends, whatever the run did with it.
func cancelOnceRunning(t *testing.T, path string) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	t.Cleanup(func() {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					t.Errorf("end the stand-in %d: %v", pid, err)
				}
			}
		}
	})

	go func() {
		for {
			if _, err := os.Stat(path); err == nil {
				cancel()

				return
			}

			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Millisecond):
			}
		}
	}()

	return ctx
}

// fakeSystemctl writes an executable answering as body says and returns its
// path; every case here runs the real execRunner against it.
func fakeSystemctl(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "systemctl")
	if err := forkSafeWriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}

func shortWaitDelay(t *testing.T, d time.Duration) {
	t.Helper()

	prev := runnerWaitDelay
	runnerWaitDelay = d

	t.Cleanup(func() { runnerWaitDelay = prev })
}

func TestARunEndsAtTheCallersDeadlineByTerm(t *testing.T) {
	// THE WAIT DELAY IS AS LONG AS THE SLEEP, so only the TERM can end the run
	// before the guard: a cancel that sent nothing would wait for the KILL. The
	// caller's context ends once the stand-in is running.
	pid := filepath.Join(t.TempDir(), "pid")
	bin := fakeSystemctl(t, runningStandIn(pid, "", "exec sleep 600\n"))
	i := NewInspector(WithSystemctl(bin), WithWaitDelay(10*time.Minute))
	ctx := cancelOnceRunning(t, pid)

	ran := within(t, runGuard, "a hanging stop whose caller's context ended", func() ranOut {
		out, err := i.exec(ctx, []string{"stop", "--", "x.service"})

		return ranOut{out, err}
	})
	if ran.err == nil || !strings.Contains(ran.err.Error(), "the deadline ended it") {
		t.Fatalf("a hanging stop whose caller's context ended: %v", ran.err)
	}
}

func TestARunThatIgnoresTermIsKilledAfterTheWaitDelay(t *testing.T) {
	shortWaitDelay(t, 500*time.Millisecond)

	// THE TRAP IS SET BEFORE THE PID IS WRITTEN, so the TERM always meets a
	// process that ignores it and only the KILL after the wait delay ends it.
	pid := filepath.Join(t.TempDir(), "pid")
	bin := fakeSystemctl(t, runningStandIn(pid, "trap '' TERM\n", "exec sleep 600\n"))
	i := NewInspector(WithSystemctl(bin))
	ctx := cancelOnceRunning(t, pid)

	ran := within(t, runGuard, "a TERM-ignoring stop whose caller's context ended", func() ranOut {
		out, err := i.exec(ctx, []string{"stop", "--", "x.service"})

		return ranOut{out, err}
	})
	if ran.err == nil || !strings.Contains(ran.err.Error(), "the deadline ended it") {
		t.Fatalf("a TERM-ignoring stop whose caller's context ended: %v", ran.err)
	}
}

func TestAnOutputHeldOpenByADescendantIsNotReadWhole(t *testing.T) {
	shortWaitDelay(t, 500*time.Millisecond)

	// The background sleep inherits stdout and keeps it open after the script
	// exits; the run ends at the wait delay and the prefix is refused.
	//
	// THE SLEEP OUTLASTS THE GUARD, so a run that returned only once the
	// descendant let go is still waiting when the guard ends the test. A run
	// that ends at the wait delay took about 0.75s alone and 5.3s to 6.1s under
	// a full `make check` (2026-10-04, the process start being the slow part),
	// which a 5s bound could not tell from the regression (#188). The sleep is
	// ended by the test, never left to outlive it.
	pidFile := filepath.Join(t.TempDir(), "descendant")
	bin := fakeSystemctl(t, "sleep 600 &\necho $! > '"+pidFile+"'\necho 'ActiveState=active'\nexit 0\n")
	i := NewInspector(WithSystemctl(bin))

	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 1 {
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					t.Errorf("end the descendant %d: %v", pid, err)
				}
			}
		}
	})

	ran := within(t, runGuard, "a run whose output a descendant holds open", func() ranOut {
		out, err := i.exec(t.Context(), []string{"show", "--", "x.service"})

		return ranOut{out, err}
	})
	if ran.err == nil || !strings.Contains(ran.err.Error(), "not read whole") || ran.out != nil {
		t.Fatalf("an output held open past the wait delay: %v (%q)", ran.err, ran.out)
	}
}

func TestAnOutputPastTheBoundIsAnErrorNeverATruncatedValue(t *testing.T) {
	bin := fakeSystemctl(t, "head -c 1100000 /dev/zero | tr '\\0' a\n")
	i := NewInspector(WithSystemctl(bin))

	out, err := i.exec(t.Context(), []string{"show", "--", "x.service"})
	if err == nil || !strings.Contains(err.Error(), "not read whole") || out != nil {
		t.Fatalf("an overflowing output: %v (%d bytes returned)", err, len(out))
	}

	whole := fakeSystemctl(t, "head -c 100000 /dev/zero | tr '\\0' a\n")
	i = NewInspector(WithSystemctl(whole))

	out, err = i.exec(t.Context(), []string{"show", "--", "x.service"})
	if err != nil || len(out) != 100000 {
		t.Fatalf("an output under the bound: %v (%d bytes)", err, len(out))
	}
}

func TestAPropertyReadRunsUnderTheInspectorsTimeout(t *testing.T) {
	bin := fakeSystemctl(t, "exec sleep 600\n")
	i := NewInspector(WithSystemctl(bin), WithTimeout(500*time.Millisecond))

	// BELOW DefaultTimeout, so a read bounded by the default rather than the
	// inspector's own timeout is caught too; the deadline itself is compared
	// exactly in TestAStopAndAStartRunUnderTheCallersContextAlone.
	err := within(t, DefaultTimeout/2, "a hanging property read", func() error {
		_, err := i.properties(t.Context(), "x.service", "ActiveState")

		return err
	})
	if err == nil || !strings.Contains(err.Error(), "the deadline ended it") {
		t.Fatalf("a hanging property read under the inspector's timeout: %v", err)
	}
}

// deadlineRecorder is a runner that records the deadline each invocation ran
// under, keyed by its first argument, and answers a healthy running unit.
type deadlineRecorder struct {
	deadlines map[string]time.Time
	unbounded map[string]bool
}

func (r *deadlineRecorder) run(ctx context.Context, _ string, args []string) ([]byte, error) {
	if d, ok := ctx.Deadline(); ok {
		r.deadlines[args[0]] = d
	} else {
		r.unbounded[args[0]] = true
	}

	if args[0] == "show" {
		return []byte("ActiveState=active\nSubState=running\nResult=success\nMainPID=4242\nNRestarts=0\n" +
			"ExecMainStartTimestampMonotonic=1\n"), nil
	}

	return nil, nil
}

func TestAStopAndAStartRunUnderTheCallersContextAlone(t *testing.T) {
	for _, op := range []string{"stop", "start"} {
		t.Run(op, func(t *testing.T) {
			rec := &deadlineRecorder{deadlines: map[string]time.Time{}, unbounded: map[string]bool{}}
			c := NewConverger(NewInspector(withRunner(rec.run), WithTimeout(time.Second)), WithStabilityWait(0))

			// A far deadline of the caller's: the stop or start carries exactly
			// it, and the property read after it carries the inspector's, which
			// is nearer.
			far := time.Now().Add(time.Hour)
			ctx, cancel := context.WithDeadline(t.Context(), far)

			var err error
			if op == "stop" {
				_, err = c.StopAndProve(ctx, "x.service")
			} else {
				_, err = c.StartAndProve(ctx, "x.service")
			}

			cancel()

			if op == "start" && err != nil {
				t.Fatalf("the start: %v", err)
			}

			if got := rec.deadlines[op]; !got.Equal(far) {
				t.Errorf("the %s ran under the deadline %s, want the caller's %s", op, got, far)
			}

			if got := rec.deadlines["show"]; got.After(time.Now().Add(2*time.Second)) || got.IsZero() {
				t.Errorf("the property read ran under %s, want the inspector's timeout", got)
			}

			// No deadline from the caller: the stop or start has none.
			rec = &deadlineRecorder{deadlines: map[string]time.Time{}, unbounded: map[string]bool{}}
			c = NewConverger(NewInspector(withRunner(rec.run), WithTimeout(time.Second)), WithStabilityWait(0))

			// The answer is not the point; the deadline the runner saw is.
			var runErr error

			if op == "stop" {
				_, runErr = c.StopAndProve(t.Context(), "x.service")
			} else {
				_, runErr = c.StartAndProve(t.Context(), "x.service")
			}

			if runErr != nil {
				t.Logf("%s: %v", op, runErr)
			}

			if !rec.unbounded[op] {
				t.Errorf("the %s under a background context ran under %s; it carries no deadline of its own",
					op, rec.deadlines[op])
			}
		})
	}
}

// THE TWO BOUNDS ARE FAR APART AND THE TWO STARTS TOO, so how fast the machine
// starts a process decides neither answer: a start that never finishes under a
// one-second bound, and a two-second start under a ten-minute one. A
// two-second start against a ten-second bound took 12.68s on a loaded machine
// (2026-10-10, #188).
func TestASlowStartSucceedsUnderTheUnitsBoundAndFailsUnderAShorterOne(t *testing.T) {
	const script = `case "$1" in
  start) exec sleep %s ;;
  show) printf 'ActiveState=active\nSubState=running\nResult=success\nMainPID=4242\nNRestarts=0\nExecMainStartTimestampMonotonic=1\n' ;;
esac
exit 0
`

	hung := NewConverger(NewInspector(WithSystemctl(fakeSystemctl(t, strings.Replace(script, "%s", "600", 1)))),
		WithStabilityWait(0))

	short, cancelShort := context.WithTimeout(t.Context(), time.Second)
	defer cancelShort()

	if err := within(t, runGuard, "a start under a one-second bound", func() error {
		_, err := hung.StartAndProve(short, "x.service")

		return err
	}); err == nil || !strings.Contains(err.Error(), "the deadline ended it") {
		t.Fatalf("a start that never finishes under a one-second bound: %v", err)
	}

	slow := NewConverger(NewInspector(WithSystemctl(fakeSystemctl(t, strings.Replace(script, "%s", "2", 1)))),
		WithStabilityWait(0))

	long, cancelLong := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancelLong()

	if _, err := slow.StartAndProve(long, "x.service"); err != nil {
		t.Fatalf("a two-second start under a ten-minute bound: %v", err)
	}
}
