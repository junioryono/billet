package github

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func runEvidenceServer(t *testing.T, run, repository string) (*runnerGroupPolicyClient, *atomic.Int32) {
	t.Helper()

	key, _ := testKeyPKCS1(t)
	var repositoryReads atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /app/installations/22/access_tokens", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"installation-secret","expires_at":%q}`,
			time.Now().Add(time.Hour).UTC().Format(time.RFC3339))
	})
	mux.HandleFunc("GET /repos/acme/api/actions/runs/31", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer installation-secret" {
			t.Errorf("Authorization = %q", got)
		}
		fmt.Fprint(w, run)
	})
	mux.HandleFunc("GET /repos/acme/api", func(w http.ResponseWriter, _ *http.Request) {
		repositoryReads.Add(1)
		fmt.Fprint(w, repository)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return newRunnerGroupPolicyClient(srv.Client(), srv.URL, OrganizationTarget("acme"), 11, 22, key),
		&repositoryReads
}

const pullRequestRun = `{"id":31,"event":"pull_request","head_branch":"feature",
	"head_repository":{"full_name":"acme/api"},"repository":{"full_name":"acme/api"},
	"pull_requests":[{"number":7,"base":{"ref":"main"}}],
	"path":".github/workflows/ci.yml","head_sha":"abc123",
	"referenced_workflows":[{"path":"acme/api/.github/workflows/build.yml@refs/heads/main","sha":"def456"}]}`

// THE RUN'S OWN BRANCH, EVENT AND HEAD REPOSITORY ARE READ AS GITHUB RECORDS
// THEM, pull requests included, because those are what prove which ref a job
// may write.
func TestAWorkflowRunIsReadAsGitHubRecordsIt(t *testing.T) {
	t.Parallel()

	c, _ := runEvidenceServer(t, pullRequestRun, `{}`)
	run, err := c.WorkflowRun(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("WorkflowRun: %v", err)
	}
	if run.ID != 31 || run.Event != "pull_request" || run.HeadBranch != "feature" ||
		run.HeadRepository != "acme/api" || run.Repository != "acme/api" ||
		len(run.PullRequests) != 1 || run.PullRequests[0] != (RunPullRequest{Number: 7, Base: "main"}) ||
		run.Path != ".github/workflows/ci.yml" || run.HeadSHA != "abc123" ||
		len(run.ReferencedWorkflows) != 1 || run.ReferencedWorkflows[0] != (ReferencedWorkflow{
		Path: "acme/api/.github/workflows/build.yml@refs/heads/main", SHA: "def456"}) {
		t.Fatalf("run = %+v", run)
	}
}

// AN INCOMPLETE ANSWER IS REFUSED, NEVER READ AS EMPTY. An absent head
// repository read as "" would compare unequal and refuse anyway, but an absent
// pull-request list read as empty would drop a base branch; every field is
// required so the rule cannot depend on which absence is harmless.
func TestAnIncompleteWorkflowRunIsRefused(t *testing.T) {
	t.Parallel()

	for name, body := range map[string]string{
		"no head repository": strings.Replace(pullRequestRun,
			`"head_repository":{"full_name":"acme/api"},`, "", 1),
		"no event":                           strings.Replace(pullRequestRun, `"event":"pull_request",`, "", 1),
		"no pull requests":                   strings.Replace(pullRequestRun, `"pull_requests"`, `"x"`, 1),
		"another run":                        strings.Replace(pullRequestRun, `"id":31`, `"id":32`, 1),
		"no path":                            strings.Replace(pullRequestRun, `"path":".github/workflows/ci.yml",`, "", 1),
		"no head commit":                     strings.Replace(pullRequestRun, `"head_sha":"abc123",`, "", 1),
		"a reusable workflow with no commit": strings.Replace(pullRequestRun, `,"sha":"def456"`, "", 1),
		"a pull request with no base": strings.Replace(pullRequestRun,
			`,"base":{"ref":"main"}`, "", 1),
	} {
		c, _ := runEvidenceServer(t, body, `{}`)
		if run, err := c.WorkflowRun(t.Context(), "acme", "api", 31); err == nil {
			t.Errorf("%s: accepted %+v", name, run)
		}
	}
}

// THE DEFAULT BRANCH IS ASKED EVERY TIME, because a rename between two
// questions must change the second answer.
func TestTheDefaultBranchIsNeverCached(t *testing.T) {
	t.Parallel()

	c, reads := runEvidenceServer(t, pullRequestRun, `{"default_branch":"main"}`)
	for range 2 {
		branch, err := c.DefaultBranch(t.Context(), "acme", "api")
		if err != nil || branch != "main" {
			t.Fatalf("DefaultBranch = %q, %v", branch, err)
		}
	}
	if reads.Load() != 2 {
		t.Errorf("repository reads = %d, want one per question", reads.Load())
	}
}

// A PATH SEGMENT FROM A MESSAGE CANNOT STEER THE REQUEST ELSEWHERE.
func TestARepositorySegmentThatIsNotOneIsRefused(t *testing.T) {
	t.Parallel()

	c, _ := runEvidenceServer(t, pullRequestRun, `{"default_branch":"main"}`)
	for _, repository := range []string{"", "..", "api/../other", "api?x=1", "api%2f"} {
		if _, err := c.DefaultBranch(t.Context(), "acme", repository); err == nil {
			t.Errorf("repository %q was requested", repository)
		}
	}
}
