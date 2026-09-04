#!/bin/bash
# The rollout rehearsal: a packaged control plane and a packaged node, both on
# release FROM under real systemd, moved to release TO by `billet rollout start`
# and nothing else, then asked to take a candidate that cannot serve them and
# proved to roll back.
#
# WHAT THIS PROVES THAT NO TEST DOES. `cmd/billet/hostupgrade.go` stops a
# service, replaces a binary and migrates a ledger; every test of it supplies a
# fake host and asserts the ORDER those are called in. This is the first place
# the doing happens: systemd's `Type=notify` start, the packaged units, the
# service account's ownership of the ledger, the detached updater surviving the
# node that started it, the coordinator observing a real registration on the
# target, and the controller's own upgrade through the root timer the package
# enables (from v0.6.0; on an older FROM the controller is upgraded the way its
# documentation of the day said, by running `billet host-upgrade` on it).
#
# THE FAILING CANDIDATE IS A DOWNGRADE, because a rehearsal that runs on the day
# a release is cut has exactly two releases to work with. A rollout back to FROM
# with --allow-downgrade puts a binary on the controller that cannot open the
# ledger TO migrated, so the candidate's own probe refuses, the transaction rolls
# back and the host stays on TO. That is the rollback path under a real
# service manager, and the watermark's own restore with it.
#
#   BILLET_REHEARSAL_FROM=v0.5.0 BILLET_REHEARSAL_TO=v0.6.0 \
#   BILLET_REHEARSAL_APP_CONFIG=... BILLET_REHEARSAL_APP_KEY=... \
#   scripts/rollout-rehearsal.sh
set -euo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/rehearsal-lib.sh
. "${here}/rehearsal-lib.sh"

rehearsal_require_docker
rehearsal_require_app

: "${BILLET_REHEARSAL_FROM:?set BILLET_REHEARSAL_FROM to the release tag the hosts start on}"
: "${BILLET_REHEARSAL_TO:?set BILLET_REHEARSAL_TO to the release tag the rollout moves them to}"

FROM=${BILLET_REHEARSAL_FROM}
TO=${BILLET_REHEARSAL_TO}
FROM_VERSION=${FROM#v}
TO_VERSION=${TO#v}

if [ "${FROM}" = "${TO}" ]; then
    rehearsal_fail "FROM and TO are both ${FROM}; a rollout to the running release moves nothing"
fi

# THE PACKAGE ENABLES billet-upgrade.timer FROM v0.6.0. Before that the
# controller's own upgrade was the operator's, and this rehearsal does what the
# operator would have done, so a FROM below v0.6.0 is a supported starting point
# and the record says which half was manual.
timer_release=v0.6.0
controller_has_timer=no
if [ "$(printf '%s\n%s\n' "${timer_release}" "${FROM}" | sort -V | head -1)" = "${timer_release}" ]; then
    controller_has_timer=yes
fi

id=$(date -u +%Y%m%d%H%M%S)
network="billet-rehearsal-${id}"
controller="rehearsal-controller"
node="rehearsal-node"
label="rehearse-${id}-2vcpu"
storage=$(mktemp -d)
work=$(mktemp -d)
started_at=$(date -u +%s)

# THE SCALE SET OUTLIVES THE CONTAINERS unless something removes it, and a scale
# set nothing serves makes a job aimed at its label queue for 24 hours. So the
# teardown asks the control plane to remove it before the hosts go, and runs on
# every exit.
cleanup() {
    status=$?
    set +e

    echo
    echo "=== teardown"
    if docker exec "${controller}" test -f /etc/billet/billet.yaml >/dev/null 2>&1; then
        rehearsal_as_billet "${controller}" /usr/bin/billet teardown --all --yes \
            --config /etc/billet/billet.yaml 2>&1 | tail -3 || true
    fi

    if [ "${status}" -ne 0 ]; then
        echo "--- ${controller} journal (server, upgrade timer)"
        docker exec "${controller}" journalctl -u billet-server -u billet-upgrade.service \
            -n 40 --no-pager -o cat 2>&1 | tail -40 || true
        echo "--- ${node} journal"
        docker exec "${node}" journalctl -u billet-node -n 40 --no-pager -o cat 2>&1 | tail -40 || true
    fi

    rehearsal_teardown_hosts "${network}" "${storage}" "${controller}" "${node}"
    rm -rf "${work}" || true
    exit "${status}"
}
trap cleanup EXIT INT TERM

rehearsal_step "fetch and verify both releases"
rehearsal_fetch_release "${FROM}" "${work}/from"
from_deb=${REHEARSAL_DEB}
rehearsal_fetch_release "${TO}" "${work}/to"
to_deb=${REHEARSAL_DEB}
to_digest=${REHEARSAL_MANIFEST_SHA256}
: "${to_deb}"

rehearsal_step "two packaged hosts on ${FROM}, on one network"
docker network create "${network}" >/dev/null
rehearsal_start_host "${controller}" "${network}" no "${storage}"
rehearsal_start_host "${node}" "${network}" yes "${storage}"
rehearsal_install_package "${controller}" "${from_deb}"
rehearsal_install_package "${node}" "${from_deb}"

# The controller's file: a server, the borrowed App, one docker tier under a
# label no other deployment uses. No release: block, so the deployment is on
# automatic updates exactly as a deployment that says nothing is, which is the
# shape this rehearsal is about; the rollout below is still the operator's, and
# the starter starts nothing over an open rollout or a fleet on the channel.
{
    cat <<EOF
server:
  listen: 0.0.0.0:7717
  node_tls_hosts: [${controller}]
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 16GiB
EOF
    rehearsal_github_block
    cat <<EOF
tiers:
  - label: ${label}
    provider: docker
    trust: untrusted
    vcpu: 2
    memory: 4GiB
    disk: 20GiB
    image: ghcr.io/actions/actions-runner:latest
    command: ["./run.sh"]
EOF
} | rehearsal_install_config "${controller}"

# The node's file: no server, no App, the docker provider, a certificate the
# controller issues below. Its name comes from the certificate.
rehearsal_install_config "${node}" <<EOF
node:
  server_addr: ${controller}:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
  tls:
    cert: /etc/billet/tls/node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt
EOF

rehearsal_step "the controller issues the node's certificate"
rehearsal_issue_bundle "${controller}" "${node}" "${work}/bundle"
rehearsal_install_bundle "${node}" "${work}/bundle"

rehearsal_step "billet local up on both, controller first"
docker exec "${controller}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -6
docker exec "${node}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -6

test "$(rehearsal_active "${controller}" billet-server.service)" = active ||
    rehearsal_fail "billet-server.service is not active on ${controller} after local up"
test "$(rehearsal_active "${node}" billet-node.service)" = active ||
    rehearsal_fail "billet-node.service is not active on ${node} after local up"

rehearsal_wait_for 120 "the node to register" "${controller}" \
    runuser -u billet -- sh -c "/usr/bin/billet status --config /etc/billet/billet.yaml | grep -q '${node}'" ||
    rehearsal_fail "${node} never appeared in billet status on the controller"

test "$(rehearsal_version "${controller}")" = "${FROM_VERSION}" ||
    rehearsal_fail "the controller reports $(rehearsal_version "${controller}"), not ${FROM_VERSION}, before the rollout"
test "$(rehearsal_version "${node}")" = "${FROM_VERSION}" ||
    rehearsal_fail "the node reports $(rehearsal_version "${node}"), not ${FROM_VERSION}, before the rollout"

rehearsal_step "billet rollout start --version ${TO}"
rehearsal_as_billet "${controller}" /usr/bin/billet rollout start --version "${TO}" \
    --config /etc/billet/billet.yaml 2>&1 | tail -8

rehearsal_step "the controller goes first"
controller_upgrade_started=$(date -u +%s)
if [ "${controller_has_timer}" = yes ]; then
    # The packaged timer runs every five minutes; a rehearsal asks its unit to
    # run now rather than waiting for the tick, which is the same command with
    # the same instruction.
    docker exec "${controller}" systemctl start billet-upgrade.service 2>&1 | tail -3 || true
else
    echo "(${FROM} predates billet-upgrade.timer; running host-upgrade on the controller as its docs said)"
    docker exec "${controller}" /usr/bin/billet host-upgrade --version "${TO}" \
        --manifest-sha256 "${to_digest}" --config /etc/billet/billet.yaml 2>&1 | tail -12
fi

rehearsal_wait_for 600 "the controller to run ${TO}" "${controller}" \
    sh -c "/usr/bin/billet version | grep -q ' ${TO_VERSION} '" ||
    rehearsal_fail "the controller did not come up on ${TO}"
controller_upgrade_took=$(($(date -u +%s) - controller_upgrade_started))

step=$(rehearsal_journal_step "${controller}")
test "${step}" = committed ||
    rehearsal_fail "the controller's upgrade journal ends at '${step:-<none>}', not committed"
test "$(rehearsal_active "${controller}" billet-server.service)" = active ||
    rehearsal_fail "billet-server.service is not active on the controller after its upgrade"
docker exec "${controller}" grep -qF "${to_digest}" /var/lib/billet/installed.json ||
    rehearsal_fail "the controller's provenance does not name ${TO}'s manifest"

rehearsal_step "the coordinator moves the node"
node_upgrade_started=$(date -u +%s)
rehearsal_wait_for 900 "the node to run ${TO}" "${node}" \
    sh -c "/usr/bin/billet version | grep -q ' ${TO_VERSION} '" ||
    rehearsal_fail "the node was never moved to ${TO}"
node_upgrade_took=$(($(date -u +%s) - node_upgrade_started))

step=$(rehearsal_journal_step "${node}")
test "${step}" = committed ||
    rehearsal_fail "the node's upgrade journal ends at '${step:-<none>}', not committed"
test "$(rehearsal_active "${node}" billet-node.service)" = active ||
    rehearsal_fail "billet-node.service is not active on the node after its upgrade"
docker exec "${node}" grep -qF "${to_digest}" /var/lib/billet/installed.json ||
    rehearsal_fail "the node's provenance does not name ${TO}'s manifest"

rehearsal_wait_for 300 "the rollout to complete" "${controller}" \
    runuser -u billet -- sh -c "/usr/bin/billet rollout status --config /etc/billet/billet.yaml | grep -q 'No rollout is running. The last one was .* completed'" ||
    rehearsal_fail "billet rollout status never reported the rollout completed"

rehearsal_as_billet "${controller}" /usr/bin/billet rollout status --config /etc/billet/billet.yaml 2>&1 | tail -4
docker exec "${controller}" /usr/bin/billet host-upgrade --status --config /etc/billet/billet.yaml 2>&1 | tail -6

for u in billet-upgrade.timer billet-backup.timer; do
    en=$(docker exec "${controller}" systemctl is-enabled "${u}" 2>/dev/null || true)
    echo "${controller}: ${u} is ${en:-absent}"
done
test "$(docker exec "${controller}" systemctl is-enabled billet-upgrade.timer 2>/dev/null || true)" = enabled ||
    rehearsal_fail "billet-upgrade.timer is not enabled on the controller after moving to ${TO}"

rehearsal_step "a candidate that cannot serve this ledger is rolled back"
# FROM cannot open a ledger TO migrated. Recording the downgrade is the operator's
# deliberate act (--allow-downgrade); what follows is the transaction refusing
# to commit a binary whose own probe failed, and putting TO back.
rehearsal_as_billet "${controller}" /usr/bin/billet rollout start --version "${FROM}" --allow-downgrade \
    --config /etc/billet/billet.yaml 2>&1 | tail -6

journals_before=$(docker exec "${node}" sh -c 'ls -1d /var/lib/billet/upgrades/*/ 2>/dev/null | wc -l')
rollback_started=$(date -u +%s)
docker exec "${controller}" systemctl start billet-upgrade.service 2>&1 | tail -3 || true

rehearsal_wait_for 600 "the controller's candidate to be rolled back" "${controller}" \
    sh -c 'j=$(ls -1dt /var/lib/billet/upgrades/*/journal.json | head -1); grep -qE "\"step\": *\"rolled_back\"" "$j"' ||
    rehearsal_fail "the controller's journal never recorded a rollback"
rollback_took=$(($(date -u +%s) - rollback_started))

test "$(rehearsal_version "${controller}")" = "${TO_VERSION}" ||
    rehearsal_fail "after the rollback the controller reports $(rehearsal_version "${controller}"), not ${TO_VERSION}"
rehearsal_wait_for 120 "the server to be back" "${controller}" systemctl is-active --quiet billet-server.service ||
    rehearsal_fail "billet-server.service did not come back after the rollback"
test "$(rehearsal_version "${node}")" = "${TO_VERSION}" ||
    rehearsal_fail "the node moved to $(rehearsal_version "${node}") although the controller never converged"
journals_after=$(docker exec "${node}" sh -c 'ls -1d /var/lib/billet/upgrades/*/ 2>/dev/null | wc -l')
test "${journals_before}" = "${journals_after}" ||
    rehearsal_fail "the node was dispatched an upgrade (${journals_before} -> ${journals_after} journals) before the controller converged"

rehearsal_as_billet "${controller}" /usr/bin/billet rollout status --config /etc/billet/billet.yaml 2>&1 | tail -6
rehearsal_as_billet "${controller}" /usr/bin/billet rollout abort \
    --reason "rehearsal: the downgrade candidate was proved to roll back" \
    --config /etc/billet/billet.yaml 2>&1 | tail -3

echo
echo "rollout rehearsal: PASSED"
echo "  ${FROM} -> ${TO} on ${REHEARSAL_ARCH}; controller upgrade ${controller_upgrade_took}s (timer: ${controller_has_timer}); node converged ${node_upgrade_took}s after the controller; rollback ${rollback_took}s; total $(($(date -u +%s) - started_at))s"
