#!/usr/bin/env bash
# lint.sh: golangci-lint at the pinned version, for this platform AND for
# linux/amd64. With --fix it fixes what it can, for this platform only.
#
# AND AGAIN FOR LINUX, because a linter only analyses the files it would compile.
# billet is developed on darwin and RUNS on linux, so every linux-only file, and
# every branch of a platform-dependent type, is unexamined by the first pass. Not
# theoretical: `uint64(stat.Rdev)` in the firecracker backend is a redundant
# conversion on linux and a required one on darwin, so it passed one pass and
# failed CI; three defects on one branch had exactly this shape. ON CI'S
# ARCHITECTURE: a stat field's width differs between linux/amd64 (Nlink is
# uint64) and linux/arm64 (uint32), so a linux pass at a Mac's arm64 missed a
# conversion CI refused as unnecessary (2026-09-09); a conversion that must exist
# on one architecture and not another is spelled through a generic widening.
#
# --allow-serial-runners WAITS for another golangci-lint on this machine rather
# than failing: its lock is machine-wide, and a lint another project ran outside
# the gate lock failed this gate (2026-10-06).
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

# A CHECKOUT PATH WITH GLOB SYNTAX IN IT IS REFUSED, before any golangci-lint run.
# depguard's file globs start at ${config-path}, which golangci-lint substitutes
# unescaped (2.12.2), so glob syntax in the path changes which files a rule
# selects or exempts: under `billet[2]`, a rule that selects files under the root
# selects none, and one that exempts them exempts none. The path is read by the
# shell, never pasted into it.
case "$gates_root" in
*[][*?{}]*)
	printf 'lint: %s contains glob syntax ([ ] * ? { }), which depguard would read as a pattern: a rule could check no file, or exempt none. Lint from a checkout whose path has none.\n' "$gates_root" >&2
	exit 1
	;;
esac

# ONE SPELLING OF THE CHECKOUT for the guard and the linter. A shell's `pwd` and
# Go's os.Getwd both prefer an inherited $PWD, which can name the checkout
# through a symlink, while gates_root is the physical path; exported here, the
# guard checks the path golangci-lint will substitute.
export PWD=$gates_root

if [ "${1-}" = --fix ]; then
	exec golangci-lint run --fix --timeout=15m --allow-serial-runners
fi

${nice_cmd[@]+"${nice_cmd[@]}"} golangci-lint run --timeout=15m --allow-serial-runners
GOOS=linux GOARCH=amd64 ${nice_cmd[@]+"${nice_cmd[@]}"} golangci-lint run --timeout=15m --allow-serial-runners
