package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// The release channels billet publishes.
//
// DECLARED HERE RATHER THAN IMPORTED FROM internal/releasesource, because config
// is a LEAF package: depguard forbids it importing any other billet package, and
// that rule is what keeps a validation rule from depending on the thing it
// validates. The two lists are kept in step by a test in internal/integration,
// which is where a cross-layer proof belongs — a package-local test here could
// only assert this file agrees with itself.
const (
	ChannelStable    = "stable"
	ChannelCandidate = "candidate"
)

// releaseVersion is the shape of an exact pin.
//
// A TAG OR A COMMIT SHA, and nothing else. "latest" and "main" both read as
// versions and are neither: one is a moving pointer this config already has a
// field for, and the other is a branch nobody publishes artifacts from. Accepting
// either would produce a deployment that follows something while reporting itself
// pinned.
var (
	releaseTag = regexp.MustCompile(`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)
	releaseSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// ReleaseConfig says how this deployment learns about new billet releases.
//
// EVERY FIELD IS OPTIONAL AND THE ZERO VALUE IS SAFE. A deployment that says
// nothing follows the signed stable channel and updates only when an operator
// asks — which is the behaviour every existing install already has, so adding
// this block changes nothing about a fleet that ignores it.
type ReleaseConfig struct {
	// Channel is the signed pointer this deployment follows. Empty means stable.
	Channel string `yaml:"channel,omitempty"`

	// Version pins an exact release, and it NEVER MOVES.
	//
	// It wins over Channel, and setting both is an error rather than a precedence
	// puzzle: an operator who pinned a version and also named a channel has said
	// two things, and guessing which they meant is how a deployment that believes
	// itself pinned quietly follows a pointer.
	Version string `yaml:"version,omitempty"`

	// Automatic lets the control plane start a rollout by itself when the channel
	// advances.
	//
	// OFF BY DEFAULT, AND THAT IS NOT TIMIDITY. An automatic rollout drains hosts
	// and replaces binaries; a deployment that had not decided to allow that must
	// not start doing it because it upgraded to a billet that could. Turning it on
	// is a sentence somebody writes.
	Automatic bool `yaml:"automatic,omitempty"`

	// MaintenanceWindow bounds when an automatic rollout may START.
	//
	// IT NEVER STOPS ONE. A rollout in progress waits for the work already running
	// on a host for as long as it takes, and a window that could interrupt that
	// would be a clock authorising a teardown — the thing this whole area exists
	// to refuse. What the window decides is whether a new one begins.
	MaintenanceWindow *MaintenanceWindow `yaml:"maintenance_window,omitempty"`

	// SigningIdentity and SigningIssuer override what a release manifest must be
	// signed by, for a deployment mirroring releases internally.
	SigningIdentity string `yaml:"signing_identity,omitempty"`
	SigningIssuer   string `yaml:"signing_issuer,omitempty"`
}

// MaintenanceWindow is a daily span, in UTC, during which a rollout may begin.
type MaintenanceWindow struct {
	// Start and End are "HH:MM" in UTC.
	//
	// UTC RATHER THAN LOCAL, because a fleet spans machines whose local time is
	// not one thing, and a window that meant something different on each host is a
	// window nobody can reason about. It is also stable across a DST transition,
	// which a local window is not — and the hour that repeats or vanishes is
	// exactly the hour somebody chose because it is quiet.
	Start string `yaml:"start"`
	End   string `yaml:"end"`
}

// EffectiveChannel is the channel this deployment follows, or empty when it is
// pinned to an exact version.
func (r *ReleaseConfig) EffectiveChannel() string {
	if r == nil {
		return ChannelStable
	}

	if r.Version != "" {
		return ""
	}

	if r.Channel == "" {
		return ChannelStable
	}

	return r.Channel
}

// Pinned reports whether this deployment follows nothing.
func (r *ReleaseConfig) Pinned() bool { return r != nil && r.Version != "" }

// ValidateRelease reports everything wrong with a release block.
//
// EXPORTED SO EVERY READER APPLIES THE SAME RULES. config is a leaf package and
// the rollout planner is not, so the rule has to be callable from both — the same
// arrangement the capacity rules already have, and for the same reason: a rule
// enforced in one of two entry points has a second entry point that does not
// enforce it.
func ValidateRelease(r *ReleaseConfig) []error {
	if r == nil {
		return nil
	}

	var errs []error

	channel := strings.TrimSpace(r.Channel)
	version := strings.TrimSpace(r.Version)

	// AN IDENTITY IS REFUSED RATHER THAN TRIMMED, which is this codebase's rule
	// for any string something outside the process has an opinion about. A channel
	// name is a URL component and a version is matched against a published tag;
	// trimming either changes which release the deployment names, invisibly.
	if channel != r.Channel {
		errs = append(errs, fmt.Errorf("release.channel %q has surrounding whitespace; it is "+
			"a URL component, so trimming it would change which pointer this deployment "+
			"follows", r.Channel))
	}

	if version != r.Version {
		errs = append(errs, fmt.Errorf("release.version %q has surrounding whitespace; it is "+
			"matched against a published tag, so trimming it would change which release "+
			"this deployment installs", r.Version))
	}

	if channel != "" && version != "" {
		errs = append(errs, fmt.Errorf("release.version %s pins an exact release and "+
			"release.channel %s follows a moving one; set one or the other, because a "+
			"deployment that does both is following a pointer while reporting itself "+
			"pinned", version, channel))
	}

	if channel != "" && channel != ChannelStable && channel != ChannelCandidate {
		errs = append(errs, fmt.Errorf("release.channel %q is not a channel billet "+
			"publishes; use %s or %s", channel, ChannelStable, ChannelCandidate))
	}

	if version != "" && !releaseTag.MatchString(version) && !releaseSHA.MatchString(version) {
		errs = append(errs, fmt.Errorf("release.version %q is neither a release tag "+
			"(vX.Y.Z) nor a commit SHA. \"latest\" and \"main\" both read as versions and "+
			"are neither: one is a moving pointer release.channel already expresses, the "+
			"other is a branch nothing publishes artifacts from", version))
	}

	// HALF A SIGNING POLICY IS NOT A POLICY. A SAN says who a certificate is FOR;
	// the issuer says who vouched for that. Without the issuer, any authority able
	// to mint a certificate carrying that name satisfies the check.
	switch {
	case r.SigningIdentity != "" && r.SigningIssuer == "":
		errs = append(errs, errors.New("release.signing_identity is set with no "+
			"release.signing_issuer, so any authority able to mint a certificate carrying "+
			"that name would satisfy it"))
	case r.SigningIssuer != "" && r.SigningIdentity == "":
		errs = append(errs, errors.New("release.signing_issuer is set with no "+
			"release.signing_identity, so any workflow that issuer signs for would be "+
			"accepted"))
	}

	if r.SigningIdentity != "" {
		// COMPILED HERE, so an unusable pattern is a configuration error rather
		// than something discovered when an upgrade fails to match it at three in
		// the morning.
		if _, err := regexp.Compile(r.SigningIdentity); err != nil {
			errs = append(errs, fmt.Errorf("release.signing_identity is not a usable "+
				"pattern: %w", err))
		}
	}

	errs = append(errs, validateMaintenanceWindow(r)...)

	return errs
}

func validateMaintenanceWindow(r *ReleaseConfig) []error {
	w := r.MaintenanceWindow
	if w == nil {
		return nil
	}

	var errs []error

	if !r.Automatic {
		// NOT AN ERROR ABOUT THE WINDOW, an error about what it would do. A window
		// with no automatic rollout to gate decides nothing, and an operator who
		// wrote one believes their fleet updates inside it.
		errs = append(errs, errors.New("release.maintenance_window bounds when an "+
			"AUTOMATIC rollout may start, and release.automatic is false, so nothing "+
			"here starts one. Set release.automatic, or remove the window"))
	}

	start, startErr := parseWindowTime(w.Start)
	if startErr != nil {
		errs = append(errs, fmt.Errorf("release.maintenance_window.start: %w", startErr))
	}

	end, endErr := parseWindowTime(w.End)
	if endErr != nil {
		errs = append(errs, fmt.Errorf("release.maintenance_window.end: %w", endErr))
	}

	if startErr == nil && endErr == nil && start == end {
		// A ZERO-WIDTH WINDOW NEVER OPENS, and a deployment that wrote one believes
		// it updates. It is refused rather than treated as "always", because
		// "always" is what an ABSENT window already means and the two spellings
		// must not disagree.
		errs = append(errs, errors.New("release.maintenance_window opens and closes at the "+
			"same minute, so no rollout could ever start in it. Omit the window to allow "+
			"one at any time"))
	}

	return errs
}

func parseWindowTime(v string) (time.Duration, error) {
	parsed, err := time.Parse("15:04", v)
	if err != nil {
		return 0, fmt.Errorf("%q is not a UTC time of day in HH:MM", v)
	}

	return time.Duration(parsed.Hour())*time.Hour +
		time.Duration(parsed.Minute())*time.Minute, nil
}

// OpenAt reports whether an automatic rollout may start at a given moment.
//
// AN ABSENT WINDOW IS ALWAYS OPEN, which is what makes the field optional rather
// than something every deployment has to write.
//
// A WINDOW THAT WRAPS MIDNIGHT IS THE ORDINARY CASE, not an edge one: 22:00 to
// 04:00 is what a person picks, and a comparison that only handled start < end
// would silently never open for exactly the windows operators choose.
func (r *ReleaseConfig) OpenAt(t time.Time) bool {
	if r == nil || r.MaintenanceWindow == nil {
		return true
	}

	start, err := parseWindowTime(r.MaintenanceWindow.Start)
	if err != nil {
		// AN UNPARSEABLE WINDOW IS CLOSED. Validation refuses one, so reaching this
		// means something bypassed it — and treating "I could not read the window"
		// as "start draining hosts" is the collapse every other three-valued answer
		// in this codebase exists to prevent.
		return false
	}

	end, err := parseWindowTime(r.MaintenanceWindow.End)
	if err != nil {
		return false
	}

	utc := t.UTC()
	now := time.Duration(utc.Hour())*time.Hour + time.Duration(utc.Minute())*time.Minute

	if start < end {
		return now >= start && now < end
	}

	return now >= start || now < end
}
