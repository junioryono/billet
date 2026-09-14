package guestassets

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// writeExecutable is os.WriteFile for a file some test will exec, with the
// write held under syscall.ForkLock.
//
// THE RETRY BELOW COVERS ONLY A START GO MAKES. A script that execs another
// file this package wrote (the docker shim execs the fake docker behind it)
// fails inside the shell, where nothing can retry it: CI run 34831856345
// (2026-09-14) failed `exec: .../behind/docker: Text file busy`. Every fork in
// this process takes ForkLock, so no child can be created while a write
// descriptor is open, and no child can hold a copy of one when the file is
// later executed.
func writeExecutable(name string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()

	return os.WriteFile(name, data, perm)
}

// retryETXTBSY runs attempt(), which must build a FRESH command each call — an
// exec.Cmd cannot be reused after Start — and retries only the text-file-busy
// start failure.
//
// ONLY THE SYSCALL, NEVER A STATUS. A shell that could not exec its child
// answers 126, and CI has produced exactly that (`hosted result 103 became
// exit status 126`, 2026-09-12) — but an exit status means the process RAN,
// and retrying it would re-run whatever it did before it failed, which is the
// one thing this retry may not do. The nested race is removed at its source
// instead: see sharedListener.
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

// sharedListener is the fake `Runner.Listener` every runner-service fixture
// runs, written ONCE by TestMain and reached by SYMLINK from each fixture's
// tree.
//
// WHY ONCE, AND WHY A LINK. The ETXTBSY window above is opened by WRITING an
// executable while other tests fork; a wrapper that then execs such a file
// meets the same race one level down, where the shell reports it as exit
// status 126 and the start-only retry cannot see it (CI, 2026-09-12). Writing
// the listener before any test has started leaves no concurrent fork to
// capture its descriptor, and a symlink creates no descriptor at all, so the
// race is gone rather than retried. The exit code comes from the environment,
// which is what makes one file serve every case.
var sharedListener string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "billet-guestassets-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "make the shared listener's directory:", err)
		os.Exit(1)
	}

	sharedListener = filepath.Join(dir, "Runner.Listener")

	if err := writeExecutable(sharedListener, []byte("#!/bin/sh\nexit \"${BILLET_TEST_RESULT:-7}\"\n"), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "write the shared listener:", err)
		os.Exit(1)
	}

	code := m.Run()

	_ = os.RemoveAll(dir)

	os.Exit(code)
}

// linkListener puts the shared listener where a runner tree expects it.
func linkListener(t *testing.T, root string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatalf("make runner bin: %v", err)
	}

	if err := os.Symlink(sharedListener, filepath.Join(root, "bin", "Runner.Listener")); err != nil {
		t.Fatalf("link the shared listener: %v", err)
	}
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
