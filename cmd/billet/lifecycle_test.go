package main

import (
	"context"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// waitFor polls a condition rather than sleeping a fixed amount.
//
// The deadline bounds a STALL rather than budgeting the work, so it is far
// larger than any of these waits needs. Five seconds looked ample and was not:
// under the full suite, with -race and -covermode=atomic, a goroutine can go
// unscheduled that long and the test then fails on the wait rather than on
// what it asserts.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
	}
}

// WHAT THE SIGNALS SAY THEY DO IS WHAT THEY DO.
//
// The second signal's message claimed the jobs still running were about to be
// destroyed and their builds failed. That was true while an ended drain reached a
// destructive teardown; it stopped being true, and the message stayed. An
// operator told their jobs are already dead has no reason to be careful with the
// host afterwards, and killing that machine is how compute billet is still
// accounting for disappears outside its accounting.
//
// So the words are pinned. A message this consequential should break a test when
// it stops matching the code rather than be noticed in a review months later.
func TestTheSignalMessagesDescribeWhatTheSignalsActuallyDo(t *testing.T) {
	signals := make(chan os.Signal, 3)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	lc := newLifecycle(cancel)

	exited := make(chan int, 1)

	out := captureStderr(t, func() {
		done := make(chan struct{})

		go func() {
			defer close(done)

			lc.escalate(signals, func(code int) { exited <- code })
		}()

		signals <- syscall.SIGTERM
		waitFor(t, "the first signal to cancel", func() bool { return ctx.Err() != nil })

		signals <- syscall.SIGTERM
		waitFor(t, "the second signal to hurry the drain", func() bool {
			select {
			case <-lc.hurry:
				return true
			default:
				return false
			}
		})

		signals <- syscall.SIGTERM

		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			t.Error("the third signal never exited")
		}

		<-done
	})

	// THE SECOND SIGNAL LEAVES THE WORK ALONE, and says so.
	for _, want := range []string{"no longer waiting", "LEFT RUNNING", "re-adopted"} {
		if !strings.Contains(out, want) {
			t.Errorf("the second signal does not say %q:\n%s", want, out)
		}
	}

	// AND IT NAMES NO PARTICULAR ROLE. `billet server` and `billet node` install
	// this same handler, and what recovers the work differs: a control plane
	// re-adopts leases when IT returns, a node's provider inventory when the NODE
	// does. Naming either sends half the operators to restart something that
	// cannot help, and "re-adopted" alone cannot tell the two apart.
	if !strings.Contains(out, "process responsible for them returns") {
		t.Errorf("the second signal does not say which process recovers the work in a "+
			"way that is true for both roles:\n%s", out)
	}

	// AND CLAIMS NO DESTRUCTION. Asserting only the presence of the new words
	// would pass against a message that said both.
	for _, forbidden := range []string{
		"Destroying them", "will FAIL those builds", "when a control plane returns",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("a signal still claims to destroy running jobs, which none of them "+
				"does: %q\n%s", forbidden, out)
		}
	}
}

// THREE LEVELS, BECAUSE A DRAIN CAN LAST AS LONG AS A JOB.
//
// Before the drain existed two were enough: the first signal tore everything
// down, the second gave up on that. Now the first signal may wait hours for the
// jobs already running, so an operator who wants to stop sooner needs a way to
// say "do not wait" that is not also "abandon the containers". That is the
// second signal. The third is the old escape hatch, unchanged.
func TestTheSecondSignalSkipsTheDrainAndTheThirdGivesUp(t *testing.T) {
	signals := make(chan os.Signal, 3)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	lc := newLifecycle(cancel)

	exited := make(chan int, 1)

	done := make(chan struct{})

	go func() {
		defer close(done)

		lc.escalate(signals, func(code int) { exited <- code })
	}()

	// FIRST: drain. The context is cancelled, which is what starts it, and
	// nothing has been hurried or exited.
	signals <- syscall.SIGTERM

	waitFor(t, "the first signal to cancel", func() bool { return ctx.Err() != nil })

	select {
	case <-lc.hurry:
		t.Fatal("the first signal skipped the drain")
	case code := <-exited:
		t.Fatalf("the first signal exited with %d", code)
	default:
	}

	// SECOND: stop waiting, but still tear down properly.
	signals <- syscall.SIGTERM

	waitFor(t, "the second signal to hurry the drain", func() bool {
		select {
		case <-lc.hurry:
			return true
		default:
			return false
		}
	})

	select {
	case code := <-exited:
		t.Fatalf("the second signal exited with %d instead of hurrying the teardown", code)
	default:
	}

	// THIRD: give up.
	signals <- syscall.SIGTERM

	select {
	case code := <-exited:
		if code != 130 {
			t.Errorf("exit code = %d, want 130", code)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the third signal did not exit")
	}

	<-done
}

// Hurrying twice must not panic. An operator leaning on Ctrl-C sends far more
// than three, and a second close of the same channel is a panic that would take
// the process down in the middle of a teardown — the one moment it must not.
func TestHurryingIsIdempotent(t *testing.T) {
	lc := newLifecycle(func() {})

	lc.rush()
	lc.rush()
	lc.rush()

	select {
	case <-lc.hurry:
	default:
		t.Fatal("rush did not signal")
	}
}
