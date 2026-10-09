package uplink

import (
	"io"
	"os"
	"os/exec"
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

	// HELD OPEN, AS A RUNNING SHAPER HOLDS ITS OWN BINARY: a file nothing holds
	// has its inode freed on removal, and the filesystem may give that same
	// inode to the new file, which then reads as the old one. Measured in CI.
	held, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

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

	replaceAt(t, path)

	if !exe.replaced() {
		t.Fatal("a new file at the executable's path was not noticed")
	}

	if (executable{}).replaced() {
		t.Fatal("an executable that could not be read at start read as replaced")
	}
}

// helperEnv names the path a copy of this test binary runs from, when the copy
// is the helper below.
const helperEnv = "BILLET_UPLINK_EXE_HELPER"

// THE IDENTITY IS THE RUNNING FILE'S, EVEN WITH ITS PATH GONE. A copy of this
// test binary unlinks the path it runs from before it records its identity,
// which is a shaper starting while an upgrade has the path empty, and then sees
// a new file appear there. Reading the identity from the path records nothing in
// that window and never notices the new file; /proc/self/exe still names the
// running file. NOT PARALLEL: it writes an executable and runs it, and a fork
// from a parallel test holding the write open makes that exec fail.
func TestTheRecordedIdentityIsTheRunningFileWithItsPathGone(t *testing.T) {
	if _, err := os.Stat("/proc/self/exe"); err != nil {
		t.Skip("no /proc/self/exe on this platform")
	}

	self, err := os.Open("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}
	defer self.Close()

	path := filepath.Join(t.TempDir(), "billet")

	out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := io.Copy(out, self); err != nil {
		t.Fatal(err)
	}

	if err := out.Close(); err != nil {
		t.Fatal(err)
	}

	cmd := exec.CommandContext(t.Context(), path, "-test.run=^TestExecutableHelper$")
	cmd.Env = append(os.Environ(), helperEnv+"="+path)

	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the helper running from a path it unlinked: %v\n%s", err, output)
	}
}

// TestExecutableHelper is the helper above, and does nothing in an ordinary run.
func TestExecutableHelper(t *testing.T) {
	path := os.Getenv(helperEnv)
	if path == "" {
		t.Skip("run only as the helper of TestTheRecordedIdentityIsTheRunningFileWithItsPathGone")
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}

	exe := currentExecutable()
	if exe.info == nil {
		t.Fatal("with its path gone, the running file recorded no identity")
	}

	replaceAt(t, path)

	if !exe.replaced() {
		t.Fatal("a new file at the path of the running file was not noticed")
	}
}

// replaceAt puts a new file at path, by rename as an upgrade does.
func replaceAt(t *testing.T, path string) {
	t.Helper()

	next := path + ".next"
	if err := os.WriteFile(next, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}
