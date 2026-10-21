#!/bin/bash
# The restored deployment's first start: a control plane a real App served is
# backed up, its host is lost, and the deployment is restored onto a packaged
# host that has never seen it and started there with `billet local up`. The
# node that trusted the old controller, holding the certificate the old one
# issued and told nothing, is served by the new one.
#
# WHAT THIS PROVES THAT THE RESTORE REHEARSAL DOES NOT. scripts/restore-rehearsal.sh
# runs on every pull request and has no App, so nothing after the restore starts
# a control plane: `--upgrade-probe` is as far as it gets. Here the restored
# host runs `billet local up` against GitHub under real systemd, which is the
# step an operator takes next and the one the restore's ownership repair, the
# units and the restored authority all have to survive together.
#
# THE OLD CONTROLLER IS FENCED THE WAY AN OPERATOR FENCES IT: stopped with
# `billet local down`, which disables its units, and cut off the network. The
# node keeps the address it was given (a network alias the restored host takes
# over), so nothing about the node changes between the two controllers.
#
# THE PACKAGE UNDER TEST IS THE TREE'S OWN (`make dist`).
#
#   BILLET_REHEARSAL_APP_CONFIG=... BILLET_REHEARSAL_APP_KEY=... scripts/restore-first-start-rehearsal.sh
set -euo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/rehearsal-lib.sh
. "${here}/rehearsal-lib.sh"

rehearsal_require_docker
rehearsal_require_app
rehearsal_require_dist_package "${here}/.."

id=$(date -u +%Y%m%d%H%M%S)
network="billet-rehearsal-${id}"
old="rehearsal-old-controller"
new="rehearsal-new-controller"
node="rehearsal-node"
# THE ADDRESS THE NODE DIALS, held by whichever controller is serving.
address="rehearsal-controller"
label="rehearse-${id}-2vcpu"
storage=$(mktemp -d)
work=$(mktemp -d)
started_at=$(date -u +%s)

cleanup() {
    status=$?
    set +e
    trap '' INT TERM
    status=$(rehearsal_verdict "${status}")

    echo
    echo "=== teardown"
    # THE SCALE SET IS REMOVED THROUGH WHICHEVER CONTROLLER SERVED LAST, by
    # evidence rather than by probe: the old one is cut off the network once the
    # new one is the deployment, so it can reach no GitHub to tear anything down.
    if [ -n "${serving}" ]; then
        if ! rehearsal_teardown_scale_sets "${serving}"; then
            echo "TEARDOWN FAILED: the scale set for ${label} may still exist. Remove it from any host" >&2
            echo "holding this App: billet teardown --tier ${label} --yes --config <that config>" >&2
            if [ "${status}" -eq 0 ]; then status=1; fi
        fi
    fi

    if [ "${status}" -ne 0 ]; then
        for host in "${old}" "${new}"; do
            echo "--- ${host} journal"
            docker exec "${host}" journalctl -u billet-server -n 40 --no-pager -o cat 2>&1 | tail -40 || true
        done
        echo "--- ${node} journal"
        docker exec "${node}" journalctl -u billet-node -n 40 --no-pager -o cat 2>&1 | tail -40 || true
    fi

    rehearsal_teardown_hosts "${network}" "${storage}" "${old}" "${new}" "${node}"
    rm -rf "${work}" || true
    exit "${status}"
}
REHEARSAL_PASSED=0
serving=
trap 'exit 130' INT
trap 'exit 143' TERM
trap cleanup EXIT

# controller_config prints a controller's config: the tree's snapshot build on
# no automatic updates (see recover-rehearsal.sh for why), the borrowed App,
# and one tier under a label no other deployment uses. The same file on both
# controllers, which is what a restore onto a replacement host looks like.
controller_config() {
    cat <<EOF
server:
  listen: 0.0.0.0:7717
  node_tls_hosts: [${address}]
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 16GiB
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
}

rehearsal_step "the old controller and a node, on the tree's own package"
docker network create "${network}" >/dev/null
rehearsal_start_host "${old}" "${network}" no "${storage}" "${address}"
rehearsal_start_host "${node}" "${network}" yes "${storage}"
rehearsal_install_package "${old}" "${REHEARSAL_DIST_DEB}"
rehearsal_install_package "${node}" "${REHEARSAL_DIST_DEB}"
rehearsal_install_app_key "${old}"
controller_config | rehearsal_install_config "${old}"

rehearsal_install_config "${node}" <<EOF
node:
  server_addr: ${address}:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
  tls:
    cert: /etc/billet/tls/node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt
EOF

rehearsal_step "commission the deployment and serve the node"
rehearsal_issue_bundle "${old}" "${node}" "${work}/bundle"
rehearsal_install_bundle "${node}" "${work}/bundle"

since=$(rehearsal_clock "${old}")
serving=${old}
docker exec "${old}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -6
docker exec "${node}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -6
rehearsal_wait_registered 120 "${old}" "${node}" "${since}" ||
    rehearsal_fail "${node} never registered with the old controller"

rehearsal_step "back the served deployment up, and keep the archive off the host"
docker exec "${old}" install -d -o billet -g billet -m 0700 /var/backups/billet
rehearsal_as_billet "${old}" /usr/bin/billet local backup --config /etc/billet/billet.yaml \
    --out /var/backups/billet/a --no-upload 2>&1 | tail -6
docker cp "${old}:/var/backups/billet/a" "${work}/archive"
test -f "${work}/archive/manifest.json" || rehearsal_fail "the backup wrote no manifest"
docker cp "${old}:/var/lib/billet/server/deployment-id" "${work}/deployment-id"
docker cp "${old}:/var/lib/billet/server/ca/ca.crt" "${work}/ca.crt"

rehearsal_step "the old controller is lost: stopped, disabled, and cut off"
docker exec "${old}" /usr/bin/billet local down --timeout 10m \
    --reason "restore first-start rehearsal: this host is lost" --config /etc/billet/billet.yaml 2>&1 | tail -6
rehearsal_stopped "${old}" billet-server.service ||
    rehearsal_fail "billet-server.service is $(rehearsal_active "${old}" billet-server.service) on the old controller after local down"
enabled=$(docker exec "${old}" systemctl is-enabled billet-server.service 2>/dev/null || true)
test "${enabled}" = disabled ||
    rehearsal_fail "billet-server.service is ${enabled:-unknown} on the old controller; it must be disabled before a restore elsewhere"
docker network disconnect "${network}" "${old}"
serving=

rehearsal_step "a host that has never seen the deployment, restored as root"
rehearsal_start_host "${new}" "${network}" no "${storage}"
rehearsal_install_package "${new}" "${REHEARSAL_DIST_DEB}"
rehearsal_install_app_key "${new}"
controller_config | rehearsal_install_config "${new}"

# THE APP KEY THE RESTORE INSTALLS, not the copy rehearsal_install_app_key put
# there: the restore refuses to replace a key, so a host that already holds the
# same bytes would prove the refusal rather than the install. Moved aside and
# kept, because the teardown needs nothing from it.
docker exec "${new}" mv /etc/billet/app-private-key.pem /root/staged-app-key.pem

docker exec "${new}" install -d -m 0700 /var/backups/billet
docker cp "${work}/archive" "${new}:/var/backups/billet/a"
docker exec "${new}" /usr/bin/billet local restore --config /etc/billet/billet.yaml \
    --from /var/backups/billet/a --dry-run 2>&1 | tail -8
docker exec "${new}" /usr/bin/billet local restore --config /etc/billet/billet.yaml \
    --from /var/backups/billet/a --old-controller-fenced 2>&1 | tail -16

docker exec "${new}" cmp -s /etc/billet/app-private-key.pem /root/staged-app-key.pem ||
    rehearsal_fail "the restored App key differs from the one the old controller used"
docker cp "${new}:/var/lib/billet/server/deployment-id" "${work}/new-deployment-id"
docker cp "${new}:/var/lib/billet/server/ca/ca.crt" "${work}/new-ca.crt"
cmp -s "${work}/deployment-id" "${work}/new-deployment-id" ||
    rehearsal_fail "the restored host is a different deployment from the one that was backed up"
cmp -s "${work}/ca.crt" "${work}/new-ca.crt" ||
    rehearsal_fail "the restored host's authority is not the one the node trusts"

foreign=$(docker exec "${new}" find /var/lib/billet/server -not -user billet -print) ||
    rehearsal_fail "could not inspect the restored state directory's ownership"
test -z "${foreign}" || rehearsal_fail "root left these behind in the restored state directory: ${foreign}"

rehearsal_step "its first start, with billet local up"
# THE NEW HOST TAKES THE ADDRESS the node was given: reconnected under the alias,
# so the node's next dial reaches it with nothing on the node changed.
docker network disconnect "${network}" "${new}"
docker network connect --alias "${address}" "${network}" "${new}"

since=$(rehearsal_clock "${new}")
serving=${new}
docker exec "${new}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -8
test "$(rehearsal_active "${new}" billet-server.service)" = active ||
    rehearsal_fail "billet-server.service is not active on the restored host after local up"
test "$(docker exec "${new}" systemctl is-enabled billet-server.service 2>/dev/null || true)" = enabled ||
    rehearsal_fail "billet-server.service is not enabled on the restored host after local up"

rehearsal_step "the node that trusted the old controller is served by the restored one"
rehearsal_wait_registered 300 "${new}" "${node}" "${since}" ||
    rehearsal_fail "${node} did not register with the restored controller"

status=$(rehearsal_as_billet "${new}" /usr/bin/billet status --config /etc/billet/billet.yaml 2>&1) ||
    rehearsal_fail "billet status failed on the restored deployment: ${status}"
sed -n '1,8p' <<<"${status}"
grep -qF "${node}" <<<"${status}" ||
    rehearsal_fail "billet status on the restored host does not list ${node}"

echo
echo "restore first-start rehearsal: PASSED"
echo "  package $(rehearsal_version "${new}") on ${REHEARSAL_ARCH}; total $(($(date -u +%s) - started_at))s"
REHEARSAL_PASSED=1
