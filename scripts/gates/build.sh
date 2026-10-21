#!/usr/bin/env bash
# build.sh: build ./bin/billet, the way a release does: CGO off, so a node is
# deployed by copying one static file.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

mkdir -p bin
CGO_ENABLED=0 go build -o bin/billet ./cmd/billet
