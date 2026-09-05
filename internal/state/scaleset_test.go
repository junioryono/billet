package state

import (
	"strings"
	"testing"
)

const testOrg = "acme"

func scaleSets(t *testing.T, db *DB) []ScaleSetRecord {
	t.Helper()

	got, err := db.ScaleSets(t.Context(), testOrg)
	if err != nil {
		t.Fatalf("ScaleSets: %v", err)
	}

	return got
}

// The whole point: what billet created survives the tier that made it, so the
// control plane can say what it no longer declares.
func TestARecordedScaleSetOutlivesItsTier(t *testing.T) {
	db := open(t)

	rec := ScaleSetRecord{Target: testOrg, RunnerGroup: "billet", Label: "billet-2vcpu", ID: 7}
	if err := db.RecordScaleSet(t.Context(), rec); err != nil {
		t.Fatalf("RecordScaleSet: %v", err)
	}

	got := scaleSets(t, db)
	if len(got) != 1 || got[0] != rec {
		t.Fatalf("ScaleSets = %v, want [%v]", got, rec)
	}
}

// THE SAME LABEL IN TWO GROUPS IS TWO SCALE SETS, which is why teardown reports
// "nothing in runner group X; if it was created under a different group it is
// still there". Keyed on the label alone, the second would overwrite the first
// and one of the two would become invisible.
func TestOneLabelInTwoRunnerGroupsIsTwoRecords(t *testing.T) {
	db := open(t)

	for i, group := range []string{"billet", "trusted"} {
		rec := ScaleSetRecord{Target: testOrg, RunnerGroup: group, Label: "billet-2vcpu", ID: 7 + i}
		if err := db.RecordScaleSet(t.Context(), rec); err != nil {
			t.Fatalf("RecordScaleSet(%s): %v", group, err)
		}
	}

	if got := scaleSets(t, db); len(got) != 2 {
		t.Fatalf("ScaleSets = %v, want both groups", got)
	}
}

// A SET THAT MOVED IS NOT A NEW SET. A runner group renamed, or a scale set
// moved between groups, keeps its id and arrives under a different key. Left
// alone, the row under the old name is reported as an orphan while its id names
// the set the config still declares — an operator sent to delete something that
// is in use.
func TestAScaleSetThatMovedRetiresItsOldName(t *testing.T) {
	db := open(t)

	first := ScaleSetRecord{Target: testOrg, RunnerGroup: "billet", Label: "billet-2vcpu", ID: 7}
	if err := db.RecordScaleSet(t.Context(), first); err != nil {
		t.Fatalf("RecordScaleSet: %v", err)
	}

	moved := first
	moved.RunnerGroup = "renamed"

	if err := db.RecordScaleSet(t.Context(), moved); err != nil {
		t.Fatalf("RecordScaleSet after the move: %v", err)
	}

	got := scaleSets(t, db)
	if len(got) != 1 {
		t.Fatalf("ScaleSets = %v, want the moved set once", got)
	}

	if got[0].RunnerGroup != "renamed" {
		t.Errorf("recorded group is %q, want where the set is now", got[0].RunnerGroup)
	}
}

// ...and the retirement is scoped to the organization, like everything else
// about a record.
func TestAnIDInAnotherOrganizationIsNotRetired(t *testing.T) {
	db := open(t)

	other := ScaleSetRecord{Target: "other", RunnerGroup: "billet", Label: "billet-2vcpu", ID: 7}
	if err := db.RecordScaleSet(t.Context(), other); err != nil {
		t.Fatalf("RecordScaleSet: %v", err)
	}

	mine := ScaleSetRecord{Target: testOrg, RunnerGroup: "elsewhere", Label: "billet-2vcpu", ID: 7}
	if err := db.RecordScaleSet(t.Context(), mine); err != nil {
		t.Fatalf("RecordScaleSet: %v", err)
	}

	got, err := db.ScaleSets(t.Context(), "other")
	if err != nil {
		t.Fatalf("ScaleSets: %v", err)
	}

	if len(got) != 1 || got[0].RunnerGroup != "billet" {
		t.Fatalf("another organization's record was retired by this one: %v", got)
	}
}

// The server reconciles every tier on every start, so this runs constantly
// against rows that already exist. A scale set deleted and recreated outside
// billet keeps its name and takes a NEW id, and reporting the stale one sends an
// operator looking for an object that is gone.
func TestRecordingAgainRefreshesTheID(t *testing.T) {
	db := open(t)

	for _, id := range []int{7, 9} {
		rec := ScaleSetRecord{Target: testOrg, RunnerGroup: "billet", Label: "billet-2vcpu", ID: id}
		if err := db.RecordScaleSet(t.Context(), rec); err != nil {
			t.Fatalf("RecordScaleSet(%d): %v", id, err)
		}
	}

	got := scaleSets(t, db)
	if len(got) != 1 {
		t.Fatalf("ScaleSets = %v, want one row", got)
	}

	if got[0].ID != 9 {
		t.Errorf("recorded id is %d, want the one from the latest reconcile", got[0].ID)
	}
}

// Forgetting is scoped to the group, for the same reason recording is.
func TestForgettingOneGroupLeavesTheOther(t *testing.T) {
	db := open(t)

	for _, group := range []string{"billet", "trusted"} {
		rec := ScaleSetRecord{Target: testOrg, RunnerGroup: group, Label: "billet-2vcpu", ID: 7}
		if err := db.RecordScaleSet(t.Context(), rec); err != nil {
			t.Fatalf("RecordScaleSet(%s): %v", group, err)
		}
	}

	if err := db.ForgetScaleSet(t.Context(), testOrg, "billet", "billet-2vcpu"); err != nil {
		t.Fatalf("ForgetScaleSet: %v", err)
	}

	got := scaleSets(t, db)
	if len(got) != 1 || got[0].RunnerGroup != "trusted" {
		t.Fatalf("ScaleSets = %v, want only the group that was not forgotten", got)
	}
}

// Teardown may run against a deployment whose ledger predates the record, and
// refusing there would fail a teardown GitHub has already completed.
func TestForgettingSomethingNeverRecordedIsNotAnError(t *testing.T) {
	db := open(t)

	if err := db.ForgetScaleSet(t.Context(), testOrg, "billet", "never-existed"); err != nil {
		t.Errorf("ForgetScaleSet on an absent row: %v", err)
	}
}

// An id GitHub never issued is a bug in the caller, and recording it would send
// an operator to an object that does not exist.
//
// The DIAGNOSTIC is what is asserted, not merely that it failed: the CHECK
// constraint refuses this too, so a test happy with any error passes with the
// caller's own guard deleted and the operator reads SQLite's constraint text
// rather than which label and which id.
func TestAnImpossibleScaleSetIDIsRefused(t *testing.T) {
	db := open(t)

	for _, id := range []int{0, -1} {
		err := db.RecordScaleSet(t.Context(), ScaleSetRecord{
			Target: testOrg, RunnerGroup: "billet", Label: "billet-2vcpu", ID: id,
		})
		if err == nil {
			t.Errorf("RecordScaleSet accepted id %d", id)

			continue
		}

		for _, want := range []string{"billet-2vcpu", "not one GitHub issued"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("RecordScaleSet(%d) = %v, want it to name %q", id, err, want)
			}
		}
	}

	if got := scaleSets(t, db); len(got) != 0 {
		t.Errorf("a refused record was written anyway: %v", got)
	}
}

// A label is equally unusable as a pointer to an object, and the message has to
// say which of the two inputs was wrong.
func TestRecordingWithNoLabelIsRefused(t *testing.T) {
	db := open(t)

	err := db.RecordScaleSet(t.Context(), ScaleSetRecord{Target: testOrg, RunnerGroup: "billet", ID: 7})
	if err == nil {
		t.Fatal("RecordScaleSet accepted a record with no label")
	}

	if !strings.Contains(err.Error(), "no label") {
		t.Errorf("RecordScaleSet = %v, want it to say the label is missing", err)
	}
}
