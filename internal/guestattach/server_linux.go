//go:build linux

package guestattach

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/relay"
)

// How a session ends once nobody is at the other end of it.
const (
	// hangupGrace is how long a shell has after its hangup before it is killed: one can ignore a
	// hangup, and a session's slot is one of few.
	hangupGrace = 5 * time.Second
	// drainGrace is how long what is left on a terminal has to reach the operator once its shell
	// is gone, and again to be written: something the shell left behind can hold the terminal
	// open for ever, and an operator can stop reading.
	drainGrace = 2 * time.Second
	// terminalWriteBound is how long what an operator typed waits for a terminal nobody is reading.
	// Past it the input is dropped: waiting on, this would stop reading the stream, and a disconnect
	// is only seen by reading it.
	terminalWriteBound = 5 * time.Second
	// acceptRetry is how long the accept loop waits after a failure of the guest's, not of the
	// listener: a job that ran out of descriptors is just what someone attaches to look at.
	acceptRetry = time.Second
	// acceptPollMillis is how often the accept loop looks at whether it should stop. A listening
	// vsock socket cannot be shut down to wake it, and a stop is a VM going away.
	acceptPollMillis = 1000
)

// Serve accepts terminals on vsock Port until ctx ends. A failure on one session ends that
// session. It runs in its own systemd unit, so it outlives the runner, which is what makes
// a failed job's held VM still answer.
func Serve(ctx context.Context, cfg Config, log *slog.Logger) error {
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
	if err != nil {
		return fmt.Errorf("guestattach: open a vsock socket: %w", err)
	}
	defer unix.Close(fd)

	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: Port}); err != nil {
		return fmt.Errorf("guestattach: bind vsock port %d: %w", Port, err)
	}

	if err := unix.Listen(fd, maxSessions); err != nil {
		return fmt.Errorf("guestattach: listen on vsock port %d: %w", Port, err)
	}

	slots := make(chan struct{}, maxSessions)

	var sessions sync.WaitGroup
	defer sessions.Wait()

	for ctx.Err() == nil {
		waiting := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}

		ready, err := unix.Poll(waiting, acceptPollMillis)
		if errors.Is(err, unix.EINTR) || (err == nil && ready == 0) {
			continue
		}

		if err != nil {
			return fmt.Errorf("guestattach: wait for a connection: %w", err)
		}

		// NON-BLOCKING, so a stream joins the runtime's poller and its Close ends a read of it. On
		// a blocking descriptor the read would hold the descriptor and its goroutine for ever.
		conn, _, err := unix.Accept4(fd, unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK)
		if err != nil {
			if errors.Is(err, unix.EINTR) || errors.Is(err, unix.ECONNABORTED) || errors.Is(err, unix.EAGAIN) {
				continue
			}

			log.Warn("could not accept a terminal; trying again", "error", err)
			closedIn(ctx.Done(), acceptRetry)

			continue
		}

		stream := os.NewFile(uintptr(conn), "vsock-session")

		select {
		case slots <- struct{}{}:
		default:
			turnAway(stream)

			continue
		}

		sessions.Go(func() {
			defer func() { <-slots }()
			defer stream.Close()

			if err := runSession(ctx, cfg, stream); err != nil {
				log.Warn("a terminal session ended on an error", "error", err)
			}
		})
	}

	return nil
}

// turnAway tells an operator past the slots why, since a stream that just closes reads as a VM gone. The
// accept loop is what this holds up, so the write gets a second and no more.
func turnAway(stream *os.File) {
	defer stream.Close()

	if err := stream.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return
	}

	//nolint:errcheck // the stream closing says what is left to say
	_ = relay.WriteFrame(stream, relay.KindData, []byte("billet: this VM has as many terminals open as it may\r\n"))
}

// runSession runs one shell on a new terminal, joined to stream by frames, until either ends.
//
// A DISCONNECT ENDS IT, whatever the shell does. A shell can ignore its hangup, and something it
// left behind can hold the terminal open, and this returning is what gives a slot back, of which a
// guest has few.
func runSession(ctx context.Context, cfg Config, stream io.ReadWriteCloser) error {
	primary, secondary, err := openTerminal()
	if err != nil {
		return err
	}
	defer primary.Close()

	session, disconnected := context.WithCancel(ctx)
	defer disconnected()

	cmd, err := shellCommand(session, cfg, secondary)
	if err != nil {
		_ = secondary.Close()

		return err
	}

	if err := cmd.Start(); err != nil {
		_ = secondary.Close()

		return fmt.Errorf("guestattach: start the shell: %w", err)
	}

	_ = secondary.Close()

	var pump sync.WaitGroup

	// FROM THE TERMINAL TO THE OPERATOR, until the shell and everything holding its
	// terminal are gone, which reads as EIO, or the bound below closes it.
	pump.Go(func() {
		buf := make([]byte, relay.MaxFrame)

		for {
			n, err := primary.Read(buf)
			if n > 0 {
				// NOBODY THERE TO WRITE TO IS A DISCONNECT, whatever the other direction has seen.
				if relay.WriteFrame(stream, relay.KindData, buf[:n]) != nil {
					disconnected()

					return
				}
			}

			if err != nil {
				return
			}
		}
	})

	// FROM THE OPERATOR TO THE TERMINAL. An end of the stream is a disconnect. Ends when the stream
	// is closed, which the caller does on return, or when the terminal is.
	go func() {
		for {
			f, err := relay.ReadFrame(stream)
			if err != nil {
				disconnected()

				return
			}

			switch f.Kind {
			case relay.KindData:
				_ = primary.SetWriteDeadline(time.Now().Add(terminalWriteBound)) //nolint:errcheck // with none, a write waits

				if _, err := primary.Write(f.Payload); err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
					return
				}
			case relay.KindResize:
				if rows, cols, err := relay.ParseResize(f.Payload); err == nil {
					setWindow(primary, rows, cols)
				}
			}
		}
	}()

	waitErr := cmd.Wait()

	drained := make(chan struct{})

	go func() {
		defer close(drained)

		pump.Wait()
	}()

	// THE TERMINAL'S LAST BYTES BEFORE THE EXIT FRAME, which the pump ends on once nothing holds its
	// other end. Bounded: closing the terminal ends a read of it, and closing the stream a write.
	if !closedIn(drained, drainGrace) {
		_ = primary.Close()

		if !closedIn(drained, drainGrace) {
			_ = stream.Close()

			<-drained
		}
	}

	// NOBODY TO TELL.
	if session.Err() != nil {
		return nil
	}

	status := 0
	if exit, ok := errors.AsType[*exec.ExitError](waitErr); ok {
		status = exit.ExitCode()
	}

	// BOUNDED LIKE THE REST: an operator who stopped reading must not keep this slot.
	deadline := time.AfterFunc(drainGrace, func() { _ = stream.Close() })
	defer deadline.Stop()

	return relay.WriteFrame(stream, relay.KindExit, relay.ExitPayload(status))
}

// closedIn says ch closed within d.
func closedIn(ch <-chan struct{}, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ch:
		return true
	case <-timer.C:
		return false
	}
}

// openTerminal opens a new pseudo-terminal, unlocked.
func openTerminal() (*os.File, *os.File, error) {
	primary, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("guestattach: open a terminal: %w", err)
	}

	var number uint32

	if err := onDescriptor(primary, func(fd int) error {
		if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
			return fmt.Errorf("unlock it: %w", err)
		}

		var err error

		number, err = unix.IoctlGetUint32(fd, unix.TIOCGPTN)

		return err
	}); err != nil {
		_ = primary.Close()

		return nil, nil, fmt.Errorf("guestattach: prepare a terminal: %w", err)
	}

	secondary, err := os.OpenFile("/dev/pts/"+strconv.FormatUint(uint64(number), 10),
		os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		_ = primary.Close()

		return nil, nil, fmt.Errorf("guestattach: open a terminal's other end: %w", err)
	}

	return primary, secondary, nil
}

// onDescriptor runs do on f's descriptor. NEVER f.Fd(), which puts the file in blocking mode for good:
// its Close would then no longer end a read of it, and that is what bounds a session's end.
func onDescriptor(f *os.File, do func(fd int) error) error {
	raw, err := f.SyscallConn()
	if err != nil {
		return err
	}

	var doErr error

	if err := raw.Control(func(fd uintptr) { doErr = do(int(fd)) }); err != nil {
		return err
	}

	return doErr
}

// setWindow tells the terminal its size. A failure leaves the old one, which costs a
// wrapped line.
func setWindow(f *os.File, rows, cols uint16) {
	_ = onDescriptor(f, func(fd int) error { //nolint:errcheck // see above
		return unix.IoctlSetWinsize(fd, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	})
}

// shellCommand is a login shell as the runner's user, the job's own, in the job's work
// directory, which is what an operator came to look at, with the account's groups, as the job has
// them: docker's among them. The node's bearer and the runner's registration are in the agent's
// environment, and this one starts from nothing. When ctx ends the shell is hung up on, and killed
// if it is still there hangupGrace later.
func shellCommand(ctx context.Context, cfg Config, terminal *os.File) (*exec.Cmd, error) {
	account, err := user.Lookup(cfg.User)
	if err != nil {
		return nil, fmt.Errorf("guestattach: find the job's user: %w", err)
	}

	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("guestattach: read the job's user: %w", err)
	}

	gid, err := strconv.ParseUint(account.Gid, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("guestattach: read the job's group: %w", err)
	}

	cmd := exec.CommandContext(ctx, cfg.Shell, "-l")
	cmd.Dir = cfg.Dir
	cmd.Env = []string{"HOME=" + account.HomeDir, "USER=" + account.Username, "LOGNAME=" + account.Username,
		"SHELL=" + cfg.Shell, "TERM=xterm-256color", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = terminal, terminal, terminal
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}

	// A LEADER OF ITS OWN SESSION AND SO OF ITS OWN GROUP, which is who a hangup is for.
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGHUP) }
	cmd.WaitDelay = hangupGrace

	// THE ONE ACCOUNT ALREADY NEEDS NO CHANGING, and only root can change one.
	if int(uid) != os.Geteuid() {
		names, err := account.GroupIds()
		if err != nil {
			return nil, fmt.Errorf("guestattach: read the job's groups: %w", err)
		}

		groups := make([]uint32, 0, len(names))

		for _, name := range names {
			group, err := strconv.ParseUint(name, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("guestattach: read the job's groups: %w", err)
			}

			groups = append(groups, uint32(group))
		}

		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: groups}

		// THE TERMINAL IS THE SHELL'S, or what opens it again by name (tmux, screen) cannot.
		if err := terminal.Chown(int(uid), int(gid)); err != nil {
			return nil, fmt.Errorf("guestattach: give the job's user its terminal: %w", err)
		}
	}

	return cmd, nil
}
