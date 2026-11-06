package integration_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/server"
)

// BILLET'S OWN CLIENT READS GITHUB'S REAL RECORD, AND HEARS ITS REAL REFUSAL (#541).
//
// The runs are ones TestLiveDisruptedJob left behind: BILLET_LIVE_RERUN_LOST names a
// completed run whose runner was killed, and BILLET_LIVE_RERUN_OWN one whose job
// failed on its own, both on BILLET_LIVE_RERUN_REPOSITORY. The App under test holds
// `actions: read`, so asking to re-run is refused, which is what it records rather
// than a run started on a shared repository.
func TestLiveRerunRecordAndRefusal(t *testing.T) {
	lost := os.Getenv("BILLET_LIVE_RERUN_LOST")
	if lost == "" {
		t.Skip("BILLET_LIVE_RERUN_LOST is unset; this measurement is opt-in")
	}

	client, _ := liveScaleSet(t)
	owner, repository, ok := config.SplitRepository(required(t, "BILLET_LIVE_RERUN_REPOSITORY"))
	if !ok {
		t.Fatal("BILLET_LIVE_RERUN_REPOSITORY is not owner/name")
	}

	read := func(name string) server.RunAttempt {
		t.Helper()

		run, err := strconv.ParseInt(required(t, name), 10, 64)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		attempt, err := client.LatestAttempt(probeCtx(t), owner, repository, run)
		if err != nil {
			t.Fatalf("read run %d: %v", run, err)
		}
		t.Logf("%s, run %d: %+v", name, run, attempt)

		return attempt
	}

	if attempt := read("BILLET_LIVE_RERUN_LOST"); !attempt.Completed || len(attempt.Jobs) != 1 || !attempt.Jobs[0].RunnerLost {
		t.Errorf("the killed runner's run did not read as a lost runner: %+v", attempt)
	}
	if attempt := read("BILLET_LIVE_RERUN_OWN"); !attempt.Completed || len(attempt.Jobs) != 1 ||
		attempt.Jobs[0].RunnerLost || !attempt.Jobs[0].Failed {
		t.Errorf("a job's own failure did not read as one: %+v", attempt)
	}

	run, err := strconv.ParseInt(lost, 10, 64)
	if err != nil {
		t.Fatalf("BILLET_LIVE_RERUN_LOST: %v", err)
	}

	err = client.RerunFailedJobs(probeCtx(t), owner, repository, run)
	t.Logf("the App asking to re-run run %d: %v", run, err)
	if !errors.Is(err, server.ErrRerunRefused) {
		t.Errorf("an App with actions: read = %v, want ErrRerunRefused", err)
	}
}

// WHAT GITHUB.COM RECORDS ABOUT A JOB WHOSE RUNNER DIED, MEASURED (#541).
//
// A re-run asks for proof that billet's infrastructure ended a job rather than
// the job itself, and that proof is read from GitHub's record of the job, so
// what that record says about a lost runner is a question about somebody else's
// service. Like TestLiveAssignmentProtocol this RECORDS rather than asserts: it
// holds one message session on a throwaway scale set, starts a real runner for
// every assignment, and writes every message to a JSON report. What happens to
// the runners, and the REST reads, are the operator's, from another shell:
//
//	BILLET_LIVE_DISRUPTED=1 BILLET_LIVE_ORG=org BILLET_LIVE_APP_ID=1 \
//	BILLET_LIVE_INSTALLATION_ID=2 BILLET_LIVE_APP_KEY=/path/key.pem \
//	BILLET_LIVE_SCALE_SET=billet-measure-r1 BILLET_LIVE_REPORT_DIR=/tmp/measure \
//	BILLET_LIVE_RUNNER_IMAGE=ghcr.io/actions/actions-runner:latest \
//	go test ./internal/integration/ -run TestLiveDisruptedJob -v -count=1 -timeout 4h
//
// With BILLET_LIVE_JIT_DIR set, each assignment's registration is written there,
// mode 0600, instead of starting a container, for a machine started by hand: an
// EC2 instance booted from a billet image, whose own shutdown is the question.
//
// docs/reference/records/job-reruns.md holds what it measured. The session and
// the containers are closed at the end, or when the file BILLET_LIVE_STOP_FILE
// names appears; the scale set is left for `billet teardown`.
func TestLiveDisruptedJob(t *testing.T) {
	if os.Getenv("BILLET_LIVE_DISRUPTED") == "" {
		t.Skip("BILLET_LIVE_DISRUPTED is unset; this measurement is opt-in")
	}

	client, set := liveScaleSet(t)
	image := required(t, "BILLET_LIVE_RUNNER_IMAGE")

	p := &protocolProbe{
		t: t, client: client, set: set, start: time.Now(),
		report: &protocolReport{
			Measure: "disrupted", Organization: os.Getenv("BILLET_LIVE_ORG"), ScaleSet: set.Name,
			ScaleSetID: set.ID, StartedAt: time.Now().UTC(), Findings: map[string]any{},
		},
	}
	t.Cleanup(p.finish)

	p.open()

	window := durationOr("BILLET_LIVE_WINDOW", 3*time.Hour)
	stopFile := os.Getenv("BILLET_LIVE_STOP_FILE")
	jitDir := os.Getenv("BILLET_LIVE_JIT_DIR")

	for time.Since(p.start) < window && t.Context().Err() == nil {
		if stopFile != "" {
			if _, err := os.Stat(stopFile); err == nil {
				return
			}
		}

		msg := p.poll(4)
		if msg == nil {
			continue
		}

		for range msg.Assigned {
			if jitDir == "" {
				p.startRunner(image)

				continue
			}

			name := fmt.Sprintf("%s-runner-%d", set.Name, time.Now().UnixNano())
			jit, err := client.JITConfig(probeCtx(t), set.ID, name, "")
			if err != nil {
				t.Fatalf("generate a runner registration: %v", err)
			}

			path := filepath.Join(jitDir, name+".jit")
			if err := os.WriteFile(path, []byte(jit.Config()), 0o600); err != nil {
				t.Fatalf("write the runner registration: %v", err)
			}

			p.record(protocolEvent{Error: "wrote a registration for runner " + name})
		}
	}
}
