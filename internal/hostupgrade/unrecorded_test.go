package hostupgrade

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
	"testing"
)

// A FAILURE THE JOURNAL CANNOT RECORD IS A CORDON MARKED UNRECORDED, so the host's
// command can say how the machine is recovered (#802). Driven through Run, with the
// recovery directory gone as the probe fails, so the rollback's own write of the
// failure is what cannot happen.
func TestAFailureTheJournalCannotRecordIsMarkedUnrecorded(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "probe", onFail: func() {
		if err := os.RemoveAll(j.Dir); err != nil {
			t.Fatal(err)
		}
	}}

	err := Run(t.Context(), Request{Journal: j, Host: h, Log: quiet()})

	if !errors.Is(err, ErrCordoned) || !errors.Is(err, ErrUnrecorded) {
		t.Fatalf("an unrecordable failure returned %v, want ErrCordoned and ErrUnrecorded", err)
	}
}

// THE CAUSE AND THE WRITE'S OWN ERROR BOTH SURVIVE, so a full disk is still ENOSPC to
// the caller that says to free room.
func TestAnUnrecordedCordonKeepsTheCauseAndTheWritesError(t *testing.T) {
	cause := errors.New("the probe failed")
	full := &fs.PathError{Op: "write", Path: "/var/lib/billet/upgrades/u/journal.json.new", Err: syscall.ENOSPC}

	err := unrecorded(cause, full)

	if !errors.Is(err, ErrCordoned) || !errors.Is(err, ErrUnrecorded) || !errors.Is(err, cause) ||
		!errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("the cordon lost one of the cordon, the mark, the cause and the write's error: %v", err)
	}

	// A RECORDED CORDON IS NOT MARKED, so the recovery steps are said only when they apply.
	recorded := cordonWithoutRestoring(Request{Journal: newJournal(t)}, quiet(), cause)
	if !errors.Is(recorded, ErrCordoned) || errors.Is(recorded, ErrUnrecorded) {
		t.Fatalf("a cordon the journal recorded returned %v, want ErrCordoned without ErrUnrecorded", recorded)
	}
}
