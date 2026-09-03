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
// AND BILLET'S RUNNERS CANNOT UPDATE THEMSELVES OUT OF IT, which is what turns this
// from a slow-job problem into an outage. A self-hosted runner ordinarily updates
// itself in place — so a stale image would cost a large download per job and keep
// working. billet's do not: a JIT configuration minted by GitHub's REST API carries
// `DisableUpdate = True` alongside `Ephemeral = True`, measured against the live API,
// and there is no parameter to ask for anything else. So the version baked into the
// image is the version forever, and republishing is the only way past the deadline.
//
// That is the same posture actions-runner-controller runs in, deliberately, and the
// same one it gets its `Outdated` scale-set failure from. The difference this package
// makes is being told before it happens rather than after.
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

// pinned is the release billet installs and the checksums of the tarballs it may
// fetch, kept in a file rather than a constant so the build script and the Go code
// cannot disagree about either.
//
// TWO PINS IS ONE PIN THAT IS WRONG. It was a Go constant in `billet ami` AND a
// shell default in the guest image script, which meant bumping the runner was two
// edits in two languages — and a version bump that updated the AMI and not the
// microVM image would produce exactly one stale fleet, discovered on the day GitHub
// stopped queueing to it.
//
// THE FORMAT IS LINE 1 THEN A PLATFORM TABLE, and line 1 keeps its original shape
// for a reason: scripts/build-guest-image.sh reads the checksum with
// `awk "NR==1{print $2}"`, so APPENDING lines is backwards compatible while
// changing line 1 would break the script that actually builds the guest image.
// Line 1's checksum is linux-x64's, which is what that script installs.
//
//	<version> <sha256 of the linux-x64 tarball>
//	<platform> <sha256>
//	...
//
// A SECOND BACKEND IS WHY THE TABLE EXISTS. A CodeBuild macOS build needs
// `osx-arm64` and an arm64 Linux build needs `linux-arm64`, and neither was pinned
// — so the alternative to this table was a constant somewhere else, which is the
// two-pins problem wearing different clothes.
//
//go:embed pinned.txt
var pinned string

// Pinned is the actions/runner release billet installs.
func Pinned() string {
	version, _, _ := strings.Cut(firstPinLine(pinned), " ")

	return strings.TrimSpace(version)
}

// PinnedSHA256 is the checksum of that release's linux-x64 tarball.
//
// IT LIVES BESIDE THE VERSION BECAUSE IT IS ONLY TRUE OF THAT VERSION. Held apart
// — the version in one file and the checksum in a build script — a bump updates one
// and the build fails its own integrity check, or worse updates the checksum alone
// and verifies a download against a number for a different release. Together, a
// bump is one line and cannot be half done.
//
// READ FROM LINE 1 ONLY. It used to Cut the whole embedded file at its first space,
// which was correct while the file had one line and would silently have returned
// the entire platform table the moment it had four — a "checksum" containing
// newlines, handed to a build that then verifies nothing successfully.
func PinnedSHA256() string {
	_, sum, _ := strings.Cut(firstPinLine(pinned), " ")

	return strings.TrimSpace(sum)
}

// PinnedSHA256For is the checksum of one platform's tarball, and whether billet
// pins one at all.
//
// THREE-VALUED IN THE WAY THAT MATTERS: a platform billet has no checksum for
// answers false rather than an empty string, because an empty checksum handed to a
// verifying download is a verification that passes against anything. A caller that
// cannot name a platform must refuse the launch, not proceed unverified.
//
// The platform is actions/runner's OWN asset spelling — `linux-x64`, `linux-arm64`,
// `osx-arm64` — and not billet's, or dpkg's, or the tool-cache's. Not one of those
// naming schemes is derivable from another (see install-toolcache.sh, where six
// translations disagree), which is why a caller maps its own concept onto this one
// explicitly rather than assembling a name.
//
// LINE 1 IS linux-x64's ENTRY, and it is resolved here rather than repeated in the
// table. Listing it twice would be the two-pins problem this file exists to remove,
// one file down: a bump that updated line 1 and not the duplicate would verify an
// arm64 download against the right checksum and an x64 one against a stale number
// — a mismatch that fails, and a mismatch nobody would look for in a table that
// agreed with itself yesterday.
const primaryPinPlatform = "linux-x64"

func PinnedSHA256For(platform string) (string, bool) {
	if platform == primaryPinPlatform {
		if sum := PinnedSHA256(); sum != "" {
			return sum, true
		}

		return "", false
	}

	for _, line := range strings.Split(strings.TrimSpace(pinned), "\n")[1:] {
		name, sum, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || strings.TrimSpace(name) != platform {
			continue
		}

		if sum = strings.TrimSpace(sum); sum == "" {
			return "", false
		}

		return sum, true
	}

	return "", false
}

// firstPinLine is the pin file's version line, whatever follows it.
func firstPinLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")

	return strings.TrimSpace(line)
}

// Grace is how long GitHub gives a runner to take up a new release.
//
// From their own documentation: "If you do not perform a software update within 30
// days, the GitHub Actions service will not queue jobs to your runner."
//
// COUNTED FROM THE FIRST RELEASE NEWER THAN THE INSTALLED ONE, which is what
// Freshness computes and is the only timestamp that means anything here. Every
// major, minor and patch release is an available update, so the clock starts at the
// first of them and does not restart when the next one lands.
//
// AND IT IS THE ORDINARY WINDOW RATHER THAN A GUARANTEE. GitHub says a critical
// security release may be required immediately, and it publishes no endpoint saying
// what it will refuse — its own advice is to subscribe to release notifications. So
// this is the best mechanical estimate of acceptance there is, and every diagnostic
// built on it says "ordinary" rather than implying billet knows the answer.
const Grace = 30 * 24 * time.Hour

// Warn is when billet starts saying so.
//
// A third of the window, because the action this warns about is not "click update"
// — it is building an image, verifying it boots and registers, and rolling a fleet
// onto it. Ten days is enough for that to be scheduled rather than urgent, and it
// leaves the last ten for the case where the first attempt failed.
const Warn = 20 * 24 * time.Hour

// What billet knows about how current a runner is lives in Freshness, in
// history.go, because answering it needs the release HISTORY rather than the newest
// release. The type this file used to carry counted from the newest release's
// publication date, which moves an already-expired deadline forward every time
// something else ships.
