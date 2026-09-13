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
# The parent belongs to root: the service account may write its server child,
# but must not rename the root node, kernel or upgrade directories beside it.
# StateDirectory in the units creates each private child under its own identity.
chown root:root "${STATE_DIR}"
chmod 0755 "${STATE_DIR}"

# THE HOST IS PREPARED FOR THE AUTHORITY EXCLUSION HERE, AFTER ITS PARENT
# EXISTS AND BEFORE ANY UNIT IS TOUCHED. `billet local prepare` records the
# service account, creates the server's identity directory when it is absent
# (so no metadata ever exists beside a missing directory), and provisions or
# repairs both authority locks by descriptor. It authorises nothing: its answer
# carries the published authority STATUS, and a closed one (a retired
# controller) is what the unit decisions below refuse on. Run on install AND
# on upgrade, because an installation prepared by an older package has neither
# the record nor the global lock, and its first unprivileged restart would
# otherwise meet a lock it cannot open. A binary that lacks the command is an
# older billet being replaced; it says so and this script goes on.
PREPARE_STATUS=absent
if [ -x /usr/bin/billet ]; then
    if prepared=$(/usr/bin/billet local prepare --json 2>&1); then
        case "${prepared}" in
            *'"closed":true'*) PREPARE_STATUS=closed ;;
            *) PREPARE_STATUS=open ;;
        esac
    else
        echo "billet: this host could not be prepared for the authority exclusion:" >&2
        echo "        ${prepared}" >&2
        echo "        Run \`billet local prepare\` as root once the cause is fixed." >&2
    fi
fi

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
# avoid. The template lives under /usr/share/billet and is copied here once.
# UNDER THE LIFECYCLE LOCK, WITH THE STATUS RE-READ THERE. Seeding a config and
# enabling timers are decisions about the host's state, and a retirement running
# on this host (or finishing between the prepare above and here) changes their
# answer: an ABSENT config is a completed server-only retirement's postcondition,
# not a gap to fill, and a retired controller's timers stay disabled. So every
# such action runs inside the same flock `billet local up` and `local down`
# hold, waits at most sixty seconds for it, and asks the status again inside.
# On contention past the bound nothing is touched: a package install must not
# fail a system upgrade for a lifecycle operation in flight, and the message
# names what was left and the retry that performs it (the package's own
# reconfiguration re-runs this script; `billet local up` needs a valid config
# and an intended start, so it is not the retry for an unseeded host).
LIFECYCLE_LOCK=/var/lock/billet-lifecycle.lock

seed_config() {
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
}

enable_timers() {
    [ -d /run/systemd/system ] || return 0

    # THE ONE EXCEPTION TO "THE PACKAGE ENABLES NOTHING". That rule keeps an
    # install from connecting a machine to GitHub before billet.yaml says
    # something true, and these two timers connect nothing: the upgrade timer
    # acts only on a rollout the ledger already records, the images timer only
    # on a node whose config names guest images, and both exit doing nothing
    # under `release: {automatic: false}`. What they buy is a deployment that
    # takes the update it decided on with nobody at a keyboard, which is the
    # promise `release.automatic` makes by default. `systemctl disable --now`
    # either one to opt this host out. NOT `|| exit`: a host whose systemd
    # refuses the enable (a container image being built, say) must still finish
    # installing.
    for timer in billet-upgrade.timer billet-images-refresh.timer; do
        if ! systemctl enable --now "${timer}" >/dev/null 2>&1; then
            echo "billet: ${timer} could not be enabled; automatic updates on this host" >&2
            echo "        need \`systemctl enable --now ${timer}\` once systemd is running." >&2
        fi
    done
}

# unit_decisions runs under the lifecycle lock. THE STATUS IS ASKED AGAIN HERE,
# INSIDE THE EXCLUSION, AND ONLY A CURRENT ANSWER AUTHORISES ANYTHING: the
# earlier answer was read before the lock and may be stale, and a preparation
# that fails here (an unreadable status, a lock it could not take) proves
# nothing about the host, so the decisions are deferred exactly as they are on
# contention, with the same retry named. An install must not fail a system
# upgrade for it, so this returns 0 either way.
unit_decisions() {
    if [ ! -x /usr/bin/billet ]; then
        echo "billet: /usr/bin/billet is not executable, so the unit decisions were not made;" >&2
        echo "        ${CONF} was not seeded and no timer was enabled. Run \`dpkg-reconfigure billet\`." >&2
        return 0
    fi

    if ! again=$(/usr/bin/billet local prepare --json 2>&1); then
        echo "billet: the authority status could not be re-read under the lifecycle lock, so" >&2
        echo "        this install left ${CONF} unseeded (if it was absent) and billet-upgrade.timer" >&2
        echo "        and billet-images-refresh.timer as they were:" >&2
        echo "        ${again}" >&2
        echo "        Automatic maintenance is DEFERRED on this host; once the cause is fixed," >&2
        echo "        \`dpkg-reconfigure billet\` (or a reinstall) re-runs these decisions." >&2
        return 0
    fi

    case "${again}" in
        *'"closed":true'*)
            echo "billet: this controller retired; its configuration is left absent and no unit" >&2
            echo "        is enabled or started. A retired controller stays retired." >&2
            return 0
            ;;
    esac

    seed_config
    enable_timers
}

# lock_dir_ready prepares the lock's directory, or says it could not.
#
# `mkdir -p` FAILS ON A DANGLING SYMLINK, which is what /var/lock is on a
# Fedora image with no tmpfs at /run/lock: the link exists, so mkdir answers
# EEXIST and, under `set -e`, an ordinary package install dies in its %post
# scriptlet (measured on fedora:42, 2026-09-12). What the link POINTS AT is
# what has to be created, and a path that is still not a directory afterwards
# is one this script declines to lock on rather than fail the install over.
lock_dir_ready() {
    dir=$(dirname "${LIFECYCLE_LOCK}")

    if [ -d "${dir}" ]; then
        return 0
    fi

    target=$(readlink -f "${dir}" 2>/dev/null || printf '%s' "${dir}")

    mkdir -p "${target}" 2>/dev/null || true

    [ -d "${dir}" ]
}

if command -v flock >/dev/null 2>&1 && lock_dir_ready; then
    # THE LOCK ON A DESCRIPTOR OF THIS SHELL, so the functions above run under it
    # in this process; `flock <file> <command>` would need a second script.
    exec 9>>"${LIFECYCLE_LOCK}"
    if flock -w 60 9; then
        unit_decisions
        flock -u 9
    else
        echo "billet: a billet lifecycle operation holds ${LIFECYCLE_LOCK}, so this install" >&2
        echo "        left ${CONF} unseeded (if it was absent) and billet-upgrade.timer and" >&2
        echo "        billet-images-refresh.timer as they were. Automatic maintenance is" >&2
        echo "        DEFERRED on this host until the deferred work runs: once the operation" >&2
        echo "        has finished, \`dpkg-reconfigure billet\` (or a reinstall) re-runs these" >&2
        echo "        decisions with the status re-read." >&2
    fi
else
    echo "billet: the unit decisions cannot be excluded against a lifecycle operation, so" >&2
    echo "        ${CONF} was not seeded and no timer was enabled: flock(1) is missing, or" >&2
    echo "        $(dirname "${LIFECYCLE_LOCK}") is not a directory this host can make." >&2
    echo "        Install util-linux, make that directory, and run \`dpkg-reconfigure billet\`." >&2
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

    # A DROP-IN CAN OUTLIVE THE ASSUMPTION IT WAS WRITTEN UNDER. These units
    # are Type=notify: the service is ready when billet's MAIN process sends
    # READY=1, so an override whose ExecStart wraps billet in something that
    # does not exec it — or replaces it with a binary predating notifyReady —
    # will hold the start until TimeoutStartSec and then fail it. That used to
    # work under Type=exec, and it fails at the next restart rather than here,
    # which is a long way from the change that caused it. Say so at install
    # time, where the operator is already looking.
    for unit in billet-server.service billet-node.service; do
        dropins=$(systemctl show --property=DropInPaths --value "${unit}" 2>/dev/null || true)
        if [ -n "${dropins}" ]; then
            echo "billet: ${unit} has drop-in overrides: ${dropins}" >&2
            echo "        This unit reports readiness through sd_notify from its main" >&2
            echo "        process. An override whose ExecStart wraps billet WITHOUT exec-ing" >&2
            echo "        it, or runs a billet older than v0.3.10, will fail to start; a" >&2
            echo "        wrapper that ends in \`exec billet ...\` is fine. Review it before" >&2
            echo "        restarting the service." >&2
        fi
    done
fi

exit 0
