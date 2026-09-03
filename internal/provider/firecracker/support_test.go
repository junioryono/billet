package firecracker

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"testing"
)

// shortDir is a temporary directory short enough to hold a unix socket path.
//
// NOT t.TempDir(), AND THAT IS THE POINT RATHER THAN A WORKAROUND. Go's temp
// directory name embeds the test's name, and a jail socket sits six components
// below its base — so on macOS `t.TempDir()` alone puts the address over the
// operating system's 104-byte limit and every launch fails with `bind: invalid
// argument`. That limit is a real constraint on a deployment too, which is why
// checkSocketPath exists; here it just means the fixture needs a shorter root.
// The budget is tight enough to be worth stating: the tail below a chroot base is
// 88 bytes — the resolved binary's directory, `billet-` plus a 32-character lease
// id, and `root/run/firecracker.socket` — so on darwin's 103-byte limit a base has
// 15 characters to live in. `/srv/jailer`, the jailer's own default and what a
// deployment uses, is 11.
func shortDir(t *testing.T) string {
	t.Helper()

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("name a temp dir: %v", err)
	}

	// NOT t.TempDir() AND NOT os.MkdirTemp: both produce a name far longer than
	// the budget above, which is the whole reason this exists.
	dir := shortTempRoot + "/b" + hex.EncodeToString(suffix[:])

	// os.Mkdir RATHER THAN MkdirAll, AND THAT IS WHAT MAKES THE CLEANUP SAFE.
	// MkdirAll accepts a directory that is already there — residue from a run
	// that was killed, or another test that drew the same name — and the
	// RemoveAll below would then delete a directory this call never created.
	// Mkdir refuses it, so the name is proved this caller's before anything is
	// registered to remove it. Failing loudly is the right direction: adopting
	// somebody else's directory is the outcome worth avoiding.
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("make a short temp dir: %v", err)
	}

	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove %s: %v", dir, err)
		}
	})

	return dir
}

// shortTempRoot is where a fixture short enough to hold a socket path lives.
const shortTempRoot = "/tmp"

// indexOf reports where a value appears in a slice, or -1.
func indexOf(haystack []string, needle string) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}

	return -1
}

// renderJSON re-encodes a decoded body so an assertion can search the whole of it
// rather than walking a map it has to know the shape of.
func renderJSON(t *testing.T, v any) string {
	t.Helper()

	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	return string(raw)
}

// hangingSocket replaces a path with a listener that accepts and never answers.
//
// The one way to stage "billet could not tell" honestly. A closed socket is proof
// the VMM is gone; a socket that accepts a connection and then says nothing is the
// timeout case, and the two must reach opposite conclusions.
func hangingSocket(t *testing.T, path string) net.Listener {
	t.Helper()

	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		t.Fatalf("clear %s: %v", path, err)
	}

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("listen on %s: %v", path, err)
	}

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			// Held open, deliberately, and never written to.
			t.Cleanup(func() {
				if err := conn.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
					t.Errorf("close a held connection: %v", err)
				}
			})
		}
	}()

	return ln
}

// writeJSON answers a fake VMM request, failing the test rather than dropping the
// error — a fake that cannot report its own failure is one that lies quietly.
func writeJSON(w http.ResponseWriter, body any) {
	raw, err := json.Marshal(body)
	if err != nil {
		panic("encoding a fake vmm answer: " + err.Error())
	}

	if _, err := w.Write(raw); err != nil {
		panic("writing a fake vmm answer: " + err.Error())
	}
}
