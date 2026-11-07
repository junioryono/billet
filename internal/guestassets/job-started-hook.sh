#!/bin/sh
# The runner's job-started hook on a tier that may be attached to: it says in the job's log how to
# open a terminal in this job's VM, then runs whatever hook this one stands in front of.
#
# THE RUNNER RUNS IT BEFORE THE JOB'S FIRST STEP, and what it prints is in the job's log. A runner
# is named after its lease (billet-<lease>). Nothing here may fail the job; the hook it stands in
# front of may, as it could before, so that one's status is the answer.
lease=${RUNNER_NAME#billet-}
if [ -n "$lease" ]; then
	printf '%s\n' "billet: to open a terminal in this job's VM, run on the control plane: billet jobs attach $lease"
	if [ -n "${BILLET_ATTACH_HOLD:-}" ]; then
		printf '%s\n' "billet: if this job fails, its VM is kept for up to $BILLET_ATTACH_HOLD, or until a session's shell has exited and the last session has closed"
	fi
fi
if [ -n "${BILLET_JOB_STARTED_NEXT:-}" ]; then
	exec "$BILLET_JOB_STARTED_NEXT"
fi
exit 0
