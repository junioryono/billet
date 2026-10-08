// Package cli is billet's command line: the command tree, the environment a
// command runs in, the exit statuses a command answers with, the flag helpers
// every command parses with, and the lifecycle the two long-running roles are
// stopped through. cmd/billet is the binary's entry and nothing else; what a
// command does moves under internal/ops one family at a time (#356 Phase 4).
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"text/tabwriter"
)

// Env is the process a command runs in: its streams and its environment. A
// command writes to Stdout and Stderr rather than to the process's own, so a
// test captures what it prints without swapping a global; only cmd/billet's
// main hands it the process's own.
type Env struct {
	Stdout io.Writer
	Stderr io.Writer
	Stdin  io.Reader
	Getenv func(string) string
}

// Command is one entry of the command tree. Run is handed the env the
// binary was given, and writes to it rather than to the process's streams.
type Command struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, env Env, args []string) error
}

// Tree is the commands the binary dispatches, built over the lifecycle so the
// two long-running roles can close over it.
type Tree func(lc *Lifecycle) []Command

// Main runs the command args name and returns the status the process exits
// with: 0 for success and for an explicit request for help, the status a
// command answered with (ExitError), and 1 for any other failure, printed to
// env.Stderr. exit ends the process at the third signal.
func Main(args []string, env Env, tree Tree, exit func(int)) int {
	err := Run(args, env, tree, exit)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		// Explicit -h is a successful request for help, not a usage error.
		return 0
	}

	// A QUIET EXIT carries a child's status whose output was already passed
	// through: nothing more is printed. A retiring host's refusal is never quiet,
	// and its status is ExitStatus's.
	if coded, ok := errors.AsType[*ExitError](err); ok && coded.Msg == "" && !retiring(err) {
		return coded.Code
	}

	fmt.Fprintf(env.Stderr, "billet: %v\n", err)

	return ExitStatus(err)
}

// Run dispatches args to the command it names.
func Run(args []string, env Env, tree Tree, exit func(int)) error {
	if len(args) == 0 {
		Usage(env.Stderr, tree)

		return errors.New("no command given")
	}

	switch args[0] {
	case "-h", "--help", "help":
		Usage(env.Stderr, tree)

		return nil
	}

	// Ctrl-C and SIGTERM cancel the context. Every long-running role drains rather
	// than dropping jobs: a runner killed mid-job leaves an orphaned registration
	// on GitHub.
	//
	// THE FIRST ASKS, THE SECOND INSISTS, THE THIRD GIVES UP — from ONE
	// registration, because two both receive every signal. See Lifecycle.Escalate.
	ctx, cancelGraceful := context.WithCancel(context.Background()) //nolint:forbidigo // the process's root context, created where the process begins.
	defer cancelGraceful()

	lc := NewLifecycle(cancelGraceful, env.Stderr)

	// Buffered for three, because there are three levels and a signal that
	// arrives while the goroutine is between receives must not be dropped.
	signals := make(chan os.Signal, 3)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)

	defer signal.Stop(signals)

	go lc.Escalate(signals, exit)

	for _, c := range tree(lc) {
		if c.Name == args[0] {
			return c.Run(ctx, env, args[1:])
		}
	}

	Usage(env.Stderr, tree)

	return fmt.Errorf("unknown command %q", args[0])
}

// Usage lists the commands.
func Usage(w io.Writer, tree Tree) {
	fmt.Fprint(w, "billet — self-hosted GitHub Actions runners\n\nusage: billet <command> [flags]\n\n")

	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)

	// A lifecycle nothing will use: usage only reads names and summaries, and
	// building one here keeps the tree from needing a nil-safe path that only
	// this call site would ever exercise.
	for _, c := range tree(NewLifecycle(func() {}, io.Discard)) {
		fmt.Fprintf(tw, "  %s\t%s\n", c.Name, c.Summary)
	}

	_ = tw.Flush()

	fmt.Fprint(w, "\nRun 'billet <command> -h' for details.\n")
}
