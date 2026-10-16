#!/usr/bin/env bash
# verify-changed.sh: check what this checkout changed, and nothing else.
#
# `make verify` runs it. It is the local check while working; CI runs the full
# gate (`make check`'s steps, the race suite, the linux lint pass, billet's own
# analyzers, the cross builds, PostgreSQL, the docs and terraform jobs) on every
# push. A full gate on a shared Mac is what starved one into a kernel panic on
# 2026-10-15, so this checks the changed packages and leaves the tree to CI.
#
# WHAT CHANGED is everything between the merge base with the base ref
# (VERIFY_BASE, default origin/main) and the working tree: commits on this
# branch, staged and unstaged edits, and untracked files git does not ignore. A
# merge base that cannot be found is could-not-tell, never "nothing changed":
# the script says so and exits 2.
#
# WHAT IT RUNS, for the module each changed package belongs to:
#   - gofmt -s -l on every changed .go file;
#   - go build ./... once any package of the module changed;
#   - go mod tidy -diff when go.mod or go.sum changed;
#   - go vet and go test on the changed packages: a package is the directory of
#     a changed .go file, or the nearest directory above a changed non-Go file
#     that holds Go files (an embedded asset, a script a test executes), with
#     any testdata component cut off first, because the go tool ignores
#     testdata. .claude/, .github/ and the Makefile are read by the tests in
#     scripts/, so they select it; a directory's CLAUDE.md or AGENTS.md selects
#     nothing. go test runs without -race, with the
#     ordinary test cache, at -p 2 and GOMAXPROCS=4: -race made internal/alloc
#     21 times slower (23 s against 491 s, measured 2026-10-15);
#   - golangci-lint on the changed packages of the root module, for this
#     platform, with --allow-serial-runners because its lock is machine-wide and
#     a second run without it fails rather than waits. Packages, never files: a
#     single file is type-checked without the rest of its package (22 bogus
#     `undefined` errors on one file, measured 2026-10-15).
# A changed docs/ file runs `make docs` when sphinx-build is on PATH (15 s and
# 150 MB, measured 2026-10-15) and otherwise says CI's docs job runs it. A
# changed .tf file runs `terraform fmt -check` on it when terraform is on PATH
# and names the terraform gates CI runs.
#
# Exit status: 0 when every check that ran passed, 1 when one failed, 2 when the
# change could not be read. A required tool that is missing is a failure. Every
# step runs even after an earlier one failed, so one run reports everything.

set -uo pipefail

say() {
	printf 'verify: %s\n' "$*"
}

cannot_tell() {
	printf 'verify: could not tell what changed: %s\n' "$*" >&2
	exit 2
}

if ! root=$(git rev-parse --show-toplevel 2>/dev/null); then
	cannot_tell "this is not a git checkout"
fi
if ! cd "$root"; then
	cannot_tell "could not enter $root"
fi

base_ref=${VERIFY_BASE:-origin/main}
if ! base=$(git merge-base "$base_ref" HEAD 2>/dev/null); then
	cannot_tell "no merge base between $base_ref and HEAD; run \`git fetch origin main\`, or name the base in VERIFY_BASE"
fi

if ! list=$(mktemp "${TMPDIR:-/tmp}/verify-changed.XXXXXX"); then
	cannot_tell "could not make a temporary file"
fi
sorted=$list.sorted
trap 'rm -f "$list" "$sorted"' EXIT

# --no-renames lists both sides of a rename, so a file moved out of a package
# still selects the package it left.
if ! git diff --name-only -z --no-renames "$base" -- >"$list"; then
	cannot_tell "git diff against $base failed"
fi
if ! git ls-files --others --exclude-standard -z >>"$list"; then
	cannot_tell "git ls-files could not list the untracked files"
fi
# Sorted into a file of its own, with its status read, rather than through a
# process substitution whose failure would read as an empty change.
if ! LC_ALL=C sort -zu "$list" >"$sorted"; then
	cannot_tell "could not sort the changed paths"
fi

: "${GOMAXPROCS:=4}"
export GOMAXPROCS

failed=()
fail() {
	failed+=("$1")
}

# run DESCRIPTION COMMAND [ARG...] prints the command, runs it and records a
# failure under DESCRIPTION.
run() {
	local what=$1
	shift
	printf 'verify: $ %s\n' "$*"
	if ! "$@"; then
		fail "$what"
	fi
}

# Sets, as newline-free arrays: membership is a linear scan, because a Mac's
# bash 3.2 has no associative arrays.
contains() {
	local needle=$1 item
	shift
	for item in "$@"; do
		[ "$item" = "$needle" ] && return 0
	done
	return 1
}

has_go() {
	local f
	for f in "$1"/*.go; do
		[ -e "$f" ] && return 0
	done
	return 1
}

# outside_testdata DIR prints DIR with everything from its first testdata
# component cut off.
outside_testdata() {
	local d=$1
	case "$d" in
	testdata | testdata/*)
		printf '.'
		return
		;;
	esac
	d="$d/"
	d=${d%%/testdata/*}
	printf '%s' "${d%/}"
}

# module_of DIR prints the nearest directory at or above DIR holding a go.mod.
module_of() {
	local d=$1
	while :; do
		if [ -f "$d/go.mod" ]; then
			printf '%s' "$d"
			return 0
		fi
		[ "$d" = . ] && return 1
		d=$(dirname "$d")
	done
}

go_files=()
pkg_dirs=()
pkg_why=()
mod_changed=()
build_only=()
docs_files=()
tf_files=()
notes=()

add_pkg() {
	local dir=$1 why=$2
	if contains "$dir" ${pkg_dirs[@]+"${pkg_dirs[@]}"}; then
		return
	fi
	pkg_dirs+=("$dir")
	pkg_why+=("$why")
}

count=0
while IFS= read -r -d '' path; do
	count=$((count + 1))
	dir=$(outside_testdata "$(dirname "$path")")

	case "$path" in
	go.mod | go.sum | */go.mod | */go.sum)
		mdir=$(dirname "$path")
		if ! contains "$mdir" ${mod_changed[@]+"${mod_changed[@]}"}; then
			mod_changed+=("$mdir")
		fi
		continue
		;;
	*.go)
		if [ -f "$path" ]; then
			go_files+=("$path")
		fi
		if has_go "$dir"; then
			add_pkg "$dir" "$path changed"
		else
			# A package that is gone still has importers to build.
			notes+=("$dir holds no Go file any more; go build ./... covers what imported it")
			if m=$(module_of "$dir"); then
				contains "$m" ${mod_changed[@]+"${mod_changed[@]}"} ||
					contains "$m" ${build_only[@]+"${build_only[@]}"} || build_only+=("$m")
			fi
		fi
		continue
		;;
	esac

	case "$path" in
	docs/*) docs_files+=("$path") ;;
	esac
	case "$path" in
	*.tf | *.tfvars | *.tftest.hcl) tf_files+=("$path") ;;
	esac
	case "$path" in
	CLAUDE.md | */CLAUDE.md | AGENTS.md | */AGENTS.md)
		# Instructions for a session, which no package reads.
		continue
		;;
	.claude/* | .github/* | Makefile)
		if has_go scripts; then
			add_pkg scripts "$path changed, and scripts/ tests what it says"
		fi
		continue
		;;
	esac

	# The nearest directory holding Go files reads this file, if any does.
	d=$dir
	while [ "$d" != . ]; do
		if has_go "$d"; then
			add_pkg "$d" "$path changed beside it"
			break
		fi
		d=$(dirname "$d")
	done
done <"$sorted"

if [ "$count" -eq 0 ]; then
	say "nothing changed against $base_ref ($base); nothing to verify"
	exit 0
fi
say "changed paths: $count, against $base_ref ($base); Go runs with GOMAXPROCS=$GOMAXPROCS"

for note in ${notes[@]+"${notes[@]}"}; do
	say "$note"
done

if [ "${#go_files[@]}" -gt 0 ]; then
	say "gofmt: ${#go_files[@]} changed Go files"
	printf 'verify: $ gofmt -s -l <%d files>\n' "${#go_files[@]}"
	if ! unformatted=$(gofmt -s -l "${go_files[@]}"); then
		fail "gofmt could not read every changed file"
	fi
	if [ -n "$unformatted" ]; then
		printf 'verify: these files need gofmt -s:\n%s\n' "$unformatted"
		fail "gofmt"
	fi
fi

# The modules to check: every one a changed package or a changed go.mod is in.
modules=()
for i in ${pkg_dirs[@]+"${!pkg_dirs[@]}"}; do
	if ! m=$(module_of "${pkg_dirs[$i]}"); then
		say "${pkg_dirs[$i]} is in no Go module; not checked"
		continue
	fi
	contains "$m" ${modules[@]+"${modules[@]}"} || modules+=("$m")
done
for m in ${mod_changed[@]+"${mod_changed[@]}"} ${build_only[@]+"${build_only[@]}"}; do
	contains "$m" ${modules[@]+"${modules[@]}"} || modules+=("$m")
done

for m in ${modules[@]+"${modules[@]}"}; do
	label=$m
	[ "$m" = . ] && label="the root module"

	pkgs=()
	for i in ${pkg_dirs[@]+"${!pkg_dirs[@]}"}; do
		d=${pkg_dirs[$i]}
		[ "$(module_of "$d")" = "$m" ] || continue
		if [ "$d" = "$m" ]; then
			rel=.
		elif [ "$m" = . ]; then
			rel=./$d
		else
			rel=./${d#"$m"/}
		fi
		pkgs+=("$rel")
		say "$label: $rel (${pkg_why[$i]})"
	done

	if ! (
		cd "$m" || exit 1
		ok=0
		say "$label: go build ./..."
		go build ./... || ok=1
		if contains "$m" ${mod_changed[@]+"${mod_changed[@]}"}; then
			say "$label: go.mod or go.sum changed, so go mod tidy -diff"
			go mod tidy -diff || ok=1
		fi
		exit "$ok"
	); then
		fail "$label: build or tidy"
	fi

	[ "${#pkgs[@]}" -gt 0 ] || continue

	if ! (cd "$m" && printf 'verify: $ go vet %s\n' "${pkgs[*]}" && go vet "${pkgs[@]}"); then
		fail "$label: go vet"
	fi
	if ! (cd "$m" && printf 'verify: $ go test -p 2 %s\n' "${pkgs[*]}" && go test -p 2 "${pkgs[@]}"); then
		fail "$label: go test"
	fi

	# golangci-lint runs for the root module, as make lint does; tools/lint is
	# checked by its own tests and by billetlint in CI.
	# As make lint does: from the physical path, and never from a path holding glob
	# syntax, which depguard reads as a pattern so a rule could check no file.
	if [ "$m" = . ]; then
		if ! here=$(pwd -P); then
			fail "golangci-lint (could not read this directory)"
		elif case "$here" in *[][*?{}]*) true ;; *) false ;; esac then
			say "$here contains glob syntax ([ ] * ? { }), which depguard would read as a pattern; lint from a checkout whose path has none"
			fail "golangci-lint (checkout path)"
		elif command -v golangci-lint >/dev/null 2>&1; then
			run "golangci-lint" env PWD="$here" golangci-lint run --allow-serial-runners --concurrency=4 "${pkgs[@]}"
		else
			say "golangci-lint is not installed; \`make tools\` installs the pinned version"
			fail "golangci-lint (not installed)"
		fi
	fi
done

if [ "${#docs_files[@]}" -gt 0 ]; then
	say "docs/ changed (${#docs_files[@]} files); CI's docs job builds them with \`make docs\` (Sphinx, warnings as errors)"
	if command -v sphinx-build >/dev/null 2>&1; then
		run "make docs" make docs
	else
		say "sphinx-build is not on PATH, so the docs build was NOT checked here (pip install -r docs/requirements.txt)"
	fi
fi

if [ "${#tf_files[@]}" -gt 0 ]; then
	say "terraform changed (${#tf_files[@]} files); CI runs make tf-fmt-check, tf-validate, tf-test, tf-classify, lambda-test and hybrid-root-check (terraform), tf-lint and tf-scan (terraform-lint), and the 1.9 consumer floor"
	existing=()
	for f in "${tf_files[@]}"; do
		case "$f" in
		*.tf | *.tfvars) [ -f "$f" ] && existing+=("$f") ;;
		esac
	done
	if [ "${#existing[@]}" -eq 0 ]; then
		:
	elif command -v terraform >/dev/null 2>&1; then
		run "terraform fmt" terraform fmt -check "${existing[@]}"
	else
		say "terraform is not on PATH, so terraform fmt was NOT checked here"
	fi
fi

say "not run here, run by CI on every push: the race suite with coverage, the linux lint pass, billetlint, the cross builds, PostgreSQL and the rehearsals"

if [ "${#failed[@]}" -gt 0 ]; then
	say "FAILED: ${failed[*]}"
	exit 1
fi
say "every check that ran passed"
