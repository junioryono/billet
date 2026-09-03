package hostupgrade

import (
	"errors"
	"slices"
	"testing"
)

// externalJournal is a journal for a host whose ledger billet does not hold.
func externalJournal(t *testing.T) *Journal {
	t.Helper()

	j := newJournal(t)
	j.Ledger = LedgerExternal

	if err := j.Write(); err != nil {
		t.Fatalf("write the journal: %v", err)
	}

	return j
}

// AN EXTERNAL LEDGER IS NEITHER FENCED, SNAPSHOTTED NOR MIGRATED BY THE UPDATER,
// and every other step runs in its usual place. billet copies no PostgreSQL
// database; the candidate migrates when it takes the controller claim, which
// happens when it is started. The three absent steps are exactly these, and a
// fourth absence would be a step this sequence silently lost.
func TestAnExternalLedgerUpgradeSkipsExactlyTheLedgerSteps(t *testing.T) {
	j := externalJournal(t)
	h := &fakeHost{}

	if err := run(t, j, h); err != nil {
		t.Fatalf("a healthy external-ledger upgrade failed: %v", err)
	}

	want := []string{
		"preserve",
		"stop-node",
		"refresh-guest-images",
		"stop-server",
		"hide-binary",
		"install-candidate",
		"probe",
		"record-installed",
		"start-services",
		"prove-stable",
	}

	if !slices.Equal(h.did, want) {
		t.Errorf("the upgrade ran\n  %v\nwant\n  %v", h.did, want)
	}

	// AND THE JOURNAL STILL RECORDS THE WHOLE SEQUENCE AS REACHED, so a resume
	// reads the same record the run wrote rather than a shape with holes in it.
	if j.Step != StepCommitted {
		t.Errorf("the journal records %s, want committed", j.Step)
	}

	for _, step := range []Step{StepFenced, StepSnapshotted, StepMigrated} {
		if !j.Reached(step) {
			t.Errorf("the committed journal does not record %s as reached", step)
		}
	}
}

// A FAILED EXTERNAL-LEDGER UPGRADE RESTORES THE BINARY AND NOTHING ELSE. There
// is no snapshot to put back and no fence to clear; the rollback boundary is
// the candidate's start, and a rollback that reached for a snapshot nothing took
// would cordon the host over a file that was never meant to exist.
func TestAFailedExternalLedgerUpgradeRestoresOnlyTheBinary(t *testing.T) {
	j := externalJournal(t)
	h := &fakeHost{failAt: "probe"}

	err := run(t, j, h)
	if !errors.Is(err, ErrRolledBack) {
		t.Fatalf("a failed probe on an external ledger did not roll back: %v", err)
	}

	for _, forbidden := range []string{"restore-ledger", "clear-fence", "fence", "snapshot",
		"migrate"} {
		if slices.Contains(h.did, forbidden) {
			t.Errorf("an external-ledger rollback ran %q: %v", forbidden, h.did)
		}
	}

	for _, wanted := range []string{"restore-preserved", "start-services", "prove-stable"} {
		if !slices.Contains(h.did, wanted) {
			t.Errorf("an external-ledger rollback did not run %q: %v", wanted, h.did)
		}
	}

	if j.Step != StepRolledBack {
		t.Errorf("the journal records %s, want rolled_back", j.Step)
	}
}

// THE SHAPE SURVIVES A RESUME. A journal written by an external-ledger run and
// resumed by a fresh process must skip the same steps, or the resume tries to
// restore a snapshot that was never taken.
func TestAnExternalLedgerJournalKeepsItsShapeAcrossAResume(t *testing.T) {
	j := externalJournal(t)

	read, err := ReadJournal(j.Dir)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}

	if !read.ExternalLedger() {
		t.Fatal("the journal read back without its external-ledger shape")
	}

	if newJournal(t).ExternalLedger() {
		t.Fatal("a journal for a local ledger reads as external")
	}
}
