#!/bin/sh
# The runner's job-completed hook in a guest whose agent started the guest monitor:
# it asks the monitor for its final batch, as the runner account.
#
# THE RUNNER RUNS IT AFTER THE JOB'S LAST STEP AND BEFORE GITHUB RECORDS THE JOB
# COMPLETE, so the final batch reaches the node before the destroy is sent. A hook
# that failed would fail the job and anything it printed would be in the job's
# log, so it always exits 0 and prints nothing, the shell's own word on a killed
# command included. `billet guest flush` waits at most 2 s for the monitor's answer
# and exits 0 either way; timeout holds the whole hook to 4 s whatever it does.
exec </dev/null >/dev/null 2>&1
billet=/opt/billet/bin/billet
if [ -x "$billet" ]; then
	timeout -k 1 3 "$billet" guest flush || true
fi
exit 0
