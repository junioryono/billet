#!/usr/bin/env bash
# fmt-check.sh: refuse a tree with any Go file gofmt -s would change. `gofmt -s -w
# .` fixes it.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

# THE LIST IS READ BEFORE IT IS JUDGED, so gofmt failing to read a file fails the
# gate rather than reading as a tree with nothing to change.
unformatted=$(gofmt -s -l .)
if [ -n "$unformatted" ]; then
	printf 'These files need gofmt -s:\n%s\n' "$unformatted"
	exit 1
fi
