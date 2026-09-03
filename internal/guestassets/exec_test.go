package guestassets

import (
	"errors"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// These tests write executable scripts and then exec them, in parallel. On
// Linux that combination races: another subtest's fork can capture this test's
// still-open write descriptor between os.WriteFile's open and close, and the
// exec then fails with ETXTBSY even though every writer closed correctly —
// the child holds a duplicate until its own execve. The kernel gives no way to
// close the window from here, so the accepted fix (the one the Go project
// itself uses, golang.org/issue/22315) is to retry the START. Only the start:
// ETXTBSY happens before the process runs, so a retry can never re-run a
// script whose first attempt executed.
const (
	etxtbsyAttempts = 5
	etxtbsyBackoff  = 10 * time.Millisecond
)

// retryETXTBSY runs attempt(), which must build a FRESH command each call — an
// exec.Cmd cannot be reused after Start — and retries only the text-file-busy
// start failure.
func retryETXTBSY[T any](attempt func() (T, error)) (T, error) {
	var (
		out T
		err error
	)

	for range etxtbsyAttempts {
		out, err = attempt()
		if !errors.Is(err, syscall.ETXTBSY) {
			return out, err
		}

		time.Sleep(etxtbsyBackoff)
	}

	return out, err
}

// cloneCmd rebuilds a command for a retry attempt.
func cloneCmd(t *testing.T, cmd *exec.Cmd) *exec.Cmd {
	t.Helper()

	fresh := exec.CommandContext(t.Context(), cmd.Path, cmd.Args[1:]...)
	fresh.Env = cmd.Env
	fresh.Dir = cmd.Dir

	return fresh
}

func combinedOutputRetry(t *testing.T, cmd *exec.Cmd) ([]byte, error) {
	t.Helper()

	first := true

	return retryETXTBSY(func() ([]byte, error) {
		run := cmd
		if !first {
			run = cloneCmd(t, cmd)
		}
		first = false

		return run.CombinedOutput()
	})
}

func outputRetry(t *testing.T, cmd *exec.Cmd) ([]byte, error) {
	t.Helper()

	first := true

	return retryETXTBSY(func() ([]byte, error) {
		run := cmd
		if !first {
			run = cloneCmd(t, cmd)
		}
		first = false

		return run.Output()
	})
}

func runRetry(t *testing.T, cmd *exec.Cmd) error {
	t.Helper()

	first := true
	_, err := retryETXTBSY(func() (struct{}, error) {
		run := cmd
		if !first {
			run = cloneCmd(t, cmd)
		}
		first = false

		return struct{}{}, run.Run()
	})

	return err
}
