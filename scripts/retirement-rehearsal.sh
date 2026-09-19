#!/bin/bash
# Real retained-node retirement, with the tree's package under real systemd.
# Every input report comes from these hosts after reservation, never a fixture.
# A refusal or an unreadable observation ends the run and preserves diagnostics.
#
# BILLET_REHEARSAL_APP_CONFIG=... BILLET_REHEARSAL_APP_KEY=... scripts/retirement-rehearsal.sh
set -Eeuo pipefail
umask 077

here=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
# shellcheck source=scripts/rehearsal-lib.sh
. "${here}/rehearsal-lib.sh"

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) prerequisites"
rehearsal_require_app
# The shared helper fails on unsupported architectures; this rehearsal skips.
if command -v docker >/dev/null 2>&1; then
    arch=$(docker version --format '{{.Server.Arch}}') ||
        rehearsal_fail "expected Docker architecture; daemon did not answer"
    case "${arch}" in
        amd64 | arm64) ;;
        *) echo "retirement rehearsal: SKIPPED; no package for Docker architecture ${arch}"; exit 0 ;;
    esac
fi
rehearsal_require_docker
command -v jq >/dev/null 2>&1 || rehearsal_fail "expected jq for real report collection; jq is not installed"
rehearsal_require_dist_package "${here}/.."

id=$(date -u +%Y%m%d%H%M%S)
network="billet-rehearsal-${id}"
postgres="rehearsal-postgres"
controller_a="rehearsal-controller-a"
controller_b="rehearsal-controller-b"
controllers="rehearsal-controllers"
node="rehearsal-node"
label="rehearse-retire-${id}-2vcpu"
run="retirement-${id}"
dsn="postgres://billet:billet@${postgres}:5432/billet?sslmode=disable"
storage=
work=
started_at=$(date -u +%s)

active_controller() {
    # Whichever controller's `billet status` names as holding the claim; the
    # ledger is shared so either host answers the same.
    docker exec -e "BILLET_STATE_DSN=${dsn}" "$1" runuser -u billet -- \
        env "BILLET_STATE_DSN=${dsn}" /usr/bin/billet status --config /etc/billet/billet.yaml 2>/dev/null |
        awk '/^claim / && !seen { print $2; seen = 1 }'
}

cleanup() {
    status=$?
    set +e
    trap - ERR
    # IGNORED, NOT RESET: a second Ctrl-C or a TERM during the teardown must not
    # end the shell before the scale set and the hosts are gone. The status the
    # first signal chose (130 or 143) is already in ${status}.
    trap '' INT TERM
    status=$(rehearsal_verdict "${status}")

    echo
    rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) teardown"
    # Abandon only through the command: it refuses a transition already begun.
    # BEFORE THE SERVERS STOP, because this one does open the ledger.
    if [ "${reservation_attempted}" = yes ] && [ "${REHEARSAL_PASSED}" != 1 ]; then
        docker exec "${controller_a}" /usr/bin/billet server retire --abandon-reservation \
            --json --run "${run}" --retiring-host "${controller_a}" \
            --config /etc/billet/billet.yaml --environment-file /etc/billet/server.env
    fi
    # NO LISTENER MAY CREATE A SCALE SET AFTER THE TEARDOWN DELETED IT. An
    # interrupt during startup leaves a controller mid-creation, and a set
    # created after the delete is the leak this trap exists to prevent, so both
    # servers stop first. `billet teardown` reads the config and GitHub and
    # opens no ledger (cmdTeardown), so it needs neither of them running.
    for c in "${controller_a}" "${controller_b}"; do
        docker container inspect "${c}" >/dev/null 2>&1 || continue
        docker exec "${c}" systemctl stop billet-server.service >/dev/null 2>&1 ||
            echo "TEARDOWN: could not stop ${c}'s server; a listener there may still create a scale set" >&2
    done
    torn_down=no
    for c in "${controller_b}" "${controller_a}"; do
        if docker exec "${c}" test -f /etc/billet/billet.yaml >/dev/null 2>&1; then
            if rehearsal_teardown_scale_sets "${c}" "BILLET_STATE_DSN=${dsn}"; then
                torn_down=yes
                break
            fi
        fi
    done
    if [ "${plane_started}" = yes ] && [ "${torn_down}" = no ]; then
        echo "TEARDOWN FAILED: the scale set for ${label} may still exist. Remove it from any host" >&2
        echo "holding this App: billet teardown --tier ${label} --yes --config <that config>" >&2
        if [ "${status}" -eq 0 ]; then status=1; fi
    fi

    if [ "${status}" -ne 0 ]; then
        for evidence in "${work}"/*.json; do
            if [ -n "${work}" ] && [ -f "${evidence}" ]; then
                echo "--- $(basename "${evidence}")"
                # The input carries configuration; report only command answers.
                case "${evidence}" in */input.json | */continuation.json) continue ;; esac
                cat "${evidence}"
            fi
        done
        docker exec "${controller_a}" cat /var/lib/billet/retired/journal.json

        for h in "${controller_a}" "${controller_b}" "${node}"; do
            echo "--- ${h} journal"
            docker exec "${h}" journalctl -u billet-server -u billet-node -n 30 --no-pager -o cat 2>&1 | tail -30 || true
        done
    fi

    if ! rehearsal_teardown_hosts "${network}" "${storage}" "${controller_a}" "${controller_b}" "${node}" "${postgres}"; then
        echo "TEARDOWN FAILED: what is named above is still on this machine" >&2
        if [ "${status}" -eq 0 ]; then status=1; fi
    fi
    rm -rf "${work}" || true
    exit "${status}"
}
# THE SENTINEL STARTS AT 0 HERE, whatever the environment says, or an exported
# REHEARSAL_PASSED=1 would turn an aborted run green. A signal exits through its
# own status so that cleanup, which only the EXIT trap runs, reads a failure and
# not the $? of whatever the signal interrupted.
REHEARSAL_PASSED=0
plane_started=no
reservation_attempted=no
# THE CONTAINER NAMES ARE FIXED AND THE TEARDOWN REMOVES THEM BY NAME, so a
# name already taken is refused HERE, before any trap is installed: starting
# anyway would tear down whatever holds it — another rehearsal's hosts, mid-run
# — on the way out. Nothing has been created at this point, so the refusal
# removes nothing.
for c in "${postgres}" "${controller_a}" "${controller_b}" "${node}"; do
    if docker container inspect "${c}" >/dev/null 2>&1; then
        rehearsal_fail "expected no container named ${c}; one exists. Another rehearsal may be running on this machine; if it is not, remove it with: docker rm -f -v ${c}"
    fi
done
trap 'exit 130' INT
trap 'exit 143' TERM
trap cleanup EXIT
trap 'rehearsal_fail "expected command at line ${LINENO} to succeed; observed exit $?"' ERR
storage=$(mktemp -d)
work=$(mktemp -d)

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) PostgreSQL, two controllers, and two nodes (A retains its node)"
docker network create "${network}" >/dev/null
docker run -d --name "${postgres}" --network "${network}" \
    -e POSTGRES_USER=billet -e POSTGRES_PASSWORD=billet -e POSTGRES_DB=billet \
    postgres:18-alpine >/dev/null
rehearsal_wait_for 90 "PostgreSQL to accept connections" "${postgres}" pg_isready -U billet -d billet ||
    rehearsal_fail "PostgreSQL never came up"

rehearsal_start_host "${controller_a}" "${network}" yes "${storage}" "${controllers}"
rehearsal_start_host "${controller_b}" "${network}" no "${storage}" "${controllers}"
rehearsal_start_host "${node}" "${network}" yes "${storage}"
a_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${controller_a}")
b_ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' "${controller_b}")
test -n "${a_ip}" && test -n "${b_ip}" && test "${a_ip}" != "${b_ip}" ||
    rehearsal_fail "expected two distinct controller addresses; observed A=${a_ip} B=${b_ip}"
for h in "${controller_a}" "${controller_b}" "${node}"; do
    rehearsal_install_package "${h}" "${REHEARSAL_DIST_DEB}"
done
rehearsal_install_app_key "${controller_a}"
rehearsal_install_app_key "${controller_b}"

node_config() {
    cat <<EOF
node:
  server_addr: ${a_ip}:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
  tls:
    cert: /etc/billet/tls/node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt
EOF
}

# THE DSN REACHES THE UNIT THE WAY THE HOST ROLE DELIVERS IT: an environment
# file the unit imports, never a value in billet.yaml, which names only the
# variable (docs/deploying/postgres-and-active-passive.md). The packaged
# billet-server.service carries no EnvironmentFile, so the rehearsal renders a
# whole unit into /etc carrying the one the role would render (below).
for c in "${controller_a}" "${controller_b}"; do
    {
        cat <<EOF
server:
  listen: 0.0.0.0:7717
  node_tls_hosts: [${controllers}, ${c}, ${a_ip}, ${b_ip}]
  identity_dir: /var/lib/billet/server
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_STATE_DSN
  controllers: active-passive
  max_vcpu: 8
  max_memory: 16GiB
# A snapshot must not roll itself onto the published channel.
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
        if [ "${c}" = "${controller_a}" ]; then
            node_config
        fi
    } | rehearsal_install_config "${c}"

    docker exec -i "${c}" sh -c 'cat >/tmp/server.env' <<<"BILLET_STATE_DSN=${dsn}"
    docker exec "${c}" install -m 0640 -o root -g billet /tmp/server.env /etc/billet/server.env
    # A WHOLE UNIT, NOT A DROP-IN. `billet local up` and the host transaction
    # refuse a billet unit with an effective drop-in, because a drop-in can
    # replace ExecStart, the account or the readiness protocol and neither can
    # start what it has not accounted for; the third promotion run measured the
    # refusal ("billet-server.service has drop-in overrides", 2026-09-04). The
    # host role renders its own unit carrying EnvironmentFile= for exactly this
    # reason (docs/deploying/postgres-and-active-passive.md), and the rehearsal
    # does the same from the packaged unit, in /etc where it shadows /usr/lib.
    docker exec "${c}" sh -c 'sed "/^\[Service\]/a EnvironmentFile=-/etc/billet/server.env" /usr/lib/systemd/system/billet-server.service >/etc/systemd/system/billet-server.service'
    docker exec "${c}" grep -q '^EnvironmentFile=-/etc/billet/server.env$' /etc/systemd/system/billet-server.service ||
        rehearsal_fail "the rendered unit on ${c} does not carry the EnvironmentFile line"
    docker exec "${c}" rm -rf /etc/systemd/system/billet-server.service.d
    docker exec "${c}" systemctl daemon-reload
done

node_config | rehearsal_install_config "${node}"

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) one identity and one authority on both controllers"
# `ca issue` on A mints the identity and the authority into A's identity
# directory; B must hold the SAME ones or it is a rival deployment. In a fleet
# this is `billet ca sync` through an identity store; the rehearsal copies the
# files the documented way and records that as its limitation.
for h in "${controller_a}" "${node}"; do
    rehearsal_as_billet "${controller_a}" env "BILLET_STATE_DSN=${dsn}" \
        /usr/bin/billet ca issue "${h}" --config /etc/billet/billet.yaml --out "/tmp/${h}-tls" >/dev/null ||
        rehearsal_fail "expected certificate for ${h}; A could not issue it"
    docker cp "${controller_a}:/tmp/${h}-tls" "${work}/${h}-tls"
    rehearsal_install_bundle "${h}" "${work}/${h}-tls"
    docker exec "${controller_a}" rm -rf "/tmp/${h}-tls"
    rm -rf "${work}/${h}-tls"
done

# THE ARCHIVE HOLDS THE CA KEY, so it is created under a private umask, its
# mode is checked, and every copy is removed the moment it has been read.
docker exec "${controller_a}" sh -c 'umask 077 && tar -C /var/lib/billet/server -cf /tmp/identity.tar deployment-id authority-created ca'
test "$(docker exec "${controller_a}" stat -c '%a' /tmp/identity.tar)" = 600 ||
    rehearsal_fail "the identity archive is not mode 0600; it holds the authority's key"
docker cp "${controller_a}:/tmp/identity.tar" "${work}/identity.tar"
docker exec "${controller_a}" rm -f /tmp/identity.tar
docker cp "${work}/identity.tar" "${controller_b}:/tmp/identity.tar"
rm -f "${work}/identity.tar"
# B HAS NO IDENTITY DIRECTORY YET: only A ran `ca issue`, and the service
# account's directory is what systemd's StateDirectory= would create on first
# start. It is created here as that account, 0700, so root's extraction lands in
# a directory the server can open (the first two promotion runs failed on this
# line with "Cannot open: No such file or directory", 2026-09-04).
docker exec "${controller_b}" install -d -m 0700 -o billet -g billet /var/lib/billet/server
docker exec "${controller_b}" tar -C /var/lib/billet/server -xf /tmp/identity.tar
docker exec "${controller_b}" rm -f /tmp/identity.tar
docker exec "${controller_b}" chown -R billet:billet /var/lib/billet/server
identity_a=$(docker exec "${controller_a}" cat /var/lib/billet/server/deployment-id) ||
    rehearsal_fail "expected A's deployment identity; could not read it"
identity_b=$(docker exec "${controller_b}" cat /var/lib/billet/server/deployment-id) ||
    rehearsal_fail "expected B's deployment identity; could not read it"
test -n "${identity_a}" && test "${identity_a}" = "${identity_b}" ||
    rehearsal_fail "expected one deployment identity; observed A=${identity_a} B=${identity_b}"

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) start A, then B; one claims and one stands by"
since_a=$(rehearsal_clock "${controller_a}")
plane_started=yes
docker exec -e "BILLET_STATE_DSN=${dsn}" "${controller_a}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
docker exec -e "BILLET_STATE_DSN=${dsn}" "${controller_b}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
docker exec "${node}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4

rehearsal_wait_for 120 "A to claim the controller" "${controller_a}" \
    bash -c 'set -o pipefail; journalctl -u billet-server --no-pager -o cat | grep -Eq "(claimed|promoted to) this deployment.s controller"' ||
    rehearsal_fail "controller A never claimed"
rehearsal_wait_for 120 "B to stand by" "${controller_b}" \
    bash -c 'set -o pipefail; journalctl -u billet-server --no-pager -o cat | grep -q "standing by for this deployment.s controller"' ||
    rehearsal_fail "controller B never stood by"
rehearsal_wait_registered 120 "${controller_a}" "${node}" "${since_a}" ||
    rehearsal_fail "${node} never registered with A"
# ASSERTED, NOT PRINTED: everything measured below is relative to A holding
# the claim now, and a run where B had it would measure nothing.
test "$(active_controller "${controller_b}")" = "${controller_a}" ||
    rehearsal_fail "billet status does not name A as the claim holder before the handoff"
rehearsal_wait_registered 120 "${controller_a}" "${controller_a}" "${since_a}" ||
    rehearsal_fail "expected A's retained node registered through A; no post-start registration observed"
for h in "${controller_a}" "${controller_b}"; do
    observed=$(rehearsal_active "${h}" billet-server.service)
    test "${observed}" = active || rehearsal_fail "expected ${h} server active; observed ${observed}"
done
for h in "${controller_a}" "${node}"; do
    observed=$(rehearsal_active "${h}" billet-node.service)
    test "${observed}" = active || rehearsal_fail "expected ${h} node active; observed ${observed}"
done
for unit in billet-server.service billet-backup.timer billet-upgrade.timer; do
    observed=$(rehearsal_active "${controller_a}" "${unit}")
    enabled=$(docker exec "${controller_a}" systemctl show -p UnitFileState --value "${unit}")
    test "${observed}" = active && test "${enabled}" = enabled ||
        rehearsal_fail "expected ${unit} active and enabled before retirement; observed ${observed}/${enabled}"
done
echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) claim held by ${controller_a}; both nodes registered"

# Capture the producer's status before judging its document. Unknown is never
# absence, and an exit-zero report still has to satisfy the requested facts.
json_command() {
    local out=$1 host=$2 code=0
    shift 2
    docker exec -i "${host}" /usr/bin/billet "$@" >"${out}" || code=$?
    if [ "${code}" -ne 0 ]; then
        cat "${out}" >&2
        rehearsal_fail "expected ${host} $1 $2 to answer successfully; observed exit ${code} (see answer above)"
    fi
    jq -e 'type == "object"' "${out}" >/dev/null ||
        rehearsal_fail "expected ${host} $1 $2 JSON object; observed invalid or missing document"
}

require_json() {
    local file=$1 filter=$2 expected=$3
    shift 3
    if ! jq -e "$@" "${filter}" "${file}" >/dev/null; then
        cat "${file}" >&2
        rehearsal_fail "expected ${expected}; observed ${file##*/} above"
    fi
}

inspect_host() {
    json_command "$2" "$1" release inspect --json --config /etc/billet/billet.yaml
}

prove_endpoint() {
    local host=$1 endpoint=$2 file=$3
    inspect_host "${host}" "${file}"
    require_json "${file}" '
        .config_binding == true and .installed_config.has_node == true and
        .services.node.active_state == "active" and .services.node.same_as_executable == true and
        .host.installed_endpoint == $ep and .host.registration.endpoint == $ep and
        .host.registration.node == $host and
        .services.node.config_changed_since_start == false and
        (.host.registration.deployment | type == "string" and length > 0) and
        (.host.registration.incarnation | type == "string" and test("^[0-9a-f]{32}$")) and
        (.services.node.invocation_id | type == "string" and test("^[0-9a-f]{32}$")) and
        .host.registration.invocation_id == .services.node.invocation_id
    ' "${host} running and registered at ${endpoint} with its installed configuration bound" --arg ep "${endpoint}" --arg host "${host}"
}

prove_receipt() {
    local host=$1 file=$2
    require_json "${file}" '
        .host.endpoint_receipt.presence == "present" and
        (.host.endpoint_receipt.receipt as $r | .host.registration as $n |
         ($r.installed_sha256 | type == "string" and test("^[0-9a-f]{64}$")) and
         $r.installed_sha256 == .installed_config.sha256 and
         $r.installed_endpoint == .host.installed_endpoint and
         $r.effective_endpoint == $n.endpoint and
         $r.invocation_id == $n.invocation_id and $r.incarnation == $n.incarnation and
         $r.node == $n.node and $r.deployment == $n.deployment)
    ' "${host} current receipt, configuration and runtime registration to agree"
}

# THE PROCESS THAT RESTARTED, NOT THE FILE THAT CHANGED. Retention is not a
# promise of uninterrupted node uptime: the transition stops and starts the node
# on the serverless configuration before done, and nothing above would notice a
# retirement that installed the new bytes, refreshed the receipt and left the old
# process running on the configuration it read at start. The systemd invocation
# is that process's identity, so a new one is the restart itself; an unreadable
# invocation on either side is could-not-tell and fails here.
node_invocation() {
    local file=$1 when=$2
    require_json "${file}" '.services.node.invocation_id | type == "string" and test("^[0-9a-f]{32}$")' \
        "a readable node invocation ${when}"
    jq -er '.services.node.invocation_id' "${file}"
}

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) prove the initial installed and effective endpoints"
for h in "${controller_a}" "${node}"; do
    prove_endpoint "${h}" "https://${a_ip}:7717" "${work}/${h}-initial.json"
done

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) hand leadership to B before migrating endpoints"
# A standby waits before opening its node listener (becomeController). Move the
# claim first, then return A as a running standby; retirement still stops A's
# real running server. Never stop A's node to perform this leadership handoff.
since_b=$(rehearsal_clock "${controller_b}")
docker exec "${controller_a}" systemctl stop billet-server.service
rehearsal_stopped "${controller_a}" billet-server.service ||
    rehearsal_fail "expected A stopped for leadership handoff; observed $(rehearsal_active "${controller_a}" billet-server.service)"
rehearsal_wait_for 180 "B to claim leadership" "${controller_b}" \
    bash -c "set -o pipefail; journalctl -u billet-server --since '${since_b}' --no-pager -o cat | grep -q 'promoted to this deployment.s controller'" ||
    rehearsal_fail "expected B promoted after A stopped; no promotion observed"
since_standby=$(rehearsal_clock "${controller_a}")
docker exec "${controller_a}" systemctl start billet-server.service
rehearsal_wait_for 120 "A to stand by after handoff" "${controller_a}" \
    bash -c "set -o pipefail; journalctl -u billet-server --since '${since_standby}' --no-pager -o cat | grep -q 'standing by for this deployment.s controller'" ||
    rehearsal_fail "expected A running as standby after handoff; no standby observation"
observed=$(active_controller "${controller_b}")
test "${observed}" = "${controller_b}" || rehearsal_fail "expected claim held by B; observed ${observed}"

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) prepare and settle this run's guards"
for h in "${controller_a}" "${controller_b}" "${node}"; do
    json_command "${work}/${h}-prepare.json" "${h}" converge-guard prepare --validate --holder "${run}" --json
    require_json "${work}/${h}-prepare.json" \
        '.holder == $run and .preparing == true and .pointer == false and (.token | type == "string" and length == 32)' \
        "a new preparing pointer-free guard for ${h}" --arg run "${run}"
    token=$(jq -er '.token' "${work}/${h}-prepare.json")
    guard_id=$(jq -er '.id' "${work}/${h}-prepare.json")
    json_command "${work}/${h}-no-change.json" "${h}" converge-guard prepare --no-change --holder "${run}" --token "${token}" --expect-id "${guard_id}" --json
    require_json "${work}/${h}-no-change.json" \
        '.outcome == "validated" and .id == $id and .pointer == false and .preparing == true' \
        "the same prepared guard with no binary change" --arg id "${guard_id}"
    json_command "${work}/${h}-settle.json" "${h}" converge-guard settle --holder "${run}" --token "${token}" --json
    require_json "${work}/${h}-settle.json" '.outcome == "settled" and .holder == $run' "${h}'s settled guard" --arg run "${run}"
done

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) migrate both nodes to B and write confirmed receipts"
for h in "${controller_a}" "${node}"; do
    docker cp "${h}:/etc/billet/billet.yaml" "${work}/${h}-before.yaml"
    # The rehearsal generated this block-style node section. Only its endpoint
    # changes; the migration command requires the desired bytes installed first.
    sed "s/server_addr: ${a_ip}:7717/server_addr: ${b_ip}:7717/" \
        "${work}/${h}-before.yaml" >"${work}/${h}-desired.yaml"
    json_command "${work}/${h}-decision.json" "${h}" node migrate-endpoint \
        --config /etc/billet/billet.yaml --desired - --dry-run --wait 30s --json <"${work}/${h}-desired.yaml"
    require_json "${work}/${h}-decision.json" \
        '.outcome == "reported" and .planned == true and .to == $ep and .node_removed == false' \
        "a planned endpoint move to B" --arg ep "https://${b_ip}:7717"
    rehearsal_install_config "${h}" <"${work}/${h}-desired.yaml"
    since_migration=$(rehearsal_clock "${controller_b}")
    json_command "${work}/${h}-migration.json" "${h}" node migrate-endpoint \
        --config /etc/billet/billet.yaml --desired - --stop-timeout 120s --wait 120s --json <"${work}/${h}-desired.yaml"
    require_json "${work}/${h}-migration.json" '.outcome == "migrated" and .to == $ep' \
        "${h} migrated to B" --arg ep "https://${b_ip}:7717"
    rehearsal_wait_registered 120 "${controller_b}" "${h}" "${since_migration}" ||
        rehearsal_fail "expected ${h} registration on B after migration; none observed"
    incarnation=$(jq -er '.incarnation' "${work}/${h}-migration.json")
    json_command "${work}/${h}-confirmation.json" "${controller_b}" rollout registration \
        --node "${h}" --incarnation "${incarnation}" --wait 120s --json \
        --config /etc/billet/billet.yaml --environment-file /etc/billet/server.env
    require_json "${work}/${h}-confirmation.json" \
        '.outcome == "confirmed" and .live == true and .incarnation == $inc' \
        "B to confirm ${h}'s live incarnation" --arg inc "${incarnation}"
    docker cp "${work}/${h}-migration.json" "${h}:/tmp/migration.json"
    docker cp "${work}/${h}-confirmation.json" "${h}:/tmp/confirmation.json"
    json_command "${work}/${h}-receipt.json" "${h}" node receipt \
        --evidence /tmp/migration.json --confirmation /tmp/confirmation.json \
        --config /etc/billet/billet.yaml --run "${run}" --json
    require_json "${work}/${h}-receipt.json" '.outcome == "written" or .outcome == "current"' "a durable endpoint receipt"
    docker exec "${h}" rm -f /tmp/migration.json /tmp/confirmation.json
    prove_endpoint "${h}" "https://${b_ip}:7717" "${work}/${h}-moved.json"
    prove_receipt "${h}" "${work}/${h}-moved.json"
done

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) reserve before collecting any retirement evidence"
installed_sha=$(rehearsal_sha256_of "${work}/${controller_a}-desired.yaml")
reservation_attempted=yes
json_command "${work}/reservation.json" "${controller_a}" server retire --reserve --json \
    --run "${run}" --retiring-host "${controller_a}" --survivor-host "${controller_b}" \
    --installed-sha256 "${installed_sha}" --config /etc/billet/billet.yaml --environment-file /etc/billet/server.env
require_json "${work}/reservation.json" \
    '.outcome == "reserved" and .run == $run and .retiring == $a and .survivor == $b and (.transition_id | test("^[0-9a-f]{32}$"))' \
    "a fresh reservation bound to this run and pair" --arg run "${run}" --arg a "${controller_a}" --arg b "${controller_b}"
transition=$(jq -er '.transition_id' "${work}/reservation.json")

# Remove complete top-level blocks from the installed document generated here.
# Preserve all retained bytes, including the final newline, for the exact digest.
awk '
    /^[^ #]/ { omit = ($0 ~ /^(server|github|targets|backup):/) }
    !omit { print }
' "${work}/${controller_a}-desired.yaml" >"${work}/serverless.yaml"
round_started=$(docker exec "${controller_a}" date -u '+%Y-%m-%dT%H:%M:%S.%NZ')
for h in "${controller_a}" "${controller_b}" "${node}"; do
    inspect_host "${h}" "${work}/${h}-inspect.json"
    require_json "${work}/${h}-inspect.json" '(.installed_config.has_server | type) == "boolean"' "known ledger presence on ${h}"
    has_server=$(jq -r '.installed_config.has_server' "${work}/${h}-inspect.json")
    if [ "${has_server}" = true ]; then
        require_json "${work}/${h}-inspect.json" \
            '.services.server.environment_files == ["/etc/billet/server.env"]' "the installed server environment file on ${h}"
        json_command "${work}/${h}-status.json" "${h}" rollout status --json \
            --config /etc/billet/billet.yaml --environment-file /etc/billet/server.env
    else
        test "${h}" = "${node}" || rehearsal_fail "expected controller ${h} with a ledger; observed has_server=false"
        printf 'null\n' >"${work}/${h}-status.json"
    fi
    if [ "${h}" != "${controller_b}" ]; then
        # A's desired-node digest is its FULL pre-transition config. The separate
        # desired member is the serverless config the retirement will install.
        json_command "${work}/${h}-endpoint.json" "${h}" node migrate-endpoint \
            --config /etc/billet/billet.yaml --desired - --dry-run --wait 30s --json <"${work}/${h}-desired.yaml"
        require_json "${work}/${h}-endpoint.json" \
            '.outcome == "reported" and .node_removed == false and .planned == false and .to == $ep' \
            "${h}'s desired endpoint already installed and effective at B" --arg ep "https://${b_ip}:7717"
        desired_sha=$(rehearsal_sha256_of "${work}/${h}-desired.yaml")
        jq --arg sha "${desired_sha}" '{sha256:$sha, endpoint:.to}' \
            "${work}/${h}-endpoint.json" >"${work}/${h}-desired-node.json"
    fi
    collected=$(docker exec "${controller_a}" date -u '+%Y-%m-%dT%H:%M:%S.%NZ')
    jq -n --arg host "${h}" --arg at "${collected}" \
        --slurpfile inspect "${work}/${h}-inspect.json" --slurpfile status "${work}/${h}-status.json" \
        '{host:$host, collected_at:$at, inspect:$inspect[0], status:$status[0]}' >"${work}/${h}-envelope.json"
done
jq -n --arg run "${run}" --arg start "${round_started}" \
    --slurpfile a "${work}/${controller_a}-envelope.json" --slurpfile b "${work}/${controller_b}-envelope.json" \
    --slurpfile n "${work}/${node}-envelope.json" --rawfile desired "${work}/serverless.yaml" \
    --slurpfile ad "${work}/${controller_a}-desired-node.json" --slurpfile nd "${work}/${node}-desired-node.json" \
    '{schema:1, round:{id:($run+"-"+$start), started_at:$start}, self:$a[0], survivor:$b[0],
      nodes:{($a[0].host):$a[0], ($n[0].host):$n[0]}, desired:$desired,
      desired_nodes:{($a[0].host):$ad[0], ($n[0].host):$nd[0]}}' >"${work}/input.json"

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) request retained-node retirement from real inspections"
retained_before=$(node_invocation "${work}/${controller_a}-inspect.json" "before the retirement")
since_retire=$(rehearsal_clock "${controller_b}")
json_command "${work}/retirement.json" "${controller_a}" server retire --input - --json \
    --run "${run}" --retiring-host "${controller_a}" --survivor-host "${controller_b}" \
    --installed-sha256 "${installed_sha}" --reservation-fresh \
    --config /etc/billet/billet.yaml --environment-file /etc/billet/server.env <"${work}/input.json"
require_json "${work}/retirement.json" \
    '.outcome == "retired" and .variant == "retained-node" and .state == "done" and .transition_id == $id and
     (.receipt == "written" or .receipt == "current") and (.row == "done" or .row == "already" or .row == "pending")' \
    "retained-node retirement done with a receipt and a known row outcome" --arg id "${transition}"
require_json "${work}/retirement.json" \
    '.completion as $c | $r[0] as $r | $c.deployment == $r.deployment and $c.retiring == $r.retiring and
     $c.survivor == $r.survivor and $c.transition_id == $r.transition_id and $c.reservation == $r.reserved_at' \
    "completion bound to the reservation" --slurpfile r "${work}/reservation.json"
cat "${work}/retirement.json"
row=$(jq -er '.row' "${work}/retirement.json")
if [ "${row}" = pending ]; then
    rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) complete the pending row on B, acknowledge on A, settle locally"
    jq '.completion' "${work}/retirement.json" >"${work}/completion.json"
    json_command "${work}/completed.json" "${controller_b}" server retire --complete-row --json \
        --run "${run}" --as-host "${controller_b}" --completion - \
        --config /etc/billet/billet.yaml --environment-file /etc/billet/server.env <"${work}/completion.json"
    require_json "${work}/completed.json" \
        '(.row == "done" or .row == "already") and .transition_id == $id' \
        "B's row completion for this transition" --arg id "${transition}"
    require_json "${work}/completed.json" \
        '. as $a | $c[0] as $c | $a.deployment == $c.deployment and $a.retiring == $c.retiring and
         $a.survivor == $c.survivor and $a.transition_id == $c.transition_id and $a.reservation == $c.reservation' \
        "survivor answer bound to this completion" --slurpfile c "${work}/completion.json"
    json_command "${work}/acknowledged.json" "${controller_a}" server retire --acknowledge-row --json \
        --run "${run}" --retiring-host "${controller_a}" --answer - --config /etc/billet/billet.yaml <"${work}/completed.json"
    require_json "${work}/acknowledged.json" \
        '(.outcome == "acknowledged" or .outcome == "already") and .transition_id == $id' \
        "A's acknowledgement of this transition" --arg id "${transition}"
    jq '{schema:1, round:.round, self:(.self + {status:null}), survivor:null, nodes:{}, desired:null}' \
        "${work}/input.json" >"${work}/continuation.json"
    json_command "${work}/retirement.json" "${controller_a}" server retire --input - --json \
        --run "${run}" --retiring-host "${controller_a}" --survivor-host "${controller_b}" \
        --config /etc/billet/billet.yaml --environment-file /etc/billet/server.env <"${work}/continuation.json"
fi
require_json "${work}/retirement.json" \
    '(.outcome == "retired" or .outcome == "unchanged") and .state == "done" and
     .variant == "retained-node" and .settled == true and .transition_id == $id' \
    "settled retained-node retirement" --arg id "${transition}"

prove_controller_quiet() {
    local unit observed enabled
    for unit in billet-server.service billet-backup.timer billet-upgrade.timer; do
        observed=$(rehearsal_active "${controller_a}" "${unit}")
        test "${observed}" = inactive || rehearsal_fail "expected ${unit} inactive on A; observed ${observed}"
        enabled=$(docker exec "${controller_a}" systemctl show -p UnitFileState --value "${unit}") ||
            rehearsal_fail "expected ${unit} enablement observation; could not read it"
        test "${enabled}" = disabled || rehearsal_fail "expected ${unit} disabled on A; observed ${enabled}"
    done
}

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) prove retirement's files, services, registration, row and journal"
prove_controller_quiet
docker cp "${controller_a}:/var/lib/billet/retired/journal.json" "${work}/journal.json"
require_json "${work}/journal.json" \
    '.phase == "done" and .variant == "retained-node" and .row_done == true and .settled == true and
     .provenance.transition_id == $id and .identity_dir == "/var/lib/billet/server" and
     (.archive | startswith("/var/lib/billet/retired/"))' \
    "done, row-done, settled journal and archived identity locator" --arg id "${transition}"
archive=$(jq -er '.archive' "${work}/journal.json")
# A successful enumeration, followed by a negative match, distinguishes a
# missing name from a failed filesystem observation.
entries=$(docker exec "${controller_a}" ls -A /var/lib/billet) || rehearsal_fail "could not examine A's state directory"
case $'\n'"${entries}"$'\n' in
    *$'\nserver\n'*) rehearsal_fail "expected original identity directory absent; observed server entry" ;;
esac
archived_id=$(docker exec "${controller_a}" cat "${archive}/deployment-id") || rehearsal_fail "expected archived identity; could not read it"
survivor_id=$(docker exec "${controller_b}" cat /var/lib/billet/server/deployment-id) || rehearsal_fail "could not read B's identity"
test -n "${archived_id}" && test "${archived_id}" = "${survivor_id}" ||
    rehearsal_fail "expected archived identity equal to survivor; observed ${archived_id} versus ${survivor_id}"
docker cp "${controller_a}:/etc/billet/billet.yaml" "${work}/installed-after.yaml"
cmp "${work}/serverless.yaml" "${work}/installed-after.yaml" ||
    rehearsal_fail "expected exact serverless rendering installed; observed different bytes"
rehearsal_wait_registered 120 "${controller_b}" "${controller_a}" "${since_retire}" ||
    rehearsal_fail "expected retained node registered on B after retirement restart; no registration observed"
for h in "${controller_a}" "${node}"; do
    prove_endpoint "${h}" "https://${b_ip}:7717" "${work}/${h}-after.json"
    prove_receipt "${h}" "${work}/${h}-after.json"
    incarnation=$(jq -er '.host.registration.incarnation' "${work}/${h}-after.json")
    json_command "${work}/${h}-live.json" "${controller_b}" rollout registration \
        --node "${h}" --incarnation "${incarnation}" --wait 120s --json \
        --config /etc/billet/billet.yaml --environment-file /etc/billet/server.env
    require_json "${work}/${h}-live.json" '.outcome == "confirmed" and .live == true' "${h} still live on B"
done
require_json "${work}/${controller_a}-after.json" \
    '.installed_config.has_server == false and .installed_config.has_node == true' "A retaining only its node"
retained_after=$(node_invocation "${work}/${controller_a}-after.json" "after the retirement")
test "${retained_after}" != "${retained_before}" ||
    rehearsal_fail "expected the retained node restarted on the serverless configuration; observed the same systemd invocation ${retained_after} it was running before the retirement"
echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) retained node restarted: invocation ${retained_before} -> ${retained_after}"
observed=$(rehearsal_active "${controller_b}" billet-server.service)
test "${observed}" = active || rehearsal_fail "expected B still serving; observed ${observed}"
json_command "${work}/survivor-status.json" "${controller_b}" rollout status --json \
    --config /etc/billet/billet.yaml --environment-file /etc/billet/server.env
require_json "${work}/survivor-status.json" \
    '.retirement.state == "done" and .retirement.transition_id == $id' "done ledger row read through B" --arg id "${transition}"
cat "${work}/journal.json"

rehearsal_step "$(date -u +%Y-%m-%dT%H:%M:%SZ) converge-like read-only re-entry leaves the controller inactive and disabled"
json_command "${work}/reentry.json" "${controller_a}" server retire --dry-run --json \
    --run "${run}" --retiring-host "${controller_a}" --config /etc/billet/billet.yaml \
    --environment-file /etc/billet/server.env
require_json "${work}/reentry.json" \
    '.outcome == "reported" and .route == "continue" and .journal.phase == "done" and
     .journal.variant == "retained-node" and .journal.row_done == true and .journal.settled == true and
     .journal.transition_id == $id and .installed_roles == "node"' \
    "the classifier's settled retained-node answer" --arg id "${transition}"
cat "${work}/reentry.json"
prove_controller_quiet
prove_endpoint "${controller_a}" "https://${b_ip}:7717" "${work}/reentry-node.json"
prove_receipt "${controller_a}" "${work}/reentry-node.json"
for h in "${controller_a}" "${controller_b}" "${node}"; do
    docker exec "${h}" /usr/bin/billet converge-guard release --holder "${run}"
done

echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) retirement rehearsal: PASSED"
echo "  package $(rehearsal_version "${controller_a}") on ${REHEARSAL_ARCH}; transition ${transition}; total $(($(date -u +%s) - started_at))s"
REHEARSAL_PASSED=1
