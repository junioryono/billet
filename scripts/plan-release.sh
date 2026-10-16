#!/bin/bash
set -euo pipefail

requested=${REQUESTED:-}
output=${GITHUB_OUTPUT:?GITHUB_OUTPUT must name the workflow output file}

# THE CALLER MUST PREVIEW AND CONFIRM (#145). Re-running an old Cut Release run
# from the Actions page runs that run's own workflow file, which predates the
# preview: it checks out main, runs THIS planner, and cuts whatever main is with
# nobody having read it. The planner is the one step every version of the
# workflow shares, so it is where an old caller is turned away.
if [ "${BILLET_CUT_PROTOCOL:-}" != preview-confirm ]; then
	echo "::error::this run of Cut Release predates its preview and confirmation; start a new run from the Actions page instead of re-running this one" >&2
	exit 1
fi

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

# THE RELEASE THIS ONE IS READ AGAINST (release-contents.sh): the series' newest
# for a patch, which is what the branch carried until now, and the newest release
# of all for a new series, which is what an operator is moving from. Empty only
# before billet's first release.
previous=${series_newest:-$newest}

{
	echo "tag=$tag"
	echo "branch=$branch"
	echo "collection_version=$collection_version"
	echo "previous=$previous"
} >> "$output"
