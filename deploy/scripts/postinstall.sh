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
# THE LOAD ITSELF IS OPTIONAL TOO. `modprobe -n` says the module resolves, not
# that this kernel will load it: a locked-down or container host can refuse the
# real load, and under `set -e` that would end an install for a host that may
# not use Ceph at all.
if command -v modprobe >/dev/null 2>&1 && modprobe -n rbd >/dev/null 2>&1; then
    if ! modprobe rbd; then
        echo "billet: the rbd module resolves but this kernel would not load it; a Ceph" >&2
        echo "        configuration will be refused by \`billet check\` until it can." >&2
    fi
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

# EVERY MUTATION HERE CHECKS ITSELF, because nothing in this function is
# covered by `set -e`: it runs inside a subshell that is an `if` condition, and
# POSIX suspends errexit for a condition AND everything it calls. A copy onto a
# full filesystem would otherwise leave a partial config and go on to enable
# timers over it, with the install reporting success.
seed_config() {
    if [ ! -e "${CONF}" ]; then
        if [ ! -e "${TEMPLATE}" ]; then
            # Loud, because the alternative is a machine with no config and nothing
            # to say why.
            echo "billet: ${TEMPLATE} is missing, so ${CONF} was not created." >&2
            echo "        Copy billet.example.yaml there before starting billet." >&2

            return 1
        fi

        if ! cp "${TEMPLATE}" "${CONF}"; then
            echo "billet: ${TEMPLATE} could not be copied to ${CONF}, which is left as it was" >&2
            echo "        (absent, or partly written and removed below)." >&2

            rm -f "${CONF}"

            return 1
        fi
    fi

    if [ -e "${CONF}" ]; then
        # root owns it so an unprivileged process cannot edit what billet trusts;
        # the billet group can read it or the service cannot start at all.
        if ! chown root:billet "${CONF}" || ! chmod 0640 "${CONF}"; then
            echo "billet: ${CONF} could not be given root:billet 0640; billet-server will not" >&2
            echo "        read a configuration it cannot trust, so fix its owner and mode." >&2

            return 1
        fi
    fi

    return 0
}

enable_timers() {
    # AN IMAGE BUILD OR A CHROOT HAS NO RUNNING SYSTEMD, and booting it does not
    # re-run this scriptlet, so work skipped here is work nobody will do unless
    # this says so.
    if [ ! -d /run/systemd/system ]; then
        echo "billet: systemd is not running (this looks like an image build or a chroot), so" >&2
        echo "        billet-upgrade.timer and billet-images-refresh.timer were not enabled." >&2
        echo "        Run \`systemctl enable --now billet-upgrade.timer billet-images-refresh.timer\`" >&2
        echo "        on the booted host, or reinstall the package there." >&2

        return 0
    fi

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
        echo "        ${CONF} was not seeded and no timer was enabled. Re-run these decisions" >&2
        echo "        with \`dpkg-reconfigure billet\` on a deb host, or by reinstalling the" >&2
        echo "        package on an rpm one." >&2

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

    # THE SEEDING'S FAILURE IS CARRIED OUT, so the caller says what was left
    # half-done rather than reporting a clean install; the timers are still
    # attempted, because they are independent of the configuration and their
    # own failures are already reported one by one.
    seeded=0
    seed_config || seeded=$?

    enable_timers

    return "${seeded}"
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

# lock_name_ordinary says the lock's name is one this script may open.
#
# A FIFO AT THAT NAME WOULD HANG THE INSTALL. `>>` on a fifo with no reader
# BLOCKS, and nothing after it — not `flock -w`, not any bound here — ever
# runs; a device would be opened for what its driver does. What billet writes
# there is a regular file, so an existing name that is anything else is one
# this script declines to open at all.
#
# THE RESIDUAL, STATED: this is a check of the NAME, and the open that follows
# is a second lookup, so a writer that replaced the file between them would not
# be caught. Shell cannot open a path with O_NOFOLLOW|O_NONBLOCK and judge the
# descriptor, which is what would close it. The directory is root-owned, so
# that writer is already root; what this refuses is the shape a host can
# honestly be in, not an adversary who has the machine.
lock_name_ordinary() {
    if [ -e "${LIFECYCLE_LOCK}" ] || [ -L "${LIFECYCLE_LOCK}" ]; then
        [ -f "${LIFECYCLE_LOCK}" ] && [ ! -L "${LIFECYCLE_LOCK}" ]
    fi
}

# The statuses lock_and_decide answers with, beside 0 for work performed.
LOCK_HELD=66
LOCK_UNUSABLE=67
# DECISIONS_INCOMPLETE is not a deferral: the lock was taken and the work ran,
# and something in it failed with its own diagnostic already printed.
DECISIONS_INCOMPLETE=68

# lock_and_decide runs the unit decisions under the lifecycle lock, IN A
# SUBSHELL that holds the descriptor for its whole life.
#
# THE OPEN IS INSIDE IT BECAUSE A FAILED REDIRECTION ON `exec` ENDS THE SHELL
# IT RUNS IN, and that shell must not be the install's: a filesystem that went
# read-only between a probe and the open would otherwise take the scriptlet
# down instead of deferring. One descriptor, opened once, used by the flock and
# by nothing else, so there is no window between a check and its use.
lock_and_decide() (
    exec 9>>"${LIFECYCLE_LOCK}" || exit "${LOCK_UNUSABLE}"

    # -E SEPARATES CONTENTION FROM FAILURE: with it, 66 means the wait ended
    # with somebody else holding the lock, and any other non-zero status is
    # flock saying it could not lock at all — a filesystem that does not
    # support it, say — which waiting cannot fix.
    # THE VERDICT IS CAPTURED FROM THE COMMAND, not from the `if` around it:
    # `$?` after an `if` whose condition FAILED is the if statement's own
    # status, which POSIX makes zero, so contention read as a failure to lock
    # at all (probed in fedora:42, 2026-09-12).
    status=0
    flock -w 60 -E "${LOCK_HELD}" 9 || status=$?

    if [ "${status}" -eq 0 ]; then
        decided=0
        unit_decisions || decided=$?

        if [ "${decided}" -ne 0 ]; then
            exit "${DECISIONS_INCOMPLETE}"
        fi

        exit 0
    fi

    if [ "${status}" -eq "${LOCK_HELD}" ]; then
        exit "${LOCK_HELD}"
    fi

    exit "${LOCK_UNUSABLE}"
)

deferred=0

if command -v flock >/dev/null 2>&1 && lock_dir_ready && lock_name_ordinary; then
    if lock_and_decide; then
        deferred=0
    else
        deferred=$?
    fi
else
    deferred="${LOCK_UNUSABLE}"
fi

if [ "${deferred}" -ne 0 ]; then
    echo "billet: the unit decisions did not complete, so ${CONF} may not be seeded and" >&2
    echo "        billet-upgrade.timer and billet-images-refresh.timer may be as they were." >&2

    if [ "${deferred}" -eq "${DECISIONS_INCOMPLETE}" ]; then
        echo "        Part of that work FAILED rather than being deferred; its own message is" >&2
        echo "        above, and this host is left needing it done by hand." >&2
    elif [ "${deferred}" -eq "${LOCK_HELD}" ]; then
        echo "        A billet lifecycle operation holds ${LIFECYCLE_LOCK}; it may be a drain," >&2
        echo "        which takes as long as the work already on this host." >&2
    elif ! command -v flock >/dev/null 2>&1; then
        echo "        flock(1) is missing; install util-linux." >&2
    else
        echo "        ${LIFECYCLE_LOCK} could not be locked. Its directory is" >&2
        echo "        $(readlink -f "$(dirname "${LIFECYCLE_LOCK}")" 2>/dev/null || dirname "${LIFECYCLE_LOCK}")," >&2
        echo "        which must exist and be writable by root, and the lock itself must be a" >&2
        echo "        regular file on a filesystem that supports locking." >&2
    fi

    echo "        Once that is so, re-run these decisions: \`dpkg-reconfigure billet\` on a" >&2
    echo "        deb host, or reinstalling the package on an rpm one." >&2
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
