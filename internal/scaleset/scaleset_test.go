package scaleset

import (
	"testing"

	gh "github.com/actions/scaleset"
)

// Field mapping is where a silent error hides, so every field gets a DISTINCT
// value and every one is asserted.
//
// This is worth doing properly rather than spot-checking. Wiring these types up
// is what revealed that billet's schema had been storing RunnerRequestID under a
// column named job_id — a mapping that compiled, ran, and would only have
// surfaced when a human tried to correlate a lease with GitHub's API. Nothing
// about a struct copy fails loudly.
//
// The distinct values are what give this teeth: with every counter set to 1, a
// test that swapped two of them would pass.
func TestTranslateMapsEveryStatistic(t *testing.T) {
	msg := &gh.RunnerScaleSetMessage{
		MessageID: 4242,
		Statistics: &gh.RunnerScaleSetStatistic{
			TotalAvailableJobs:     1,
			TotalAcquiredJobs:      2,
			TotalAssignedJobs:      3,
			TotalRunningJobs:       4,
			TotalRegisteredRunners: 5,
			TotalBusyRunners:       6,
			TotalIdleRunners:       7,
		},
	}

	got := translate(msg)

	if got.MessageID != 4242 {
		t.Errorf("MessageID = %d, want 4242", got.MessageID)
	}

	if got.Statistics == nil {
		t.Fatal("statistics were dropped")
	}

	for name, pair := range map[string][2]int{
		"TotalAvailableJobs":     {got.Statistics.TotalAvailableJobs, 1},
		"TotalAcquiredJobs":      {got.Statistics.TotalAcquiredJobs, 2},
		"TotalAssignedJobs":      {got.Statistics.TotalAssignedJobs, 3},
		"TotalRunningJobs":       {got.Statistics.TotalRunningJobs, 4},
		"TotalRegisteredRunners": {got.Statistics.TotalRegisteredRunners, 5},
		"TotalBusyRunners":       {got.Statistics.TotalBusyRunners, 6},
		"TotalIdleRunners":       {got.Statistics.TotalIdleRunners, 7},
	} {
		if pair[0] != pair[1] {
			t.Errorf("%s = %d, want %d", name, pair[0], pair[1])
		}
	}
}

// The identity billet records must be the one it can ACQUIRE with.
//
// JobMessageBase carries RunnerRequestID (int64), WorkflowRunID (int64) AND
// JobID (a string). Only the request id claims work through AcquireJobs and only
// it makes a redelivered message idempotent — so reading the wrong int64 field
// here produces a lease that looks correctly populated and cannot be acquired.
func TestTranslateReadsTheRequestIDNotTheRunID(t *testing.T) {
	msg := &gh.RunnerScaleSetMessage{
		JobAssignedMessages: []*gh.JobAssigned{
			{JobMessageBase: gh.JobMessageBase{
				RunnerRequestID: 111,
				WorkflowRunID:   222,
				JobID:           "assigned-guid",
				OwnerName:       "acme",
				RepositoryName:  "api",
				JobWorkflowRef:  "acme/api/.github/workflows/ci.yml@refs/heads/main",
			}},
		},
		JobStartedMessages: []*gh.JobStarted{{
			RunnerID: 55, RunnerName: "billet-lease-55",
			JobMessageBase: gh.JobMessageBase{
				RunnerRequestID: 555,
				WorkflowRunID:   556,
				JobID:           "started-guid",
				OwnerName:       "acme",
				RepositoryName:  "api",
				JobWorkflowRef:  "acme/api/.github/workflows/ci.yml@refs/heads/main",
			},
		}},
		JobCompletedMessages: []*gh.JobCompleted{
			{Result: "succeeded", RunnerName: "billet-lease-333", JobMessageBase: gh.JobMessageBase{
				RunnerRequestID: 333,
				WorkflowRunID:   444,
				JobID:           "completed-guid",
			}},
		},
	}

	got := translate(msg)

	if len(got.Assigned) != 1 {
		t.Fatalf("assigned = %d, want 1", len(got.Assigned))
	}

	if got.Assigned[0].RequestID != 111 {
		t.Errorf("assigned RequestID = %d, want 111 (RunnerRequestID)", got.Assigned[0].RequestID)
	}

	if got.Assigned[0].RunID != 222 {
		t.Errorf("assigned RunID = %d, want 222 (WorkflowRunID)", got.Assigned[0].RunID)
	}
	if got.Assigned[0].JobID != "assigned-guid" {
		t.Errorf("assigned JobID = %q, want assigned-guid", got.Assigned[0].JobID)
	}
	if got.Assigned[0].Owner != "acme" || got.Assigned[0].Repository != "api" ||
		got.Assigned[0].WorkflowRef != "acme/api/.github/workflows/ci.yml@refs/heads/main" {
		t.Errorf("assigned cache identity was lost: %+v", got.Assigned[0])
	}
	if len(got.Started) != 1 || got.Started[0].RunnerID != 55 ||
		got.Started[0].RunnerName != "billet-lease-55" || got.Started[0].RequestID != 555 {
		t.Fatalf("started runner/job binding was lost: %+v", got.Started)
	}

	if len(got.Completed) != 1 {
		t.Fatalf("completed = %d, want 1", len(got.Completed))
	}

	if got.Completed[0].RequestID != 333 {
		t.Errorf("completed RequestID = %d, want 333", got.Completed[0].RequestID)
	}
	if got.Completed[0].Result != "succeeded" {
		t.Errorf("completed Result = %q, want succeeded", got.Completed[0].Result)
	}
	if got.Completed[0].RunnerName != "billet-lease-333" {
		t.Errorf("completed RunnerName = %q, want billet-lease-333", got.Completed[0].RunnerName)
	}
	if got.Completed[0].JobID != "completed-guid" {
		t.Errorf("completed JobID = %q, want completed-guid", got.Completed[0].JobID)
	}
}

// A message with no statistics is ordinary, and a nil entry in a slice is
// something a wire format can produce. Neither may panic: this runs inside the
// poll loop, and a panic there takes the control plane down.
func TestTranslateSurvivesMissingParts(t *testing.T) {
	got := translate(&gh.RunnerScaleSetMessage{
		MessageID:            7,
		JobAssignedMessages:  []*gh.JobAssigned{nil},
		JobCompletedMessages: []*gh.JobCompleted{nil},
	})

	if got.Statistics != nil {
		t.Error("statistics were invented from nothing")
	}

	if len(got.Assigned) != 0 || len(got.Completed) != 0 {
		t.Errorf("nil entries became jobs: assigned=%d completed=%d",
			len(got.Assigned), len(got.Completed))
	}
}

// JobAvailable and JobStarted are dropped DELIBERATELY, and that is a decision
// rather than an oversight — so it is pinned.
//
// Available is pre-assignment noise: billet advertises capacity and GitHub
// decides who gets it. Started duplicates a transition the allocator already
// owns. Translating them would invite a scheduler that reacts to messages, and a
// message carries at most 50 entries with large backlogs truncated — which is
// exactly why the protocol notes say to scale on statistics instead.
func TestTranslateDropsPreAssignmentMessages(t *testing.T) {
	got := translate(&gh.RunnerScaleSetMessage{
		MessageID:            1,
		JobAvailableMessages: []*gh.JobAvailable{{JobMessageBase: gh.JobMessageBase{RunnerRequestID: 9}}},
		JobStartedMessages:   []*gh.JobStarted{{JobMessageBase: gh.JobMessageBase{RunnerRequestID: 10}}},
	})

	if len(got.Assigned) != 0 {
		t.Errorf("a JobAvailable or JobStarted became an assignment: %+v", got.Assigned)
	}
}
