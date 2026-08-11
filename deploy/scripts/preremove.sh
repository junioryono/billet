#!/bin/sh
set -e

# AN UPGRADE IS NOT A REMOVAL, and conflating them takes billet down for good.
#
# dpkg runs prerm during an upgrade; rpm runs the OLD package's preun during one.
# Stopping the service here without checking would mean every routine upgrade
# blocks the package manager for as long as the drain takes — up to six hours —
# then leaves billet stopped, because postinstall deliberately starts nothing.
# The operator would have upgraded a running control plane into a stopped one.
#
# dpkg passes "upgrade"; rpm passes the number of instances that will remain,
# which is 1 during an upgrade and 0 on a real removal.
case "${1:-}" in
    upgrade | failed-upgrade | 1)
        exit 0
        ;;
esac

# A REAL REMOVAL. `systemctl stop` sends SIGTERM, which begins billet's drain, so
# this waits for the jobs already running — up to the unit's TimeoutStopSec. That
# is the intended behaviour and it can take a while; an operator in a hurry sends
# a second SIGTERM with
#
#   systemctl kill --kill-whom=main --signal=SIGTERM billet-server
#
# A FAILURE TO STOP IS FATAL. Swallowing it would remove the binary and the unit
# while an unmanaged billet process kept running — holding leases, managing
# containers, and answering to nothing.
if [ -d /run/systemd/system ]; then
    for unit in billet-node billet-server; do
        if systemctl is-active --quiet "${unit}" 2>/dev/null; then
            if ! systemctl stop "${unit}"; then
                echo "billet: could not stop ${unit}. Refusing to remove the package" >&2
                echo "        while it is still running: it holds leases and manages" >&2
                echo "        containers that nothing else is tracking." >&2
                exit 1
            fi
        fi
    done
fi

exit 0
