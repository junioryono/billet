#!/bin/bash
# Print the ref this tree's own references must name: the release it belongs to,
# or `main`.
#
# WHY THIS EXISTS. Two checks hold billet's references to itself -- the bundled
# actions' sibling `uses:` (actions/siblingrefs_test.go) and the documented
# Terraform module sources and fleet action (check-module-sources.sh) -- and
# both assumed `main`. cut-release.yml rewrites every one of them to the tag on
# the release branch, so CI on a release branch failed precisely because the
# branch was correct (#149), and a real regression there could not be told from
# the failure every carry merged through.
#
# THE ANSWER IS THE NEAREST RELEASE ON HEAD'S FIRST-PARENT CHAIN. cut-release.yml
# commits the rewrite on the release branch and tags that commit, so a release
# branch's own history passes through its newest `Release vX.Y.Z` commit, and
# the refs in its tree name that tag until the next cut rewrites them. main's
# first-parent history passes through no release commit, so it answers `main`.
#
# FIRST-PARENT, NOT REACHABILITY. A hotfix is merged back into main, which makes
# every tag on that release branch reachable from main; `git tag --merged` would
# then make main's own CI expect a release ref. The first-parent chain of a
# merge into main stays on main.
#
# COULD NOT TELL IS NOT `main`. Outside a git work tree, or in a shallow clone
# whose history is cut off before the release commit, this refuses rather than
# answering: a release branch read as `main` fails for the wrong reason, and a
# main read as a release would pass a tree that composes an old release.
set -euo pipefail

if ! inside=$(git rev-parse --is-inside-work-tree 2>/dev/null) || [ "$inside" != true ]; then
	printf 'release-ref: not inside a git work tree, so which release this tree belongs to cannot be told\n' >&2
	exit 1
fi

if ! shallow=$(git rev-parse --is-shallow-repository); then
	printf 'release-ref: git could not say whether this clone is shallow\n' >&2
	exit 1
fi

if [ "$shallow" != false ]; then
	printf 'release-ref: this clone is shallow, so its history may stop before the release commit it belongs to\n' >&2
	printf '  fetch the whole history (actions/checkout with fetch-depth: 0, or git fetch --unshallow --tags)\n' >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# PEELED, so an annotated tag (what cut-release.yml writes) names its commit
# rather than the tag object, and a lightweight one names its commit as it is.
git for-each-ref \
	--format='%(if)%(*objectname)%(then)%(*objectname)%(else)%(objectname)%(end) %(refname:strip=2)' \
	'refs/tags/v*' >"$work/tags"

git rev-list --first-parent HEAD >"$work/chain"

# One pass: the tags by commit, then the chain from HEAD back. The first commit
# carrying a release tag answers, with the highest version on it, compared by
# number rather than as text so v0.4.10 is newer than v0.4.9.
#
# THE PATTERN IS A LITERAL IN THE PROGRAM, not passed with -v: awk processes
# escapes in a -v value, so `\.` would arrive as `.` and match any character.
# FILENAME, not FNR == NR, tells the files apart, because with no tags at all
# the first file is empty and FNR == NR holds for the chain as well.
ref=$(awk '
	function newer(a, b,    x, y, i) {
		split(substr(a, 2), x, ".")
		split(substr(b, 2), y, ".")
		for (i = 1; i <= 3; i++) {
			if (x[i] + 0 != y[i] + 0) return x[i] + 0 > y[i] + 0
		}
		return 0
	}
	FILENAME == ARGV[1] {
		if ($2 ~ /^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/ &&
			(!($1 in best) || newer($2, best[$1]))) best[$1] = $2
		next
	}
	$1 in best { print best[$1]; found = 1; exit }
	END { if (!found) print "main" }
' "$work/tags" "$work/chain")

printf '%s\n' "$ref"
