#!/usr/bin/env bash
# Decides which job families of CI a change needs. It prints one line per family,
# `<family>=true` or `<family>=false`, in a form written straight to
# $GITHUB_OUTPUT, plus `schema=2`; the reason goes to stderr.
#
# ANYTHING IT CANNOT ESTABLISH SELECTS EVERY FAMILY. A push, a checkout that is
# not a pull request's merge commit, a git command that fails, an empty change,
# a record this script cannot read, a mode change, a symlink, a submodule and a
# path no rule below names all select everything: skipping a job on a guess is
# a green check over an untested tree, and running one too many costs only time.
#
# EACH RULE IS A SAFE SUPERSET OF WHAT A FAMILY READS, derived from the jobs'
# steps, the Makefile targets they run and the Go closures of what they build,
# not from names. A family is skipped only when no changed path can reach it.
# A path may select several families; a path no rule names selects all of them.
# The jobs that always run (changes, test, release-config, docs) are not
# families: test in particular reads scripts, actions, workflows, docs, Ansible,
# Terraform and source text, so every path is still exercised somewhere.
#
# DOCUMENTATION IS A LIST OF FILE KINDS, NOT A LIST OF DIRECTORIES. A Go file
# under docs/ is compiled by the suite and checked only by lint, so docs/ counts
# only for what the Sphinx build reads; LICENSE and the root README.md are
# package inputs (.goreleaser.yaml).
#
# The pull request is read from its merge commit (actions/checkout's default for
# pull_request), whose first parent is the base branch's tip, so HEAD^1..HEAD is
# exactly what merging would change. --no-renames lists both sides of a rename:
# with rename detection, moving a file out of cmd/ would be judged by its
# destination alone.
set -euo pipefail

families=(host_lifecycle postgres postgres_command postgres_retirement package_lifecycle
	terraform terraform_consumer_floor terraform_lint replay lint)

# The selected families, space-delimited with a space at each end, so membership
# is one exact-word match. Not an associative array: a Mac's bash is 3.2.
selected=" "

is_selected() {
	case "$selected" in *" $1 "*) return 0 ;; *) return 1 ;; esac
}

select_families() {
	local f
	for f in "$@"; do
		is_selected "$f" || selected="$selected$f "
	done
}

emit() {
	local f
	printf 'schema=2\n'
	for f in "${families[@]}"; do
		if is_selected "$f"; then printf '%s=true\n' "$f"; else printf '%s=false\n' "$f"; fi
	done
}

# everything selects every family and ends the decision.
everything() {
	select_families "${families[@]}"
	emit
	printf 'ci-scope: every family: %s\n' "$1" >&2
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

# The packages the replay harness builds and runs (go list -deps of
# internal/replay's tests), with their embedded assets and testdata. provider
# and store mean their own directories, not their backends' subtrees.
is_replay_input() {
	case "$1" in
	internal/provider/simulated/*) return 0 ;;
	internal/provider/*/*) return 1 ;;
	internal/provider/*) return 0 ;;
	internal/store/*/*) return 1 ;;
	internal/store/*) return 0 ;;
	internal/alloc/* | internal/config/* | internal/deploymentid/* | internal/durablefile/* | \
		internal/endpoint/* | internal/fakeactions/* | internal/github/* | internal/importcheck/* | \
		internal/node/* | internal/nodeapi/* | internal/nodeclient/* | internal/nodeplane/* | \
		internal/provenance/* | internal/regularfile/* | internal/replay/* | internal/retirement/* | \
		internal/rollout/* | internal/scaleset/* | internal/server/* | internal/state/* | \
		internal/version/* | internal/wirecert/* | internal/wiring/*) return 0 ;;
	*) return 1 ;;
	esac
}

# classify selects the families one path reaches, or returns 1 when no rule
# names the path, which the caller turns into every family.
classify() {
	local p=$1

	# Global: what drives the jobs themselves, and the module graph.
	case "$p" in
	.github/* | Makefile | sqlc.yaml | go.work | go.work.sum | */go.mod | */go.sum | go.mod | go.sum | \
		scripts/ci-scope.sh)
		select_families "${families[@]}"
		return 0
		;;
	esac

	if is_documentation "$p"; then
		return 0
	fi

	local known=1

	case "$p" in
	*.go)
		known=0
		select_families lint
		# The PostgreSQL suite's raw-SQL walk reads every non-test Go file.
		case "$p" in *_test.go) ;; *) select_families postgres ;; esac
		;;
	esac

	case "$p" in
	cmd/* | internal/* | deploy/*)
		known=0
		select_families host_lifecycle postgres_command postgres_retirement package_lifecycle terraform lint
		;;
	esac

	case "$p" in
	internal/alloc/* | internal/config/* | internal/deploymentid/* | internal/regularfile/* | \
		internal/state/* | internal/version/*)
		known=0
		select_families postgres
		;;
	esac

	if is_replay_input "$p"; then
		known=0
		select_families replay
	fi

	case "$p" in
	ansible_collections/*)
		known=0
		select_families host_lifecycle postgres_command postgres_retirement
		;;
	scripts/check-retirement-shards.py)
		known=0
		select_families host_lifecycle postgres_retirement
		;;
	.goreleaser.yaml | README.md | LICENSE | billet.example.yaml | scripts/mkreleasemanifest/* | \
		scripts/test-package-lifecycle.sh | scripts/test-restore-rehearsal.sh | scripts/restore-rehearsal.sh | \
		scripts/test-postgres-restore-rehearsal.sh | scripts/postgres-restore-rehearsal.sh)
		known=0
		select_families package_lifecycle
		;;
	terraform/*)
		known=0
		select_families terraform terraform_lint
		case "$p" in terraform/modules/billet/*) select_families terraform_consumer_floor ;; esac
		;;
	scripts/hybrid-root-check.sh)
		known=0
		select_families terraform
		;;
	tools/lint/* | .golangci.yml)
		known=0
		select_families lint
		;;
	# Read only by the jobs that always run: test executes and inspects these,
	# and release-config reads the module sources.
	actions/* | scripts/*_test.go | scripts/install.sh | scripts/build-guest-image.sh | \
		scripts/check-guest-image.sh | scripts/boot-guest-image.sh | scripts/check-module-sources.sh)
		known=0
		;;
	esac

	return "$known"
}

if [ "${CI_EVENT:-}" != pull_request ]; then
	everything "event '${CI_EVENT:-}' is not a pull request"
fi

if ! git rev-parse --verify --quiet HEAD^2 >/dev/null; then
	everything "HEAD is not a merge commit, so its change cannot be read from HEAD^1"
fi

list=$(mktemp)
trap 'rm -f "$list"' EXIT

# --raw -z prints, per path, ":<old mode> <new mode> <old id> <new id> <status>"
# then the path, each NUL-terminated.
if ! git diff --no-renames --raw -z HEAD^1 HEAD >"$list"; then
	everything "git diff HEAD^1 HEAD failed"
fi

# EVERY RECORD MUST BE WHOLE. A header without its path, a path without its
# terminator or a header of any other shape is output this script cannot read,
# and what it cannot read selects everything: a loop that simply stopped there
# would have judged only the records before it.
record='^:([0-7]{6}) ([0-7]{6}) [0-9a-f]+ [0-9a-f]+ [ADMTUX]$'
count=0
docs_only=true
while :; do
	meta=
	if ! IFS= read -r -d '' meta; then
		if [ -z "$meta" ]; then
			break
		fi
		everything "git diff's output ends inside a record header"
	fi
	path=
	if ! IFS= read -r -d '' path; then
		everything "a git diff record has no terminated path"
	fi
	if ! [[ $meta =~ $record ]]; then
		everything "git diff printed a record this script cannot read: $meta"
	fi
	old_mode=${BASH_REMATCH[1]}
	new_mode=${BASH_REMATCH[2]}
	count=$((count + 1))

	# A symlink or a submodule on either side, or any mode transition, is not a
	# change this script can scope: an executable bit can turn a data file into
	# something a job runs.
	case "$old_mode $new_mode" in
	*120000* | *160000*) everything "$path is a symlink or a submodule ($old_mode -> $new_mode)" ;;
	esac
	if [ "$old_mode" != 000000 ] && [ "$new_mode" != 000000 ] && [ "$old_mode" != "$new_mode" ]; then
		everything "$path changes mode ($old_mode -> $new_mode)"
	fi
	# Documentation must also be a regular, non-executable file.
	if is_documentation "$path"; then
		case "$old_mode $new_mode" in
		"100644 100644" | "000000 100644" | "100644 000000") ;;
		*) everything "$path is documentation-shaped but not a regular file ($old_mode -> $new_mode)" ;;
		esac
	else
		docs_only=false
	fi

	if ! classify "$path"; then
		everything "no rule names $path"
	fi
done <"$list"

if [ "$count" -eq 0 ]; then
	everything "the change lists no paths"
fi

emit
if $docs_only; then
	printf 'ci-scope: all %d paths are documentation; no family\n' "$count" >&2
else
	chosen=$(printf '%s' "$selected" | sed -e 's/^ *//' -e 's/ *$//')
	printf 'ci-scope: %d paths select: %s\n' "$count" "${chosen:-no family}" >&2
fi
