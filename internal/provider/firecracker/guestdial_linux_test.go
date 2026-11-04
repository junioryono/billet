//go:build linux

package firecracker

import (
	"bufio"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/junioryono/billet/internal/provider"
)

// vmmEnd is a unix socket pair whose far end the test answers as a VMM would. A real socket,
// because the peer's uid is the thing being checked.
func vmmEnd(t *testing.T) (*net.UnixConn, net.Conn) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "v.sock")

	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	near, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { near.Close() })

	far, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { far.Close() })

	unix, ok := near.(*net.UnixConn)
	if !ok {
		t.Fatalf("a unix dial gave %T", near)
	}

	return unix, far
}

// THE VMM'S ANSWER DECIDES: `OK <port>` is a stream, with whatever followed it in the same read
// still there, and anything else is nothing listening, never quoted.
func TestAVsockHandshakeReadsTheVMMsAnswer(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("uid 0 is refused as a microVM's, so this needs another account")
	}

	near, far := vmmEnd(t)

	done := make(chan error, 1)
	go func() {
		line := make([]byte, 64)

		n, err := far.Read(line)
		if err == nil && string(line[:n]) != "CONNECT 5151\n" {
			err = errors.New("asked " + string(line[:n]))
		}

		if err == nil {
			_, err = far.Write([]byte("OK 1073741824\nfirst bytes"))
		}

		done <- err
	}()

	stream, err := handshakeVsock(near, os.Geteuid(), 5151)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("the VMM's side: %v", err)
	}

	got := make([]byte, len("first bytes"))
	if _, err := io.ReadFull(stream, got); err != nil || string(got) != "first bytes" {
		t.Errorf("after the answer the stream read %q, %v", got, err)
	}
}

// A SOCKET ANSWERED BY ANOTHER UID IS NOT THIS MICROVM'S, however it answers: a VMM that has been
// taken over can link its socket to another lease's.
func TestAVsockAnsweredByAnotherUIDIsRefused(t *testing.T) {
	t.Parallel()

	near, _ := vmmEnd(t)

	_, err := handshakeVsock(near, os.Geteuid()+1, 5151)
	if !errors.Is(err, errVMMPeer) {
		t.Errorf("another uid's socket: %v", err)
	}

	near2, _ := vmmEnd(t)

	if _, err := handshakeVsock(near2, 0, 5151); !errors.Is(err, errVMMPeer) {
		t.Errorf("a resource record naming no uid: %v", err)
	}
}

// NOTHING LISTENING IS ITS OWN ANSWER, and what a VMM said is never in the error.
func TestAVsockWithNothingListeningSaysSo(t *testing.T) {
	t.Parallel()

	if os.Geteuid() == 0 {
		t.Skip("uid 0 is refused as a microVM's, so this needs another account")
	}

	for name, answer := range map[string]string{
		"closed":      "",
		"another":     "HOSTILE words\n",
		"past bound":  strings.Repeat("HOSTILE", 20),
		"no line end": "HOSTILE",
	} {
		near, far := vmmEnd(t)

		go func() {
			_, _ = far.Read(make([]byte, 64)) //nolint:errcheck // the handshake's answer is what is checked
			_, _ = far.Write([]byte(answer))  //nolint:errcheck // the handshake's answer is what is checked
			_ = far.Close()
		}()

		_, err := handshakeVsock(near, os.Geteuid(), 5151)
		if !errors.Is(err, provider.ErrNoGuestListener) {
			t.Errorf("%s: %v", name, err)
		}

		if err != nil && strings.Contains(err.Error(), "HOSTILE") {
			t.Errorf("%s: the error quotes the VMM: %v", name, err)
		}
	}
}

// A READ THAT FAILED IS NOT AN ANSWER: a VMM that stalled says nothing of whether the guest listens,
// and the node must not say it does not.
func TestAVsockThatStalledIsNotNothingListening(t *testing.T) {
	t.Parallel()

	_, err := readVsockAnswer(bufio.NewReader(iotest.ErrReader(os.ErrDeadlineExceeded)))
	if err == nil || errors.Is(err, provider.ErrNoGuestListener) {
		t.Errorf("a read that timed out: %v", err)
	}

	_, err = readVsockAnswer(bufio.NewReader(strings.NewReader("")))
	if !errors.Is(err, provider.ErrNoGuestListener) {
		t.Errorf("a VMM that closed: %v", err)
	}
}
