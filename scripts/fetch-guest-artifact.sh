#!/usr/bin/env bash
#
# Fetch this run's checked guest image into a directory, proving it whole.
#
# NOT actions/download-artifact. On the 21GB runner-images artifact it stopped part
# way through extracting and still succeeded, twice (runs 36610196542 and
# 36674924000, 2026-09-29/30, with 108GiB free): no digest line, no completion
# line, and the kernel missing from the directory. That is the action's known
# failure (actions/download-artifact#454). So the zip is fetched through the REST
# API, held to the digest GitHub published for the artifact, and only then
# extracted.
#
# Usage: fetch-guest-artifact.sh <directory>
# Needs GH_TOKEN (actions: read), GITHUB_REPOSITORY, GITHUB_RUN_ID and RUNNER_TEMP.
set -euo pipefail

out=${1:?usage: fetch-guest-artifact.sh <directory>}
: "${GH_TOKEN:?GH_TOKEN is required}"
: "${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
: "${GITHUB_RUN_ID:?GITHUB_RUN_ID is required}"
: "${RUNNER_TEMP:?RUNNER_TEMP is required}"

fail() {
	echo "fetch-guest-artifact: $*" >&2
	exit 1
}

# EXACTLY ONE ARTIFACT OF THAT NAME ON THIS RUN, with a size and a digest. Anything
# else is not the artifact this run checked, or cannot be proved to be.
listing=$(gh api "repos/$GITHUB_REPOSITORY/actions/runs/$GITHUB_RUN_ID/artifacts?name=guest-image&per_page=100") ||
	fail "could not list this run's artifacts"
if ! meta=$(jq -er '
	[.artifacts[] | select(.name == "guest-image" and (.expired | not))]
	| if length != 1 then error("\(length) artifacts named guest-image") else .[0] end
	| "\(.id) \(.size_in_bytes) \(.digest // "")"
' <<<"$listing"); then
	fail "this run does not have exactly one live guest-image artifact"
fi
read -r id size digest <<<"$meta"
[[ $id =~ ^[1-9][0-9]*$ ]] || fail "the artifact's id '$id' is not a number"
[[ $size =~ ^[1-9][0-9]*$ ]] || fail "the artifact's size '$size' is not a number of bytes"
want=${digest#sha256:}
[[ $digest == sha256:* && $want =~ ^[0-9a-f]{64}$ ]] ||
	fail "GitHub published no sha256 for the artifact ('$digest'), so a download cannot be proved whole"

# ROOM FOR THE ZIP AND WHAT IT HOLDS, both at once, before either is written.
zip="$RUNNER_TEMP/guest-image.zip"
mkdir -p "$out"
free=$(df --output=avail -B1 "$RUNNER_TEMP" | tail -n 1 | tr -dc '0-9')
needed=$((2 * size + 4 * 1024 * 1024 * 1024))
echo "the artifact is $((size >> 30))GiB; $((free >> 30))GiB free"
[ "$free" -ge "$needed" ] ||
	fail "only $((free >> 30))GiB free to hold a $((size >> 30))GiB artifact and its contents"

# THE TOKEN GOES TO THE API ONLY: curl does not carry an Authorization header across
# the redirect to the blob host.
# A STALL IS AN ERROR, so it is retried: slower than 1MiB/s for two minutes ends the
# attempt, where a connection that stops sending would otherwise hold the job.
curl --fail --silent --show-error --location --retry 5 --retry-all-errors \
	--connect-timeout 30 --speed-limit 1048576 --speed-time 120 \
	-H "Authorization: Bearer $GH_TOKEN" -H "Accept: application/vnd.github+json" \
	-o "$zip" "https://api.github.com/repos/$GITHUB_REPOSITORY/actions/artifacts/$id/zip" ||
	fail "could not download artifact $id"

got=$(sha256sum "$zip" | cut -d ' ' -f 1)
[ "$got" = "$want" ] ||
	fail "the downloaded artifact is not the one GitHub describes (sha256 $got, want $want)"

unzip -q "$zip" -d "$out" || fail "could not extract artifact $id"
rm -f "$zip"
echo "fetched artifact $id ($((size >> 30))GiB) into $out, sha256 $got"
