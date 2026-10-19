package scripts_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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
		tmpdir  string // a TMPDIR of this name inside the repository, where git could find its config
		author  string
		owner   string // the repository's owner; empty is dcoOwner
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
			// A SIGNED MERGE OF MAIN INTO THE BRANCH passes, and main's own
			// commits are the base's, not the pull request's, so they are not asked.
			name:   "a signed-off merge of main inside the pull request",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				mergeMainInto(t, repo, "--signoff")
			},
		},
		{
			// AN UNSIGNED ONE IS REFUSED: a merge can carry new code or a
			// substantive conflict resolution, so it is a contribution too.
			name:   "an unsigned merge of main inside the pull request",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				mergeMainInto(t, repo)
			},
			code:  1,
			names: "Merge branch 'main' into pr",
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
			// GIT'S CONFIGURATION DOES NOT DECIDE WHAT A TRAILER IS: with "="
			// as the only separator, git would not read `git commit -s`'s line.
			name:   "a clone configured with another trailer separator",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				fixtureGit(t, repo, "config", "trailer.separators", "=")
				authored(t, repo, ada, "signed under odd configuration", adaSign)
			},
		},
		{
			// AN ALIAS CANNOT FORGE A SIGN-OFF: configured so, git would rewrite
			// "sign: ..." into the exact line the check looks for.
			name:   "a clone whose alias turns another line into a sign-off",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				fixtureGit(t, repo, "config", "trailer.sign.key", "Signed-off-by")
				authored(t, repo, ada, "forged by an alias", "sign: "+ada)
			},
			code:  1,
			names: "forged by an alias",
		},
		{
			// NOR FROM A TMPDIR INSIDE THE CHECKOUT, where git would walk up into
			// the repository and read the alias anyway.
			name:   "an alias with the temporary directory inside the repository",
			author: "ada",
			tmpdir: "tmp",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				fixtureGit(t, repo, "config", "trailer.sign.key", "Signed-off-by")
				authored(t, repo, ada, "forged from inside", "sign: "+ada)
			},
			code:  1,
			names: "forged from inside",
		},
		{
			// A COLON WOULD SPLIT THE CEILING, so such a TMPDIR is a refusal.
			name:   "a temporary directory whose path holds a colon",
			author: "ada",
			tmpdir: "tmp:inside",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				fixtureGit(t, repo, "config", "trailer.sign.key", "Signed-off-by")
				authored(t, repo, ada, "forged through a split ceiling", "sign: "+ada)
			},
			code:  2,
			names: "holds a colon",
		},
		{
			// A SIGNED COMMIT'S VERIFICATION REPORT IS NOT PART OF ITS MESSAGE:
			// with log.showSignature, git show prints it ahead of either read.
			name:   "a signed commit in a clone that shows signatures",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				sshSigned(t, repo)
				fixtureGit(t, repo, "config", "log.showSignature", "true")
				authored(t, repo, ada, "signed twice over", adaSign)
			},
		},
		{
			// GIT'S OUTPUT ENCODING DOES NOT DECIDE IT EITHER: UTF-16 would put a
			// NUL between every letter of a valid sign-off.
			name:   "a clone that logs in UTF-16",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				// NOT ASCII, so the NULs cannot fall out and leave the name intact.
				jose := "José Contributor <jose@example.invalid>"
				fixtureGit(t, repo, "config", "i18n.logOutputEncoding", "UTF-16LE")
				authored(t, repo, jose, "signed, logged in UTF-16", "Signed-off-by: "+jose)
			},
		},
		{
			// NOR HIDE ONE: mapped to another key, a real sign-off would vanish.
			name:   "a clone whose alias renames the sign-off",
			author: "ada",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				fixtureGit(t, repo, "config", "trailer.signed-off-by.key", "Approved-by")
				authored(t, repo, ada, "renamed by an alias", adaSign)
			},
		},
		{
			// THE OWNER OF THIS REPOSITORY, not of some repository: in a fork or
			// a repository another account owns, the same login is a contributor.
			name:   "the usual owner in a repository someone else owns",
			author: dcoOwner,
			owner:  "acme-forks",
			commits: func(t *testing.T, repo string) {
				t.Helper()

				authored(t, repo, ada, "unsigned in someone else's repository")
			},
			code:  1,
			names: "unsigned in someone else's repository",
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

			owner := tc.owner
			if owner == "" {
				owner = dcoOwner
			}

			var env []string
			if tc.tmpdir != "" {
				tmp := filepath.Join(repo, tc.tmpdir)
				if err := os.Mkdir(tmp, 0o700); err != nil {
					t.Fatal(err)
				}
				env = append(env, "TMPDIR="+tmp)
			}

			out, code := runDCO(t, repo, event, tc.author, owner, env...)
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

// A SHALLOW CLONE OR A BROKEN HISTORY IS A REFUSAL, never a pass over the
// commits it happens to show. The shallow case is the one a depth-2 checkout
// would make: a signed tip whose unsigned parent sits below the cut.
func TestTheDCOCheckRefusesAnIncompleteHistory(t *testing.T) {
	t.Parallel()

	build := func(t *testing.T, repo string) {
		t.Helper()

		authored(t, repo, ada, "unsigned, below the cut")
		authored(t, repo, ada, "signed tip", adaSign)
	}

	t.Run("a shallow clone", func(t *testing.T) {
		t.Parallel()

		full := prMergeRef(t, build)
		shallow := t.TempDir()
		fixtureGit(t, shallow, "clone", "--quiet", "--depth", "2", "file://"+full, ".")

		if out, code := runDCO(t, shallow, "pull_request", "ada", dcoOwner); code != 2 {
			t.Errorf("dco.sh exited %d on a depth-2 clone, want 2\n%s", code, out)
		}
	})

	t.Run("a commit whose object is missing", func(t *testing.T) {
		t.Parallel()

		repo := prMergeRef(t, build)
		unsigned := strings.TrimSpace(gitOut(t, repo, "rev-parse", "HEAD^2^"))
		if err := os.Remove(filepath.Join(repo, ".git", "objects", unsigned[:2], unsigned[2:])); err != nil {
			t.Fatalf("remove the unsigned commit's object: %v", err)
		}

		if out, code := runDCO(t, repo, "pull_request", "ada", dcoOwner); code != 2 {
			t.Errorf("dco.sh exited %d over a missing commit, want 2\n%s", code, out)
		}
	})
}

// THE REPAIR IT PRINTS WORKS, run exactly as printed, and changes nothing a
// merge into billet would produce. The contributor works in a clone of their own
// fork, whose main is stale; their branch has an unsigned commit and an unsigned
// merge of billet's main that carries a conflict resolution of its own; and
// billet's main then reverts a change the contributor merged in. A repair onto
// the fork's stale main would bring that reverted change back.
func TestTheRepairTheDCOCheckPrintsMakesTheBranchPass(t *testing.T) {
	t.Parallel()

	upstream := newRepo(t)
	write := func(repo, name, body string) {
		t.Helper()

		if err := os.WriteFile(filepath.Join(repo, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		fixtureGit(t, repo, "add", name)
	}

	write(upstream, "x", "0\n")
	write(upstream, "c", "base\n")
	fixtureGit(t, upstream, "commit", "--quiet", "-m", "base")

	// THE FORK STAYS AT base, and the contributor's origin is the fork.
	fork := filepath.Join(t.TempDir(), "fork.git")
	fixtureGit(t, upstream, "clone", "--quiet", "--bare", "file://"+upstream, fork)
	work := t.TempDir()
	fixtureGit(t, work, "clone", "--quiet", "file://"+fork, ".")
	fixtureGit(t, work, "config", "user.name", "Ada Contributor")
	fixtureGit(t, work, "config", "user.email", "ada@example.invalid")

	write(upstream, "x", "1\n")
	write(upstream, "c", "main\n")
	fixtureGit(t, upstream, "commit", "--quiet", "-m", "A: x to 1, c from main")

	fixtureGit(t, work, "checkout", "--quiet", "-b", "pr")
	write(work, "f", "ada's work\n")
	write(work, "c", "ada\n")
	fixtureGit(t, work, "commit", "--quiet", "-m", "unsigned work")

	// THE MERGE CARRIES CONTENT OF ITS OWN: c conflicts, and its resolution is
	// in no parent.
	fixtureGit(t, work, "fetch", "--quiet", "file://"+upstream, "main")
	merge := exec.CommandContext(t.Context(), "git", "merge", "--quiet", "--no-ff", "--no-edit", "FETCH_HEAD")
	merge.Dir = work
	merge.Env = fixtureGitEnv()
	if out, err := merge.CombinedOutput(); err == nil {
		t.Fatalf("the fixture's merge did not conflict:\n%s", out)
	}
	write(work, "c", "resolved by ada\n")
	fixtureGit(t, work, "commit", "--quiet", "--no-edit")
	fixtureGit(t, work, "push", "--quiet", "-u", "origin", "pr")

	write(upstream, "x", "0\n")
	fixtureGit(t, upstream, "commit", "--quiet", "-m", "B: revert x")

	// integration is the merge ref GitHub would test: billet's main with the
	// fork's pr merged in, and its tree.
	integration := func() (string, string) {
		t.Helper()

		ref := t.TempDir()
		fixtureGit(t, ref, "clone", "--quiet", "file://"+upstream, ".")
		fixtureGit(t, ref, "config", "user.name", "GitHub")
		fixtureGit(t, ref, "config", "user.email", "noreply@example.invalid")
		fixtureGit(t, ref, "fetch", "--quiet", "file://"+fork, "pr")
		fixtureGit(t, ref, "checkout", "--quiet", "--detach", "origin/main")
		fixtureGit(t, ref, "merge", "--quiet", "--no-ff", "--no-edit", "FETCH_HEAD")

		return ref, strings.TrimSpace(gitOut(t, ref, "rev-parse", "HEAD^{tree}"))
	}

	ref, before := integration()
	out, code := runDCO(t, ref, "pull_request", "ada", dcoOwner,
		"DCO_UPSTREAM=file://"+upstream, "DCO_BASE_REF=main")
	if code != 1 {
		t.Fatalf("the unrepaired branch: dco.sh exited %d, want 1\n%s", code, out)
	}

	var steps []string
	for _, line := range strings.Split(out, "\n") {
		if cmd, ok := strings.CutPrefix(line, "dco:   $ "); ok {
			steps = append(steps, cmd)
		}
	}
	if len(steps) == 0 {
		t.Fatalf("the refusal printed no repair:\n%s", out)
	}

	// THE PRINTED STEPS, verbatim, as the contributor would run them. The editor
	// gives `git commit -s` a subject above the sign-off it wrote.
	for _, step := range steps {
		cmd := exec.CommandContext(t.Context(), "bash", "-c", step)
		cmd.Dir = work
		cmd.Env = append(fixtureGitEnv(), "GIT_EDITOR=sed -i.bak 1s/^/ada-work/")
		if stepOut, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("the printed step %q failed: %v\n%s", step, err, stepOut)
		}
	}

	ref, after := integration()
	if after != before {
		t.Errorf("merging the repaired branch into billet gives tree %s, and the original gave %s", after, before)
	}
	if got := gitOut(t, ref, "show", "HEAD:x"); got != "0\n" {
		t.Errorf("after the repair the merge has x = %q; billet's revert of it was undone", got)
	}
	if got := gitOut(t, ref, "show", "HEAD:c"); got != "resolved by ada\n" {
		t.Errorf("after the repair the merge has c = %q; the merge's own resolution was lost", got)
	}
	if out, code := runDCO(t, ref, "pull_request", "ada", dcoOwner); code != 0 {
		t.Errorf("the repaired branch: dco.sh exited %d, want 0\n%s", code, out)
	}
}

// THE JOB CHECKS OUT EVERY COMMIT. A depth that is not a literal 0 leaves the
// range short (a bare 0 in an expression is falsy, so `&& 0 || 1` gives 1).
func TestTheDCOJobChecksOutTheWholeHistory(t *testing.T) {
	t.Parallel()

	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}

	var wf struct {
		Jobs map[string]struct {
			Steps []struct {
				Uses string         `yaml:"uses"`
				With map[string]any `yaml:"with"`
				Run  string         `yaml:"run"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}

	job, ok := wf.Jobs["dco"]
	if !ok {
		t.Fatal("ci.yml has no dco job")
	}

	checkedOut, ran := false, false
	for _, s := range job.Steps {
		if strings.HasPrefix(s.Uses, "actions/checkout@") {
			checkedOut = true
			if d, ok := s.With["fetch-depth"].(int); !ok || d != 0 {
				t.Errorf("the dco job checks out with fetch-depth %#v, want the literal 0", s.With["fetch-depth"])
			}
		}
		if s.Run == "scripts/dco.sh" {
			ran = checkedOut
		}
	}
	if !ran {
		t.Error("the dco job does not run scripts/dco.sh after a checkout")
	}
}

// mergeMainInto adds a signed commit to branch pr, moves main by a commit of its
// own, and merges main into pr as ada, with the merge options given.
func mergeMainInto(t *testing.T, repo string, opts ...string) {
	t.Helper()

	authored(t, repo, ada, "before the merge", adaSign)
	fixtureGit(t, repo, "checkout", "--quiet", "main")
	authored(t, repo, grace, "main moved, unsigned")
	fixtureGit(t, repo, "checkout", "--quiet", "pr")
	args := append([]string{"-c", "user.name=Ada Contributor", "-c", "user.email=ada@example.invalid",
		"merge", "--quiet", "--no-ff", "--no-edit"}, opts...)
	fixtureGit(t, repo, append(args, "main")...)

	// THE PREMISE, CHECKED: the merge is Ada's, so the sign-off it is asked for
	// is hers, and --signoff wrote it or did not as opts says.
	if got := strings.TrimSpace(gitOut(t, repo, "show", "-s", "--format=%an <%ae>|%cn <%ce>", "HEAD")); got != ada+"|"+ada {
		t.Fatalf("the merge's author and committer are %q, want Ada for both", got)
	}

	authored(t, repo, ada, "after the merge", adaSign)
}

// sshSigned makes repo sign every commit with a throwaway SSH key, the signing
// format that needs no GPG installation.
func sshSigned(t *testing.T, repo string) {
	t.Helper()

	key := filepath.Join(t.TempDir(), "id")
	gen := exec.CommandContext(t.Context(), "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key)
	if out, err := gen.CombinedOutput(); err != nil {
		// CI MUST RUN IT, or a green run would say nothing about signed commits.
		if os.Getenv("CI") != "" {
			t.Fatalf("ssh-keygen: %v\n%s", err, out)
		}
		t.Skipf("ssh-keygen is not usable here: %v\n%s", err, out)
	}

	fixtureGit(t, repo, "config", "gpg.format", "ssh")
	fixtureGit(t, repo, "config", "user.signingkey", key)
	fixtureGit(t, repo, "config", "commit.gpgsign", "true")
}

// gitOut is git's standard output in dir.
func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = fixtureGitEnv()

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}

	return string(out)
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
// opened by author in a repository owned by owner, with env added.
func runDCO(t *testing.T, repo, event, author, owner string, env ...string) (string, int) {
	t.Helper()

	script, err := filepath.Abs("dco.sh")
	if err != nil {
		t.Fatalf("absolute script path: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), script)
	cmd.Dir = repo
	cmd.Env = append(fixtureGitEnv(), "DCO_EVENT="+event, "DCO_PR_AUTHOR="+author, "DCO_OWNER="+owner)
	cmd.Env = append(cmd.Env, env...)

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
