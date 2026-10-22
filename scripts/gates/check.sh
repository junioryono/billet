#!/usr/bin/env bash
# check.sh: the full gate, for a person who wants it on their own machine, one
# run at a time per machine. CI runs every one of its steps, and more, on every
# push; a Claude session verifies narrowly with scripts/gates/verify.sh and reads
# CI instead, and the repository's hook refuses this to a session unless Claude
# Code was started with CLAUDE_FULL_CHECKS=1.
#
# ONE GATE AT A TIME PER MACHINE, across projects. Two sessions, two worktrees
# or two repositories running their gates together is how a machine starved
# (see env.sh), so the steps run under the lock every repository's gate shares
# and a run waits for whichever gate holds it (scripts/with-check-lock.sh).
# CHECK_LOCK names another lock file; empty means the shared one. It is a
# courtesy, not a gate: without a lock tool, or a lock that cannot be taken, the
# run goes ahead and says so.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)

exec "$here/../with-check-lock.sh" "${CHECK_LOCK-}" "$here/check-steps.sh" "$@"
