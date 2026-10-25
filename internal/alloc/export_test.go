package alloc

import (
	"strings"
	"testing"
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

	// THE MATRIX FALLBACK STILL WORKS: a stand-in name's base is one name.
	if base, ok := matrixBase(got[0].name); !ok || base != "job-1" {
		t.Errorf("a stand-in's base name is %q, %v; want job-1", base, ok)
	}

	// KEPT, NOTHING IS REPLACED but what was not recorded.
	kept := newStandIns(true)
	if o, r, w, n := kept.of("acme/api", "acme/api/.github/workflows/ci.yml@refs/heads/main", "test (x)"); o != "acme" ||
		r != "api" || w != "acme/api/.github/workflows/ci.yml@refs/heads/main" || n != "test (x)" {
		t.Errorf("with names kept: %q %q %q %q", o, r, w, n)
	}

	if _, r, w, _ := kept.of("", "", ""); r != unrecorded || !strings.Contains(w, unrecorded) {
		t.Errorf("a row with nothing recorded became repository %q, workflow %q; want the %q stand-in", r, w,
			unrecorded)
	}
}
