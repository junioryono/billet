#!/usr/bin/env bash
# check-steps.sh: the full gate's steps, in order, without the machine-wide lock
# check.sh takes. It stops at the first step that fails.
#
# A GREEN RUN IS NECESSARY, NOT SUFFICIENT. CI also proves `go mod tidy` leaves no
# diff, uploads coverage, runs govulncheck, the cross builds, `sqlc diff`, the
# suite against a real PostgreSQL, the docs build, the package and restore
# rehearsals, the Ansible role tests and the terraform gates
# (scripts/gates/terraform.sh), and the required check on main fans all of them
# in.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

step() {
	printf '== %s\n' "$1"
	shift
	"$@"
}

# FIRST, because this one cannot be a false alarm: a `.bak` beside a tracked file
# means an interrupted mutation run left the ORIGINAL holding a mutant. It
# compiles and mostly passes, so every other step is happy to say so.
step no-mutants python3 scripts/check-no-mutants.py
step build "$here/build.sh"
step vet go vet ./...
step fmt-check "$here/fmt-check.sh"
step lint "$here/lint.sh"
step lint-custom "$here/lint-custom.sh"
step test "$here/test.sh"
step lambda-test "$here/lambda-test.sh"
# HERE RATHER THAN WITH THE TERRAFORM GATES, which need terraform, tflint and
# trivy installed. This one is git and shell, so it can fail on the commit that
# breaks it instead of on the release. The ref it expects is the tree's own
# (scripts/release-ref.sh): main here, the branch's tag on a release branch.
step module-sources scripts/check-module-sources.sh
