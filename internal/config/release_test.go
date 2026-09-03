package config

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func releaseErrors(r *ReleaseConfig) string {
	return errors.Join(ValidateRelease(r)...).Error()
}

// SAYING NOTHING FOLLOWS THE SIGNED STABLE CHANNEL, which is the behaviour every
// existing install already has. Adding this block must change nothing about a
// fleet that ignores it.
func TestAnAbsentReleaseBlockFollowsStable(t *testing.T) {
	t.Parallel()

	var absent *ReleaseConfig

	if got := absent.EffectiveChannel(); got != ChannelStable {
		t.Errorf("an absent release block follows %q, want %q", got, ChannelStable)
	}

	if absent.Pinned() {
		t.Error("an absent release block reports itself pinned")
	}

	if errs := ValidateRelease(absent); len(errs) != 0 {
		t.Errorf("an absent release block is invalid: %v", errs)
	}
}

// A PIN AND A CHANNEL TOGETHER ARE TWO DIFFERENT INSTRUCTIONS.
//
// Guessing which the operator meant is how a deployment that believes itself
// pinned quietly follows a pointer, which is the one thing a pin exists to
// prevent.
func TestAPinAndAChannelTogetherAreRefused(t *testing.T) {
	t.Parallel()

	got := releaseErrors(&ReleaseConfig{Channel: ChannelStable, Version: "v0.4.0"})

	if !strings.Contains(got, "set one or the other") {
		t.Errorf("a pin beside a channel was not refused: %v", got)
	}
}

// AN EXACT PIN IS A TAG OR A SHA, and "latest" is neither.
//
// It reads as a version and is a moving pointer the channel field already
// expresses; accepting it would produce a deployment that follows something while
// reporting itself pinned.
func TestOnlyATagOrACommitShaIsAPin(t *testing.T) {
	t.Parallel()

	for _, version := range []string{"latest", "main", "0.4.0", "v0.4", "v0.4.0-rc1"} {
		got := releaseErrors(&ReleaseConfig{Version: version})
		if !strings.Contains(got, "neither a release tag") {
			t.Errorf("release.version %q was accepted as a pin: %v", version, got)
		}
	}

	for _, version := range []string{
		"v0.4.0",
		"v10.0.1",
		"0123456789abcdef0123456789abcdef01234567",
	} {
		if errs := ValidateRelease(&ReleaseConfig{Version: version}); len(errs) != 0 {
			t.Errorf("release.version %q was refused: %v", version, errs)
		}
	}
}

// AN IDENTITY IS REFUSED RATHER THAN TRIMMED. A channel name is a URL component
// and a version is matched against a published tag, so trimming either changes
// which release the deployment names, invisibly.
func TestSurroundingWhitespaceIsRefusedRatherThanTrimmed(t *testing.T) {
	t.Parallel()

	if got := releaseErrors(&ReleaseConfig{Channel: " stable "}); !strings.Contains(got,
		"surrounding whitespace") {
		t.Errorf("a padded channel was accepted: %v", got)
	}

	if got := releaseErrors(&ReleaseConfig{Version: " v0.4.0"}); !strings.Contains(got,
		"surrounding whitespace") {
		t.Errorf("a padded version was accepted: %v", got)
	}
}

// A CHANNEL BILLET DOES NOT PUBLISH IS REFUSED at load rather than at the first
// upgrade, which is otherwise where an operator finds out.
func TestAnUnpublishedChannelIsRefused(t *testing.T) {
	t.Parallel()

	got := releaseErrors(&ReleaseConfig{Channel: "nightly"})

	if !strings.Contains(got, "not a channel billet publishes") {
		t.Errorf("an unpublished channel was accepted: %v", got)
	}
}

// HALF A SIGNING POLICY IS NOT A POLICY. A SAN says who a certificate is for; the
// issuer says who vouched for that.
func TestHalfASigningPolicyIsRefused(t *testing.T) {
	t.Parallel()

	got := releaseErrors(&ReleaseConfig{SigningIdentity: "^https://example$"})
	if !strings.Contains(got, "any authority able to mint") {
		t.Errorf("an identity with no issuer was accepted: %v", got)
	}

	got = releaseErrors(&ReleaseConfig{SigningIssuer: "https://example"})
	if !strings.Contains(got, "any workflow that issuer signs for") {
		t.Errorf("an issuer with no identity was accepted: %v", got)
	}

	got = releaseErrors(&ReleaseConfig{
		SigningIdentity: "^(unclosed", SigningIssuer: "https://example",
	})
	if !strings.Contains(got, "not a usable pattern") {
		t.Errorf("an uncompilable identity was accepted: %v", got)
	}
}

// A WINDOW WITH NOTHING TO GATE IS REFUSED, because an operator who wrote one
// believes their fleet updates inside it.
func TestAMaintenanceWindowWithoutAutomaticIsRefused(t *testing.T) {
	t.Parallel()

	got := releaseErrors(&ReleaseConfig{
		MaintenanceWindow: &MaintenanceWindow{Start: "02:00", End: "04:00"},
	})

	if !strings.Contains(got, "nothing here starts one") {
		t.Errorf("a window with no automatic rollout was accepted: %v", got)
	}
}

// A ZERO-WIDTH WINDOW NEVER OPENS, and a deployment that wrote one believes it
// updates. Refused rather than treated as "always", because an ABSENT window
// already means always and the two spellings must not disagree.
func TestAZeroWidthWindowIsRefused(t *testing.T) {
	t.Parallel()

	got := releaseErrors(&ReleaseConfig{
		Automatic:         true,
		MaintenanceWindow: &MaintenanceWindow{Start: "02:00", End: "02:00"},
	})

	if !strings.Contains(got, "no rollout could ever start") {
		t.Errorf("a zero-width window was accepted: %v", got)
	}
}

func TestMalformedWindowTimesAreRefused(t *testing.T) {
	t.Parallel()

	got := releaseErrors(&ReleaseConfig{
		Automatic:         true,
		MaintenanceWindow: &MaintenanceWindow{Start: "2am", End: "25:00"},
	})

	for _, want := range []string{"maintenance_window.start", "maintenance_window.end"} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not name %s: %v", want, got)
		}
	}
}

// A WINDOW THAT WRAPS MIDNIGHT IS THE ORDINARY CASE, not an edge one. 22:00 to
// 04:00 is what a person picks, and a comparison that only handled start < end
// would silently never open for exactly the windows operators choose.
func TestAWindowThatWrapsMidnightOpens(t *testing.T) {
	t.Parallel()

	r := &ReleaseConfig{
		Automatic:         true,
		MaintenanceWindow: &MaintenanceWindow{Start: "22:00", End: "04:00"},
	}

	if errs := ValidateRelease(r); len(errs) != 0 {
		t.Fatalf("a wrapping window was refused: %v", errs)
	}

	cases := []struct {
		at   string
		open bool
	}{
		{"23:30", true},
		{"02:00", true},
		{"22:00", true},
		{"03:59", true},
		{"04:00", false},
		{"12:00", false},
		{"21:59", false},
	}

	for _, tc := range cases {
		at, err := time.Parse("15:04", tc.at)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.at, err)
		}

		if got := r.OpenAt(at); got != tc.open {
			t.Errorf("at %s UTC the window is open=%v, want %v", tc.at, got, tc.open)
		}
	}
}

// AN ABSENT WINDOW IS ALWAYS OPEN, which is what makes the field optional rather
// than something every deployment has to write.
func TestAnAbsentWindowIsAlwaysOpen(t *testing.T) {
	t.Parallel()

	var absent *ReleaseConfig

	if !absent.OpenAt(time.Now()) {
		t.Error("an absent release block reports its window closed")
	}

	if !(&ReleaseConfig{Automatic: true}).OpenAt(time.Now()) {
		t.Error("a release block with no window reports it closed")
	}
}

// AN UNREADABLE WINDOW IS CLOSED. Validation refuses one, so reaching this means
// something bypassed it — and treating "I could not read the window" as "start
// draining hosts" is the collapse every other three-valued answer in this
// codebase exists to prevent.
func TestAnUnreadableWindowIsClosed(t *testing.T) {
	t.Parallel()

	r := &ReleaseConfig{
		Automatic:         true,
		MaintenanceWindow: &MaintenanceWindow{Start: "not a time", End: "04:00"},
	}

	if r.OpenAt(time.Now()) {
		t.Error("a window billet cannot read reports itself open")
	}
}

// A PIN FOLLOWS NO CHANNEL, which is what makes `EffectiveChannel` the one
// question a caller has to ask.
func TestAPinnedDeploymentFollowsNoChannel(t *testing.T) {
	t.Parallel()

	r := &ReleaseConfig{Version: "v0.4.0"}

	if !r.Pinned() {
		t.Error("a version pin does not report itself pinned")
	}

	if got := r.EffectiveChannel(); got != "" {
		t.Errorf("a pinned deployment follows channel %q, want none", got)
	}
}
