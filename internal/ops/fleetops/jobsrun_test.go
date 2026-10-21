package fleetops

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/cli"
)

// bufferedEnv is an env whose output a test reads.
func bufferedEnv() (cli.Env, *bytes.Buffer) {
	var out, errs bytes.Buffer

	return cli.Env{Stdout: &out, Stderr: &errs, Stdin: strings.NewReader(""), Getenv: os.Getenv}, &out
}

// A RUN RESOLVES TO EVERY JOB OF IT THE LEDGER RECORDED, and each prints in
// full under a line naming it, oldest first; a lease whose runner GitHub gave
// another run's job is that run's, and is left out.
func TestJobsShowRunPrintsEveryJobOfTheRun(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	fakeJobsGitHub(t, noJobs)

	first := seedJob(t, stateDir, jobSeed{requestID: 77})
	second := seedJob(t, stateDir, jobSeed{requestID: 78})
	elsewhere := seedJob(t, stateDir, jobSeed{requestID: 79,
		job: &alloc.HistoryJob{JobID: "other-run-job", RunID: 9999}})

	env, out := bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"show", "--config", cfg, "--run", "4242"}); err != nil {
		t.Fatalf("billet jobs show --run: %v", err)
	}

	text := out.String()
	for _, want := range []string{
		"== run 4242, job 1 of 2: lease " + first + " ==",
		"== run 4242, job 2 of 2: lease " + second + " ==",
		"lease      " + first, "lease      " + second,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the run's report does not say %q:\n%s", want, text)
		}
	}
	if strings.Index(text, first) > strings.Index(text, second) {
		t.Errorf("the run's jobs are not oldest first:\n%s", text)
	}
	if strings.Contains(text, elsewhere) {
		t.Errorf("a lease whose job started in another run is printed under run 4242:\n%s", text)
	}

	// --json IS ONE ARRAY of the same records.
	env, out = bufferedEnv()
	if err := Jobs(t.Context(), env, []string{"show", "--config", cfg, "--json", "--run", "4242"}); err != nil {
		t.Fatalf("billet jobs show --json --run: %v", err)
	}

	var records []jobJSON
	if err := json.Unmarshal(out.Bytes(), &records); err != nil {
		t.Fatalf("the run's --json is not an array of job records: %v\n%s", err, out)
	}
	if len(records) != 2 || records[0].Lease != first || records[1].Lease != second {
		t.Fatalf("the run's --json holds %+v, want %s then %s", records, first, second)
	}
	if records[0].LaunchedAt != "" || records[0].BoughtAt == "" {
		t.Errorf("the record's phases are launched %q and bought %q; the seeded job was bought and "+
			"never launched", records[0].LaunchedAt, records[0].BoughtAt)
	}
}

// --run IS REFUSED WHERE IT CANNOT MEAN ONE THING: beside a lease, with --html,
// which is one lease's page, and as a run id no run has; and a run the ledger
// holds no job of says so rather than printing nothing.
func TestJobsShowRunRefusesWhatItCannotAnswer(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeJobsConfig(t, stateDir)
	fakeJobsGitHub(t, noJobs)
	lease := seedJob(t, stateDir, jobSeed{requestID: 77})

	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"beside a lease":   {[]string{"--run", "4242", lease}, "give it or a lease, not both"},
		"with --html":      {[]string{"--run", "4242", "--html"}, "--html prints one lease's page"},
		"a zero run":       {[]string{"--run", "0"}, "positive id of a GitHub workflow run"},
		"a run of nothing": {[]string{"--run", "31337"}, "no job of run 31337 is recorded"},
	} {
		t.Run(name, func(t *testing.T) {
			env, out := bufferedEnv()
			err := Jobs(t.Context(), env, append([]string{"show", "--config", cfg}, tc.args...))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("billet jobs show %v: %v, want an error saying %q", tc.args, err, tc.want)
			}
			if out.Len() != 0 {
				t.Errorf("a refused show printed:\n%s", out)
			}
		})
	}
}

// A JOB'S LIFE IS WORDED FROM WHAT THE LEDGER RECORDED, and what it did not
// record is said in those words: a phase with no time is left out rather than
// shown as never entered, a process nobody recorded is "not recorded", and a
// replacement is named only when both processes are known.
func TestTheJobsLifeIsWordedFromWhatWasRecorded(t *testing.T) {
	var lines []string
	line := func(label, format string, args ...any) {
		lines = append(lines, strings.TrimRight(label+"|"+fmt.Sprintf(format, args...), " "))
	}

	renderLifecycle(line, alloc.JobRecord{
		LeaseID:  "6f1c2a9e0b7d4c3e8a5f9d2b1c0e7a64",
		BoughtAt: "2026-10-20T12:00:00.000000000Z", AssignedAt: "2026-10-20T12:00:01.500000000Z",
		LaunchedAt: "2026-10-20T12:00:02.000000000Z", RegisteredAt: "2026-10-20T12:00:04.000000000Z",
		StartedAt: "2026-10-20T12:00:09.000000000Z", TeardownAt: "2026-10-20T12:03:09.000000000Z",
		FinishedAt: "2026-10-20T12:03:10.000000000Z", RunnerID: 91,
		LaunchIncarnation: "3333333333333333aaaa", HolderIncarnation: "5555555555555555bbbb",
		FailureReason: "spot\ninterruption", Disruption: "spot-interruption",
		DisruptedAt: "2026-10-20T12:03:00.000000000Z",
		Caches: alloc.CacheObservation{ImageCache: alloc.ImageCacheWarm, CacheGeneration: "g7",
			ActionsCache: "from-a-newer-binary"},
	})

	want := []string{
		"phases|bought      2026-10-20T12:00:00.000000000Z",
		"|assigned    2026-10-20T12:00:01.500000000Z  +1.5s",
		"|launched    2026-10-20T12:00:02.000000000Z  +500ms",
		"|registered  2026-10-20T12:00:04.000000000Z  +2s",
		"|started     2026-10-20T12:00:09.000000000Z  +5s",
		"|teardown    2026-10-20T12:03:09.000000000Z  +3m0s",
		"|finished    2026-10-20T12:03:10.000000000Z  +1s",
		"runner|GitHub runner 91, instance billet-6f1c2a9e0b7d4c3e8a5f9d2b1c0e7a64",
		"held by|launched by process 333333333333, held at the end by process 555555555555, which replaced it",
		`failure|reason "spot\ninterruption"; disrupted by billet: "spot-interruption" at 2026-10-20T12:03:00.000000000Z`,
		`caches|image warm (generation "g7"), actions "from-a-newer-binary", sticky not observed, git not observed, ` +
			"bazel not observed, go not observed",
	}
	if strings.Join(lines, "\n") != strings.Join(want, "\n") {
		t.Errorf("the job's life reads\n%s\nwant\n%s", strings.Join(lines, "\n"), strings.Join(want, "\n"))
	}

	// A ROW FROM BEFORE ANY OF IT WAS KEPT.
	lines = nil
	renderLifecycle(line, alloc.JobRecord{LeaseID: "6f1c2a9e0b7d4c3e8a5f9d2b1c0e7a64"})
	for _, want := range []string{"phases|none recorded", "held by|not recorded", "failure|none recorded"} {
		if !strings.Contains(strings.Join(lines, "\n"), want) {
			t.Errorf("an unrecorded job does not say %q:\n%s", want, strings.Join(lines, "\n"))
		}
	}

	for _, tc := range []struct{ launched, held, want string }{
		{"aaaa", "aaaa", "launched and held to the end by process aaaa"},
		{"aaaa", "", "launched by process aaaa; the process holding it at the end is not recorded"},
		{"", "bbbb", "held at the end by process bbbb; the process that launched it is not recorded"},
		// A NODE NAMES ITS OWN PROCESS, and a newline in the name would forge a line.
		{"a\nlease      forged", "", `launched by process "a\nlease      forged"; the process holding it at ` +
			"the end is not recorded"},
	} {
		if got := describeProcesses(tc.launched, tc.held); got != tc.want {
			t.Errorf("describeProcesses(%q, %q) = %q, want %q", tc.launched, tc.held, got, tc.want)
		}
	}
}
