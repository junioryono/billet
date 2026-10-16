//go:build linux

package firecracker

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// THE EXECUTABLE IS THE ONE SERVING THE SOCKET, named by the kernel: a listener this
// test opens is served by this test binary, and a uid other than its own is refused.
func TestTheSocketPeerNamesTheExecutableServingIt(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(shortDir(t), "api.sock")

	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			_ = conn.Close()
		}
	}()

	if os.Getuid() == 0 {
		t.Skip("root's uid is 0, which a jail is never given")
	}

	exe, err := apiPeerExecutable(socket, os.Getuid())
	if err != nil {
		t.Fatalf("apiPeerExecutable: %v", err)
	}
	defer exe.Close()

	var d fileDigests

	got, err := d.ofOpen(exe, "peer")
	if err != nil {
		t.Fatal(err)
	}

	want, err := d.of("/proc/self/exe")
	if err != nil {
		t.Fatal(err)
	}

	if got != want {
		t.Errorf("the socket's peer executable digests to %s, and this test binary to %s", got, want)
	}

	if _, err := apiPeerExecutable(socket, os.Getuid()+1); err == nil {
		t.Error("a socket served by another uid was accepted as the microVM's")
	}
}
