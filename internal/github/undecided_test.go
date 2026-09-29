package github

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
)

// GitHub being unable to answer is not an answer: a 5xx, a throttle and a
// request that got no response are undecided, and a status that IS GitHub's
// answer (a refusal, a missing group) is decided. Each case is wrapped the way
// the runner-group client wraps its errors, so the classification survives the
// operation name.
func TestUndecidedSeparatesGitHubBeingDownFromGitHubAnswering(t *testing.T) {
	wrap := func(err error) error { return fmt.Errorf("github: list runner groups: %w", err) }

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"a 503", wrap(apiError(http.StatusServiceUnavailable, []byte(`github-launch service unavailable`))), true},
		{"a 500", wrap(apiError(http.StatusInternalServerError, nil)), true},
		{"a 429", wrap(apiError(http.StatusTooManyRequests, nil)), true},
		{"a rate-limited 403", wrap(apiError(http.StatusForbidden, []byte(`{"message":"API rate limit exceeded"}`))), true},
		{"no response", wrap(&url.Error{Op: "Get", URL: "https://api.github.com", Err: errors.New("connection reset")}), true},
		{"already unverifiable", fmt.Errorf("%w: %w", ErrAppUnverifiable, errors.New("x")), true},
		{"a refusing 403", wrap(apiError(http.StatusForbidden, []byte(`{"message":"Resource not accessible by integration"}`))), false},
		{"a 404", wrap(apiError(http.StatusNotFound, nil)), false},
		{"a group that does not exist", wrap(ErrRunnerGroupNotFound), false},
		{"an undecodable answer", errors.New("github: decode runner groups: unexpected end of JSON input"), false},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Undecided(c.err); got != c.want {
				t.Errorf("Undecided(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}
