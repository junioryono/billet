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

# A REAL REMOVAL, AND THIS HOST'S STOP IS MARKED FINAL FIRST. A server stopped
# while the deployment admits work hands over to the next control plane, leaving
# its jobs and its message sessions for a successor (#365). A removal has no
# successor on this host, so the marker below makes its stop the drain it always
# was. It is a marker on this host and not a seal of the deployment, which other
# controllers may still be serving. Written unconditionally, before anything is
# stopped, so no service state read in between can skip it; a marker that cannot
# be written refuses the removal rather than turning it into a handoff to nobody.
# cmd/billet/stophandoff.go names the same file.
#
# `systemctl stop` sends SIGTERM, which begins billet's drain, so
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
    # THE TIMERS GO FIRST, so nothing schedules an upgrade or an image refresh
    # into the middle of the removal. Disabled as well as stopped: postinstall
    # enabled them, and a removal that left the enablement behind would have the
    # next install's daemon-reload find a timer pointing at a unit that is gone.
    for timer in billet-upgrade.timer billet-images-refresh.timer; do
        systemctl disable --now "${timer}" >/dev/null 2>&1 || true
    done

    server_state=${BILLET_SERVER_STATE_DIR:-/var/lib/billet/server}
    if [ -d "${server_state}" ]; then
        if ! : >"${server_state}/drain-on-stop"; then
            echo "billet: could not mark this host's stop as final in ${server_state}." >&2
            echo "        Refusing to remove the package: its control plane would hand" >&2
            echo "        its jobs to a successor that is not coming." >&2
            exit 1
        fi
    fi

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
