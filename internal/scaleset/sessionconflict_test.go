package scaleset

import (
	"errors"
	"fmt"
	"testing"
)

// THE 409 GITHUB ACTUALLY ANSWERS, recorded verbatim from a live conformance run
// against a real organization on 2026-08-30.
//
// PINNED TO THE MEASUREMENT RATHER THAN TO A READING OF IT. The upstream client
// returns a formatted error rather than a typed one, so what separates "another
// control plane still holds this session" from every other failure is text billet
// does not own. If GitHub rewords it, this test is what fails — loudly, in one
// place — rather than a restarted control plane quietly going back to failing to
// start, which is the behaviour this recognition exists to prevent.
const liveSessionConflict = `failed to create message session: failed to do the session ` +
	`request: request POST https://broker.actions.githubusercontent.com/rest/_apis/` +
	`runtime/runnerscalesets/1/sessions?api-version=6.0-preview failed(status="409 ` +
	`Conflict", github_request_id="FDE7:2CEC23:20F50E1:21AB6FB:6A94D802"): unexpected ` +
	`status code 409 Conflict: GitHub.Actions.Runtime.WebApi.` +
	`RunnerScaleSetSessionConflictException, GitHub.Actions.Runtime.WebApi: The runner ` +
	`scale set billet-conformance already has an active session for owner ` +
	`billet-conformance.`

func TestAHeldSessionIsRecognisedFromWhatGitHubAnswers(t *testing.T) {
	t.Parallel()

	if !isSessionConflict(errors.New(liveSessionConflict)) {
		t.Error("the 409 GitHub answers when a session is already held was not recognised, " +
			"so a restarted control plane fails to start instead of waiting for the " +
			"abandoned session to expire")
	}

	// WRAPPED THE WAY IT ARRIVES, since the client adds its own context on the way
	// out and the recognition has to survive that.
	wrapped := fmt.Errorf("scaleset: open session for scale set 1: %w",
		errors.New(liveSessionConflict))
	if !isSessionConflict(wrapped) {
		t.Error("a wrapped conflict was not recognised")
	}
}

// AND NOTHING ELSE IS READ AS ONE.
//
// BOTH HALVES ARE REQUIRED — the status and the exception name — because a 409
// from some other endpoint is a different fact, and treating it as "wait, this
// resolves itself" would leave a control plane retrying forever against something
// that will never change.
func TestOnlyASessionConflictIsRecognisedAsOne(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		err  error
	}{
		{"nothing at all", nil},
		{"an unrelated failure", errors.New("dial tcp: connection refused")},
		{"a 409 from somewhere else", errors.New(`failed(status="409 Conflict"): ` +
			`GitHub.Actions.Runtime.WebApi.SomethingElseEntirely`)},
		{"the exception name without the status", errors.New(
			"RunnerScaleSetSessionConflictException")},
		{"a 500 carrying the words", errors.New(`failed(status="500 Internal Server ` +
			`Error"): RunnerScaleSetSessionConflictException`)},

		// THE RESPONSE BODY IS APPENDED TO THE FORMATTED ERROR, so every word in it
		// is text GitHub chose rather than text the client's formatter produced. This
		// is a 500 whose body quotes BOTH the phrase and the exception name — which a
		// bare `strings.Contains(text, "409 Conflict")` accepts, sending a real fault
		// into an unbounded retry that hides it forever. Only the structured
		// `status="409 Conflict"` separates them.
		{"a 500 whose body quotes a conflict", errors.New(`failed(status="500 Internal ` +
			`Server Error"): upstream reported 409 Conflict: ` +
			`RunnerScaleSetSessionConflictException while proxying`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if isSessionConflict(tc.err) {
				t.Errorf("%v was read as a held session, so a control plane would wait "+
					"forever for something that will not resolve", tc.err)
			}
		})
	}
}
