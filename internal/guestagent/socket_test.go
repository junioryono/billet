package guestagent

import (
	"context"
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

// waitFor polls cond for at most five seconds of real time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// The job-completed hook's flush sends the final batch and is answered once it
// is; after it the agent sends nothing more, a second flush is answered at once,
// and the SIGTERM that follows only ends the agent. Over a real socket and a real
// connection to the node, through the transport the agent builds itself.
func TestASocketFlushThenSIGTERMSendsExactlyOneFinalBatch(t *testing.T) {
	t.Parallel()

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

	if err := Flush(t.Context(), socket, FlushWait); err != nil {
		t.Fatalf("the flush was not answered: %v", err)
	}

	if _, posts := node.snapshot(); len(posts) != 1 || !posts[0].final || posts[0].batch.Seq != 1 {
		t.Fatalf("after the flush's answer, %d posts, want the final alone", len(posts))
	}

	began := time.Now()
	if err := Flush(t.Context(), socket, FlushWait); err != nil || time.Since(began) > time.Second {
		t.Fatalf("a second flush answered %v after %v, want at once", err, time.Since(began))
	}

	stopped := time.Now()
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the agent returned %v", err)
		}
	case <-time.NewTimer(3 * time.Second).C:
		t.Fatal("the agent did not return within 3s of SIGTERM")
	}

	if took := time.Since(stopped); took > time.Second {
		t.Errorf("SIGTERM after a flush took %v; it only ends the agent", took)
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
func TestAFlushWithNoMonitorReturnsAtOnce(t *testing.T) {
	t.Parallel()

	began := time.Now()

	if err := Flush(t.Context(), filepath.Join(t.TempDir(), "absent.sock"), FlushWait); err == nil ||
		time.Since(began) > time.Second {
		t.Fatalf("answered %v after %v, want an error at once", err, time.Since(began))
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

	if err := Flush(t.Context(), socket, 200*time.Millisecond); err == nil {
		t.Fatal("a silent monitor answered")
	}

	if took := time.Since(began); took < 200*time.Millisecond || took > 2*time.Second {
		t.Fatalf("waited %v, want about 200ms", took)
	}
}
