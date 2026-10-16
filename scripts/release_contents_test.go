package scripts_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// releaseFixture is a repository shaped the way billet's is when a new series is
// cut: a previous release on its own branch, carrying a migration main also
// gained, and main moving on through merged and squashed pull requests.
type releaseFixture struct {
	repo      string
	candidate string
	// earlier is a main commit before the candidate, which a stale preview names.
	earlier string
}

func newReleaseFixture(t *testing.T) releaseFixture {
	t.Helper()

	repo := newRepo(t)

	write := func(path, body string) {
		t.Helper()

		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(full), err)
		}

		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}

		fixtureGit(t, repo, "add", "--", path)
	}

	// A PULL REQUEST MERGED WITH A MERGE COMMIT: the change is on the second
	// parent, and the first-parent history sees only the merge's subject.
	merged := func(branch, subject string, files map[string]string) {
		t.Helper()

		fixtureGit(t, repo, "checkout", "--quiet", "-b", branch)
		for path, body := range files {
			write(path, body)
		}
		commit(t, repo, "work on "+branch)
		fixtureGit(t, repo, "checkout", "--quiet", "main")
		fixtureGit(t, repo, "merge", "--quiet", "--no-ff", "-m", subject, branch)
	}

	// AND ONE SQUASHED: one commit on main carrying the change.
	squashed := func(subject string, files map[string]string) {
		t.Helper()

		for path, body := range files {
			write(path, body)
		}
		commit(t, repo, subject)
	}

	write("internal/state/migrations/0001_init.sql", "CREATE TABLE a (x);\n")
	write("internal/state/pgmigrations/0001_init.sql", "CREATE TABLE a (x);\n")
	commit(t, repo, "State: Start the ledger (#1)")

	// THE PREVIOUS SERIES, cut from here and carrying a migration main also has.
	fixtureGit(t, repo, "checkout", "--quiet", "-b", "release/v0.4")
	commit(t, repo, "Release v0.4.0")
	fixtureGit(t, repo, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
	write("internal/state/migrations/0002_carried.sql", "ALTER TABLE a ADD y;\n")
	write("internal/state/pgmigrations/0002_carried.sql", "ALTER TABLE a ADD y;\n")
	commit(t, repo, "Release: Carry the column to v0.4 (#3)")
	commit(t, repo, "Release v0.4.1")
	fixtureGit(t, repo, "tag", "-a", "v0.4.1", "-m", "v0.4.1")
	fixtureGit(t, repo, "checkout", "--quiet", "main")

	squashed("State: Add the column (#2)", map[string]string{
		"internal/state/migrations/0002_carried.sql":   "ALTER TABLE a ADD y;\n",
		"internal/state/pgmigrations/0002_carried.sql": "ALTER TABLE a ADD y;\n",
	})
	merged("placement", "Capacity: Change placement (#4)", map[string]string{
		"internal/alloc/place.go": "package alloc\n",
	})
	squashed("Tests: Hold placement to its order (#5)", map[string]string{
		"internal/alloc/place_test.go": "package alloc\n",
	})

	earlier := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))

	merged("wire", "Wire: Add a field (#6)", map[string]string{
		"internal/nodeapi/types.go": "package nodeapi\n",
	})
	squashed("State: Record the window (#7)", map[string]string{
		"internal/state/migrations/0003_window.sql":   "ALTER TABLE a ADD z;\n",
		"internal/state/pgmigrations/0003_window.sql": "ALTER TABLE a ADD z;\n",
	})
	squashed("Docs: Explain the scheduler (#8)", map[string]string{
		"docs/scheduler.md":         "words\n",
		"internal/server/CLAUDE.md": "words\n",
	})
	squashed("fix a typo", map[string]string{"README.md": "billet\n"})

	return releaseFixture{
		repo:      repo,
		candidate: strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD")),
		earlier:   earlier,
	}
}

// runContents runs release-contents.sh in the fixture with env added, and
// returns its stdout, its stderr, what it wrote to GITHUB_OUTPUT and its error.
func runContents(t *testing.T, repo string, env ...string) (string, string, string, error) {
	t.Helper()

	script, err := filepath.Abs("release-contents.sh")
	if err != nil {
		t.Fatalf("absolute script path: %v", err)
	}

	output := filepath.Join(t.TempDir(), "github-output")

	cmd := exec.CommandContext(t.Context(), script)
	cmd.Dir = repo
	cmd.Env = append(append(fixtureGitEnv(), "GITHUB_OUTPUT="+output), env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()

	written, readErr := os.ReadFile(output)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read GITHUB_OUTPUT: %v", readErr)
	}

	return stdout.String(), stderr.String(), string(written), err
}

// section is the report's text from a heading to the next heading of the same
// level or above.
func section(t *testing.T, report, heading string) string {
	t.Helper()

	start := strings.Index(report, heading+"\n")
	if start < 0 {
		t.Fatalf("the report has no %q section:\n%s", heading, report)
	}

	rest := report[start+len(heading)+1:]
	if end := strings.Index(rest, "\n### "); end >= 0 {
		rest = rest[:end]
	}

	return rest
}

// THE REPORT SAYS WHAT THE RELEASE CARRIES, and marks what can hurt.
//
// The previous release is on its own branch, as it is for every new series, so
// the list is main since the branch point; the migration that release already
// carried is not counted as new, and the one it did not carry leads the report.
// A change is marked by the source it ships, not by a test or a CLAUDE.md beside
// it, and a pull request merged with a merge commit is judged by its whole change.
func TestTheReleaseReportSaysWhatTheReleaseCarries(t *testing.T) {
	t.Parallel()

	f := newReleaseFixture(t)

	report, stderr, written, err := runContents(t, f.repo,
		"TAG=v0.5.0", "PREVIOUS=v0.4.1", "CANDIDATE="+f.candidate)
	if err != nil {
		t.Fatalf("release-contents.sh: %v\n%s", err, stderr)
	}

	if written != "confirmed=false\n" {
		t.Errorf("a preview wrote %q to GITHUB_OUTPUT, want confirmed=false", written)
	}

	banner := strings.Index(report, "**This release migrates the ledger, and it cannot be rolled back.** It adds 1 migration(s)")
	flagged := strings.Index(report, "### Flagged")
	if banner < 0 || flagged < 0 || banner > flagged {
		t.Fatalf("the migration is not announced ahead of the changes:\n%s", report)
	}

	for _, want := range []string{
		"> - `internal/state/migrations/0003_window.sql`\n",
		"> - `internal/state/pgmigrations/0003_window.sql`\n",
		"`v0.4.1` is on another branch",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report is missing %q:\n%s", want, report)
		}
	}

	// THE CARRIED MIGRATION IS ALREADY IN v0.4.1, and the first one in every
	// release: neither is new.
	for _, carried := range []string{"0002_carried.sql", "0001_init.sql"} {
		if strings.Contains(report, carried) {
			t.Errorf("the report counts %s, which v0.4.1 already carries:\n%s", carried, report)
		}
	}

	flags := section(t, report, "### Flagged")
	for _, want := range []string{
		"- **scheduler**: Capacity: Change placement (#4)",
		"- **node wire**: Wire: Add a field (#6)",
		"- **ledger schema**: State: Record the window (#7)",
	} {
		if !strings.Contains(flags, want) {
			t.Errorf("the flagged changes are missing %q:\n%s", want, flags)
		}
	}

	// A TEST, A CLAUDE.md AND A MIGRATION v0.4.1 ALREADY HAS mark nothing.
	for _, unmarked := range []string{"Tests: Hold placement", "Docs: Explain", "State: Add the column", "fix a typo"} {
		if strings.Contains(flags, unmarked) {
			t.Errorf("%q is flagged, and it changes no scheduler, wire or schema:\n%s", unmarked, flags)
		}
	}

	areas := section(t, report, "### By area (7 changes)")
	for _, want := range []string{
		"#### Capacity\n\n- Capacity: Change placement (#4) (`",
		"#### State\n\n- State: Record the window (#7) (`",
		"#### Other\n\n- fix a typo (`",
		"#### Tests\n\n- Tests: Hold placement to its order (#5) (`",
	} {
		if !strings.Contains(areas, want) {
			t.Errorf("the grouping is missing %q:\n%s", want, areas)
		}
	}

	// HISTORY ORDER WITHIN AN AREA: newest first, as the log reads.
	if a, b := strings.Index(areas, "State: Record the window"), strings.Index(areas, "State: Add the column"); a < 0 || b < 0 || a > b {
		t.Errorf("the State area is not in history order:\n%s", areas)
	}

	if strings.Contains(report, "Release v0.4") || strings.Contains(report, "Carry the column") {
		t.Errorf("the report lists the previous release's own commits:\n%s", report)
	}

	if !strings.Contains(report, "Not looked up: no repository is named") {
		t.Errorf("with no repository named the CI section should say it did not look:\n%s", report)
	}

	if !strings.Contains(report, "v0.5.0@"+f.candidate) {
		t.Errorf("the preview does not print the confirmation to paste:\n%s", report)
	}
}

// AND A RELEASE WITHOUT ANY OF THAT SAYS SO, rather than printing an empty
// section an operator could read as the report having failed.
func TestAReleaseThatTouchesNothingFlaggedSaysSo(t *testing.T) {
	t.Parallel()

	f := newReleaseFixture(t)

	// From the commit after the window migration: the docs and the typo only.
	previous := strings.TrimSpace(gitOutput(t, f.repo, "rev-parse", "HEAD~2"))
	fixtureGit(t, f.repo, "tag", "-a", "v0.5.0", "-m", "v0.5.0", previous)

	report, stderr, _, err := runContents(t, f.repo,
		"TAG=v0.5.1", "PREVIOUS=v0.5.0", "CANDIDATE="+f.candidate)
	if err != nil {
		t.Fatalf("release-contents.sh: %v\n%s", err, stderr)
	}

	if strings.Contains(report, "migrates the ledger") {
		t.Errorf("a release with no new migration announced one:\n%s", report)
	}

	if !strings.Contains(section(t, report, "### Flagged"),
		"Nothing here touches the scheduler, the ledger schema or the node wire.") {
		t.Errorf("an unflagged release does not say so:\n%s", report)
	}

	if strings.Contains(report, "is on another branch") {
		t.Errorf("a patch on its own line is described as another branch's:\n%s", report)
	}
}

// ONLY A CONFIRMATION NAMING THIS TAG AND THIS COMMIT CUTS, so a preview copied
// before main moved, a different version, a prefix, or anything that is not one
// line cuts nothing; and a stale confirmation is a failure, not a quiet preview.
func TestOnlyTheConfirmationThePreviewPrintedCuts(t *testing.T) {
	t.Parallel()

	f := newReleaseFixture(t)
	exact := "v0.5.0@" + f.candidate

	for _, tc := range []struct {
		name        string
		confirm     string
		wantStatus  int
		wantWritten string
		wantSay     string
	}{
		{name: "a preview", confirm: "", wantWritten: "confirmed=false\n", wantSay: "Nothing was cut."},
		{name: "the line the preview printed", confirm: exact, wantWritten: "confirmed=true\n",
			wantSay: "Confirmed as `" + exact + "`"},
		{name: "a preview taken before main moved", confirm: "v0.5.0@" + f.earlier, wantStatus: 1,
			wantWritten: "confirmed=false\n", wantSay: "**Not cut.**"},
		{name: "another version", confirm: "v0.5.1@" + f.candidate, wantStatus: 1,
			wantWritten: "confirmed=false\n", wantSay: "**Not cut.**"},
		{name: "an abbreviated commit", confirm: "v0.5.0@" + f.candidate[:12], wantStatus: 1,
			wantWritten: "confirmed=false\n", wantSay: "**Not cut.**"},
		{name: "two lines", confirm: exact + "\n" + exact, wantStatus: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			report, stderr, written, err := runContents(t, f.repo,
				"TAG=v0.5.0", "PREVIOUS=v0.4.1", "CANDIDATE="+f.candidate, "CONFIRM="+tc.confirm)

			status := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("release-contents.sh did not run: %v", err)
				}

				status = exit.ExitCode()
			}

			if status != tc.wantStatus {
				t.Fatalf("exit status %d, want %d\n%s\n%s", status, tc.wantStatus, report, stderr)
			}

			// THE OUTPUT IS WHAT GATES THE CUT, so it is asserted exactly: nothing
			// but confirmed=true may ever let the cut step run.
			if written != tc.wantWritten {
				t.Errorf("GITHUB_OUTPUT = %q, want %q", written, tc.wantWritten)
			}

			if tc.wantSay != "" && !strings.Contains(report, tc.wantSay) {
				t.Errorf("the report does not say %q:\n%s", tc.wantSay, report)
			}
		})
	}
}

// WHAT CI SAID ABOUT THE CANDIDATE, with its three answers kept apart: runs,
// none, and could not ask. A failed lookup read as "no run" would tell an
// operator nothing tested the commit when the truth is that billet does not know.
func TestTheReleaseReportShowsCIOnTheCandidate(t *testing.T) {
	t.Parallel()

	f := newReleaseFixture(t)

	for _, tc := range []struct {
		name    string
		mode    string
		want    []string
		notWant []string
	}{
		{name: "runs", mode: "runs", want: []string{
			"- push: completed, success (https://example.invalid/runs/1)\n",
			"- pull_request: in_progress, none (https://example.invalid/runs/2)\n",
		}, notWant: []string{"No CI run", "Could not read"}},
		{name: "none", mode: "none", want: []string{"**No CI run on this commit**"},
			notWant: []string{"Could not read"}},
		{name: "could not ask", mode: "fail", want: []string{
			"**Could not read CI for this commit**",
			"HTTP 403: Resource not accessible by integration",
		}, notWant: []string{"No CI run"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			bin := t.TempDir()
			argsFile := filepath.Join(bin, "args")

			// The jq filter is gh's; this prints what it would have produced.
			fake := "#!/bin/sh\n" +
				"printf '%s\\n' \"$@\" > \"" + argsFile + "\"\n" +
				"case \"$BILLET_TEST_GH_MODE\" in\n" +
				"  runs) printf 'push\\tcompleted\\tsuccess\\thttps://example.invalid/runs/1\\n" +
				"pull_request\\tin_progress\\tnone\\thttps://example.invalid/runs/2\\n' ;;\n" +
				"  none) ;;\n" +
				"  *) echo 'HTTP 403: Resource not accessible by integration' >&2; exit 1 ;;\n" +
				"esac\n"

			if err := forkSafeWriteFile(filepath.Join(bin, "gh"), []byte(fake), 0o755); err != nil {
				t.Fatalf("write the fake gh: %v", err)
			}

			report, stderr, _, err := runContents(t, f.repo,
				"TAG=v0.5.0", "PREVIOUS=v0.4.1", "CANDIDATE="+f.candidate,
				"GITHUB_REPOSITORY=junioryono/billet", "BILLET_TEST_GH_MODE="+tc.mode,
				"PATH="+bin+":"+os.Getenv("PATH"))
			if err != nil {
				t.Fatalf("release-contents.sh failed on a CI lookup it only reports: %v\n%s", err, stderr)
			}

			ci := section(t, report, "### CI on `"+f.candidate[:12]+"`")

			for _, want := range tc.want {
				if !strings.Contains(ci, want) {
					t.Errorf("the CI section is missing %q:\n%s", want, ci)
				}
			}

			for _, notWant := range tc.notWant {
				if strings.Contains(ci, notWant) {
					t.Errorf("the CI section says %q:\n%s", notWant, ci)
				}
			}

			args, err := os.ReadFile(argsFile)
			if err != nil {
				t.Fatalf("gh was never asked: %v", err)
			}

			for _, want := range []string{
				"repos/junioryono/billet/actions/workflows/ci.yml/runs\n",
				"head_sha=" + f.candidate + "\n",
			} {
				if !strings.Contains(string(args), want) {
					t.Errorf("gh was not asked about %q:\n%s", want, args)
				}
			}
		})
	}
}

// WHAT IT CANNOT READ, IT REFUSES: a commit that is not a full id or not in the
// repository, and a previous release that is not a tag, rather than a report about
// something else.
func TestTheReleaseReportRefusesInputsItCannotRead(t *testing.T) {
	t.Parallel()

	f := newReleaseFixture(t)

	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{
		{"an abbreviated candidate", []string{"CANDIDATE=" + f.candidate[:12], "PREVIOUS=v0.4.1"},
			"must be a full commit id"},
		{"a candidate not in the repository", []string{"CANDIDATE=" + strings.Repeat("a", 40), "PREVIOUS=v0.4.1"},
			"is not a commit in this repository"},
		{"a previous release that is not a tag", []string{"CANDIDATE=" + f.candidate, "PREVIOUS=v0.3.9"},
			"the previous release v0.3.9 is not a tag"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, stderr, written, err := runContents(t, f.repo, append([]string{"TAG=v0.5.0"}, tc.env...)...)
			if err == nil {
				t.Fatal("release-contents.sh reported on an input it cannot read")
			}

			if !strings.Contains(stderr, tc.want) {
				t.Errorf("the refusal does not say %q:\n%s", tc.want, stderr)
			}

			if written != "" {
				t.Errorf("a refusal wrote %q to GITHUB_OUTPUT", written)
			}
		})
	}
}

// CUT RELEASE READS THE REPORT BEFORE IT CUTS, and cuts only on its confirmation.
//
// The script decides; this holds the workflow to asking it. The cut step and the
// release build run only on confirmed == 'true', the cut proves HEAD is the
// candidate the report described, and the confirmation reaches the script through
// the environment alone, never interpolated into a shell.
func TestCutReleaseCutsOnlyWhatItsReportConfirmed(t *testing.T) {
	t.Parallel()

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "cut-release.yml"))
	if err != nil {
		t.Fatalf("read cut-release.yml: %v", err)
	}

	workflow := string(body)

	candidate := strings.Index(workflow, "      - name: Find the candidate\n")
	contents := strings.Index(workflow, "      - name: Read what it carries\n")
	cut := strings.Index(workflow, "      - name: Cut it\n")
	tag := strings.Index(workflow, `git tag -a "${TAG}"`)
	releaseJob := strings.Index(workflow, "\n  release:\n")

	if candidate < 0 || contents <= candidate || cut <= contents || tag <= cut || releaseJob <= tag {
		t.Fatalf("the cut job's steps are missing or out of order: candidate %d, report %d, cut %d, "+
			"tag %d, release job %d", candidate, contents, cut, tag, releaseJob)
	}

	report := workflow[contents:cut]
	for _, want := range []string{
		"id: contents\n",
		"CONFIRM: ${{ inputs.confirm }}\n",
		"PREVIOUS: ${{ steps.plan.outputs.previous }}\n",
		"CANDIDATE: ${{ steps.candidate.outputs.commit }}\n",
		"set -euo pipefail\n",
		`scripts/release-contents.sh | tee -a "$GITHUB_STEP_SUMMARY"`,
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report step is missing %q:\n%s", want, report)
		}
	}

	cutStep := workflow[cut:releaseJob]
	for _, want := range []string{
		"if: steps.contents.outputs.confirmed == 'true'\n",
		`if [ "$(git rev-parse HEAD)" != "${CANDIDATE}" ]; then`,
	} {
		if !strings.Contains(cutStep, want) {
			t.Errorf("the cut step is missing %q", want)
		}
	}

	if !strings.Contains(workflow[releaseJob:], "if: needs.cut.outputs.confirmed == 'true'\n") {
		t.Error("the release build does not wait for a confirmed cut")
	}

	if !strings.Contains(workflow[:candidate], "confirmed: ${{ steps.contents.outputs.confirmed }}\n") {
		t.Error("the cut job does not hand the confirmation to the jobs after it")
	}

	// ONE MENTION: the environment line. Anywhere else it would be interpolated
	// into a shell, and this job can push.
	if n := strings.Count(workflow, "inputs.confirm"); n != 1 {
		t.Errorf("inputs.confirm appears %d times, want only the environment line", n)
	}
}

// AND CI RUNS ON A RELEASE BRANCH'S TIP, so the commit a patch is cut from has
// a run of its own for the report to show.
func TestCIRunsOnPushesToReleaseBranches(t *testing.T) {
	t.Parallel()

	assertContains(t, filepath.Join("..", ".github", "workflows", "ci.yml"),
		"  push:\n    branches: [main, 'release/**']\n")
}

// gitOutput runs git in a fixture and returns its stdout.
func gitOutput(t *testing.T, dir string, args ...string) string {
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
