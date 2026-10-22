#!/usr/bin/env bash
# bench.sh: the allocator's and the node wire's benchmarks, three runs each of
# BENCHTIME (default 2s); compare two runs with benchstat. A change to escrow,
# placement or renewal shows its numbers before and after.
#
# SQLite unless BILLET_TEST_LEDGER=postgres and BILLET_TEST_POSTGRES_DSN say
# otherwise, as the tests are. What they measure, and the decision made on them,
# is docs/reference/records/ledger-write-budget.md.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

${nice_cmd[@]+"${nice_cmd[@]}"} go test -run '^$' -bench . -benchtime "${BENCHTIME:-2s}" -count 3 \
	./internal/alloc/ ./internal/nodeplane/
