#!/bin/bash
# The promotion rehearsal: an active-passive pair of packaged controllers on one
# PostgreSQL ledger, the active one cut off the network, the standby measured
# taking over, and the old leader proved to stop rather than keep serving.
#
# WHAT THIS PROVES THAT THE TESTS DO NOT. ADR-009's election is a process
# waiting on a PostgreSQL session advisory lock: a controller is dead when its
# session is, decided by the database. The fence and the election are under
# test and the promotion order is asserted structurally; what had never been
# measured is the time a real partition takes to become a promotion, which is
# the database's TCP keepalive settings and nothing billet owns. With
# PostgreSQL's defaults (tcp_keepalives_idle = 2 hours) a standby waits hours;
# this rehearsal runs the server with short keepalives and RECORDS what it
# measured, because the operator has to set them.
#
# THE OLD LEADER IS PROVED TO STOP. When the partition heals its next write is
# refused by the epoch fence, `stopWhenReplaced` ends the process non-zero, and
# systemd restarts it as a standby. Destroying nothing on the way out is the
# whole point of the fence (billet-state): a successor demonstrably exists.
#
#   BILLET_REHEARSAL_APP_CONFIG=... BILLET_REHEARSAL_APP_KEY=... scripts/promotion-rehearsal.sh
set -euo pipefail

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/rehearsal-lib.sh
. "${here}/rehearsal-lib.sh"

rehearsal_require_docker
rehearsal_require_app
rehearsal_require_dist_package "${here}/.."

id=$(date -u +%Y%m%d%H%M%S)
network="billet-rehearsal-${id}"
postgres="rehearsal-postgres"
controller_a="rehearsal-controller-a"
controller_b="rehearsal-controller-b"
controllers="rehearsal-controllers"
node="rehearsal-node"
label="rehearse-${id}-2vcpu"
dsn="postgres://billet:billet@${postgres}:5432/billet?sslmode=disable"
# The keepalives the PostgreSQL server runs with. THESE DECIDE THE PROMOTION
# TIME and they are the operator's to set; the record names them beside the
# measurement so nobody reads the number as billet's.
keepalive_idle=10
keepalive_interval=5
keepalive_count=3
storage=$(mktemp -d)
work=$(mktemp -d)
started_at=$(date -u +%s)

active_controller() {
    # Whichever controller's `billet status` names as holding the claim; the
    # ledger is shared so either host answers the same.
    docker exec -e "BILLET_STATE_DSN=${dsn}" "$1" runuser -u billet -- \
        env "BILLET_STATE_DSN=${dsn}" /usr/bin/billet status --config /etc/billet/billet.yaml 2>/dev/null |
        awk '/^claim / { print $2; exit }'
}

cleanup() {
    status=$?
    set +e

    echo
    echo "=== teardown"
    for c in "${controller_b}" "${controller_a}"; do
        if docker exec "${c}" test -f /etc/billet/billet.yaml >/dev/null 2>&1; then
            docker exec -e "BILLET_STATE_DSN=${dsn}" "${c}" runuser -u billet -- \
                env "BILLET_STATE_DSN=${dsn}" /usr/bin/billet teardown --all --yes \
                --config /etc/billet/billet.yaml 2>&1 | tail -2 && break
        fi
    done

    if [ "${status}" -ne 0 ]; then
        for h in "${controller_a}" "${controller_b}" "${node}"; do
            echo "--- ${h} journal"
            docker exec "${h}" journalctl -u billet-server -u billet-node -n 30 --no-pager -o cat 2>&1 | tail -30 || true
        done
    fi

    rehearsal_teardown_hosts "${network}" "${storage}" "${controller_a}" "${controller_b}" "${node}" "${postgres}"
    rm -rf "${work}" || true
    exit "${status}"
}
trap cleanup EXIT INT TERM

rehearsal_step "a PostgreSQL with short keepalives, two controllers and a node"
docker network create "${network}" >/dev/null
docker run -d --name "${postgres}" --network "${network}" \
    -e POSTGRES_USER=billet -e POSTGRES_PASSWORD=billet -e POSTGRES_DB=billet \
    postgres:18-alpine \
    -c "tcp_keepalives_idle=${keepalive_idle}" \
    -c "tcp_keepalives_interval=${keepalive_interval}" \
    -c "tcp_keepalives_count=${keepalive_count}" >/dev/null
rehearsal_wait_for 90 "PostgreSQL to accept connections" "${postgres}" pg_isready -U billet -d billet ||
    rehearsal_fail "PostgreSQL never came up"

rehearsal_start_host "${controller_a}" "${network}" no "${storage}" "${controllers}"
rehearsal_start_host "${controller_b}" "${network}" no "${storage}" "${controllers}"
rehearsal_start_host "${node}" "${network}" yes "${storage}"
for h in "${controller_a}" "${controller_b}" "${node}"; do
    rehearsal_install_package "${h}" "${REHEARSAL_DIST_DEB}"
done

# THE DSN REACHES THE UNIT THE WAY THE HOST ROLE DELIVERS IT: an environment
# file the unit imports, never a value in billet.yaml, which names only the
# variable (docs/deploying/postgres-and-active-passive.md). The packaged
# billet-server.service carries no EnvironmentFile, so a drop-in adds the one
# the role would render.
for c in "${controller_a}" "${controller_b}"; do
    {
        cat <<EOF
server:
  listen: 0.0.0.0:7717
  node_tls_hosts: [${controllers}, ${c}]
  identity_dir: /var/lib/billet/server
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_STATE_DSN
  controllers: active-passive
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
    } | rehearsal_install_config "${c}"

    docker exec -i "${c}" sh -c 'cat >/tmp/server.env' <<<"BILLET_STATE_DSN=${dsn}"
    docker exec "${c}" install -m 0640 -o root -g billet /tmp/server.env /etc/billet/server.env
    docker exec "${c}" install -d -m 0755 /etc/systemd/system/billet-server.service.d
    docker exec -i "${c}" sh -c 'cat >/etc/systemd/system/billet-server.service.d/10-environment.conf' <<'EOF'
[Service]
EnvironmentFile=-/etc/billet/server.env
EOF
    docker exec "${c}" systemctl daemon-reload
done

rehearsal_install_config "${node}" <<EOF
node:
  server_addr: ${controllers}:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
  tls:
    cert: /etc/billet/tls/node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt
EOF

rehearsal_step "one identity and one authority on both controllers"
# `ca issue` on A mints the identity and the authority into A's identity
# directory; B must hold the SAME ones or it is a rival deployment. In a fleet
# this is `billet ca sync` through an identity store; the rehearsal copies the
# files the documented way and records that as its limitation.
docker exec -e "BILLET_STATE_DSN=${dsn}" "${controller_a}" runuser -u billet -- \
    env "BILLET_STATE_DSN=${dsn}" /usr/bin/billet ca issue "${node}" \
    --config /etc/billet/billet.yaml --out "/tmp/${node}-tls" >/dev/null ||
    rehearsal_fail "controller A could not issue the node's certificate"
docker cp "${controller_a}:/tmp/${node}-tls" "${work}/bundle"
rehearsal_install_bundle "${node}" "${work}/bundle"

docker exec "${controller_a}" tar -C /var/lib/billet/server -cf /tmp/identity.tar deployment-id authority-created ca
docker cp "${controller_a}:/tmp/identity.tar" "${work}/identity.tar"
docker cp "${work}/identity.tar" "${controller_b}:/tmp/identity.tar"
docker exec "${controller_b}" tar -C /var/lib/billet/server -xf /tmp/identity.tar
docker exec "${controller_b}" chown -R billet:billet /var/lib/billet/server
docker exec "${controller_b}" rm -f /tmp/identity.tar
test "$(docker exec "${controller_a}" cat /var/lib/billet/server/deployment-id)" = \
    "$(docker exec "${controller_b}" cat /var/lib/billet/server/deployment-id)" ||
    rehearsal_fail "the two controllers do not share one deployment identity"

rehearsal_step "start A, then B; one claims and one stands by"
docker exec -e "BILLET_STATE_DSN=${dsn}" "${controller_a}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
docker exec -e "BILLET_STATE_DSN=${dsn}" "${controller_b}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
docker exec "${node}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4

rehearsal_wait_for 120 "A to claim the controller" "${controller_a}" \
    sh -c 'journalctl -u billet-server --no-pager -o cat | grep -q "claimed this deployment.s controller"' ||
    rehearsal_fail "controller A never claimed"
rehearsal_wait_for 120 "B to stand by" "${controller_b}" \
    sh -c 'journalctl -u billet-server --no-pager -o cat | grep -q "standing by for this deployment.s controller"' ||
    rehearsal_fail "controller B never stood by"
rehearsal_wait_for 120 "the node to register with A" "${controller_a}" \
    sh -c "BILLET_STATE_DSN='${dsn}' runuser -u billet -- env BILLET_STATE_DSN='${dsn}' /usr/bin/billet status --config /etc/billet/billet.yaml | grep -q '${node}'" ||
    rehearsal_fail "${node} never registered"
echo "claim held by: $(active_controller "${controller_b}")"

rehearsal_step "partition A; measure the promotion"
partitioned_at=$(date -u +%s)
docker network disconnect "${network}" "${controller_a}"
rehearsal_wait_for 300 "B to be promoted" "${controller_b}" \
    sh -c 'journalctl -u billet-server --no-pager -o cat | grep -q "promoted to this deployment.s controller"' ||
    rehearsal_fail "controller B was never promoted after A was partitioned"
promoted_at=$(date -u +%s)
promotion_took=$((promoted_at - partitioned_at))
echo "promotion took ${promotion_took}s (keepalives idle=${keepalive_idle}s interval=${keepalive_interval}s count=${keepalive_count})"
test "$(active_controller "${controller_b}")" = "${controller_b}" ||
    rehearsal_fail "billet status on B does not name B as the claim holder after the promotion"

rehearsal_wait_for 300 "the node to re-register with B" "${controller_b}" \
    sh -c "BILLET_STATE_DSN='${dsn}' runuser -u billet -- env BILLET_STATE_DSN='${dsn}' /usr/bin/billet status --config /etc/billet/billet.yaml | grep -q '${node}'" ||
    rehearsal_fail "${node} did not re-register with the promoted controller"
node_moved_at=$(date -u +%s)

rehearsal_step "heal the partition; the old leader stops and stands by"
restarts_before=$(docker exec "${controller_a}" systemctl show -p NRestarts --value billet-server.service)
docker network connect --alias "${controllers}" "${network}" "${controller_a}"
healed_at=$(date -u +%s)
rehearsal_wait_for 300 "A to notice it was replaced" "${controller_a}" \
    sh -c 'journalctl -u billet-server --no-pager -o cat | grep -q "no longer this deployment.s controller"' ||
    rehearsal_fail "controller A never noticed it had been replaced"
rehearsal_wait_for 120 "A to be restarted by systemd" "${controller_a}" \
    sh -c "test \"\$(systemctl show -p NRestarts --value billet-server.service)\" -gt ${restarts_before}" ||
    rehearsal_fail "systemd did not restart controller A after it stopped"
rehearsal_wait_for 120 "A to stand by" "${controller_a}" \
    sh -c 'journalctl -u billet-server --no-pager -o cat | grep -c "standing by for this deployment.s controller" | grep -qvE "^0$"' ||
    rehearsal_fail "controller A did not come back as a standby"
stood_by_at=$(date -u +%s)
test "$(active_controller "${controller_a}")" = "${controller_b}" ||
    rehearsal_fail "after healing, billet status on A does not name B as the claim holder"
test "$(rehearsal_active "${controller_b}" billet-server.service)" = active ||
    rehearsal_fail "controller B is not active after A rejoined"

echo
echo "promotion rehearsal: PASSED"
echo "  package $(rehearsal_version "${controller_a}") on ${REHEARSAL_ARCH}; PostgreSQL keepalives idle=${keepalive_idle}s interval=${keepalive_interval}s count=${keepalive_count}; partition -> promotion ${promotion_took}s; node re-registered $((node_moved_at - partitioned_at))s after the partition; heal -> old leader standing by $((stood_by_at - healed_at))s; total $(($(date -u +%s) - started_at))s"
