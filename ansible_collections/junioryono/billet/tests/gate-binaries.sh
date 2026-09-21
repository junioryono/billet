#!/usr/bin/env bash
# The converge guard gate's backing binaries: billet from this checkout, stamped
# with the release versions the gate's cases name and built with the crash seam.
#
#   gate-binaries.sh versions      print the versions, one per line
#   gate-binaries.sh build DIR     build billet-<version> for each into DIR
#   gate-binaries.sh verify DIR    refuse DIR unless each billet-<version> is one
#                                  build would have produced from this checkout
#
# ONE DEFINITION for converge-guard-check.sh, which builds them itself on a
# laptop, and for CI, which builds them once per run and hands every group the
# same files. A binary handed in is VERIFIED rather than trusted: it must report
# the version its name says and carry the billetgatecrash tag, so an artifact
# built wrong fails the gate instead of exercising the wrong build. Which commit
# it came from is the artifact's scope, one workflow run, not a check here: Go's
# vcs stamp does not follow a git worktree (it walks up to the main checkout's
# .git directory and stamps that commit, measured 2026-09-21 with go1.26.6).
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo_root=$(cd "$here/../../../.." && pwd)

versions=(v0.10.0 v0.10.1 v0.11.0 v0.9.0)

die() {
  echo "gate-binaries: $*" >&2
  exit 1
}

build() {
  local dir=$1 v
  mkdir -p "$dir"
  for v in "${versions[@]}"; do
    (cd "$repo_root" && go build -tags billetgatecrash \
      -ldflags "-X github.com/junioryono/billet/internal/version.version=$v" \
      -o "$dir/billet-$v" ./cmd/billet)
  done
}

verify() {
  local dir=$1 v bin first info status
  for v in "${versions[@]}"; do
    bin="$dir/billet-$v"
    [ -f "$bin" ] && [ -x "$bin" ] || die "$bin is missing or not executable"

    status=0
    first=$("$bin" version) || status=$?
    [ "$status" -eq 0 ] || die "$bin version exited $status"
    first=${first%%$'\n'*}
    # A build without VCS metadata prints no revision after the version.
    case "$first" in
      "billet $v" | "billet $v "*) ;;
      *) die "$bin reports '$first', not $v" ;;
    esac

    info=$(go version -m "$bin") || die "go version -m could not read $bin"
    grep -qE '^[[:space:]]+build[[:space:]]+-tags=([^[:space:]]*,)?billetgatecrash(,|$|[[:space:]])' <<<"$info" ||
      die "$bin was not built with -tags billetgatecrash"
  done
}

# provide puts the gate's binaries in DIR: the verified contents of
# BILLET_GATE_PREBUILT when it is set, and never a build then; a fresh build
# otherwise.
provide() {
  local dir=$1 v
  if [ -z "${BILLET_GATE_PREBUILT:-}" ]; then
    echo "building the backing binaries ..."
    build "$dir"
    return
  fi

  echo "verifying the prebuilt backing binaries in $BILLET_GATE_PREBUILT ..."
  verify "$BILLET_GATE_PREBUILT"
  mkdir -p "$dir"
  for v in "${versions[@]}"; do
    cp "$BILLET_GATE_PREBUILT/billet-$v" "$dir/billet-$v"
    chmod 0755 "$dir/billet-$v"
  done
}

case "${1:-}" in
  versions) printf '%s\n' "${versions[@]}" ;;
  build) build "${2:?build DIR}" ;;
  verify) verify "${2:?verify DIR}" ;;
  provide) provide "${2:?provide DIR}" ;;
  *) die "usage: gate-binaries.sh versions | build DIR | verify DIR | provide DIR" ;;
esac
