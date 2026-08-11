#!/bin/sh
set -e

# THE STATE DIRECTORY AND THE CONFIG ARE LEFT BEHIND, deliberately.
#
# /var/lib/billet holds the deployment identity and the mTLS CA that every node's
# certificate was issued against. Deleting it on `apt remove` would mean an
# upgrade-by-reinstall silently invalidates every node in the fleet. The `billet`
# user is left for the same reason: the files it owns are still there.
#
# To remove them, do it deliberately:
#
#   rm -rf /var/lib/billet /etc/billet
#   userdel billet

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

exit 0
