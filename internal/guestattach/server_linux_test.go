//go:build linux

package guestattach

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/relay"
)

// sessionEnds is longer than any bound of a session's own and of the shells below, so only a session
// that would never end reaches it.
const sessionEnds = 60 * time.Second

// session runs one session of shell against a stream the test holds, in the test's own account.
// reader is what came back, and done what runSession returned.
func session(t *testing.T, shell string) (operator net.Conn, frames *frameLog, done <-chan error) {
	t.Helper()

	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	guest, operator := net.Pipe()
	t.Cleanup(func() {
		guest.Close()
		operator.Close()
	})

	cfg := Config{User: account.Username, Shell: shell, Dir: t.TempDir()}
	ctx := t.Context()
	ended := make(chan error, 1)

	go func() { ended <- runSession(ctx, cfg, guest) }()

	frames = &frameLog{}

	go frames.read(operator)

	return operator, frames, ended
}

// frameLog is the frames a stream delivered.
type frameLog struct {
	mu     sync.Mutex
	data   bytes.Buffer
	status *int
}

func (l *frameLog) read(stream net.Conn) {
	for {
		f, err := relay.ReadFrame(stream)
		if err != nil {
			return
		}

		l.mu.Lock()

		switch f.Kind {
		case relay.KindData:
			l.data.Write(f.Payload)
		case relay.KindExit:
			if status, err := relay.ParseExit(f.Payload); err == nil {
				l.status = &status
			}
		}

		l.mu.Unlock()
	}
}

func (l *frameLog) got() (string, *int) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.data.String(), l.status
}

func await(t *testing.T, done <-chan error) error {
	t.Helper()

	select {
	case err := <-done:
		return err
	case <-time.After(sessionEnds):
		t.Fatal("the session did not end, so its slot would never come back")

		return nil
	}
}

// aShell writes a shell script of body and answers its path. It is run with an argument it ignores.
func aShell(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "shell")

	// UNDER ForkLock: another test's fork can otherwise hold this write's descriptor when the file is
	// executed, which fails with ETXTBSY (#233).
	syscall.ForkLock.Lock()
	defer syscall.ForkLock.Unlock()

	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	return path
}

// A SESSION RUNS WHAT IS TYPED AND ANSWERS THE SHELL'S STATUS, after everything it wrote.
func TestASessionRunsAShellAndSaysHowItEnded(t *testing.T) {
	t.Parallel()

	operator, frames, done := session(t, "/bin/sh")

	if err := relay.WriteFrame(operator, relay.KindData, []byte("echo hi-$((40+2)); exit 3\n")); err != nil {
		t.Fatal(err)
	}

	waitForStatus(t, frames)

	if err := await(t, done); err != nil {
		t.Fatalf("runSession: %v", err)
	}

	text, status := frames.got()
	if !strings.Contains(text, "hi-42") {
		t.Errorf("the terminal showed %q", text)
	}
	if status == nil || *status != 3 {
		t.Errorf("the status was %v, want 3", status)
	}
}

// waitForStatus waits for the exit frame, which arrives just after runSession wrote it.
func waitForStatus(t *testing.T, frames *frameLog) {
	t.Helper()

	deadline := time.Now().Add(sessionEnds)
	for time.Now().Before(deadline) {
		if _, status := frames.got(); status != nil {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("no exit frame arrived")
}

// A SHELL THAT IGNORES ITS HANGUP STILL ENDS WITH ITS SESSION. Four of these would otherwise take
// every slot a guest has, and nobody could attach to it again.
func TestADisconnectEndsAShellThatIgnoresItsHangup(t *testing.T) {
	t.Parallel()

	operator, _, done := session(t, aShell(t, "trap '' HUP\nexec sleep 30"))

	// Long enough for the trap to be set, or this proves only an ordinary hangup.
	time.Sleep(500 * time.Millisecond)

	if err := operator.Close(); err != nil {
		t.Fatal(err)
	}

	if err := await(t, done); err != nil {
		t.Errorf("a session ended by a disconnect answered %v", err)
	}
}

// SOMETHING A SHELL LEFT BEHIND DOES NOT KEEP ITS SESSION OPEN by holding the terminal: the shell
// ending is what the operator is told about.
func TestASessionEndsWithItsShellWhateverItLeftBehind(t *testing.T) {
	t.Parallel()

	_, frames, done := session(t, aShell(t, "trap '' HUP\nsleep 30 &\nexit 5"))

	if err := await(t, done); err != nil && !errors.Is(err, os.ErrClosed) {
		t.Errorf("runSession: %v", err)
	}

	waitForStatus(t, frames)

	if _, status := frames.got(); status == nil || *status != 5 {
		t.Errorf("the status was %v, want the shell's 5", status)
	}
}

// AN OPERATOR WHO STOPPED READING DOES NOT KEEP A SLOT: nothing reads this stream, so the exit frame
// cannot be written, and the session ends anyway.
func TestAnExitFrameNobodyReadsDoesNotHoldASession(t *testing.T) {
	t.Parallel()

	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}

	guest, operator := net.Pipe()
	t.Cleanup(func() {
		guest.Close()
		operator.Close()
	})

	cfg := Config{User: account.Username, Shell: aShell(t, "exit 0"), Dir: t.TempDir()}
	ctx := t.Context()
	done := make(chan error, 1)

	go func() { done <- runSession(ctx, cfg, guest) }()

	if err := await(t, done); err == nil {
		t.Error("an exit frame nobody read was taken for written")
	}
}
