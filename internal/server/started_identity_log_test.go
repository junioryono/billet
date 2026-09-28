package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"testing"
)

// THE JOB'S OWN IDENTITY IS LOGGED WHEN A POOL MEMBER STARTS IT, because what
// GitHub puts in each field for each event is to be measured on a fleet before
// any cache publication rule rests on it (#226). Driven through handle, so the
// test fails if the line moves off the started path, and read back as one
// structured record, so a field that is dropped or renamed fails by name.
func TestAStartedJobLogsTheIdentityGitHubBound(t *testing.T) {
	t.Parallel()

	p := newRunnerlessPool(t, job11)
	var logged bytes.Buffer
	p.l.log = slog.New(slog.NewJSONHandler(&logged, nil))

	started := job11
	started.RunnerID, started.RunnerName = 77, p.launchedFor11(t).RunnerName
	started.Event = "push"
	started.Owner, started.Repository = "acme", "api"
	started.WorkflowRef = "acme/api/.github/workflows/ci.yml@refs/heads/main"

	if err := p.l.handle(t.Context(), &Message{MessageID: 2, Started: []Job{started},
		Statistics: &Statistics{TotalAssignedJobs: 1}}); err != nil {
		t.Fatalf("start job 11: %v", err)
	}

	want := map[string]any{
		"runner": started.RunnerName, "job": started.JobID, "run": float64(started.RunID),
		"event": "push", "owner": "acme", "repository": "api",
		"workflow_ref": "acme/api/.github/workflows/ci.yml@refs/heads/main",
	}

	for line := range bytes.SplitSeq(bytes.TrimSpace(logged.Bytes()), []byte("\n")) {
		var record map[string]any
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatalf("a log line is not one JSON record: %q: %v", line, err)
		}
		if record["msg"] != "a pooled runner started a job" {
			continue
		}
		for key, value := range want {
			if record[key] != value {
				t.Errorf("logged %s = %v, want %v", key, record[key], value)
			}
		}

		return
	}

	t.Fatalf("no started-job line was logged:\n%s", logged.String())
}
