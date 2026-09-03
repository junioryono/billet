package main

import (
	"errors"
	"fmt"
	"os/exec"
	"testing"
)

// ONLY BILLET'S OWN EXIT CODES REACH THE PROCESS EXIT STATUS.
//
// `billet runner check` documents 2 as "a rebuild is due" and 3 as "GitHub is
// already refusing", and a monitor is expected to act on the difference. That only
// means anything if nothing else can produce those numbers.
//
// It could. Matching on an anonymous `interface{ ExitCode() int }` also matches
// *exec.ExitError — which every failed subprocess in this program produces — so a
// failing `rbd` made billet exit with rbd's status. Measured: a verify against a
// missing image exited 2, carrying rbd's, which reads as "your runner image is due
// to be rebuilt".
func TestOnlyBilletsOwnCodesBecomeAnExitStatus(t *testing.T) {
	t.Parallel()

	// A REAL ONE, produced the way the program produces them: by running something
	// that fails. A hand-built stub could implement the interface differently from
	// the type that actually turns up.
	subprocess := exec.CommandContext(t.Context(), "sh", "-c", "exit 2").Run()
	if subprocess == nil {
		t.Fatal("a command that exits 2 reported success")
	}

	wrapped := fmt.Errorf("ceph: unmap a device: %w", subprocess)

	if got := exitStatus(wrapped); got != 1 {
		t.Errorf("a failed subprocess sets billet's exit status to %d; that is a number "+
			"`billet runner check` gives its own meaning, and a monitor would act on it", got)
	}

	// AND BILLET'S OWN STILL DO, including through a wrap and a join, which is how
	// they actually travel: a verification joins its verdict with its cleanup.
	ours := errors.Join(fmt.Errorf("something else went wrong: %w", subprocess), errRunnerDue)

	if got := exitStatus(ours); got != 2 {
		t.Errorf("billet's own exit code came out as %d; errRunnerDue is documented as 2, and it "+
			"has to survive being joined with another error because that is how it travels", got)
	}
}
