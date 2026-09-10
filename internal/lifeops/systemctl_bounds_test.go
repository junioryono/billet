package lifeops

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE RUNNER UNDER A DEADLINE, against real processes: a systemctl that hangs
// ends when the caller's context expires, by TERM and then KILL; a descendant
// holding the output ends the wait at the delay with the answer refused as
// not read whole; output past the bound is an error and never a truncated
// value; a property read runs under the inspector's timeout; a stop and a
// start run under the caller's context and nothing shorter.

// fakeSystemctl writes an executable answering as body says and returns its
// path; every case here runs the real execRunner against it.
func fakeSystemctl(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "systemctl")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
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
	bin := fakeSystemctl(t, "exec sleep 30\n")
	i := NewInspector(WithSystemctl(bin))

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	started := time.Now()

	_, err := i.exec(ctx, []string{"stop", "--", "x.service"})
	if err == nil || !strings.Contains(err.Error(), "the deadline ended it") {
		t.Fatalf("a hanging stop under a one-second deadline: %v", err)
	}

	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the deadline took %s to end the run", took)
	}
}

func TestARunThatIgnoresTermIsKilledAfterTheWaitDelay(t *testing.T) {
	shortWaitDelay(t, 500*time.Millisecond)

	bin := fakeSystemctl(t, "trap '' TERM\nexec sleep 30\n")
	i := NewInspector(WithSystemctl(bin))

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()

	started := time.Now()

	_, err := i.exec(ctx, []string{"stop", "--", "x.service"})
	if err == nil || !strings.Contains(err.Error(), "the deadline ended it") {
		t.Fatalf("a TERM-ignoring stop under a one-second deadline: %v", err)
	}

	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the kill after the wait delay took %s", took)
	}
}

func TestAnOutputHeldOpenByADescendantIsNotReadWhole(t *testing.T) {
	shortWaitDelay(t, 500*time.Millisecond)

	// The background sleep inherits stdout and keeps it open after the script
	// exits; the run ends at the wait delay and the prefix is refused.
	bin := fakeSystemctl(t, "sleep 3 &\necho 'ActiveState=active'\nexit 0\n")
	i := NewInspector(WithSystemctl(bin))

	started := time.Now()

	out, err := i.exec(t.Context(), []string{"show", "--", "x.service"})
	if err == nil || !strings.Contains(err.Error(), "not read whole") {
		t.Fatalf("an output held open past the wait delay: %v (%q)", err, out)
	}

	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the held output took %s to refuse", took)
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
	bin := fakeSystemctl(t, "exec sleep 30\n")
	i := NewInspector(WithSystemctl(bin), WithTimeout(500*time.Millisecond))

	started := time.Now()

	_, err := i.properties(t.Context(), "x.service", "ActiveState")
	if err == nil || !strings.Contains(err.Error(), "the deadline ended it") {
		t.Fatalf("a hanging property read under the inspector's timeout: %v", err)
	}

	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the property read took %s to end", took)
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

func TestASlowStartSucceedsUnderTheUnitsBoundAndFailsUnderAShorterOne(t *testing.T) {
	bin := fakeSystemctl(t, `case "$1" in
  start) sleep 2 ;;
  show) printf 'ActiveState=active\nSubState=running\nResult=success\nMainPID=4242\nNRestarts=0\nExecMainStartTimestampMonotonic=1\n' ;;
esac
exit 0
`)
	c := NewConverger(NewInspector(WithSystemctl(bin)), WithStabilityWait(0))

	short, cancelShort := context.WithTimeout(t.Context(), time.Second)
	defer cancelShort()

	if _, err := c.StartAndProve(short, "x.service"); err == nil || !strings.Contains(err.Error(), "the deadline ended it") {
		t.Fatalf("a two-second start under a one-second bound: %v", err)
	}

	long, cancelLong := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancelLong()

	if _, err := c.StartAndProve(long, "x.service"); err != nil {
		t.Fatalf("a two-second start under a ten-second bound: %v", err)
	}
}
