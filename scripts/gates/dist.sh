#!/usr/bin/env bash
# dist.sh: build the release artifacts into dist/, exactly as a tag would, with
# the pinned goreleaser, and publish nothing. The package rehearsals install what
# this builds; run it too when touching .goreleaser.yaml. It runs the goreleaser
# on PATH; scripts/gates/tools.sh installs the pinned one.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

goreleaser release --snapshot --clean --skip=publish
