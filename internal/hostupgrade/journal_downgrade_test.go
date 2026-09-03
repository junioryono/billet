package hostupgrade

import "testing"

// THE DOWNGRADE PERMISSION SURVIVES THE JOURNAL. A resumed transaction is built
// from the journal alone, and one that had forgotten the permission would have
// the older candidate refused by the ledger this same transaction had already
// snapshotted and was about to hand it.
func TestADowngradeJournalKeepsItsPermissionAcrossAResume(t *testing.T) {
	t.Parallel()

	j := newJournal(t)
	j.AllowDowngrade = true

	if err := j.Write(); err != nil {
		t.Fatalf("Write: %v", err)
	}

	read, err := ReadJournal(j.Dir)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}

	if !read.AllowDowngrade {
		t.Fatal("the journal read back without the downgrade permission it was written with")
	}

	plain := newJournal(t)

	read, err = ReadJournal(plain.Dir)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}

	if read.AllowDowngrade {
		t.Fatal("a journal written without the permission read back with it")
	}
}
