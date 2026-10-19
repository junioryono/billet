package scripts_test

import (
	"bytes"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	ada      = "Ada Contributor <ada@example.invalid>"
	adaSign  = "Signed-off-by: " + ada
	grace    = "Grace Other <grace@example.invalid>"
	dcoOwner = "junioryono"
)

// A CONTRIBUTOR'S COMMITS ARE SIGNED OFF BY THEIR OWN AUTHORS, OR REFUSED.
//
// Each case is a repository shaped as GitHub checks a pull request out: a merge
// ref whose first parent is the base and whose second is the pull request's
// head. Asserted on the exit status, which is what fails the job, and on the
// commit a refusal names, so a refusal for the wrong commit cannot pass.
func TestAContributorsCommitsMustBeSignedOffByTheirAuthors(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		event   string
		author  string
		commits func(t *testing.T, repo string)
		code    int
		names   string
		// passed is a commit the refusal must not name, so a refusal that
		// lists every commit cannot pass for one that lists the right one.
		passed string
	}{
		{
			name:   "every commit signed off by its author",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, ada, "one", adaSign)
				authored(t, repo, ada, "two", adaSign)
			},
		},
		{
			name:   "one commit of two unsigned",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, ada, "signed", adaSign)
				authored(t, repo, ada, "forgot the sign-off")
			},
			code:   1,
			names:  "forgot the sign-off",
			passed: "signed",
		},
		{
			// THE SIGN-OFF MUST BE THE AUTHOR'S: one person cannot certify
			// another's work by signing it.
			name:   "a sign-off naming someone other than the author",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, grace, "signed by the wrong person", adaSign)
			},
			code:  1,
			names: "signed by the wrong person",
		},
		{
			// A TRAILER, NOT A MENTION: the line counts only in the message's
			// closing trailer block, where git commit -s writes it.
			name:   "a sign-off line in the middle of the message",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, ada, "mentioned, not signed", adaSign+"\nand then more prose", "the end")
			},
			code:  1,
			names: "mentioned, not signed",
		},
		{
			// A MERGE OF MAIN INTO THE BRANCH adds no authorship of its own, and
			// main's commits are the base's, not the pull request's.
			name:   "a merge of main inside the pull request",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, ada, "before the merge", adaSign)
				fixtureGit(t, repo, "checkout", "--quiet", "main")
				authored(t, repo, grace, "main moved, unsigned")
				fixtureGit(t, repo, "checkout", "--quiet", "pr")
				fixtureGit(t, repo, "merge", "--quiet", "--no-ff", "--no-edit", "main")
				authored(t, repo, ada, "after the merge", adaSign)
			},
		},
		{
			name:   "the repository owner's pull request",
			author: dcoOwner,
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, ada, "unsigned")
			},
		},
		{
			name:   "a bot's pull request",
			author: "dependabot[bot]",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, ada, "unsigned")
			},
		},
		{
			// THE EXEMPTION IS BY LOGIN, so a commit claiming the owner's name
			// in a contributor's pull request is still checked.
			name:   "a contributor's commit authored as the owner",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, "junioryono <owner@example.invalid>", "claims to be the owner")
			},
			code:  1,
			names: "claims to be the owner",
		},
		{
			name:  "a push",
			event: "push",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, ada, "unsigned")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := prMergeRef(t, tc.commits)

			event := tc.event
			if event == "" {
				event = "pull_request"
			}

			out, code := runDCO(t, repo, event, tc.author, dcoOwner)
			if code != tc.code {
				t.Fatalf("dco.sh exited %d, want %d\n%s", code, tc.code, out)
			}
			if tc.names != "" && !strings.Contains(out, tc.names) {
				t.Errorf("dco.sh refused without naming %q:\n%s", tc.names, out)
			}
			if tc.passed != "" && strings.Contains(out, " "+tc.passed+" has no") {
				t.Errorf("dco.sh named %q, which is signed off:\n%s", tc.passed, out)
			}
		})
	}
}

// COULD NOT TELL IS A REFUSAL. A checkout that is not a merge ref, or a pull
// request run missing who opened it, leaves the commits to check, or whether to
// check them, unknown; passing it would be a green check over unread commits.
func TestTheDCOCheckRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	t.Run("a checkout that is not a merge ref", func(t *testing.T) {
		t.Parallel()

		repo := newRepo(t)
		authored(t, repo, ada, "base")
		authored(t, repo, ada, "unsigned")

		if out, code := runDCO(t, repo, "pull_request", "ada", dcoOwner); code != 2 {
			t.Errorf("dco.sh exited %d on a linear HEAD, want 2\n%s", code, out)
		}
	})

	t.Run("a pull request with no author", func(t *testing.T) {
		t.Parallel()

		repo := prMergeRef(t, func(t *testing.T, repo string) {
			t.Helper()

			authored(t, repo, ada, "unsigned")
		})

		if out, code := runDCO(t, repo, "pull_request", "", dcoOwner); code != 2 {
			t.Errorf("dco.sh exited %d with no pull request author, want 2\n%s", code, out)
		}
	})
}

// prMergeRef is a repository whose HEAD is a pull request's merge ref: a base
// commit on main, the commits build adds on branch pr, and a detached merge of
// pr into main, as actions/checkout leaves a pull_request run.
func prMergeRef(t *testing.T, build func(t *testing.T, repo string)) string {
	t.Helper()

	repo := newRepo(t)
	authored(t, repo, grace, "base")
	fixtureGit(t, repo, "checkout", "--quiet", "-b", "pr")
	build(t, repo)
	fixtureGit(t, repo, "checkout", "--quiet", "--detach", "main")
	fixtureGit(t, repo, "merge", "--quiet", "--no-ff", "--no-edit", "pr")

	return repo
}

// authored records an empty commit by author, with the subject and any further
// paragraphs given.
func authored(t *testing.T, repo, author, subject string, paragraphs ...string) {
	t.Helper()

	args := []string{"commit", "--quiet", "--allow-empty", "--author", author, "-m", subject}
	for _, p := range paragraphs {
		args = append(args, "-m", p)
	}

	fixtureGit(t, repo, args...)
}

// runDCO runs scripts/dco.sh in repo as a CI run of event, for a pull request
// opened by author in a repository owned by owner.
func runDCO(t *testing.T, repo, event, author, owner string) (string, int) {
	t.Helper()

	script, err := filepath.Abs("dco.sh")
	if err != nil {
		t.Fatalf("absolute script path: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), script)
	cmd.Dir = repo
	cmd.Env = append(fixtureGitEnv(), "DCO_EVENT="+event, "DCO_PR_AUTHOR="+author, "DCO_OWNER="+owner)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	err = cmd.Run()

	var exit *exec.ExitError
	switch {
	case err == nil:
		return out.String(), 0
	case errors.As(err, &exit):
		return out.String(), exit.ExitCode()
	default:
		t.Fatalf("dco.sh did not run: %v", err)

		return "", -1
	}
}
