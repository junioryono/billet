package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/retirement"
)

// A HOST WHOSE AUTHORITY A RETIREMENT CLOSED EXITS 6, wherever the refusal was
// met. The retirement reads that status back from a timer unit to recognise the
// one backup its own stop interrupted, so a command that met the refusal inside
// a ledger open, wrapped twice on the way out, must still answer it; and an
// error that merely mentions a retirement must not.
func TestARetiringHostsRefusalExitsWithItsOwnStatus(t *testing.T) {
	t.Parallel()

	refused := fmt.Errorf("server state: %w", fmt.Errorf("open: %w", retirement.ErrRetiring{Phase: "stopped"}))

	if retirement.ExitRetiring != 6 {
		t.Fatalf("retirement.ExitRetiring is %d; the retirement reads 6 back from a unit's status", retirement.ExitRetiring)
	}

	if got := ExitStatus(refused); got != 6 {
		t.Errorf("a retiring host's refusal exits %d, want 6", got)
	}

	if got := ExitStatus(fmt.Errorf("the retirement journal is unreadable")); got != 1 {
		t.Errorf("an ordinary failure exits %d, want 1", got)
	}

	// AND THROUGH MAIN, printed once with its whole chain.
	var stdout, stderr bytes.Buffer

	tree := func(*Lifecycle) []Command {
		return []Command{{Name: "backup", Run: func(context.Context, Env, []string) error { return refused }}}
	}

	if code := Main([]string{"backup"}, Env{Stdout: &stdout, Stderr: &stderr}, tree, func(int) {}); code != 6 {
		t.Errorf("Main answered %d for a retiring host's refusal, want 6", code)
	}

	if want := "billet: " + refused.Error() + "\n"; stderr.String() != want {
		t.Errorf("Main printed %q, want %q", stderr.String(), want)
	}

	// JOINED WITH ANOTHER STATUS, in either order, loud or quiet, it still exits 6:
	// a refusal met beside a failure that has its own status is still the
	// refusal the transition reads back.
	for name, joined := range map[string]error{
		"after a status":     errors.Join(&ExitError{Code: 2, Msg: "another failure"}, refused),
		"before a status":    errors.Join(refused, &ExitError{Code: 2, Msg: "another failure"}),
		"beside a quiet one": errors.Join(&ExitError{Code: 4}, refused),
	} {
		if got := ExitStatus(joined); got != 6 {
			t.Errorf("a retiring host's refusal %s exits %d, want 6", name, got)
		}

		stderr.Reset()

		quiet := func(*Lifecycle) []Command {
			return []Command{{Name: "backup", Run: func(context.Context, Env, []string) error { return joined }}}
		}

		if code := Main([]string{"backup"}, Env{Stdout: &stdout, Stderr: &stderr}, quiet, func(int) {}); code != 6 {
			t.Errorf("Main answered %d for a retiring host's refusal %s, want 6", code, name)
		}

		if !strings.Contains(stderr.String(), "retirement") {
			t.Errorf("Main printed nothing of a retiring host's refusal %s: %q", name, stderr.String())
		}
	}
}
