#!/usr/bin/env bash
# Usage: matrix-workflows-match.sh <owner/repo> [ref]
#
# Says whether the acceptance matrix's workflows in <owner/repo> are byte for byte the
# ones in this checkout's scripts/acceptance/matrix, so what the matrix runs is what was
# reviewed. Exit 0: every file matches. Exit 1: one differs or is missing. Exit 2: could
# not tell (GitHub did not answer, or answered something that would not decode), which is
# never read as a match or a mismatch. Needs gh, authenticated for <owner/repo>.
set -uo pipefail

repo=${1:?usage: matrix-workflows-match.sh <owner/repo> [ref]}
ref=${2:-}
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
dir="$here/matrix"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

differs=0
for local in "$dir"/*.yml; do
	name=$(basename "$local")
	path="repos/$repo/contents/.github/workflows/$name"
	if [ -n "$ref" ]; then
		path="$path?ref=$ref"
	fi
	# THE STATUS IS CAPTURED BEFORE ANYTHING READS THE BODY: a 404 is a missing file,
	# and anything else that failed is could-not-tell.
	gh api -i "$path" >"$work/response" 2>"$work/err"
	status=$?
	code=$(sed -n '1s/^HTTP\/[0-9.]* \([0-9][0-9][0-9]\).*/\1/p' "$work/response")
	if [ "$status" -ne 0 ] && [ "$code" = 404 ]; then
		echo "$name: not in $repo" >&2
		differs=1
		continue
	fi
	if [ "$status" -ne 0 ] || [ "$code" != 200 ]; then
		echo "$name: could not read it from $repo (gh exit $status, HTTP ${code:-none})" >&2
		exit 2
	fi
	# The body follows the headers' blank line.
	sed '1,/^\r\{0,1\}$/d' "$work/response" >"$work/body"
	if ! jq -er '.content' "$work/body" >"$work/b64"; then
		echo "$name: $repo's answer has no content" >&2
		exit 2
	fi
	if ! tr -d '\n' <"$work/b64" | base64 -d >"$work/remote" 2>/dev/null; then
		echo "$name: $repo's content would not decode" >&2
		exit 2
	fi
	# cmp IS THREE-VALUED TOO: 1 is a difference, and anything past it is a comparison
	# that could not be made, which is never read as either.
	cmp -s "$local" "$work/remote"
	compared=$?
	case "$compared" in
	0) ;;
	1)
		echo "$name: $repo's copy differs from this checkout's" >&2
		differs=1
		;;
	*)
		echo "$name: could not compare it with $repo's copy (cmp exit $compared)" >&2
		exit 2
		;;
	esac
done

if [ "$differs" -ne 0 ]; then
	exit 1
fi
echo "every matrix workflow in $repo matches this checkout"
