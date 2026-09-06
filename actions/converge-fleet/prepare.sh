#!/usr/bin/env bash
# The refusals that cost nothing, before anything is installed or enrolled.
set -euo pipefail

case "${BILLET_MODE:-}" in
  check|converge) ;;
  *)
    echo "::error::mode must be check or converge, got '${BILLET_MODE:-}'"
    exit 1
    ;;
esac

case "${BILLET_REACH:-none}" in
  none|cloudflare-warp) ;;
  *)
    echo "::error::reach must be none or cloudflare-warp, got '${BILLET_REACH}'"
    exit 1
    ;;
esac

# A CONVERGE FROM A RUNNER BILLET MANAGES DESTROYS ITSELF: the drain takes the
# job running the playbook with it, and GitHub does not requeue it. billet names
# its runners billet-<lease id>, which is what makes this detectable. The host
# role refuses the same shape again before its transaction; refusing here costs
# no WARP enrolment and no package install.
case "${RUNNER_NAME:-}" in
  billet-?*)
    echo "::error::This job is running on ${RUNNER_NAME}, a runner billet itself manages. A converge restarts billet's services and drains the node, which destroys the jobs on that host, including this one. Run the converge on a GitHub-hosted runner or on a runner billet does not manage."
    exit 1
    ;;
esac

echo "billet_ref=${BILLET_ACTION_REF:-}" >>"$GITHUB_OUTPUT"
echo "converging with billet ${BILLET_ACTION_REF:-(unknown ref)} in ${BILLET_MODE} mode"
