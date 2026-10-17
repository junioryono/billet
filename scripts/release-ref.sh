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
# COULD NOT TELL IS NOT `main`. Outside a git work tree, in a shallow clone whose
# history is cut off before the release commit, or in a clone whose history
# reaches a `Release vX.Y.Z` commit that carries no tag, this refuses rather than
# answering: a release branch read as `main` fails for the wrong reason, and a
# tree read as the wrong release passes references reverted to that release.
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

# THE RELEASE TAGS AND THE COMMITS THEY NAME, peeled all the way: an annotated
# tag (what cut-release.yml writes) names a tag object, and a tag of a tag names
# another, which for-each-ref's %(*objectname) peels only once. `^{}` follows the
# chain to the first object that is not a tag, and one cat-file answers for every
# tag in input order. Three answers, kept apart: a commit is a release; a tree or
# a blob names no release and is left out; `missing` is an object this clone
# cannot read, which is could-not-tell and refuses, because skipping it could
# leave an older release on the same commit to answer. cat-file's own failure
# ends this under set -e.
git for-each-ref --format='%(refname:strip=2)' 'refs/tags/v*' >"$work/refs"
awk '/^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/' "$work/refs" >"$work/names"
awk '{ print "refs/tags/" $0 "^{}" }' "$work/names" |
	git cat-file --batch-check='%(objectname) %(objecttype)' 2>/dev/null >"$work/peeled"

if [ "$(wc -l <"$work/names")" -ne "$(wc -l <"$work/peeled")" ]; then
	printf 'release-ref: git answered for %s of %s release tags\n' \
		"$(wc -l <"$work/peeled" | tr -d ' ')" "$(wc -l <"$work/names" | tr -d ' ')" >&2
	exit 1
fi

paste -d ' ' "$work/peeled" "$work/names" >"$work/answers"

if unreadable=$(awk '$2 == "missing" { print $NF; exit 1 }' "$work/answers"); then
	:
else
	printf 'release-ref: the tag %s names an object this clone cannot read\n' "$unreadable" >&2
	exit 1
fi

awk '$2 == "commit" { print $1, $3 }' "$work/answers" >"$work/tags"

git log --first-parent --format='%H %s' HEAD >"$work/chain"

# One pass: the tags by commit, then the chain from HEAD back. The first commit
# carrying a release tag answers, with the highest version on it, compared by
# number rather than as text so v0.4.10 is newer than v0.4.9.
#
# A RELEASE THIS CLONE HAS NO TAG FOR IS COULD-NOT-TELL. Full history does not
# prove the tags came with it (`git fetch --no-tags`), and with them missing a
# release branch would read as main, or as an older release when only its newest
# tag is gone. cut-release.yml commits `Release vX.Y.Z` on the branch it tags,
# so a commit with that exact subject, reached before any tag answers, whose
# vX.Y.Z is no tag in this clone at all, is a tag this clone lacks, and the
# answer is a refusal (status 3). When vX.Y.Z IS a tag, on another commit, this
# one is a copy (a release branch rebased back into main writes one), and the
# walk goes on past it.
#
# THE PATTERN IS A LITERAL IN THE PROGRAM, not passed with -v: awk processes
# escapes in a -v value, so `\.` would arrive as `.` and match any character.
# FILENAME, not FNR == NR, tells the files apart, because with no tags at all
# the first file is empty and FNR == NR holds for the chain as well.
set +e
ref=$(awk '
	# BY DIGITS, NOT AS AWK NUMBERS, which are doubles and call two components
	# past 2^53 equal. The pattern admits no leading zero, so the longer number
	# is the larger, and two of one length compare as strings.
	function newer(a, b,    x, y, i) {
		split(substr(a, 2), x, ".")
		split(substr(b, 2), y, ".")
		for (i = 1; i <= 3; i++) {
			if (length(x[i]) != length(y[i])) return length(x[i]) > length(y[i])
			if (x[i] "" != y[i] "") return x[i] "" > y[i] ""
		}
		return 0
	}
	FILENAME == ARGV[1] {
		known[$2] = 1
		if (!($1 in best) || newer($2, best[$1])) best[$1] = $2
		next
	}
	{
		subject = substr($0, length($1) + 2)
		if (subject ~ /^Release v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$/) {
			version = substr(subject, 9)
			if (!(version in known)) {
				print version " " $1
				done = 1
				exit 3
			}
		}
	}
	$1 in best { print best[$1]; done = 1; exit }
	END { if (!done) print "main" }
' "$work/tags" "$work/chain")
status=$?
set -e

case $status in
	0) ;;
	3)
		printf 'release-ref: commit %s cut %s, and this clone has no %s tag\n' "${ref#* }" "${ref%% *}" "${ref%% *}" >&2
		printf '  fetch the tags (git fetch --tags, or actions/checkout with fetch-depth: 0)\n' >&2
		exit 1
		;;
	*)
		printf 'release-ref: reading the tags against the history failed (awk exited %d)\n' "$status" >&2
		exit 1
		;;
esac

printf '%s\n' "$ref"
