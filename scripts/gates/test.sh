#!/usr/bin/env bash
# test.sh: the race-enabled suite, instrumented exactly as CI's test job runs it.
# With --report it then prints the coverage total and opens the HTML report.
#
# COVERAGE INSTRUMENTATION IS PART OF THE GATE, not an extra. CI runs the suite
# with -covermode=atomic, and that is not the same test run: the counters change
# timing enough to reorder goroutines the plain -race build happens to schedule
# one way every time. This is not hypothetical. A launch still in progress was
# being handed to teardown (it carries no outcome, so the release failed with
# `invalid phase transition: "" is not terminal`), and the bug was invisible
# without coverage while being reliable under it. A local gate weaker than CI
# trains you to trust it and then be surprised.
#
# -timeout 35m IS PER PACKAGE, and matches CI: internal/alloc alone took 530s
# under -race and atomic coverage before migration 49 added fourteen statements
# to every test ledger's open, against go test's default of 600s, and cmd/billet
# crossed 20m on a loaded fleet runner on 2026-09-26.
#
# COVERPROFILE names the profile (coverage.out).
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

profile=${COVERPROFILE:-coverage.out}

${nice_cmd[@]+"${nice_cmd[@]}"} go test ${test_p[@]+"${test_p[@]}"} -race -count=1 -timeout 35m \
	-covermode=atomic -coverprofile="$profile" ./...

if [ "${1-}" = --report ]; then
	go tool cover -func="$profile" | tail -1
	go tool cover -html="$profile"
fi
