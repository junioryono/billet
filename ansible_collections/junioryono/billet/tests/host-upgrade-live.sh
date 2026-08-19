#!/bin/sh
set -eu

if [ "${BILLET_ACCEPT_DESTRUCTIVE_HOST_UPGRADE_TEST:-}" != 1 ]; then
    echo "host-upgrade-live: set BILLET_ACCEPT_DESTRUCTIVE_HOST_UPGRADE_TEST=1 for the named disposable or maintenance-window host" >&2
    exit 1
fi
if [ "$#" -ne 4 ]; then
    echo "usage: host-upgrade-live.sh INVENTORY HOST CANDIDATE_A CANDIDATE_B" >&2
    exit 1
fi

inventory=$1
host=$2
candidate_a=$3
candidate_b=$4
here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
collection_root=$(CDPATH= cd -- "${here}/../../.." && pwd)
callback_dir=${here}/callback_plugins
work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT INT TERM

export ANSIBLE_COLLECTIONS_PATH=${collection_root}
export PYTHONDONTWRITEBYTECODE=1

observe() {
    output=$1
    ansible "${host}" -i "${inventory}" --become --one-line \
        -m ansible.builtin.shell \
        -a 'set -eu; sha256sum /usr/bin/billet /etc/billet/billet.yaml /etc/systemd/system/billet-server.service /etc/systemd/system/billet-node.service; stat -c "%n %U:%G %a" /usr/bin/billet /etc/billet/billet.yaml /etc/systemd/system/billet-server.service /etc/systemd/system/billet-node.service; systemctl is-active billet-server.service billet-node.service; systemctl is-enabled billet-server.service billet-node.service; test ! -e /var/lib/billet/upgrades/active; test ! -e /var/lib/billet/server/billet.maintenance; ! systemctl show-environment | grep -q "^BILLET_MAINTENANCE="; /usr/bin/billet check --config /etc/billet/billet.yaml >/dev/null; echo ledger-check=ok' >"${output}"
}

run_playbook() {
    ansible-playbook -i "${inventory}" "${here}/host-upgrade-live.yml" --limit "${host}"
}

interrupt_after() {
    target=$1
    candidate=$2
    echo "Interrupting after: ${target}"
    if (
        export BILLET_BINARY_PATH=${candidate}
        export ANSIBLE_CALLBACK_PLUGINS=${callback_dir}
        export ANSIBLE_CALLBACKS_ENABLED=billet_kill_after
        export BILLET_KILL_AFTER_TASK=${target}
        run_playbook
    ); then
        echo "host-upgrade-live: controller was not interrupted after ${target}" >&2
        exit 1
    fi
}

recover_uncommitted() {
    target=$1
    if (unset BILLET_BINARY_PATH; run_playbook); then
        echo "host-upgrade-live: recovery after ${target} did not request the required fresh converge" >&2
        exit 1
    fi
    observe "${work}/after"
    if ! cmp -s "${work}/baseline" "${work}/after"; then
        diff -u "${work}/baseline" "${work}/after" >&2 || true
        echo "host-upgrade-live: ${target} did not restore exact host inputs and service state" >&2
        exit 1
    fi
}

observe "${work}/baseline"
for target in \
    'Publish the durable host-upgrade pointer before live mutation' \
    'Hide the old executable before establishing the maintenance fence' \
    'Commit the durable ledger snapshot before exposing the candidate' \
    'Install the candidate billet binary after fencing and ledger preservation' \
    'Validate and migrate with the new billet binary as the only ledger writer' \
    'Start the upgraded billet server before admitting compute' \
    'Start the upgraded billet node after validation and image verification'
do
    interrupt_after "${target}" "${candidate_a}"
    recover_uncommitted "${target}"
done

interrupt_after 'Open the committed ledger to operator commands' "${candidate_a}"
if (unset BILLET_BINARY_PATH; run_playbook); then
    echo "host-upgrade-live: committed finalization did not request the required fresh converge" >&2
    exit 1
fi
observe "${work}/committed-a"

interrupt_after 'Close the durable host-upgrade transaction after commit' "${candidate_b}"
observe "${work}/after-pointer-removal"
(export BILLET_BINARY_PATH=${candidate_b}; run_playbook) >"${work}/idempotent.log"
if ! grep -Eq 'changed=0[[:space:]]+unreachable=0[[:space:]]+failed=0' "${work}/idempotent.log"; then
    tail -80 "${work}/idempotent.log" >&2
    echo "host-upgrade-live: post-commit converge was not idempotent" >&2
    exit 1
fi
observe "${work}/committed-b"
if ! cmp -s "${work}/after-pointer-removal" "${work}/committed-b"; then
    diff -u "${work}/after-pointer-removal" "${work}/committed-b" >&2 || true
    echo "host-upgrade-live: pointer-removal interruption did not leave the committed host exact" >&2
    exit 1
fi

echo "host-upgrade-live: every rollback and commit interruption boundary passed"
