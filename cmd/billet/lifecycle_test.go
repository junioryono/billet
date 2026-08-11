package main

import (
	"context"
	"os"
	"syscall"
	"testing"
	"time"
)

// waitFor polls a condition rather than sleeping a fixed amount, so the test is
// not a bet on how fast a goroutine gets scheduled.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(time.Millisecond)
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
	case <-time.After(5 * time.Second):
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
