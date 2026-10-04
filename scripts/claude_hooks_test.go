package scripts_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// claudeGuardScript is the one program every billet hook runs.
const claudeGuardScript = "../.claude/hooks/guard.py"

// hookEnv is the environment every fixture git command and every hook run
// gets: no global or system git configuration and no inherited GIT_ variable,
// so a developer's hooksPath or GIT_DIR cannot reach a fixture.
func hookEnv(extra ...string) []string {
	var env []string

	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "GIT_") {
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
		t.Fatalf("python3 is required by the hooks (and by make check): %v", err)
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
		{"make check && git push origin main", "never pushes to main"},
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
