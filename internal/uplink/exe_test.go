package uplink

import (
	"os"
	"path/filepath"
	"testing"
)

// A NEW FILE AT THE PATH IS A REPLACEMENT, AND NO FILE IS A SWAP UNDERWAY. Every
// upgrade path puts a new file where the old one was; mid-transaction the path
// is empty, and the shaper must not end then, because the cleanup it would ask
// for needs the binary that is not there yet.
func TestAReplacedExecutableIsNoticedAndAMissingOneIsNot(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "billet")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	exe := executable{path: path, info: info}

	if exe.replaced() {
		t.Fatal("the executable it started from read as replaced")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	if exe.replaced() {
		t.Fatal("a path with nothing at it read as replaced; that is a transaction mid-swap")
	}

	next := path + ".next"
	if err := os.WriteFile(next, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}

	if !exe.replaced() {
		t.Fatal("a new file at the executable's path was not noticed")
	}

	if (executable{}).replaced() {
		t.Fatal("an executable that could not be read at start read as replaced")
	}
}

// THE IDENTITY IS THE RUNNING FILE'S. A shaper started after its path already
// holds a newer file, or none, must still know which file it runs; on Linux that
// is /proc/self/exe, and the test binary is as good a running file as any.
func TestTheRecordedIdentityIsTheRunningFile(t *testing.T) {
	t.Parallel()

	running, err := os.Stat("/proc/self/exe")
	if err != nil {
		t.Skip("no /proc/self/exe on this platform")
	}

	exe := currentExecutable()
	if exe.info == nil || !os.SameFile(exe.info, running) {
		t.Fatalf("the recorded identity is not the running file's (path %q)", exe.path)
	}
}
