package scripts_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE TREE'S RELEASE IS THE NEAREST ONE ON ITS FIRST-PARENT CHAIN, or main.
//
// Each case is a repository shaped the way cut-release.yml leaves one: a release
// branch whose own history passes through its tagged `Release vX.Y.Z` commits,
// and a main that passes through none of them, even after a hotfix is merged
// back. Asserted on the answer, which is what two checks compare a tree against.
func TestTheReleaseRefIsTheNearestReleaseOnTheFirstParentChain(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		build func(t *testing.T, repo string)
		want  string
	}{
		{
			name: "a tree no release was cut from",
			build: func(t *testing.T, repo string) {
				t.Helper()

				commit(t, repo, "one")
				commit(t, repo, "two")
			},
			want: "main",
		},
		{
			name: "a lightweight release tag behind HEAD",
			build: func(t *testing.T, repo string) {
				t.Helper()

				commit(t, repo, "Release v0.4.0")
				fixtureGit(t, repo, "tag", "v0.4.0")
				commit(t, repo, "a carry")
			},
			want: "v0.4.0",
		},
		{
			// cut-release.yml writes annotated tags, whose object is not the commit.
			name: "an annotated release tag behind HEAD",
			build: func(t *testing.T, repo string) {
				t.Helper()

				commit(t, repo, "Release v0.4.0")
				fixtureGit(t, repo, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
				commit(t, repo, "a carry")
			},
			want: "v0.4.0",
		},
		{
			name: "several patches on a release branch",
			build: func(t *testing.T, repo string) {
				t.Helper()

				commit(t, repo, "Release v0.4.0")
				fixtureGit(t, repo, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
				commit(t, repo, "a carry")
				commit(t, repo, "Release v0.4.1")
				fixtureGit(t, repo, "tag", "-a", "v0.4.1", "-m", "v0.4.1")
				commit(t, repo, "another carry")
			},
			want: "v0.4.1",
		},
		{
			// BY NUMBER, NOT TEXT: v0.4.10 sorts before v0.4.9 as a string.
			name: "two releases on one commit",
			build: func(t *testing.T, repo string) {
				t.Helper()

				commit(t, repo, "Release")
				fixtureGit(t, repo, "tag", "v0.4.10")
				fixtureGit(t, repo, "tag", "v0.4.9")
			},
			want: "v0.4.10",
		},
		{
			// NONE OF THESE IS A RELEASE: the moving major, a prerelease, a guest
			// image, a name that is not a version, a leading zero, and a tag whose
			// dots are not dots (what an unescaped pattern would admit).
			name: "tags that are not releases on nearer commits",
			build: func(t *testing.T, repo string) {
				t.Helper()

				commit(t, repo, "Release v0.4.0")
				fixtureGit(t, repo, "tag", "v0.4.0")
				for _, tag := range []string{"v0", "v0.5.0-rc.1", "guest-20260101-000000",
					"release-4", "v0.05.0", "v0x5x0"} {
					commit(t, repo, tag)
					fixtureGit(t, repo, "tag", tag)
				}

				// A RELEASE NAME ON A TREE names no commit, so it is no release.
				commit(t, repo, "a tagged tree")
				fixtureGit(t, repo, "tag", "v0.9.0", "HEAD^{tree}")
			},
			want: "v0.4.0",
		},
		{
			// A TAG OF A TAG, which %(*objectname) would peel only as far as the
			// inner tag object, so no commit would ever match it.
			name: "a release tag on another tag",
			build: func(t *testing.T, repo string) {
				t.Helper()

				commit(t, repo, "Release v0.4.0")
				fixtureGit(t, repo, "tag", "-a", "inner", "-m", "inner")
				fixtureGit(t, repo, "tag", "-a", "v0.4.0", "-m", "v0.4.0", "inner")
				commit(t, repo, "a carry")
			},
			want: "v0.4.0",
		},
		{
			// THE CASE FIRST-PARENT EXISTS FOR. A hotfix merged back into main
			// makes the release tag reachable from main; main's tree still says
			// main, and so must the answer.
			name: "a release branch merged back into main",
			build: func(t *testing.T, repo string) {
				t.Helper()

				commit(t, repo, "main before the cut")
				fixtureGit(t, repo, "checkout", "--quiet", "-b", "release/v0.4")
				commit(t, repo, "Release v0.4.0")
				fixtureGit(t, repo, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
				fixtureGit(t, repo, "checkout", "--quiet", "main")
				commit(t, repo, "main after the cut")
				fixtureGit(t, repo, "merge", "--quiet", "--no-ff", "-m", "Merge the hotfix back",
					"release/v0.4")
			},
			want: "main",
		},
		{
			// AND A PULL REQUEST INTO A RELEASE BRANCH, whose merge commit's first
			// parent is the branch: what CI checks out for a carry.
			name: "a pull request merged into a release branch",
			build: func(t *testing.T, repo string) {
				t.Helper()

				commit(t, repo, "main before the cut")
				fixtureGit(t, repo, "checkout", "--quiet", "-b", "release/v0.4")
				commit(t, repo, "Release v0.4.0")
				fixtureGit(t, repo, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
				fixtureGit(t, repo, "checkout", "--quiet", "-b", "carry")
				commit(t, repo, "the carry")
				fixtureGit(t, repo, "checkout", "--quiet", "release/v0.4")
				fixtureGit(t, repo, "merge", "--quiet", "--no-ff", "-m", "Merge the carry", "carry")
			},
			want: "v0.4.0",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t)
			tc.build(t, repo)

			got, stderr, err := releaseRef(t, repo)
			if err != nil {
				t.Fatalf("release-ref.sh refused: %v\n%s", err, stderr)
			}

			if got != tc.want+"\n" {
				t.Errorf("release-ref.sh answered %q, want %q", got, tc.want+"\n")
			}
		})
	}
}

// COULD NOT TELL IS NOT MAIN. A shallow clone's history can stop before the
// release commit it belongs to, and outside a repository there is no history at
// all; both refuse, saying why, rather than answering main.
func TestTheReleaseRefRefusesWhatItCannotTell(t *testing.T) {
	t.Parallel()

	t.Run("a shallow clone", func(t *testing.T) {
		t.Parallel()

		origin := newRepo(t)
		commit(t, origin, "Release v0.4.0")
		fixtureGit(t, origin, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
		commit(t, origin, "a carry")

		clone := filepath.Join(t.TempDir(), "clone")
		fixtureGit(t, origin, "clone", "--quiet", "--depth", "1", "file://"+origin, clone)

		got, stderr, err := releaseRef(t, clone)
		if err == nil {
			t.Fatalf("release-ref.sh answered %q from a shallow clone", got)
		}

		if !strings.Contains(stderr, "this clone is shallow") {
			t.Errorf("the refusal does not say the clone is shallow:\n%s", stderr)
		}

		if got != "" {
			t.Errorf("a refusal also printed an answer: %q", got)
		}
	})

	// FULL HISTORY IS NOT ALL THE TAGS. cut-release.yml commits `Release vX.Y.Z`
	// on the branch it tags, so a clone that reaches that commit without its tag
	// is missing a tag, and reading on past it would answer main, or an older
	// release when only the newest tag is gone.
	t.Run("a full clone fetched without tags", func(t *testing.T) {
		t.Parallel()

		origin := newRepo(t)
		commit(t, origin, "main before the cut")
		commit(t, origin, "Release v0.4.0")
		fixtureGit(t, origin, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
		commit(t, origin, "a carry")

		clone := filepath.Join(t.TempDir(), "clone")
		fixtureGit(t, origin, "clone", "--quiet", "--no-tags", "file://"+origin, clone)

		got, stderr, err := releaseRef(t, clone)
		if err == nil {
			t.Fatalf("release-ref.sh answered %q from a clone without its tags", got)
		}

		if !strings.Contains(stderr, "cut v0.4.0, and this clone has no v0.4.0 tag") {
			t.Errorf("the refusal does not name the missing tag:\n%s", stderr)
		}

		if got != "" {
			t.Errorf("a refusal also printed an answer: %q", got)
		}
	})

	t.Run("only the newest release's tag missing", func(t *testing.T) {
		t.Parallel()

		repo := newRepo(t)
		commit(t, repo, "Release v0.4.0")
		fixtureGit(t, repo, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
		commit(t, repo, "a carry")
		commit(t, repo, "Release v0.4.1")
		commit(t, repo, "another carry")

		got, stderr, err := releaseRef(t, repo)
		if err == nil {
			t.Fatalf("release-ref.sh answered %q past an untagged v0.4.1", got)
		}

		if !strings.Contains(stderr, "cut v0.4.1, and this clone has no v0.4.1 tag") {
			t.Errorf("the refusal does not name the missing tag:\n%s", stderr)
		}
	})

	// AN OLDER TAG ON THE RELEASE COMMIT IS NOT ITS TAG: the subject names the
	// release the commit was cut as, and that tag is the one that must be here.
	t.Run("the release's tag missing beside an older one on its commit", func(t *testing.T) {
		t.Parallel()

		repo := newRepo(t)
		commit(t, repo, "Release v0.4.10")
		fixtureGit(t, repo, "tag", "v0.4.9")
		commit(t, repo, "a carry")

		got, stderr, err := releaseRef(t, repo)
		if err == nil {
			t.Fatalf("release-ref.sh answered %q for a commit cut as v0.4.10 with only v0.4.9 on it", got)
		}

		if !strings.Contains(stderr, "cut v0.4.10, and this clone has no v0.4.10 tag") {
			t.Errorf("the refusal does not name the missing tag:\n%s", stderr)
		}
	})

	t.Run("no repository", func(t *testing.T) {
		t.Parallel()

		got, stderr, err := releaseRef(t, t.TempDir())
		if err == nil {
			t.Fatalf("release-ref.sh answered %q outside a repository", got)
		}

		if !strings.Contains(stderr, "not inside a git work tree") {
			t.Errorf("the refusal does not say there is no repository:\n%s", stderr)
		}
	})
}

// check-module-sources.sh TAKES THE TREE'S OWN REF when it is not told one, so a
// release branch's documentation passes on the release branch (#149) and main's
// on main; EXPECTED_REF, which cut-release.yml passes, still wins.
func TestTheModuleSourcesCheckExpectsTheTreesOwnRef(t *testing.T) {
	t.Parallel()

	checker, err := filepath.Abs("check-module-sources.sh")
	if err != nil {
		t.Fatalf("absolute checker path: %v", err)
	}

	for _, tc := range []struct {
		name string
		// documented is the ref the fixture's documentation names; tag, when set,
		// is the release tag on the commit that carries it.
		documented string
		tag        string
		expected   string
		wantOK     bool
		wantOutput string
	}{
		{name: "main on main", documented: "main", wantOK: true},
		{name: "a release on its own branch", documented: "v0.4.3", tag: "v0.4.3", wantOK: true},
		{name: "main on a release branch", documented: "main", tag: "v0.4.3",
			wantOutput: "expected ?ref=v0.4.3"},
		{name: "a release on main", documented: "v0.4.3",
			wantOutput: "expected ?ref=main"},
		{name: "EXPECTED_REF wins over the tree", documented: "v0.4.3", tag: "v0.4.3",
			expected: "main", wantOutput: "expected ?ref=main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			repo := newRepo(t)
			module := filepath.Join(repo, "terraform", "modules", "billet")
			fleet := filepath.Join(repo, "actions", "converge-fleet")

			for _, dir := range []string{module, fleet} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("create %s: %v", dir, err)
				}
			}

			files := map[string]string{
				filepath.Join(module, "versions.tf"): "terraform {}\n",
				filepath.Join(module, "README.md"): "  source = \"github.com/junioryono/billet//" +
					"terraform/modules/billet?ref=" + tc.documented + "\"\n",
				filepath.Join(fleet, "README.md"): "- uses: junioryono/billet/actions/converge-fleet@" +
					tc.documented + "\n",
			}

			for path, body := range files {
				if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
					t.Fatalf("write %s: %v", path, err)
				}
			}

			fixtureGit(t, repo, "add", "-A")
			commit(t, repo, "the documentation")

			if tc.tag != "" {
				fixtureGit(t, repo, "tag", "-a", tc.tag, "-m", tc.tag)
			}

			cmd := exec.CommandContext(t.Context(), checker)
			cmd.Dir = repo
			cmd.Env = append(fixtureGitEnv(), "EXPECTED_REF="+tc.expected)

			out, err := cmd.CombinedOutput()
			if (err == nil) != tc.wantOK {
				t.Fatalf("check-module-sources.sh error = %v, want success %t\n%s", err, tc.wantOK, out)
			}

			if tc.wantOutput != "" && !strings.Contains(string(out), tc.wantOutput) {
				t.Errorf("the refusal does not say %q:\n%s", tc.wantOutput, out)
			}
		})
	}
}

// THE FIXTURES DO NOT DEPEND ON THE DEVELOPER'S GIT. A global configuration whose
// hooks refuse every commit and which signs every commit and tag is installed
// for this test, proved hostile on a plain git, and the fixture helpers and the
// resolver still build and read a release branch through it. Not parallel:
// t.Setenv changes the process's environment.
func TestTheReleaseRefFixturesIgnoreTheDevelopersGitConfiguration(t *testing.T) {
	home := t.TempDir()
	hooks := filepath.Join(home, "hooks")

	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatalf("create the hooks directory: %v", err)
	}

	const refusal = "billet-hostile-hook refused this commit"

	for _, hook := range []string{"pre-commit", "commit-msg"} {
		body := "#!/bin/sh\necho '" + refusal + "' >&2\nexit 1\n"
		if err := forkSafeWriteFile(filepath.Join(hooks, hook), []byte(body), 0o755); err != nil {
			t.Fatalf("write the %s hook: %v", hook, err)
		}
	}

	// WHERE AN UNGUARDED GIT LOOKS: the global file under HOME, and the -c
	// settings a parent passes in GIT_CONFIG_PARAMETERS. The first is what
	// GIT_CONFIG_GLOBAL=/dev/null shuts out, the second what stripping GIT_* does.
	config := "[core]\n\thooksPath = " + hooks + "\n[commit]\n\tgpgsign = true\n[tag]\n\tgpgsign = true\n" +
		"[user]\n\tname = Hostile\n\temail = hostile@example.invalid\n"

	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte(config), 0o600); err != nil {
		t.Fatalf("write the hostile configuration: %v", err)
	}

	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	// An identity too, so the hook is the only thing this route can refuse with.
	parameters := "'core.hookspath'='" + hooks + "' 'user.name'='Hostile' " +
		"'user.email'='hostile@example.invalid'"

	t.Setenv("GIT_CONFIG_PARAMETERS", parameters)

	// HOSTILE, PROVED, EACH ROUTE ON ITS OWN: a plain git that reads only that
	// route initialises a repository and then cannot commit, refused by the hook.
	for _, route := range []struct {
		name string
		env  []string
	}{
		{"the global configuration under HOME", withoutEnv("GIT_CONFIG_PARAMETERS")},
		{"GIT_CONFIG_PARAMETERS", append(withoutEnv("GIT_CONFIG_PARAMETERS"),
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_PARAMETERS="+parameters)},
	} {
		plain := t.TempDir()

		initialise := exec.CommandContext(t.Context(), "git", "init", "--quiet")
		initialise.Dir = plain
		initialise.Env = route.env

		if out, err := initialise.CombinedOutput(); err != nil {
			t.Fatalf("a plain git init under %s failed, so the probe proves nothing: %v\n%s",
				route.name, err, out)
		}

		probe := exec.CommandContext(t.Context(), "git", "commit", "--quiet", "--allow-empty", "-m", "probe")
		probe.Dir = plain
		probe.Env = route.env

		out, err := probe.CombinedOutput()
		if err == nil || !strings.Contains(string(out), refusal) {
			t.Fatalf("%s did not make a plain commit fail in its hook (%v), so it proves "+
				"nothing\n%s", route.name, err, out)
		}
	}

	repo := newRepo(t)
	commit(t, repo, "Release v0.4.0")
	fixtureGit(t, repo, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
	commit(t, repo, "a carry")

	got, stderr, err := releaseRef(t, repo)
	if err != nil {
		t.Fatalf("release-ref.sh refused: %v\n%s", err, stderr)
	}

	if got != "v0.4.0\n" {
		t.Errorf("release-ref.sh answered %q, want %q", got, "v0.4.0\n")
	}
}

// releaseRef runs scripts/release-ref.sh in dir.
func releaseRef(t *testing.T, dir string) (string, string, error) {
	t.Helper()

	resolver, err := filepath.Abs("release-ref.sh")
	if err != nil {
		t.Fatalf("absolute resolver path: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), resolver)
	cmd.Dir = dir
	cmd.Env = fixtureGitEnv()

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()

	return stdout.String(), stderr.String(), err
}

// newRepo is an empty repository on a branch named main, with an identity for
// its commits and annotated tags and none of the developer's configuration.
func newRepo(t *testing.T) string {
	t.Helper()

	repo := t.TempDir()

	// --template= copies no template, so no hook a template carries is installed.
	fixtureGit(t, repo, "init", "--quiet", "--template=", "--initial-branch", "main")
	fixtureGit(t, repo, "config", "user.name", "Billet Test")
	fixtureGit(t, repo, "config", "user.email", "billet@example.invalid")

	return repo
}

// commit records an empty commit with the message given.
func commit(t *testing.T, repo, message string) {
	t.Helper()

	fixtureGit(t, repo, "commit", "--quiet", "--allow-empty", "-m", message)
}

// withoutEnv is this process's environment without the named variable.
func withoutEnv(name string) []string {
	env := make([]string, 0, len(os.Environ()))

	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, name+"=") {
			continue
		}

		env = append(env, kv)
	}

	return env
}

// fixtureGit runs git in a fixture repository, in fixtureGitEnv.
func fixtureGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = fixtureGitEnv()

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// fixtureGitEnv is the environment a fixture repository's git runs in, and the
// resolver under test with it: no inherited GIT_* variable (a GIT_DIR would point
// it at another repository), and neither the developer's global nor the system
// configuration, whose hooks, signing or includes can fail or change a commit.
func fixtureGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+3)

	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}

		env = append(env, kv)
	}

	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1")
}
