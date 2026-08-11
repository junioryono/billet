#!/bin/sh
set -e

# DAEMON-RELOAD AND NOTHING ELSE.
#
# The package does not enable or start billet. Installing it must not connect a
# machine to GitHub and begin accepting other people's jobs — that is the
# administrator's decision, and it cannot be made before /etc/billet/billet.yaml
# says something true. An upgrade must not start a service the operator had
# deliberately stopped, either.
#
#   systemctl enable --now billet-server
#   systemctl enable --now billet-node

if ! getent group billet >/dev/null 2>&1; then
    groupadd --system billet
fi

if ! getent passwd billet >/dev/null 2>&1; then
    # The nologin shell is not in the same place everywhere: Debian has
    # /usr/sbin/nologin, and a minimal Fedora has neither that nor a symlink to
    # it, where useradd warns and creates the account with a shell that does not
    # exist. /bin/false is the portable fallback.
    shell=/bin/false
    for candidate in /usr/sbin/nologin /sbin/nologin; do
        if [ -x "${candidate}" ]; then
            shell="${candidate}"
            break
        fi
    done

    useradd --system --gid billet --home-dir /var/lib/billet \
        --shell "${shell}" \
        --comment "billet self-hosted runner platform" billet
fi

# 0700: this holds the capacity ledger, the deployment identity and the mTLS CA's
# private key. billet sets this itself on every start; doing it here means the
# directory is never briefly readable between install and first run.
mkdir -p /var/lib/billet
chown billet:billet /var/lib/billet
chmod 0700 /var/lib/billet

# /etc/billet is written by an operator and READ BY THE SERVICE USER, which is
# the half the modes have to get right in both directions: root owns it so an
# unprivileged process cannot edit the config or the App key, and the billet
# group can read it so the service can start at all.
#
# THE GROUP ON THE FILES MATTERS AS MUCH AS THE ONE ON THE DIRECTORY. Left at
# root:root with mode 0640, the directory is traversable by the billet group and
# the file inside it is readable by nobody but root — so billet.service starts,
# fails to open its own config, and the diagnostic is a permission error on a
# path the operator can plainly see and read themselves.
chown root:billet /etc/billet
chmod 0750 /etc/billet

for f in /etc/billet/billet.yaml /etc/billet/app-private-key.pem; do
    if [ -e "${f}" ]; then
        chown root:billet "${f}"
        chmod 0640 "${f}"
    fi
done

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

exit 0
