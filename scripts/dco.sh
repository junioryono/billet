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
# WHAT IS CHECKED. Every commit the pull request adds and its base lacks must
# carry a sign-off naming that commit's own author, merges included: a merge can
# carry new code or a substantive conflict resolution, so `git merge --signoff`.
# GitHub's own merge ref is HEAD, outside the range, so it is never asked. The checkout is GitHub's merge ref, whose
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

if ! commits=$(git rev-list HEAD^1..HEAD^2); then
	echo "dco: listing the pull request's commits failed" >&2
	exit 2
fi

# A PARSER NO CONFIGURATION REACHES. git reads trailer aliases
# (trailer.<token>.key), separators and the comment character from every config
# file and the environment, and an alias can forge a sign-off out of another line
# or hide a real one. So the parse runs in an empty directory no repository is
# found above, with no global, system or environment configuration.
#
# THE CEILING IS THE PARENT OF WHERE THE PARSE RUNS. git stops before entering a
# ceiling, not at one, so the parse runs in a child of the temporary directory
# and the ceiling is that directory's physical path; a TMPDIR inside a checkout
# would otherwise let git walk up into it. The template is explicit because BSD
# mktemp -d ignores TMPDIR without one, and GNU mktemp honours it.
if ! iso=$(mktemp -d "${TMPDIR:-/tmp}/dco.XXXXXX"); then
	echo "dco: no temporary directory to parse trailers in" >&2
	exit 2
fi
trap 'rm -rf "$iso"' EXIT
iso=$(cd -P "$iso" && pwd -P)
# A COLON SPLITS THE CEILING into two paths, neither of them this one.
if [[ $iso == *:* ]]; then
	echo "dco: the temporary directory $iso holds a colon, which GIT_CEILING_DIRECTORIES cannot name" >&2
	exit 2
fi
mkdir "$iso/parse"
parse_trailers() {
	(cd "$iso/parse" && env -u GIT_CONFIG_PARAMETERS -u GIT_CONFIG_COUNT -u GIT_DIR \
		GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_SYSTEM=/dev/null GIT_CONFIG_NOSYSTEM=1 \
		GIT_CEILING_DIRECTORIES="$iso" git -c trailer.separators=: interpret-trailers --parse)
}

missing=0
for c in $commits; do
	# UTF-8 AND NO SIGNATURE REPORT, BOTH TIMES: i18n.logOutputEncoding would
	# re-encode the name and the message, breaking a valid sign-off or
	# transliterating two names into one, and log.showSignature would add a
	# signed commit's verification lines to either.
	author=$(git show -s --no-show-signature --encoding=UTF-8 --format='%an <%ae>' "$c")
	if [[ $author == *$'\n'* ]]; then
		echo "dco: $c's author read as more than one line" >&2
		exit 2
	fi
	want="Signed-off-by: $author"
	if ! trailers=$(git show -s --no-show-signature --encoding=UTF-8 --format=%B "$c" | parse_trailers); then
		echo "dco: reading $c's trailers failed" >&2
		exit 2
	fi
	if ! grep -ixF -- "$want" >/dev/null <<<"$trailers"; then
		echo "dco: $(git show -s --format='%h %s' "$c") has no \"$want\""
		missing=1
	fi
done

if [[ $missing -ne 0 ]]; then
	# THE REPAIR NAMES BILLET'S REPOSITORY AND THIS PULL REQUEST'S BASE, never
	# origin/main: a contributor's origin is usually their fork, whose main may be
	# far behind, and squashing onto a stale base would make work main has since
	# reverted look like the contributor's own and bring it back.
	#
	# AND IT NAMES WHERE THE PUSH GOES. A bare push follows push.default and the
	# branch's tracking, which can update another branch or several; the job knows
	# the pull request's head repository, branch and commit, so the push names all
	# three, the commit as the lease.
	upstream=${DCO_UPSTREAM:-"<billet's repository URL>"}
	base=${DCO_BASE_REF:-"<your pull request's base branch>"}
	head_repo=${DCO_HEAD_REPO:-"<your fork's URL>"}
	head_ref=${DCO_HEAD_REF:-"<your branch>"}
	head_sha=${DCO_HEAD_SHA:-"<your branch's commit>"}
	cat <<'EOF'
dco: sign off each commit to certify you wrote it or may contribute it
dco: (https://developercertificate.org). Commit with `git commit -s`, and merge
dco: main with `git merge --signoff`: GitHub's "Update branch" button and web
dco: conflict editor make unsigned merges. To repair this branch, the one
dco: rewrite billet asks for, squash it into one signed-off commit, which keeps
dco: its contents exactly, merges included. On the branch, with nothing
dco: uncommitted or staged (`git status --short` prints nothing), run these, and
dco: stop if one fails. The first fails when your branch is not the commit this
dco: run checked (a commit added on GitHub or another machine): pull it, then rerun
dco: CI for current instructions. The third fails when the branch has more than
dco: one merge base with billet's (a criss-cross history), where no single squash
dco: keeps what a merge into billet would give; say so on the pull request instead.
EOF
	# THE BRANCH IS THE ONE CHECKED, or the squash covers less than the pull request
	# holds and the exact lease, which the remote still satisfies, deletes the rest.
	printf 'dco:   $ test "$(git rev-parse HEAD)" = %q\n' "$head_sha"
	# FULLY QUALIFIED: a branch named +x would otherwise read as a forced fetch of x,
	# and a short name can match a tag.
	printf 'dco:   $ git fetch %q %q\n' "$upstream" "refs/heads/$base"
	# shellcheck disable=SC2016 # printed for the contributor to run, unexpanded.
	printf 'dco:   $ %s\n' 'test -z "$(git merge-base --all HEAD FETCH_HEAD | sed 1d)"' \
		'git reset --soft "$(git merge-base HEAD FETCH_HEAD)"' 'git commit -s'
	printf 'dco:   $ git push %q %q %q\n' "--force-with-lease=refs/heads/$head_ref:$head_sha" \
		"$head_repo" "HEAD:refs/heads/$head_ref"
	exit 1
fi

echo "dco: every commit is signed off by its author"
