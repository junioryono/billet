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

if [ "${CI_EVENT:-}" != pull_request ]; then
	decide full "event '${CI_EVENT:-}' is not a pull request"
fi

if ! git rev-parse --verify --quiet HEAD^2 >/dev/null; then
	decide full "HEAD is not a merge commit, so its change cannot be read from HEAD^1"
fi

list=$(mktemp)
trap 'rm -f "$list"' EXIT

if ! git diff --no-renames --name-only -z HEAD^1 HEAD >"$list"; then
	decide full "git diff HEAD^1 HEAD failed"
fi

count=0
while IFS= read -r -d '' path; do
	count=$((count + 1))
	case "$path" in
	docs/* | *.md | .claude/* | .agents/* | LICENSE) ;;
	*) decide full "touches $path" ;;
	esac
done <"$list"

if [ "$count" -eq 0 ]; then
	decide full "the change lists no paths"
fi

decide docs "all $count paths are documentation"
