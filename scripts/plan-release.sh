#!/bin/bash
set -euo pipefail

requested=${REQUESTED:-}
output=${GITHUB_OUTPUT:?GITHUB_OUTPUT must name the workflow output file}

# Reject line injection before any grep or workflow-output operation sees the input.
case "$requested" in
	*[$'\n\r']*)
		echo "::error::the version must be a single line" >&2
		exit 1
		;;
esac

semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
# Malformed and prerelease tags cannot become the basis for an automatic version.
newest="$(git tag --list 'v*' | grep -E "$semver" | sort -V | tail -n1 || true)"

if [[ -n $requested ]]; then
	tag=$requested
elif [[ -z $newest ]]; then
	tag=v0.1.0
else
	tag="$(echo "$newest" | awk -F. '{ sub(/^v/, "", $1); printf "v%d.%d.0\n", $1, $2 + 1 }')"
fi

if ! echo "$tag" | grep -Eq "$semver"; then
	echo "::error::$tag is not a release version like v0.4.0 (no pre-releases, no leading zeros)" >&2
	exit 1
fi
if git rev-parse -q --verify "refs/tags/$tag" >/dev/null; then
	echo "::error::$tag already exists" >&2
	exit 1
fi

series="$(echo "$tag" | awk -F. '{ print $1 "." $2 }')"
collection_version="${tag#v}"
series_newest="$(git tag --list "$series.*" | grep -E "$semver" | sort -V | tail -n1 || true)"

# Existing minor lines may receive hotfixes after a newer line exists. A brand-new
# minor line must still advance the global version rather than relabel newer code.
if [[ -n $series_newest && $(printf '%s\n%s\n' "$series_newest" "$tag" | sort -V | tail -n1) != "$tag" ]]; then
	echo "::error::$tag is not newer than $series_newest in $series" >&2
	exit 1
fi
if [[ -z $series_newest && -n $newest && $(printf '%s\n%s\n' "$newest" "$tag" | sort -V | tail -n1) != "$tag" ]]; then
	echo "::error::a new release series $tag must be newer than the newest release $newest" >&2
	exit 1
fi

branch="release/$series"
{
	echo "tag=$tag"
	echo "branch=$branch"
	echo "collection_version=$collection_version"
} >> "$output"
