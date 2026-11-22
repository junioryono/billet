package scaleset

import (
	"context"
	"errors"
	"testing"

	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/server"
)

// strandedPolicy is a policy client whose recovery calls answer from fields.
type strandedPolicy struct {
	billetgithub.RunnerGroupPolicyClient

	attempt billetgithub.QueuedAttempt
	err     error
}

func (r strandedPolicy) QueuedAttempt(context.Context, string, string, int64) (billetgithub.QueuedAttempt, error) {
	return r.attempt, r.err
}

func (r strandedPolicy) CancelRun(context.Context, string, string, int64) error { return r.err }

// A RUN'S ATTEMPT REACHES THE LOOP IN THE SERVER'S WORDS: a job queued with no runner
// is unstarted, a completed one is done, and anything else, a runner on it, is
// neither, which is what keeps a cancel off a run that is running.
func TestAQueuedAttemptIsReadInTheServersWords(t *testing.T) {
	t.Parallel()

	c := &Client{policy: strandedPolicy{attempt: billetgithub.QueuedAttempt{Attempt: 3, Jobs: []billetgithub.QueuedJob{
		{Name: "a", Status: billetgithub.JobStatusQueued, Attempt: 3},
		{Name: "b", Status: billetgithub.JobStatusQueued, Attempt: 3, RunnerName: "billet-x"},
		{Name: "c", Status: "in_progress", Attempt: 3, RunnerName: "billet-y"},
		{Name: "d", Status: billetgithub.RunStatusCompleted, Attempt: 3},
	}}}}

	got, err := c.QueuedAttempt(t.Context(), "acme", "api", 31)
	if err != nil {
		t.Fatalf("QueuedAttempt: %v", err)
	}
	want := []server.QueuedJob{
		{Name: "a", Attempt: 3, Unstarted: true},
		{Name: "b", Attempt: 3},
		{Name: "c", Attempt: 3},
		{Name: "d", Attempt: 3, Done: true},
	}
	if got.Attempt != 3 || len(got.Jobs) != len(want) {
		t.Fatalf("QueuedAttempt = %+v, want %+v", got, want)
	}
	for i := range want {
		if got.Jobs[i] != want[i] {
			t.Errorf("job %d = %+v, want %+v", i, got.Jobs[i], want[i])
		}
	}

	// AND HOW A COMPLETED RUN ENDED, which is what a recovery re-runs on.
	ended := &Client{policy: strandedPolicy{attempt: billetgithub.QueuedAttempt{Completed: true, Cancelled: true,
		Attempt: 2}}}
	if got, err := ended.QueuedAttempt(t.Context(), "acme", "api", 31); err != nil || !got.Completed ||
		!got.Cancelled || got.Attempt != 2 {
		t.Errorf("a run a cancel ended = %+v, %v; want completed and cancelled on attempt 2", got, err)
	}
}

// A CANCEL'S ANSWER REACHES THE LOOP AS THE SERVER'S OWN SENTINEL: a run already over
// is told from a refusal, which pauses the installation, and from a throttle.
func TestACancelsAnswerIsTheServersSentinel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"a run already over", billetgithub.ErrRunAlreadyCompleted, server.ErrRunCompleted},
		{"no actions: write", &billetgithub.APIError{Status: 403}, server.ErrRerunRefused},
		{"a throttle", &billetgithub.APIError{Status: 429, RateLimited: true}, server.ErrRerunThrottled},
	} {
		c := &Client{policy: strandedPolicy{err: tc.err}}

		if err := c.CancelRun(t.Context(), "acme", "api", 31); !errors.Is(err, tc.want) {
			t.Errorf("%s: CancelRun = %v, want %v", tc.name, err, tc.want)
		}
	}

	if err := (&Client{policy: strandedPolicy{}}).CancelRun(t.Context(), "acme", "api", 31); err != nil {
		t.Errorf("an accepted cancel = %v", err)
	}
}
