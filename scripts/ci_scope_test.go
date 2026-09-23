package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

var ciScopeScript = filepath.Join("..", "scripts", "ci-scope.sh")

// scopeRepo is a git repository whose HEAD is the merge commit a pull_request
// checkout would give: the base branch's tip, then a branch carrying change,
// merged with --no-ff.
func scopeRepo(t *testing.T, change func(t *testing.T, dir string)) string {
	t.Helper()

	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()

		cmd := exec.CommandContext(t.Context(), "git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, body string) {
		t.Helper()

		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	git("init", "-q", "-b", "main")
	write("cmd/billet/main.go", "package main\n")
	write("docs/index.md", "# billet\n")
	write("docs/run.md", "#!/bin/sh\n")
	if err := os.Chmod(filepath.Join(dir, "docs", "run.md"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("index.md", filepath.Join(dir, "docs", "link.md")); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-q", "-m", "base")
	git("checkout", "-q", "-b", "change")
	change(t, dir)
	git("add", "-A")
	git("commit", "-q", "--allow-empty", "-m", "change")
	git("checkout", "-q", "main")
	// The base moves on after the branch was cut, as it does on a busy repository,
	// so HEAD^1 is not the branch point.
	write("internal/other.go", "package other\n")
	// Only its own file: anything the change left on disk (an embedded repository
	// survives a checkout) must reach the merge through the change, not the base.
	git("add", "internal/other.go")
	git("commit", "-q", "-m", "base moves on")
	git("merge", "-q", "--no-ff", "-m", "merge", "change")

	return dir
}

func runScope(t *testing.T, dir, event string, env ...string) (string, string) {
	t.Helper()

	script, err := filepath.Abs(ciScopeScript)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", script)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "CI_EVENT="+event), env...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ci-scope.sh: %v\n%s", err, stderr.String())
	}

	return strings.TrimSpace(string(out)), stderr.String()
}

func TestCIScopeRunsOnlyDocsJobsForAChangeThatIsAllDocumentation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		change func(t *testing.T, dir string)
		event  string
		want   string
		reason string
	}{
		{
			name: "docs, markdown anywhere, skills and the licence",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "docs/deploying/mac-tart.md", "# Mac\n")
				mustWrite(t, dir, "docs/conf.py", "project = 'billet'\n")
				mustWrite(t, dir, ".claude/skills/x/SKILL.md", "---\nname: x\n---\n")
				mustWrite(t, dir, "CLAUDE.md", "# billet\n")
			},
			event: "pull_request", want: "docs", reason: "all 4 paths",
		},
		{
			name: "one code path among docs",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "docs/a.md", "a\n")
				mustWrite(t, dir, "cmd/billet/main.go", "package main // changed\n")
			},
			event: "pull_request", want: "full", reason: "touches cmd/billet/main.go",
		},
		{
			name: "code renamed into docs is still a code change",
			change: func(t *testing.T, dir string) {
				t.Helper()

				gitIn(t, dir, "mv", "cmd/billet/main.go", "docs/main.go.md")
			},
			event: "pull_request", want: "full", reason: "touches cmd/billet/main.go",
		},
		{
			name: "a docs-only change pushed rather than proposed",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "docs/a.md", "a\n")
			},
			event: "push", want: "full", reason: "is not a pull request",
		},
		{
			name: "LICENSE is a package input",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "LICENSE", "Apache\n")
			},
			event: "pull_request", want: "full", reason: "touches LICENSE",
		},
		{
			name: "the root README is a package input",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "README.md", "# billet\n")
			},
			event: "pull_request", want: "full", reason: "touches README.md",
		},
		{
			name: "Go source under docs is source",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "docs/helper.go", "package docs\n")
			},
			event: "pull_request", want: "full", reason: "touches docs/helper.go",
		},
		{
			name: "a script under .claude is not documentation",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, ".claude/skills/x/run.sh", "#!/bin/sh\n")
			},
			event: "pull_request", want: "full", reason: "touches .claude/skills/x/run.sh",
		},
		{
			name: "a skills symlink under .agents is not documentation",
			change: func(t *testing.T, dir string) {
				t.Helper()

				if err := os.MkdirAll(filepath.Join(dir, ".agents", "skills"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../../.claude/skills/x", filepath.Join(dir, ".agents", "skills", "x")); err != nil {
					t.Fatal(err)
				}
			},
			event: "pull_request", want: "full", reason: "touches .agents/skills/x",
		},
		{
			name: "a documentation path made executable",
			change: func(t *testing.T, dir string) {
				t.Helper()

				if err := os.Chmod(filepath.Join(dir, "docs", "index.md"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			event: "pull_request", want: "full", reason: "100644 -> 100755",
		},
		{
			name: "a documentation path replaced by a symlink",
			change: func(t *testing.T, dir string) {
				t.Helper()

				p := filepath.Join(dir, "docs", "index.md")
				if err := os.Remove(p); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("../cmd/billet/main.go", p); err != nil {
					t.Fatal(err)
				}
			},
			event: "pull_request", want: "full", reason: "100644 -> 120000",
		},
		{
			name: "an executable made a regular documentation file",
			change: func(t *testing.T, dir string) {
				t.Helper()

				// The base's docs/run.md is executable (see scopeRepo); this change only
				// clears the bit, so only the old side is not a regular file.
				if err := os.Chmod(filepath.Join(dir, "docs", "run.md"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			event: "pull_request", want: "full", reason: "100755 -> 100644",
		},
		{
			name: "a documentation-shaped symlink deleted",
			change: func(t *testing.T, dir string) {
				t.Helper()

				gitIn(t, dir, "rm", "-q", "docs/link.md")
			},
			event: "pull_request", want: "full", reason: "120000 -> 000000",
		},
		{
			name: "a submodule at a documentation-shaped path",
			change: func(t *testing.T, dir string) {
				t.Helper()

				// An embedded repository, which `git add` records as a gitlink (160000).
				sub := filepath.Join(dir, "docs", "sub.md")
				if err := os.MkdirAll(sub, 0o755); err != nil {
					t.Fatal(err)
				}
				gitIn(t, sub, "init", "-q")
				gitIn(t, sub, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "sub")
			},
			event: "pull_request", want: "full", reason: "000000 -> 160000",
		},
		{
			name: "a documentation file deleted",
			change: func(t *testing.T, dir string) {
				t.Helper()

				gitIn(t, dir, "rm", "-q", "docs/index.md")
			},
			event: "pull_request", want: "docs", reason: "all 1 paths",
		},
		{
			name:   "a change that lists no paths",
			change: func(*testing.T, string) {},
			event:  "pull_request", want: "full", reason: "lists no paths",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := scopeRepo(t, tc.change)
			got, stderr := runScope(t, dir, tc.event)
			if got != tc.want || !strings.Contains(stderr, tc.reason) {
				t.Fatalf("scope %q (%q), want %q with a reason naming %q", got, stderr, tc.want, tc.reason)
			}
		})
	}
}

func TestCIScopeIsFullWhenHEADIsNotAMergeCommit(t *testing.T) {
	t.Parallel()

	dir := scopeRepo(t, func(t *testing.T, dir string) {
		t.Helper()
		mustWrite(t, dir, "docs/a.md", "a\n")
	})
	gitIn(t, dir, "checkout", "-q", "HEAD^1")

	got, stderr := runScope(t, dir, "pull_request")
	if got != "full" || !strings.Contains(stderr, "not a merge commit") {
		t.Fatalf("scope %q (%q), want full because HEAD has one parent", got, stderr)
	}
}

func TestCIScopeIsFullWhenGitCannotAnswer(t *testing.T) {
	t.Parallel()

	// Not a repository at all: rev-parse fails, which must read as "full".
	got, stderr := runScope(t, t.TempDir(), "pull_request")
	if got != "full" {
		t.Fatalf("scope %q (%q) outside a repository, want full", got, stderr)
	}
}

func mustWrite(t *testing.T, dir, rel, body string) {
	t.Helper()

	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func gitIn(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// ciWorkflow is the part of .github/workflows/ci.yml the scope rules depend on.
type ciWorkflow struct {
	Jobs map[string]struct {
		Needs yaml.Node `yaml:"needs"`
		If    string    `yaml:"if"`
		Steps []struct {
			Name string            `yaml:"name"`
			Env  map[string]string `yaml:"env"`
			Run  string            `yaml:"run"`
		} `yaml:"steps"`
	} `yaml:"jobs"`
}

const scopeGate = "needs.changes.outputs.scope == 'full'"

func readCIWorkflow(t *testing.T) ciWorkflow {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var wf ciWorkflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		t.Fatal(err)
	}

	return wf
}

func jobNeeds(t *testing.T, n yaml.Node) []string {
	t.Helper()

	switch n.Kind {
	case 0:
		return nil
	case yaml.ScalarNode:
		return []string{n.Value}
	case yaml.SequenceNode:
		var out []string
		if err := n.Decode(&out); err != nil {
			t.Fatal(err)
		}

		return out
	default:
		t.Fatalf("needs is a %v node", n.Kind)

		return nil
	}
}

// verifyStep is verify-all-checks' one step: its script and its KEPT list.
func verifyStep(t *testing.T, wf ciWorkflow) (string, []string) {
	t.Helper()

	job, ok := wf.Jobs["verify-all-checks"]
	if !ok || len(job.Steps) != 1 {
		t.Fatalf("verify-all-checks has %d steps, want 1", len(job.Steps))
	}
	var kept []string
	if err := yaml.Unmarshal([]byte(job.Steps[0].Env["KEPT"]), &kept); err != nil || len(kept) == 0 {
		t.Fatalf("KEPT %q: %v", job.Steps[0].Env["KEPT"], err)
	}

	return job.Steps[0].Run, kept
}

// A docs-scoped run skips exactly the gated jobs, so a kept job must never be
// gated (it would be skipped and fail the run), every gated job must be one
// verify-all-checks judges, and every job must be one it judges at all.
func TestCIScopeGatesOnlyJobsVerifyMaySeeSkipped(t *testing.T) {
	t.Parallel()

	wf := readCIWorkflow(t)
	_, kept := verifyStep(t, wf)
	verified := jobNeeds(t, wf.Jobs["verify-all-checks"].Needs)

	if wf.Jobs["verify-all-checks"].If != "always()" {
		t.Errorf("verify-all-checks runs if %q; it must be always(), or a failed or skipped job leaves the one required check unreported", wf.Jobs["verify-all-checks"].If)
	}

	gated := 0
	for name, job := range wf.Jobs {
		if name == "verify-all-checks" {
			continue
		}
		if !slices.Contains(verified, name) {
			t.Errorf("job %s is not in verify-all-checks' needs, so nothing requires it", name)
		}
		isGated := job.If == scopeGate
		if isGated {
			gated++
			if !slices.Contains(jobNeeds(t, job.Needs), "changes") {
				t.Errorf("job %s is gated on changes without needing it", name)
			}
		}
		if slices.Contains(kept, name) && isGated {
			t.Errorf("job %s is KEPT for docs runs and gated off them", name)
		}
		if strings.Contains(job.If, "changes.outputs.scope") && !isGated {
			t.Errorf("job %s reads the scope as %q; the one gate is %q", name, job.If, scopeGate)
		}
	}
	for _, k := range kept {
		if _, ok := wf.Jobs[k]; !ok {
			t.Errorf("KEPT names %s, which is not a job", k)
		}
	}
	if gated == 0 {
		t.Fatal("no job is gated on the scope, so a docs change still runs everything")
	}
}

func TestCIVerifyAcceptsASkipOnlyWhereTheScopeDecidedIt(t *testing.T) {
	t.Parallel()

	script, kept := verifyStep(t, readCIWorkflow(t))

	// needs builds NEEDS as GitHub renders it: every job with a result, changes
	// with its scope output.
	needs := func(scope, changesResult string, results map[string]string) string {
		parts := []string{`"changes":{"result":"` + changesResult + `","outputs":{"scope":"` + scope + `"}}`}
		for k, v := range results {
			parts = append(parts, `"`+k+`":{"result":"`+v+`","outputs":{}}`)
		}

		return "{" + strings.Join(parts, ",") + "}"
	}

	for _, tc := range []struct {
		name    string
		needs   string
		wantErr bool
	}{
		{"full run, all succeeded", needs("full", "success", map[string]string{"test": "success", "lint": "success"}), false},
		{"docs run, gated job skipped", needs("docs", "success", map[string]string{"test": "success", "lint": "skipped"}), false},
		{"docs run, a kept job skipped", needs("docs", "success", map[string]string{"test": "skipped", "lint": "skipped"}), true},
		{"full run, a job skipped", needs("full", "success", map[string]string{"test": "success", "lint": "skipped"}), true},
		{"docs run, a job cancelled", needs("docs", "success", map[string]string{"test": "success", "lint": "cancelled"}), true},
		{"docs run, a job failed", needs("docs", "success", map[string]string{"test": "success", "lint": "failure"}), true},
		{"changes failed", needs("docs", "failure", map[string]string{"test": "success", "lint": "skipped"}), true},
		{"no scope recorded", needs("", "success", map[string]string{"test": "success", "lint": "skipped"}), true},
		{"an unknown scope", needs("partial", "success", map[string]string{"test": "success", "lint": "skipped"}), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			cmd := exec.CommandContext(t.Context(), "bash", "-e", "-o", "pipefail", "-c", script)
			cmd.Env = append(os.Environ(), "NEEDS="+tc.needs, "KEPT=["+`"`+strings.Join(kept, `","`)+`"`+"]")
			out, err := cmd.CombinedOutput()
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("verify err=%v, want failure=%v\n%s", err, tc.wantErr, out)
			}
		})
	}
}

// A git whose diff prints raw, a truncated or malformed record after a valid
// one, and which hands every other command to the real git, so the merge-commit
// check still reads a real repository.
func fakeDiffGit(t *testing.T, raw string) string {
	t.Helper()

	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	out := filepath.Join(bin, "diff-output")
	if err := os.WriteFile(out, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nif [ \"$1\" = diff ]; then cat " + out + "; exit 0; fi\nexec " + realGit + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	return bin
}

func TestCIScopeIsFullWhenGitDiffOutputIsIncomplete(t *testing.T) {
	t.Parallel()

	const valid = ":100644 100644 aaaaaaa bbbbbbb M\x00docs/index.md\x00"
	for _, tc := range []struct {
		name, raw, reason string
	}{
		{"a header cut short", valid + ":100644 1006", "ends inside a record header"},
		{"a header with no path", valid + ":100644 100644 aaaaaaa bbbbbbb M\x00", "no terminated path"},
		{"a path with no terminator", valid + ":100644 100644 aaaaaaa bbbbbbb M\x00docs/a.md", "no terminated path"},
		{"a header of another shape", valid + "garbage\x00docs/a.md\x00", "cannot read"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := scopeRepo(t, func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "docs/a.md", "a\n")
			})
			bin := fakeDiffGit(t, tc.raw)
			got, stderr := runScope(t, dir, "pull_request", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			if got != "full" || !strings.Contains(stderr, tc.reason) {
				t.Fatalf("scope %q (%q), want full naming %q", got, stderr, tc.reason)
			}
		})
	}

	// The same fake with only the valid record answers docs, so the cases above
	// fail on the truncation and not on the fake.
	dir := scopeRepo(t, func(t *testing.T, dir string) {
		t.Helper()

		mustWrite(t, dir, "docs/a.md", "a\n")
	})
	bin := fakeDiffGit(t, valid)
	if got, stderr := runScope(t, dir, "pull_request", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH")); got != "docs" {
		t.Fatalf("the fake's one valid record gave %q (%q), want docs", got, stderr)
	}
}
