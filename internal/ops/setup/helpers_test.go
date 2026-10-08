package setup

import (
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/retirement"
)

// Helpers this package's tests share with cmd/billet's, copied rather than
// imported: a test helper is not part of any package's API.

// forkSafeWriteFile writes an executable a test is about to run.
//
// UNDER syscall.ForkLock, which every os/exec start holds while it creates its
// child: a fork by another parallel test while this file is open for writing
// inherits the descriptor, and exec of the file then fails with ETXTBSY ("text
// file busy") until that child execs (Go issue 22315). A shell that meets it on a
// shebang interpreter reports exit 126 and fails the test for nothing.
func forkSafeWriteFile(name string, data []byte, perm os.FileMode) error {
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()

	return os.WriteFile(name, data, perm)
}

// capture redirects stdout for the duration of fn and returns what was written.
//
// `billet check` REPORTS to an operator, so what it prints is the whole product
// and asserting only its error return would leave the interesting half untested.
func capture(t *testing.T, fn func()) string {
	t.Helper()

	saved := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	os.Stdout = w

	// RESTORED BY CLEANUP, not only by the happy path below. A t.Fatal inside fn
	// unwinds past the restore, leaving every later test in the package writing
	// into a pipe nobody reads — which surfaces as an unrelated test hanging or
	// losing its output, a long way from the test that actually failed.
	t.Cleanup(func() { os.Stdout = saved })

	done := make(chan string, 1)

	go func() {
		var b strings.Builder

		_, _ = io.Copy(&b, r) //nolint:errcheck // the write end is closed below, ending the copy

		done <- b.String()
	}()

	fn()

	os.Stdout = saved

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}

	return <-done
}

// useRetirementRoot points the leaf at a directory the test owns and pins the
// platform seam to Linux, since the exclusion is Linux's.
func useRetirementRoot(t *testing.T) {
	t.Helper()

	oldRoot := retirement.Root
	retirement.Root = t.TempDir()

	oldOS, oldPlatform := cli.HostOS, retirement.Platform
	cli.HostOS, retirement.Platform = "linux", "linux"

	t.Cleanup(func() {
		retirement.Root = oldRoot
		cli.HostOS, retirement.Platform = oldOS, oldPlatform
	})
}
