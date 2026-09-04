#!/bin/bash
# The recover rehearsal: `billet local recover` over a deployment a REAL control
# plane has served, on a packaged host, with a node that has to come back.
#
# WHAT THIS PROVES THAT THE RESTORE REHEARSAL DOES NOT. scripts/restore-rehearsal.sh
# recovers a deployment that was commissioned by `billet ca issue` alone: no
# server ever ran, no node ever registered, and nothing afterwards starts a
# control plane, because that leg has no App. Here the control plane runs under
# systemd against GitHub, a node registers over the real wire, the ledger is
# backed up, moved on, and put back with `local recover`; then `billet local up`
# starts the recovered control plane, the seal the recovery left is proved to
# survive that start, `billet resume` lifts it, and the node that trusted the
# old ledger is proved to be serving the recovered one.
#
# THE PACKAGE UNDER TEST IS THE TREE'S OWN (`make dist`), not a published
# release: this rehearsal is about what the code being changed does on a real
# host, on every run.
#
#   BILLET_REHEARSAL_APP_CONFIG=... BILLET_REHEARSAL_APP_KEY=... scripts/recover-rehearsal.sh
set -euo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/rehearsal-lib.sh
. "${here}/rehearsal-lib.sh"

rehearsal_require_docker
rehearsal_require_app
rehearsal_require_dist_package "${here}/.."

id=$(date -u +%Y%m%d%H%M%S)
network="billet-rehearsal-${id}"
controller="rehearsal-controller"
node="rehearsal-node"
label="rehearse-${id}-2vcpu"
storage=$(mktemp -d)
work=$(mktemp -d)
started_at=$(date -u +%s)

cleanup() {
    status=$?
    set +e
    trap - INT TERM
    status=$(rehearsal_verdict "${status}")

    echo
    echo "=== teardown"
    if docker exec "${controller}" test -f /etc/billet/billet.yaml >/dev/null 2>&1; then
        if ! rehearsal_teardown_scale_sets "${controller}"; then
            echo "TEARDOWN FAILED: the scale set for ${label} may still exist. Remove it from any host" >&2
            echo "holding this App: billet teardown --tier ${label} --yes --config <that config>" >&2
            if [ "${status}" -eq 0 ]; then status=1; fi
        fi
    fi

    if [ "${status}" -ne 0 ]; then
        echo "--- ${controller} journal"
        docker exec "${controller}" journalctl -u billet-server -n 40 --no-pager -o cat 2>&1 | tail -40 || true
        echo "--- ${node} journal"
        docker exec "${node}" journalctl -u billet-node -n 40 --no-pager -o cat 2>&1 | tail -40 || true
    fi

    rehearsal_teardown_hosts "${network}" "${storage}" "${controller}" "${node}"
    rm -rf "${work}" || true
    exit "${status}"
}
# THE SENTINEL STARTS AT 0 HERE, whatever the environment says, or an exported
# REHEARSAL_PASSED=1 would turn an aborted run green. A signal exits through its
# own status so that cleanup, which only the EXIT trap runs, reads a failure and
# not the $? of whatever the signal interrupted.
REHEARSAL_PASSED=0
trap 'exit 130' INT
trap 'exit 143' TERM
trap cleanup EXIT

rehearsal_step "two packaged hosts on the tree's own package"
docker network create "${network}" >/dev/null
rehearsal_start_host "${controller}" "${network}" no "${storage}"
rehearsal_start_host "${node}" "${network}" yes "${storage}"
rehearsal_install_package "${controller}" "${REHEARSAL_DIST_DEB}"
rehearsal_install_package "${node}" "${REHEARSAL_DIST_DEB}"
rehearsal_install_app_key "${controller}"

{
    cat <<EOF
server:
  listen: 0.0.0.0:7717
  node_tls_hosts: [${controller}]
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 16GiB
# NOT ON AUTOMATIC UPDATES. This is the tree's own snapshot build, which reports
# a release the stable channel is not on, so the starter opened a rollout to the
# channel within a minute of boot (measured 2026-09-04) and the packaged root
# timer then fenced the ledger for its host transaction, refusing the very
# `local down` this rehearsal is about. The rollout rehearsal is the one that
# wants the default.
release:
  automatic: false
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

rehearsal_step "commission the deployment and start it"
rehearsal_issue_bundle "${controller}" "${node}" "${work}/bundle"
rehearsal_install_bundle "${node}" "${work}/bundle"

since=$(rehearsal_clock "${controller}")
docker exec "${controller}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -6
docker exec "${node}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -6
rehearsal_wait_registered 120 "${controller}" "${node}" "${since}" ||
    rehearsal_fail "${node} never registered with the controller"

rehearsal_step "back the served deployment up"
docker exec "${controller}" install -d -o billet -g billet -m 0700 /var/backups/billet
rehearsal_as_billet "${controller}" /usr/bin/billet local backup --config /etc/billet/billet.yaml \
    --out /var/backups/billet/a --no-upload 2>&1 | tail -6
docker exec "${controller}" test -f /var/backups/billet/a/manifest.json ||
    rehearsal_fail "the backup wrote no manifest"

rehearsal_step "the ledger moves on after the backup"
# A second admission the archive has never heard of: the recovered ledger must
# not know this name, and the ledger it supersedes must be kept because it does.
rehearsal_as_billet "${controller}" /usr/bin/billet ca issue "${node}-later" \
    --config /etc/billet/billet.yaml --out "/tmp/${node}-later-tls" >/dev/null
# PROVED PRESENT BEFORE IT IS PROVED ABSENT, or a command that stopped recording
# admissions would leave the later absence check green.
admissions=$(rehearsal_as_billet "${controller}" /usr/bin/billet nodes pending --all --config /etc/billet/billet.yaml 2>&1) ||
    rehearsal_fail "billet nodes pending --all failed: ${admissions}"
grep -qF "${node}-later" <<<"${admissions}" ||
    rehearsal_fail "the live ledger does not record the admission of ${node}-later, so its absence later would prove nothing"

rehearsal_step "stop the control plane the way an operator does"
docker exec "${controller}" /usr/bin/billet local down --timeout 10m \
    --reason "recover rehearsal" --config /etc/billet/billet.yaml 2>&1 | tail -8
rehearsal_stopped "${controller}" billet-server.service ||
    rehearsal_fail "billet-server.service is $(rehearsal_active "${controller}" billet-server.service) after local down; only inactive or failed proves a stop"

rehearsal_step "billet local recover, as root, from the archive"
docker exec "${controller}" /usr/bin/billet local recover --config /etc/billet/billet.yaml \
    --from /var/backups/billet/a --old-controller-fenced --dry-run 2>&1 | tail -8
docker exec "${controller}" /usr/bin/billet local recover --config /etc/billet/billet.yaml \
    --from /var/backups/billet/a --old-controller-fenced \
    --reason "recover rehearsal: the ledger is put back where it was at the backup" 2>&1 | tail -12

superseded=$(docker exec "${controller}" sh -c 'find /var/lib/billet/server -maxdepth 1 -name "billet.db.superseded-*" -print -quit')
test -n "${superseded}" || rehearsal_fail "the recovery did not preserve the ledger it replaced"
docker exec "${controller}" test -s "${superseded}" || rehearsal_fail "the preserved ledger is empty"

# ROOT PUT IT BACK; THE SERVICE ACCOUNT MUST OWN IT. This is what the first
# restore rehearsal found (restore-rehearsal.md): nothing repairs ownership
# under a correctly-owned directory, so a recover that leaves a root-owned file
# leaves a control plane that cannot start.
foreign=$(docker exec "${controller}" find /var/lib/billet/server -not -user billet -not -name 'billet.db.superseded-*' -print) ||
    rehearsal_fail "could not inspect the state directory's ownership"
test -z "${foreign}" || rehearsal_fail "root left these behind in the state directory: ${foreign}"

rehearsal_step "the recovered control plane starts, sealed"
since=$(rehearsal_clock "${controller}")
docker exec "${controller}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -6
test "$(rehearsal_active "${controller}" billet-server.service)" = active ||
    rehearsal_fail "billet-server.service is not active after the recovery"

status=$(rehearsal_as_billet "${controller}" /usr/bin/billet status --config /etc/billet/billet.yaml 2>&1) ||
    rehearsal_fail "billet status failed on the recovered deployment: ${status}"
sed -n '1,6p' <<<"${status}"
grep -q 'not taking new work' <<<"${status}" ||
    rehearsal_fail "the recovered deployment is not sealed after local up; nodes may hold compute it has never heard of"
grep -q 'survives a restart' <<<"${status}" ||
    rehearsal_fail "the seal is not an operator's, so the next start would have cleared it"
admissions=$(rehearsal_as_billet "${controller}" /usr/bin/billet nodes pending --all --config /etc/billet/billet.yaml 2>&1) ||
    rehearsal_fail "billet nodes pending --all failed on the recovered deployment: ${admissions}"
if grep -qF "${node}-later" <<<"${admissions}"; then
    rehearsal_fail "the recovered ledger knows ${node}-later, which was admitted after the backup"
fi

rehearsal_step "the node that trusted the old ledger serves the recovered one"
rehearsal_wait_registered 180 "${controller}" "${node}" "${since}" ||
    rehearsal_fail "${node} did not re-register with the recovered control plane"

rehearsal_step "billet resume lifts the operator's seal"
rehearsal_as_billet "${controller}" /usr/bin/billet resume --config /etc/billet/billet.yaml 2>&1 | tail -3
status=$(rehearsal_as_billet "${controller}" /usr/bin/billet status --config /etc/billet/billet.yaml 2>&1) ||
    rehearsal_fail "billet status failed after resume: ${status}"
if grep -q 'not taking new work' <<<"${status}"; then
    rehearsal_fail "the deployment is still sealed after billet resume"
fi

echo
echo "recover rehearsal: PASSED"
echo "  package $(rehearsal_version "${controller}") on ${REHEARSAL_ARCH}; superseded ledger kept at ${superseded}; total $(($(date -u +%s) - started_at))s"
# THE LAST STATEMENT, after every line of output: a signal landing before this
# still fails the run.
REHEARSAL_PASSED=1
