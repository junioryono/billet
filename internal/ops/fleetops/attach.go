package fleetops

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/relay"
)

// JobsAttach is `billet jobs attach`: a terminal in a job's VM. WHAT IT SHOWS IS THE GUEST'S: the
// job is root in that VM, and its bytes go to this terminal as any remote machine's would.
func JobsAttach(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet jobs attach", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	run := fs.Int64("run", 0, "attach to a job of the GitHub workflow run with this id, with --job")
	job := fs.String("job", "", "with --run, the name of the job to attach to")

	if err := cli.ParseWithArgs(fs, args, 1); err != nil {
		return err
	}

	runGiven := false
	fs.Visit(func(f *flag.Flag) { runGiven = runGiven || f.Name == "run" })

	switch {
	case runGiven && (fs.NArg() != 0 || *job == "" || *run <= 0):
		return errors.New("billet jobs attach: --run takes the positive id of a run, and --job the name of one of " +
			"its jobs, and no lease")
	case !runGiven && (fs.NArg() != 1 || *job != ""):
		return errors.New("usage: billet jobs attach [--config PATH] <lease> | --run <run id> --job <name>")
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	var leaseID string

	if runGiven {
		leaseID, err = leaseOfRunJob(ctx, *cfgPath, *run, *job)
		if err != nil {
			return err
		}
	} else {
		leaseID = fs.Arg(0)
	}

	stream, err := app.AttachJob(ctx, cfg, leaseID)
	if err != nil {
		if errors.Is(err, app.ErrNoController) {
			return fmt.Errorf("billet jobs attach: no control plane answered on this host, and attaching goes "+
				"through it: %w", err)
		}

		return fmt.Errorf("billet jobs attach: %w", err)
	}
	defer stream.Close()

	status, err := operatorTerminal(ctx, env, stream)
	if err != nil {
		return fmt.Errorf("billet jobs attach: %w", err)
	}

	// THIS IS WHAT SAYS SOMEBODY FINISHED, as against a connection that merely went, since nothing between
	// here and the guest reads the exit frame. Not hearing it costs a hold its early end.
	if err := app.AttachedShellExited(ctx, cfg, leaseID); err != nil {
		fmt.Fprintf(env.Stderr, "the control plane was not told this session ended, so a hold on this job runs to "+
			"its bound; `billet jobs release` ends it: %v\n", err)
	}

	if status != 0 {
		return &cli.ExitError{Code: status & 0xff}
	}

	return nil
}

// leaseOfRunJob finds a run's job by name. Two of one name are a matrix's legs, which only a
// lease can tell apart, so that is refused with what to use instead.
func leaseOfRunJob(ctx context.Context, cfgPath string, run int64, name string) (string, error) {
	a, closeDB, err := ControlPlaneAllocator(ctx, cfgPath)
	if err != nil {
		return "", err
	}
	defer closeDB()

	leases, _, err := a.JobsOfRun(ctx, run)
	if err != nil {
		return "", fmt.Errorf("billet jobs attach: read the jobs of run %d: %w", run, err)
	}

	var found []string

	for _, id := range leases {
		rec, err := a.Job(ctx, id)
		if err != nil {
			return "", fmt.Errorf("billet jobs attach: read lease %s: %w", id, err)
		}

		if rec.Job.Name == name {
			found = append(found, id)
		}
	}

	switch len(found) {
	case 0:
		return "", fmt.Errorf("billet jobs attach: run %d has no job named %q that this ledger holds", run, name)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("billet jobs attach: run %d has %d jobs named %q; name one by its lease: %v",
			run, len(found), name, found)
	}
}

// operatorTerminal runs a session on this process's terminal: raw while it lasts, with the window
// sent as it changes, and answers the shell's status.
func operatorTerminal(ctx context.Context, env cli.Env, stream io.ReadWriteCloser) (int, error) {
	stop := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stop()

	var resizes chan [2]uint16

	if in, ok := env.Stdin.(*os.File); ok && term.IsTerminal(int(in.Fd())) {
		fd := int(in.Fd())

		previous, err := term.MakeRaw(fd)
		if err != nil {
			return 0, fmt.Errorf("put this terminal in raw mode: %w", err)
		}
		// RESTORED ON EVERY PATH, or the shell this was run from is left with no echo.
		defer term.Restore(fd, previous) //nolint:errcheck // nothing can be done, and nothing to say it on

		resizes = make(chan [2]uint16, 1)

		winch := make(chan os.Signal, 1)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)

		send := func() {
			if cols, rows, err := term.GetSize(fd); err == nil {
				select {
				case resizes <- [2]uint16{uint16(rows), uint16(cols)}:
				default:
				}
			}
		}
		send()

		done := make(chan struct{})
		defer close(done)

		go func() {
			for {
				select {
				case <-winch:
					send()
				case <-done:
					return
				}
			}
		}()
	}

	return runSession(stream, env.Stdin, env.Stdout, resizes)
}

// runSession speaks the guest's frames: what in holds and each window go out, what comes back goes
// to out, until the shell's status arrives. A STREAM THAT ENDS WITHOUT ONE IS AN ERROR, never a
// status of zero: the job's VM may have been destroyed under it.
func runSession(stream io.ReadWriter, in io.Reader, out io.Writer, resizes <-chan [2]uint16) (int, error) {
	var writing sync.Mutex

	write := func(kind byte, payload []byte) error {
		writing.Lock()
		defer writing.Unlock()

		return relay.WriteFrame(stream, kind, payload)
	}

	// NEITHER IS WAITED FOR: a read of a terminal cannot be interrupted, and the process ends
	// when the session does.
	go func() {
		buf := make([]byte, 4096)

		for {
			n, err := in.Read(buf)
			if n > 0 && write(relay.KindData, buf[:n]) != nil {
				return
			}

			if err != nil {
				return
			}
		}
	}()

	if resizes != nil {
		go func() {
			for size := range resizes {
				if write(relay.KindResize, relay.ResizePayload(size[0], size[1])) != nil {
					return
				}
			}
		}()
	}

	for {
		frame, err := relay.ReadFrame(stream)
		if err != nil {
			return 0, errors.New("the session ended without its shell's status; the job's VM may be gone")
		}

		switch frame.Kind {
		case relay.KindData:
			if _, err := out.Write(frame.Payload); err != nil {
				return 0, err
			}
		case relay.KindExit:
			return relay.ParseExit(frame.Payload)
		}
	}
}

// JobsRelease is `billet jobs release`: end a failed job's hold now.
func JobsRelease(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet jobs release", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)

	if err := cli.ParseWithArgs(fs, args, 1); err != nil {
		return err
	}

	if fs.NArg() != 1 {
		return errors.New("usage: billet jobs release [--config PATH] <lease>")
	}

	a, closeDB, err := ControlPlaneAllocator(ctx, *cfgPath)
	if err != nil {
		return err
	}
	defer closeDB()

	ended, err := a.EndLeaseHold(ctx, fs.Arg(0), alloc.HoldEndedByOperator)
	if err != nil {
		return fmt.Errorf("billet jobs release: %w", err)
	}

	if !ended {
		return fmt.Errorf("billet jobs release: lease %s is not held", fs.Arg(0))
	}

	fmt.Fprintf(env.Stdout, "Released. The control plane destroys this job's VM on its next pass, and its capacity "+
		"comes back then.\n")

	return nil
}

// PrintHolds is `billet status`'s lines for what is being held. A READ THAT FAILED IS SAID, since
// nothing shown would read as nothing held.
func PrintHolds(ctx context.Context, env cli.Env, a *alloc.Allocator) {
	holds, err := liveHolds(ctx, a)
	if err != nil {
		fmt.Fprintf(env.Stdout, "holds     unavailable: %v\n", err)

		return
	}

	now := time.Now()

	for i := range holds {
		h := &holds[i]
		fmt.Fprintf(env.Stdout, "held      %s  tier %s  a failed job's VM, %s left of its hold (until %s); "+
			"`billet jobs attach` opens it, `billet jobs release` ends it\n",
			h.LeaseID, h.Tier, max(h.Until.Sub(now), 0).Round(time.Second), h.Until.UTC().Format(time.RFC3339))
	}
}

// liveHolds is what is holding, less a hold whose lease has ended: a force operation or a teardown can
// take a held VM, and its row says nothing until the bound. What cannot be read is kept, since a hold
// left out would read as nothing held.
func liveHolds(ctx context.Context, a *alloc.Allocator) ([]alloc.LeaseHold, error) {
	holds, err := a.HoldingLeaseHolds(ctx)
	if err != nil {
		return nil, err
	}

	return slices.DeleteFunc(holds, func(h alloc.LeaseHold) bool {
		_, err := a.Lease(ctx, h.LeaseID)

		return errors.Is(err, alloc.ErrLeaseNotFound)
	}), nil
}

// settleHolds ends every hold when a drain is told to, and says it did.
func settleHolds(ctx context.Context, env cli.Env, a *alloc.Allocator) error {
	n, err := a.EndHoldingLeaseHolds(ctx, alloc.HoldEndedByDrain)
	if err != nil {
		return fmt.Errorf("end the holds: %w", err)
	}

	fmt.Fprintf(env.Stdout, "\nEnded %d held failed jobs' holds; their VMs are destroyed on the next pass.\n", n)

	return nil
}

// sayHolds says what a wait for quiet will wait for, since a wait that seems to hang on a job that has
// already failed is exactly what somebody would otherwise have to work out.
func sayHolds(ctx context.Context, env cli.Env, a *alloc.Allocator) {
	holds, err := liveHolds(ctx, a)
	if err != nil {
		fmt.Fprintf(env.Stdout, "\nCould not read what is held: %v\n", err)

		return
	}

	if len(holds) > 0 {
		fmt.Fprintf(env.Stdout, "\n%d failed jobs' VMs are held, until %s at the latest. A drain waits for them; "+
			"`billet drain --cut-holds` or `billet jobs release` ends them now.\n",
			len(holds), holds[len(holds)-1].Until.UTC().Format(time.RFC3339))
	}
}
