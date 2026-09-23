#!/usr/bin/env bash
# Decides how much of CI a change needs, and prints one word: "docs" when every
# path the pull request touches is documentation, "full" otherwise. The reason
# goes to stderr.
#
# ANYTHING IT CANNOT ESTABLISH IS "full". A push, a checkout that is not a pull
# request's merge commit, a git command that fails and an empty change all run
# everything: skipping a job on a guess is a green check over an untested tree,
# and running one too many costs only time.
#
# DOCUMENTATION IS A LIST OF FILE KINDS, NOT A LIST OF DIRECTORIES. A Go file
# under docs/ is compiled by the suite and checked only by the lint a docs run
# skips, so docs/ counts only for what the Sphinx build reads; LICENSE and the
# root README.md are package inputs (.goreleaser.yaml), so a change to either is
# a change to what ships. Every path must also be a regular, non-executable file
# before and after: a symlink, a submodule, an executable bit or a mode change is
# not documentation whatever its name.
#
# The pull request is read from its merge commit (actions/checkout's default for
# pull_request), whose first parent is the base branch's tip, so HEAD^1..HEAD is
# exactly what merging would change. --no-renames lists both sides of a rename:
# with rename detection, moving a file from cmd/ into docs/ reads as docs only.
set -euo pipefail

decide() {
	printf '%s\n' "$1"
	printf 'ci-scope: %s: %s\n' "$1" "$2" >&2
	exit 0
}

is_documentation() {
	case "$1" in
	docs/*.go) return 1 ;;
	docs/*.md | docs/*.rst | docs/*.css | docs/*.html | docs/*.txt | docs/*.png | docs/*.svg | docs/*.jpg) return 0 ;;
	docs/conf.py | docs/Makefile) return 0 ;;
	.claude/*.md | CLAUDE.md) return 0 ;;
	*) return 1 ;;
	esac
}

# A regular file (100644), or no file at all (000000: the path was added or
# deleted on the other side).
is_plain_mode() {
	[ "$1" = 100644 ] || [ "$1" = 000000 ]
}

if [ "${CI_EVENT:-}" != pull_request ]; then
	decide full "event '${CI_EVENT:-}' is not a pull request"
fi

if ! git rev-parse --verify --quiet HEAD^2 >/dev/null; then
	decide full "HEAD is not a merge commit, so its change cannot be read from HEAD^1"
fi

list=$(mktemp)
trap 'rm -f "$list"' EXIT

# --raw -z prints, per path, ":<old mode> <new mode> <old id> <new id> <status>"
# then the path, each NUL-terminated.
if ! git diff --no-renames --raw -z HEAD^1 HEAD >"$list"; then
	decide full "git diff HEAD^1 HEAD failed"
fi

# EVERY RECORD MUST BE WHOLE. A header without its path, a path without its
# terminator or a header of any other shape is output this script cannot read,
# and what it cannot read is "full": a loop that simply stopped there would have
# judged only the records before it.
record='^:([0-7]{6}) ([0-7]{6}) [0-9a-f]+ [0-9a-f]+ [ADMTUX]$'
count=0
while :; do
	meta=
	if ! IFS= read -r -d '' meta; then
		if [ -z "$meta" ]; then
			break
		fi
		decide full "git diff's output ends inside a record header"
	fi
	path=
	if ! IFS= read -r -d '' path; then
		decide full "a git diff record has no terminated path"
	fi
	if ! [[ $meta =~ $record ]]; then
		decide full "git diff printed a record this script cannot read: $meta"
	fi
	old_mode=${BASH_REMATCH[1]}
	new_mode=${BASH_REMATCH[2]}
	count=$((count + 1))
	if ! is_documentation "$path"; then
		decide full "touches $path"
	fi
	if ! is_plain_mode "$old_mode" || ! is_plain_mode "$new_mode"; then
		decide full "$path is not a regular file on both sides ($old_mode -> $new_mode)"
	fi
done <"$list"

if [ "$count" -eq 0 ]; then
	decide full "the change lists no paths"
fi

decide docs "all $count paths are documentation"
