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

# THE GUARD'S HOLDER IS THIS RUN, AND THIS ACTION OWNS ITS WHOLE LIFE.
#
# The role holds every host under a name before it changes anything and
# deliberately never releases: the name is what a takeover, a release and a
# recovery are addressed to, and the role cannot know when the converge driving
# it has ended. This action can, so it names the run here and releases under
# that name in a step that runs however the job ends.
#
# NO SLASH, because `converge-guard` refuses one in a holder (checkHolder) and
# GITHUB_REPOSITORY is owner/name. THE RUN AND THE ATTEMPT, because two runs
# that shared a name would each read the other's guard as their own and release
# it mid-converge. The repository is in it so a guard found standing says what
# left it.
#
# AN EXPORTED NAME WINS: a person converging from a laptop, or a workflow that
# sets its own, releases under a name they chose and this must not replace it.
if [[ -z ${BILLET_CONVERGE_GUARD_HOLDER:-} ]]; then
  billet_guard_repository=${GITHUB_REPOSITORY:-unknown-repository}
  billet_guard_holder="gha-${billet_guard_repository//\//-}-${GITHUB_RUN_ID:-0}-${GITHUB_RUN_ATTEMPT:-1}"
  # GITHUB_ENV is what carries it to the later steps; a run without one (a
  # script run by hand) still reports the name it would have used.
  if [[ -n ${GITHUB_ENV:-} ]]; then
    echo "BILLET_CONVERGE_GUARD_HOLDER=${billet_guard_holder}" >>"$GITHUB_ENV"
  fi
else
  billet_guard_holder=$BILLET_CONVERGE_GUARD_HOLDER
fi

echo "converge_guard_holder=${billet_guard_holder}" >>"$GITHUB_OUTPUT"
echo "billet_ref=${BILLET_ACTION_REF:-}" >>"$GITHUB_OUTPUT"
echo "converging with billet ${BILLET_ACTION_REF:-(unknown ref)} in ${BILLET_MODE} mode, holding every host as ${billet_guard_holder}"
