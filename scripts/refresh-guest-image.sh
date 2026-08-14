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
	exec flock -n /var/lock/billet-image-refresh.lock "$0" "$@"
fi

BILLET="${BILLET:-billet}"
CONFIG="${CONFIG:-/etc/billet/billet.yaml}"
REPO="${REPO:-$(cd "$(dirname "$0")/.." && pwd)}"
IMAGE_NAME="${IMAGE_NAME:-ubuntu-2404-x64}"
IMAGE_POOL="${IMAGE_POOL:-billet-images}"
CEPH_USER="${CEPH_USER:-billet}"

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

# bump_pin writes the newest release into the pin, and says whether it changed.
#
# THE CHECKSUM COMES FROM THE MARKERS, NOT FROM A REGEX OVER THE BODY. GitHub's
# release notes carry one per platform between `<!-- BEGIN SHA linux-x64 -->` and
# `<!-- END SHA linux-x64 -->`, and the notes also contain OTHER 64-character hex
# strings — a greedy pattern picks up the wrong one, which was measured while writing
# this and would produce a build that fails its own integrity check for a reason
# nobody could see.
bump_pin() {
	local pin="$REPO/internal/runnerrelease/pinned.txt" body version sha current

	body=$(curl -sSf -H "Accept: application/vnd.github+json" \
		https://api.github.com/repos/actions/runner/releases/latest) ||
		die "could not ask github for the latest runner release"

	version=$(printf '%s' "$body" | jq -r '.tag_name' | sed 's/^v//')
	sha=$(printf '%s' "$body" | jq -r '.body' |
		sed -n 's/.*<!-- BEGIN SHA linux-x64 -->\([0-9a-f]\{64\}\)<!-- END SHA linux-x64 -->.*/\1/p' |
		head -1)

	if [ -z "$version" ] || [ -z "$sha" ]; then
		die "github's answer did not carry a version and a linux-x64 checksum; refusing to pin
	something unverified"
	fi

	current=$(awk 'NR==1{print $1}' "$pin" 2>/dev/null || true)

	if [ "$current" = "$version" ]; then
		log "runner $version is already pinned"

		return 1
	fi

	log "runner $current -> $version"

	# WRITTEN WITH ITS OWN CHECK, because this whole function runs as an `if`
	# condition -- which suspends `set -e` for every command in it. A failed write on
	# a full or read-only filesystem would otherwise be ignored and the run would go
	# on to log a bump that did not happen and build the old release.
	if ! printf '%s %s\n' "$version" "$sha" >"$pin"; then
		die "could not write the pin at $pin"
	fi

	return 0
}

# reinstall_billet rebuilds the binary so its embedded pin matches the file.
#
# THE PIN IS EMBEDDED, so bumping the file alone splits it in two: the guest image
# would install the new runner while `billet runner check` kept reporting the old one
# and `billet ami` kept building the ec2 image from it. Refusing here is better than
# leaving that split in place -- the operator ends up on the old runner, which the
# check will keep saying, rather than on two runners and a monitor that lies.
reinstall_billet() {
	if ! command -v go >/dev/null 2>&1; then
		die "the pin moved but go is not installed here, so $BILLET cannot be rebuilt from it.
	Bump internal/runnerrelease/pinned.txt in source and deploy a new billet instead, or run
	this with BUMP=no to keep rebuilding at the pinned release"
	fi

	log "rebuilding $BILLET so its embedded pin matches"

	( cd "$REPO" && go build -o "$BILLET" ./cmd/billet ) ||
		die "could not rebuild $BILLET from $REPO"
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

	# A BUMP IS NOT REQUIRED FOR A REBUILD. Even on the current release the image
	# accumulates unpatched packages, and a build script nobody runs is one that has
	# quietly broken — so the schedule rebuilds regardless, and a bump is only what
	# makes a given week's rebuild urgent.
	#
	# BUMPING IS OFF BY DEFAULT, AND THAT IS THE HONEST DEFAULT. The pinned version
	# is SOURCE: it is embedded into the billet binary, so a script that rewrote the
	# file without rebuilding and reinstalling that binary would leave `billet runner
	# check` alarming about a fleet that is current, and `billet ami` building the ec2
	# image from the old release — which is precisely the two-pin drift the pin file
	# exists to prevent. With BUMP=yes this rebuilds the binary from the same checkout
	# so the two cannot disagree, and refuses if it cannot.
	if [ "${BUMP:-no}" = "yes" ]; then
		if bump_pin; then
			reinstall_billet
			log "the pin moved and $BILLET was rebuilt from it"
		fi
	else
		log "rebuilding at the pinned runner ($(awk 'NR==1{print $1}' \
			"$REPO/internal/runnerrelease/pinned.txt")); set BUMP=yes to take up a new release"
	fi

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
		die "$IMAGE_NAME@$generation was published and does NOT work; nothing has been pointed
	at it, so the fleet is unaffected — but the next rebuild will not fix it by itself"
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
