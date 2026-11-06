#!/usr/bin/env bash
#
# Write the guest channel's pointer for a release, as OUT/current.json.
#
# WHAT IT SAYS IS READ FROM WHAT THE RELEASE CARRIES (ADR-022): x86_64 always, and
# aarch64 when OUT holds its manifest, as `uname -m` spells them, which is what a node
# compares its own machine with. A list leaving aarch64 out is final to a reader, and
# one that a reader refuses stops every node of every machine, so the list is never
# written by hand or from what a run hoped to publish.
#
# Usage: TAG=guest-YYYYMMDD-HHMMSS write-guest-channel.sh OUT
set -euo pipefail

out=${1:?usage: write-guest-channel.sh OUT}
tag=${TAG:?TAG names the release the channel points at}

if [ ! -r "$out/manifest.json" ]; then
	echo "write-guest-channel: $out holds no manifest.json, so no release to point at" >&2
	exit 1
fi

architectures='["x86_64"]'
if [ -e "$out/manifest-aarch64.json" ]; then
	architectures='["x86_64","aarch64"]'
fi

# IT EXPIRES BEFORE TWO WEEKLY BUILDS COULD BE REPLAYED AS CURRENT, which is what makes
# a pointer on a mutable branch something a fleet can follow unattended. A reader refuses
# a longer window (imagesource.ParseChannel), so changing this is a change to every node.
jq -cn --arg tag "$tag" --argjson architectures "$architectures" \
	'(now | floor) as $published |
	{schema: 1, tag: $tag, published_at: ($published | todate),
		expires_at: ($published + 10 * 24 * 60 * 60 | todate),
		release_immutable: true, architectures: $architectures}' >"$out/current.json"
