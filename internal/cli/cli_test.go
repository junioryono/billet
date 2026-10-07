package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
)

// testTree is a tree of commands that answer as told, in the order given.
func testTree(names []string, answers map[string]error) Tree {
	return func(*Lifecycle) []Command {
		out := make([]Command, 0, len(names))

		for _, name := range names {
			out = append(out, Command{Name: name, Summary: "answers " + name,
				Run: func(context.Context, []string) error { return answers[name] }})
		}

		return out
	}
}

// fixtureUsage is what Usage prints for testTree(fixtureNames, ...).
const fixtureUsage = "billet — self-hosted GitHub Actions runners\n\nusage: billet <command> [flags]\n\n" +
	"  ok          answers ok\n" +
	"  asks-help   answers asks-help\n" +
	"  due         answers due\n" +
	"  quiet       answers quiet\n" +
	"  failure     answers failure\n" +
	"\nRun 'billet <command> -h' for details.\n"

var fixtureNames = []string{"ok", "asks-help", "due", "quiet", "failure"}

// WHAT THE PROCESS EXITS WITH, AND EXACTLY WHAT IT SAYS WHERE. A command's
// success and an explicit request for help are 0; a status a command answers
// with is that status, printed unless it is quiet (a child's, whose output
// already went through); anything else is 1, printed; no command, or one the
// tree does not have, is 1 after the usage; and -h, --help and help print the
// usage and are 0. Nothing goes to stdout: a command's own output is its own.
func TestMainAnswersWithTheStatusACommandGives(t *testing.T) {
	t.Parallel()

	tree := testTree(fixtureNames, map[string]error{
		"asks-help": flag.ErrHelp,
		"due":       &ExitError{Code: 2, Msg: "the runner image is due to be rebuilt"},
		"quiet":     &ExitError{Code: 4},
		"failure":   errors.New("it broke"),
	})

	for _, tc := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"ok"}, 0, ""},
		{[]string{"asks-help"}, 0, ""},
		{[]string{"due"}, 2, "billet: the runner image is due to be rebuilt\n"},
		{[]string{"quiet"}, 4, ""},
		{[]string{"failure"}, 1, "billet: it broke\n"},
		{nil, 1, fixtureUsage + "billet: no command given\n"},
		{[]string{"nope"}, 1, fixtureUsage + "billet: unknown command \"nope\"\n"},
		{[]string{"-h"}, 0, fixtureUsage},
		{[]string{"--help"}, 0, fixtureUsage},
		{[]string{"help"}, 0, fixtureUsage},
	} {
		var stdout, stderr bytes.Buffer

		exited := false

		code := Main(tc.args, Env{Stdout: &stdout, Stderr: &stderr}, tree, func(int) { exited = true })

		if code != tc.code || exited {
			t.Errorf("Main(%q) = %d (exited by signal: %v), want %d", tc.args, code, exited, tc.code)
		}

		if stderr.String() != tc.stderr {
			t.Errorf("Main(%q) printed to stderr:\n%q\nwant:\n%q", tc.args, stderr.String(), tc.stderr)
		}

		if stdout.Len() != 0 {
			t.Errorf("Main(%q) wrote to stdout: %q", tc.args, stdout.String())
		}
	}
}

// THE LIFECYCLE A COMMAND IS GIVEN TELLS THE OPERATOR ON THE ENV'S STDERR: the
// second and third signals' warnings go where the binary's stderr is, not
// to its stdout or nowhere.
func TestTheLifecycleACommandIsGivenWritesToTheEnvsStderr(t *testing.T) {
	t.Parallel()

	var (
		stdout, stderr bytes.Buffer
		given          *Lifecycle
	)

	env := Env{Stdout: &stdout, Stderr: &stderr}

	tree := func(lc *Lifecycle) []Command {
		return []Command{{Name: "server", Run: func(context.Context, []string) error {
			given = lc

			return nil
		}}}
	}

	if err := Run([]string{"server"}, env, tree, func(int) {}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if given == nil || given.stderr != env.Stderr {
		t.Fatal("the lifecycle the command was given does not write to the env's stderr")
	}
}

// A COMMAND IS GIVEN THE ARGUMENTS AFTER ITS NAME, and a context the first
// signal cancels.
func TestACommandIsGivenItsArguments(t *testing.T) {
	t.Parallel()

	var got []string

	tree := func(*Lifecycle) []Command {
		return []Command{{Name: "ca", Run: func(ctx context.Context, args []string) error {
			if ctx.Err() != nil {
				return errors.New("the command's context was already cancelled")
			}

			got = args

			return nil
		}}}
	}

	if err := Run([]string{"ca", "issue", "epyc-1"}, Env{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}},
		tree, func(int) {}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if strings.Join(got, " ") != "issue epyc-1" {
		t.Errorf("the command was given %q, want [issue epyc-1]", got)
	}
}
