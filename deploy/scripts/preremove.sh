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

# A REAL REMOVAL. `systemctl stop` sends SIGTERM. The node's is a drain. The
# server's is a drain only once the deployment is sealed: while it still admits
# work the server hands over to the next control plane (#365) and returns at once.
# Shutting the whole deployment down is `billet local down` first, which seals; a
# seal is deployment-wide, so this hook does not take one, and retiring one
# controller of a pair follows the controller-retirement procedure instead.
#
# The node's drain waits for the jobs already running — up to the unit's TimeoutStopSec. That
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

    # A REMOVAL IS A NODE LEAVING, so its stop must be a drain even where
    # node.stop says handoff (#374): nothing will start again to adopt the guests
    # a handoff leaves behind. A node process that stops while
    # /var/run/billet-node-drain exists drains; it is written whatever the config
    # says, left in place (a reinstall's node drains on its stops until the next
    # reboot or `billet local up`), and a failure to write it refuses the removal.
    if ! printf 'drain\n' >/var/run/billet-node-drain; then
        echo "billet: could not ask billet-node to drain. Refusing to remove the package" >&2
        echo "        while it may hand its guests to a node that is not coming back." >&2
        exit 1
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

    # THE UPLINK SHAPER, where the host role installed it, stops while the binary
    # its ExecStopPost clears with is still here: afterwards nothing could remove
    # the CAKE it left on the uplink. Its record is forgotten only once that
    # cleanup succeeded, so a record still present is a cleanup that did not, and
    # the removal is refused rather than leave the shaping behind.
    if [ -e /etc/systemd/system/billet-uplink.service ]; then
        systemctl disable --now billet-uplink.service >/dev/null 2>&1 || true
        if [ -e /run/billet-uplink/interface ]; then
            echo "billet: billet-uplink did not clear its shaping (/run/billet-uplink/interface" >&2
            echo "        remains). Refusing to remove the package, which would take the binary" >&2
            echo "        that clears it; run 'billet uplink clear' and remove again." >&2
            exit 1
        fi
    fi
fi

exit 0
