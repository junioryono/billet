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
	// THE STRUCT THAT REACHES GITHUB, not just the helper beside it. Asserting
	// that clientVersion() agrees with version.Version() stays true while the
	// value New actually sends says something else entirely.
	info := systemInfo()

	if want := version.Version(); info.Version != want {
		t.Errorf("system info version = %q, want %q", info.Version, want)
	}

	if strings.Contains(info.Version, "0.0.0-dev") {
		t.Errorf("system info version = %q, which is the hardcoded placeholder", info.Version)
	}

	if strings.TrimSpace(info.Version) == "" {
		t.Error("system info version is empty; GitHub cannot tell that from an unset field")
	}

	// The rest of it identifies billet in GitHub's telemetry, which is the whole
	// reason this struct is populated at all.
	if info.System != "billet" {
		t.Errorf("system = %q, want billet", info.System)
	}
}
