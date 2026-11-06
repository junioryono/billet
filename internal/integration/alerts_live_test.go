package integration_test

import (
	"os"
	"strconv"
	"testing"

	"github.com/junioryono/billet/internal/alerts"
	"github.com/junioryono/billet/internal/config"
)

// WHAT AN ALERT RULE NAMING A BRANCH OR A STEP READS FROM GITHUB.COM, read through
// billet's own client for the App (#537).
//
// The runs are ones TestLiveDisruptedJob left behind, as TestLiveRerunRecordAndRefusal
// reads them: BILLET_LIVE_ALERT_CANCELLED names a run whose job was cancelled while
// it ran, and BILLET_LIVE_ALERT_LOST one whose runner was killed, both on
// BILLET_LIVE_ALERT_REPOSITORY, each with its runner's name in
// BILLET_LIVE_ALERT_CANCELLED_RUNNER and BILLET_LIVE_ALERT_LOST_RUNNER.
//
//	BILLET_LIVE_ORG=org BILLET_LIVE_APP_ID=1 BILLET_LIVE_INSTALLATION_ID=2 \
//	BILLET_LIVE_APP_KEY=/path/key.pem BILLET_LIVE_SCALE_SET=billet-measure-r1 \
//	BILLET_LIVE_ALERT_REPOSITORY=org/jobs BILLET_LIVE_ALERT_CANCELLED=1 \
//	BILLET_LIVE_ALERT_CANCELLED_RUNNER=runner-a BILLET_LIVE_ALERT_LOST=2 \
//	BILLET_LIVE_ALERT_LOST_RUNNER=runner-b \
//	go test ./internal/integration/ -run TestLiveAlertReads -v -count=1
func TestLiveAlertReads(t *testing.T) {
	if os.Getenv("BILLET_LIVE_ALERT_CANCELLED") == "" {
		t.Skip("BILLET_LIVE_ALERT_CANCELLED is unset; this read is opt-in")
	}

	client, _ := liveScaleSet(t)
	owner, repository, ok := config.SplitRepository(required(t, "BILLET_LIVE_ALERT_REPOSITORY"))
	if !ok {
		t.Fatal("BILLET_LIVE_ALERT_REPOSITORY is not owner/name")
	}

	run := func(name string) int64 {
		t.Helper()

		id, err := strconv.ParseInt(required(t, name), 10, 64)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		return id
	}

	steps := func(runID int64, runner string) []alerts.Step {
		t.Helper()

		jobs, err := client.RunJobSteps(probeCtx(t), owner, repository, runID)
		if err != nil {
			t.Fatalf("read run %d's jobs: %v", runID, err)
		}
		record, named := jobs[runner]
		t.Logf("run %d, runner %s: %+v (named %v)", runID, runner, record, named)
		if !named || !record.Completed {
			t.Fatalf("run %d's job on %s = %+v, named %v; want a concluded job", runID, runner, record, named)
		}

		out := make([]alerts.Step, 0, len(record.Steps))
		for _, s := range record.Steps {
			out = append(out, alerts.Step{Name: s.Name, Conclusion: s.Conclusion})
		}

		return out
	}

	cancelled := run("BILLET_LIVE_ALERT_CANCELLED")

	record, err := client.WorkflowRun(probeCtx(t), owner, repository, cancelled)
	if err != nil {
		t.Fatalf("read run %d: %v", cancelled, err)
	}
	t.Logf("run %d is on branch %q", cancelled, record.HeadBranch)
	branchRule := alerts.Rule{Match: config.AlertMatch{Branch: "ma*"}}
	if record.HeadBranch == "" || !branchRule.MatchesBranch(record.HeadBranch) {
		t.Errorf("run %d's branch %q is not one a rule for `ma*` takes", cancelled, record.HeadBranch)
	}

	holdRule := alerts.Rule{Match: config.AlertMatch{Step: "Hold*"}}

	// A JOB CANCELLED WHILE IT RAN: its running step is cancelled, which a rule about a
	// cancelled step takes and a rule about a failed step does not.
	got := steps(cancelled, required(t, "BILLET_LIVE_ALERT_CANCELLED_RUNNER"))
	if !holdRule.MatchesSteps(got, alerts.Cancelled) {
		t.Errorf("a rule for a cancelled `Hold*` step does not take %+v", got)
	}
	if holdRule.MatchesSteps(got, alerts.Failed) {
		t.Errorf("a rule for a failed `Hold*` step takes a cancelled one: %+v", got)
	}

	// A JOB WHOSE RUNNER WAS KILLED: GitHub leaves its running step without a
	// conclusion, so no rule about a step that failed takes it.
	lost := steps(run("BILLET_LIVE_ALERT_LOST"), required(t, "BILLET_LIVE_ALERT_LOST_RUNNER"))
	if holdRule.MatchesSteps(lost, alerts.Failed) || holdRule.MatchesSteps(lost, alerts.Cancelled) {
		t.Errorf("a killed runner's steps %+v read as a step that ended", lost)
	}
}
