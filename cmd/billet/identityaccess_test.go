package main

import (
	"errors"
	"os"
	"strings"
	"syscall"
	"testing"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/retirement"
)

// THE EXCLUSION BEFORE THE FIRST IDENTITY ACCESS, PROVED WHERE A NON-ROOT TEST
// CAN PROVE IT. The ownership hand-backs and the package's bootstrap need root
// and a real service account; the lifecycle job's container runs them. What
// this file holds to is the classification, the locks each mode takes and in
// which order, and the refusals: an absent directory a command may not create,
// a closed authority, metadata that contradicts itself.

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

// locked reports whether path's flock is held by somebody else, by trying a
// non-blocking take on a fresh descriptor and releasing it at once.
func locked(t *testing.T, path string) bool {
	t.Helper()

	f, err := os.OpenFile(path, os.O_RDONLY, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false
		}

		t.Fatal(err)
	}

	defer func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	}()

	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}

		return false
	}

	return true
}

func TestLocalPrepareRefusesTheCallersItIsNotFor(t *testing.T) {
	old := cli.HostOS
	t.Cleanup(func() { cli.HostOS = old })

	cli.HostOS = "darwin"

	if err := cmdLocalPrepare(t.Context(), processEnv(), []string{"--json"}); err == nil ||
		!strings.Contains(err.Error(), "Linux") {
		t.Fatalf("darwin must refuse naming the platform, got %v", err)
	}

	if os.Geteuid() == 0 {
		t.Skip("the root refusal is for a caller that is not root")
	}

	cli.HostOS = "linux"

	if err := cmdLocalPrepare(t.Context(), processEnv(), []string{"--json"}); err == nil ||
		!strings.Contains(err.Error(), "only root") {
		t.Fatalf("a non-root caller must be refused, got %v", err)
	}
}
