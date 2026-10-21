#!/usr/bin/env bash
# verify.sh: THE CHECK WHILE WORKING. It checks what this checkout changed against
# its merge base with origin/main (VERIFY_BASE overrides): gofmt, the build, vet,
# tests without -race and golangci-lint for this platform, on the changed
# packages only, and it says what it ran and why (scripts/verify-changed.sh). CI
# runs the full gate on every push, so a session verifies narrowly here and reads
# CI; scripts/gates/check.sh stays for a person who wants the whole gate on their
# own machine.
#
# AND IT TAKES THE SAME MACHINE-WIDE LOCK AS check.sh. A narrow check is still a
# build, a vet, two packages' tests and a lint pass, and a dozen sessions running
# theirs at once is the starved machine the lock exists for: on 2026-10-20 the
# development Mac sat at a load average of 392 with billet's verifies running
# side by side. The lock is the one every repository's gate shares, so a verify
# here also takes turns with the other repository's gates and verifies. It runs
# under NICE (env.sh) outside CI.
set -euo pipefail

here=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
. "$here/env.sh"

exec "$gates_root/scripts/with-check-lock.sh" "${CHECK_LOCK-}" \
	${nice_cmd[@]+"${nice_cmd[@]}"} "$gates_root/scripts/verify-changed.sh" "$@"
