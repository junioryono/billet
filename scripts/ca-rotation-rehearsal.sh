#!/bin/bash
# The CA rotation rehearsal: `billet ca rotate` then `billet ca retire` across a
# real fleet whose nodes renew, on the tree's own package under real systemd.
#
# WHAT THIS PROVES THAT THE TESTS DO NOT. internal/wirecert's five-file state
# machine is tested in every state, and the end-to-end suite enrols across a
# rotation in process. What had never happened is the operator's sequence on a
# packaged host with nodes that renew on their own clock: rotate, restart the
# control plane so it presents the overlap, wait for each node's sweep to renew
# onto the new authority and install the certificate it was handed, retire the
# old one, and prove every node still polls and a fresh bundle verifies.
#
# NODES RENEW WHEN LESS THAN A THIRD OF A LEAF'S LIFE REMAINS, on the sweep that
# runs every five minutes, so with year-long leaves nothing renews inside a
# rehearsal. `billet ca issue --lifetime` exists for exactly this: the nodes
# below get twelve-minute certificates and the rehearsal watches the window in
# which each must renew.
#
#   BILLET_REHEARSAL_APP_CONFIG=... BILLET_REHEARSAL_APP_KEY=... scripts/ca-rotation-rehearsal.sh
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
node_a="rehearsal-node-a"
node_b="rehearsal-node-b"
label="rehearse-${id}-2vcpu"
leaf_lifetime=12m
# The window in which a twelve-minute leaf must be renewed: renewal is due once
# less than four minutes remain (eight minutes in), the sweep runs every five,
# so the latest a healthy node renews is about thirteen minutes after issue.
renew_deadline=$((16 * 60))
storage=$(mktemp -d)
work=$(mktemp -d)
started_at=$(date -u +%s)

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
        for h in "${controller}" "${node_a}" "${node_b}"; do
            echo "--- ${h} journal"
            docker exec "${h}" journalctl -u billet-server -u billet-node -n 30 --no-pager -o cat 2>&1 | tail -30 || true
        done
    fi

    rehearsal_teardown_hosts "${network}" "${storage}" "${controller}" "${node_a}" "${node_b}"
    rm -rf "${work}" || true
    exit "${status}"
}
trap cleanup EXIT INT TERM

rehearsal_step "a controller and two nodes on the tree's own package"
docker network create "${network}" >/dev/null
rehearsal_start_host "${controller}" "${network}" no "${storage}"
rehearsal_start_host "${node_a}" "${network}" yes "${storage}"
rehearsal_start_host "${node_b}" "${network}" yes "${storage}"
for h in "${controller}" "${node_a}" "${node_b}"; do
    rehearsal_install_package "${h}" "${REHEARSAL_DIST_DEB}"
done

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

for h in "${node_a}" "${node_b}"; do
    rehearsal_install_config "${h}" <<EOF
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
done

rehearsal_step "issue ${leaf_lifetime} certificates and start the fleet"
issued_at=$(date -u +%s)
rehearsal_issue_bundle "${controller}" "${node_a}" "${work}/bundle-a" --lifetime "${leaf_lifetime}"
rehearsal_issue_bundle "${controller}" "${node_b}" "${work}/bundle-b" --lifetime "${leaf_lifetime}"
rehearsal_install_bundle "${node_a}" "${work}/bundle-a"
rehearsal_install_bundle "${node_b}" "${work}/bundle-b"

old_authority=$(rehearsal_cert_issuer "${node_a}" /etc/billet/tls/node.crt)
serial_a=$(rehearsal_cert_serial "${node_a}" /etc/billet/tls/node.crt)
serial_b=$(rehearsal_cert_serial "${node_b}" /etc/billet/tls/node.crt)
echo "issued by ${old_authority}: ${node_a} serial ${serial_a}, ${node_b} serial ${serial_b}"

docker exec "${controller}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
docker exec "${node_a}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
docker exec "${node_b}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
for n in "${node_a}" "${node_b}"; do
    rehearsal_wait_for 120 "${n} to register" "${controller}" \
        runuser -u billet -- sh -c "/usr/bin/billet status --config /etc/billet/billet.yaml | grep -q '${n}'" ||
        rehearsal_fail "${n} never appeared in billet status"
done

rehearsal_step "billet ca rotate, then restart the control plane to present the overlap"
rehearsal_as_billet "${controller}" /usr/bin/billet ca show --config /etc/billet/billet.yaml 2>&1 | head -4
rotated_at=$(date -u +%s)
rehearsal_as_billet "${controller}" /usr/bin/billet ca rotate --config /etc/billet/billet.yaml 2>&1 | head -3
docker exec "${controller}" test -f /var/lib/billet/server/ca/ca-previous.key ||
    rehearsal_fail "ca rotate left no committed previous authority (ca-previous.key)"
docker exec "${controller}" systemctl restart billet-server.service
rehearsal_wait_for 60 "the server to be back" "${controller}" systemctl is-active --quiet billet-server.service ||
    rehearsal_fail "billet-server.service did not come back after the restart"
new_authority=$(docker exec "${controller}" openssl x509 -in /var/lib/billet/server/ca/ca.crt -noout -subject | cut -d= -f2-)
echo "new authority: ${new_authority}"
test "${new_authority}" != "${old_authority}" ||
    rehearsal_fail "the authority's subject did not change across the rotation"

rehearsal_step "both nodes renew onto the new authority within the window"
for n in "${node_a}" "${node_b}"; do
    rehearsal_wait_for "${renew_deadline}" "${n} to renew" "${n}" \
        sh -c "openssl x509 -in /etc/billet/tls/node.crt -noout -issuer | grep -qF '${new_authority}'" ||
        rehearsal_fail "${n} did not renew onto the new authority within ${renew_deadline}s of issue"
done
renewed_at=$(date -u +%s)
new_serial_a=$(rehearsal_cert_serial "${node_a}" /etc/billet/tls/node.crt)
new_serial_b=$(rehearsal_cert_serial "${node_b}" /etc/billet/tls/node.crt)
test "${new_serial_a}" != "${serial_a}" || rehearsal_fail "${node_a}'s certificate serial did not change"
test "${new_serial_b}" != "${serial_b}" || rehearsal_fail "${node_b}'s certificate serial did not change"
for n in "${node_a}" "${node_b}"; do
    docker exec "${n}" journalctl -u billet-node --no-pager -o cat 2>/dev/null | grep -F 'renewed this node' | tail -1 || true
done

rehearsal_step "billet ca retire drops the old authority; the fleet keeps polling"
rehearsal_as_billet "${controller}" /usr/bin/billet ca retire --config /etc/billet/billet.yaml 2>&1 | head -4
if docker exec "${controller}" test -f /var/lib/billet/server/ca/ca-previous.crt; then
    rehearsal_fail "ca retire left ca-previous.crt in place"
fi
docker exec "${controller}" systemctl restart billet-server.service
rehearsal_wait_for 60 "the server to be back" "${controller}" systemctl is-active --quiet billet-server.service ||
    rehearsal_fail "billet-server.service did not come back after the retire"

# THE PROOF IS A FRESH REGISTRATION, not a row that survived from before: the
# plane restarted and forgot every host, so a node listed now has connected to a
# plane that trusts only the new authority, with the certificate it renewed.
for n in "${node_a}" "${node_b}"; do
    rehearsal_wait_for 180 "${n} to register with the retired authority gone" "${controller}" \
        runuser -u billet -- sh -c "/usr/bin/billet status --config /etc/billet/billet.yaml | grep -q '${n}'" ||
        rehearsal_fail "${n} did not re-register after the old authority was retired"
done

rehearsal_step "a bundle issued now vouches for the server, and not for the old certificates"
rehearsal_issue_bundle "${controller}" "rehearsal-node-c" "${work}/bundle-c"
docker cp "${work}/bundle-c/ca.crt" "${node_a}:/tmp/ca-now.crt"
docker exec "${node_a}" sh -c 'openssl s_client -connect '"${controller}"':7717 -CAfile /tmp/ca-now.crt -cert /etc/billet/tls/node.crt -key /etc/billet/tls/node.key </dev/null 2>/dev/null | grep -q "Verify return code: 0"' ||
    rehearsal_fail "a bundle issued after the retire does not verify the server ${node_a}'s renewed certificate dials"
if openssl verify -CAfile "${work}/bundle-c/ca.crt" "${work}/bundle-a/node.crt" >/dev/null 2>&1; then
    rehearsal_fail "the ORIGINAL certificate still verifies against the trust bundle after the retire"
fi

echo
echo "ca rotation rehearsal: PASSED"
echo "  package $(rehearsal_version "${controller}") on ${REHEARSAL_ARCH}; leaf ${leaf_lifetime}; both nodes renewed $((renewed_at - issued_at))s after issue ($((renewed_at - rotated_at))s after the rotation); total $(($(date -u +%s) - started_at))s"
