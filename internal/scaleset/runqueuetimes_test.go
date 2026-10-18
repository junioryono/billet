package scaleset

import (
	"context"
	"errors"
	"testing"
	"time"

	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/server"
)

// queueTimesPolicy is a policy client whose queue-time read answers err.
type queueTimesPolicy struct {
	billetgithub.RunnerGroupPolicyClient

	err error
}

func (q queueTimesPolicy) RunQueueTimes(context.Context, string, string, int64) (map[string]time.Time, error) {
	if q.err != nil {
		return nil, q.err
	}

	return map[string]time.Time{"billet-a": time.Unix(1, 0)}, nil
}

// GITHUB'S THROTTLE AND ITS REFUSAL REACH THE SWEEP AS THE SERVER'S OWN
// SENTINELS, which pause the App installation; any other failure pauses only its
// run, and a client that cannot read queue times says so.
func TestAQueueTimeReadsRefusalIsTheServersSentinel(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"primary rate limit", &billetgithub.APIError{Status: 403, RateLimited: true}, server.ErrQueueTimesThrottled},
		{"secondary rate limit", &billetgithub.APIError{Status: 429, RateLimited: true}, server.ErrQueueTimesThrottled},
		{"no actions: read", &billetgithub.APIError{Status: 403}, server.ErrQueueTimesRefused},
		{"credential", &billetgithub.APIError{Status: 401}, server.ErrQueueTimesRefused},
		{"run gone", &billetgithub.APIError{Status: 404}, nil},
		{"transport", errors.New("connection reset"), nil},
	} {
		c := &Client{policy: queueTimesPolicy{err: tc.err}}
		_, err := c.RunQueueTimes(t.Context(), "acme", "api", 31)
		throttled, refused := errors.Is(err, server.ErrQueueTimesThrottled), errors.Is(err, server.ErrQueueTimesRefused)
		switch {
		case err == nil:
			t.Errorf("%s: no error", tc.name)
		case tc.want == nil && (throttled || refused):
			t.Errorf("%s: %v pauses the whole installation", tc.name, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: %v, want %v", tc.name, err, tc.want)
		case !errors.Is(err, tc.err):
			t.Errorf("%s: %v no longer carries GitHub's answer", tc.name, err)
		}
	}

	times, err := (&Client{policy: queueTimesPolicy{}}).RunQueueTimes(t.Context(), "acme", "api", 31)
	if err != nil || len(times) != 1 {
		t.Errorf("a read GitHub answered = %v, %v", times, err)
	}
	if _, err := (&Client{}).RunQueueTimes(t.Context(), "acme", "api", 31); !errors.Is(err,
		billetgithub.ErrRunEvidenceUnavailable) {
		t.Errorf("a client with no App = %v, want ErrRunEvidenceUnavailable", err)
	}
}
