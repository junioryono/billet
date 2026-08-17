#!/bin/sh
set -e

# DAEMON-RELOAD AND NOTHING ELSE IS STARTED.
#
# The package does not enable or start billet. Installing it must not connect a
# machine to GitHub and begin accepting other people's jobs — that is the
# administrator's decision, and it cannot be made before
# /etc/billet/billet.yaml says something true. An upgrade must not start a
# service the operator had deliberately stopped, either.
#
#   systemctl enable --now billet-server
#   systemctl enable --now billet-node

STATE_DIR=/var/lib/billet
CONF_DIR=/etc/billet
CONF="${CONF_DIR}/billet.yaml"
# NOT under /usr/share/doc: Debian's slim images set
# `path-exclude=/usr/share/doc/*`, which silently drops anything kept there.
TEMPLATE=/usr/share/billet/billet.yaml
KEY="${CONF_DIR}/app-private-key.pem"
JAILER_DIR=/srv/jailer

# THE NODE UNIT CANNOT LOAD MODULES. ProtectKernelModules is intentional service
# hardening, so the host prepares the RBD client before the unit can start. The
# package also installs a modules-load.d entry for reboots. A kernel without RBD
# is allowed here because an EC2 or Docker-only node does not use Ceph; `billet
# check` remains the place that rejects an enabled Ceph configuration whose host
# cannot satisfy it.
if command -v modprobe >/dev/null 2>&1 && modprobe -n rbd >/dev/null 2>&1; then
    modprobe rbd
fi

if ! getent group billet >/dev/null 2>&1; then
    groupadd --system billet
fi

if getent passwd billet >/dev/null 2>&1; then
    # AN EXISTING ACCOUNT IS NOT AUTOMATICALLY OURS. What follows hands this
    # identity the deployment's private CA key and group-read on the GitHub App
    # key, so a login account or a directory-service account that happens to be
    # called "billet" must not silently inherit an organization credential.
    home="$(getent passwd billet | cut -d: -f6)"
    if [ "${home}" != "${STATE_DIR}" ]; then
        echo "billet: a user named 'billet' already exists with home ${home}," >&2
        echo "        not ${STATE_DIR}. Refusing to give it this deployment's" >&2
        echo "        credentials. Remove or rename that account, or install" >&2
        echo "        billet under a different user by editing the units." >&2
        exit 1
    fi
else
    # The nologin shell is not in the same place everywhere: Debian has
    # /usr/sbin/nologin, and a minimal Fedora has neither that nor a symlink,
    # where useradd warns and creates an account with a shell that does not
    # exist. /bin/false is the portable fallback.
    shell=/bin/false
    for candidate in /usr/sbin/nologin /sbin/nologin; do
        if [ -x "${candidate}" ]; then
            shell="${candidate}"
            break
        fi
    done

    useradd --system --gid billet --home-dir "${STATE_DIR}" \
        --shell "${shell}" \
        --comment "billet self-hosted runner platform" billet
fi

# NOT SHIPPED IN THE PACKAGE, CREATED HERE. A directory the package manager owns
# is a directory it deletes: shipping this one meant `dpkg -r billet` removed the
# deployment identity and the mTLS CA's private key, invalidating every node
# certificate in the fleet.
mkdir -p "${STATE_DIR}"
chown billet:billet "${STATE_DIR}"
chmod 0700 "${STATE_DIR}"

# CREATED RATHER THAN PACKAGED, so removing the package cannot erase a jail that
# still holds guest state. The node unit makes this path writable through its
# otherwise read-only filesystem view; a fresh Firecracker install therefore
# needs it before the service starts.
mkdir -p "${JAILER_DIR}"
chown root:root "${JAILER_DIR}"
chmod 0755 "${JAILER_DIR}"

mkdir -p "${CONF_DIR}"
chown root:billet "${CONF_DIR}"
chmod 0750 "${CONF_DIR}"

# SEEDED, NOT OWNED, for the same reason as the state directory. A config the
# package manager owns is one `apt purge` removes and one `rpm -e` can rename to
# .rpmsave — taking the App ids, the tier catalog and the capacity ceilings with
# it, while the deployment identity and the App key survive separately. That
# leaves a half-recoverable machine, which is the state all of this exists to
# avoid. The template lives under /usr/share/doc and is copied here once.
if [ ! -e "${CONF}" ]; then
    if [ -e "${TEMPLATE}" ]; then
        cp "${TEMPLATE}" "${CONF}"
    else
        # Loud, because the alternative is a machine with no config and nothing
        # to say why.
        echo "billet: ${TEMPLATE} is missing, so ${CONF} was not created." >&2
        echo "        Copy billet.example.yaml there before starting billet." >&2
    fi
fi

if [ -e "${CONF}" ]; then
    # root owns it so an unprivileged process cannot edit what billet trusts;
    # the billet group can read it or the service cannot start at all.
    chown root:billet "${CONF}"
    chmod 0640 "${CONF}"
fi

# THE APP KEY IS OWNED BY THE SERVICE USER AT 0600, and it is the one file here
# that cannot be root-owned-and-group-readable.
#
# billet refuses any App key with group or other bits set (githubapp.go, the
# perm&0o077 check) — a private key readable by a group is one that leaks through
# a group. So 0640 root:billet, which is right for the config, makes the server
# refuse to start; and 0600 root:root is unreadable by the service. The only
# arrangement that satisfies both is the ordinary Unix one: the process that
# needs the secret owns the secret.
if [ -e "${KEY}" ]; then
    chown billet:billet "${KEY}"
    chmod 0600 "${KEY}"
fi

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

exit 0
