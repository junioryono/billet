package cli

import (
	"errors"

	"github.com/junioryono/billet/internal/retirement"
)

// ExitError is a failure that names the status the process exits with.
//
// MOST FAILURES ARE JUST FAILURES and exit 1. A few are ANSWERS rather than errors:
// `billet runner check` reporting that the runner image is due to be rebuilt is a
// fact a monitor acts on differently from "billet could not run", and differently
// again from "GitHub has already stopped queueing jobs". Collapsing those into one
// status means a cron entry cannot tell a task from an outage, and will end up
// treating both like whichever it saw first.
type ExitError struct {
	Code int
	Msg  string
	// Err is the cause when a status is being given to one, so `errors.Is` and
	// `errors.As` still reach it: a refusal that carries its own exit code is
	// still the refusal it was, and callers match on its type.
	Err error
}

func (e *ExitError) Error() string { return e.Msg }

func (e *ExitError) Unwrap() error { return e.Err }

// ExitCode is what the process should exit with.
func (e *ExitError) ExitCode() int { return e.Code }

// ExitStatus is what the process exits with for an error.
//
// THE CONCRETE TYPE, NOT AN ANONYMOUS INTERFACE. `interface{ ExitCode() int }` also
// matches *exec.ExitError, which every failed subprocess in this program produces —
// so `rbd` exiting 2 made BILLET exit 2, which is the status `billet runner check`
// documents as "the runner image is due to be rebuilt". A monitor reading that would
// act on a storage error as though it were a scheduled task. Measured: a verify
// against a missing image exited 2, carrying rbd's status.
//
// A FUNCTION RATHER THAN FOUR LINES IN main, because the first test written for this
// replicated the decision instead of exercising it, and passed against the very bug
// it described.
//
// AND ONE REFUSAL BY ITS TYPE, BEFORE ANY OTHER STATUS: a host whose authority a
// retirement has closed answers retirement.ExitRetiring, which the retirement
// reads back from a timer unit's status, wherever in the command the refusal was
// met and whatever it was joined with. Another status found first would hide it,
// and the one failure the transition admits would read as any other.
func ExitStatus(err error) int {
	if retiring(err) {
		return retirement.ExitRetiring
	}

	if coded, ok := errors.AsType[*ExitError](err); ok {
		return coded.Code
	}

	return 1
}

// retiring reports whether err's chain holds a retiring host's refusal.
func retiring(err error) bool {
	//nolint:errcheck // the match is the answer: the refusal is err itself, classified, not a second error
	_, ok := errors.AsType[retirement.ErrRetiring](err)

	return ok
}
