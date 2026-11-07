// Package guest is `billet guest`: the commands that run inside a job's VM. `billet
// guest monitor` is the in-VM agent the guest's own agent starts as root, and
// `billet guest flush` is the client the runner's job-completed hook runs to ask it
// for its final batch.
//
// Neither ever makes the job fail: each exits 0 on every path the node's contract
// names, and the guest's agent ignores their status besides.
package guest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/signal"
	"strings"
	"syscall"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/guestagent"
	"github.com/junioryono/billet/internal/guestattach"
	"github.com/junioryono/billet/internal/version"
)

// The guest's view of the node's guest-facing service: the runner's environment
// carries both, and neither is ever an argument.
const (
	envEndpoint = "BILLET_CACHE_ENDPOINT"
	envToken    = "BILLET_CACHE_TOKEN"
)

// The places in a guest the agent reads and listens at.
const (
	defaultProc      = "/proc"
	defaultDiag      = "/home/runner/runner/_diag"
	defaultJUnitRoot = "/home/runner/work"
	defaultSocket    = "/run/billet/guest-monitor.sock"
	// A session is the job's own account, in the job's own directory.
	defaultAttachUser  = "runner"
	defaultAttachShell = "/bin/bash"
	defaultAttachDir   = "/home/runner/runner/_work"
	// defaultNice is low enough that a job's own threads win the CPU, and high
	// enough that the final flush still finishes inside its 3 s on a guest
	// whose every CPU is busy.
	defaultNice = 10
)

// Run is `billet guest`.
func Run(ctx context.Context, env cli.Env, args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "monitor":
			return monitor(ctx, env, args[1:])
		case "flush":
			return flush(ctx, env, args[1:])
		case "attach-serve":
			return attachServe(ctx, env, args[1:])
		}
	}

	return errors.New("usage: billet guest <monitor|flush|attach-serve> [flags] (run inside a job's VM)")
}

// attachServe is `billet guest attach-serve`, the terminal server the image's unit starts on a tier
// that may be attached to. It never reaches the job: it is a unit of its own, so it outlives the
// runner, which is what makes a held VM worth attaching to.
func attachServe(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet guest attach-serve", env.Stdout)
	user := fs.String("user", defaultAttachUser, "the account a session's shell runs as")
	shell := fs.String("shell", defaultAttachShell, "the shell a session runs")
	dir := fs.String("dir", defaultAttachDir, "where a session's shell starts")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	return guestattach.Serve(ctx, guestattach.Config{User: *user, Shell: *shell, Dir: *dir},
		slog.New(slog.NewTextHandler(env.Stderr, nil)))
}

// monitor is `billet guest monitor`.
func monitor(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet guest monitor", env.Stdout)
	proc := fs.String("proc", defaultProc, "the /proc tree the guest's totals and processes are read from")
	diag := fs.String("diag", defaultDiag, "the runner's diagnostics directory, whose Worker log marks each step")

	var roots []string

	fs.Func("junit-root", "a directory searched for JUnit XML at the final flush; repeat for more (default "+
		defaultJUnitRoot+")", func(v string) error {
		if v == "" {
			return errors.New("a JUnit root is a directory")
		}

		roots = append(roots, v)

		return nil
	})

	socket := fs.String("socket", defaultSocket, "the unix socket a connection to asks for the final flush")
	nice := fs.Int("nice", defaultNice, "the nice value the agent runs at; 0 leaves it as started")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	if *nice < -20 || *nice > 19 {
		return fmt.Errorf("--nice %d is not a nice value from -20 to 19", *nice)
	}

	if err := guestagent.Supported(); err != nil {
		return err
	}

	// A Go process that does not handle SIGPIPE is killed by it on a write to a
	// closed pipe at stderr, so a log whose reader went away would end the agent
	// by a signal, not with the 0 every stop owes. Ignored, the write just fails.
	signal.Ignore(syscall.SIGPIPE)

	return guestagent.Run(ctx, monitorConfig(env, *proc, *diag, roots, *socket, *nice))
}

// monitorConfig is the agent the flags and the environment describe. /proc is
// read through guestagent.ProcFS, which never waits on what the job put there.
func monitorConfig(env cli.Env, proc, diag string, roots []string, socket string, nice int) guestagent.Config {
	if len(roots) == 0 {
		roots = []string{defaultJUnitRoot}
	}

	return guestagent.Config{
		Endpoint:   strings.TrimSpace(env.Getenv(envEndpoint)),
		Token:      env.Getenv(envToken),
		Proc:       guestagent.ProcFS(proc),
		DiagDir:    diag,
		JUnitRoots: roots,
		Socket:     socket,
		Nice:       nice,
		Log:        env.Stderr,
		Version:    version.Version(),
	}
}

// flush is `billet guest flush`: it exits 0 whether or not a monitor answered.
func flush(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet guest flush", env.Stdout)
	socket := fs.String("socket", defaultSocket, "the monitor's unix socket")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	// A job without a monitor is the usual case where monitoring is off, and the
	// hook's output lands in the job's log: say nothing.
	_ = guestagent.Flush(ctx, *socket, guestagent.FlushWait)

	return nil
}
