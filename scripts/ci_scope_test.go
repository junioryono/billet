package scripts_test

import (
	"encoding/json"
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

// ciFamilies is every family scripts/ci-scope.sh decides, in its output order.
var ciFamilies = []string{
	"host_lifecycle", "postgres", "postgres_command", "postgres_retirement", "package_lifecycle",
	"terraform", "terraform_consumer_floor", "terraform_lint", "replay", "lint",
}

// runScope runs the classifier and returns the families it selected, sorted,
// "all" when it selected every family, or "none" when it selected none, with
// its stderr. Its stdout must be exactly schema=2 and one true/false line per
// family, because that is what the workflow writes to $GITHUB_OUTPUT.
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

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != len(ciFamilies)+1 || lines[0] != "schema=2" {
		t.Fatalf("ci-scope.sh printed %q, want schema=2 and one line per family", out)
	}
	var selected []string
	for i, f := range ciFamilies {
		switch lines[i+1] {
		case f + "=true":
			selected = append(selected, f)
		case f + "=false":
		default:
			t.Fatalf("line %d is %q, want %s=true or %s=false", i+2, lines[i+1], f, f)
		}
	}
	switch len(selected) {
	case 0:
		return "none", stderr.String()
	case len(ciFamilies):
		return "all", stderr.String()
	}
	slices.Sort(selected)

	return strings.Join(selected, ","), stderr.String()
}

// families names a selection the way runScope reports one. It sorts a copy: the
// callers pass the shared application slice from parallel tests.
func families(f ...string) string {
	f = slices.Clone(f)
	slices.Sort(f)

	return strings.Join(f, ",")
}

// scopeOfPath classifies one path through the classifier's CI_SCOPE_PATH mode.
func scopeOfPath(t *testing.T, path string) string {
	t.Helper()

	got, _ := runScope(t, t.TempDir(), "pull_request", "CI_SCOPE_PATH="+path)

	return got
}

// THE REPLAY RULE COVERS EVERY PACKAGE THE REPLAY HARNESS BUILDS. The rule is a
// list, and a list drifts: a package replay starts importing would otherwise be
// scoped out of replay on every change to it. This asks go itself for the
// closure of internal/replay's tests and requires a Go file in each of billet's
// packages in it to select replay.
func TestCIScopeReplayRuleCoversTheReplayClosure(t *testing.T) {
	t.Parallel()

	cmd := exec.CommandContext(t.Context(), "go", "list", "-deps", "-test",
		"-f", "{{if not .Standard}}{{.ImportPath}}{{end}}", "./internal/replay/...")
	cmd.Dir = ".."
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list the replay closure: %v", err)
	}

	const module = "github.com/junioryono/billet/"
	seen := 0
	for _, pkg := range strings.Fields(string(out)) {
		if !strings.HasPrefix(pkg, module+"internal/") {
			continue
		}
		// A test binary appears as "pkg.test" and its package as "pkg [pkg.test]".
		dir := strings.TrimPrefix(strings.SplitN(pkg, " ", 2)[0], module)
		if strings.HasSuffix(dir, ".test") {
			continue
		}
		// An external test package (foo_test) lives in foo's directory.
		dir = strings.TrimSuffix(dir, "_test")
		seen++
		if got := scopeOfPath(t, dir+"/any.go"); got != "all" && !slices.Contains(strings.Split(got, ","), "replay") {
			t.Errorf("replay builds %s, but a change to it selects %q, not replay", dir, got)
		}
	}
	if seen == 0 {
		t.Fatalf("go list found no billet package in the replay closure:\n%s", out)
	}
}

// application is what any path under cmd/, internal/ or deploy/ selects.
var application = []string{
	"host_lifecycle", "postgres_command", "postgres_retirement", "package_lifecycle", "terraform", "lint",
}

func TestCIScopeSelectsTheFamiliesAChangeReaches(t *testing.T) {
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
			event: "pull_request", want: "none", reason: "all 4 paths are documentation",
		},
		{
			name: "one code path among docs",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "docs/a.md", "a\n")
				mustWrite(t, dir, "cmd/billet/main.go", "package main // changed\n")
			},
			event: "pull_request", want: families(append([]string{"postgres"}, application...)...), reason: "2 paths select",
		},
		{
			name: "code renamed into docs is still a code change",
			change: func(t *testing.T, dir string) {
				t.Helper()

				gitIn(t, dir, "mv", "cmd/billet/main.go", "docs/main.go.md")
			},
			event: "pull_request", want: families(append([]string{"postgres"}, application...)...), reason: "2 paths select",
		},
		{
			name: "a docs-only change pushed rather than proposed",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "docs/a.md", "a\n")
			},
			event: "push", want: "all", reason: "is not a pull request",
		},
		{
			name: "LICENSE is a package input",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "LICENSE", "Apache\n")
			},
			event: "pull_request", want: "package_lifecycle", reason: "select: package_lifecycle",
		},
		{
			name: "the root README is a package input",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "README.md", "# billet\n")
			},
			event: "pull_request", want: "package_lifecycle", reason: "select: package_lifecycle",
		},
		{
			name: "Go source under docs is source",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, "docs/helper.go", "package docs\n")
			},
			event: "pull_request", want: families("lint", "postgres"), reason: "1 paths select",
		},
		{
			name: "a script under .claude is not documentation",
			change: func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, ".claude/skills/x/run.sh", "#!/bin/sh\n")
			},
			event: "pull_request", want: "all", reason: "no rule names .claude/skills/x/run.sh",
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
			event: "pull_request", want: "all", reason: "symlink or a submodule",
		},
		{
			name: "a documentation path made executable",
			change: func(t *testing.T, dir string) {
				t.Helper()

				if err := os.Chmod(filepath.Join(dir, "docs", "index.md"), 0o755); err != nil {
					t.Fatal(err)
				}
			},
			event: "pull_request", want: "all", reason: "100644 -> 100755",
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
			event: "pull_request", want: "all", reason: "100644 -> 120000",
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
			event: "pull_request", want: "all", reason: "100755 -> 100644",
		},
		{
			name: "a documentation-shaped symlink deleted",
			change: func(t *testing.T, dir string) {
				t.Helper()

				gitIn(t, dir, "rm", "-q", "docs/link.md")
			},
			event: "pull_request", want: "all", reason: "120000 -> 000000",
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
			event: "pull_request", want: "all", reason: "000000 -> 160000",
		},
		{
			name: "a documentation file deleted",
			change: func(t *testing.T, dir string) {
				t.Helper()

				gitIn(t, dir, "rm", "-q", "docs/index.md")
			},
			event: "pull_request", want: "none", reason: "all 1 paths are documentation",
		},
		{
			name:   "a change that lists no paths",
			change: func(*testing.T, string) {},
			event:  "pull_request", want: "all", reason: "lists no paths",
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

// EACH PATH REACHES THE FAMILIES THAT READ IT, AND AN UNKNOWN PATH REACHES ALL.
// One written file per case, on top of scopeRepo's base, so the selection is
// that path's alone.
func TestCIScopeMapsEachPathToItsFamilies(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		path string
		want string
	}{
		// Application code: every family that builds or drives billet, plus
		// postgres's raw-SQL walk for non-test Go and replay for its packages.
		{"internal/server/listener.go", families(append([]string{"postgres", "replay"}, application...)...)},
		{"internal/server/listener_test.go", families(append([]string{"replay"}, application...)...)},
		{"internal/provider/docker/docker.go", families(append([]string{"postgres"}, application...)...)},
		{"internal/provider/provider.go", families(append([]string{"postgres", "replay"}, application...)...)},
		{"internal/provider/simulated/sim.go", families(append([]string{"postgres", "replay"}, application...)...)},
		{"internal/store/ceph/ceph.go", families(append([]string{"postgres"}, application...)...)},
		{"internal/provider/testdata/shape.json", families(append([]string{"replay"}, application...)...)},
		{"internal/store/embedded/schema.json", families(append([]string{"replay"}, application...)...)},
		{"tools/lint/go.work", "all"},
		{"internal/state/migrations/0099_x.sql", families(append([]string{"postgres", "replay"}, application...)...)},
		{"internal/lifeops/unit.go", families(append([]string{"postgres"}, application...)...)},
		{"deploy/billet-server.service", families(application...)},
		// The collection and the retirement shard checker.
		{"ansible_collections/junioryono/billet/roles/host/tasks/main.yml",
			families("host_lifecycle", "postgres_command", "postgres_retirement")},
		{"scripts/check-retirement-shards.py", families("host_lifecycle", "postgres_retirement")},
		// Packaging inputs.
		{".goreleaser.yaml", "package_lifecycle"},
		{"billet.example.yaml", "package_lifecycle"},
		{"scripts/restore-rehearsal.sh", "package_lifecycle"},
		{"scripts/mkreleasemanifest/main.go", families("package_lifecycle", "lint", "postgres")},
		// Terraform: every module selects plan and lint, only the billet module
		// the consumer floor, and the hybrid root check selects plan alone.
		{"terraform/modules/fleet-ec2/main.tf", families("terraform", "terraform_lint")},
		{"terraform/modules/billet/main.tf", families("terraform", "terraform_lint", "terraform_consumer_floor")},
		{"scripts/hybrid-root-check.sh", "terraform"},
		// The linter's own module and configuration.
		{"tools/lint/rawsql/rawsql.go", families("lint", "postgres")},
		{".golangci.yml", "lint"},
		// Read only by the jobs that always run.
		{"actions/stickydisk/action.yml", "none"},
		{"scripts/install.sh", "none"},
		{"scripts/ci_scope_test.go", "lint"},
		// What drives the jobs, and the module graph: everything.
		{".github/workflows/ci.yml", "all"},
		{"Makefile", "all"},
		{"go.sum", "all"},
		{"tools/lint/go.mod", "all"},
		{"sqlc.yaml", "all"},
		{"scripts/ci-scope.sh", "all"},
		// A path no rule names: everything, never nothing.
		{"scripts/some-new-gate.sh", "all"},
		{"vendor.txt", "all"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			t.Parallel()

			dir := scopeRepo(t, func(t *testing.T, dir string) {
				t.Helper()

				mustWrite(t, dir, tc.path, "changed\n")
			})
			if got, stderr := runScope(t, dir, "pull_request"); got != tc.want {
				t.Fatalf("%s selected %q (%q), want %q", tc.path, got, stderr, tc.want)
			}
		})
	}
}

func TestCIScopeSelectsEverythingWhenHEADIsNotAMergeCommit(t *testing.T) {
	t.Parallel()

	dir := scopeRepo(t, func(t *testing.T, dir string) {
		t.Helper()
		mustWrite(t, dir, "docs/a.md", "a\n")
	})
	gitIn(t, dir, "checkout", "-q", "HEAD^1")

	got, stderr := runScope(t, dir, "pull_request")
	if got != "all" || !strings.Contains(stderr, "not a merge commit") {
		t.Fatalf("scope %q (%q), want every family because HEAD has one parent", got, stderr)
	}
}

func TestCIScopeSelectsEverythingWhenGitCannotAnswer(t *testing.T) {
	t.Parallel()

	// Not a repository at all: rev-parse fails, which must select every family.
	got, stderr := runScope(t, t.TempDir(), "pull_request")
	if got != "all" {
		t.Fatalf("scope %q (%q) outside a repository, want every family", got, stderr)
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

// familyGate is the one gate a job of family f carries.
func familyGate(f string) string { return "needs.changes.outputs." + f + " == 'true'" }

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

// verifyStep is verify-all-checks' one step: its script, its KEPT list and its
// FAMILY map.
func verifyStep(t *testing.T, wf ciWorkflow) (string, []string, map[string]string) {
	t.Helper()

	job, ok := wf.Jobs["verify-all-checks"]
	if !ok || len(job.Steps) != 1 {
		t.Fatalf("verify-all-checks has %d steps, want 1", len(job.Steps))
	}
	var kept []string
	if err := yaml.Unmarshal([]byte(job.Steps[0].Env["KEPT"]), &kept); err != nil || len(kept) == 0 {
		t.Fatalf("KEPT %q: %v", job.Steps[0].Env["KEPT"], err)
	}
	var family map[string]string
	if err := yaml.Unmarshal([]byte(job.Steps[0].Env["FAMILY"]), &family); err != nil || len(family) == 0 {
		t.Fatalf("FAMILY %q: %v", job.Steps[0].Env["FAMILY"], err)
	}

	return job.Steps[0].Run, kept, family
}

// EVERY JOB IS KEPT OR BELONGS TO ONE FAMILY, AND IS GATED ON EXACTLY THAT
// FAMILY. A KEPT job must never be gated (it would be skipped and fail the run);
// a family job must carry its own family's gate and need changes to read it, or
// it runs when it should not or is skipped when it should run; the families the
// map names must be exactly the ones scripts/ci-scope.sh decides; and every job
// must be one verify-all-checks judges at all.
func TestCIScopeGatesEachJobOnItsOwnFamily(t *testing.T) {
	t.Parallel()

	wf := readCIWorkflow(t)
	_, kept, family := verifyStep(t, wf)
	verified := jobNeeds(t, wf.Jobs["verify-all-checks"].Needs)

	if wf.Jobs["verify-all-checks"].If != "always()" {
		t.Errorf("verify-all-checks runs if %q; it must be always(), or a failed or skipped job leaves the one required check unreported", wf.Jobs["verify-all-checks"].If)
	}

	for name, job := range wf.Jobs {
		if name == "verify-all-checks" {
			continue
		}
		if !slices.Contains(verified, name) {
			t.Errorf("job %s is not in verify-all-checks' needs, so nothing requires it", name)
		}
		f, inFamily := family[name]
		isKept := slices.Contains(kept, name)
		switch {
		case inFamily && isKept:
			t.Errorf("job %s is both KEPT and in family %s", name, f)
		case !inFamily && !isKept:
			t.Errorf("job %s is neither KEPT nor in a family, so verify-all-checks cannot judge a skip of it", name)
		case isKept:
			if strings.Contains(job.If, "needs.changes") {
				t.Errorf("KEPT job %s is gated (%q), so a run could skip a job that must always run", name, job.If)
			}
		default:
			if job.If != familyGate(f) {
				t.Errorf("job %s runs if %q, want its family's gate %q", name, job.If, familyGate(f))
			}
			if !slices.Contains(jobNeeds(t, job.Needs), "changes") {
				t.Errorf("job %s is gated on changes without needing it", name)
			}
		}
	}
	for _, k := range kept {
		if _, ok := wf.Jobs[k]; !ok {
			t.Errorf("KEPT names %s, which is not a job", k)
		}
	}
	named := map[string]bool{}
	for job, f := range family {
		if _, ok := wf.Jobs[job]; !ok {
			t.Errorf("FAMILY names %s, which is not a job", job)
		}
		if !slices.Contains(ciFamilies, f) {
			t.Errorf("FAMILY puts %s in %s, which scripts/ci-scope.sh does not decide", job, f)
		}
		named[f] = true
	}
	for _, f := range ciFamilies {
		if !named[f] {
			t.Errorf("family %s gates no job, so deciding it changes nothing", f)
		}
	}
}

func TestCIVerifyAcceptsASkipOnlyWhereTheFamilyWasNotSelected(t *testing.T) {
	t.Parallel()

	script, kept, family := verifyStep(t, readCIWorkflow(t))

	// outputs is changes' outputs with every family false except those named.
	outputs := func(selected ...string) map[string]string {
		o := map[string]string{"schema": "2"}
		for _, f := range ciFamilies {
			o[f] = "false"
		}
		for _, f := range selected {
			o[f] = "true"
		}

		return o
	}
	// needs renders NEEDS as GitHub does: every job with a result, changes with
	// its outputs. Every job succeeds unless results says otherwise.
	needs := func(changesResult string, out map[string]string, results map[string]string) string {
		var o []string
		for k, v := range out {
			o = append(o, `"`+k+`":"`+v+`"`)
		}
		parts := []string{`"changes":{"result":"` + changesResult + `","outputs":{` + strings.Join(o, ",") + `}}`}
		for _, j := range kept {
			if j == "changes" {
				continue
			}
			r := "success"
			if v, ok := results[j]; ok {
				r = v
			}
			parts = append(parts, `"`+j+`":{"result":"`+r+`","outputs":{}}`)
		}
		for j := range family {
			r := "success"
			if v, ok := results[j]; ok {
				r = v
			}
			parts = append(parts, `"`+j+`":{"result":"`+r+`","outputs":{}}`)
		}

		return "{" + strings.Join(parts, ",") + "}"
	}
	// skipped marks every job of the unselected families skipped.
	skipped := func(out map[string]string) map[string]string {
		r := map[string]string{}
		for j, f := range family {
			if out[f] == "false" {
				r[j] = "skipped"
			}
		}

		return r
	}
	docsOnly := outputs()
	hostOnly := outputs("host_lifecycle")
	all := outputs(ciFamilies...)
	noSchema := outputs()
	delete(noSchema, "schema")
	badValue := outputs()
	badValue["lint"] = "maybe"
	missing := outputs()
	delete(missing, "replay")

	for _, tc := range []struct {
		name    string
		event   string
		needs   string
		wantErr bool
	}{
		{"everything selected and succeeded", "pull_request", needs("success", all, nil), false},
		{"docs-only: every family skipped", "pull_request", needs("success", docsOnly, skipped(docsOnly)), false},
		{"host only: its jobs ran, the rest skipped", "pull_request", needs("success", hostOnly, skipped(hostOnly)), false},
		{"an unselected job that ran anyway and passed", "pull_request", needs("success", docsOnly, nil), false},
		{"a selected job skipped", "pull_request", needs("success", hostOnly,
			merge(skipped(hostOnly), map[string]string{"host-lifecycle": "skipped"})), true},
		{"a selected job skipped because what it needs failed", "pull_request", needs("success", hostOnly,
			merge(skipped(hostOnly), map[string]string{"gate-binaries": "failure", "host-lifecycle": "skipped"})), true},
		{"a kept job skipped", "pull_request", needs("success", docsOnly,
			merge(skipped(docsOnly), map[string]string{"test": "skipped"})), true},
		{"an unselected job cancelled", "pull_request", needs("success", docsOnly,
			merge(skipped(docsOnly), map[string]string{"lint": "cancelled"})), true},
		{"an unselected job failed", "pull_request", needs("success", docsOnly,
			merge(skipped(docsOnly), map[string]string{"lint": "failure"})), true},
		{"changes failed", "pull_request", needs("failure", docsOnly, skipped(docsOnly)), true},
		{"no schema recorded", "pull_request", needs("success", noSchema, skipped(noSchema)), true},
		{"a family neither true nor false", "pull_request", needs("success", badValue, skipped(docsOnly)), true},
		{"a family missing", "pull_request", needs("success", missing, skipped(docsOnly)), true},
		{"a push that did not select everything", "push", needs("success", docsOnly, skipped(docsOnly)), true},
		{"a push that selected everything", "push", needs("success", all, nil), false},
		{"a job verify-all-checks does not know", "pull_request",
			strings.TrimSuffix(needs("success", all, nil), "}") + `,"stranger":{"result":"success","outputs":{}}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			fam, err := json.Marshal(family)
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-e", "-o", "pipefail", "-c", script)
			cmd.Env = append(os.Environ(), "NEEDS="+tc.needs, "EVENT="+tc.event,
				"KEPT=["+`"`+strings.Join(kept, `","`)+`"`+"]", "FAMILY="+string(fam))
			out, err := cmd.CombinedOutput()
			// A jq program that errors fails every run, so a rejection that is
			// really a jq error proves nothing about the rule it names.
			if strings.Contains(string(out), "jq: error") {
				t.Fatalf("the verifier's jq program errored instead of deciding:\n%s", out)
			}
			if gotErr := err != nil; gotErr != tc.wantErr {
				t.Fatalf("verify err=%v, want failure=%v\n%s", err, tc.wantErr, out)
			}
		})
	}

	// A job absent from NEEDS altogether (dropped from verify's needs) fails too.
	t.Run("a mapped job missing from needs", func(t *testing.T) {
		t.Parallel()

		full := needs("success", all, nil)
		var decoded map[string]any
		if err := json.Unmarshal([]byte(full), &decoded); err != nil {
			t.Fatal(err)
		}
		delete(decoded, "replay")
		trimmed, err := json.Marshal(decoded)
		if err != nil {
			t.Fatal(err)
		}
		fam, err := json.Marshal(family)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "bash", "-e", "-o", "pipefail", "-c", script)
		cmd.Env = append(os.Environ(), "NEEDS="+string(trimmed), "EVENT=pull_request",
			"KEPT=["+`"`+strings.Join(kept, `","`)+`"`+"]", "FAMILY="+string(fam))
		out, err := cmd.CombinedOutput()
		if strings.Contains(string(out), "jq: error") {
			t.Fatalf("the verifier's jq program errored instead of deciding:\n%s", out)
		}
		if err == nil {
			t.Fatalf("verify passed with replay missing from needs\n%s", out)
		}
	})
}

// merge returns a with b's entries laid over it.
func merge(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}

	return out
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
	script := "#!/bin/sh\nif [ \"$1\" = diff ]; then exec cat '" + out + "'; fi\nexec '" + realGit + "' \"$@\"\n"
	writeExecutable(t, filepath.Join(bin, "git"), script)

	return bin
}

func TestCIScopeSelectsEverythingWhenGitDiffOutputIsIncomplete(t *testing.T) {
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
			if got != "all" || !strings.Contains(stderr, tc.reason) {
				t.Fatalf("scope %q (%q), want every family naming %q", got, stderr, tc.reason)
			}
		})
	}

	// The same fake with only the valid record selects nothing, so the cases above
	// fail on the truncation and not on the fake.
	dir := scopeRepo(t, func(t *testing.T, dir string) {
		t.Helper()

		mustWrite(t, dir, "docs/a.md", "a\n")
	})
	bin := fakeDiffGit(t, valid)
	if got, stderr := runScope(t, dir, "pull_request", "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH")); got != "none" {
		t.Fatalf("the fake's one valid record gave %q (%q), want no family", got, stderr)
	}
}
