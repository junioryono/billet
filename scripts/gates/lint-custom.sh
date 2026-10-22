#!/usr/bin/env bash
# lint-custom.sh: billet's own analyzers, and the tests that prove they still
# detect.
#
# IT NEEDS NOTHING INSTALLED: tools/lint is a nested module built with the Go
# toolchain already here. It is nested so golang.org/x/tools/go/analysis never
# becomes a dependency of the billet binary, which ships with four direct ones.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

work=$(mktemp -d "${TMPDIR:-/tmp}/billetlint.XXXXXX")
trap 'rm -rf "$work"' EXIT

(cd tools/lint && go build -o "$work/billetlint" ./cmd/billetlint)

# The analyzers' OWN tests, which `go test ./...` from the root cannot reach across
# the module boundary. Without them an analyzer could silently stop detecting
# anything and still report zero violations, which reads exactly like a clean
# tree, and is the failure the whole directory is arranged against.
(cd tools/lint && go test ./...)

# ONCE PER PLATFORM, for the reason lint.sh runs two passes: an analyzer only sees
# the files it would COMPILE, so a single pass on darwin never examines a
# linux-only test and a single pass on linux never examines a darwin-only one.
# Diagnostics go to stderr, and the binary exits non-zero on any finding.
for target in darwin/arm64 linux/amd64; do
	printf '  billetlint %s\n' "$target"
	GOOS=${target%/*} GOARCH=${target#*/} "$work/billetlint" -parallelshared -rawsql ./...
done
