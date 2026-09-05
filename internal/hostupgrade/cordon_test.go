package hostupgrade

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// A CANDIDATE THAT CANNOT BE PROVED GONE CORDONS THE HOST AND RESTORES NOTHING.
//
// The probe step returns ErrUnsafeToRestore when a candidate it sent SIGKILL never
// exited: that process may still hold the ledger, and restoring the snapshot under
// it would corrupt the one copy the transaction exists to protect. So Run must
// record the failure, stop, and report a cordon, and neither restore may run. The
// fake records every step, which is what makes "nothing was restored" assertable.
func TestAProbeThatCannotBeProvedGoneCordonsWithoutRestoring(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{
		failAt:   "probe",
		failWith: fmt.Errorf("%w: server was sent SIGKILL and its exit never arrived", ErrUnsafeToRestore),
	}

	err := Run(t.Context(), Request{Journal: j, Host: h, Log: quiet()})

	if !errors.Is(err, ErrCordoned) {
		t.Fatalf("an unsafe probe failure did not cordon: %v", err)
	}

	if errors.Is(err, ErrRolledBack) {
		t.Fatalf("an unsafe probe failure was reported as a rollback: %v", err)
	}

	for _, step := range h.did {
		if strings.HasPrefix(step, "restore") || step == "start-services" {
			t.Fatalf("the transaction restored or restarted over a candidate that may still be "+
				"running: %v", h.did)
		}
	}

	if j.Failure == "" {
		t.Fatal("the journal does not record why the host was cordoned")
	}

	if j.Step == StepRolledBack {
		t.Fatal("the journal says rolled back when nothing was restored")
	}
}

// AN ORDINARY PROBE FAILURE STILL ROLLS BACK, so the cordon is the exception it is
// meant to be and not a new default.
func TestAnOrdinaryProbeFailureStillRollsBack(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "probe"}

	err := Run(t.Context(), Request{Journal: j, Host: h, Log: quiet()})

	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("a plain probe failure did not roll back: %v", err)
	}

	restored := false

	for _, step := range h.did {
		if strings.HasPrefix(step, "restore") {
			restored = true
		}
	}

	if !restored {
		t.Fatalf("a plain probe failure restored nothing: %v", h.did)
	}
}
