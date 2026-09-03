#!/bin/bash
#
# Point the signed `stable` channel at a release this run has already proved
# immutable.
#
# THE CHANNEL IS THE ONLY THING IN THE RELEASE CONTRACT THAT MOVES. Everything
# else — a tag, its assets, the manifest inside it — is frozen by GitHub release
# immutability. So this file is where "which release is current" lives, and it is
# signed, expiring and self-describing precisely because it is the mutable half:
# an unsigned edit to the branch, or an old signed statement replayed onto it, is
# refused by the reader rather than by the branch's protection.
#
# IT RUNS AFTER THE IMMUTABILITY GATE, and the statement asserts that fact. The
# assertion has to be a finding rather than a claim, because GitHub's immutability
# applies only to releases created after it was enabled — "the repository is
# protected now" says nothing about a given release.
set -euo pipefail

tag=${RELEASE_TAG:?RELEASE_TAG must name the release}
channel=${BILLET_CHANNEL:-stable}
branch=release-channel

semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
if ! [[ $tag =~ $semver ]]; then
	printf 'release tag is not semantic versioning: %s\n' "$tag" >&2
	exit 1
fi

# A HOTFIX ON AN OLDER MINOR MUST NOT MOVE THE CHANNEL BACKWARDS.
#
# `stable` names the newest release, and cutting v0.3.27 after v0.4.0 exists is an
# ordinary thing to do — the fix belongs to the v0.3 series and its consumers pin
# that series by exact version. Pointing the channel at it would roll every
# deployment following stable back a minor version, silently, as a side effect of
# a patch release.
#
# COMPARED WITH sort -V AGAINST EVERY EXISTING TAG rather than against the current
# channel statement: the statement can be missing (the first time this runs) or
# stale, and neither is a reason to publish a regression.
newest=$(git tag --list 'v*' --sort=-v:refname | head -n 1)
if [[ $newest != "$tag" ]]; then
	printf 'not advancing the %s channel: %s is newer than %s, so this release is a hotfix on an older series\n' \
		"$channel" "$newest" "$tag"
	exit 0
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# THE MANIFEST IS FETCHED BACK FROM THE PUBLISHED RELEASE rather than read out of
# dist/. The digest this statement carries has to name the bytes a deployment will
# actually download; hashing a local copy would vouch for a file nobody else can
# see, and any difference between the two is exactly the thing worth catching.
gh release download "$tag" --pattern release-manifest.json --dir "$work"

digest=$(sha256sum "$work/release-manifest.json" | cut -d' ' -f1)

# BUILT AND VALIDATED BY THE SAME PACKAGE THAT READS IT, so a publisher cannot
# emit a statement its own fleet would refuse — which is otherwise discovered when
# every deployment stops resolving the channel.
go run ./scripts/mkchannelstatement \
	--channel "$channel" \
	--tag "$tag" \
	--manifest-sha256 "$digest" \
	--out "$work/$channel.json"

cosign sign-blob --yes --new-bundle-format \
	--bundle "$work/$channel.sigstore.json" \
	"$work/$channel.json"

# THE BRANCH IS ORPHANED AND FAST-FORWARDED. It carries no source, only pointers,
# so a full history of every channel advance is noise; what matters is that the
# two files always move together, because a reader that fetched a new statement
# and an old signature would refuse an authentic channel.
git config user.name github-actions[bot]
git config user.email 41898282+github-actions[bot]@users.noreply.github.com

if git ls-remote --exit-code --heads origin "$branch" >/dev/null 2>&1; then
	git fetch origin "$branch:$branch"
	git checkout "$branch"
else
	git checkout --orphan "$branch"
	git rm -rf --cached . >/dev/null 2>&1 || true
	find . -mindepth 1 -maxdepth 1 -not -name .git -exec rm -rf {} +
fi

cp "$work/$channel.json" "$channel.json"
cp "$work/$channel.sigstore.json" "$channel.sigstore.json"

git add "$channel.json" "$channel.sigstore.json"

if git diff --cached --quiet; then
	printf 'the %s channel already names %s\n' "$channel" "$tag"
	exit 0
fi

git commit -m "$channel -> $tag"
git push origin "HEAD:$branch"

printf 'the %s channel now names %s (manifest %s)\n' "$channel" "$tag" "$digest"
