#!/bin/sh
set -e

# STOPPED, NOT DRAINED-AND-FORGOTTEN. systemctl stop sends SIGTERM, which begins
# billet's drain — so removing the package waits for the jobs already running,
# up to the unit's TimeoutStopSec. That is the intended behaviour and it can take
# a while; an operator in a hurry sends a second SIGTERM.
if [ -d /run/systemd/system ]; then
    for unit in billet-node billet-server; do
        if systemctl is-active --quiet "${unit}" 2>/dev/null; then
            systemctl stop "${unit}" || true
        fi
    done
fi

exit 0
