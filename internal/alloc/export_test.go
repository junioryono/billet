package alloc

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// A STAND-IN IS STABLE AND NAMES NOTHING: one name is one stand-in wherever it
// recurs, two names are two, nothing recorded survives into what is shared,
// and a job's matrix values get a stand-in of their own so its name without
// them is still one name.
func TestAnExportsStandInsAreStableAndNameNothing(t *testing.T) {
	s := newStandIns(false)

	type job struct{ repo, workflow, name string }

	jobs := []job{
		{"initech/ledger", "initech/ledger/.github/workflows/ci.yml@refs/heads/feature-x", "build (ubuntu, 1.22)"},
		{"initech/ledger", "initech/ledger/.github/workflows/ci.yml@refs/heads/main", "build (macos, 1.22)"},
		{"initech/ledger", "initech/ledger/.github/workflows/ci.yml@refs/heads/main", "lint"},
		{"initech/site", "initech/site/.github/workflows/deploy.yml@refs/tags/v2", "build (ubuntu, 1.22)"},
		{"otherco/tools", "otherco/tools/.github/workflows/ci.yml@refs/heads/main", "lint"},
	}

	type out struct{ owner, repository, workflow, name string }

	got := make([]out, len(jobs))
	for i, j := range jobs {
		got[i].owner, got[i].repository, got[i].workflow, got[i].name = s.of(j.repo, j.workflow, j.name)
	}

	want := []out{
		{"owner-1", "repo-1", "owner-1/repo-1/.github/workflows/workflow-1.yml", "job-1 (matrix-1)"},
		{"owner-1", "repo-1", "owner-1/repo-1/.github/workflows/workflow-1.yml", "job-1 (matrix-2)"},
		{"owner-1", "repo-1", "owner-1/repo-1/.github/workflows/workflow-1.yml", "job-2"},
		{"owner-1", "repo-2", "owner-1/repo-2/.github/workflows/workflow-2.yml", "job-3 (matrix-1)"},
		{"owner-2", "repo-3", "owner-2/repo-3/.github/workflows/workflow-3.yml", "job-4"},
	}

	for i := range want {
		if got[i] != want[i] {
			t.Errorf("job %d became %+v; want %+v", i, got[i], want[i])
		}
	}

	for _, g := range got {
		for _, secret := range []string{"initech", "ledger", "otherco", "tools", "site", "deploy", "ubuntu",
			"macos", "1.22", "feature-x", "refs", "lint", "build"} {
			for _, field := range []string{g.owner, g.repository, g.workflow, g.name} {
				if strings.Contains(field, secret) {
					t.Errorf("a stand-in %q carries the recorded %q", field, secret)
				}
			}
		}
	}

	// AN OWNER NOT RECORDED TAKES THE NEXT STAND-IN, never the trace's default.
	if o, _, w, _ := s.of("", "", ""); o != "owner-3" || !strings.HasPrefix(w, "owner-3/") {
		t.Errorf("an unrecorded owner became %q in %q; want owner-3", o, w)
	}

	// A RUN ID IS NUMBERED TOO, the jobs of one run still sharing one, and a run
	// not recorded taking the next number as its own.
	if a, b, c, none := s.run(39165193253), s.run(39165200000), s.run(39165193253), s.run(0); a != 1 || b != 2 ||
		c != 1 || none != 3 {
		t.Errorf("run ids became %d, %d, %d and %d; want 1, 2, 1 and 3 for one not recorded", a, b, c, none)
	}

	// THE MATRIX FALLBACK STILL WORKS: a stand-in name's base is one name.
	if base, ok := matrixBase(got[0].name); !ok || base != "job-1" {
		t.Errorf("a stand-in's base name is %q, %v; want job-1", base, ok)
	}

	// KEPT, NOTHING IS REPLACED but what was not recorded.
	kept := newStandIns(true)
	if id := kept.run(39165193253); id != 39165193253 {
		t.Errorf("with names kept a run id became %d", id)
	}

	if o, r, w, n := kept.of("acme/api", "acme/api/.github/workflows/ci.yml@refs/heads/main", "test (x)"); o != "acme" ||
		r != "api" || w != "acme/api/.github/workflows/ci.yml@refs/heads/main" || n != "test (x)" {
		t.Errorf("with names kept: %q %q %q %q", o, r, w, n)
	}

	// A RUN NOT RECORDED IS ITS OWN: a stand-in from the same count, never one
	// a recorded run has, and with names kept zero, which the trace numbers.
	fresh := newStandIns(false)
	if recorded, unknown, other := fresh.run(77), fresh.run(0), fresh.run(-3); recorded != 1 || unknown != 2 ||
		other != 3 {
		t.Errorf("runs 77, unrecorded and unrecorded became %d, %d and %d; want 1, 2 and 3", recorded, unknown,
			other)
	}
	if id := kept.run(0); id != 0 {
		t.Errorf("with names kept an unrecorded run became %d, want 0", id)
	}
	if id := kept.run(-3); id != 0 {
		t.Errorf("with names kept a run id of -3 became %d, want 0", id)
	}
	if a, b := fresh.run(0), fresh.run(0); a == b {
		t.Errorf("two unrecorded runs share stand-in %d; want each its own", a)
	}

	if _, r, w, _ := kept.of("", "", ""); r != unrecorded || !strings.Contains(w, unrecorded) {
		t.Errorf("a row with nothing recorded became repository %q, workflow %q; want the %q stand-in", r, w,
			unrecorded)
	}
}

// A TIME THAT DOES NOT READ IS AN ERROR, never a time not recorded.
func TestAnExportsTimesAreReadStrictly(t *testing.T) {
	if got, err := strictStamp(""); err != nil || !got.IsZero() {
		t.Errorf("an empty time read as %v, %v; want not recorded", got, err)
	}

	if got, err := strictStamp("2026-10-20T09:00:00.000000000Z"); err != nil || got.IsZero() {
		t.Errorf("a ledger time read as %v, %v", got, err)
	}

	if _, err := strictStamp("yesterday"); err == nil || !strings.Contains(err.Error(), `"yesterday"`) {
		t.Errorf("an unreadable time gave %v; want an error naming it", err)
	}
}

// A TIME THE LEDGER HOLDS THAT DOES NOT READ FAILS THE EXPORT, naming the lease
// and the column, never a job dropped or a time read as not recorded.
func TestAnExportRefusesATimeItCannotRead(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	a := runtimeFleet(t, &now)
	lease := ranJob(t, a, &now, "acme/api", "build", now.Add(time.Minute), 3*time.Minute)

	ctx := t.Context()
	if err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE job_history SET started_at = 'yesterday' WHERE lease_id = $1`, lease)

		return err
	}); err != nil {
		t.Fatalf("write an unreadable start: %v", err)
	}

	_, _, err := a.ExportTrace(ctx, now.Add(-time.Hour), now.Add(time.Hour), false)
	if err == nil || !strings.Contains(err.Error(), lease) || !strings.Contains(err.Error(), "started_at") {
		t.Errorf("an unreadable start gave %v; want an error naming lease %s and started_at", err, lease)
	}
}
