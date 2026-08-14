#!/usr/bin/env bash
#
# Keep the guest image current, unattended.
#
# WHY THIS IS A SCHEDULED JOB AND NOT A REMINDER. GitHub requires a self-hosted
# runner to take up each release within 30 days, and past that the Actions service
# stops handing it jobs — it refuses the message rather than asking the runner to
# update, so nothing on the runner's side recovers. billet bakes the runner into an
# image, so taking up a release means rebuilding one. A fleet whose freshness depends
# on somebody remembering is a fleet that stops on a date nobody wrote down.
#
# WHY IT RUNS HERE AND NOT IN CI. Building needs debootstrap, root, and a Ceph
# cluster to publish into; verifying needs /dev/kvm and the firecracker backend. That
# is a billet node, not a hosted runner.
#
# SAFE TO ENABLE ON EVERY NODE. Two runs on one machine are kept apart by the flock
# above; two NODES sharing a pool are kept apart by an `rbd lock` taken in the
# cluster before anything is written. The second one refuses and names the holder
# rather than interleaving a write into the same head image.
#
# WHAT IT DELIBERATELY DOES NOT DO: promote. Publishing a generation is safe by
# construction — it is immutable, and nothing boots it until a tier names it — while
# promotion puts a new image in front of every job at once. So this leaves a verified
# generation and says which it is, and pointing tiers at it stays a decision somebody
# makes.
set -euo pipefail

# ONE AT A TIME, AND THE LOCK IS HELD FOR THE WHOLE RUN. A systemd ExecStartPre of
# `flock ... true` acquires and releases in the same millisecond and protects
# nothing; holding it here covers the hand-run case too, which is the one a unit file
# cannot reach. Two of these overlapping would debootstrap into one workspace and,
# worse, `dd` into the same head image concurrently.
if [ -z "${BILLET_REFRESH_LOCKED:-}" ]; then
	export BILLET_REFRESH_LOCKED=1

	# NOT `exec`, SO THAT LOSING THE LOCK CAN BE EXPLAINED. `flock -n` prints nothing
	# at all when it cannot acquire -- measured: exit 1, no output -- so an exec'd
	# version left a second run dying in total silence. Under systemd that is a failed
	# unit with an empty journal, which is the least diagnosable failure this script
	# has, and it happens exactly when somebody is already fixing something by hand.
	# `|| status=$?` RATHER THAN A BARE CALL, because `set -e` would otherwise kill
	# this script the instant flock returns 1 -- before the line that explains why.
	# The first version of this fix printed nothing for exactly that reason, which is
	# the same trap the guest agent carries a paragraph about.
	# -E 200 SO THAT "COULD NOT LOCK" HAS ITS OWN STATUS. Without it flock exits 1 on
	# contention -- and so does every `die` in this script, whose status passes
	# straight through. A failed verification, a missing tool, an unreachable GitHub:
	# all exited 1, and the wrapper then appended "another refresh is already running"
	# as the LAST lines in the journal. The real error was above it, and the sentence
	# after it was false.
	status=0
	flock -n -E 200 /var/lock/billet-image-refresh.lock "$0" "$@" || status=$?

	if [ "$status" -eq 200 ]; then
		echo "refresh-guest-image: another refresh is already running on this machine; this" >&2
		echo "run is skipping rather than building into the same workspace" >&2

		exit 1
	fi

	exit "$status"
fi

BILLET="${BILLET:-billet}"
CONFIG="${CONFIG:-/etc/billet/billet.yaml}"
REPO="${REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
IMAGE_NAME="${IMAGE_NAME:-ubuntu-2404-x64}"
IMAGE_POOL="${IMAGE_POOL:-billet-images}"
CEPH_USER="${CEPH_USER:-billet}"

# MAX_AGE is how fresh a generation has to be for this node to stand down.
#
# Six days against a weekly timer: long enough that the node whose jitter puts it
# second does not rebuild what the first just built, short enough that a genuinely
# missed week still triggers one. It is not the expiry bound -- that is thirty days
# from a runner release, and `billet runner check` watches it.
MAX_AGE="${MAX_AGE:-144h}"

# The cluster lock lives in build-guest-image.sh, with the write it protects, so a
# hand-run build takes it too.

log() { echo "refresh-guest-image: $*"; }

die() {
	echo "refresh-guest-image: $*" >&2
	exit 1
}

need_root() {
	if [ "$(id -u)" -ne 0 ]; then
		die "this builds a filesystem and maps a block device; run it as root"
	fi
}

# latest_release prints the newest published runner and its linux-x64 checksum.
#
# THE CHECKSUM COMES FROM THE MARKERS, NOT FROM A REGEX OVER THE BODY. GitHub's
# release notes carry one per platform between `<!-- BEGIN SHA linux-x64 -->` and
# `<!-- END SHA linux-x64 -->`, and the notes also contain OTHER 64-character hex
# strings — a greedy pattern picks up the wrong one, which was measured while writing
# this and would produce a build that fails its own integrity check for a reason
# nobody could see.
latest_release() {
	local body version sha

	# BOUNDED, because this runs unattended. A blackholed endpoint would otherwise
	# hold the whole job until systemd killed it two hours later, and the operator
	# would see a build timeout rather than "github did not answer".
	body=$(curl -sSf --connect-timeout 10 --max-time 60 --retry 2 --retry-delay 5 \
		-H "Accept: application/vnd.github+json" \
		https://api.github.com/repos/actions/runner/releases/latest) ||
		die "could not ask github for the latest runner release"

	version=$(printf '%s' "$body" | jq -r '.tag_name' | sed 's/^v//')
	sha=$(printf '%s' "$body" | jq -r '.body' |
		sed -n 's/.*<!-- BEGIN SHA linux-x64 -->\([0-9a-f]\{64\}\)<!-- END SHA linux-x64 -->.*/\1/p' |
		head -1)

	# CHECKED FOR SHAPE, NOT ONLY FOR EMPTINESS. `jq -r` prints the STRING `null` for
	# a field that is not there, which is not empty and would be built as a version —
	# producing a download URL for a release called null.
	case "$version" in
		[0-9]*.[0-9]*.[0-9]*) ;;
		*) die "github's answer named the release '$version', which is not a version" ;;
	esac

	case "$sha" in
		[0-9a-f][0-9a-f][0-9a-f][0-9a-f]*) ;;
		*) die "github's answer carried no linux-x64 checksum; refusing to build something unverified" ;;
	esac

	printf '%s %s\n' "$version" "$sha"
}

# newest_generation is the most recently published snapshot of the image.
newest_generation() {
	rbd --id "$CEPH_USER" -p "$IMAGE_POOL" snap ls "$IMAGE_NAME" --format json |
		jq -r 'sort_by(.id) | last | .name'
}

main() {
	need_root

	for t in curl jq rbd "$BILLET"; do
		command -v "$t" >/dev/null 2>&1 || die "missing: $t"
	done

	# THE WHOLE POINT IS TO TAKE UP NEW RELEASES, so this builds at whatever GitHub
	# has published rather than at whatever the checkout pins.
	#
	# An earlier version defaulted to the pinned release and offered a BUMP=yes flag.
	# That was wrong in the way that matters: the schedule would rebuild the same
	# runner every week forever, and thirty days after a release the fleet would stop
	# being sent jobs -- which is the exact failure this timer exists to prevent. A
	# safety mechanism that runs weekly and never advances anything is worse than
	# none, because it looks like the problem is handled.
	#
	# IT DOES NOT WRITE THE CHECKOUT. The pinned version is compiled into the billet
	# binary, so rewriting the file here without rebuilding that binary would split
	# the two -- and rebuilding a binary on a production node is not this script's
	# business. The version is passed to the build instead, and the build records what
	# it installed on the image itself, which is what `billet runner check` reads.
	# Source stays the default for a hand-run build and is bumped by a human.
	# IS ANYONE ELSE'S RECENT BUILD GOOD ENOUGH? This is what makes it safe -- and
	# useful -- to run this timer on EVERY node.
	#
	# A schedule on one machine stops when that machine does, and GitHub's thirty days
	# do not pause while a node is down. So every node should carry it. But the
	# cluster lock alone only stops builds OVERLAPPING: with the timer's jitter, the
	# second node usually starts after the first has finished, takes the lock, and
	# publishes a second identical generation. N nodes, N builds.
	#
	# Asking first turns that into one build and N-1 machines standing by, and a node
	# that stands down exits 0 rather than reporting a failure nobody should act on.
	local due=0
	"$BILLET" images due --config "$CONFIG" --max-age "$MAX_AGE" || due=$?

	case "$due" in
		0) ;;
		2)
			log "another node has already published a recent generation; standing by"

			exit 0
			;;
		*)
			die "could not tell whether a rebuild is due"
			;;
	esac

	# ASSIGNED ON ITS OWN LINE, THEN READ. `die` inside `$( )` exits the SUBSHELL, so
	# `read ... <<<"$(latest_release)"` left the script running with empty values --
	# which were then exported, and build-guest-image.sh's own `${VAR:-<pin>}`
	# fallbacks quietly built the PINNED runner instead. The weekly job would have
	# reported success while doing the one thing it exists to prevent.
	#
	# A bare assignment propagates the failure under `set -e`; `local x=$(...)` would
	# NOT, because the status becomes local's.
	local release
	release=$(latest_release)

	local version sha
	read -r version sha <<<"$release"

	# AND EMPTY IS REFUSED, because that is what every remaining way to get here
	# looks like: the build must not fall back to a pin nobody asked for.
	if [ -z "$version" ] || [ -z "$sha" ]; then
		die "github did not name a runner release and a checksum; refusing to build a version nobody chose"
	fi

	log "building at runner $version (the checkout pins $(awk 'NR==1{print $1}' \
		"$REPO/internal/runnerrelease/pinned.txt" 2>/dev/null || echo unknown))"

	export RUNNER_VERSION="$version" RUNNER_SHA256="$sha"

	"$REPO/scripts/build-guest-image.sh"

	local generation
	generation=$(newest_generation)

	if [ -z "$generation" ] || [ "$generation" = "null" ]; then
		die "the build published nothing this script can find in $IMAGE_POOL/$IMAGE_NAME"
	fi

	log "verifying $IMAGE_NAME@$generation"

	# THE GATE. A generation that cannot boot, take its registration and run a
	# container is one no tier should ever be pointed at, and the whole value of
	# publishing automatically is that this runs before anybody could.
	if ! "$BILLET" images verify --config "$CONFIG" "$IMAGE_NAME@$generation"; then
		die "$IMAGE_NAME@$generation was published and does NOT work; nothing has been pointed at it, so the fleet is unaffected -- but the next rebuild will not fix it by itself"
	fi

	log "verified $IMAGE_NAME@$generation"
	echo
	echo "Point a tier at it when you are ready:"
	echo
	echo "    image: $IMAGE_NAME@$generation"
	echo
	echo "Nothing boots it until you do. The generation you are running now is untouched."
}

main "$@"
