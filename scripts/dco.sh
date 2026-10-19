#!/bin/bash
# Refuse a contributor's pull request with a commit not signed off under the
# Developer Certificate of Origin (https://developercertificate.org).
#
# WHY THIS EXISTS. billet outside ee/ is Apache-2.0, and an outside contribution
# arrives under that license only if its author says the code is theirs to give.
# The DCO is that statement, one trailer per commit: `git commit -s` writes
# "Signed-off-by: Name <email>". A check in CI is the only place every pull
# request passes, so it is where a missing one is caught, before the merge rather
# than in an audit after it.
#
# WHO IS CHECKED. A pull request opened by the repository's owner, or by a bot,
# is not: the owner holds the copyright already, and a bot's commits are
# generated. The exemption reads the pull request's author as GitHub records it
# (DCO_PR_AUTHOR), never a commit's author field, which anyone can set to any
# name and address.
#
# WHAT IS CHECKED. Every commit the pull request adds and its base lacks, merges
# excluded (a merge of main carries no new authorship), must carry a sign-off
# naming that commit's own author. The checkout is GitHub's merge ref, whose
# first parent is the base and second the pull request's head, so HEAD^1..HEAD^2
# is exactly what the merge would bring in; a HEAD of any other shape is refused,
# because checking the wrong range would pass a commit nobody looked at. So is a
# shallow clone, whose range stops at the cut and hides the commits below it, and
# a traversal that fails, whose partial list would read as all signed off.
#
# WHAT IT CANNOT HOLD. The job runs the pull request's own copy of this script
# and workflow, as every pull_request check does, so a contributor's edit to
# either is caught by the maintainer's review of the diff, not by the check.
#
# Environment: DCO_EVENT (github.event_name), DCO_PR_AUTHOR (the pull request
# author's login), DCO_OWNER (github.repository_owner).
set -euo pipefail

if [[ "${DCO_EVENT:-}" != pull_request ]]; then
	echo "dco: a ${DCO_EVENT:-local} run has no contributor to check"
	exit 0
fi

author=${DCO_PR_AUTHOR:-}
owner=${DCO_OWNER:-}
if [[ -z $author || -z $owner ]]; then
	echo "dco: DCO_PR_AUTHOR and DCO_OWNER must both be set on a pull request" >&2
	exit 2
fi

if [[ $author == "$owner" || $author == *'[bot]' ]]; then
	echo "dco: opened by $author, who is not asked to sign off"
	exit 0
fi

if [[ $(git rev-parse --is-shallow-repository) != false ]]; then
	echo "dco: the clone is shallow, so the pull request's commits below the cut are unseen" >&2
	exit 2
fi

parents=$(git rev-list --parents -n 1 HEAD | wc -w)
if [[ $parents -ne 3 ]]; then
	echo "dco: HEAD is not a pull request's merge ref (it has $((parents - 1)) parents), so the commits to check are unknown" >&2
	exit 2
fi

if ! commits=$(git rev-list --no-merges HEAD^1..HEAD^2); then
	echo "dco: listing the pull request's commits failed" >&2
	exit 2
fi

missing=0
for c in $commits; do
	want="Signed-off-by: $(git show -s --format='%an <%ae>' "$c")"
	if ! trailers=$(git show -s --format=%B "$c" | git interpret-trailers --parse); then
		echo "dco: reading $c's trailers failed" >&2
		exit 2
	fi
	if ! grep -ixF -- "$want" >/dev/null <<<"$trailers"; then
		echo "dco: $(git show -s --format='%h %s' "$c") has no \"$want\""
		missing=1
	fi
done

if [[ $missing -ne 0 ]]; then
	cat <<'EOF'
dco: sign off each commit to certify you wrote it or may contribute it
     (https://developercertificate.org). Commit with `git commit -s`; for the
     commits already on this branch, which is the one rewrite billet asks for:
       git rebase --signoff origin/main && git push --force-with-lease
EOF
	exit 1
fi

echo "dco: every commit is signed off by its author"
