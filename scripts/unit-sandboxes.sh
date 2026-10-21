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

if ! dpkg -i /tmp/billet.deb >/tmp/dpkg.log 2>&1; then
    cat /tmp/dpkg.log >&2
    fail "the package did not install"
fi

# THE PACKAGE'S ONE EXCEPTION TO ENABLING NOTHING is two timers, and one of them
# fires a unit under test; this run starts every unit itself.
systemctl disable --now billet-upgrade.timer billet-images-refresh.timer >/dev/null 2>&1 || true

if [ -d /tmp/replacement-units ]; then
    for unit in /tmp/replacement-units/*.service; do
        [ -f "${unit}" ] || continue
        cp "${unit}" /usr/lib/systemd/system/
        echo "installed a replacement $(basename "${unit}") over the package's"
    done
fi

# A HOST THAT RUNS BOTH ROLES AND BOOTS FIRECRACKER GUESTS, with a node
# certificate under /etc/billet/tls, so every strict unit's command has every
# path it can need, renewal's directory included. Nothing here is contacted:
# the App key is read and never used, and no cluster is reached. The bundle is
# issued by billet itself, before the node's configuration names it.
write_config() {
    cat >/etc/billet/billet.yaml <<EOF
server:
  listen: 127.0.0.1:7717
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
$1
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

write_config ""
if ! billet ca issue unit-sandboxes --config /etc/billet/billet.yaml --out /etc/billet/tls >/tmp/issue.log 2>&1; then
    cat /tmp/issue.log >&2
    fail "billet could not issue the node's certificate bundle"
fi
write_config "  tls:
    cert: /etc/billet/tls/node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt"

marker=billet-unit-paths-gate
config=/etc/billet/billet.yaml

# THE UNITS ARE BILLET'S TO NAME, in the order `billet local up` starts them
# (the server first: the node on a host that runs both claims the identity the
# server founded in its directory, which the node's own sandbox cannot write).
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

    started=0
    if systemctl start "${unit}"; then
        started=1
    fi

    result=$(systemctl show --property=Result --value "${unit}")
    if [ "${started}" -ne 1 ] || [ "${result}" != success ]; then
        journalctl -u "${unit}" -o cat --no-pager 2>&1 | tail -40 >&2
        fail "${unit} did not start and succeed under its sandbox (Result=${result})"
    fi

    journalctl -u "${unit}" -o cat --no-pager 2>&1 | grep -E '^(ok|REFUSED) ' || true

    # WHERE EACH WRITE LANDED, judged outside the sandbox by the same
    # declaration: a shared directory must hold the marker the unit wrote, and a
    # private one must not, or the write went somewhere the command does not
    # expect.
    if ! billet local unit-paths --config "${config}" "${unit}" >/tmp/paths 2>&1; then
        cat /tmp/paths >&2
        fail "billet could not say which paths ${unit} needs"
    fi

    writes=0
    while read -r access path _; do
        case "${access}" in
            write)
                if [ ! -f "${path}/${marker}" ]; then
                    fail "${unit} wrote in ${path} inside its sandbox, and the host's ${path} has no trace of it"
                fi
                rm -f "${path}/${marker}"
                writes=$((writes + 1))
                ;;
            private)
                if [ -e "${path}/${marker}" ]; then
                    fail "${unit}'s private ${path} is the host's: its write reached ${path}/${marker}"
                fi
                ;;
        esac
    done </tmp/paths

    [ "${writes}" -gt 0 ] || fail "${unit} declares nowhere it writes, so nothing was judged"

    if ! systemctl stop "${unit}"; then
        fail "${unit} could not be stopped after its exercise"
    fi
    rm -rf "/etc/systemd/system/${unit}.d"
    systemctl daemon-reload
    echo "ok   ${unit}: its sandbox permits every path its command needs (${writes} writes judged)"
done 3</tmp/units

echo "unit sandboxes: every strict unit started under its own sandbox and reached what its command needs"
