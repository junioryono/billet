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
# THREE ANSWERS, AND ONLY TWO ARE ABOUT THE SET. Nothing to carry (a channel that was never
# published, a release with no aarch64 set, another contract) exits 0 and leaves OUT
# without one, and x86_64 is published alone. A set that can be carried is proved and left
# in OUT. Anything it could not read or prove exits 1: the release would otherwise tell
# every arm64 node, in a signed pointer, that there is no image, on no evidence that there
# is none.
#
# THE POINTER IS PROVED BEFORE IT CHOOSES ANYTHING, under the policy a node holds it to,
# its expiry included, and the release it names must be immutable: this run signs a new
# pointer over what it carries, so an unproved one would launder whatever the branch said,
# an older genuine one replayed there would choose an older image, and only a locked
# release binds the source parts, which no manifest signature covers.
#
# A POINTER THAT HAS LAPSED STOPS THE PUBLICATION, unless a person says otherwise. A channel
# lapses only through ten days of failed builds, and an operator recovering from that, who
# has read what it names, dispatches with carry_expired_channel, which sets
# CARRY_EXPIRED_CHANNEL=true here.
#
# THE SET'S PROVENANCE STAYS THE RELEASE IT WAS FIRST PUBLISHED IN. Each release's tag
# points at the commit of its x86_64 build, so what built the aarch64 set is a record of
# its own, manifest-aarch64.origin, written once when the set is first published and carried
# with it unchanged. The notes name that release, however many carries ago it was, and the
# release this one came from.
#
# Usage: carry-guest-set.sh OUT, from a checkout with `origin` reachable.
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

scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

verify() {
	go -C "$here/.." run ./scripts/verifyguestset "$@"
}

# WHAT THE CHANNEL NAMES NOW is the previous release, because it is what an arm64 node
# reads today. git's own three answers, as the step that advances the channel asks them:
# 2 is the one honest absence, a branch never published. A branch that exists with no
# pointer on it is damage, not absence, and says nothing about whether arm64 has an image.
status=0
git -C "$here/.." ls-remote --exit-code --heads origin refs/heads/guest-channel >/dev/null || status=$?
case "$status" in
0) ;;
2) alone "the guest channel has never been published" ;;
*) fail "could not ask whether the guest channel exists (git exited $status)" ;;
esac
git -C "$here/.." fetch --quiet origin refs/heads/guest-channel ||
	fail "could not fetch the guest channel"
git -C "$here/.." show FETCH_HEAD:current.json >"$scratch/pointer.json" ||
	fail "the guest channel holds no current.json"
git -C "$here/.." show FETCH_HEAD:current.sigstore.json >"$scratch/pointer.sigstore.json" ||
	fail "the guest channel holds no current.sigstore.json"
question=channel
if [ "${CARRY_EXPIRED_CHANNEL:-}" = true ]; then
	question=channel-expired
fi
verify "$question" "$scratch/pointer.json" "$scratch/pointer.sigstore.json" ||
	fail "the guest channel's pointer is not one a node would take, by billet's own policy; if it has only expired, an operator who has read what it names can dispatch this workflow with carry_expired_channel"
previous=$(jq -er '.tag' "$scratch/pointer.json") || fail "the guest channel names no release"
[[ $previous =~ ^guest-[0-9]{8}-[0-9]{6}$ ]] || fail "the guest channel names $previous, which is not a guest release"

# THE RELEASE'S OWN ASSET LIST, whatever the pointer's list says: a pointer from before
# the list existed says nothing about it.
release=$(gh release view "$previous" --repo "$GITHUB_REPOSITORY" --json assets,isDraft,isImmutable) ||
	fail "could not read the release $previous"
[ "$(jq -r '.isImmutable and (.isDraft | not)' <<<"$release")" = true ] ||
	fail "$previous is not an immutable, published release, so nothing binds what it holds"
assets=$(jq -r '.assets[].name' <<<"$release") || fail "could not list the assets of $previous"
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
	--pattern manifest-aarch64.json --pattern manifest-aarch64.sigstore.json \
	--pattern manifest-aarch64.origin ||
	fail "could not download the aarch64 manifest of $previous"

verify manifest aarch64 "$scratch/manifest-aarch64.json" "$scratch/manifest-aarch64.sigstore.json" ||
	fail "the aarch64 manifest of $previous is not one billet's own signing policy accepts"

carried_contract=$(jq -er '.guest_contract' "$scratch/manifest-aarch64.json") ||
	fail "could not read the guest contract of the aarch64 manifest of $previous"
if [ "$carried_contract" != "$want_contract" ]; then
	alone "$previous's aarch64 image speaks guest contract $carried_contract and this release $want_contract, so no node of this billet could import it"
fi

# WHAT BUILT IT: `<release> <commit>`, as the publisher wrote it when the set was new.
origin_release='' origin_commit='' extra=''
read -r origin_release origin_commit extra <"$scratch/manifest-aarch64.origin" ||
	[ -n "$origin_release" ] ||
	fail "$previous has no manifest-aarch64.origin, so nothing says which release built its set"
[[ $origin_release =~ ^guest-[0-9]{8}-[0-9]{6}$ ]] && [[ $origin_commit =~ ^[0-9a-f]{40,64}$ ]] && [ -z "$extra" ] ||
	fail "$previous's manifest-aarch64.origin does not name a release and a commit"

# THE IMAGE'S AND THE SOURCE'S ASSETS, each by the name the set's own files give it: the
# publisher then holds every one to the manifest's digests and the source's parts list,
# as it holds a set built this run, so a carried set is checked by the same lines.
gh release download "$previous" --repo "$GITHUB_REPOSITORY" --dir "$out" \
	--pattern 'rootfs-aarch64.img.zst*' --pattern 'vmlinux-billet-aarch64' \
	--pattern 'image-sources-aarch64*' ||
	fail "could not download the aarch64 set of $previous"
mv "$scratch/manifest-aarch64.json" "$scratch/manifest-aarch64.sigstore.json" \
	"$scratch/manifest-aarch64.origin" "$out/"

built_at=$(jq -r '.built_at' "$out/manifest-aarch64.json")
runner=$(jq -r '.runner_version' "$out/manifest-aarch64.json")
{
	echo
	echo "**The aarch64 image is carried forward from \`$previous\`**, because this run's arm64"
	echo "build failed. It was built at $built_at with runner \`$runner\`. Its provenance is"
	echo "\`$origin_release\`, where it was first published, built from \`$origin_commit\`;"
	echo "this release's tag points at the commit of the x86_64 build."
} >"$out/release-notes-carried.md"

echo "carry-guest-set: carried the aarch64 set of $previous"
