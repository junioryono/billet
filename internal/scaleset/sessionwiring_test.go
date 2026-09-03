package scaleset

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	gh "github.com/actions/scaleset"

	"github.com/junioryono/billet/internal/server"
)

// SESSION ITSELF TURNS GITHUB'S REFUSAL INTO THE SENTINEL, and that is the link
// the recognition tests do not cover.
//
// isSessionConflict has its own tests, and they would all still pass if the call
// to it were deleted from Session — leaving a restarted control plane back where
// it started, failing outright on a session an abandoned predecessor still holds.
// This drives the real Session against the error GitHub actually returns.
func TestSessionReportsAHeldSessionAsOneToWaitFor(t *testing.T) {
	t.Parallel()

	c := &Client{
		log: slog.Default(),
		openMessageSession: func(context.Context, int, string) (*gh.MessageSessionClient, error) {
			return nil, errors.New(liveSessionConflict)
		},
	}

	_, err := c.Session(t.Context(), 1, "billet")
	if err == nil {
		t.Fatal("opening a session GitHub refused returned no error")
	}

	if !errors.Is(err, server.ErrSessionHeld) {
		t.Errorf("the 409 for a session that is already held did not come back as "+
			"ErrSessionHeld, so the control plane reports it and stops instead of "+
			"waiting for the abandoned session to expire: %v", err)
	}

	// THE ORIGINAL SURVIVES THE WRAP, because it is the only thing that says which
	// scale set and which owner, and an operator reading the log needs both.
	if !isSessionConflict(err) {
		t.Errorf("what GitHub said was lost on the way out: %v", err)
	}
}

// AND EVERY OTHER FAILURE IS STILL REPORTED AT ONCE.
//
// The retry loop this feeds is UNBOUNDED, so a failure wrongly carrying the
// sentinel is not a slow start — it is a control plane that waits forever on
// something that will never resolve, with the real fault never surfacing.
func TestSessionReportsAnyOtherFailureImmediately(t *testing.T) {
	t.Parallel()

	c := &Client{
		log: slog.Default(),
		openMessageSession: func(context.Context, int, string) (*gh.MessageSessionClient, error) {
			return nil, errors.New("dial tcp 140.82.121.6:443: connect: connection refused")
		},
	}

	_, err := c.Session(t.Context(), 1, "billet")
	if err == nil {
		t.Fatal("a refused connection returned no error")
	}

	if errors.Is(err, server.ErrSessionHeld) {
		t.Errorf("an unrelated failure came back as a held session, so the control "+
			"plane would retry it forever instead of reporting it: %v", err)
	}
}
