#!/bin/sh
# Runs INSIDE the container scripts/test-unit-sandboxes.sh starts, as root under
# systemd: installs the package, starts each strict unit with a stub ExecStart
# that exercises its command's paths inside the unit's sandbox, and judges from
# outside the sandbox where every write landed (#174).
#
# EVERY GATE IS AN if WITH AN EXPLICIT EXIT, never a `;` chain or a `!`-prefixed
# pipeline: under `set -e` both run on past a failure (billet-shell-gates).
set -eu

fail() {
    echo >&2
    echo "unit sandboxes FAILED: $*" >&2
    exit 1
}

# THROUGH apt, AS AN OPERATOR INSTALLS IT, so its dependencies (kmod and the
# rest) come with it; `dpkg -i` alone leaves it unconfigured.
if ! timeout -k 10 300 apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 \
    -o Acquire::https::CAInfo=/usr/local/share/billet-host-ca.crt \
    install -y -qq /tmp/billet.deb >/tmp/apt.log 2>&1; then
    tail -30 /tmp/apt.log >&2
    fail "the package did not install"
fi

# THE PACKAGE'S ONE EXCEPTION TO ENABLING NOTHING is two timers, and one of them
# fires a unit under test; this run starts every unit itself.
systemctl disable --now billet-upgrade.timer billet-images-refresh.timer >/dev/null 2>&1 || true

# A HOST THAT RUNS BOTH ROLES AND BOOTS FIRECRACKER GUESTS, with a node
# certificate under /etc/billet/tls, so every strict unit's command has every
# path it can need, renewal's directory included. Nothing here is contacted:
# the App key is read and never used, and no cluster is reached. The bundle is
# issued by billet itself, before the node's configuration names it.
write_config() {
    cat >/etc/billet/billet.yaml <<EOF
server:
  listen: $1
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: unit-sandboxes
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/app-private-key.pem
node:
  name: unit-sandboxes
  server_addr: 127.0.0.1:7717
  provider: firecracker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
$2
  firecracker:
    kernel_image: /var/lib/billet/kernels/vmlinux
    bridge: billet0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
tiers:
  - label: small
    provider: firecracker
    vcpu: 2
    memory: 4GiB
    image: ubuntu-2404-x64@verified
EOF
    chown root:billet /etc/billet/billet.yaml
    chmod 0640 /etc/billet/billet.yaml
}

printf 'a placeholder the units read and never use\n' >/etc/billet/app-private-key.pem
chown billet:billet /etc/billet/app-private-key.pem
chmod 0600 /etc/billet/app-private-key.pem

# A NODE WITH A CERTIFICATE DIALS A SERVER THAT SERVES mTLS, which a server
# listening on loopback alone does not, so the configuration that names the
# bundle binds the server beyond loopback (the node still dials 127.0.0.1).
write_config 127.0.0.1:7717 ""
if ! billet ca issue unit-sandboxes --config /etc/billet/billet.yaml --out /etc/billet/tls >/tmp/issue.log 2>&1; then
    cat /tmp/issue.log >&2
    fail "billet could not issue the node's certificate bundle"
fi
write_config 0.0.0.0:7717 "  tls:
    cert: /etc/billet/tls/node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt"

exercises=0
config=/etc/billet/billet.yaml
authority_lock=/var/lib/billet/authority.lock

# THE CONFIGURATION EVERY EXERCISE LOADS, loaded once first: a refusal here is
# the gate's own fixture, and must not read as a sandbox refusing a unit.
if ! billet local unit-paths --config "${config}" billet-node.service >/tmp/config-check 2>&1; then
    cat /tmp/config-check >&2
    fail "the gate's configuration does not load"
fi

# exercise_unit UNIT starts UNIT with only its ExecStart replaced by the
# exercise, judges from outside its sandbox where every write and lock landed,
# stops it, and judges that its locks outlived the stop. It answers 0, or 1
# with the reason in $why, and leaves no drop-in behind either way.
exercise_unit() {
    unit=$1
    why=

    # A FRESH MARKER EVERY EXERCISE: one an earlier, refused exercise left
    # behind would make this one's exclusive create fail for a reason that is
    # not the sandbox's.
    exercises=$((exercises + 1))
    marker=billet-unit-paths-gate-${exercises}

    # ONLY THE COMMAND IS REPLACED, and the unit stays active once it has run,
    # because its stop removes the runtime directories it declares, markers and
    # all, before anything outside could look.
    mkdir -p "/etc/systemd/system/${unit}.d"
    cat >"/etc/systemd/system/${unit}.d/50-unit-sandbox-stub.conf" <<EOF
[Service]
Type=oneshot
RemainAfterExit=yes
Restart=no
ExecStart=
ExecStart=/usr/bin/billet local unit-paths --config ${config} --exercise --marker ${marker} ${unit}
EOF
    systemctl daemon-reload

    # WHERE EACH WRITE AND LOCK MUST LAND, from the same declaration, read
    # outside the sandbox.
    if ! billet local unit-paths --config "${config}" "${unit}" >/tmp/paths 2>&1; then
        cat /tmp/paths >&2
        why="billet could not say which paths ${unit} needs"
        unstub "${unit}"
        return 1
    fi

    # A LOCK FILE ALREADY THERE PROVES NOTHING ABOUT THIS START, so each is
    # removed first (every unit is stopped, so nothing holds one) and must be
    # back once the unit has run. The host's authority lock stays: the
    # package's `local prepare` made it, its presence is what makes this a
    # prepared host, and only the exercise inside the sandbox proves it
    # reachable.
    while read -r access path rest; do
        case "${access}:${rest}" in
            lock:*"could not tell"*) ;;
            lock:*) [ "${path}" = "${authority_lock}" ] || rm -f "${path}" ;;
        esac
    done </tmp/paths

    # WHAT THIS START LOGGED, never an earlier exercise's: a refusal left in the
    # journal by another run would otherwise read as this one's reason.
    cursor=$(journalctl -n 0 --show-cursor -o cat --no-pager 2>/dev/null | sed -n 's/^-- cursor: //p')

    started=0
    if systemctl start "${unit}"; then
        started=1
    fi

    result=$(systemctl show --property=Result --value "${unit}")
    status=$(systemctl show --property=ExecMainStatus --value "${unit}")
    journalctl -u "${unit}" -o cat --no-pager ${cursor:+--after-cursor "${cursor}"} >/tmp/journal 2>&1 || true
    grep -E '^(ok|REFUSED) ' /tmp/journal || true
    refused=$(grep -E '^REFUSED ' /tmp/journal | tr '\n' ' ' || true)

    if [ "${started}" -ne 1 ] || [ "${result}" != success ]; then
        tail -20 /tmp/journal >&2
        why="${unit} did not start and succeed under its sandbox (Result=${result}, status=${status}) ${refused}"
        unstub "${unit}"
        return 1
    fi

    # Judged outside the sandbox: a shared directory must hold the marker the
    # unit wrote and a private one must not, and every lock must be a file on
    # the host, or the exercise wrote or locked somewhere the command does not
    # expect.
    writes=0
    : >/tmp/locks
    while read -r access path rest; do
        case "${access}" in
            write)
                if [ ! -f "${path}/${marker}" ]; then
                    why="${unit} wrote in ${path} inside its sandbox, and the host's ${path} has no trace of it"
                    unstub "${unit}"
                    return 1
                fi
                rm -f "${path}/${marker}"
                writes=$((writes + 1))
                ;;
            private)
                if [ -e "${path}/${marker}" ]; then
                    why="${unit}'s private ${path} is the host's: its write reached ${path}/${marker}"
                    unstub "${unit}"
                    return 1
                fi
                ;;
            lock)
                case "${rest}" in *"could not tell"*) continue ;; esac
                if [ ! -f "${path}" ]; then
                    why="${unit} took the lock ${path} inside its sandbox, and the host has no such file"
                    unstub "${unit}"
                    return 1
                fi
                echo "${path}" >>/tmp/locks
                ;;
        esac
    done </tmp/paths

    if [ "${writes}" -eq 0 ]; then
        why="${unit} declares nowhere it writes, so nothing was judged"
        unstub "${unit}"
        return 1
    fi

    if ! systemctl stop "${unit}"; then
        why="${unit} could not be stopped after its exercise"
        unstub "${unit}"
        return 1
    fi

    # A LOCK OUTLIVES THE UNIT THAT TOOK IT (#175): a stop that removed a lock's
    # directory under it would let the next locker take a fresh file at the
    # same name.
    while read -r lock; do
        if [ ! -f "${lock}" ]; then
            why="${unit}'s stop removed ${lock}, so the next locker would lock a different file"
            unstub "${unit}"
            return 1
        fi
    done </tmp/locks

    unstub "${unit}"
    echo "ok   ${unit}: its sandbox permits every path its command needs (${writes} writes judged, its locks outlive its stop)"
    return 0
}

# unstub stops UNIT if it is up, removes the stub drop-in, and removes every
# marker this exercise may have left, whichever way it ended.
unstub() {
    systemctl stop "$1" >/dev/null 2>&1 || true
    systemctl reset-failed "$1" >/dev/null 2>&1 || true
    rm -rf "/etc/systemd/system/$1.d"
    systemctl daemon-reload
    if billet local unit-paths --config "${config}" "$1" >/tmp/cleanup-paths 2>/dev/null; then
        while read -r access path _; do
            [ "${access}" = write ] && rm -f "${path}/${marker}"
        done </tmp/cleanup-paths
    fi
    return 0
}

# THE UNITS ARE BILLET'S TO NAME, in the order a fresh host runs them (the
# server first: the node on a host that runs both claims the identity the server
# founded in its directory, which the node's own sandbox cannot write; then the
# node, and then the units the timers `billet local up` enables last start).
# A strict unit billet declares is started here; one it does not is refused by
# TestEveryStrictUnitDeclaresItsPaths before a package is built.
if ! billet local unit-paths --units >/tmp/units 2>&1; then
    cat /tmp/units >&2
    fail "billet could not say which units it declares"
fi

[ -s /tmp/units ] || fail "billet declares no unit"

while read -r unit <&3; do
    if ! systemctl cat "${unit}" >/dev/null 2>&1; then
        fail "${unit} is not installed by the package"
    fi

    if ! exercise_unit "${unit}"; then
        fail "${why}"
    fi
done 3</tmp/units

# THE GATE CAN FAIL, and on the two incidents it exists for. Each is the shipped
# billet-images-refresh.service with one fix reverted, installed over the
# package's and refused, then the package's put back: #169 (its private staging
# directory named in ReadWritePaths), #176 (no writable /run/billet/locks) and
# #175 (the lock directory not preserved across its stop).
shipped=/usr/lib/systemd/system/billet-images-refresh.service
cp "${shipped}" /tmp/images-refresh.service
for incident in 169 176 175; do
    case "${incident}" in
        169) sed 's#^ReadWritePaths=/var/lib/billet -/srv/jailer$#& /var/tmp/billet-images#' /tmp/images-refresh.service >"${shipped}" ;;
        176) sed '/^RuntimeDirectory=billet\/locks$/d; /^RuntimeDirectoryMode=/d; /^RuntimeDirectoryPreserve=/d' /tmp/images-refresh.service >"${shipped}" ;;
        175) sed '/^RuntimeDirectoryPreserve=yes$/d' /tmp/images-refresh.service >"${shipped}" ;;
    esac

    if cmp -s "${shipped}" /tmp/images-refresh.service; then
        cp /tmp/images-refresh.service "${shipped}"
        fail "#${incident}'s revert did not change the shipped unit, so it proves nothing"
    fi

    systemctl daemon-reload
    if exercise_unit billet-images-refresh.service; then
        cp /tmp/images-refresh.service "${shipped}"
        fail "the gate passed billet-images-refresh.service with #${incident}'s fix reverted"
    fi

    # REFUSED FOR ITS OWN REASON, or the refusal proves nothing about the check
    # that should have made it: systemd's namespace setup for #169, the probe
    # lock under the read-only /run for #176, the lock gone after the stop for
    # #175.
    case "${incident}" in
        169) expected="status=226" ;;
        176) expected="/run/billet/locks/billet-imageverify-" ;;
        175) expected="stop removed /run/billet/locks/" ;;
    esac
    case "${why}" in
        *"${expected}"*) ;;
        *)
            cp /tmp/images-refresh.service "${shipped}"
            fail "#${incident}'s revert was refused, but not for the reason it reverts (want ${expected}): ${why}"
            ;;
    esac
    echo "ok   #${incident}'s revert is refused for its reason: ${why}"

    cp /tmp/images-refresh.service "${shipped}"
    systemctl daemon-reload
    # A REVERT THAT REMOVED THE LOCK DIRECTORY leaves it gone for the next one,
    # as on a host; it is made again the way the package's tmpfiles entry does.
    systemd-tmpfiles --create /usr/lib/tmpfiles.d/billet.conf
done

echo "unit sandboxes: every strict unit started under its own sandbox and reached what its command needs, and #169, #175 and #176 are refused"
