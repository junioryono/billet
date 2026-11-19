package hostupgrade

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// A FAILURE THE JOURNAL CANNOT RECORD NAMES THE WAY OUT: the preserved release's
// own `host-upgrade --resume`, since the installed binary may be hidden by then
// (#802). Driven through Run, with the recovery directory gone as the probe fails,
// so the rollback's own write of the failure is what cannot happen.
func TestAFailureTheJournalCannotRecordNamesTheResume(t *testing.T) {
	j := newJournal(t)
	h := &fakeHost{failAt: "probe", onFail: func() {
		if err := os.RemoveAll(j.Dir); err != nil {
			t.Fatal(err)
		}
	}}

	err := Run(t.Context(), Request{Journal: j, Host: h, Log: quiet()})

	if !errors.Is(err, ErrCordoned) {
		t.Fatalf("an unrecordable failure returned %v, want ErrCordoned", err)
	}

	resume := filepath.Join(j.Dir, "preserved", "billet") + " host-upgrade --resume"
	if !strings.Contains(err.Error(), resume) {
		t.Fatalf("the cordon does not name %q:\n%v", resume, err)
	}
}

// A FULL DISK IS SAID AS THE ROOM TO FREE, and the cause and the cordon both survive
// for a caller that tells them apart.
func TestAFullDiskIsSaidAsTheRoomToFree(t *testing.T) {
	j := &Journal{Dir: "/var/lib/billet/upgrades/upgrade-1"}
	cause := errors.New("the probe failed")
	full := &fs.PathError{Op: "write", Path: j.Dir + "/journal.json.new", Err: syscall.ENOSPC}

	err := unrecorded(j, cause, full)

	for _, want := range []string{
		"the filesystem holding /var/lib/billet/upgrades/upgrade-1 is full",
		"Free room on it, then run `/var/lib/billet/upgrades/upgrade-1/preserved/billet host-upgrade --resume`",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the cordon lacks %q:\n%v", want, err)
		}
	}

	if !errors.Is(err, ErrCordoned) || !errors.Is(err, cause) || !errors.Is(err, syscall.ENOSPC) {
		t.Errorf("the cordon lost one of the cordon, the cause and the write's error: %v", err)
	}

	other := unrecorded(j, cause, errors.New("permission denied"))
	if strings.Contains(other.Error(), "is full") {
		t.Errorf("a failure that was not a full disk was said as one: %v", other)
	}
}
