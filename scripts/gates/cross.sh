#!/usr/bin/env bash
# cross.sh: build every target a node can run on. A node is deployed by copying
# one file, so a build-tag mistake on a platform nobody develops on is invisible
# until it reaches that machine. CI's lint job runs this; run it before anything
# touching a build tag.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

for target in linux/amd64 linux/arm64 darwin/arm64; do
	printf '  %s\n' "$target"
	GOOS=${target%/*} GOARCH=${target#*/} CGO_ENABLED=0 go build -o /dev/null ./cmd/billet
done
