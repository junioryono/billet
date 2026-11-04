#!/usr/bin/env bash
#
# Carry the previous release's aarch64 image into this one, when the arm64 build failed
# and that image still speaks this release's guest contract (ADR-022).
#
# WHY A FAILED arm64 BUILD IS NOT A RELEASE WITHOUT ITS IMAGE. The channel names one
# release, and an arm64 node finds its image only through that release: published
# without one, every arm64 node and every new one reads a final "no image for this
# machine" until the next good build. So the last good set is uploaded again, unchanged,
# as long as a node of this release's billet could import it. Its manifest says when it
# was built, and its runner's window is what `billet check` reports.
#
# THREE ANSWERS, AND ONLY TWO ARE ABOUT THE SET. Nothing to carry (no channel, a release
# with no aarch64 set, another contract) exits 0 and leaves OUT without one, and x86_64
# is published alone. A set that can be carried is proved and left in OUT. Anything it
# could not read or prove exits 1: the release would otherwise tell every arm64 node, in
# a signed pointer, that there is no image, on no evidence that there is none.
#
# THE SET'S PROVENANCE STAYS THE RELEASE IT WAS FIRST PUBLISHED IN. The new tag points at
# the x86_64 build's commit, so the notes name where this set came from, and what wrote
# it is that release's own signed manifest and tag.
#
# Usage: carry-guest-set.sh OUT
# Needs GH_TOKEN and GITHUB_REPOSITORY; writes OUT/release-notes-carried.md when it
# carries one, and go on the PATH runs scripts/verifyguestset against imagesource's own
# signing policy.
set -euo pipefail

out=${1:?usage: carry-guest-set.sh OUT}
: "${GH_TOKEN:?GH_TOKEN is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"

here=$(cd "$(dirname "$0")" && pwd)

fail() {
	echo "carry-guest-set: $*" >&2
	exit 1
}

alone() {
	echo "carry-guest-set: $*; this release carries x86_64 alone"
	exit 0
}

[ -r "$out/manifest.json" ] || fail "$out holds no manifest.json, the image a carried set must match"
want_contract=$(jq -er '.guest_contract' "$out/manifest.json") ||
	fail "could not read the guest contract of $out/manifest.json"

# WHAT THE CHANNEL NAMES NOW is the previous release, because it is what an arm64 node
# reads today. A channel that does not exist yet is the one honest absence here; gh
# exits 1 for a 404 and for a network failure alike, so the two are told apart by what
# it says, and anything but GitHub's own Not Found stops the publication.
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

status=0
gh api "repos/$GITHUB_REPOSITORY/contents/current.json?ref=guest-channel" --jq .content \
	>"$scratch/pointer.b64" 2>"$scratch/pointer.err" || status=$?
if [ "$status" -ne 0 ]; then
	if grep -q 'HTTP 404' "$scratch/pointer.err"; then
		alone "the guest channel does not exist yet"
	fi
	cat "$scratch/pointer.err" >&2
	fail "could not read the guest channel (gh exited $status)"
fi
base64 -d "$scratch/pointer.b64" >"$scratch/pointer.json" 2>/dev/null ||
	fail "the guest channel's pointer is not base64"
previous=$(jq -er '.tag' "$scratch/pointer.json") || fail "the guest channel names no release"
[[ $previous =~ ^guest-[0-9]{8}-[0-9]{6}$ ]] || fail "the guest channel names $previous, which is not a guest release"

# THE RELEASE'S OWN ASSET LIST, whatever the pointer's list says: a pointer from before
# the list existed says nothing about it.
assets=$(gh release view "$previous" --repo "$GITHUB_REPOSITORY" --json assets --jq '.assets[].name') ||
	fail "could not list the assets of $previous"
status=0
grep -qxF manifest-aarch64.json <<<"$assets" || status=$?
case "$status" in
0) ;;
1) alone "$previous carries no aarch64 set" ;;
*) fail "could not read the asset list of $previous (grep $status)" ;;
esac

# THE MANIFEST FIRST, because whether the rest is worth 20 GB is decided by it, and it
# goes to OUT only once it is proved: publish-guest-release.sh reads a manifest there as
# a set this release carries.
gh release download "$previous" --repo "$GITHUB_REPOSITORY" --dir "$scratch" \
	--pattern manifest-aarch64.json --pattern manifest-aarch64.sigstore.json ||
	fail "could not download the aarch64 manifest of $previous"

go -C "$here/.." run ./scripts/verifyguestset aarch64 \
	"$scratch/manifest-aarch64.json" "$scratch/manifest-aarch64.sigstore.json" ||
	fail "the aarch64 manifest of $previous is not one billet's own signing policy accepts"

carried_contract=$(jq -er '.guest_contract' "$scratch/manifest-aarch64.json") ||
	fail "could not read the guest contract of the aarch64 manifest of $previous"
if [ "$carried_contract" != "$want_contract" ]; then
	alone "$previous's aarch64 image speaks guest contract $carried_contract and this release $want_contract, so no node of this billet could import it"
fi

# THE IMAGE'S AND THE SOURCE'S ASSETS, each by the name the set's own files give it: the
# publisher then holds every one to the manifest's digests and the source's parts list,
# as it holds a set built this run, so a carried set is checked by the same lines.
gh release download "$previous" --repo "$GITHUB_REPOSITORY" --dir "$out" \
	--pattern 'rootfs-aarch64.img.zst*' --pattern 'vmlinux-billet-aarch64' \
	--pattern 'image-sources-aarch64*' ||
	fail "could not download the aarch64 set of $previous"
mv "$scratch/manifest-aarch64.json" "$scratch/manifest-aarch64.sigstore.json" "$out/"

built_at=$(jq -r '.built_at' "$out/manifest-aarch64.json")
runner=$(jq -r '.runner_version' "$out/manifest-aarch64.json")
{
	echo
	echo "**The aarch64 image is carried forward from \`$previous\`**, because this run's arm64"
	echo "build failed. It was built at $built_at with runner \`$runner\`, and its provenance is"
	echo "that release, whose tag points at the commit that built it; this release's tag"
	echo "points at the commit of the x86_64 build."
} >"$out/release-notes-carried.md"

echo "carry-guest-set: carried the aarch64 set of $previous"
