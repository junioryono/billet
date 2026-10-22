#!/usr/bin/env bash
# fuzz.sh: search past every fuzz target's seeds for FUZZTIME each (default 1m).
# The suite already runs the seeds on every pull request.
#
# THE NIGHTLY WORKFLOW'S LIST, which TestEveryFuzzTargetIsSearchedNightly holds
# equal to the Fuzz functions in the tree, so there is one list. ONE TARGET A RUN,
# because `go test -fuzz` takes exactly one. The list is read into a variable
# first, so a failed read stops here rather than iterating over nothing; an empty
# list is refused. A finding is written under the package's testdata/fuzz/, and
# committed beside its fix it becomes a seed.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

targets=$(sed -n 's/^ *- { package: \(\.\/[^,]*\), fuzz: \(Fuzz[A-Za-z0-9_]*\) }$/\1 \2/p' .github/workflows/fuzz.yml)
if [ -z "$targets" ]; then
	printf 'fuzz: .github/workflows/fuzz.yml lists no targets\n' >&2
	exit 1
fi

while read -r pkg fn; do
	printf '== %s %s\n' "$pkg" "$fn"
	CGO_ENABLED=0 ${nice_cmd[@]+"${nice_cmd[@]}"} go test "$pkg" -run '^$' -fuzz "^$fn\$" -fuzztime "${FUZZTIME:-1m}"
done <<<"$targets"
