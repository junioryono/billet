package guestagent

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

// socketDir is a directory short enough for a unix socket's address, which
// t.TempDir's names are not on Darwin.
func socketDir(t *testing.T) string {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "billet-gm-") //nolint:usetesting // t.TempDir exceeds Darwin's unix socket address limit
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	return dir
}

// socketGuard is how long a test over a real socket waits for something whose
// bound under test is a few seconds or none. It is a failure deadline, never a
// condition: what it waits for happens long before it however loaded the
// machine, or never happens at all.
const socketGuard = time.Minute

// waitFor polls cond for at most socketGuard of real time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(socketGuard)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// flushWithin is Flush with a wait of its own, failing the test if it has not
// returned by socketGuard.
func flushWithin(t *testing.T, what, socket string, wait time.Duration) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- Flush(t.Context(), socket, wait) }()

	select {
	case err := <-done:
		return err
	case <-time.After(socketGuard):
		t.Fatalf("%s had not returned after %s", what, socketGuard)

		return nil
	}
}

// The job-completed hook's flush sends the final batch and is answered once it
// is; after it the agent sends nothing more, a second flush is answered at once,
// and the SIGTERM that follows only ends the agent. Over a real socket and a real
// connection to the node, through the transport the agent builds itself.
//
// SERIAL, because it lengthens flushAnswerWait past the guard: a connection is
// then answered only by a finished flush, so neither answer can be the timer's,
// and how fast a loaded machine answers is not what is measured.
func TestASocketFlushThenSIGTERMSendsExactlyOneFinalBatch(t *testing.T) {
	saved := flushAnswerWait
	flushAnswerWait = 10 * time.Minute

	t.Cleanup(func() { flushAnswerWait = saved })

	node := newFakeNode(t)
	server := httptest.NewServer(node)
	t.Cleanup(server.Close)

	socket := filepath.Join(socketDir(t), "m.sock")

	var log syncBuffer

	cfg := testConfig(t, node, &log)
	cfg.Endpoint, cfg.Transport, cfg.Socket = server.URL, nil, socket

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	done := make(chan error, 1)

	go func() { done <- Run(ctx, cfg) }()

	waitFor(t, "the hello", func() bool {
		hellos, _ := node.snapshot()

		return len(hellos) == 1
	})

	waitFor(t, "the socket", func() bool {
		info, err := os.Stat(socket)

		return err == nil && info.Mode()&fs.ModeSocket != 0 && info.Mode().Perm() == 0o666
	})

	if err := flushWithin(t, "the flush", socket, flushAnswerWait); err != nil {
		t.Fatalf("the flush was not answered: %v", err)
	}

	if _, posts := node.snapshot(); len(posts) != 1 || !posts[0].final || posts[0].batch.Seq != 1 {
		t.Fatalf("after the flush's answer, %d posts, want the final alone", len(posts))
	}

	if err := flushWithin(t, "a second flush", socket, flushAnswerWait); err != nil {
		t.Fatalf("a second flush answered %v, want at once", err)
	}

	// THE THREE SECONDS SIGTERM IS ANSWERED IN are proved on the bubble's clock
	// (TestSIGTERMIsAnsweredWithinThreeSecondsWhateverHoldsTheLoop); here the
	// guard only ends a test whose agent never returns, and that SIGTERM only
	// ends the agent is the count of what it sent.
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the agent returned %v", err)
		}
	case <-time.After(socketGuard):
		t.Fatalf("the agent did not return within %s of SIGTERM", socketGuard)
	}

	if _, posts := node.snapshot(); len(posts) != 1 || finals(posts) != 1 {
		t.Fatalf("%d posts, %d final, want the one final batch", len(posts), finals(posts))
	}

	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Errorf("the socket outlived the agent: %v", err)
	}
}

// A connection is answered once the final flush is done, or after 2 s whatever
// became of it; nothing the client sends is read.
func TestAFlushIsAnsweredAfterTwoSecondsWhenTheFinalBatchIsNot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		agentSide, client := net.Pipe()
		began := time.Now()

		go answerFlush(agentSide, make(chan struct{}))

		b, err := io.ReadAll(client)
		if err != nil || string(b) != "1" || time.Since(began) != flushAnswerWait {
			t.Fatalf("read %q (%v) after %v, want one byte after %v", b, err, time.Since(began), flushAnswerWait)
		}
	})

	synctest.Test(t, func(t *testing.T) {
		agentSide, client := net.Pipe()
		flushed := make(chan struct{})
		began := time.Now()

		go answerFlush(agentSide, flushed)

		time.Sleep(300 * time.Millisecond)
		close(flushed)

		b, err := io.ReadAll(client)
		if err != nil || string(b) != "1" || time.Since(began) != 300*time.Millisecond {
			t.Fatalf("read %q (%v) after %v, want one byte once flushed", b, err, time.Since(began))
		}
	})
}

// The socket is mode 0666 in a directory only its owner can write to; a directory
// others can write to, or something other than a socket at the name, is refused,
// and a socket an earlier agent left is replaced.
func TestTheFlushSocketListensOnlyWhereItIsSafeTo(t *testing.T) {
	t.Parallel()

	dir := socketDir(t)

	stale, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(dir, "s.sock"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	stale.SetUnlinkOnClose(false)
	_ = stale.Close()

	l, err := listenFlush(filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatalf("a stale socket was not replaced: %v", err)
	}

	_ = l.Close()

	writeFile(t, filepath.Join(dir, "file.sock"), "")

	if _, err := listenFlush(filepath.Join(dir, "file.sock")); err == nil || !strings.Contains(err.Error(), "other than a socket") {
		t.Errorf("a regular file at the socket's name answered %v", err)
	}

	open := filepath.Join(dir, "open")
	if err := os.Mkdir(open, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.Chmod(open, 0o777); err != nil {
		t.Fatal(err)
	}

	if _, err := listenFlush(filepath.Join(open, "s.sock")); err == nil || !strings.Contains(err.Error(), "only it can write") {
		t.Errorf("a directory anyone can write to answered %v", err)
	}

	if err := os.Symlink(dir, filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	if _, err := listenFlush(filepath.Join(dir, "link", "s.sock")); err == nil {
		t.Error("a directory reached through a symlink was used")
	}

	l, err = listenFlush(filepath.Join(dir, "new", "s.sock"))
	if err != nil {
		t.Fatalf("a directory the agent creates was refused: %v", err)
	}

	_ = l.Close()

	info, err := os.Stat(filepath.Join(dir, "new"))
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Errorf("the directory the agent created: %v %v, want 0755", info, err)
	}
}

// With no monitor listening, `billet guest flush` has nothing to wait for.
//
// THE WAIT IS FAR PAST THE GUARD, not a second: a failed dial took 1.87 s on a
// loaded CI runner (2026-10-12), and what this holds is that the flush does not
// wait out its wait.
func TestAFlushWithNoMonitorReturnsAtOnce(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(socketDir(t), "absent.sock")

	err := flushWithin(t, "a flush with no monitor", socket, 10*time.Minute)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("answered %v, want no such socket", err)
	}
}

// A monitor that never answers is waited for FlushWait and no longer.
func TestAFlushWaitsNoLongerThanItsWait(t *testing.T) {
	t.Parallel()

	socket := filepath.Join(socketDir(t), "q.sock")

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = l.Close() })

	began := time.Now()

	// NO EARLIER THAN THE WAIT, which no load can make shorter; the guard, not a
	// stopwatch, catches a flush that waits for an answer the monitor never sends.
	if err := flushWithin(t, "a flush to a silent monitor", socket, 200*time.Millisecond); err == nil {
		t.Fatal("a silent monitor answered")
	}

	if took := time.Since(began); took < 200*time.Millisecond {
		t.Fatalf("waited %v, want the whole 200ms", took)
	}
}
