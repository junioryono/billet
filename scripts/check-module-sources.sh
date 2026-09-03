#!/bin/bash
# Prove every documented Terraform module source names a version, and that the
# version it names carries the module.
#
# WHY THIS EXISTS. `terraform/` was in no release for months and nothing noticed:
# it simply postdated every tag, so a consumer pinning
# `…//terraform/modules/billet?ref=v0.3.26` got "subdir not found" and the only
# refs that resolved were `main` and a bare commit SHA. This repo is otherwise
# unambiguous that a moving target is unacceptable — the Ansible role refuses
# `billet_version: latest` in those words — so the module an operator uses to
# create the instance the binary runs on has to be pinnable too.
#
# TWO MODES, ONE SCRIPT. On main EXPECTED_REF is `main`, which is honest: the
# documentation on main describes main. `cut-release.yml` rewrites every source
# to the release tag on the release branch and runs this again with EXPECTED_REF
# set to it, exactly as it already does for the internal action refs — so the
# README inside a release names that release, and a rename that would break a
# consumer's `terraform init` fails the release instead.
set -euo pipefail

expected_ref=${EXPECTED_REF:-main}
prefix='github.com/junioryono/billet//'

# FROM THE TOP OF THE REPOSITORY, and said rather than assumed. Every path here
# is repository-relative — git grep prints them relative to the working
# directory, `git ls-files --error-unmatch` resolves them the same way, and the
# module sweep globs `terraform/**` — so run from a subdirectory this finds
# nothing and refuses with a message about missing documentation, which is true
# of nowhere and sends the reader to the wrong file.
top=$(git rev-parse --show-toplevel 2>/dev/null) || {
	printf 'not inside a git repository; this check asks git what a tag would carry\n' >&2
	exit 1
}

if [ "$(pwd -P)" != "$(cd "$top" && pwd -P)" ]; then
	printf 'run this from %s; every path it checks is relative to the repository root\n' "$top" >&2
	exit 1
fi

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# EVERY TRACKED MARKDOWN FILE, because a source block is documentation wherever
# it lives and the two module READMEs added for that fix would otherwise have to be
# remembered here as well as written.
#
# `git grep`, NOT `git ls-files | xargs grep`. xargs splits a long file list into
# several grep invocations, and a batch with no match exits 1, which GNU xargs
# reports as 123 — indistinguishable from the "could not look" status below. That
# is a gate that starts refusing releases once the repository grows enough
# markdown, for no reason. git grep is one process over exactly the tracked
# files, which is also the set a tag ships.
#
# Its status is three-valued: 0 found, 1 not found, ANYTHING ABOVE 1 could not
# look. Folding the third into "not found" would turn an unreadable tree into a
# gate that passes, so it is separated.
set +e
git grep -nE "\"${prefix}terraform/[^\"]*\"" -- '*.md' >"$work/matches"
status=$?
set -e

if [ "$status" -gt 1 ]; then
	printf 'could not search the tracked markdown files for module sources (git grep exited %d)\n' "$status" >&2
	exit 1
fi

# A VACUOUS PASS IS THE FAILURE THIS WHOLE FILE IS ABOUT. Zero matches means the
# documentation stopped saying how to consume the module, which is the state
# that made the unshipped module invisible.
if [ ! -s "$work/matches" ]; then
	printf 'no documented Terraform module sources found in any tracked .md file\n' >&2
	printf 'every check below would pass without examining anything\n' >&2
	exit 1
fi

failed=false

while IFS= read -r match; do
	[ -n "$match" ] || continue

	where=${match%%:*}
	rest=${match#*:}
	line=${rest%%:*}

	source_value=$(printf '%s\n' "$match" | sed -E "s#.*\"(${prefix}[^\"]*)\".*#\1#")
	subdir=${source_value#"$prefix"}
	subdir=${subdir%%\?*}
	query=${source_value#*\?}

	if [ "$query" = "$source_value" ]; then
		printf '%s:%s names no version: %s\n' "$where" "$line" "$source_value" >&2
		printf '  a source with no ?ref= resolves to the default branch, which is a moving target\n' >&2
		failed=true

		continue
	fi

	if [ "$query" != "ref=${expected_ref}" ]; then
		printf '%s:%s names ?%s, expected ?ref=%s: %s\n' "$where" "$line" "$query" "$expected_ref" "$source_value" >&2
		failed=true

		continue
	fi

	# TRACKED, NOT MERELY PRESENT. This is the assertion that was missing: a tag
	# carries what git carries, and `test -d` would happily pass on a directory
	# that is ignored, untracked, or built. It runs on the commit about to be
	# tagged, so a source naming a path a release would not ship fails the
	# release rather than the operator's `terraform init`.
	if ! git ls-files --error-unmatch "$subdir" >/dev/null 2>&1; then
		printf '%s:%s names %s, which git does not carry\n' "$where" "$line" "$subdir" >&2
		printf '  a consumer would get "Failed to expand subdir globs" from terraform init\n' >&2
		failed=true

		continue
	fi

	printf '%s\n' "$subdir" >>"$work/referenced"
done <"$work/matches"

# AND EVERY CONSUMABLE MODULE IS DOCUMENTED, which is what stops the checks above
# from being satisfied by deleting the source blocks. A versions.tf is what makes
# a directory something a consumer resolves; the examples deliberately have none
# and source their module by relative path.
touch "$work/referenced"

while IFS= read -r versions; do
	[ -n "$versions" ] || continue

	module=$(dirname "$versions")

	if ! grep -qxF "$module" "$work/referenced"; then
		printf '%s is a consumable module and no tracked .md documents how to pin it\n' "$module" >&2
		printf '  add a usage block naming %s%s?ref=%s\n' "$prefix" "$module" "$expected_ref" >&2
		failed=true
	fi
done < <(git ls-files -- 'terraform/**/versions.tf' 'terraform/versions.tf')

if [ "$failed" = true ]; then
	exit 1
fi

printf 'documented Terraform module sources all resolve to ?ref=%s\n' "$expected_ref"
