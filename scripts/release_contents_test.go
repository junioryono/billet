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

	// A MIGRATION v0.4.1 PUBLISHED, CHANGED: its own alarm, and a schema mark.
	squashed("State: Rewrite the first migration (#9)", map[string]string{
		"internal/state/migrations/0001_init.sql": "CREATE TABLE a (x, w);\n",
	})

	// EVERY DIRECTORY THE MARKS NAME, two of them under names git would quote
	// without -z: a non-ASCII byte and a tab.
	squashed("Capacity: Place by name (#10)", map[string]string{
		"internal/alloc/café.go": "package alloc\n",
	})
	squashed("Listener: Serve the scale set (#11)", map[string]string{
		"internal/server/listener.go": "package server\n",
	})
	squashed("Plane: Answer a node (#12)", map[string]string{
		"internal/nodeplane/plane.go": "package nodeplane\n",
	})
	squashed("Client: Ask the plane (#13)", map[string]string{
		"internal/nodeclient/ask\tplane.go": "package nodeclient\n",
	})

	// A FIXTURE IS NOT SOURCE, even when it is Go.
	squashed("Tests: Add a placement fixture (#14)", map[string]string{
		"internal/alloc/testdata/fixture.go": "package fixture\n",
	})

	// A TAB IN A SUBJECT, which must not cut the title short.
	squashed("Docs: Say\twhat placement does (#15)", map[string]string{
		"README.md": "billet, placed\n",
	})

	// AND A MIGRATION WITH A TAB IN ITS NAME, which the status and the path are
	// split around, so its commit is marked only if the whole name survives.
	squashed("State: Add a tabbed migration (#16)", map[string]string{
		"internal/state/migrations/0004_tab\tname.sql": "ALTER TABLE a ADD v;\n",
	})

	return releaseFixture{
		repo:      repo,
		candidate: strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD")),
		earlier:   earlier,
	}
}

// contentsEnv is the environment release-contents.sh runs in under test: the
// fixtures' git, and none of the variables the script reads that a CI job sets
// for itself (GITHUB_REPOSITORY above all, which would send every case to the
// real API).
func contentsEnv() []string {
	read := map[string]bool{"TAG": true, "PREVIOUS": true, "CANDIDATE": true, "CONFIRM": true}

	env := make([]string, 0, len(fixtureGitEnv()))

	for _, kv := range fixtureGitEnv() {
		name, _, _ := strings.Cut(kv, "=")
		if read[name] || strings.HasPrefix(name, "GITHUB_") || strings.HasPrefix(name, "GH_") {
			continue
		}

		env = append(env, kv)
	}

	return env
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
	cmd.Env = append(append(contentsEnv(), "GITHUB_OUTPUT="+output), env...)

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
// carried is not counted as new, the one it did not carry leads the report, and
// the one it published and main changed raises its own alarm. A change is marked
// by the source it ships, not by a test, a fixture or a CLAUDE.md beside it, under
// whatever bytes its path holds, and a pull request merged with a merge commit is
// judged by its whole change.
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

	banner := strings.Index(report, "**This release migrates the ledger, and it cannot be rolled back.** It adds 2 migration(s)")
	flagged := strings.Index(report, "### Flagged")
	if banner < 0 || flagged < 0 || banner > flagged {
		t.Fatalf("the migration is not announced ahead of the changes:\n%s", report)
	}

	for _, want := range []string{
		"> - `internal/state/migrations/0003_window.sql`\n",
		"> - `internal/state/pgmigrations/0003_window.sql`\n",
		"> - `internal/state/migrations/0004_tab\tname.sql`\n",
		"**This release changes or removes a migration an earlier release published.**",
		"> - `internal/state/migrations/0001_init.sql` (M)\n",
		"`v0.4.1` is on another branch",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the report is missing %q:\n%s", want, report)
		}
	}

	// THE CARRIED MIGRATION IS ALREADY IN v0.4.1: not new, not changed.
	if strings.Contains(report, "0002_carried.sql") {
		t.Errorf("the report counts 0002_carried.sql, which v0.4.1 already carries:\n%s", report)
	}

	flags := section(t, report, "### Flagged")
	for _, want := range []string{
		"- **scheduler**: Capacity: Change placement (#4) (`",
		"- **node wire**: Wire: Add a field (#6) (`",
		"- **ledger schema**: State: Record the window (#7) (`",
		"- **ledger schema**: State: Rewrite the first migration (#9) (`",
		"- **scheduler**: Capacity: Place by name (#10) (`",
		"- **scheduler**: Listener: Serve the scale set (#11) (`",
		"- **node wire**: Plane: Answer a node (#12) (`",
		"- **node wire**: Client: Ask the plane (#13) (`",
		"- **ledger schema**: State: Add a tabbed migration (#16) (`",
	} {
		if !strings.Contains(flags, want) {
			t.Errorf("the flagged changes are missing %q:\n%s", want, flags)
		}
	}

	// A TEST, A FIXTURE, A CLAUDE.md AND A MIGRATION v0.4.1 ALREADY HAS mark
	// nothing.
	for _, unmarked := range []string{"Tests: Hold placement", "Tests: Add a placement fixture",
		"Docs: Explain", "State: Add the column", "fix a typo", "Docs: Say what placement does"} {
		if strings.Contains(flags, unmarked) {
			t.Errorf("%q is flagged, and it changes no scheduler, wire or schema:\n%s", unmarked, flags)
		}
	}

	areas := section(t, report, "### By area (15 changes)")
	for _, want := range []string{
		"#### Capacity\n\n- Capacity: Place by name (#10) (`",
		"#### Other\n\n- fix a typo (`",
		"#### Tests\n\n- Tests: Add a placement fixture (#14) (`",
		// THE WHOLE TITLE, its tab a space.
		"- Docs: Say what placement does (#15) (`",
	} {
		if !strings.Contains(areas, want) {
			t.Errorf("the grouping is missing %q:\n%s", want, areas)
		}
	}

	// HISTORY ORDER WITHIN AN AREA: newest first, as the log reads.
	newest := strings.Index(areas, "State: Rewrite the first migration")
	middle := strings.Index(areas, "State: Record the window")
	oldest := strings.Index(areas, "State: Add the column")

	if newest < 0 || middle < 0 || oldest < 0 || newest > middle || middle > oldest {
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

	fixtureGit(t, f.repo, "tag", "-a", "v0.5.0", "-m", "v0.5.0", f.candidate)

	if err := os.WriteFile(filepath.Join(f.repo, "README.md"), []byte("billet, again\n"), 0o600); err != nil {
		t.Fatalf("write README.md: %v", err)
	}

	fixtureGit(t, f.repo, "add", "README.md")
	commit(t, f.repo, "Docs: Say it again (#16)")

	candidate := strings.TrimSpace(gitOutput(t, f.repo, "rev-parse", "HEAD"))

	report, stderr, _, err := runContents(t, f.repo,
		"TAG=v0.5.1", "PREVIOUS=v0.5.0", "CANDIDATE="+candidate)
	if err != nil {
		t.Fatalf("release-contents.sh: %v\n%s", err, stderr)
	}

	if strings.Contains(report, "migrates the ledger") || strings.Contains(report, "changes or removes a migration") {
		t.Errorf("a release with no migration change announced one:\n%s", report)
	}

	if !strings.Contains(section(t, report, "### Flagged"),
		"Nothing here touches the scheduler, the ledger schema or the node wire.") {
		t.Errorf("an unflagged release does not say so:\n%s", report)
	}

	if strings.Contains(report, "is on another branch") {
		t.Errorf("a patch on its own line is described as another branch's:\n%s", report)
	}

	if !strings.Contains(report, "### By area (1 changes)") {
		t.Errorf("the patch is not one change:\n%s", report)
	}
}

// BILLET'S FIRST RELEASE IS READ AGAINST THE EMPTY TREE, so its migrations are
// announced like any other release's.
func TestTheFirstReleaseReportsItsMigrations(t *testing.T) {
	t.Parallel()

	repo := newRepo(t)

	for _, path := range []string{"internal/state/migrations/0001_init.sql", "internal/state/pgmigrations/0001_init.sql"} {
		full := filepath.Join(repo, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("create %s: %v", filepath.Dir(full), err)
		}

		if err := os.WriteFile(full, []byte("CREATE TABLE a (x);\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	fixtureGit(t, repo, "add", "-A")
	commit(t, repo, "State: Start the ledger (#1)")

	candidate := strings.TrimSpace(gitOutput(t, repo, "rev-parse", "HEAD"))

	report, stderr, _, err := runContents(t, repo, "TAG=v0.1.0", "PREVIOUS=", "CANDIDATE="+candidate)
	if err != nil {
		t.Fatalf("release-contents.sh: %v\n%s", err, stderr)
	}

	for _, want := range []string{
		"There is no earlier release",
		"It adds 1 migration(s)",
		"> - `internal/state/migrations/0001_init.sql`\n",
		"- **ledger schema**: State: Start the ledger (#1) (`",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("the first release's report is missing %q:\n%s", want, report)
		}
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
		{name: "a trailing space", confirm: exact + " ", wantStatus: 1,
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

			// THE WHOLE REQUEST, one argument per line, because the fake cannot
			// judge it: without -X GET the fields become a POST body and the real
			// API refuses, and the filter is what turns runs into these rows.
			want := "api\n--paginate\n-X\nGET\n" +
				"repos/junioryono/billet/actions/workflows/ci.yml/runs\n" +
				"-f\nhead_sha=" + f.candidate + "\n-f\nper_page=100\n" +
				"--jq\n.workflow_runs[] | [.event, .status, (.conclusion // \"none\"), .html_url] | @tsv\n"
			if string(args) != want {
				t.Errorf("gh was asked\n%s\nwant\n%s", args, want)
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

// cutWorkflow is the part of cut-release.yml these tests read.
type cutWorkflow struct {
	Jobs map[string]struct {
		If          string            `yaml:"if"`
		Needs       any               `yaml:"needs"`
		Outputs     map[string]string `yaml:"outputs"`
		Permissions map[string]string `yaml:"permissions"`
		Steps       []cutStep         `yaml:"steps"`
	} `yaml:"jobs"`
}

type cutStep struct {
	Name string            `yaml:"name"`
	ID   string            `yaml:"id"`
	If   string            `yaml:"if"`
	Env  map[string]string `yaml:"env"`
	Run  string            `yaml:"run"`
}

func readCutWorkflow(t *testing.T) cutWorkflow {
	t.Helper()

	body, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "cut-release.yml"))
	if err != nil {
		t.Fatalf("read cut-release.yml: %v", err)
	}

	var w cutWorkflow
	if err := yaml.Unmarshal(body, &w); err != nil {
		t.Fatalf("parse cut-release.yml: %v", err)
	}

	return w
}

// cutJobStep is the cut job's step with this name, and its position.
func cutJobStep(t *testing.T, w cutWorkflow, name string) (cutStep, int) {
	t.Helper()

	for i, step := range w.Jobs["cut"].Steps {
		if step.Name == name {
			return step, i
		}
	}

	t.Fatalf("the cut job has no step named %q", name)

	return cutStep{}, -1
}

// CUT RELEASE READS THE REPORT BEFORE IT CUTS, and cuts only on its confirmation.
//
// Read as YAML, so a condition commented out or moved to another step is not a
// condition: the cut step and the release build run only on confirmed == 'true',
// the planner is told this workflow speaks the protocol, and no `run:` script
// interpolates a dispatch input, which this job could push with.
func TestCutReleaseCutsOnlyWhatItsReportConfirmed(t *testing.T) {
	t.Parallel()

	w := readCutWorkflow(t)
	cut := w.Jobs["cut"]

	plan, planAt := cutJobStep(t, w, "Work out what to cut")
	_, candidateAt := cutJobStep(t, w, "Find the candidate")
	report, reportAt := cutJobStep(t, w, "Read what it carries")
	cutIt, cutAt := cutJobStep(t, w, "Cut it")

	if planAt >= candidateAt || candidateAt >= reportAt || reportAt >= cutAt {
		t.Fatalf("the cut job's steps are out of order: plan %d, candidate %d, report %d, cut %d",
			planAt, candidateAt, reportAt, cutAt)
	}

	if plan.Env["BILLET_CUT_PROTOCOL"] != "preview-confirm" {
		t.Errorf("the planner is not told this workflow previews and confirms: %q",
			plan.Env["BILLET_CUT_PROTOCOL"])
	}

	if report.ID != "contents" || report.Env["CONFIRM"] != "${{ inputs.confirm }}" ||
		report.Env["TAG"] != "${{ steps.plan.outputs.tag }}" ||
		report.Env["PREVIOUS"] != "${{ steps.plan.outputs.previous }}" ||
		report.Env["CANDIDATE"] != "${{ steps.candidate.outputs.commit }}" {
		t.Errorf("the report step is not wired to the plan, the candidate and the confirmation: %+v", report)
	}

	if !strings.Contains(report.Run, "set -euo pipefail\n") ||
		!strings.Contains(report.Run, `scripts/release-contents.sh | tee -a "$GITHUB_STEP_SUMMARY"`) {
		t.Errorf("the report step does not run the report under pipefail:\n%s", report.Run)
	}

	if cutIt.If != "steps.contents.outputs.confirmed == 'true'" {
		t.Errorf("the cut step runs on %q, want only a confirmation", cutIt.If)
	}

	// THE CUT USES WHAT THE REPORT WAS GIVEN: the planned tag the confirmation
	// named, and the candidate it read. A literal or another step's value here
	// would cut one thing after confirming another.
	for name, want := range map[string]string{
		"TAG":                "${{ steps.plan.outputs.tag }}",
		"BRANCH":             "${{ steps.plan.outputs.branch }}",
		"COLLECTION_VERSION": "${{ steps.plan.outputs.collection_version }}",
		"CANDIDATE":          "${{ steps.candidate.outputs.commit }}",
		"EXISTING":           "${{ steps.candidate.outputs.existing }}",
	} {
		if cutIt.Env[name] != want {
			t.Errorf("the cut step's %s is %q, want %q", name, cutIt.Env[name], want)
		}
	}

	if !strings.Contains(cutIt.Run, `git tag -a "${TAG}"`) {
		t.Error("the step gated on the confirmation is not the one that tags")
	}

	if cut.Outputs["confirmed"] != "${{ steps.contents.outputs.confirmed }}" {
		t.Errorf("the cut job hands on confirmed = %q", cut.Outputs["confirmed"])
	}

	if cut.Permissions["actions"] != "read" {
		t.Errorf("the cut job cannot read CI's runs: actions = %q", cut.Permissions["actions"])
	}

	if w.Jobs["release"].If != "needs.cut.outputs.confirmed == 'true'" {
		t.Errorf("the release build runs on %q, want only a confirmed cut", w.Jobs["release"].If)
	}

	for name, job := range w.Jobs {
		for _, step := range job.Steps {
			if strings.Contains(step.Run, "inputs.") {
				t.Errorf("%s's step %q interpolates a dispatch input into its script", name, step.Name)
			}
		}
	}
}

// candidateRepos is an origin with main and, when withBranch, release/v0.4 one
// commit past it, and a full clone of it standing where the job checks out main.
func candidateRepos(t *testing.T, withBranch bool) (string, string, string, string) {
	t.Helper()

	origin := newRepo(t)
	commit(t, origin, "main")

	mainTip := strings.TrimSpace(gitOutput(t, origin, "rev-parse", "HEAD"))
	branchTip := ""

	if withBranch {
		fixtureGit(t, origin, "checkout", "--quiet", "-b", "release/v0.4")
		commit(t, origin, "Release v0.4.0")
		fixtureGit(t, origin, "tag", "-a", "v0.4.0", "-m", "v0.4.0")
		branchTip = strings.TrimSpace(gitOutput(t, origin, "rev-parse", "HEAD"))
		fixtureGit(t, origin, "checkout", "--quiet", "main")
	}

	work := filepath.Join(t.TempDir(), "work")
	fixtureGit(t, origin, "clone", "--quiet", "file://"+origin, work)

	return origin, work, mainTip, branchTip
}

// runStep runs a step's script as Actions does, in dir, with env added.
func runStep(t *testing.T, dir, script string, env ...string) (string, error) {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), "bash", "--noprofile", "--norc", "-e", "-c", script)
	cmd.Dir = dir
	cmd.Env = append(fixtureGitEnv(), env...)

	out, err := cmd.CombinedOutput()

	return string(out), err
}

// THE CANDIDATE IS FOUND WITHOUT CHECKING THE BRANCH OUT, and the report that
// follows runs main's script against it; ls-remote's could-not-ask is a refusal,
// never a new branch from main. The step's own script, run against real
// repositories.
func TestTheCandidateStepFindsTheCommitAndLeavesMainCheckedOut(t *testing.T) {
	t.Parallel()

	step, _ := cutJobStep(t, readCutWorkflow(t), "Find the candidate")

	for _, tc := range []struct {
		name         string
		withBranch   bool
		breakOrigin  bool
		wantExisting string
	}{
		{name: "a patch on an existing branch", withBranch: true, wantExisting: "true"},
		{name: "a new series", wantExisting: "false"},
		{name: "an origin that cannot be asked", breakOrigin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, work, mainTip, branchTip := candidateRepos(t, tc.withBranch)

			if tc.breakOrigin {
				fixtureGit(t, work, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone"))
			}

			output := filepath.Join(t.TempDir(), "github-output")

			out, err := runStep(t, work, step.Run, "BRANCH=release/v0.4", "GITHUB_OUTPUT="+output)

			written, readErr := os.ReadFile(output)
			if readErr != nil && !os.IsNotExist(readErr) {
				t.Fatalf("read GITHUB_OUTPUT: %v", readErr)
			}

			if tc.breakOrigin {
				if err == nil {
					t.Fatalf("the step went on when origin could not be asked:\n%s", out)
				}

				if !strings.Contains(out, "could not ask origin whether release/v0.4 exists") {
					t.Errorf("the refusal does not say origin could not be asked:\n%s", out)
				}

				if len(written) != 0 {
					t.Errorf("a refusal named a candidate: %q", written)
				}

				return
			}

			if err != nil {
				t.Fatalf("the step failed: %v\n%s", err, out)
			}

			commitWant := mainTip
			if tc.withBranch {
				commitWant = branchTip
			}

			want := "commit=" + commitWant + "\nexisting=" + tc.wantExisting + "\n"
			if string(written) != want {
				t.Errorf("GITHUB_OUTPUT = %q, want %q", written, want)
			}

			// MAIN IS STILL WHAT IS CHECKED OUT, so the report is main's.
			if got := strings.TrimSpace(gitOutput(t, work, "symbolic-ref", "HEAD")); got != "refs/heads/main" {
				t.Errorf("the step left %s checked out, so the report would run that tree's script", got)
			}
		})
	}
}

// AND THE CUT CHECKS OUT WHAT WAS READ, OR NOTHING. Its script, up to and
// including the HEAD guard, run against real repositories: the candidate the
// report described gets through on both paths, and a candidate the branch has
// moved past stops it before a single ref is written.
func TestTheCutChecksOutTheCandidateOrStops(t *testing.T) {
	t.Parallel()

	step, _ := cutJobStep(t, readCutWorkflow(t), "Cut it")

	marker := "# THE TAG CARRIES WHAT WAS READ"
	at := strings.Index(step.Run, marker)
	if at < 0 {
		t.Fatalf("the cut step has no %q guard:\n%s", marker, step.Run)
	}

	end := strings.Index(step.Run[at:], "\nfi\n")
	if end < 0 {
		t.Fatalf("the cut step's guard is not closed:\n%s", step.Run[at:])
	}

	const through = "billet-passed-the-guard"
	guard := step.Run[:at+end+len("\nfi\n")] + "echo " + through + "\n"

	for _, tc := range []struct {
		name       string
		existing   bool
		stale      bool
		wantBranch bool
	}{
		{name: "a patch whose branch is where the report read it", existing: true},
		{name: "a new series from the main the report read"},
		{name: "a patch whose branch moved since the report", existing: true, stale: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			origin, work, mainTip, branchTip := candidateRepos(t, tc.existing)

			candidate := mainTip
			if tc.existing {
				candidate = branchTip
				// The candidate step leaves a local branch behind; so does this.
				fixtureGit(t, work, "branch", "release/v0.4", "origin/release/v0.4")
			}

			if tc.stale {
				candidate = mainTip
			}

			out, err := runStep(t, work, guard,
				"TAG=v0.4.1", "BRANCH=release/v0.4", "COLLECTION_VERSION=0.4.1",
				"CANDIDATE="+candidate, "EXISTING="+map[bool]string{true: "true", false: "false"}[tc.existing])

			if tc.stale {
				if err == nil || strings.Contains(out, through) {
					t.Fatalf("the cut went on from a commit the report did not describe:\n%s", out)
				}

				if !strings.Contains(out, "::error::HEAD is "+branchTip+", not the candidate "+mainTip) {
					t.Errorf("the refusal does not name both commits:\n%s", out)
				}

				if got := strings.TrimSpace(gitOutput(t, origin, "tag", "--list", "v0.4.1")); got != "" {
					t.Errorf("a refused cut left a tag on origin: %q", got)
				}

				return
			}

			if err != nil || !strings.Contains(out, through) {
				t.Fatalf("the cut stopped on the candidate the report described: %v\n%s", err, out)
			}

			if got := strings.TrimSpace(gitOutput(t, work, "symbolic-ref", "HEAD")); got != "refs/heads/release/v0.4" {
				t.Errorf("the cut is on %s, not the release branch", got)
			}

			if got := strings.TrimSpace(gitOutput(t, work, "rev-parse", "HEAD")); got != candidate {
				t.Errorf("the cut is at %s, not the candidate %s", got, candidate)
			}
		})
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
