#!/bin/bash
# The promotion rehearsal: an active-passive pair of packaged controllers on one
# PostgreSQL ledger and three nodes that name both, the active controller first
# cut off the network and later killed, each failover measured segment by
# segment, and the old leader proved to stop rather than keep serving.
#
# WHAT THIS PROVES THAT THE TESTS DO NOT. ADR-009's election is a process
# waiting on a PostgreSQL session advisory lock: a controller is dead when its
# session is, decided by the database. The fence and the election are under
# test and the promotion order is asserted structurally; what only a real
# network can measure is how long each part of a failover takes. The promotion
# is the database's TCP keepalive settings and nothing billet owns. With
# PostgreSQL's defaults (tcp_keepalives_idle = 2 hours) a standby waits hours;
# this rehearsal runs the server with short keepalives and RECORDS what it
# measured, because the operator has to set them. What billet owns is the rest:
# a node that lists every controller (node.server_addrs, #529) notices the one
# it served has gone and registers with whichever serves, and the target is
# every node registered within ten seconds of the promotion. GitHub's message
# sessions are the last segment and GitHub's, reported beside it and not held
# to it.
#
# TWO FAILURES, because they reach a node differently. A partition sends
# nothing: the node's connection is ended by its own keepalive probes and a dial
# that is not answered gives up after its timeout. A crash is answered by the
# kernel at once, with a reset and then a refused connection, and the
# database's session ends with the process, so the promotion is immediate.
#
# A NODE'S LIST CHANGES THROUGH THE ENDPOINT COMMANDS. A process's list is fixed
# when it starts, so taking a controller off a node's list and putting it back
# runs the three commands the host role calls, on a real unit, confirmed by the
# real controller: the decision, the migration and the receipt, whose schema says
# how many controllers the node names.
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
nodes=(rehearsal-node-1 rehearsal-node-2 rehearsal-node-3)
# TWO TIERS, SO TWO MESSAGE SESSIONS, each of which the promoted controller has
# to open again; two of the installation's twenty.
labels=("rehearse-${id}-one" "rehearse-${id}-two")
dsn="postgres://billet:billet@${postgres}:5432/billet?sslmode=disable"
# The keepalives the PostgreSQL server runs with. THESE DECIDE THE PROMOTION
# TIME after a partition and they are the operator's to set; the record names
# them beside the measurement so nobody reads the number as billet's.
keepalive_idle=10
keepalive_interval=5
keepalive_count=3
# THE TARGET (#529): every node registered within this many seconds of the
# promotion.
target=10
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

# node_config prints a node's configuration naming the list given.
node_config() {
    cat <<EOF
node:
  server_addrs: $1
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
  tls:
    cert: /etc/billet/tls/node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt
EOF
}

# member prints a string member at the top of a command's answer.
member() {
    awk -F'"' -v key="$2" '$2 == key && found == "" { found = $4 } END { print found }' <<<"$1"
}

# move_controllers takes a node to the list given as the host role would: the
# decision over the rendering, the rendering installed, the migration, the
# controller's confirmation of the new process's registration, and the receipt
# bound to both, whose schema is what that many controllers write.
move_controllers() {
    local n=$1 list=$2 schema=$3 controller=$4
    local answer incarnation confirmation

    node_config "${list}" | docker exec -i "${n}" sh -c 'cat >/tmp/rendering.yaml' ||
        rehearsal_fail "could not stage ${n}'s rendering"

    answer=$(docker exec "${n}" /usr/bin/billet node migrate-endpoint --config /etc/billet/billet.yaml \
        --desired /tmp/rendering.yaml --dry-run --json) ||
        rehearsal_fail "the decision for ${n} to ${list} was not an answer: ${answer}"
    grep -q '"planned": true' <<<"${answer}" ||
        rehearsal_fail "the decision for ${n} to ${list} planned nothing: ${answer}"

    docker exec "${n}" install -m 0640 -o root -g billet /tmp/rendering.yaml /etc/billet/billet.yaml ||
        rehearsal_fail "could not install ${n}'s rendering"

    answer=$(docker exec "${n}" /usr/bin/billet node migrate-endpoint --config /etc/billet/billet.yaml \
        --desired /tmp/rendering.yaml --stop-timeout 120s --wait 120s --json) ||
        rehearsal_fail "the migration of ${n} to ${list} failed: ${answer}"
    test "$(member "${answer}" outcome)" = migrated ||
        rehearsal_fail "the migration of ${n} to ${list} answered: ${answer}"

    incarnation=$(member "${answer}" incarnation)
    docker exec -i "${n}" sh -c 'umask 077 && cat >/tmp/evidence.json' <<<"${answer}"

    confirmation=$(docker exec -e "BILLET_STATE_DSN=${dsn}" "${controller}" runuser -u billet -- \
        env "BILLET_STATE_DSN=${dsn}" /usr/bin/billet rollout registration --config /etc/billet/billet.yaml \
        --node "${n}" --incarnation "${incarnation}" --wait 120s --json) ||
        rehearsal_fail "${controller} did not confirm ${n} under ${incarnation}: ${confirmation}"
    docker exec -i "${n}" sh -c 'umask 077 && cat >/tmp/confirmation.json' <<<"${confirmation}"

    answer=$(docker exec "${n}" /usr/bin/billet node receipt --evidence /tmp/evidence.json \
        --confirmation /tmp/confirmation.json --config /etc/billet/billet.yaml --run rehearsal --json) ||
        rehearsal_fail "the receipt of ${n}'s move to ${list} failed: ${answer}"
    test "$(member "${answer}" outcome)" = written ||
        rehearsal_fail "the receipt of ${n}'s move to ${list} answered: ${answer}"
    grep -q "^    \"schema\": ${schema},\$" <<<"${answer}" ||
        rehearsal_fail "the receipt of ${n} naming ${list} is not schema ${schema}: ${answer}"

    docker exec "${n}" rm -f /tmp/evidence.json /tmp/confirmation.json
}

# partition drops every packet a controller sends or receives on the rehearsal
# network, and heal lets them through again.
#
# PACKETS DROPPED, NOT THE NETWORK DISCONNECTED. A container taken off its
# network stops resolving, so every dial to it failed at once on the name and
# the dial timeout a real partition meets was never exercised; the first run of
# this rehearsal measured that way (2026-10-25). The rules name the rehearsal
# network's interface, so the controller's own loopback still works.
partition() {
    docker exec "$1" sh -c 'iptables -I INPUT 1 -i eth0 -j DROP && iptables -I OUTPUT 1 -o eth0 -j DROP'
}

heal() {
    docker exec "$1" sh -c 'iptables -D INPUT -i eth0 -j DROP && iptables -D OUTPUT -o eth0 -j DROP'
}

# mark_at prints a host's clock as seconds since the epoch, to the microsecond.
# Every container reads the one kernel clock of the machine running them, so a
# mark taken in one host and a journal timestamp read in another are on one
# timeline.
mark_at() {
    docker exec "$1" date -u +%s.%6N
}

# line_at prints when a unit in a host first logged a line matching an
# extended regular expression at or after a mark_at, as seconds since the
# epoch; nothing when it has not. The whole journal since the mark's second is
# read, and awk never exits early, so journalctl is never ended by a closed pipe
# that pipefail would report as the reader's failure (billet-shell-gates).
line_at() {
    local host=$1 unit=$2 after=$3 pattern=$4

    docker exec "${host}" journalctl -u "${unit}" --since "@${after%.*}" --no-pager -o short-unix |
        awk -v after="${after}" -v pattern="${pattern}" \
            'at == "" && $1 + 0 >= after + 0 && $0 ~ pattern { at = $1 } END { print at }'
}

# wait_line waits up to a deadline in seconds for line_at to find its line, and
# prints the line's time. A journal that cannot be read is a failure, never a
# line that has not come yet.
wait_line() {
    local deadline=$1 host=$2 unit=$3 after=$4 pattern=$5
    local at waited=0

    while [ "${waited}" -lt "${deadline}" ]; do
        at=$(line_at "${host}" "${unit}" "${after}" "${pattern}") || return 1
        if [ -n "${at}" ]; then
            echo "${at}"
            return 0
        fi
        sleep 1
        waited=$((waited + 1))
    done
    return 1
}

# span prints the seconds from one time to another, to a tenth.
span() {
    awk -v a="$1" -v b="$2" 'BEGIN { printf "%.1f", b - a }'
}

# measure_failover reads one failover's segments from the journals, from the
# failure to every node registered with the controller that took over and every
# tier's message session open there, prints each, and holds the target. It
# sets the summary line the PASSED line repeats.
measure_failover() {
    local name=$1 successor=$2 failed_at=$3
    local promoted wire first='' last='' at n label lost accepted

    echo "--- ${name}: segments, in seconds"
    promoted=$(wait_line 300 "${successor}" billet-server "${failed_at}" 'promoted to this deployment.s controller') ||
        rehearsal_fail "${successor} was never promoted after the ${name}"
    echo "failure -> promoted: $(span "${failed_at}" "${promoted}")"
    wire=$(wait_line 120 "${successor}" billet-server "${promoted}" 'serving the node wire') ||
        rehearsal_fail "${successor} never served the node wire after its promotion"
    echo "promoted -> serving the node wire: $(span "${promoted}" "${wire}")"

    for n in "${nodes[@]}"; do
        # THE NODE'S OWN LINE, written once it has read the acceptance and made
        # that controller the one it dials. The controller logs its side before
        # it answers, so its line alone could stand for a registration the
        # node never received.
        at=$(wait_line 120 "${n}" billet-node "${failed_at}" \
            "registered with the control plane .*control_plane=https://${successor}:7717( |$)") ||
            rehearsal_fail "${n} never registered with ${successor} after the ${name}"
        accepted=$(line_at "${successor}" billet-server "${failed_at}" "node registered node=${n}( |$)") ||
            rehearsal_fail "could not read ${successor}'s journal"
        test -n "${accepted}" ||
            rehearsal_fail "${n} says it registered with ${successor}, whose journal records no such registration"
        # READ AFTER THE REGISTRATION, which the node makes only once it has
        # given up on the controller it had, so the line is there if it ever is.
        lost=$(line_at "${n}" billet-node "${failed_at}" 'lost the control plane') ||
            rehearsal_fail "could not read ${n}'s journal"
        test -n "${lost}" ||
            rehearsal_fail "${n} registered with ${successor} without saying it lost the controller it had"
        echo "${n}: failure -> lost the control plane $(span "${failed_at}" "${lost}"), accepted by ${successor} $(span "${failed_at}" "${accepted}"), registered $(span "${failed_at}" "${at}")"
        if [ -z "${first}" ] || awk -v a="${at}" -v b="${first}" 'BEGIN { exit !(a < b) }'; then
            first=${at}
        fi
        if [ -z "${last}" ] || awk -v a="${at}" -v b="${last}" 'BEGIN { exit !(a > b) }'; then
            last=${at}
        fi
    done
    echo "serving the node wire -> first node registered: $(span "${wire}" "${first}")"
    echo "serving the node wire -> every node registered: $(span "${wire}" "${last}")"
    echo "promoted -> every node registered: $(span "${promoted}" "${last}") (target ${target})"

    # GITHUB'S SEGMENT, reported and not held to the target: the scale set's
    # session is single-holder and GitHub expires the one the old leader left
    # (billet-github-protocol).
    for label in "${labels[@]}"; do
        at=$(wait_line 300 "${successor}" billet-server "${promoted}" "opened this tier.s message session tier=${label}( |$)") ||
            rehearsal_fail "${successor} never opened ${label}'s message session after the ${name}"
        echo "promoted -> ${label}'s message session open (GitHub's): $(span "${promoted}" "${at}")"
    done

    summary="${summary}; ${name}: failure -> promoted $(span "${failed_at}" "${promoted}")s, promoted -> every node registered $(span "${promoted}" "${last}")s"

    # PRINTED FIRST AND HELD AFTER, so a miss shows every segment it is made of.
    awk -v a="${promoted}" -v b="${last}" -v t="${target}" 'BEGIN { exit !(b - a < t) }' ||
        rehearsal_fail "the ${name}: every node registered $(span "${promoted}" "${last}")s after the promotion, against a target of ${target}s"
}

cleanup() {
    status=$?
    set +e
    # IGNORED, NOT RESET: a second Ctrl-C or a TERM during the teardown must not
    # end the shell before the scale set and the hosts are gone. The status the
    # first signal chose (130 or 143) is already in ${status}.
    trap '' INT TERM
    status=$(rehearsal_verdict "${status}")

    echo
    echo "=== teardown"
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
        echo "TEARDOWN FAILED: the scale sets for ${labels[*]} may still exist. Remove them from any host" >&2
        echo "holding this App: billet teardown --all --yes --config <that config>" >&2
        if [ "${status}" -eq 0 ]; then status=1; fi
    fi

    if [ "${status}" -ne 0 ]; then
        for h in "${controller_a}" "${controller_b}" "${nodes[@]}"; do
            echo "--- ${h} journal"
            docker exec "${h}" journalctl -u billet-server -u billet-node -n 30 --no-pager -o cat 2>&1 | tail -30 || true
        done
    fi

    # A HOST, NETWORK OR STORAGE LEFT BEHIND FAILS THE RUN, so a green rehearsal
    # never hides something an operator has to remove by hand.
    if ! rehearsal_teardown_hosts "${network}" "${storage}" "${controller_a}" "${controller_b}" "${nodes[@]}" "${postgres}"; then
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
summary=
trap 'exit 130' INT
trap 'exit 143' TERM
trap cleanup EXIT

rehearsal_step "a PostgreSQL with short keepalives, two controllers and three nodes"
docker network create "${network}" >/dev/null
docker run -d --name "${postgres}" --network "${network}" \
    -e POSTGRES_USER=billet -e POSTGRES_PASSWORD=billet -e POSTGRES_DB=billet \
    postgres:18-alpine \
    -c "tcp_keepalives_idle=${keepalive_idle}" \
    -c "tcp_keepalives_interval=${keepalive_interval}" \
    -c "tcp_keepalives_count=${keepalive_count}" >/dev/null
rehearsal_wait_for 90 "PostgreSQL to accept connections" "${postgres}" pg_isready -U billet -d billet ||
    rehearsal_fail "PostgreSQL never came up"

# NO SHARED NAME. Each node names both controllers, and each controller's
# serving certificate names itself, which is what node.server_addrs is for: an
# alias both answered to was how the first runs moved the node, and it moved
# nothing a real fleet could copy.
rehearsal_start_host "${controller_a}" "${network}" no "${storage}"
rehearsal_start_host "${controller_b}" "${network}" no "${storage}"
for n in "${nodes[@]}"; do
    rehearsal_start_host "${n}" "${network}" yes "${storage}"
done
for h in "${controller_a}" "${controller_b}" "${nodes[@]}"; do
    rehearsal_install_package "${h}" "${REHEARSAL_DIST_DEB}"
done
rehearsal_install_app_key "${controller_a}"
rehearsal_install_app_key "${controller_b}"
for c in "${controller_a}" "${controller_b}"; do
    docker exec "${c}" sh -c 'timeout -v -k 10 300 apt-get -o Acquire::Retries=3 -o Acquire::http::Timeout=30 -o Acquire::https::CAInfo=/usr/local/share/billet-host-ca.crt install -y -qq iptables >/dev/null' ||
        rehearsal_fail "iptables would not install in ${c}, so it cannot be partitioned"
    docker exec "${c}" test -d /sys/class/net/eth0 ||
        rehearsal_fail "${c} has no eth0, the interface the partition drops"
done

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
  node_tls_hosts: [${c}]
  identity_dir: /var/lib/billet/server
  state:
    backend: postgres
    postgres:
      dsn_env: BILLET_STATE_DSN
  controllers: active-passive
  max_vcpu: 8
  max_memory: 16GiB
# NOT ON AUTOMATIC UPDATES. This is the tree's own snapshot build, which reports
# a release the stable channel is not on, so the starter opened a rollout to the
# channel within a minute of boot (measured 2026-09-04) and the packaged root
# timer then fenced the ledger for its host transaction, refusing the very
# local down this rehearsal is about. The rollout rehearsal is the one that
# wants the default. (Plain words, no backticks: this heredoc is unquoted, and
# a backtick pair inside it is a command substitution; the first runs of every
# rehearsal ran the words local down as a command at the top level of the script,
# "local: can only be used in a function" against the heredoc's line.)
release:
  automatic: false
EOF
        rehearsal_github_block
        echo "tiers:"
        for label in "${labels[@]}"; do
            cat <<EOF
  - label: ${label}
    provider: docker
    trust: untrusted
    vcpu: 2
    memory: 4GiB
    disk: 20GiB
    image: ghcr.io/actions/actions-runner:latest
    command: ["./run.sh"]
EOF
        done
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

for n in "${nodes[@]}"; do
    node_config "[${controller_a}:7717, ${controller_b}:7717]" | rehearsal_install_config "${n}"
done

rehearsal_step "one identity and one authority on both controllers"
# `ca issue` on A mints the identity and the authority into A's identity
# directory; B must hold the SAME ones or it is a rival deployment. In a fleet
# this is `billet ca sync` through an identity store; the rehearsal copies the
# files the documented way and records that as its limitation.
for n in "${nodes[@]}"; do
    docker exec -e "BILLET_STATE_DSN=${dsn}" "${controller_a}" runuser -u billet -- \
        env "BILLET_STATE_DSN=${dsn}" /usr/bin/billet ca issue "${n}" \
        --config /etc/billet/billet.yaml --out "/tmp/${n}-tls" >/dev/null ||
        rehearsal_fail "controller A could not issue ${n}'s certificate"
    docker cp "${controller_a}:/tmp/${n}-tls" "${work}/${n}"
    docker exec "${controller_a}" rm -rf "/tmp/${n}-tls"
    rehearsal_install_bundle "${n}" "${work}/${n}"
    rm -rf "${work:?}/${n}"
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
test "$(docker exec "${controller_a}" cat /var/lib/billet/server/deployment-id)" = \
    "$(docker exec "${controller_b}" cat /var/lib/billet/server/deployment-id)" ||
    rehearsal_fail "the two controllers do not share one deployment identity"

rehearsal_step "start A, then B; one claims and one stands by"
since_a=$(rehearsal_clock "${controller_a}")
plane_started=yes
docker exec -e "BILLET_STATE_DSN=${dsn}" "${controller_a}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
docker exec -e "BILLET_STATE_DSN=${dsn}" "${controller_b}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
for n in "${nodes[@]}"; do
    docker exec "${n}" /usr/bin/billet local up --config /etc/billet/billet.yaml 2>&1 | tail -4
done

rehearsal_wait_for 120 "A to claim the controller" "${controller_a}" \
    sh -c 'journalctl -u billet-server --no-pager -o cat | grep -Eq "(claimed|promoted to) this deployment.s controller"' ||
    rehearsal_fail "controller A never claimed"
rehearsal_wait_for 120 "B to stand by" "${controller_b}" \
    sh -c 'journalctl -u billet-server --no-pager -o cat | grep -q "standing by for this deployment.s controller"' ||
    rehearsal_fail "controller B never stood by"
for n in "${nodes[@]}"; do
    rehearsal_wait_registered 120 "${controller_a}" "${n}" "${since_a}" ||
        rehearsal_fail "${n} never registered with A"
done
# ASSERTED, NOT PRINTED: everything measured below is relative to A holding
# the claim now, and a run where B had it would measure nothing.
test "$(active_controller "${controller_b}")" = "${controller_a}" ||
    rehearsal_fail "billet status does not name A as the claim holder before the partition"
echo "claim held by: ${controller_a}"
# AND HOLDING BOTH SESSIONS, which it opens after it is ready: a partition
# before then leaves B nothing to wait out, and its session segment would
# measure an uncontended open rather than a failover.
for label in "${labels[@]}"; do
    rehearsal_wait_for 300 "A to open ${label}'s message session" "${controller_a}" \
        sh -c "journalctl -u billet-server --since '${since_a}' --no-pager -o cat | grep -q 'opened this tier.s message session tier=${label} '" ||
        rehearsal_fail "controller A never opened ${label}'s message session"
done

rehearsal_step "partition A; measure the failover to B"
# EVERY TIME FROM HERE ON IS READ FROM A JOURNAL AFTER A MARK taken before the
# boundary it is about. A whole-journal grep is satisfied by a line from before
# the failure (B's own startup, A's first "standing by"), and the second review
# round found the post-heal wait below being answered by exactly that line.
partitioned_at=$(mark_at "${controller_b}")
partition "${controller_a}" || rehearsal_fail "could not partition ${controller_a}"
measure_failover "partition of A" "${controller_b}" "${partitioned_at}"
test "$(active_controller "${controller_b}")" = "${controller_b}" ||
    rehearsal_fail "billet status on B does not name B as the claim holder after the promotion"

rehearsal_step "heal the partition; the old leader stops and stands by"
restarts_before=$(docker exec "${controller_a}" systemctl show -p NRestarts --value billet-server.service)
since_heal=$(rehearsal_clock "${controller_a}")
healed_at=$(mark_at "${controller_a}")
heal "${controller_a}" || rehearsal_fail "could not heal ${controller_a}'s partition"
rehearsal_wait_for 300 "A to notice it was replaced" "${controller_a}" \
    sh -c "journalctl -u billet-server --since '${since_heal}' --no-pager -o cat | grep -q 'no longer this deployment.s controller'" ||
    rehearsal_fail "controller A never noticed it had been replaced"
rehearsal_wait_for 120 "A to be restarted by systemd" "${controller_a}" \
    sh -c "test \"\$(systemctl show -p NRestarts --value billet-server.service)\" -gt ${restarts_before}" ||
    rehearsal_fail "systemd did not restart controller A after it stopped"
stood_by_at=$(wait_line 120 "${controller_a}" billet-server "${healed_at}" 'standing by for this deployment.s controller') ||
    rehearsal_fail "controller A did not come back as a standby"
test "$(rehearsal_active "${controller_a}" billet-server.service)" = active ||
    rehearsal_fail "controller A is not active after coming back as a standby"
test "$(active_controller "${controller_a}")" = "${controller_b}" ||
    rehearsal_fail "after healing, billet status on A does not name B as the claim holder"
test "$(rehearsal_active "${controller_b}" billet-server.service)" = active ||
    rehearsal_fail "controller B is not active after A rejoined"
echo "heal -> old leader standing by: $(span "${healed_at}" "${stood_by_at}")"

rehearsal_step "kill B; measure the failover back to A"
killed_at=$(mark_at "${controller_a}")
docker exec "${controller_b}" systemctl kill -s KILL billet-server.service
measure_failover "kill of B" "${controller_a}" "${killed_at}"
test "$(active_controller "${controller_a}")" = "${controller_a}" ||
    rehearsal_fail "billet status on A does not name A as the claim holder after the second promotion"
# THE KILLED CONTROLLER COMES BACK AS A STANDBY, restarted by systemd: a crash
# is not a retirement.
wait_line 120 "${controller_b}" billet-server "${killed_at}" 'standing by for this deployment.s controller' >/dev/null ||
    rehearsal_fail "controller B did not come back as a standby after it was killed"

rehearsal_step "change a node's controllers through the endpoint commands"
mover=${nodes[0]}
# OFF THE LIST AND BACK ON: A serves, so the node stays on it both ways, and what
# the decision sees each time is a process that dials another list than the one
# wanted, on a controller that list still names.
move_controllers "${mover}" "[${controller_a}:7717]" 1 "${controller_a}"
echo "${mover} moved to ${controller_a} alone, its receipt schema 1"
move_controllers "${mover}" "[${controller_a}:7717, ${controller_b}:7717]" 2 "${controller_a}"
echo "${mover} moved back to both, its receipt schema 2"

# AN ORDER IS NOT A MOVE, and the node dialling the list is left alone.
reordered=$(node_config "[${controller_b}:7717, ${controller_a}:7717]")
answer=$(docker exec -i "${mover}" /usr/bin/billet node migrate-endpoint --config /etc/billet/billet.yaml \
    --desired - --dry-run --json <<<"${reordered}") ||
    rehearsal_fail "the decision over a reordered list was not an answer: ${answer}"
grep -q '"planned": false' <<<"${answer}" ||
    rehearsal_fail "a reordered list planned a move: ${answer}"

# THE REFRESH FINDS THE RECEIPT CURRENT, the same process on the same controller.
docker exec "${mover}" sh -c "/usr/bin/billet node receipt --refresh --config /etc/billet/billet.yaml --desired /tmp/rendering.yaml --run rehearsal --json" >"${work}/refresh.json" ||
    rehearsal_fail "the refresh after the moves failed: $(cat "${work}/refresh.json")"
test "$(member "$(cat "${work}/refresh.json")" outcome)" = current ||
    rehearsal_fail "the refresh after the moves answered: $(cat "${work}/refresh.json")"
docker exec "${mover}" rm -f /tmp/rendering.yaml
echo "an order is not a move, and the refresh finds the receipt current"

echo
echo "promotion rehearsal: PASSED"
echo "  package $(rehearsal_version "${controller_a}") on ${REHEARSAL_ARCH}; PostgreSQL keepalives idle=${keepalive_idle}s interval=${keepalive_interval}s count=${keepalive_count}${summary}; heal -> old leader standing by $(span "${healed_at}" "${stood_by_at}")s; total $(($(date -u +%s) - started_at))s"
# THE LAST STATEMENT, after every line of output: a signal landing before this
# still fails the run.
REHEARSAL_PASSED=1
