// Package runnerrelease says which actions/runner billet installs, and how close
// that is to being refused by GitHub.
//
// THIS IS A DEADLINE, NOT A PREFERENCE, and that is the whole reason the package
// exists. GitHub requires a self-hosted runner to be updated within 30 days of a
// release, and past that the Actions service simply stops handing it jobs — the
// server refuses the message rather than asking the runner to update, so nothing on
// the runner's side can rescue it. A fleet that was working perfectly stops taking
// work, and the visible symptom is jobs queueing against runners that look healthy.
//
// billet bakes the runner into an image, so "update the runner" means "rebuild and
// republish the image". That is a thing somebody has to do on a schedule, and a
// thing somebody will forget — so it needs to be something a machine notices.
//
// THERE IS NO API FOR THE MINIMUM VERSION. GitHub publishes no endpoint saying what
// it will refuse; its own documented advice is to subscribe to release
// notifications. So the only mechanical signal available is the release feed, and
// the rule to apply to it is the 30-day one from their documentation.
package runnerrelease

import (
	_ "embed"
	"strings"
	"time"
)

// pinned is the release billet installs and the checksum of its linux-x64 tarball,
// kept in a file rather than a constant so the build script and the Go code cannot
// disagree about either.
//
// TWO PINS IS ONE PIN THAT IS WRONG. It was a Go constant in `billet ami` AND a
// shell default in the guest image script, which meant bumping the runner was two
// edits in two languages — and a version bump that updated the AMI and not the
// microVM image would produce exactly one stale fleet, discovered on the day GitHub
// stopped queueing to it.
//
//go:embed pinned.txt
var pinned string

// Pinned is the actions/runner release billet installs.
func Pinned() string {
	version, _, _ := strings.Cut(strings.TrimSpace(pinned), " ")

	return version
}

// PinnedSHA256 is the checksum of that release's linux-x64 tarball.
//
// IT LIVES BESIDE THE VERSION BECAUSE IT IS ONLY TRUE OF THAT VERSION. Held apart
// — the version in one file and the checksum in a build script — a bump updates one
// and the build fails its own integrity check, or worse updates the checksum alone
// and verifies a download against a number for a different release. Together, a
// bump is one line and cannot be half done.
func PinnedSHA256() string {
	_, sum, _ := strings.Cut(strings.TrimSpace(pinned), " ")

	return strings.TrimSpace(sum)
}

// Grace is how long GitHub gives a runner to take up a new release.
//
// From their own documentation: "If you do not perform a software update within 30
// days, the GitHub Actions service will not queue jobs to your runner."
const Grace = 30 * 24 * time.Hour

// Warn is when billet starts saying so.
//
// A third of the window, because the action this warns about is not "click update"
// — it is building an image, verifying it boots and registers, and rolling a fleet
// onto it. Ten days is enough for that to be scheduled rather than urgent, and it
// leaves the last ten for the case where the first attempt failed.
const Warn = 20 * 24 * time.Hour

// Status is what billet knows about how current its runner is.
type Status struct {
	// Pinned is what billet installs; Latest is what GitHub has published.
	Pinned string
	Latest string
	// Published is when Latest was released, which is what the deadline counts from.
	Published time.Time
	// Deadline is when GitHub stops queueing jobs to a runner still on Pinned.
	Deadline time.Time
}

// Current reports whether the pinned release is the published one.
func (s Status) Current() bool { return s.Pinned == s.Latest }

// Expired reports whether GitHub will already refuse to send jobs.
func (s Status) Expired(now time.Time) bool {
	return !s.Current() && !now.Before(s.Deadline)
}

// Due reports whether the image should be rebuilt now to stay inside the window.
func (s Status) Due(now time.Time) bool {
	return !s.Current() && !now.Before(s.Published.Add(Warn))
}

// Remaining is how long is left before GitHub stops queueing jobs. It is negative
// once that has happened, and it is meaningless when the pin is current.
func (s Status) Remaining(now time.Time) time.Duration { return s.Deadline.Sub(now) }
