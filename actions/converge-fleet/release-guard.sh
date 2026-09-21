#!/usr/bin/env bash
# Release the converge guard this run holds, on every host it held.
#
# THE ROLE HOLDS AND NEVER RELEASES, on purpose: the hold is what stops a
# converge racing a rollout's `billet host-upgrade` for the same binaries, and
# the role cannot know when the converge driving it has ended. The action can,
# and this is where it says so. A guard nobody releases stands until a person
# removes it, and every later converge and every rollout dispatch on that host
# refuses while it does.
#
# ON EVERY PATH, INCLUDING FAILURE AND CANCELLATION, which is the case that
# matters: a converge that died half way is exactly the one holding guards. The
# step is `if: always()` and this script is idempotent — a guard already gone is
# not an error, and one held by another name is left alone and reported.
#
# BEFORE THE CREDENTIALS ARE SHREDDED. This needs the SSH key and known_hosts
# that converge.sh wrote, so the step runs ahead of "Leave nothing behind".
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
checkout=$(cd "$here/../.." && pwd)

readonly billet_release_runner_temp=${RUNNER_TEMP:?}
readonly billet_release_marker="$billet_release_runner_temp/billet-guard-held"

# NOTHING WAS HELD, SO NOTHING IS RELEASED. A run refused for its mode, one that
# could not enrol with WARP, one whose install failed: none of them reached a
# host, and running a playbook here would fail the job over a fleet this run
# never touched.
if [[ ! -f $billet_release_marker ]]; then
  echo "this run holds no converge guard; nothing to release"
  exit 0
fi

# THE MARKER'S NAME WINS over the environment: it is what the converge actually
# ran under.
holder=$(head -n1 "$billet_release_marker" 2>/dev/null || true)
holder=${holder:-${BILLET_CONVERGE_GUARD_HOLDER:-}}
if [[ -z $holder ]]; then
  echo "::error::this run held hosts under no name, so its guards cannot be released by name. Run 'billet converge-guard status' on each host and release it there."
  exit 1
fi

export BILLET_CONVERGE_GUARD_HOLDER="$holder"
export ANSIBLE_COLLECTIONS_PATH="$checkout:$billet_release_runner_temp/billet-collections"

# THE KEY THE CONVERGE CONNECTED WITH. converge.sh writes it here and exports its
# path, but an export ends with the step that made it and this is a step of its
# own. Without it ansible offers no key, every host refuses, and the refusal reads
# as a host that does not trust the converge — measured on a real fleet, where
# the host's authorized_keys held the CI key all along. known_hosts needs nothing:
# converge.sh wrote the pins into the runner user's own file, which persists.
if [[ -f $billet_release_runner_temp/billet-ssh-key ]]; then
  export ANSIBLE_PRIVATE_KEY_FILE="$billet_release_runner_temp/billet-ssh-key"
fi
export ANSIBLE_HOST_KEY_CHECKING=True
export ANSIBLE_STDOUT_CALLBACK=default
export ANSIBLE_RESULT_FORMAT=yaml
export ANSIBLE_FORCE_COLOR=0
export ANSIBLE_NOCOLOR=1

args=(-i "${BILLET_INVENTORY:?}")
if [[ -n ${BILLET_LIMIT:-} ]]; then
  # THE SAME HOSTS THIS RUN CONVERGED. Without the limit the release walks every
  # host in the inventory, and each one this run never held answers that the
  # guard is not its to release — a failure that says nothing.
  args+=(--limit "$BILLET_LIMIT")
fi

echo "releasing the converge guard held as $holder"

if ! ansible-playbook "${args[@]}" junioryono.billet.release_guard; then
  echo "::error::the converge guard held as $holder could not be released on every host. Every later converge and every rollout dispatch refuses on a host that is still held: run 'billet converge-guard status' there, and 'billet converge-guard release --holder $holder' to clear it."
  exit 1
fi

rm -f "$billet_release_marker"
