package scripts_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// claudeGuardScript is the one program every billet hook runs.
const claudeGuardScript = "../.claude/hooks/guard.py"

// hookEnv is the environment every fixture git command and every hook run
// gets: no global or system git configuration and no inherited GIT_ variable,
// so a developer's hooksPath or GIT_DIR cannot reach a fixture; and neither CI
// nor CLAUDE_FULL_CHECKS, which change what the bash hook refuses, so a case
// reads the same on a laptop and in CI unless it sets one itself.
func hookEnv(extra ...string) []string {
	var env []string

	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if !strings.HasPrefix(kv, "GIT_") && name != "CI" && name != "CLAUDE_FULL_CHECKS" {
			env = append(env, kv)
		}
	}

	env = append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1")

	return append(env, extra...)
}

func hookGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = hookEnv()

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func hookWrite(t *testing.T, dir, rel, body string) {
	t.Helper()

	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fixtureRefs says which of main and origin/main exist, and what each holds.
type fixtureRefs int

const (
	bothRefs       fixtureRefs = iota // main and origin/main both hold the published migrations
	staleOrigin                       // origin/main predates a migration main has
	noDefaultRefs                     // neither main nor origin/main exists
	repoOnMainOnly                    // like bothRefs, but HEAD stays on main
	originAhead                       // origin/main holds a migration local main does not
	corruptMain                       // refs/heads/main holds something that is not a commit
)

// claudeHookRepo is a throwaway repository shaped like billet's: published
// migrations on main, and a working branch checked out.
func claudeHookRepo(t *testing.T, refs fixtureRefs) string {
	t.Helper()

	dir := t.TempDir()
	branch := "main"

	if refs == noDefaultRefs {
		branch = "trunk"
	}

	hookGit(t, dir, "init", "-q", "-b", branch)
	hookGit(t, dir, "config", "user.email", "test@example.com")
	hookGit(t, dir, "config", "user.name", "test")
	hookGit(t, dir, "config", "commit.gpgsign", "false")

	hookWrite(t, dir, "internal/state/migrations/0001_init.sql", "-- +billet:statement\nSELECT 1\n-- +billet:end\n")
	hookWrite(t, dir, "internal/state/pgmigrations/0001_init.sql", "-- +billet:statement\nSELECT 1\n-- +billet:end\n")
	hookGit(t, dir, "add", ".")
	hookGit(t, dir, "commit", "-q", "-m", "init")

	if refs != noDefaultRefs {
		hookGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	}

	if refs == staleOrigin {
		hookWrite(t, dir, "internal/state/migrations/0002_local.sql", "-- +billet:statement\nSELECT 2\n-- +billet:end\n")
		hookGit(t, dir, "add", ".")
		hookGit(t, dir, "commit", "-q", "-m", "local")
	}

	if refs == originAhead {
		hookGit(t, dir, "checkout", "-q", "-b", "upstream")
		hookWrite(t, dir, "internal/state/migrations/0003_upstream.sql", "-- +billet:statement\nSELECT 3\n-- +billet:end\n")
		hookGit(t, dir, "add", ".")
		hookGit(t, dir, "commit", "-q", "-m", "upstream")
		hookGit(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
		hookGit(t, dir, "checkout", "-q", "main")
	}

	if refs != repoOnMainOnly {
		hookGit(t, dir, "checkout", "-q", "-b", "junior-oct03-work")
	}

	if refs == corruptMain {
		if err := os.WriteFile(filepath.Join(dir, ".git", "refs", "heads", "main"), []byte("not-a-commit\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// shPlusS is what the hook answers `sh +s <a gate>` with here: nothing where sh
// is bash (macOS, whose /bin/sh runs what /private/var/select/sh names), which
// reads standard input, and gate where it is dash (Linux), which runs the script.
func shPlusS(t *testing.T, gate string) string {
	t.Helper()

	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatal(err)
	}

	target, err := filepath.EvalSymlinks(sh)
	if err != nil {
		t.Fatal(err)
	}

	if runtime.GOOS == "darwin" && target == "/bin/sh" {
		if target, err = filepath.EvalSymlinks("/private/var/select/sh"); err != nil {
			t.Fatal(err)
		}
	}

	if filepath.Base(target) == "bash" {
		return ""
	}

	return gate
}

// runClaudeGuard runs one hook and answers its exit status and stderr.
func runClaudeGuard(t *testing.T, mode, root string, payload map[string]any, extraEnv ...string) (int, string) {
	t.Helper()

	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	return runClaudeGuardRaw(t, mode, root, body, extraEnv...)
}

func runClaudeGuardRaw(t *testing.T, mode, root string, body []byte, extraEnv ...string) (int, string) {
	t.Helper()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 is required by the hooks (and by the gate scripts): %v", err)
	}

	cmd := exec.CommandContext(t.Context(), python, claudeGuardScript, mode)
	cmd.Stdin = bytes.NewReader(body)
	cmd.Env = hookEnv(append([]string{"CLAUDE_PROJECT_DIR=" + root, "HOME=" + t.TempDir(),
		"XDG_CONFIG_HOME=" + t.TempDir()}, extraEnv...)...)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err = cmd.Run()
	if err == nil {
		return 0, stderr.String()
	}

	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), stderr.String()
	}

	t.Fatalf("run %s: %v", claudeGuardScript, err)

	return -1, ""
}

func editPayload(root, rel string) map[string]any {
	return map[string]any{
		"tool_name": "Edit", "cwd": root,
		"tool_input": map[string]any{"file_path": filepath.Join(root, rel)},
	}
}

func bashPayload(root, command string) map[string]any {
	return map[string]any{"tool_name": "Bash", "cwd": root, "tool_input": map[string]any{"command": command}}
}

// expectHook asserts one hook verdict: refuse is a clause the refusal must
// carry, and empty means the call is allowed.
func expectHook(t *testing.T, label string, code int, stderr, refuse string) {
	t.Helper()

	switch {
	case refuse == "" && code != 0:
		t.Errorf("%s: refused (%d) though nothing forbids it: %s", label, code, stderr)
	case refuse != "" && code != 2:
		t.Errorf("%s: exit %d, want 2 (refused); stderr %q", label, code, stderr)
	case refuse != "" && !strings.Contains(stderr, refuse):
		t.Errorf("%s: refused with %q, want it to say %q", label, stderr, refuse)
	}
}

// THE EDIT GUARD REFUSES WHAT THE REPOSITORY SAYS NOBODY EDITS, AND NOTHING ELSE.
func TestTheEditHookRefusesGeneratedCodePublishedMigrationsAndSymlinkPaths(t *testing.T) {
	t.Parallel()

	root := claudeHookRepo(t, bothRefs)

	// An in-repository alias of internal/state, the way a session can reach a
	// file by a second name.
	if err := os.Symlink(filepath.Join("internal", "state"), filepath.Join(root, "statealias")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ rel, refuse string }{
		{"internal/state/ledgerdb/queries.sql.go", "generated by sqlc"},
		{"internal/state/LEDGERDB/models.go", "generated by sqlc"},
		{"statealias/ledgerdb/models.go", "generated by sqlc"},
		{"internal/state/migrations/0001_init.sql", "published migration"},
		{"internal/state/migrations/0001_INIT.sql", "published migration"},
		{"statealias/../state/migrations/0001_init.sql", "published migration"},
		{"internal/state/pgmigrations/0001_init.sql", "published migration"},
		{"statealias/migrations/0001_init.sql", "published migration"},
		{"internal/state/migrations/0002_new.sql", ""},
		{"internal/state/pgmigrations/0002_new.sql", ""},
		{"internal/state/queries/leases.sql", ""},
		{"docs/ledgerdb_notes.md", ""},
		{"AGENTS.md", "committed symlink"},
		{"agents.md", "committed symlink"},
		{"internal/state/AGENTS.md", "committed symlink"},
		{"ansible_collections/junioryono/billet/Agents.md", "committed symlink"},
		{"internal/state/CLAUDE.md", ""},
		{"docs/agents.md.txt", ""},
		{".AGENTS/skills/billet-state/SKILL.md", "committed symlink"},
		{".agents/skills/billet-state/SKILL.md", "committed symlink"},
		{"CLAUDE.md", ""},
		{"internal/server/listener.go", ""},
	} {
		payload := editPayload(root, tc.rel)
		if strings.Contains(tc.rel, "/../") {
			// Sent as written: filepath.Join would clean away the `..` the case is about.
			payload = map[string]any{"tool_name": "Edit", "cwd": root, "tool_input": map[string]any{"file_path": tc.rel}}
		}

		code, stderr := runClaudeGuard(t, "edit", root, payload)
		expectHook(t, tc.rel, code, stderr, tc.refuse)
	}

	// A relative path is the cwd's, and a path outside the project is not judged.
	rel := map[string]any{"tool_name": "Write", "cwd": root, "tool_input": map[string]any{
		"file_path": "internal/state/ledgerdb/models.go",
	}}
	code, stderr := runClaudeGuard(t, "edit", root, rel)
	expectHook(t, "a relative path into ledgerdb", code, stderr, "generated by sqlc")

	code, stderr = runClaudeGuard(t, "edit", root, editPayload(t.TempDir(), "internal/state/ledgerdb/models.go"))
	expectHook(t, "a path outside the project", code, stderr, "")
}

// A MIGRATION EITHER REF HOLDS IS PUBLISHED, and a stale origin/main does not
// make local main's migration editable.
func TestTheEditHookReadsBothDefaultRefs(t *testing.T) {
	t.Parallel()

	root := claudeHookRepo(t, staleOrigin)

	code, stderr := runClaudeGuard(t, "edit", root, editPayload(root, "internal/state/migrations/0002_local.sql"))
	expectHook(t, "a migration only local main holds", code, stderr, "published migration")

	ahead := claudeHookRepo(t, originAhead)
	code, stderr = runClaudeGuard(t, "edit", ahead, editPayload(ahead, "internal/state/migrations/0003_upstream.sql"))
	expectHook(t, "a migration only origin/main holds", code, stderr, "published migration")

	corrupt := claudeHookRepo(t, corruptMain)
	code, stderr = runClaudeGuard(t, "edit", corrupt, editPayload(corrupt, "internal/state/migrations/0009_new.sql"))
	expectHook(t, "a main ref that cannot be read", code, stderr, "could not read refs/heads/main")
}

// COULD NOT TELL IS NOT PERMISSION: without main, without git, or without a
// readable input, the hook refuses and says why.
func TestTheEditHookRefusesWhatItCannotJudge(t *testing.T) {
	t.Parallel()

	root := claudeHookRepo(t, noDefaultRefs)

	code, stderr := runClaudeGuard(t, "edit", root, editPayload(root, "internal/state/migrations/0002_new.sql"))
	expectHook(t, "no default refs", code, stderr, "cannot tell")

	withMain := claudeHookRepo(t, bothRefs)
	code, stderr = runClaudeGuard(t, "edit", withMain, editPayload(withMain, "internal/state/migrations/0002_new.sql"),
		"PATH="+t.TempDir())
	expectHook(t, "no git on PATH", code, stderr, "git could not be run")

	for label, body := range map[string]string{
		"not JSON":             "not json",
		"not an object":        "[1, 2]",
		"tool_input not a map": `{"cwd": "/", "tool_input": [1]}`,
		"a numeric path":       `{"cwd": "/", "tool_input": {"file_path": 7}}`,
		// An embedded NUL makes the path functions raise: an error nothing
		// expected, which the outer boundary still turns into a refusal.
		"a path holding a NUL": `{"cwd": "/", "tool_input": {"file_path": "internal/state/ledgerdb/x\u0000.go"}}`,
	} {
		code, stderr := runClaudeGuardRaw(t, "edit", withMain, []byte(body))
		if code != 2 || !strings.Contains(stderr, "billet hook") {
			t.Errorf("%s: exit %d, stderr %q; want an explained refusal", label, code, stderr)
		}
	}
}

// THE BASH GUARD HOLDS THE GIT FLOW: NO FORCE, NO REBASE, NOTHING ONTO MAIN,
// however the command line is spelled.
func TestTheBashHookHoldsTheGitFlow(t *testing.T) {
	t.Parallel()

	root := claudeHookRepo(t, bothRefs)

	for _, tc := range []struct{ command, refuse string }{
		{"git push --force origin junior-oct03-work", "force"},
		{"git push -f origin junior-oct03-work", "force"},
		{"git push -vf origin junior-oct03-work", "force"},
		{"git push --force-with-lease origin junior-oct03-work", "force"},
		{"git push origin +junior-oct03-work", "force"},
		{"git status\ngit push --force origin junior-oct03-work", "force"},
		{"git status # inspect\ngit push --force origin junior-oct03-work", "force"},
		{"echo '<<EOF'\ngit push --force origin junior-oct03-work", "force"},
		{"echo \"<<EOF\"\ngit push --force origin junior-oct03-work", "force"},
		{"cat <<EOF\nhello\nEOF\ngit push --force origin junior-oct03-work", "force"},
		{"cat <<-EOF\n\thello\n\tEOF\ngit push --force origin junior-oct03-work", "force"},
		{"env git push --force origin junior-oct03-work", "force"},
		{"env -u GIT_DIR git push --force origin junior-oct03-work", "force"},
		{"bash -c \"git push --force origin junior-oct03-work\"", "force"},
		{"bash -lc 'git push --force origin junior-oct03-work'", "force"},
		{"git push origin main", "never pushes to main"},
		{"git push origin main:main", "never pushes to main"},
		{"git push origin HEAD:main", "never pushes to main"},
		{"git push origin HEAD:refs/heads/main", "never pushes to main"},
		{"git push --repo=origin HEAD:main", "never pushes to main"},
		{"git push origin --delete main", "never pushes to main"},
		{"git push origin --all", "push one branch by name"},
		{"git push origin --mirror", "push one branch by name"},
		{"git push origin 'refs/heads/*:refs/heads/*'", "wildcard or matching"},
		{"git push origin :", "wildcard or matching"},
		{"git push", "must name what it pushes"},
		{"git push origin", "must name what it pushes"},
		{"scripts/gates/check.sh && git push origin main", "never pushes to main"},
		{"git rebase main", "merging"},
		{"git -C /somewhere rebase -i main", "merging"},
		{"git pull --rebase", "merging"},
		{"git pull -r", "merging"},
		{"git -c pull.rebase=true pull", "pull.rebase"},
		{"command git rebase main", "merging"},
		{"sudo -u runner git rebase main", "merging"},
		{"(git rebase main)", "merging"},
		{"GIT_EDITOR=true git rebase --continue", "merging"},
		{"git --git-dir=/elsewhere/.git commit -m x", "cannot judge"},
		{"git switch main && git commit -m x", "two separate calls"},
		{"git switch - && git commit -m x", "two separate calls"},
		{"git switch -c feature && git commit -m x", "two separate calls"},
		{"git checkout main && git commit -m x", "two separate calls"},
		{"git switch main && bash -c 'git commit -m x'", "two separate calls"},
		{"git switch main && git push origin junior-oct03-work", "two separate calls"},
		{"git checkout main -- README.md && git commit -m x", ""},
		{"git checkout -- README.md", ""},
		{"git switch main", ""},
		{"git push -u origin junior-oct03-work", ""},
		{"git push origin junior-main", ""},
		{"git push origin main-fix", ""},
		{"git push origin HEAD", ""},
		{"git pull", ""},
		{"git pull --no-rebase", ""},
		{"git commit -m 'work # 356'", ""},
		{"echo git rebase main", ""},
		{"cat <<'EOF'\nDon't use git rebase.\nEOF", ""},
		{"go test ./... | tee out.log", ""},
		{"gh pr merge 357 --merge", ""},
		{"git push origin # publish", "must name what it pushes"},
		{"git push origin junior-oct03-work # merge main", ""},
		{"git commit -m \"$(cat <<'EOF'\nDocument \"git status; git rebase main\" examples\nEOF\n)\"", ""},
		{"git push \\\n  -u origin junior-oct03-work", ""},
		{"git \\\npush origin main", "never pushes to main"},
		{"if git diff --cached --quiet; then echo clean; else git rebase main; fi", "merging"},
	} {
		code, stderr := runClaudeGuard(t, "bash", root, bashPayload(root, tc.command))
		expectHook(t, tc.command, code, stderr, tc.refuse)
	}
}

// A COMMIT ON MAIN IS REFUSED BY THE BRANCH IT LANDS ON, read in the repository
// the command names.
func TestTheBashHookJudgesTheBranchACommitLandsOn(t *testing.T) {
	t.Parallel()

	work := claudeHookRepo(t, bothRefs)
	onMain := claudeHookRepo(t, repoOnMainOnly)

	// A first commit, on a branch that has no commit yet.
	for branch, refuse := range map[string]string{"trunk": "", "main": "never commits to main"} {
		unborn := t.TempDir()
		hookGit(t, unborn, "init", "-q", "-b", branch)

		code, stderr := runClaudeGuard(t, "bash", unborn, bashPayload(unborn, "git commit -m first"))
		expectHook(t, "a first commit on unborn "+branch, code, stderr, refuse)
	}

	for _, tc := range []struct{ root, command, refuse string }{
		{onMain, "git commit -m 'oops'", "never commits to main"},
		{onMain, "git checkout feature -- README.md && git commit -m x", "never commits to main"},
		{work, "git -C " + onMain + " commit -m x", "never commits to main"},
		{work, "cd " + onMain + " && git commit -m x", "never commits to main"},
		{work, "git commit -m x", ""},
		{onMain, "git push origin HEAD", "never pushes to main"},
		{work, "(cd " + onMain + " && git status); git commit -m x", ""},
		{onMain, "(cd " + work + " && git status); git commit -m x", "never commits to main"},
		{work, "env -C " + onMain + " git commit -m x", "never commits to main"},
		{onMain, "if git diff --cached --quiet; then echo clean; else git commit -m x; fi", "never commits to main"},
	} {
		code, stderr := runClaudeGuard(t, "bash", tc.root, bashPayload(tc.root, tc.command))
		expectHook(t, tc.command, code, stderr, tc.refuse)
	}

	code, stderr := runClaudeGuard(t, "bash", work, bashPayload(work, "git commit -m x"), "PATH="+t.TempDir())
	expectHook(t, "a commit with no git on PATH", code, stderr, "git could not be run")
}

// A REPOSITORY CONFIGURED TO REBASE ON PULL IS REFUSED A BARE PULL.
func TestTheBashHookRefusesAPullThatWouldRebase(t *testing.T) {
	t.Parallel()

	root := claudeHookRepo(t, bothRefs)
	hookGit(t, root, "config", "pull.rebase", "true")

	code, stderr := runClaudeGuard(t, "bash", root, bashPayload(root, "git pull"))
	expectHook(t, "git pull with pull.rebase", code, stderr, "pull.rebase")

	code, stderr = runClaudeGuard(t, "bash", root, bashPayload(root, "git pull --no-rebase"))
	expectHook(t, "git pull --no-rebase with pull.rebase", code, stderr, "")
}

// THE BASH GUARD REFUSES THE FULL GATES, which CI runs on every push, and points
// at scripts/gates/verify.sh; it lets every targeted check through, and a
// command line it cannot read.
func TestTheBashHookRefusesTheFullGatesAndLetsTargetedChecksThrough(t *testing.T) {
	t.Parallel()

	root := claudeHookRepo(t, bothRefs)
	hookWrite(t, root, "go.mod", "module github.com/junioryono/billet\n\ngo 1.26\n")
	hookWrite(t, root, "tools/lint/go.mod", "module github.com/junioryono/billet/tools/lint\n\ngo 1.26\n")
	// A REAL GATE SCRIPT AND A SYMLINK TO ITS DIRECTORY, so a path through the
	// link is judged by the file it resolves to.
	hookWrite(t, root, "scripts/gates/test.sh", "#!/bin/sh\n")
	if err := os.Symlink(filepath.Join(root, "scripts", "gates"), filepath.Join(root, "g")); err != nil {
		t.Fatal(err)
	}
	// TWO SHELLS NAMED sh, one bash and one dash by the file each resolves to, in
	// directories of their own, so a shell is judged from the command's directory.
	for dir, target := range map[string]string{"b": "bash", "d": "dash"} {
		hookWrite(t, root, "shells/bin/"+target, "#!/bin/sh\n")

		if err := os.MkdirAll(filepath.Join(root, "shells", dir), 0o755); err != nil {
			t.Fatal(err)
		}

		if err := os.Symlink(filepath.Join(root, "shells", "bin", target), filepath.Join(root, "shells", dir, "sh")); err != nil {
			t.Fatal(err)
		}
	}

	const (
		gate      = "is a full gate, which CI runs"
		tree      = "over the whole tree"
		race      = "-race` across several packages"
		module    = "over the whole module"
		files     = "lint packages, never files"
		parallel  = "never --allow-parallel-runners"
		worktrees = "outside .claude/worktrees/"
	)

	for _, tc := range []struct{ command, refuse string }{
		{"scripts/gates/check.sh", gate},
		{"./scripts/gates/check.sh", gate},
		{"scripts/gates/check.sh > /tmp/check.log", gate},
		{"scripts/gates/check.sh >/tmp/check.log 2>&1", gate},
		{"bash scripts/gates/check.sh", gate},
		{"sh -x ./scripts/gates/test.sh", gate},
		{"cd scripts/gates && ./check.sh", gate},
		{root + "/scripts/gates/lint.sh", gate},
		{"CHECK_LOCK=/tmp/x scripts/gates/check.sh", gate},
		{"bash scripts/gates/test.sh -n", gate},
		{"bash -o pipefail scripts/gates/check.sh", gate},
		{"bash -x -- scripts/gates/check.sh", gate},
		{"cd scripts/gates && bash test.sh", gate},
		{"g/test.sh", gate},
		{"bash g/test.sh", gate},
		{"bash scripts/gates/check.sh -c ':'", gate},
		{"bash -n +n scripts/gates/check.sh", gate},
		{"bash - scripts/gates/check.sh", gate},
		{"bash -eo pipefail scripts/gates/check.sh", gate},
		{"zsh --emulate sh scripts/gates/check.sh", gate},
		{"bash --rcfile x scripts/gates/check.sh", gate},
		{"bash --init-file x scripts/gates/check.sh", gate},
		{"bash +o posix scripts/gates/check.sh", gate},
		{"bash -O extglob scripts/gates/check.sh", gate},
		{"bash +O extglob scripts/gates/check.sh", gate},
		{". scripts/gates/check.sh", gate},
		{"source scripts/gates/check.sh", gate},
		{"source -- scripts/gates/check.sh", gate},
		{"cd scripts/gates && . check.sh", gate},
		{"cd scripts/gates && source -p . test.sh", gate},
		{"source -p" + root + "/scripts/gates test.sh", gate},
		{"source -p /nowhere: test.sh", ""},
		{"bash -s +c scripts/gates/check.sh", gate},
		{"bash +c 'scripts/gates/check.sh'", gate},
		{"bash +s scripts/gates/check.sh", ""},
		{"zsh +s scripts/gates/check.sh", gate},
		{"zsh -s +s scripts/gates/check.sh", gate},
		{"sh +s scripts/gates/check.sh", shPlusS(t, gate)},
		{"/bin/bash +s scripts/gates/check.sh", ""},
		{"cd shells/b && ./sh +s " + root + "/scripts/gates/check.sh", ""},
		{"cd shells/d && ./sh +s " + root + "/scripts/gates/check.sh", gate},
		{"shells/b/sh +s scripts/gates/check.sh", ""},
		{"dash -s scripts/gates/check.sh", ""},
		{"bash g/../gates/test.sh", gate},
		{"bash -o pipefail -c 'git push --force origin junior-oct03-work'", "force"},
		{"golangci-lint run > /tmp/lint.log", module},
		{"golangci-lint --color always run ./...", module},
		{"golangci-lint run --allow-parallel-runners=true ./internal/alloc", parallel},
		{"go test -race ./internal/alloc ./internal/state > /tmp/test.log", race},
		{"go test -run '>' ./...", tree},
		{"go test -run \">\" ./...", tree},
		{"go test -run \\> ./...", tree},
		{"2>&1 scripts/gates/check.sh", gate},
		{"2>'/tmp/a log' scripts/gates/check.sh", gate},
		{">\"/tmp/a log\" scripts/gates/check.sh", gate},
		{"go test -race 2>&1 ./internal/alloc ./internal/state", race},
		{"git push origin 2>&1", "must name what it pushes"},
		{"git push origin >/dev/null 2>&1", "must name what it pushes"},
		{"git push --force origin junior-oct03-work > /tmp/push.log 2>&1", "force"},
		{"git push origin 2>/dev/null main", "never pushes to main"},
		// A redirection whose target never closes its quote leaves the line
		// unreadable, and the git rules still refuse what they cannot read.
		{"echo x >'unterminated; git push --force origin junior-oct03-work", "could not be parsed"},
		{"echo x >\"unterminated; git rebase main", "could not be parsed"},
		{"echo x >\"$(printf 'unterminated; git rebase main", "could not be parsed"},
		// A target holding a quoted command substitution is one word.
		{"git push origin junior-oct03-work >\"$(printf '\"')\"", ""},
		{"git push origin junior-oct03-work >$(printf '%s' \"a)b\").log; git rebase main", "merging"},
		{"scripts/gates/check.sh >\"$(date '+%H)%M').log\"", gate},
		{">$(printf ')') scripts/gates/check.sh", "`scripts/gates/check.sh` is a full gate"},
		{">$(printf ')') scripts/gates/verify.sh", ""},
		{">$(mktemp) scripts/gates/verify.sh", ""},
		// A target that runs across a line can hide a quote or a parenthesis in a
		// comment or a heredoc, so the line is unreadable, which the git rules
		// refuse and the gate rules let through.
		{"echo x >$(printf log # (\n); git rebase main # )", "could not be parsed"},
		{"echo x >$(cat <<'EOF'\n(\nEOF\n); git rebase main # )", "could not be parsed"},
		{"git >$(printf log\n) rebase main", "could not be parsed"},
		{"echo x >$(printf log # '\n); git rebase main # ')", "could not be parsed"},
		{"echo x >$(printf log # '\\\n); git rebase main # ')", "could not be parsed"},
		{">$(printf log\n) scripts/gates/check.sh", ""},
		{"scripts/gates/verify.sh > /tmp/verify.log\ngit status", ""},
		{"git push origin junior-oct03-work > /tmp/a\\\nb.log", ""},
		// A line continuation joins words before a descriptor is read.
		{"scripts/gates/check.sh 2\\\n>/dev/null", gate},
		{"scripts/gates/test.sh > \\\n /tmp/test.log", gate},
		{"git push origin main\\\n2>/dev/null", ""},
		{"git push origin junior-oct03-work 2>\\\n/dev/null", ""},
		{"scripts/gates/test.sh", gate},
		{"scripts/gates/lint.sh", gate},
		{"scripts/gates/lint.sh --fix", gate},
		{"scripts/gates/lint-custom.sh", gate},
		{"scripts/gates/cross.sh", gate},
		{"scripts/gates/check-steps.sh", gate},
		{"scripts/gates/test.sh --report", gate},
		{"scripts/gates/build.sh && scripts/gates/lint.sh", gate},
		{"CLAUDE_FULL_CHECKS=1 scripts/gates/check.sh", gate},
		{"env CLAUDE_FULL_CHECKS=1 scripts/gates/check.sh", gate},
		{"export CLAUDE_FULL_CHECKS=1; scripts/gates/check.sh", gate},
		{"nice -n 10 scripts/gates/check.sh", gate},
		{"timeout 600 scripts/gates/check.sh", gate},
		{"timeout -k 10 600 scripts/gates/test.sh", gate},
		{"bash -c 'scripts/gates/check.sh'", gate},
		{"scripts/gates/build.sh && scripts/gates/check.sh 2>&1 | tail -20", gate},
		{"go test ./...", tree},
		{"go test -count=1 -race ./...", tree},
		{"GOMAXPROCS=4 go test -p 2 ./...", tree},
		{"go test all", tree},
		{"go test github.com/junioryono/billet/...", tree},
		{"cd tools/lint && cd ../.. && go test ./...", tree},
		{"go -C . test ./...", tree},
		{"go test -race ./internal/alloc ./internal/state", race},
		{"go test -race ./internal/...", race},
		{"go test -race=true -run TestX ./internal/alloc ./internal/node", race},
		{"golangci-lint run", module},
		{"golangci-lint run --allow-serial-runners", module},
		{"golangci-lint run --timeout 5m", module},
		{"golangci-lint run ./...", module},
		{"golangci-lint run internal/alloc/alloc.go", files},
		{"golangci-lint run --allow-parallel-runners ./internal/alloc", parallel},
		{"git worktree add ../billet-feature -b junior-oct15-x", worktrees},
		{"git worktree add /tmp/billet-x origin/main", worktrees},
		{"git worktree add -b junior-oct15-x ../elsewhere", worktrees},
		{"git worktree add .claude/other/x", worktrees},
		{"scripts/gates/verify.sh", ""},
		{"scripts/gates/docs.sh", ""},
		{"scripts/gates/build.sh", ""},
		{"scripts/gates/fmt-check.sh", ""},
		{"scripts/gates/terraform.sh fmt validate", ""},
		{"scripts/gates/dist.sh", ""},
		{"sqlc diff", ""},
		{"make check", ""},
		{"bash -n scripts/gates/check.sh", ""},
		{"bash -xn scripts/gates/check.sh", ""},
		{"bash -o noexec scripts/gates/check.sh", ""},
		{"bash -s scripts/gates/check.sh", ""},
		{"cd scripts/gates && test.sh", ""},
		{"bash -c 'echo scripts/gates/check.sh'", ""},
		{"cat scripts/gates/check.sh", ""},
		{". scripts/gates/env.sh", ""},
		{"source", ""},
		{"source test.sh", ""},
		{"source -x scripts/gates/check.sh", ""},
		{"source -p /nowhere test.sh", ""},
		{"/tmp/other/scripts/gates/check.sh", ""},
		{"nice -n 10 scripts/gates/verify.sh", ""},
		{"scripts/gates/verify.sh > /tmp/verify.log 2>&1", ""},
		{"2>&1 scripts/gates/verify.sh", ""},
		{"scripts/gates/lint.sh2>/dev/null", ""},
		{"&>/tmp/verify.log scripts/gates/verify.sh", ""},
		{"go test -run '>' ./internal/alloc", ""},
		{"golangci-lint run 2>/dev/null --allow-serial-runners ./internal/alloc", ""},
		{"git push -u origin junior-oct03-work 2>&1 | tail -5", ""},
		{"git commit -m 'a > b' 2>&1", ""},
		{"go test -race ./internal/alloc > /tmp/test.log", ""},
		{"go test -race -run TestX ./internal/alloc 2>&1 | tail -20", ""},
		{"golangci-lint run --allow-serial-runners ./internal/alloc > /tmp/lint.log", ""},
		{"golangci-lint run --allow-parallel-runners=false ./internal/alloc", ""},
		{"echo scripts/gates/check.sh", ""},
		{"grep -n check.sh scripts/gates/check-steps.sh", ""},
		{"go build ./...", ""},
		{"go vet ./internal/alloc", ""},
		{"go test ./internal/alloc", ""},
		{"go test ./internal/alloc ./internal/state", ""},
		{"go test ./internal/...", ""},
		{"go test -run TestX ./internal/alloc", ""},
		{"go test -race -run TestX ./internal/alloc", ""},
		{"go test -race", ""},
		{"go test -race=false ./internal/alloc ./internal/state", ""},
		{"go test -run ./... ./internal/alloc", ""},
		{"cd tools/lint && go test ./...", ""},
		{"go test -C tools/lint ./...", ""},
		{"golangci-lint run --allow-serial-runners ./internal/alloc", ""},
		{"golangci-lint run --timeout 5m ./internal/alloc ./internal/state", ""},
		{"golangci-lint version", ""},
		{"golangci-lint fmt", ""},
		{"git worktree add .claude/worktrees/feature -b junior-oct15-x origin/main", ""},
		{"git worktree add -b junior-oct15-x .claude/worktrees/x origin/main", ""},
		{"git worktree add " + filepath.Join(root, ".claude", "worktrees", "y"), ""},
		{"git -C " + root + " worktree add .claude/worktrees/z", ""},
		{"git worktree add /private/tmp/claude-501/project/session/scratchpad/wt", ""},
		{"git worktree add /tmp/claude-501/project/session/scratchpad/wt", ""},
		{"git worktree list", ""},
		{"git worktree remove ../billet-feature", ""},
		// What cannot be read passes: these judgements fail open.
		{"scripts/gates/check.sh '", ""},
		{"go test ./... \"", ""},
	} {
		code, stderr := runClaudeGuard(t, "bash", root, bashPayload(root, tc.command))
		expectHook(t, tc.command, code, stderr, tc.refuse)
	}

	// A sourced name is the first readable file PATH holds, so a helper of the
	// same name earlier on PATH is what runs.
	helpers := t.TempDir()
	hookWrite(t, helpers, "test.sh", "echo a helper\n")

	gates := filepath.Join(root, "scripts", "gates")
	path := func(dirs ...string) string {
		return "PATH=" + strings.Join(append(dirs, os.Getenv("PATH")), string(os.PathListSeparator))
	}

	// Only the hook's own environment lifts the gate refusals, and only to 1; a CI
	// environment skips every one of these judgements.
	for _, tc := range []struct{ env, command, refuse string }{
		{"CLAUDE_FULL_CHECKS=1", "scripts/gates/check.sh", ""},
		{"CLAUDE_FULL_CHECKS=1", "scripts/gates/test.sh", ""},
		{"CLAUDE_FULL_CHECKS=1", "go test -race ./...", ""},
		{"CLAUDE_FULL_CHECKS=1", "golangci-lint run", ""},
		{"CLAUDE_FULL_CHECKS=1", "git worktree add ../billet-feature", worktrees},
		{"CLAUDE_FULL_CHECKS=1", "scripts/gates/check.sh && git push origin main", "never pushes to main"},
		{"CLAUDE_FULL_CHECKS=0", "scripts/gates/check.sh", gate},
		{"CLAUDE_FULL_CHECKS=", "scripts/gates/check.sh", gate},
		{"CI=true", "scripts/gates/check.sh", ""},
		{"CI=true", "go test -race ./...", ""},
		{"CI=true", "git worktree add ../billet-feature", ""},
		{"CI=true", "git rebase main", "merging"},
		// A sourced name without a slash is looked for in the hook's PATH.
		{path(gates), "source test.sh", gate},
		{path(helpers, gates), "source test.sh", ""},
		{path(gates, helpers), "source test.sh", gate},
		{path(helpers), "cd scripts/gates && source test.sh", ""},
		// A shell named without a slash is the first sh PATH holds, a relative
		// entry read from the command's directory.
		{path(filepath.Join(root, "shells", "b")), "sh +s scripts/gates/check.sh", ""},
		{path(filepath.Join(root, "shells", "d")), "sh +s scripts/gates/check.sh", gate},
		{path(filepath.Join("shells", "b")), "sh +s scripts/gates/check.sh", ""},
		{path(filepath.Join("shells", "b")), "cd scripts && sh +s gates/check.sh", shPlusS(t, gate)},
	} {
		code, stderr := runClaudeGuard(t, "bash", root, bashPayload(root, tc.command), tc.env)
		expectHook(t, tc.env+" "+tc.command, code, stderr, tc.refuse)
	}
}

// THE FORMAT HOOK REFORMATS GO, LEAVES EVERYTHING ELSE, AND NEVER REFUSES.
func TestTheFormatHookRunsGofmtOnGoFilesOnlyAndNeverRefuses(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	goFile := filepath.Join(root, "x.go")
	broken := filepath.Join(root, "broken.go")
	text := filepath.Join(root, "x.md")

	for path, body := range map[string]string{
		goFile: "package x\nfunc  F( ) {}\n", broken: "package x\nfunc  F( {\n", text: "func  F( ) {}\n",
	} {
		hookWrite(t, root, filepath.Base(path), body)
	}

	format := func(path string, extraEnv ...string) {
		t.Helper()

		payload := map[string]any{"tool_name": "Write", "cwd": root, "tool_input": map[string]any{"file_path": path}}
		if code, stderr := runClaudeGuard(t, "format", root, payload, extraEnv...); code != 0 {
			t.Errorf("%s: format exited %d: %s", path, code, stderr)
		}
	}

	// Without gofmt on PATH the hook does nothing and still succeeds.
	format(goFile, "PATH="+t.TempDir())

	if got, err := os.ReadFile(goFile); err != nil || string(got) != "package x\nfunc  F( ) {}\n" {
		t.Errorf("the Go file changed with no gofmt on PATH: %q (%v)", got, err)
	}

	for label, body := range map[string]string{
		"not JSON": "not json", "not an object": "[1]",
	} {
		if code, stderr := runClaudeGuardRaw(t, "format", root, []byte(body)); code != 0 {
			t.Errorf("format with %s input exited %d: %s", label, code, stderr)
		}
	}

	if _, err := exec.LookPath("gofmt"); err != nil {
		t.Skip("gofmt is not on PATH, so the formatting itself cannot be checked here")
	}

	for _, path := range []string{goFile, broken, text} {
		format(path)
	}

	for path, want := range map[string]string{
		goFile: "package x\n\nfunc F() {}\n", broken: "package x\nfunc  F( {\n", text: "func  F( ) {}\n",
	} {
		if got, err := os.ReadFile(path); err != nil || string(got) != want {
			t.Errorf("%s reads %q (%v) after the hook, want %q", filepath.Base(path), got, err, want)
		}
	}
}
