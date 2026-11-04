#!/bin/bash
# Measures what idle billet control planes cost on one Linux host (#556), the
# numbers docs/reference/records/idle-control-plane.md holds.
#
#   measure-idle-control-planes.sh prepare <billet> <integration.test>
#   measure-idle-control-planes.sh run <label> <sqlite|postgres> <GOMEMLIMIT|none>
#   measure-idle-control-planes.sh restart <label> <clean|kill>
#   measure-idle-control-planes.sh stop
#
# A DEDICATED HOST, AS ROOT, AND ONLY THAT. prepare rewrites /etc/hosts so that
# GitHub's names reach the fake on this machine, which is what lets the real
# binary run with no setting a production control plane does not have, and it
# refuses to run without BILLET_MEASURE_DEDICATED_HOST=yes. Never the shared test
# host, whose configuration no session may change.
#
# <billet> is a release-flagged build, and <integration.test> is
# `go test -c ./internal/integration`, which holds the fake GitHub.
#
# EACH OF GITHUB'S HOSTS HAS ITS OWN LOOPBACK ADDRESS, so a connection's peer says
# what it is for: 127.0.10.1 the REST API, .2 the Actions service, .3 the message
# queue. `ss` then answers whether one target's long polls share a connection.
#
# run starts MEASURE_COUNT control planes of MEASURE_TIERS tiers, waits
# MEASURE_SETTLE seconds, samples every MEASURE_INTERVAL seconds for
# MEASURE_WINDOW seconds, summarizes, and leaves them running for restart. It ends
# non-zero if a process died or the fake met a request it does not answer, since
# either makes the numbers a measurement of something else.
set -euo pipefail

: "${MEASURE_ROOT:=/srv/measure}"
: "${MEASURE_COUNT:=50}"
: "${MEASURE_TIERS:=6}"
: "${MEASURE_SETTLE:=600}"
: "${MEASURE_WINDOW:=1800}"
: "${MEASURE_INTERVAL:=60}"
: "${MEASURE_PG_IMAGE:=postgres:18}"
: "${MEASURE_PG_MAX_CONNECTIONS:=400}"
: "${MEASURE_RESTART_SINGLES:=10}"
: "${MEASURE_RESTART_TIMEOUT:=300}"

measure_user=billet-measure
admin=127.0.0.1:18080
pg_container=billet-measure-pg
pg_password=measure
export PGPASSWORD=${pg_password}

die() {
    echo "measure: $*" >&2
    exit 1
}

planes() {
    seq -f '%02g' 1 "${MEASURE_COUNT}"
}

plane_dir() {
    echo "${MEASURE_ROOT}/planes/$1"
}

prepare() {
    [ "${BILLET_MEASURE_DEDICATED_HOST:-}" = yes ] ||
        die "prepare rewrites /etc/hosts; set BILLET_MEASURE_DEDICATED_HOST=yes on a dedicated host"
    [ "$(id -u)" = 0 ] || die "prepare runs as root"
    [ -x "$1" ] || die "no billet binary at $1"
    [ -x "$2" ] || die "no fake GitHub binary at $2"

    install -d "${MEASURE_ROOT}" "${MEASURE_ROOT}/bin" "${MEASURE_ROOT}/planes" "${MEASURE_ROOT}/results"
    install -m 0755 "$1" "${MEASURE_ROOT}/bin/billet"
    install -m 0755 "$2" "${MEASURE_ROOT}/bin/integration.test"

    if ! id "${measure_user}" >/dev/null 2>&1; then
        useradd --system --no-create-home --shell /usr/sbin/nologin "${measure_user}"
    fi

    if ! grep -q '# billet-measure' /etc/hosts; then
        {
            echo '127.0.10.1 github.com api.github.com # billet-measure'
            echo '127.0.10.2 pipelines.actions.githubusercontent.com # billet-measure'
            echo '127.0.10.3 broker.actions.githubusercontent.com # billet-measure'
        } >>/etc/hosts
    fi

    docker rm -f "${pg_container}" >/dev/null 2>&1 || true
    docker run -d --name "${pg_container}" -p 127.0.0.1:5432:5432 \
        -e POSTGRES_PASSWORD="${pg_password}" "${MEASURE_PG_IMAGE}" \
        -c "max_connections=${MEASURE_PG_MAX_CONNECTIONS}" -c shared_buffers=256MB >/dev/null

    local waited=0
    until psql -h 127.0.0.1 -U postgres -Atc 'SELECT 1' >/dev/null 2>&1; do
        waited=$((waited + 1))
        [ "${waited}" -lt 60 ] || die "PostgreSQL did not answer within a minute"
        sleep 1
    done

    start_fake
}

start_fake() {
    if [ -f "${MEASURE_ROOT}/fake.pid" ] && kill -0 "$(cat "${MEASURE_ROOT}/fake.pid")" 2>/dev/null; then
        return 0
    fi

    BILLET_MEASURE_IDLE_FAKE=:443 BILLET_MEASURE_IDLE_ADMIN="${admin}" \
        BILLET_MEASURE_IDLE_CA="${MEASURE_ROOT}/ca.pem" \
        nohup "${MEASURE_ROOT}/bin/integration.test" -test.run '^TestServeIdleFakeGitHub$' -test.timeout 0 \
        >"${MEASURE_ROOT}/fake.log" 2>&1 &
    echo $! >"${MEASURE_ROOT}/fake.pid"

    local waited=0
    until curl -fsS "http://${admin}/" >/dev/null 2>&1; do
        waited=$((waited + 1))
        [ "${waited}" -lt 30 ] || die "the fake GitHub did not start; see ${MEASURE_ROOT}/fake.log"
        sleep 1
    done
}

# write_plane renders one control plane's directory and config. A fresh key and a
# fresh state each run: nothing a previous configuration left is measured.
write_plane() {
    local n=$1 backend=$2 dir wire metrics
    dir=$(plane_dir "${n}")
    wire=$((20000 + 10#${n}))
    metrics=$((21000 + 10#${n}))

    rm -rf "${dir}"
    install -d -o "${measure_user}" -m 0700 "${dir}"
    install -d -o "${measure_user}" -m 0700 "${dir}/state"
    openssl genrsa -traditional -out "${dir}/app.pem" 2048 2>/dev/null
    chown "${measure_user}" "${dir}/app.pem"
    chmod 0600 "${dir}/app.pem"

    {
        echo 'server:'
        echo "  listen: 127.0.0.1:${wire}"
        echo '  max_vcpu: 64'
        echo '  max_memory: 256GiB'
        echo "  metrics: {listen: \"127.0.0.1:${metrics}\"}"
        if [ "${backend}" = postgres ]; then
            echo "  identity_dir: ${dir}/state"
            echo '  state:'
            echo '    backend: postgres'
            echo '    postgres:'
            echo '      dsn_env: BILLET_STATE_DSN'
            if [ -n "${MEASURE_PG_BUDGET:-}" ]; then
                echo "      max_connections: ${MEASURE_PG_BUDGET}"
            fi
        else
            echo "  state_dir: ${dir}/state"
        fi
        echo 'release: {automatic: false}'
        echo 'github:'
        echo "  org: org-${n}"
        echo '  app_id: 12345'
        echo '  installation_id: 67890'
        echo "  private_key_path: ${dir}/app.pem"
        echo 'tiers:'
        local i
        for i in $(seq 1 "${MEASURE_TIERS}"); do
            echo "  - label: idle-$((i * 2))vcpu"
            echo '    provider: docker'
            echo "    vcpu: $((i * 2))"
            echo "    memory: $((i * 8))GiB"
            echo '    image: ubuntu:24.04'
        done
    } >"${dir}/billet.yaml"
    chown "${measure_user}" "${dir}/billet.yaml"
}

create_database() {
    local n=$1
    psql -h 127.0.0.1 -U postgres -qAtc "DROP DATABASE IF EXISTS cp${n}" >/dev/null
    psql -h 127.0.0.1 -U postgres -qAtc "CREATE DATABASE cp${n}" >/dev/null
}

# start_plane starts one process and records its pid. setpriv execs, so the pid
# is billet's and not a wrapper's, and a kill reaches the process measured.
start_plane() {
    local n=$1 dir limit=${2:-none}
    dir=$(plane_dir "${n}")

    local -a envs=("SSL_CERT_FILE=${MEASURE_ROOT}/ca.pem" "HOME=${dir}")
    if [ "$(cat "${dir}/backend")" = postgres ]; then
        envs+=("BILLET_STATE_DSN=postgres://postgres:${pg_password}@127.0.0.1:5432/cp${n}?sslmode=disable&application_name=cp${n}")
    fi
    if [ "${limit}" != none ]; then
        envs+=("GOMEMLIMIT=${limit}")
    fi

    env "${envs[@]}" nohup setpriv --reuid="${measure_user}" --regid="${measure_user}" --clear-groups \
        "${MEASURE_ROOT}/bin/billet" server --config "${dir}/billet.yaml" >>"${dir}/log" 2>&1 &
    echo $! >"${dir}/pid"
}

stop_all() {
    local n pid
    for n in $(planes); do
        [ -f "$(plane_dir "${n}")/pid" ] || continue
        pid=$(cat "$(plane_dir "${n}")/pid")
        kill -KILL "${pid}" 2>/dev/null || true
    done
}

alive_or_die() {
    local n pid
    for n in $(planes); do
        pid=$(cat "$(plane_dir "${n}")/pid")
        kill -0 "${pid}" 2>/dev/null || die "control plane ${n} died; see $(plane_dir "${n}")/log"
    done
}

fake_clean_or_die() {
    local unexpected
    unexpected=$(curl -fsS "http://${admin}/" | python3 -I -c 'import json,sys; print(len(json.load(sys.stdin)["unexpected"]))')
    if [ "${unexpected}" != 0 ]; then
        curl -fsS "http://${admin}/" >&2
        die "the fake GitHub met requests it does not answer, so this measured a stuck deployment"
    fi
}

# wait_sessions prints each named organization's seconds from <since> to its last
# session opening, once every one of them has every tier's session opened since.
wait_sessions() {
    local since=$1
    shift
    python3 -I - "${admin}" "${since}" "${MEASURE_TIERS}" "${MEASURE_RESTART_TIMEOUT}" "$@" <<'PY'
import datetime
import json
import sys
import time
import urllib.request

admin, since, tiers, timeout = sys.argv[1], float(sys.argv[2]), int(sys.argv[3]), float(sys.argv[4])
orgs = sys.argv[5:]
deadline = time.time() + timeout


def stamp(text):
    return datetime.datetime.fromisoformat(text.replace("Z", "+00:00")).timestamp()


while True:
    with urllib.request.urlopen(f"http://{admin}/", timeout=10) as answer:
        opened = json.load(answer)["opened"]
    done = {}
    for org in orgs:
        fresh = [t for t in (stamp(s) for s in (opened.get(org) or [])) if t >= since]
        if len(fresh) == tiers:
            done[org] = max(fresh) - since
    if len(done) == len(orgs):
        for org in orgs:
            print(f"{org}\t{done[org]:.3f}")
        sys.exit(0)
    if time.time() > deadline:
        print(f"only {len(done)} of {len(orgs)} organizations reopened every session", file=sys.stderr)
        sys.exit(1)
    time.sleep(0.05)
PY
}

# sample appends one row per process to the run's samples, one row per connection
# class, and the host's and PostgreSQL's totals. Every read is of the kernel or of
# the server, never of what billet says about itself, but for goroutines.
sample() {
    local out=$1 now n dir pid
    now=$(date +%s)
    ss -tnpH state established >"${out}/ss.$$" || die "ss failed"

    for n in $(planes); do
        dir=$(plane_dir "${n}")
        pid=$(cat "${dir}/pid")
        [ -r "/proc/${pid}/smaps_rollup" ] || die "control plane ${n} (pid ${pid}) has no smaps_rollup"

        local pss rss anon goroutines heap
        pss=$(awk '$1 == "Pss:" {print $2}' "/proc/${pid}/smaps_rollup")
        anon=$(awk '$1 == "Pss_Anon:" {print $2}' "/proc/${pid}/smaps_rollup")
        rss=$(awk '$1 == "Rss:" {print $2}' "/proc/${pid}/smaps_rollup")

        local scrape
        scrape=$(curl -fsS --max-time 10 "http://127.0.0.1:$((21000 + 10#${n}))/metrics") ||
            die "control plane ${n}'s metrics did not answer"
        goroutines=$(awk '$1 == "go_goroutines" {print $2}' <<<"${scrape}")
        heap=$(awk '$1 == "go_memstats_heap_inuse_bytes" {print $2}' <<<"${scrape}")

        local rest pipelines broker pg
        rest=$(grep -c "127.0.10.1:443 .*pid=${pid}," "${out}/ss.$$" || true)
        pipelines=$(grep -c "127.0.10.2:443 .*pid=${pid}," "${out}/ss.$$" || true)
        broker=$(grep -c "127.0.10.3:443 .*pid=${pid}," "${out}/ss.$$" || true)
        pg=$(grep -c "127.0.0.1:5432 .*pid=${pid}," "${out}/ss.$$" || true)

        printf '%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' "${now}" "cp${n}" "${pss}" "${anon}" "${rss}" \
            "${goroutines}" "${heap}" "${rest}" "${pipelines}" "${broker}" "${pg}" >>"${out}/samples.tsv"
    done
    rm -f "${out}/ss.$$"

    if [ "$(cat "$(plane_dir 01)/backend")" = postgres ]; then
        psql -h 127.0.0.1 -U postgres -AtF $'\t' -c \
            "SELECT ${now}, application_name, COUNT(*), COUNT(*) FILTER (WHERE state = 'idle')
               FROM pg_stat_activity WHERE application_name LIKE 'cp%' GROUP BY application_name" \
            >>"${out}/postgres.tsv"
        local backends
        backends=$(pgrep -x postgres | while read -r p; do
            awk '$1 == "Pss:" {print $2}' "/proc/${p}/smaps_rollup" 2>/dev/null || true
        done | awk '{s += $1} END {print s + 0}')
        printf '%s\t%s\n' "${now}" "${backends}" >>"${out}/postgres-pss.tsv"
    fi

    printf '%s\t%s\t%s\n' "${now}" "$(awk '$1 == "MemAvailable:" {print $2}' /proc/meminfo)" \
        "$(cut -d' ' -f1 /proc/loadavg)" >>"${out}/host.tsv"
}

# cpu_ticks prints a process's user plus system ticks.
cpu_ticks() {
    awk '{print $14 + $15}' "/proc/$1/stat"
}

summarize() {
    python3 -I - "$1" "$(getconf CLK_TCK)" <<'PY'
import collections
import json
import os
import statistics
import sys

out, hz = sys.argv[1], int(sys.argv[2])


def quantiles(values):
    values = sorted(values)
    return {
        "min": values[0],
        "median": statistics.median(values),
        "p95": values[int(0.95 * (len(values) - 1))],
        "max": values[-1],
    }


per = collections.defaultdict(lambda: collections.defaultdict(list))
names = ["pss_kib", "pss_anon_kib", "rss_kib", "goroutines", "heap_inuse_bytes", "rest", "pipelines", "broker", "postgres"]
with open(os.path.join(out, "samples.tsv")) as rows:
    for row in rows:
        cells = row.rstrip("\n").split("\t")
        for name, cell in zip(names, cells[2:]):
            per[cells[1]][name].append(float(cell))

summary = {"planes": len(per), "per_plane_median_across_samples": {}}
for name in names:
    summary["per_plane_median_across_samples"][name] = quantiles([statistics.median(p[name]) for p in per.values()])
summary["pss_total_mib_median"] = sum(statistics.median(p["pss_kib"]) for p in per.values()) / 1024

ticks = {}
with open(os.path.join(out, "cpu.tsv")) as rows:
    for row in rows:
        plane, before, after, seconds = row.split()
        ticks[plane] = (int(after) - int(before)) / hz / float(seconds)
summary["cpu_share_of_one_core"] = quantiles(list(ticks.values()))
summary["cpu_total_cores"] = sum(ticks.values())

path = os.path.join(out, "postgres.tsv")
if os.path.exists(path):
    counts = collections.defaultdict(list)
    idle = collections.defaultdict(list)
    with open(path) as rows:
        for row in rows:
            _, app, total, parked = row.split("\t")
            counts[app].append(int(total))
            idle[app].append(int(parked))
    summary["postgres_connections_max_per_plane"] = quantiles([max(v) for v in counts.values()])
    summary["postgres_connections_median_per_plane"] = quantiles([statistics.median(v) for v in counts.values()])
    summary["postgres_connections_total_max"] = sum(max(v) for v in counts.values())
    with open(os.path.join(out, "postgres-pss.tsv")) as rows:
        values = [int(row.split()[1]) for row in rows]
    summary["postgres_server_pss_mib_median"] = statistics.median(values) / 1024

with open(os.path.join(out, "summary.json"), "w") as result:
    json.dump(summary, result, indent=2, sort_keys=True)
print(json.dumps(summary, indent=2, sort_keys=True))
PY
}

run_config() {
    local label=$1 backend=$2 limit=$3 out n
    out=${MEASURE_ROOT}/results/${label}
    case "${backend}" in sqlite | postgres) ;; *) die "no backend ${backend}" ;; esac
    [ ! -e "${out}" ] || die "${out} already exists; a run never overwrites one"
    install -d "${out}"

    stop_all
    start_fake
    sleep 2

    for n in $(planes); do
        write_plane "${n}" "${backend}"
        echo "${backend}" >"$(plane_dir "${n}")/backend"
        echo "${limit}" >"$(plane_dir "${n}")/limit"
        [ "${backend}" != postgres ] || create_database "${n}"
    done

    local started
    started=$(date +%s.%N)
    for n in $(planes); do
        start_plane "${n}" "${limit}"
    done

    local -a orgs=()
    for n in $(planes); do
        orgs+=("org-${n}")
    done
    wait_sessions "${started}" "${orgs[@]}" >"${out}/first-start.tsv" ||
        die "the control planes did not open every session; see $(plane_dir 01)/log"

    sleep "${MEASURE_SETTLE}"
    alive_or_die

    declare -A before
    for n in $(planes); do
        before[${n}]=$(cpu_ticks "$(cat "$(plane_dir "${n}")/pid")")
    done
    local window_start elapsed
    window_start=$(date +%s)

    local end=$((window_start + MEASURE_WINDOW))
    while [ "$(date +%s)" -lt "${end}" ]; do
        sample "${out}"
        sleep "${MEASURE_INTERVAL}"
    done
    elapsed=$(($(date +%s) - window_start))

    for n in $(planes); do
        printf 'cp%s\t%s\t%s\t%s\n' "${n}" "${before[${n}]}" \
            "$(cpu_ticks "$(cat "$(plane_dir "${n}")/pid")")" "${elapsed}" >>"${out}/cpu.tsv"
    done

    alive_or_die
    fake_clean_or_die
    summarize "${out}"
}

# stop_plane stops one process the requested way and waits for it, printing how
# long the stop took. A clean stop is the handoff a restart makes.
stop_plane() {
    local n=$1 how=$2 pid started
    pid=$(cat "$(plane_dir "${n}")/pid")
    started=$(date +%s.%N)
    case "${how}" in
        clean) kill -TERM "${pid}" ;;
        kill) kill -KILL "${pid}" ;;
        *) die "no way to stop called ${how}" ;;
    esac
    while kill -0 "${pid}" 2>/dev/null; do
        sleep 0.05
    done
    python3 -I -c "import sys,time; print(f'{time.time() - float(sys.argv[1]):.3f}')" "${started}"
}

restart_config() {
    local label=$1 how=$2 out n
    out=${MEASURE_ROOT}/results/${label}
    install -d "${out}"

    # ONE AT A TIME, while the others stay up.
    for n in $(planes | head -n "${MEASURE_RESTART_SINGLES}"); do
        local stopped started seconds limit
        limit=$(cat "$(plane_dir "${n}")/limit")
        stopped=$(stop_plane "${n}" "${how}")
        started=$(date +%s.%N)
        start_plane "${n}" "${limit}"
        seconds=$(wait_sessions "${started}" "org-${n}" | cut -f2)
        printf 'single\tcp%s\t%s\t%s\n' "${n}" "${stopped}" "${seconds}" >>"${out}/restart-${how}.tsv"
    done

    # EVERY CONTROL PLANE AT ONCE, which is what a host's reboot is.
    local -a orgs=()
    for n in $(planes); do
        orgs+=("org-${n}")
        stop_plane "${n}" "${how}" >/dev/null &
    done
    wait

    local started
    started=$(date +%s.%N)
    for n in $(planes); do
        start_plane "${n}" "$(cat "$(plane_dir "${n}")/limit")"
    done
    wait_sessions "${started}" "${orgs[@]}" | sed 's/^/all\t/' >>"${out}/restart-${how}.tsv"
    alive_or_die
    fake_clean_or_die
}

case "${1:-}" in
    prepare)
        [ $# -eq 3 ] || die "usage: prepare <billet> <integration.test>"
        prepare "$2" "$3"
        ;;
    run)
        [ $# -eq 4 ] || die "usage: run <label> <sqlite|postgres> <GOMEMLIMIT|none>"
        run_config "$2" "$3" "$4"
        ;;
    restart)
        [ $# -eq 3 ] || die "usage: restart <label> <clean|kill>"
        restart_config "$2" "$3"
        ;;
    stop)
        stop_all
        ;;
    *)
        die "usage: prepare | run | restart | stop"
        ;;
esac
