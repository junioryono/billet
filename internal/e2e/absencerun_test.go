package e2e

import (
	"errors"
	"testing"
)

// AN ERROR IS NOT A MISS. The live cleanup accepts an absence only when it is
// sustained, and a lookup that failed observed nothing: eleven misses, an
// outage and one more miss must not read as twelve.
func TestAnAbsenceRunRestartsOnAnError(t *testing.T) {
	t.Parallel()

	run := 0
	for range 11 {
		run = absenceRun(run, nil, false, false)
	}
	if run != 11 {
		t.Fatalf("eleven misses = run of %d", run)
	}

	run = absenceRun(run, errors.New("throttled"), false, false)
	if run != 0 {
		t.Fatalf("an error left the run at %d; a lookup that failed observed nothing", run)
	}

	if got := absenceRun(absenceRun(run, nil, false, false), nil, true, false); got != 0 {
		t.Fatalf("a sighting left the run at %d", got)
	}
	if got := absenceRun(5, nil, true, true); got != 0 {
		t.Fatalf("a terminal record left the run at %d", got)
	}
}
