#!/bin/bash
# Say what a release carries, for the person cutting it, and decide whether they
# confirmed it (#145).
#
# WHY THIS EXISTS. Nothing between the Cut Release button and the published tag
# read what a release contained. v0.11.0 shipped #118, a scheduling change that
# had never run on a real deployment, beside unrelated work and three migrations;
# it broke job assignment on the reference deployment for most of a day, and the
# migrations made it one-way, so the only way out was forward. CI cannot be this
# gate: it proves the tree passes its tests, and #118 passed them.
#
# A REPORT AND A CONFIRMATION, NOT A POLICY. It lists the changes since the
# previous release, one per merged pull request, grouped by the area their
# subject names, and marks every one that touches the scheduler, the ledger
# schema or the node wire. A release that adds a migration says so first, because
# it cannot be undone. It shows what CI said about the very commit being cut,
# without judging it. Then it decides nothing but whether the operator confirmed
# THIS candidate: CONFIRM must be exactly `<tag>@<commit>`, as the preview
# printed it, so a confirmation given for an earlier main, or another tag, cuts
# nothing.
#
# Inputs, from the environment:
#   TAG        the release being cut (plan-release.sh's tag)
#   PREVIOUS   the release it is read against (plan-release.sh's previous); empty
#              only before billet's first release
#   CANDIDATE  the full commit the release would be cut from
#   CONFIRM    empty to preview; `<tag>@<commit>` to confirm
#   GITHUB_OUTPUT      optional: `confirmed=true|false` is written to it
#   GITHUB_REPOSITORY  optional: names the repository whose CI runs are read,
#                      with gh and GH_TOKEN
#
# The report is Markdown on stdout. The exit status is 0 for a preview and for a
# confirmation that matches, and 1 for anything this cannot read or a
# confirmation that does not match.
set -euo pipefail

tag=${TAG:?TAG must name the release being cut}
previous=${PREVIOUS-}
candidate=${CANDIDATE:?CANDIDATE must name the commit the release would be cut from}
confirm=${CONFIRM-}

fail() {
	printf 'release-contents: %s\n' "$1" >&2
	exit 1
}

case "$confirm" in
	*[$'\n\r']*) fail "the confirmation must be a single line" ;;
esac

if ! [[ $candidate =~ ^[0-9a-f]{40}$ ]]; then
	fail "CANDIDATE must be a full commit id, not '$candidate'"
fi

if ! git cat-file -e "$candidate^{commit}" 2>/dev/null; then
	fail "$candidate is not a commit in this repository"
fi

if [ -n "$previous" ] && ! git rev-parse -q --verify "refs/tags/$previous^{commit}" >/dev/null; then
	fail "the previous release $previous is not a tag in this repository"
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

docs='https://billet.readthedocs.io/en/latest/operating/upgrades.html'

{
	printf '## What %s carries\n\n' "$tag"

	if [ -z "$previous" ]; then
		printf 'Cut from `%s`. There is no earlier release, so everything in the tree is new.\n\n' "$candidate"
	else
		printf 'Cut from `%s`, read against `%s`, plus the commit the cut adds to pin billet'"'"'s references to itself at `%s`.\n\n' \
			"$candidate" "$previous" "$tag"

		# AN EARLIER RELEASE ON ANOTHER BRANCH is the new-series case: the list is
		# then everything since this line left it, carries included.
		set +e
		git merge-base --is-ancestor "$previous" "$candidate"
		ancestor=$?
		set -e

		case $ancestor in
			0) ;;
			1)
				printf '`%s` is on another branch, so this is everything since this line left it, and a change already carried to `%s` appears here again.\n\n' \
					"$previous" "$previous"
				;;
			*) fail "git could not say whether $previous is an ancestor of $candidate" ;;
		esac
	fi
} >"$work/report"

# WHAT THE RELEASE IS READ AGAINST: the previous release's tree, or the empty
# tree before billet's first release, so even that one reports its migrations.
if [ -n "$previous" ]; then
	base=$previous
else
	base=$(git hash-object -t tree /dev/null)
fi

# PATHS ARE READ NUL-TERMINATED, as git wrote them. Without -z git quotes a path
# with a non-ASCII byte or a tab, and a quoted path no longer matches the
# directories below, so a change to the scheduler would read as nothing at all.
# A newline inside a pathname is the one spelling this cannot carry, and billet
# has none.

# THE LEDGER SCHEMA, AS TREES, so a migration the previous release already
# carried is not counted again. Added files are new migrations; a published one
# changed or removed is its own alarm, because deployments record its checksum.
git diff -z --no-renames --name-status "$base" "$candidate" -- \
	internal/state/migrations internal/state/pgmigrations | tr '\0' '\n' | paste - - >"$work/schema"

# THE PATH IS EVERYTHING AFTER THE FIRST TAB, which paste put between the status
# and the path; a tab inside the path itself stays part of it.
awk '{ status = $0; sub(/\t.*/, "", status); path = $0; sub(/^[^\t]*\t/, "", path); if (status == "A") print path }' \
	"$work/schema" >"$work/added"
awk '{ status = $0; sub(/\t.*/, "", status); path = $0; sub(/^[^\t]*\t/, "", path); if (status != "A") print status "\t" path }' \
	"$work/schema" >"$work/altered"
awk '{ sub(/^[^\t]*\t/, ""); print }' "$work/schema" >"$work/schema-files"

if [ -s "$work/added" ]; then
	{
		# Counted by name: each migration is one file for SQLite and its twin for
		# PostgreSQL.
		count=$(awk -F / '{ print $NF }' "$work/added" | sort -u | wc -l | tr -d ' ')
		printf '> [!CAUTION]\n'
		printf '> **This release migrates the ledger, and it cannot be rolled back.** It adds %s migration(s):\n>\n' "$count"
		while IFS= read -r file; do
			printf '> - `%s`\n' "$file"
		done <"$work/added"
		printf '>\n> Once a host runs it, an older billet refuses the ledger, and the only way back to it is a backup taken before the upgrade, losing every row written since. A problem this release brings is fixed forward. See [upgrades](%s).\n\n' "$docs"
	} >>"$work/report"
fi

if [ -s "$work/altered" ]; then
	{
		printf '> [!CAUTION]\n'
		printf '> **This release changes or removes a migration an earlier release published.** Every deployment recorded its checksum, so the ledger will refuse it:\n>\n'
		while IFS=$'\t' read -r status file; do
			printf '> - `%s` (%s)\n' "$file" "$status"
		done <"$work/altered"
		printf '\n'
	} >>"$work/report"
fi

# ONE LINE PER MERGED PULL REQUEST: the first-parent history, each commit judged
# by what it changed against its first parent, which is the pull request whether
# it was merged or squashed.
if [ -n "$previous" ]; then
	git log --first-parent --format='%H%x09%s' "$previous..$candidate" >"$work/commits"
else
	git log --first-parent --format='%H%x09%s' "$candidate" >"$work/commits"
fi

: >"$work/rows"

while IFS=$'\t' read -r sha subject; do
	# A TAB IN A SUBJECT would split the row below and cut the title short.
	subject=${subject//$'\t'/ }

	if git rev-parse -q --verify "$sha^1" >/dev/null; then
		git diff -z --no-renames --name-only "$sha^1" "$sha" | tr '\0' '\n' >"$work/files"
	else
		git diff-tree -z --root --no-commit-id --no-renames --name-only -r "$sha" |
			tr '\0' '\n' >"$work/files"
	fi

	# ONLY WHAT SHIPS MARKS A CHANGE: a test, a fixture or a directory's CLAUDE.md
	# changes no behaviour, so the scheduler and the wire are their Go source. The
	# schema is a migration this release adds or changes against the previous one,
	# so a migration that release already carried marks nothing, as the banner
	# above does not count it.
	marks=$(awk '
		FILENAME == ARGV[1] { release[$0] = 1; next }
		/_test\.go$/ || /\/testdata\// { next }
		($0 in release) { schema = 1 }
		/^internal\/(alloc|server)\/.*\.go$/ { scheduler = 1 }
		/^internal\/(nodeapi|nodeclient|nodeplane)\/.*\.go$/ { wire = 1 }
		END {
			out = ""
			if (schema) out = "ledger schema"
			if (scheduler) out = out (out == "" ? "" : ", ") "scheduler"
			if (wire) out = out (out == "" ? "" : ", ") "node wire"
			print out
		}
	' "$work/schema-files" "$work/files")

	# THE AREA IS THE SUBJECT'S OWN `Area:` prefix, the pull request title's.
	area=Other
	if [[ $subject =~ ^([A-Za-z][A-Za-z0-9\ /.-]{0,39}):\  ]]; then
		area=${BASH_REMATCH[1]}
	fi

	printf '%s\t%s\t%s\t%s\n' "$area" "${sha:0:12}" "$marks" "$subject" >>"$work/rows"
done <"$work/commits"

changes=$(wc -l <"$work/rows" | tr -d ' ')

{
	printf '### Flagged\n\n'
	if awk -F '\t' '$3 != "" { found = 1 } END { exit !found }' "$work/rows"; then
		printf 'These change the scheduler, the ledger schema or the node wire, which is where a change that has never run on a real deployment hurts most:\n\n'
		awk -F '\t' '$3 != "" { printf "- **%s**: %s (`%s`)\n", $3, $4, $2 }' "$work/rows"
	else
		printf 'Nothing here touches the scheduler, the ledger schema or the node wire.\n'
	fi
	printf '\n'

	if [ "$changes" = 1 ]; then
		printf '### By area (1 change)\n\n'
	else
		printf '### By area (%s changes)\n\n' "$changes"
	fi
	if [ "$changes" = 0 ]; then
		printf 'No commits since `%s`.\n\n' "$previous"
	else
		# Stable, so each area keeps history order.
		sort -s -t "$(printf '\t')" -k1,1 "$work/rows" | awk -F '\t' '
			$1 != area { if (area != "") printf "\n"; area = $1; printf "#### %s\n\n", area }
			{ printf "- %s (`%s`)%s\n", $4, $2, ($3 == "" ? "" : " - " $3) }
			END { if (area != "") printf "\n" }
		'
	fi
} >>"$work/report"

# WHAT CI SAID ABOUT THIS VERY COMMIT, shown and never judged. Three answers stay
# apart: it ran (and how it ended), nothing ran, or this could not ask.
{
	printf '### CI on `%s`\n\n' "${candidate:0:12}"

	if [ -z "${GITHUB_REPOSITORY:-}" ]; then
		printf 'Not looked up: no repository is named (GITHUB_REPOSITORY), as on a workstation.\n\n'
	elif ! gh api --paginate -X GET "repos/$GITHUB_REPOSITORY/actions/workflows/ci.yml/runs" \
		-f head_sha="$candidate" -f per_page=100 \
		--jq '.workflow_runs[] | [.event, .status, (.conclusion // "none"), .html_url] | @tsv' \
		>"$work/runs" 2>"$work/gh-error"; then
		printf '**Could not read CI for this commit**, so this report cannot say whether it passed: %s\n\n' \
			"$(head -c 300 "$work/gh-error" | tr '\n' ' ')"
	elif [ ! -s "$work/runs" ]; then
		printf '**No CI run on this commit**: nothing has tested this tree as it is. A release branch'"'"'s tip has a run of its own only where that branch'"'"'s own ci.yml runs on pushes to `release/**`; a branch cut before that trigger existed runs CI on pull requests alone.\n\n'
	else
		awk -F '\t' '{ printf "- %s: %s, %s (%s)\n", $1, $2, $3, $4 }' "$work/runs"
		printf '\n'
	fi
} >>"$work/report"

expected="$tag@$candidate"
confirmed=false
status=0

{
	printf '### To cut it\n\n'

	if [ -z "$confirm" ]; then
		printf 'Nothing was cut. To cut `%s` from this commit, run Cut Release again with `confirm` set to:\n\n```\n%s\n```\n' \
			"$tag" "$expected"
	elif [ "$confirm" = "$expected" ]; then
		confirmed=true
		printf 'Confirmed as `%s`: cutting it.\n' "$expected"
	else
		status=1
		printf '**Not cut.** The confirmation names `%s`, and this run would cut `%s`. The preview it was copied from is stale: main moved, the branch moved, or the version changed since. Read this report and confirm again.\n' \
			"$confirm" "$expected"
	fi
} >>"$work/report"

cat "$work/report"

if [ -n "${GITHUB_OUTPUT:-}" ]; then
	printf 'confirmed=%s\n' "$confirmed" >>"$GITHUB_OUTPUT"
fi

if [ "$status" -ne 0 ]; then
	printf '::error::the confirmation %s does not match %s, the release this run would cut\n' "$confirm" "$expected" >&2
fi

exit "$status"
