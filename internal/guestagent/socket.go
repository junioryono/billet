package guestagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// The flush socket's bounds: the connections answered at once (one more is closed
// unanswered), and how long writing the answer may take.
const (
	maxFlushConns = 16
	answerWrite   = time.Second
)

// FlushWait is the longest `billet guest flush` waits for the agent's answer.
const FlushWait = 2 * time.Second

// listenFlush listens at path, mode 0666 so the runner's user can ask, in a
// directory that is this process's and mode 0755: one another user could write to
// is one they could put a socket of their own in, and one they cannot search is
// one whose socket they cannot reach. The directory is judged and set through the
// descriptor that names it, never by its name again, and a link there is refused.
func listenFlush(path string) (*net.UnixListener, error) {
	dir := filepath.Dir(path)
	if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, fmt.Errorf("create its directory: %w", err)
	}

	if err := settleSocketDir(dir); err != nil {
		return nil, err
	}

	// A socket an earlier agent left; anything else there is not this agent's.
	if existing, err := os.Lstat(path); err == nil {
		if existing.Mode()&fs.ModeSocket == 0 {
			return nil, fmt.Errorf("%s is already something other than a socket", path)
		}

		if err := os.Remove(path); err != nil {
			return nil, fmt.Errorf("remove a stale socket: %w", err)
		}
	}

	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, err
	}

	if err := os.Chmod(path, 0o666); err != nil {
		_ = l.Close()

		return nil, fmt.Errorf("open the socket to the runner's user: %w", err)
	}

	return l, nil
}

// settleSocketDir holds dir to being this user's directory, writable by it alone,
// and makes it searchable by everyone, whatever the umask made of it.
func settleSocketDir(dir string) error {
	d, err := os.OpenFile(dir, os.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("open its directory: %w", err)
	}
	defer d.Close()

	info, err := d.Stat()
	if err != nil {
		return fmt.Errorf("examine its directory: %w", err)
	}

	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(st.Uid) != os.Geteuid() || info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is not a directory of this user's that only it can write to", dir)
	}

	if info.Mode().Perm() != 0o755 {
		if err := d.Chmod(0o755); err != nil {
			return fmt.Errorf("open its directory to the runner's user: %w", err)
		}
	}

	return nil
}

// serveFlush answers connections until l is closed. Each asks for the final flush
// and is answered with one byte once flushed is closed or after flushAnswerWait;
// nothing a client sends is read.
func serveFlush(l *net.UnixListener, requests chan<- struct{}, flushed <-chan struct{}, log *limitedLog) {
	var wg sync.WaitGroup
	defer wg.Wait()

	slots := make(chan struct{}, maxFlushConns)

	for {
		conn, err := l.AcceptUnix()
		if errors.Is(err, net.ErrClosed) {
			return
		}

		if err != nil {
			log.printf("accept", "a flush connection was not accepted: %v", err)
			time.Sleep(100 * time.Millisecond)

			continue
		}

		select {
		case slots <- struct{}{}:
		default:
			_ = conn.Close()

			continue
		}

		select {
		case requests <- struct{}{}:
		default:
		}

		wg.Go(func() {
			defer func() { <-slots }()

			answerFlush(conn, flushed)
		})
	}
}

// answerFlush writes one byte to conn once the final flush is done or after
// flushAnswerWait, and closes it.
func answerFlush(conn net.Conn, flushed <-chan struct{}) {
	defer conn.Close()

	wait := time.NewTimer(flushAnswerWait)
	defer wait.Stop()

	select {
	case <-flushed:
	case <-wait.C:
	}

	// A client that went away has been answered as far as it can be.
	_ = conn.SetWriteDeadline(time.Now().Add(answerWrite)) //nolint:errcheck // see above
	_, _ = conn.Write([]byte{'1'})                         //nolint:errcheck // see above
}

// Flush asks the agent listening at socket for its final flush and waits at most
// wait for its answer. Its error says why no answer came; the caller exits 0
// either way, because a job never fails over its monitor.
func Flush(ctx context.Context, socket string, wait time.Duration) error {
	deadline := time.Now().Add(wait)

	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	var d net.Dialer

	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return fmt.Errorf("no monitor answered: %w", err)
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}

	if _, err := io.ReadFull(conn, make([]byte, 1)); err != nil {
		return fmt.Errorf("the monitor did not say it flushed: %w", err)
	}

	return nil
}
