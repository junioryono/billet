package scaleset

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/version"
)

// WHAT GITHUB IS TOLD IS THE REAL VERSION.
//
// This was a hardcoded const — "0.0.0-dev" — sent in the client's system info on
// every poll. Nothing else in the repository reads it, and the only observer is
// GitHub's telemetry, so it could stay wrong forever without anything noticing.
// A release that reports itself as a development build is exactly the kind of
// mistake that survives for years.
func TestTheVersionSentToGitHubIsTheRealOne(t *testing.T) {
	got := clientVersion()

	if want := version.Version(); got != want {
		t.Errorf("clientVersion() = %q, want %q", got, want)
	}

	if strings.Contains(got, "0.0.0-dev") {
		t.Errorf("clientVersion() = %q, which is the hardcoded placeholder", got)
	}

	if strings.TrimSpace(got) == "" {
		t.Error("clientVersion() is empty; GitHub cannot tell that from an unset field")
	}
}
