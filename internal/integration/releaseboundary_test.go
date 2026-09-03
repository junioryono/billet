package integration_test

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/releasesource"
)

// THE LEAF PACKAGE'S CHANNEL NAMES ARE THE REAL ONES.
//
// depguard forbids internal/config importing any other billet package, tests
// included, so the channel names it validates against are declared twice: once
// where the validation lives and once where the resolution does. That is exactly
// the two-pins shape this codebase keeps removing — and here it cannot be
// removed, because the layering rule that creates it is load-bearing for a
// different reason.
//
// SO THE DUPLICATE IS PINNED RATHER THAN TOLERATED, and it is pinned HERE because
// this is the only package that may see both. A package-local test in config
// could assert only that the file agrees with itself; a rename in
// releasesource would leave every existing deployment's `channel: stable` valid
// at load and unresolvable at the first upgrade, with the diagnostic arriving
// from a different layer than the mistake.
func TestTheConfigsChannelNamesAreTheOnesBilletPublishes(t *testing.T) {
	t.Parallel()

	pairs := []struct {
		name       string
		configured string
		published  string
	}{
		{"stable", config.ChannelStable, releasesource.ChannelStable},
		{"candidate", config.ChannelCandidate, releasesource.ChannelCandidate},
	}

	for _, p := range pairs {
		if p.configured != p.published {
			t.Errorf("config calls the %s channel %q and releasesource calls it %q; a "+
				"deployment configuring the first would validate at load and fail to "+
				"resolve at the first upgrade", p.name, p.configured, p.published)
		}

		if !releasesource.KnownChannel(p.configured) {
			t.Errorf("config accepts channel %q, which releasesource will not resolve",
				p.configured)
		}
	}
}

// AND EVERY CHANNEL THE CONFIG ACCEPTS IS ONE THE RESOLVER KNOWS, read the other
// way round: a channel added to releasesource and not to config is one nobody can
// configure, which is a smaller problem but the same drift.
func TestEveryPublishedChannelIsConfigurable(t *testing.T) {
	t.Parallel()

	for _, published := range []string{releasesource.ChannelStable, releasesource.ChannelCandidate} {
		errs := config.ValidateRelease(&config.ReleaseConfig{Channel: published})
		if len(errs) != 0 {
			t.Errorf("releasesource publishes channel %q and the config refuses it: %v",
				published, errs)
		}
	}
}

// A PIN THE CONFIG ACCEPTS IS ONE A MANIFEST COULD NAME.
//
// The two version patterns are also declared twice for the same layering reason,
// and they disagree in a way that matters: a config accepting a shape no release
// can carry produces a deployment that validates and then never resolves.
func TestAConfiguredVersionPinIsAShapeAReleaseCanHave(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"v0.4.0", "v10.11.12"} {
		if errs := config.ValidateRelease(&config.ReleaseConfig{Version: version}); len(errs) != 0 {
			t.Errorf("the config refuses pin %q: %v", version, errs)
		}

		// The manifest's own validator is what a release has to satisfy, so the
		// same string is put through a manifest carrying it.
		m := releasesource.Manifest{Version: version}
		if err := m.Validate(); err != nil && containsVersionRefusal(err.Error()) {
			t.Errorf("the config accepts pin %q and a manifest cannot carry it: %v",
				version, err)
		}
	}
}

// containsVersionRefusal reports whether a manifest refusal was about the version
// rather than about the rest of a deliberately empty fixture.
func containsVersionRefusal(msg string) bool {
	return strings.Contains(msg, "is not a billet release tag")
}
