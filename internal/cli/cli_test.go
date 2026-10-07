package cli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"strings"
	"testing"
)

// testTree is a tree of commands that answer as told.
func testTree(answers map[string]error) Tree {
	return func(*Lifecycle) []Command {
		var out []Command

		for name, err := range answers {
			out = append(out, Command{Name: name, Summary: "answers " + name,
				Run: func(context.Context, []string) error { return err }})
		}

		return out
	}
}

// WHAT THE PROCESS EXITS WITH, AND WHAT IT SAYS. A command's success and an
// explicit request for help are 0; a status a command answers with is that
// status, printed unless it is quiet (a child's, whose output already went
// through); anything else is 1, printed; and no command, or one the tree does
// not have, is 1 with the usage.
func TestMainAnswersWithTheStatusACommandGives(t *testing.T) {
	t.Parallel()

	tree := testTree(map[string]error{
		"ok":        nil,
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
		{nil, 1, "no command given"},
		{[]string{"nope"}, 1, `unknown command "nope"`},
	} {
		var stdout, stderr bytes.Buffer

		env := Env{Stdout: &stdout, Stderr: &stderr}

		exited := false

		code := Main(tc.args, env, tree, func(int) { exited = true })

		if code != tc.code || exited {
			t.Errorf("Main(%q) = %d (exited by signal: %v), want %d", tc.args, code, exited, tc.code)
		}

		if tc.stderr == "" && stderr.Len() != 0 {
			t.Errorf("Main(%q) printed %q, want nothing", tc.args, stderr.String())
		}

		if tc.stderr != "" && !strings.Contains(stderr.String(), tc.stderr) {
			t.Errorf("Main(%q) printed %q, want it to contain %q", tc.args, stderr.String(), tc.stderr)
		}

		if stdout.Len() != 0 {
			t.Errorf("Main(%q) wrote to stdout: %q", tc.args, stdout.String())
		}
	}
}

// THE USAGE LISTS EVERY COMMAND, and is what -h and an unknown command print.
func TestTheUsageListsEveryCommand(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer

	if code := Main([]string{"-h"}, Env{Stdout: &bytes.Buffer{}, Stderr: &stderr},
		testTree(map[string]error{"server": nil, "node": nil}), func(int) {}); code != 0 {
		t.Fatalf("Main(-h) = %d, want 0", code)
	}

	for _, want := range []string{"usage: billet <command> [flags]", "server", "answers server", "node"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("the usage does not say %q:\n%s", want, stderr.String())
		}
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
