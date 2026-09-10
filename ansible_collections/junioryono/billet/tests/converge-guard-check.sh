#!/usr/bin/env bash
# The host role's PREPARATION, proved case by case: the converge guard's
# refusal of a billet-managed runner (the file's original purpose), and the
# exclusion the role prepares before anything else (prepare-exclusion.yml)
# through `billet converge-guard prepare`: the two calls, the routes (the
# managed binary, the verified fallback, the candidate-first acquisition, the
# legacy protocol, no billet), the staging between them, the settlement, and
# the cleanup release of a guard this run acquired.
#
# WHY A GATE OF ITS OWN. Every other suite here runs with no RUNNER_NAME and no
# holder in the environment, so none of them reaches these branches: delete the
# guard, the holder refusal or the calls and they all stay green.
#
# THE REAL BINARY AT ITS REAL PATHS. Each Linux case runs inside its own mount
# namespace (`sudo -n unshare -m --propagation private`), where a tmpfs backs
# an overlay over /usr/bin and one over /var/lib, so /usr/bin/billet and
# /var/lib/billet are ordinary entries that can be absent, made, replaced by
# rename, unlinked and chowned, and nothing reaches the host (measured
# 2026-09-10 in the gate container, kernel 6.12.76-linuxkit, util-linux 2.39.3;
# an upper directory on the container's own overlay root is refused, hence the
# tmpfs). The managed binary and every staged candidate are RECORDING WRAPPERS
# around immutable backing binaries built once from this checkout with three
# version stamps and `-tags billetgatecrash` (the crash seam, compiled into the
# gate's binaries and no shipped one): a wrapper logs its argv and its
# invocation number, fires the case's hook for exactly its own invocation,
# runs its backing binary EXACTLY ONCE, keeps the real answer in a private
# capture, and hands the real answer, a substituted one or none to the caller.
# The guard's record names the wrapper's path and digest, as the contract
# requires; the backing binary's identity is in the private capture only. A
# script that answers "unknown command" stands in for a managed binary from
# before the guard, and one that sleeps for a hang. The unescalated cases (a
# root another account owns, a claim the account cannot examine) run inside
# their namespace as the gate's invoker, never root; the simulated-darwin
# cases run on the fake billet with the role's path overrides, because a
# Linux-built binary answers for Linux's paths and the role's darwin branch is
# what they prove (a real Mac stays the measurement). The copy-out of the
# upper directories is diagnostic only: every assertion about the tree reads
# the state the namespace dumped after the play.
#
# EVERY ALLOWED CASE MUST EXIT SUCCESSFULLY; every refusal is judged by the
# FAILING TASK'S NAME and its diagnostic, and the rescue's final re-failure by
# its own message; a prohibited effect is asserted from the log, the state
# dump and the facts the play reports from an `always` section. The token an
# `acquired` answer carries is captured privately and grepped for in every
# case's output (one case under -vvv): any occurrence fails the gate.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
collection_root=$(cd "$here/../../../.." && pwd)
repo_root=$collection_root
role_tasks="$here/../roles/host/tasks"
fixtures="$here/fixtures/guard-prepare"

work=$(mktemp -d)
have_root=0
if sudo -n true 2>/dev/null; then have_root=1; fi
# rd CMD...: read or remove what an escalated case left root-owned.
rd() { if [ "$have_root" = 1 ]; then sudo -n "$@"; else "$@"; fi; }
origin_pid=""
# BILLET_GATE_KEEP=1 keeps the work directory (every case's output, log,
# private capture and state) for a failure's diagnosis.
cleanup() {
  [ -z "$origin_pid" ] || kill "$origin_pid" 2>/dev/null || true
  if [ "${BILLET_GATE_KEEP:-0}" = 1 ]; then echo "converge guard: the work directory is kept at $work"; else rd rm -rf "$work"; fi
}
trap cleanup EXIT

python=$(ansible --version 2>/dev/null | sed -n 's/.*python version.*(\(.*\)).*/\1/p' | head -n1)
if [ -z "$python" ] || [ ! -x "$python" ]; then
  python=$(command -v python3) || { echo "converge-guard-check: no python3" >&2; exit 1; }
fi
ansible_playbook=$(command -v ansible-playbook) || { echo "converge-guard-check: no ansible-playbook" >&2; exit 1; }
collections_path="$collection_root:$HOME/.ansible/collections:/usr/share/ansible/collections"

fail() {
  echo "FAIL: $1" >&2
  if [ -n "${2:-}" ] && [ -f "$2" ]; then
    if grep -Eq "^(fatal|failed): " "$2"; then
      grep -En -B8 -A25 -m1 "^(fatal|failed): " "$2" >&2
    else
      tail -n 60 "$2" >&2
    fi
  fi
  exit 1
}

# --- the module's own check -------------------------------------------------
"$python" "$here/guard_fallback_check.py"
"$python" "$here/strict_json_check.py"

# --- the guard runs first, the preparation second ---------------------------
first_task=$(grep -n '^- name:' "$role_tasks/main.yml" | head -1 | cut -d: -f1)
guard_task=$(grep -n '^- name: Refuse a converge that would destroy the job running it' "$role_tasks/main.yml" | head -1 | cut -d: -f1)
if [ -z "$guard_task" ] || [ "$first_task" != "$guard_task" ]; then
  fail "the converge guard is not the first task in main.yml (first task at line ${first_task:-none}, guard at line ${guard_task:-none})"
fi
second_task=$(grep -n '^- name:' "$role_tasks/main.yml" | sed -n 2p | cut -d: -f1)
prepare_task=$(grep -n '^- name: Prepare the exclusion before anything changes this host' "$role_tasks/main.yml" | head -1 | cut -d: -f1)
if [ -z "$prepare_task" ] || [ "$second_task" != "$prepare_task" ]; then
  fail "the exclusion's preparation is not the second task in main.yml (second at line ${second_task:-none}, preparation at line ${prepare_task:-none})"
fi
first_prep=$(grep '^- name:' "$role_tasks/prepare-exclusion.yml" | head -1)
case "$first_prep" in
  *"Refuse a converge that would destroy the job running it") ;;
  *) fail "prepare-exclusion.yml's first task is not the converge guard's import: $first_prep" ;;
esac
if [ -e "$here/../plugins/modules/guard_status.py" ] || [ -e "$here/guard_status_check.py" ]; then
  fail "the status module is gone from the role; its files must be gone from the collection"
fi
echo "ok   the guard is the first task in the role, the preparation the second, and the guard the preparation's first import"

# --- the fakes ---------------------------------------------------------------
fakes="$work/fakes"
bins="$work/bins"
mnt="$work/mnt"
mkdir -p "$fakes" "$bins" "$mnt" "$work/cases"

# THE FAKE billet FOR THE SIMULATED-DARWIN CASES: records every invocation and
# answers `prepare` from the committed corpus (the first call from
# BILLET_FAKE_PREPARE, later ones from BILLET_FAKE_PREPARE2), `version` per the
# environment, `settle` and `release --cleanup` per the environment, and
# nothing else.
cat >"$fakes/billet-fake" <<'FAKE'
#!/bin/bash
set -u
log=${BILLET_FAKE_LOG:-/dev/null}
{
  printf 'role=fake\nargv0=%s\nargv=%s\ncwd=%s\nuid=%s\n---\n' "$0" "$*" "$PWD" "$(id -u)"
} >>"$log"
case "${1:-}" in
  version) printf 'billet %s darwin/arm64\n' "${BILLET_FAKE_VERSION:-v0.10.0}"; exit 0 ;;
  converge-guard) ;;
  *) exit 0 ;;
esac
shift
if [ -n "${BILLET_FAKE_PRE_R:-}" ]; then echo 'unknown command "converge-guard"' >&2; exit 2; fi
if [ -n "${BILLET_FAKE_HANG:-}" ]; then sleep 3600; fi
answer() { # file
  cat "$1"
  case "$(basename "$1")" in refused-*) exit 2 ;; unknown*) exit 3 ;; *) exit 0 ;; esac
}
case "${1:-}" in
  prepare)
    n=$(( $(grep -c '^prepare$' "$log.count" 2>/dev/null || true) + 1 ))
    echo prepare >>"$log.count"
    if [ "$n" -le 1 ]; then answer "${BILLET_FAKE_PREPARE:?}"; else answer "${BILLET_FAKE_PREPARE2:?}"; fi ;;
  settle) exit "${BILLET_FAKE_SETTLE_STATUS:-0}" ;;
  release)
    if [ -n "${BILLET_FAKE_RELEASE_HANG:-}" ]; then sleep 3600; fi
    rm -rf "${BILLET_FAKE_ROOT:?}/active"
    exit "${BILLET_FAKE_RELEASE_STATUS:-0}" ;;
  status) printf '{"active": "none"}\n'; exit 0 ;;
  holder) echo 'unknown command "converge-guard"' >&2; exit 2 ;;
  *) echo "the fake does not emulate converge-guard ${1:-}" >&2; exit 1 ;;
esac
FAKE
chmod 0755 "$fakes/billet-fake"

# A hook that replaces the managed binary does it BY RENAME, as an updater
# does: a script running from that name keeps its inode.
# THE HOOK a wrapper or the pre-R fake fires: BILLET_GATE_HOOK=<cmd>:<n>:<script>
# runs <script> once, before the n-th invocation of <cmd> through any of them,
# and records that it fired.
hook_lines='
hook=${BILLET_GATE_HOOK:-}
if [ -n "$hook" ]; then
  case "$hook" in
    "$cmd:$n:"*) sh -c "${hook#"$cmd:$n:"}"; printf "hook=%s:%s fired\n" "$cmd" "$n" >>"$log" ;;
  esac
fi
'

# A MANAGED BINARY FROM BEFORE THE GUARD: answers `version`, and "unknown
# command" to every converge-guard subcommand; counts its calls with the
# wrappers and fires hooks like them.
{
  cat <<'FAKE'
#!/bin/bash
set -u
log=${BILLET_GATE_LOG:-/dev/null}
priv=${BILLET_GATE_PRIVATE:-/dev/null}
cmd=${1:-}
if [ "$cmd" = converge-guard ]; then cmd="${2:-}"; fi
n=1
if [ -f "$priv" ]; then n=$(( $(grep -c "^call=$cmd\$" "$priv" || true) + 1 )); fi
printf 'call=%s\n' "$cmd" >>"$priv"
printf 'role=pre-r\ninvocation=%s\ncmd=%s\nargv0=%s\nargv=%s\nuid=%s\n---\n' "$n" "$cmd" "$0" "$*" "$(id -u)" >>"$log"
FAKE
  printf '%s' "$hook_lines"
  cat <<'FAKE'
case "${1:-}" in
  version) echo "billet ${BILLET_GATE_PRE_R_VERSION:-0.9.1} linux/amd64"; exit 0 ;;
esac
echo 'unknown command "converge-guard"' >&2
hang=${BILLET_GATE_PRE_R_HANG:-}
if [ "$hang" = all ] || [ "$hang" = "$cmd" ]; then
  # IGNORING TERM keeps the fake alive through the bound's first signal, so
  # the kill after the grace period is what ends it, as a binary that
  # ignores TERM would be ended; the loop outlives each sleep the signal
  # reaches.
  if [ "${BILLET_GATE_PRE_R_IGNORE_TERM:-0}" = 1 ]; then trap '' TERM; while :; do sleep 1; done; fi
  sleep 3600
fi
exit 2
FAKE
} >"$fakes/billet-pre-r"
chmod 0755 "$fakes/billet-pre-r"
# A SECOND PRE-R BINARY with other bytes, for a pre-R candidate on a pre-R host.
{ cat "$fakes/billet-pre-r"; echo "# another build of the same release"; } >"$fakes/billet-pre-r-other"
chmod 0755 "$fakes/billet-pre-r-other"

cat >"$fakes/billet-hang" <<'FAKE'
#!/bin/bash
printf 'role=hang\nargv0=%s\nargv=%s\nuid=%s\n---\n' "$0" "$*" "$(id -u)" >>"${BILLET_GATE_LOG:-/dev/null}"
sleep 3600
FAKE
chmod 0755 "$fakes/billet-hang"

# THE RECORDING WRAPPER, one per backing binary and role. Its answer can be
# substituted (BILLET_GATE_ANSWER=<cmd>:<n>:<file>, several separated by `;`),
# dropped (BILLET_GATE_DROP_ANSWER=<cmd>:<n>), or the invocation refused in the
# wrapper with no backing run, hung, or crashed at a boundary of the seam
# (BILLET_GATE_FAIL=<cmd>:refuse | <cmd>:hang | <cmd>:<n>:unknown, the n-th
# invocation answering "unknown command" as a binary from before the guard
# would | <cmd>:<n>:exit:<code>, the real answer printed under another exit
# status | <cmd>:crash:<kind> <path>[:n]).
write_wrapper() { # path backing role
  {
    cat <<WRAP
#!/bin/bash
set -u
BACKING="$2"
ROLE="$3"
WRAP
    cat <<'WRAP'
log=${BILLET_GATE_LOG:-/dev/null}
priv=${BILLET_GATE_PRIVATE:-/dev/null}
cmd=${1:-}
if [ "$cmd" = converge-guard ]; then cmd="${2:-}"; fi
n=1
if [ -f "$priv" ]; then n=$(( $(grep -c "^call=$cmd\$" "$priv" || true) + 1 )); fi
printf 'call=%s\n' "$cmd" >>"$priv"
printf 'role=%s\ninvocation=%s\ncmd=%s\nargv0=%s\nargv=%s\nuid=%s\n---\n' "$ROLE" "$n" "$cmd" "$0" "$*" "$(id -u)" >>"$log"
WRAP
    printf '%s' "$hook_lines"
    cat <<'WRAP'
failspec=${BILLET_GATE_FAIL:-}
forced_exit=""
case "$failspec" in
  "$cmd:refuse") printf 'backing=0 cmd=%s\n' "$cmd" >>"$priv"; echo "billet: refused by the gate's wrapper; nothing was run" >&2; exit 1 ;;
  "$cmd:hang") printf 'backing=0 cmd=%s\n' "$cmd" >>"$priv"; sleep 3600 ;;
  "$cmd:$n:unknown") printf 'backing=0 cmd=%s\n' "$cmd" >>"$priv"; echo 'unknown command "converge-guard"' >&2; exit 2 ;;
  "$cmd:$n:exit:"*) forced_exit="${failspec#"$cmd:$n:exit:"}" ;;
  "$cmd:crash:"*) export BILLET_GUARD_CRASH_AT="${failspec#"$cmd:crash:"}" ;;
esac
out=$(mktemp); err=$(mktemp)
"$BACKING" "$@" >"$out" 2>"$err"
status=$?
printf 'backing=1 cmd=%s status=%s\n' "$cmd" "$status" >>"$priv"
{ printf '=== %s:%s stdout\n' "$cmd" "$n"; cat "$out"; printf '=== %s:%s stderr\n' "$cmd" "$n"; cat "$err"; printf '=== end\n'; } >>"$priv"
cat "$err" >&2
drop=${BILLET_GATE_DROP_ANSWER:-}
if [ "$drop" = "$cmd:$n" ]; then rm -f "$out" "$err"; exit "$status"; fi
substituted=""
IFS=';' read -r -a specs <<<"${BILLET_GATE_ANSWER:-}"
for spec in "${specs[@]+"${specs[@]}"}"; do
  case "$spec" in "$cmd:$n:"*) substituted="${spec#"$cmd:$n:"}" ;; esac
done
if [ -n "$substituted" ]; then cat "$substituted"; else cat "$out"; fi
rm -f "$out" "$err"
if [ -n "$forced_exit" ]; then exit "$forced_exit"; fi
exit "$status"
WRAP
  } >"$1"
  chmod 0755 "$1"
}

# RECORDING WRAPPERS around the tools the preparation runs, `systemctl`
# refusing (nothing in the preparation may call it), and a `sync` whose
# wrapper carries the one no-command hook (a fresh root's flush is the one
# thing the no-billet path runs between its two observations).
for tool in timeout date mkdir systemctl sync; do
  real=$(command -v "$tool" || true)
  if [ "$tool" = mkdir ]; then
    cat >"$fakes/$tool" <<FAKE
#!/bin/sh
case "\$1 \$2" in
  "-m 0700") printf 'role=mkdir\nargv0=%s\nargv=%s\nuid=%s\n---\n' "\$0" "\$*" "\$(id -u)" >>"\${BILLET_GATE_LOG:-/dev/null}" ;;
esac
exec "$real" "\$@"
FAKE
    chmod 0755 "$fakes/$tool"
    continue
  fi
  cat >"$fakes/$tool" <<FAKE
#!/bin/sh
if [ "$tool" = systemctl ] && [ "\${1:-}" = --version ]; then exec "$real" "\$@"; fi
printf 'role=$tool\nargv0=%s\nargv=%s\nuid=%s\n---\n' "\$0" "\$*" "\$(id -u)" >>"\${BILLET_GATE_LOG:-/dev/null}"
FAKE
  case $tool in
    date)
      cat >>"$fakes/$tool" <<FAKE
case "\${BILLET_FAKE_DATE_MODE:-}" in
  fail) exit 1 ;;
  short) echo 2026-09-09; exit 0 ;;
  frozen) echo "\${BILLET_FAKE_DATE_STAMP:-20260909T120000}"; exit 0 ;;
esac
exec "$real" "\$@"
FAKE
      ;;
    sync)
      cat >>"$fakes/$tool" <<FAKE
if [ -n "\${BILLET_GATE_SYNC_HOOK:-}" ]; then sh -c "\$BILLET_GATE_SYNC_HOOK"; printf 'hook=sync fired\n' >>"\${BILLET_GATE_LOG:-/dev/null}"; fi
exec "$real" "\$@"
FAKE
      ;;
    systemctl)
      echo 'echo "systemctl was called by the preparation" >&2; exit 97' >>"$fakes/$tool" ;;
    *)
      echo "exec \"$real\" \"\$@\"" >>"$fakes/$tool" ;;
  esac
  chmod 0755 "$fakes/$tool"
done

cat >"$fakes/suffix" <<'FAKE'
#!/bin/sh
printf 'role=suffix\nargv0=%s\nargv=%s\nuid=%s\n---\n' "$0" "$*" "$(id -u)" >>"${BILLET_GATE_LOG:-/dev/null}"
seq=${BILLET_FAKE_SUFFIXES:-}
if [ -n "$seq" ] && [ -s "$seq" ]; then
  head -n1 "$seq"
  tail -n +2 "$seq" >"$seq.next" && mv "$seq.next" "$seq"
  exit "${BILLET_FAKE_SUFFIX_STATUS:-0}"
fi
od -An -N4 -tx1 /dev/urandom | tr -d ' \n'
exit "${BILLET_FAKE_SUFFIX_STATUS:-0}"
FAKE
chmod 0755 "$fakes/suffix"

# THE CORPUS, RE-ADDRESSED AND CORRUPTED: the committed answers name the
# holder `ci-1` and the packaged paths; a substituted answer carries this
# gate's holder, and a corruption changes exactly one member of it.
readdress() { # in out holder
  "$python" - "$1" "$2" "$3" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
if "holder" in d:
    d["holder"] = sys.argv[3]
json.dump(d, open(sys.argv[2], "w"), indent=2)
PY
}
corrupt() { # in out member
  "$python" - "$1" "$2" "$3" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
m = sys.argv[3]
bad = {"outcome": 7, "id": "zz", "holder": "h2", "preparing": "yes", "token": "short",
       "record": {}, "pointer": "no", "managed": {"present": "maybe"}, "downgrade": "x",
       "next": None, "shape": 7}
d[m] = bad[m]
json.dump(d, open(sys.argv[2], "w"), indent=2)
PY
}

# --- the plays ---------------------------------------------------------------
cat >"$work/inventory.ini" <<'INV'
[billet_hosts]
localhost ansible_connection=local
INV

# ONE PLAY FOR EVERY CASE: the entry point the case names, an optional second
# inclusion with a command between them, and the facts the gate reads from an
# `always` section, so a refusal's facts are read too. The token is never
# printed; whether one is known is.
gate_play_body() {
  cat <<'PLAY'
  tasks:
    - name: The case
      block:
        - name: Prove the identity this case runs under
          ansible.builtin.command:
            argv: [id, -u]
          register: billet_gate_uid
          changed_when: false
          check_mode: false
          become: false

        - name: Refuse the wrong identity for this case
          ansible.builtin.assert:
            that:
              - (billet_gate_expect_uid | string) == (billet_gate_uid.stdout | trim)
            fail_msg: "this case runs as uid {{ billet_gate_uid.stdout | trim }}, want {{ billet_gate_expect_uid }}"

        - name: Run the entry under test
          ansible.builtin.include_role:
            name: junioryono.billet.host
            tasks_from: "{{ billet_gate_entry }}"

        - name: Change the host between two inclusions
          ansible.builtin.command:
            argv: [/bin/sh, -c, "{{ billet_gate_between }}"]
          changed_when: true
          check_mode: false
          when: billet_gate_between | length > 0

        - name: Run the entry under test again
          ansible.builtin.include_role:
            name: junioryono.billet.host
            tasks_from: "{{ billet_gate_entry }}"
          when: billet_gate_inclusions | int >= 2
      always:
        - name: Report the facts the gate reads
          ansible.builtin.debug:
            msg: >-
              GATE shape={{ billet_upgrade_claim_shape | default('undef') }}
              interrupted={{ billet_interrupted_upgrade | default('undef') }}
              recovery={{ billet_upgrade_recovery_dir | default('undef') }}
              upgrade={{ billet_binary_upgrade | default('undef') }}
              held={{ billet_exclusion_held | default('undef') }}
              acquired={{ billet_exclusion_acquired | default('undef') }}
              released={{ billet_exclusion_released | default('undef') }}
              token_known={{ (billet_exclusion_cleanup_token | default('')) | length > 0 }}
              id={{ billet_exclusion_id | default('undef') }}
              route={{ billet_exclusion_route | default('undef') }}
              executable={{ billet_exclusion_executable | default('undef') }}
              holder={{ billet_exclusion_holder | default('undef') }}
              adopted={{ billet_exclusion_adopted | default('undef') }}
              version={{ billet_version | default('undef') }}
              resolved={{ billet_resolved_version | default('undef') }}
              end=.
PLAY
}
{
  cat <<'PLAY'
---
- name: Exercise the host role's preparation
  hosts: billet_hosts
  gather_facts: "{{ billet_gate_facts | default(false) | bool }}"
  vars:
    billet_gate_entry: prepare-exclusion
    billet_gate_inclusions: 1
    billet_gate_between: ""
PLAY
  gate_play_body
} >"$work/play.yml"

# TWO PLAYS OVER ONE HOST in one process: the facts of the first survive into
# the second, which is the rerun shape a fleet playbook's plays have.
{
  cat <<'PLAY'
---
- name: The first play over the host
  hosts: billet_hosts
  gather_facts: "{{ billet_gate_facts | default(false) | bool }}"
  vars:
    billet_gate_entry: prepare-exclusion
    billet_gate_inclusions: 1
    billet_gate_between: ""
PLAY
  gate_play_body
  cat <<'PLAY'

- name: The second play over the same host
  hosts: billet_hosts
  gather_facts: false
  vars:
    billet_gate_entry: prepare-exclusion
    billet_gate_inclusions: 1
    billet_gate_between: ""
  pre_tasks:
    - name: Change the host between two plays
      ansible.builtin.command:
        argv: [/bin/sh, -c, "{{ billet_gate_between_plays | default('') }}"]
      changed_when: true
      check_mode: false
      when: billet_gate_between_plays | default('') | length > 0
PLAY
  gate_play_body
} >"$work/play2.yml"

cat >"$work/probe.yml" <<'PLAY'
---
- name: Prove the gate's launch
  hosts: billet_hosts
  gather_facts: false
  tasks:
    - name: Run the managed wrapper under escalation
      ansible.builtin.command:
        argv: [/usr/bin/billet, version]
      changed_when: false
      become: true
    - name: Load the role's defaults
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: converge-guard
        public: true
    - name: Report the holder the role default resolved
      ansible.builtin.debug:
        msg: "GATE holder={{ billet_converge_guard_holder }}"
PLAY

# --- the helpers over a case's output ----------------------------------------
HOLDER=h1
RUNNER=""
failed_at() {
  awk '/^TASK \[/ { t=$0; sub(/^TASK \[/, "", t); sub(/\] \*+$/, "", t); sub(/^junioryono\.billet\.host : /, "", t) }
       /^(fatal|failed): / { print t; exit }' "$work/cases/$1/out"
}
# The last fatal result with the lines that belong to it (under -vvv the
# message follows the fatal line rather than sitting on it).
final_fatal() {
  awk '/^(fatal|failed): / { buf = ""; on = 1 } on { buf = buf $0 "\n" } /^(PLAY RECAP|TASK \[)/ { on = 0 } END { printf "%s", buf }' "$work/cases/$1/out"
}
expect_refused() { # case task fragment...
  local name=$1 task=$2; shift 2
  [ "$status" -ne 0 ] || fail "$name: the play succeeded, want a refusal at \"$task\"" "$work/cases/$name/out"
  local at
  at=$(failed_at "$name")
  [ "$at" = "$task" ] || fail "$name: failed at \"$at\", want \"$task\"" "$work/cases/$name/out"
  for frag in "$@"; do
    grep -qF -- "$frag" "$work/cases/$name/out" || fail "$name: the refusal does not say: $frag" "$work/cases/$name/out"
  done
  echo "ok   $name: refused at \"$task\""
}
# expect_final CASE FRAGMENT...: the rescue's re-failure, the last fatal line.
expect_final() {
  local name=$1; shift
  local line
  line=$(final_fatal "$name")
  for frag in "$@"; do
    printf '%s' "$line" | grep -qF -- "$frag" || fail "$name: the final refusal does not say: $frag" "$work/cases/$name/out"
  done
}
expect_allowed() {
  [ "$status" -eq 0 ] || fail "$1: the play failed at \"$(failed_at "$1")\", want success" "$work/cases/$1/out"
  echo "ok   $1: allowed"
}
facts() { grep -o 'GATE shape=.* end=\.' "$work/cases/$1/out" | tail -1; }
fact() { facts "$1" | tr ' ' '\n' | sed -n "s/^$2=//p" | tail -1; }
expect_fact() { # case key value
  local got
  got=$(fact "$1" "$2")
  [ "$got" = "$3" ] || fail "$1: $2=$got, want $3" "$work/cases/$1/out"
}
# The log's records as "role: argv @argv0" lines, in order.
calls() { # case [role]
  [ -f "$work/cases/$1/log" ] || return 0
  awk -v want="${2:-}" '/^role=/ {r=substr($0,6)} /^argv=/ {a=substr($0,6)} /^argv0=/ {z=substr($0,7)} /^---/ { if (want=="" || r==want) print r ": " a " @" z }' "$work/cases/$1/log"
}
count_calls() { calls "$1" "$2" | grep -cF -- "$3" || true; }
# The binaries' invocations as "role cmd" lines, in order.
commands() { # case
  awk '/^role=/ {r=substr($0,6)} /^cmd=/ {c=substr($0,5)} /^---/ { if (r=="managed"||r=="candidate"||r=="pre-r") print r, c }' "$work/cases/$1/log"
}
expect_calls() { # case role fragment count
  local n
  n=$(count_calls "$1" "$2" "$3")
  [ "$n" -eq "$4" ] || fail "$1: $2 '$3' called $n times, want $4" "$work/cases/$1/log"
}
expect_no_task() { # case task
  local verdict
  verdict=$(awk -v want="TASK [junioryono.billet.host : $2]" '
    index($0, want) == 1 { seen = 1; next }
    seen && /^skipping: / { seen = 0; next }
    seen && /^(ok|changed|fatal|failed): / { print "ran"; exit }
    seen && /^TASK \[/ { seen = 0 }' "$work/cases/$1/out")
  [ "$verdict" != ran ] || fail "$1: the task \"$2\" ran, and it must not" "$work/cases/$1/out"
}
expect_ran() { # case task
  local verdict
  verdict=$(awk -v want="TASK [junioryono.billet.host : $2]" '
    index($0, want) == 1 { seen = 1; next }
    seen && /^skipping: / { seen = 0; next }
    seen && /^(ok|changed): / { print "ran"; exit }
    seen && /^TASK \[/ { seen = 0 }' "$work/cases/$1/out")
  [ "$verdict" = ran ] || fail "$1: the task \"$2\" did not run" "$work/cases/$1/out"
}
log_empty() { [ ! -s "$work/cases/$1/log" ] || fail "$1: a fake was called, and none may be" "$work/cases/$1/log"; }
# state CASE KEY: one line of the namespace's state dump.
state() { sed -n "s/^$2=//p" "$work/cases/$1/state" | head -n1; }
expect_state() { # case key value
  local got
  got=$(state "$1" "$2")
  [ "$got" = "$3" ] || fail "$1: state $2=$got, want $3" "$work/cases/$1/state"
}
# The tokens and ids the case's private capture carries.
tokens_of() { sed -n 's/^ *"token": "\([0-9a-f]\{32\}\)".*/\1/p' "$work/cases/$1/private" 2>/dev/null | sort -u; }
id_of() { sed -n 's/^ *"id": "\([0-9a-f]\{32\}\)".*/\1/p' "$work/cases/$1/private" | head -n1; }
# The number of backing invocations of one command, from the private capture.
backing_runs() { # case cmd
  grep -c "^backing=1 cmd=$2 " "$work/cases/$1/private" || true
}
# THE SENTINEL: no token an `acquired` answer carried reaches the play's output.
check_sentinel() {
  local t
  for t in $(tokens_of "$1"); do
    if grep -qF "$t" "$work/cases/$1/out"; then fail "$1: the token reached the play's output" "$work/cases/$1/out"; fi
  done
}
# The hook a case declared must have fired.
check_hook() {
  if grep -q '^BILLET_GATE_HOOK=' "$work/cases/$1/env" 2>/dev/null; then
    grep -q '^hook=.* fired$' "$work/cases/$1/log" || fail "$1: the case's hook never fired" "$work/cases/$1/log"
  fi
  if grep -q '^BILLET_GATE_SYNC_HOOK=' "$work/cases/$1/env" 2>/dev/null; then
    grep -q '^hook=sync fired$' "$work/cases/$1/log" || fail "$1: the root-flush hook never fired" "$work/cases/$1/log"
  fi
}

# =============================================================================
# The converge guard's own cases (the file's original purpose), unescalated:
# the guard reads RUNNER_NAME on the controller.
# =============================================================================
# plant_plain CASE: a temporary tree for the unescalated launch (the guard's
# cases and the simulated-darwin cases, on the fake billet).
plant_plain() {
  local case_dir=$work/cases/$1
  rm -rf "$case_dir"
  mkdir -p "$case_dir/bin" "$case_dir/lib/billet/upgrades" "$case_dir/src"
  cp "$fakes/billet-fake" "$case_dir/bin/billet"
  cp "$fakes/billet-fake" "$case_dir/src/billet"
  chmod 0755 "$case_dir/lib/billet"
  chmod 0700 "$case_dir/lib/billet/upgrades"
}
# run_plain CASE [ENV...] -- [PLAY ARGS...]
run_plain() {
  local name=$1; shift
  local case_dir=$work/cases/$name
  mkdir -p "$case_dir"
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  : >"$case_dir/out"; : >"$case_dir/log"; rm -f "$case_dir/log.count"
  set +e
  env PATH="$fakes:$PATH" HOME="$HOME" ANSIBLE_COLLECTIONS_PATH="$collections_path" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_NOCOLOR=1 ANSIBLE_FORCE_COLOR=0 \
    ANSIBLE_LOCAL_TEMP="$case_dir/tmp" ANSIBLE_REMOTE_TEMP="$case_dir/tmp" \
    RUNNER_NAME="$RUNNER" BILLET_CONVERGE_GUARD_HOLDER="$HOLDER" \
    BILLET_FAKE_LOG="$case_dir/log" BILLET_FAKE_ROOT="$case_dir/lib/billet/upgrades" \
    BILLET_FAKE_PREPARE="$work/corpus/acquired.json" BILLET_FAKE_PREPARE2="$work/corpus/no-change.json" \
    "${envs[@]+"${envs[@]}"}" \
    "$ansible_playbook" -i "$work/inventory.ini" "$work/play.yml" \
    -e billet_upgrade_root="$case_dir/lib/billet/upgrades" \
    -e billet_managed_binary="$case_dir/bin/billet" \
    -e ansible_become=false \
    -e billet_gate_expect_uid="$(id -u)" \
    "$@" >"$case_dir/out" 2>&1
  status=$?
  set -e
}
mkdir -p "$work/corpus"
for f in acquired no-change validated-settled refused-downgrade; do
  readdress "$fixtures/$f.json" "$work/corpus/$f.json" "$HOLDER"
done
corrupt "$work/corpus/no-change.json" "$work/corpus/no-change-outcome.json" outcome

plant_plain guard-refused
RUNNER="billet-lease-abc123"; run_plain guard-refused -- -e billet_gate_entry=converge-guard; RUNNER=""
expect_refused guard-refused "Refuse a converge driven from a billet-managed runner" "runner billet itself manages"
plant_plain guard-override
RUNNER="billet-lease-abc123"; run_plain guard-override -- -e billet_gate_entry=converge-guard -e billet_allow_converge_from_billet_runner=true; RUNNER=""
expect_allowed guard-override
plant_plain guard-plain
RUNNER="gh-deploy-runner-1"; run_plain guard-plain -- -e billet_gate_entry=converge-guard; RUNNER=""
expect_allowed guard-plain
plant_plain guard-workstation
run_plain guard-workstation -- -e billet_gate_entry=converge-guard
expect_allowed guard-workstation
plant_plain guard-substring
RUNNER="ci-billet-deploy"; run_plain guard-substring -- -e billet_gate_entry=converge-guard; RUNNER=""
expect_allowed guard-substring
plant_plain guard-in-preparation
RUNNER="billet-lease-abc123"; run_plain guard-in-preparation --; RUNNER=""
expect_refused guard-in-preparation "Refuse a converge driven from a billet-managed runner" "runner billet itself manages"
expect_no_task guard-in-preparation "Inspect the durable claim"
log_empty guard-in-preparation
plant_plain no-holder
HOLDER=""; run_plain no-holder --; HOLDER=h1
expect_refused no-holder "Refuse a converge without a holder" "BILLET_CONVERGE_GUARD_HOLDER"
log_empty no-holder
echo "ok   the converge guard refuses a billet-managed runner before the preparation, and a converge needs a holder"

# =============================================================================
# THE SIMULATED-DARWIN CASES, unescalated, on the fake with the role's path
# overrides: the calls run as the agent's account under the async bound with
# no `timeout`, the staging stages nothing, and the cleanup releases.
# =============================================================================
me=$(id -u)
plant_plain p15-a
rm -rf "$work/cases/p15-a/lib" "$work/cases/p15-a/bin/billet"
run_plain p15-a -- -e billet_exclusion_platform=Darwin
expect_allowed p15-a
expect_fact p15-a route unheld
expect_fact p15-a held undef
expect_ran p15-a "Inspect the managed binary and the claim again before an unheld converge"
log_empty p15-a
[ ! -e "$work/cases/p15-a/lib" ] || fail "p15-a: a Mac with no billet had a root made for it"
plant_plain p15-d
run_plain p15-d -- -e billet_exclusion_platform=Darwin
expect_allowed p15-d
expect_fact p15-d acquired True
expect_fact p15-d held True
expect_calls p15-d fake "converge-guard prepare --validate --holder h1 --json" 1
expect_calls p15-d fake "converge-guard prepare --holder h1 --json --no-change" 1
expect_calls p15-d fake "converge-guard settle --holder h1 --token" 1
grep -q "^uid=$me$" "$work/cases/p15-d/log" || fail "p15-d: the calls did not run as the agent's account"
expect_calls p15-d timeout "" 0
grep -q "ASYNC" "$work/cases/p15-d/out" || fail "p15-d: the darwin calls did not run under the task's async bound" "$work/cases/p15-d/out"
plant_plain p15-e
run_plain p15-e BILLET_FAKE_HANG=1 -- -e billet_exclusion_platform=Darwin -e billet_guard_timeout=1
expect_refused p15-e "Refuse a preparation that did not answer" "ended by the bound"
plant_plain p15-f
run_plain p15-f -- -e billet_exclusion_platform=Darwin -e billet_binary_src="$work/cases/p15-f/src/billet" -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_allowed p15-f
expect_calls p15-f suffix "" 0
expect_calls p15-f fake "--candidate" 0
# C12: a simulated Mac acquires, the second answer is corrupted, the cleanup
# runs unescalated with no `timeout` and the async bound, and releases.
plant_plain p15-h
mkdir -p "$work/cases/p15-h/lib/billet/upgrades/active"
run_plain p15-h BILLET_FAKE_PREPARE2="$work/corpus/no-change-outcome.json" -- -e billet_exclusion_platform=Darwin
expect_refused p15-h "Judge the preparation's answer" "outcome"
expect_final p15-h "the cleanup released the guard" "is now none"
expect_calls p15-h fake "converge-guard release --holder h1 --cleanup --token" 1
expect_calls p15-h timeout "" 0
expect_fact p15-h released True
[ ! -e "$work/cases/p15-h/lib/billet/upgrades/active" ] || fail "p15-h: the guard remains after the darwin cleanup"
plant_plain p15-h2
mkdir -p "$work/cases/p15-h2/lib/billet/upgrades/active"
run_plain p15-h2 BILLET_FAKE_PREPARE2="$work/corpus/no-change-outcome.json" BILLET_FAKE_RELEASE_HANG=1 -- -e billet_exclusion_platform=Darwin -e billet_guard_timeout=1
expect_refused p15-h2 "Judge the preparation's answer" "outcome"
expect_final p15-h2 "ended by its bound"
expect_fact p15-h2 released False
echo "ok   P15: a Mac calls prepare as the agent's account under the async bound, stages nothing, takes the no-billet path, and cleans up without timeout"

if [ "$have_root" = 0 ]; then
  if [ "${BILLET_GATE_REQUIRE_ROOT:-0}" = 1 ]; then
    fail "BILLET_GATE_REQUIRE_ROOT=1 and sudo -n is not available; the namespace cases cannot run"
  fi
  echo "converge guard: the guard's and the darwin cases pass; the namespace cases were skipped (no sudo -n)"
  exit 0
fi
if [ "$(uname -s)" != Linux ]; then
  if [ "${BILLET_GATE_REQUIRE_ROOT:-0}" = 1 ]; then
    fail "BILLET_GATE_REQUIRE_ROOT=1 on $(uname -s); the namespace cases need Linux"
  fi
  echo "converge guard: the guard's and the darwin cases pass; the namespace cases need Linux and were skipped"
  exit 0
fi
if ! command -v go >/dev/null 2>&1; then
  if [ "${BILLET_GATE_REQUIRE_ROOT:-0}" = 1 ]; then fail "no go on PATH; the backing binaries cannot be built"; fi
  echo "converge guard: no go on PATH; the namespace cases were skipped"
  exit 0
fi
invoker_uid=$(id -u); invoker_gid=$(id -g)
[ "$invoker_uid" != 0 ] || fail "the gate must be invoked as a non-root account (its unescalated cases run as the invoker)"

# =============================================================================
# THE BACKING BINARIES, built once from this checkout, and their wrappers.
# =============================================================================
echo "building the backing binaries ..."
for v in v0.10.0 v0.10.1 v0.9.0; do
  (cd "$repo_root" && go build -tags billetgatecrash -ldflags "-X github.com/junioryono/billet/internal/version.version=$v" -o "$bins/billet-$v" ./cmd/billet)
done
for v in v0.10.0 v0.10.1 v0.9.0; do
  write_wrapper "$bins/wrap-managed-$v" "$mnt/bin/billet-$v" managed
  write_wrapper "$bins/wrap-candidate-$v" "$mnt/bin/billet-$v" candidate
done
# A candidate that carries the pre-R fake's bytes: the floor's case.
cp "$fakes/billet-pre-r-other" "$bins/wrap-candidate-pre-r"

# A FAKE RELEASE ORIGIN for the pinned cases: v0.10.1's archive with its
# checksums.txt holds the candidate wrapper; v0.10.2 is not published.
origin=$work/origin
mkdir -p "$origin/v0.10.1"
arch=$(uname -m); case $arch in x86_64) rel_arch=amd64 ;; aarch64|arm64) rel_arch=arm64 ;; *) rel_arch=$arch ;; esac
(cd "$bins" && cp wrap-candidate-v0.10.1 billet && tar -czf "$origin/v0.10.1/billet_0.10.1_linux_${rel_arch}.tar.gz" billet && rm -f billet)
(cd "$origin/v0.10.1" && sha256sum "billet_0.10.1_linux_${rel_arch}.tar.gz" >checksums.txt)
"$python" -u -m http.server --bind 127.0.0.1 --directory "$origin" 0 >"$work/origin.log" 2>&1 &
origin_pid=$!
port=""
for _ in $(seq 1 100); do
  port=$(sed -n 's/.*port \([0-9]*\).*/\1/p' "$work/origin.log" | head -1)
  [ -n "$port" ] && break
  sleep 0.1
done
[ -n "$port" ] || fail "the fake release origin did not start" "$work/origin.log"
origin_url="http://127.0.0.1:$port"

# =============================================================================
# THE NAMESPACE RUNNER. A case is a directory holding plant.sh (run inside the
# namespace as root before the play), env (one NAME=value per line, given to
# the play), args (the play's arguments, one per line), and optionally post.sh
# (run inside the namespace as root after the play, its output to `post`).
# After the play the namespace dumps the tree's state to `state` and copies
# the two upper directories out for diagnosis.
# =============================================================================
cat >"$work/ns-lib.sh" <<'LIB'
# Sourced inside the namespace by plant and post scripts: the tree at its real paths.
ROOT=/var/lib/billet/upgrades
plant_root() { mkdir -p /var/lib/billet; chmod 0755 /var/lib/billet; chown root:root /var/lib/billet; mkdir -p "$ROOT"; chmod 0700 "$ROOT"; chown root:root "$ROOT"; }
plant_managed() { cp "$BINS/wrap-managed-${1:-v0.10.0}" /usr/bin/billet; chmod 0755 /usr/bin/billet; chown root:root /usr/bin/billet; }
plant_managed_file() { cp "$1" /usr/bin/billet; chmod 0755 /usr/bin/billet; chown root:root /usr/bin/billet; }
plant_pre_r() { plant_managed_file "$FAKES/billet-pre-r"; }
plant_recovery() { mkdir -p "$ROOT/$1"; chmod 0700 "$ROOT/$1"; cp "$BINS/wrap-candidate-${2:-v0.10.1}" "$ROOT/$1/billet.candidate"; chmod 0755 "$ROOT/$1/billet.candidate"; }
# plant_guard HOLDER EXE [settled|preparing|legacy] [ID]
plant_guard() {
  mkdir -p "$ROOT/active"; chmod 0700 "$ROOT/active"
  "$PYTHON" - "$1" "$2" "${3:-settled}" "${4:-}" "$ROOT" <<'PY'
import hashlib, json, os, sys
holder, exe, mode, ident, root = sys.argv[1:6]
digest = hashlib.sha256(open(exe, "rb").read()).hexdigest()
rec = {"holder": holder, "claimed_at": "2026-09-09T12:00:00Z", "hostname": "billet-control-01",
       "release_executable": exe, "release_executable_sha256": digest}
if mode != "legacy":
    rec["id"] = ident or "0123456789abcdef0123456789abcdef"
    if mode == "preparing":
        rec["preparing"] = True
        rec["token"] = "fedcba9876543210fedcba9876543210"
with open(root + "/active/guard.json", "w") as f:
    json.dump(rec, f, indent=2); f.write("\n")
os.chmod(root + "/active/guard.json", 0o600)
PY
}
plant_pointer() { ln -sT "$ROOT/$1" "$ROOT/active/recovery"; }
LIB

cat >"$work/ns-run.sh" <<'NSRUN'
#!/bin/bash
# Inside the namespace, as root: the mounts, the backing binaries, the plant,
# the play, the post script, the state dump, the copy-out.
set -u
case_dir=$1; mode=$2; play=$3
export BINS PYTHON FAKES ROOT=/var/lib/billet/upgrades
mount -t tmpfs tmpfs "$MNT" || exit 90
mkdir -p "$MNT/ub-upper" "$MNT/ub-work" "$MNT/vl-upper" "$MNT/vl-work" "$MNT/bin"
mount -t overlay overlay -o "lowerdir=/usr/bin,upperdir=$MNT/ub-upper,workdir=$MNT/ub-work" /usr/bin || exit 91
mount -t overlay overlay -o "lowerdir=/var/lib,upperdir=$MNT/vl-upper,workdir=$MNT/vl-work" /var/lib || exit 92
cp "$BINS"/billet-v* "$MNT/bin/" && chmod 0755 "$MNT/bin"/* && chown root:root "$MNT/bin"/*
rm -f /usr/bin/billet
rm -rf /var/lib/billet
. "$NSLIB"
if [ -s "$case_dir/plant.sh" ]; then
  if ! (set -e; . "$case_dir/plant.sh"); then echo "plant failed" >"$case_dir/state"; exit 94; fi
fi
: >"$case_dir/log"; : >"$case_dir/private"; : >"$case_dir/out"
chown "$INVOKER_UID:$INVOKER_GID" "$case_dir/log" "$case_dir/private" "$case_dir/out"
envs=(PATH="$FAKES:$PATH" ANSIBLE_COLLECTIONS_PATH="$COLLECTIONS" \
  ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_NOCOLOR=1 ANSIBLE_FORCE_COLOR=0 \
  ANSIBLE_LOCAL_TEMP="$case_dir/tmp" ANSIBLE_REMOTE_TEMP="$case_dir/tmp" \
  RUNNER_NAME="$RUNNER" BILLET_CONVERGE_GUARD_HOLDER="$HOLDER" \
  BILLET_GATE_LOG="$case_dir/log" BILLET_GATE_PRIVATE="$case_dir/private")
while IFS= read -r line; do [ -n "$line" ] && envs+=("$line"); done <"$case_dir/env"
mapfile -t args <"$case_dir/args"
mkdir -p "$case_dir/tmp"
if [ "$mode" = escalated ]; then
  env "${envs[@]}" HOME="$HOME_DIR" "$ANSIBLE_PLAYBOOK" -i "$INVENTORY" "$play" -e ansible_become=false -e billet_gate_expect_uid=0 "${args[@]+"${args[@]}"}" >"$case_dir/out" 2>&1
else
  chown -R "$INVOKER_UID:$INVOKER_GID" "$case_dir/tmp"
  setpriv --reuid="$INVOKER_UID" --regid="$INVOKER_GID" --init-groups env "${envs[@]}" HOME="$HOME_DIR" "$ANSIBLE_PLAYBOOK" -i "$INVENTORY" "$play" -e ansible_become=false -e "billet_gate_expect_uid=$INVOKER_UID" "${args[@]+"${args[@]}"}" >"$case_dir/out" 2>&1
fi
echo "$?" >"$case_dir/status"
if [ -s "$case_dir/post.sh" ]; then
  (. "$case_dir/post.sh") >"$case_dir/post" 2>&1
  echo "post=$?" >>"$case_dir/post"
fi
# THE STATE DUMP, read by the outer assertions.
{
  t() { if [ -L "$1" ]; then echo symlink; elif [ -d "$1" ]; then echo dir; elif [ -f "$1" ]; then echo file; elif [ -e "$1" ]; then echo other; else echo absent; fi; }
  echo "managed=$(t /usr/bin/billet)"
  echo "managed_sha=$( [ -f /usr/bin/billet ] && sha256sum /usr/bin/billet | cut -d' ' -f1 )"
  echo "root=$(t $ROOT)"
  echo "parent=$(t /var/lib/billet)"
  echo "root_mode=$( [ -d $ROOT ] && stat -c %a:%u $ROOT )"
  echo "parent_mode=$( [ -d /var/lib/billet ] && stat -c %a:%u /var/lib/billet )"
  echo "active=$(t $ROOT/active)"
  echo "record=$(t $ROOT/active/guard.json)"
  echo "record_tmp=$(t $ROOT/active/guard.json.tmp)"
  echo "pointer=$(t $ROOT/active/recovery)"
  echo "entries=$( [ -d $ROOT ] && ls -A $ROOT | tr '\n' ' ' )"
  echo "active_entries=$( [ -d $ROOT/active ] && ls -A $ROOT/active | tr '\n' ' ' )"
  if [ -f $ROOT/active/guard.json ]; then
    "$PYTHON" - <<'PY'
import json
r = json.load(open("/var/lib/billet/upgrades/active/guard.json"))
for k in ("holder", "id", "release_executable", "release_executable_sha256", "claimed_at"):
    print("record_%s=%s" % (k, r.get(k, "")))
print("record_preparing=%s" % r.get("preparing", False))
print("record_has_token=%s" % ("token" in r))
PY
  fi
  for d in $ROOT/recovery-* $ROOT/[0-9]*; do
    [ -d "$d" ] || continue
    echo "recovery=$(basename "$d") $( [ -f "$d/billet.candidate" ] && sha256sum "$d/billet.candidate" | cut -d' ' -f1 )"
  done
  echo "status_json=$("$MNT/bin/billet-v0.10.0" converge-guard status --json 2>/dev/null | tr -d '\n ')"
} >"$case_dir/state" 2>/dev/null
mkdir -p "$case_dir/upper" && cp -a "$MNT/ub-upper" "$case_dir/upper/usr-bin" 2>/dev/null; cp -a "$MNT/vl-upper" "$case_dir/upper/var-lib" 2>/dev/null
chown -R "$INVOKER_UID:$INVOKER_GID" "$case_dir" 2>/dev/null || true
exit 0
NSRUN
chmod 0755 "$work/ns-run.sh"

# plant CASE: start a case; p/e/a/post append plant lines, environment lines,
# play arguments and post lines to it.
plant() {
  local case_dir=$work/cases/$1
  rd rm -rf "$case_dir"
  mkdir -p "$case_dir"
  : >"$case_dir/plant.sh"; : >"$case_dir/env"; : >"$case_dir/args"; : >"$case_dir/post.sh"
}
p() { printf '%s\n' "$2" >>"$work/cases/$1/plant.sh"; }
e() { printf '%s\n' "$2" >>"$work/cases/$1/env"; }
a() { local name=$1; shift; for x in "$@"; do printf '%s\n' "$x" >>"$work/cases/$name/args"; done; }
post() { printf '%s\n' "$2" >>"$work/cases/$1/post.sh"; }
# ns_case CASE MODE [PLAY]: MODE escalated (the play runs as root) or
# unescalated (as the invoker); PLAY play (default) or play2.
ns_case() {
  local name=$1 mode=$2 play=${3:-play}
  local case_dir=$work/cases/$name
  sudo -n env BINS="$bins" FAKES="$fakes" PYTHON="$python" NSLIB="$work/ns-lib.sh" HOME_DIR="$HOME" MNT="$mnt" \
    COLLECTIONS="$collections_path" RUNNER="$RUNNER" HOLDER="$HOLDER" INVOKER_UID="$invoker_uid" INVOKER_GID="$invoker_gid" \
    ANSIBLE_PLAYBOOK="$ansible_playbook" INVENTORY="$work/inventory.ini" \
    unshare -m --propagation private /bin/bash "$work/ns-run.sh" "$case_dir" "$mode" "$work/$play.yml"
  local rc=$?
  [ "$rc" -eq 0 ] || fail "$name: the namespace runner failed ($rc): $(cat "$case_dir/state" 2>/dev/null)"
  status=$(cat "$case_dir/status")
  check_sentinel "$name"
  check_hook "$name"
}

# =============================================================================
# M8: the namespace launch is proved before it is relied on.
# =============================================================================
plant launch
p launch 'plant_root; plant_managed v0.10.0'
HOLDER=from-the-environment ns_case launch escalated probe; HOLDER=h1
[ "$status" -eq 0 ] || fail "M8: the probe play failed" "$work/cases/launch/out"
grep -q '^uid=0$' "$work/cases/launch/log" || fail "M8: the wrapper did not run as root under become" "$work/cases/launch/log"
grep -q 'argv0=/usr/bin/billet' "$work/cases/launch/log" || fail "M8: the managed wrapper is not at /usr/bin/billet" "$work/cases/launch/log"
grep -q 'billet v0.10.0' "$work/cases/launch/private" || fail "M8: the backing binary did not answer version v0.10.0" "$work/cases/launch/private"
grep -qF "GATE holder=from-the-environment" "$work/cases/launch/out" || fail "M8: the role default did not resolve the holder from the environment" "$work/cases/launch/out"
expect_state launch managed file
[ ! -e /usr/bin/billet ] || fail "M8: /usr/bin/billet leaked outside the namespace"
[ ! -e /var/lib/billet ] || fail "M8: /var/lib/billet leaked outside the namespace"
echo "ok   M8: the namespace launch runs the managed wrapper as root at its real path, resolves the holder from the environment, and leaks nothing"

ROOT=/var/lib/billet/upgrades
REC_A=recovery-20260909T120000-0badcafe
REC_B=recovery-20260909T120000-1badcafe
LEGACY_DIR=20260909T120000000000000

# =============================================================================
# B. The role's order and its routes.
# =============================================================================
# B1. A capable host, a candidate with a differing digest: acquired, staged,
# re-bound to the candidate, settled; the role reads no version or status of
# its own.
plant b1-first
p b1-first 'plant_root; plant_managed v0.10.0'
a b1-first -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case b1-first escalated
expect_allowed b1-first
expect_fact b1-first acquired True
expect_fact b1-first held True
expect_fact b1-first upgrade True
expect_fact b1-first route managed
expect_calls b1-first managed "converge-guard prepare --validate --holder h1 --json" 1
expect_calls b1-first managed "converge-guard prepare --holder h1 --json --candidate $(fact b1-first recovery)/billet.candidate" 1
expect_calls b1-first managed "converge-guard settle --holder h1 --token" 1
expect_calls b1-first managed "converge-guard hold" 0
expect_calls b1-first candidate "converge-guard status --json" 1
expect_no_task b1-first "Read the installed and candidate releases"
expect_no_task b1-first "Ask the candidate whether it can take part in the exclusion"
expect_no_task b1-first "Hold this host for the converge"
# THE ORDER of the binaries' invocations, as role and command: the first
# call (which reads the managed binary's version to record it), the second
# call (which reads the managed version and probes the candidate under the
# lock, before any re-binding), the settlement.
order=$(commands b1-first | tr '\n' ';')
[ "$order" = "managed prepare;managed version;managed prepare;managed version;candidate status;candidate version;managed settle;" ] \
  || fail "b1-first: the order of the calls is not validate, the second call with the candidate's probes under the lock, settle: $order" "$work/cases/b1-first/log"
expect_state b1-first record_release_executable "$(fact b1-first recovery)/billet.candidate"
expect_state b1-first record_preparing False
expect_state b1-first record_holder h1
expect_state b1-first pointer absent
[ "$(state b1-first record_id)" = "$(fact b1-first id)" ] || fail "b1-first: the record's id is not the fact's"
[ "$(backing_runs b1-first prepare)" -eq 2 ] || fail "b1-first: the backing binary ran prepare $(backing_runs b1-first prepare) times, want 2"
expect_calls b1-first systemctl "" 0
echo "ok   B1: a candidate is acquired over, staged, re-bound and settled in order, and the role asks nothing of its own"

# B2. No change: acquired, nothing staged, --no-change validated, settled.
plant b2-unchanged
p b2-unchanged 'plant_root; plant_managed v0.10.0'
ns_case b2-unchanged escalated
expect_allowed b2-unchanged
expect_fact b2-unchanged acquired True
expect_fact b2-unchanged upgrade False
expect_calls b2-unchanged managed "converge-guard prepare --holder h1 --json --no-change" 1
expect_calls b2-unchanged managed "converge-guard settle --holder h1 --token" 1
expect_calls b2-unchanged suffix "" 0
expect_no_task b2-unchanged "Allocate a recovery directory exclusively"
expect_state b2-unchanged record_release_executable /usr/bin/billet
expect_state b2-unchanged record_preparing False
echo "ok   B2: a converge with no binary change acquires, declares no change and settles"

# B3. A bootstrap: no billet, no root; the root established, the staging
# first, then the candidate acquires with the bootstrap's premise.
plant b3-bootstrap
a b3-bootstrap -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case b3-bootstrap escalated
expect_allowed b3-bootstrap
expect_fact b3-bootstrap route candidate
expect_fact b3-bootstrap acquired True
rec=$(fact b3-bootstrap recovery)
expect_calls b3-bootstrap candidate "converge-guard prepare --validate --holder h1 --candidate $rec/billet.candidate --json --expect-bootstrap" 1
expect_calls b3-bootstrap candidate "converge-guard prepare --holder h1 --json --candidate $rec/billet.candidate" 1
expect_calls b3-bootstrap candidate "converge-guard settle --holder h1 --token" 1
expect_calls b3-bootstrap managed "" 0
expect_state b3-bootstrap managed absent
expect_state b3-bootstrap root_mode 700:0
expect_state b3-bootstrap parent_mode 755:0
expect_state b3-bootstrap record_release_executable "$rec/billet.candidate"
expect_state b3-bootstrap record_preparing False
# B3b. A managed binary placed before the first call refuses the premise.
plant b3b-gained
a b3b-gained -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
e b3b-gained "BILLET_GATE_HOOK=prepare:1:cp $bins/wrap-managed-v0.10.0 /usr/bin/billet.new; chmod 0755 /usr/bin/billet.new; mv -f /usr/bin/billet.new /usr/bin/billet"
ns_case b3b-gained escalated
expect_refused b3b-gained "Refuse the preparation's answer" "bootstrap"
expect_fact b3b-gained acquired False
expect_state b3b-gained active absent
expect_final b3b-gained "no cleanup was attempted"
# B3c. A stray regular file under the root refuses; a legacy stamp directory is admitted.
plant b3c-stray
p b3c-stray "plant_root; touch $ROOT/stray-file"
a b3c-stray -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case b3c-stray escalated
expect_refused b3c-stray "Refuse the preparation's answer" "bootstrap" "stray-file"
expect_state b3c-stray active absent
plant b3c-legacy-dir
p b3c-legacy-dir "plant_root; mkdir -m 0700 $ROOT/$LEGACY_DIR"
a b3c-legacy-dir -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case b3c-legacy-dir escalated
expect_allowed b3c-legacy-dir
expect_fact b3c-legacy-dir acquired True
echo "ok   B3: a bootstrap establishes the root, stages first and acquires through the candidate under its premise"

# B4. A pre-R managed binary with an R candidate: the candidate acquires (no
# bootstrap premise); the second call judges the downgrade against the
# managed binary as it is THEN.
plant b4-pre-r-candidate
p b4-pre-r-candidate 'plant_root; plant_pre_r'
a b4-pre-r-candidate -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case b4-pre-r-candidate escalated
expect_allowed b4-pre-r-candidate
expect_fact b4-pre-r-candidate route candidate
rec=$(fact b4-pre-r-candidate recovery)
expect_calls b4-pre-r-candidate candidate "converge-guard prepare --validate --holder h1 --candidate $rec/billet.candidate --json" 1
expect_calls b4-pre-r-candidate candidate "--expect-bootstrap" 0
expect_calls b4-pre-r-candidate pre-r "converge-guard prepare" 1
expect_state b4-pre-r-candidate record_release_executable "$rec/billet.candidate"
plant b4b-newer-managed
p b4b-newer-managed 'plant_root; plant_pre_r'
a b4b-newer-managed -e "billet_binary_src=$bins/wrap-candidate-v0.10.0"
e b4b-newer-managed "BILLET_GATE_HOOK=prepare:3:cp $bins/wrap-managed-v0.10.1 /usr/bin/billet.new; chmod 0755 /usr/bin/billet.new; mv -f /usr/bin/billet.new /usr/bin/billet"
ns_case b4b-newer-managed escalated
expect_refused b4b-newer-managed "Refuse the preparation's answer" "downgrade"
expect_final b4b-newer-managed "the cleanup released the guard" "is now none"
expect_state b4b-newer-managed active absent
plant b4c-older-managed
p b4c-older-managed 'plant_root; plant_pre_r'
a b4c-older-managed -e "billet_binary_src=$bins/wrap-candidate-v0.10.0"
e b4c-older-managed "BILLET_GATE_HOOK=prepare:3:cp $bins/wrap-managed-v0.9.0 /usr/bin/billet.new; chmod 0755 /usr/bin/billet.new; mv -f /usr/bin/billet.new /usr/bin/billet"
ns_case b4c-older-managed escalated
expect_allowed b4c-older-managed
plant b4d-allowed-downgrade
p b4d-allowed-downgrade 'plant_root; plant_pre_r'
a b4d-allowed-downgrade -e "billet_binary_src=$bins/wrap-candidate-v0.10.0" -e billet_allow_downgrade=true
e b4d-allowed-downgrade "BILLET_GATE_HOOK=prepare:3:cp $bins/wrap-managed-v0.10.1 /usr/bin/billet.new; chmod 0755 /usr/bin/billet.new; mv -f /usr/bin/billet.new /usr/bin/billet"
ns_case b4d-allowed-downgrade escalated
expect_allowed b4d-allowed-downgrade
expect_calls b4d-allowed-downgrade candidate "--allow-downgrade" 1
echo "ok   B4: a pre-R host with a candidate acquires through it, and the downgrade is judged against the managed binary under the lock"

# B5. No billet, nothing to install: no call, the closing re-stats, allowed
# unheld; a binary or a guard that appears in between refuses.
plant b5-no-billet
ns_case b5-no-billet escalated
expect_allowed b5-no-billet
expect_fact b5-no-billet route unheld
expect_fact b5-no-billet shape none
expect_ran b5-no-billet "Inspect the managed binary and the claim again before an unheld converge"
expect_calls b5-no-billet managed "" 0
expect_calls b5-no-billet candidate "" 0
expect_state b5-no-billet active absent
expect_state b5-no-billet root dir
plant b5b-appeared
e b5b-appeared "BILLET_GATE_SYNC_HOOK=cp $bins/wrap-managed-v0.10.0 /usr/bin/billet.new; chmod 0755 /usr/bin/billet.new; mv -f /usr/bin/billet.new /usr/bin/billet"
ns_case b5b-appeared escalated
expect_refused b5b-appeared "Refuse a converge whose managed binary appeared" "is now present"
expect_state b5b-appeared active absent
plant b5c-dangling
p b5c-dangling 'plant_root; ln -s /nonexistent/billet /usr/bin/billet'
ns_case b5c-dangling escalated
expect_refused b5c-dangling "Refuse a preparation that did not answer" "did not answer"
expect_state b5c-dangling active absent
plant b5d-guard-appeared
e b5d-guard-appeared "BILLET_GATE_SYNC_HOOK=mkdir -m 0700 $ROOT/$REC_A && cp $bins/wrap-candidate-v0.10.1 $ROOT/$REC_A/billet.candidate && $mnt/bin/billet-v0.10.1 converge-guard prepare --validate --holder h2 --candidate $ROOT/$REC_A/billet.candidate --json >/dev/null"
ns_case b5d-guard-appeared escalated
# The flush precedes the claim's stat, so a guard published at the flush is
# found by the stat and answered through the executable it records: refused
# as another holder's, nothing acquired.
expect_refused b5d-guard-appeared "Refuse the preparation's answer" "held by h2"
expect_fact b5d-guard-appeared route fallback
expect_fact b5d-guard-appeared acquired False
expect_final b5d-guard-appeared "no cleanup was attempted"
expect_state b5d-guard-appeared record_holder h2
echo "ok   B5: a host with no billet converges unheld only while it still has none and no claim appeared"

# B6. A pre-R managed binary and nothing to stage: the legacy protocol, no
# call but the closing re-ask; a binary that moved or a guard that appeared
# refuses.
plant b6-pre-r-none
p b6-pre-r-none 'plant_root; plant_pre_r'
ns_case b6-pre-r-none escalated
expect_allowed b6-pre-r-none
expect_fact b6-pre-r-none shape legacy
expect_fact b6-pre-r-none route pre-r
grep -q "runs the legacy protocol" "$work/cases/b6-pre-r-none/out" || fail "b6: the legacy protocol was not warned about"
expect_calls b6-pre-r-none pre-r "converge-guard prepare --validate" 1
expect_calls b6-pre-r-none pre-r "converge-guard holder" 1
expect_state b6-pre-r-none active absent
plant b6b-moved
p b6b-moved 'plant_root; plant_pre_r'
e b6b-moved "BILLET_GATE_HOOK=prepare:1:cp $bins/wrap-managed-v0.10.0 /usr/bin/billet.new; chmod 0755 /usr/bin/billet.new; mv -f /usr/bin/billet.new /usr/bin/billet"
ns_case b6b-moved escalated
expect_refused b6b-moved "Refuse a converge whose managed binary moved" "an updater moved the managed binary"
expect_state b6b-moved active absent
plant b6c-equal-candidate
p b6c-equal-candidate 'plant_root; plant_pre_r'
a b6c-equal-candidate -e "billet_binary_src=$bins/wrap-managed-v0.10.0"
e b6c-equal-candidate "BILLET_GATE_HOOK=prepare:1:cp $bins/wrap-managed-v0.10.0 /usr/bin/billet.new; chmod 0755 /usr/bin/billet.new; mv -f /usr/bin/billet.new /usr/bin/billet"
ns_case b6c-equal-candidate escalated
expect_refused b6c-equal-candidate "Refuse a converge whose managed binary moved" "an updater moved the managed binary"
expect_fact b6c-equal-candidate upgrade False
expect_calls b6c-equal-candidate suffix "" 0
expect_state b6c-equal-candidate active absent
# B6e. The executable that answered the first call answers "unknown command"
# to the second: it moved under this converge; refused, never read as pre-R.
# The wrapper itself answers "unknown command" on the second call (a file
# replaced on disk is refused by the backing binary's digest check instead,
# which is the verification refusal and not this path).
plant b6e-second-call-moved
p b6e-second-call-moved 'plant_root; plant_managed v0.10.0'
e b6e-second-call-moved "BILLET_GATE_FAIL=prepare:2:unknown"
ns_case b6e-second-call-moved escalated
expect_refused b6e-second-call-moved "Refuse a preparation that did not answer" "moved under it"
expect_calls b6e-second-call-moved managed "converge-guard settle" 0
expect_final b6e-second-call-moved "the cleanup released the guard" "is now none"
expect_state b6e-second-call-moved active absent
plant b6d-guard-appeared
p b6d-guard-appeared 'plant_root; plant_pre_r'
e b6d-guard-appeared "BILLET_GATE_HOOK=prepare:1:$mnt/bin/billet-v0.10.0 converge-guard prepare --validate --holder h2 --json >/dev/null"
ns_case b6d-guard-appeared escalated
expect_refused b6d-guard-appeared "Refuse an unheld converge over a claim that appeared" "another converge holds this host"
expect_state b6d-guard-appeared record_holder h2
# B6f. The diagnostic followed by a hang is not the pre-R observation: the
# first call, the closing recheck and the dry run each refuse it as not having
# answered, and the legacy protocol is never selected on it.
plant b6f-diagnostic-then-hang
p b6f-diagnostic-then-hang 'plant_root; plant_pre_r'
a b6f-diagnostic-then-hang -e billet_guard_timeout=2
e b6f-diagnostic-then-hang "BILLET_GATE_PRE_R_HANG=prepare"
ns_case b6f-diagnostic-then-hang escalated
expect_refused b6f-diagnostic-then-hang "Refuse a preparation that did not answer" "ended by the bound"
expect_no_task b6f-diagnostic-then-hang "Record that a release before the guard runs the legacy protocol"
plant b6g-recheck-hang
p b6g-recheck-hang 'plant_root; plant_pre_r'
a b6g-recheck-hang -e billet_guard_timeout=2
e b6g-recheck-hang "BILLET_GATE_PRE_R_HANG=holder"
ns_case b6g-recheck-hang escalated
expect_refused b6g-recheck-hang "Refuse a converge whose managed binary moved" "did not answer within the bound"
expect_fact b6g-recheck-hang route pre-r
plant b7u-dry-run-hang
p b7u-dry-run-hang 'plant_root; plant_pre_r'
a b7u-dry-run-hang --check -e billet_guard_timeout=2
e b7u-dry-run-hang "BILLET_GATE_PRE_R_HANG=prepare"
HOLDER=""; ns_case b7u-dry-run-hang escalated; HOLDER=h1
expect_refused b7u-dry-run-hang "Judge the dry run's answer" "with no answer"
# B6h. The diagnostic, then a hang that ignores TERM: the kill after the grace
# period ends the run with a signal, which is not an exit the executable chose.
plant b6h-diagnostic-then-kill
p b6h-diagnostic-then-kill 'plant_root; plant_pre_r'
a b6h-diagnostic-then-kill -e billet_guard_timeout=1
e b6h-diagnostic-then-kill "BILLET_GATE_PRE_R_HANG=prepare"
e b6h-diagnostic-then-kill "BILLET_GATE_PRE_R_IGNORE_TERM=1"
ns_case b6h-diagnostic-then-kill escalated
expect_refused b6h-diagnostic-then-kill "Refuse a preparation that did not answer" "ended by the bound"
expect_no_task b6h-diagnostic-then-kill "Record that a release before the guard runs the legacy protocol"
plant b6i-recheck-kill
p b6i-recheck-kill 'plant_root; plant_pre_r'
a b6i-recheck-kill -e billet_guard_timeout=1
e b6i-recheck-kill "BILLET_GATE_PRE_R_HANG=holder"
e b6i-recheck-kill "BILLET_GATE_PRE_R_IGNORE_TERM=1"
ns_case b6i-recheck-kill escalated
expect_refused b6i-recheck-kill "Refuse a converge whose managed binary moved" "did not answer within the bound"
plant b7v-dry-run-kill
p b7v-dry-run-kill 'plant_root; plant_pre_r'
a b7v-dry-run-kill --check -e billet_guard_timeout=1
e b7v-dry-run-kill "BILLET_GATE_PRE_R_HANG=prepare"
e b7v-dry-run-kill "BILLET_GATE_PRE_R_IGNORE_TERM=1"
HOLDER=""; ns_case b7v-dry-run-kill escalated; HOLDER=h1
expect_refused b7v-dry-run-kill "Judge the dry run's answer" "with no answer"
echo "ok   B6: a pre-R host runs the legacy protocol only while its binary still predates the guard and no claim appeared, and never on a diagnostic the bound ended or a kill reached"

# B7. Check mode: `prepare --dry-run` before any shape dispatch, no holder,
# every shape reported, nothing held or made, then the read-only staging.
check_case() { # name plant-line fragment...
  local name=$1 plant_line=$2; shift 2
  plant "$name"
  p "$name" "$plant_line"
  a "$name" --check
  HOLDER=""; ns_case "$name" escalated; HOLDER=h1
  expect_allowed "$name"
  for frag in "$@"; do
    grep -qF -- "$frag" "$work/cases/$name/out" || fail "$name: the dry run did not report: $frag" "$work/cases/$name/out"
  done
  expect_calls "$name" managed "converge-guard prepare --dry-run --json" 1
  expect_calls "$name" managed "converge-guard prepare --validate" 0
  expect_calls "$name" managed "converge-guard settle" 0
  expect_no_task "$name" "Classify a legacy pointer"
  expect_no_task "$name" "Refuse a converge whose exclusion moved"
}
check_case b7-none 'plant_root; plant_managed v0.10.0' "is none"
expect_state b7-none active absent
check_case b7-foreign "plant_root; plant_managed v0.10.0; plant_guard h2 /usr/bin/billet" "a guard held by h2 since 2026-09-09T12:00:00Z, settled"
expect_state b7-foreign record_holder h2
check_case b7-foreign-pointer "plant_root; plant_managed v0.10.0; plant_guard h2 /usr/bin/billet; mkdir -m 0700 $ROOT/$REC_A; plant_pointer $REC_A" "a guard held by h2" "with a transaction pointer inside it"
check_case b7-own "plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet preparing" "a guard held by h1" "preparing"
expect_state b7-own record_preparing True
check_case b7-legacy "plant_root; plant_managed v0.10.0; printf '%s\n' $ROOT/$LEGACY_DIR >$ROOT/active" "is legacy-role"
expect_state b7-legacy active file
check_case b7-go "plant_root; plant_managed v0.10.0; ln -s $ROOT/$LEGACY_DIR $ROOT/active" "is host-upgrade" "a converge would refuse this shape"
expect_state b7-go active symlink
expect_fact b7-go shape host-upgrade
check_case b7-unpublished "plant_root; plant_managed v0.10.0; mkdir -m 0700 $ROOT/active" "is unpublished-guard" "a converge would refuse this shape"
expect_state b7-unpublished active dir
expect_fact b7-unpublished shape unpublished-guard
# A guard whose record cannot be read: reported with the problem and kept as a guard; the refusal is the converge's.
check_case b7h-record-error "plant_root; plant_managed v0.10.0; plant_guard h2 /usr/bin/billet; printf 'nope\n' >$ROOT/active/guard.json" "a guard whose record cannot be read" "not JSON"
expect_fact b7h-record-error shape guard
expect_fact b7h-record-error interrupted False
# A guard whose pointer cannot be followed: reported as a guard WITH a pointer and the problem.
check_case b7g-dangling-pointer "plant_root; plant_managed v0.10.0; plant_guard h2 /usr/bin/billet; ln -sT $ROOT/$REC_A $ROOT/active/recovery" "a guard held by h2" "with a transaction pointer inside it" "whose pointer cannot be followed" "does not exist"
expect_fact b7g-dangling-pointer shape guard-pointer
# B7b. A pre-R managed binary: the capability refusal read as pre-R.
plant b7b-pre-r
p b7b-pre-r 'plant_root; plant_pre_r'
a b7b-pre-r --check
HOLDER=""; ns_case b7b-pre-r escalated; HOLDER=h1
expect_allowed b7b-pre-r
grep -qF "predates the converge guard (it answers" "$work/cases/b7b-pre-r/out" || fail "b7b: the dry run did not report the pre-R binary" "$work/cases/b7b-pre-r/out"
expect_calls b7b-pre-r pre-r "converge-guard prepare --dry-run --json" 1
# No billet: reported from the role's stat alone.
plant b7c-absent
a b7c-absent --check
HOLDER=""; ns_case b7c-absent escalated; HOLDER=h1
expect_allowed b7c-absent
grep -qF "read from this role's stat alone" "$work/cases/b7c-absent/out" || fail "b7c: the dry run over no billet did not report from the stat" "$work/cases/b7c-absent/out"
expect_state b7c-absent root absent
# A corrupted dry-run answer refuses at the one parser.
plant b7d-corrupt
p b7d-corrupt 'plant_root; plant_managed v0.10.0'
a b7d-corrupt --check
printf '{"outcome": 7}\n' >"$work/cases/b7d-corrupt/dry.json"
e b7d-corrupt "BILLET_GATE_ANSWER=prepare:1:$work/cases/b7d-corrupt/dry.json"
HOLDER=""; ns_case b7d-corrupt escalated; HOLDER=h1
expect_refused b7d-corrupt "Judge the dry run's answer" "outcome"
# The read-only staging path after the report: a pin resolved and reported, a moving pin refused, nothing fetched or made.
plant b7e-pinned
p b7e-pinned 'plant_root; plant_managed v0.10.0'
a b7e-pinned --check -e billet_gate_facts=true -e billet_version=v0.10.1 -e "billet_release_url_base=$origin_url" -e "billet_release_stage=$work/cases/b7e-pinned/stage" -e billet_fetch_retries=1
HOLDER=""; ns_case b7e-pinned escalated; HOLDER=h1
expect_allowed b7e-pinned
expect_ran b7e-pinned "Report that a dry run does not fetch"
expect_calls b7e-pinned suffix "" 0
[ ! -e "$work/cases/b7e-pinned/stage" ] || fail "b7e: a dry run fetched"
expect_state b7e-pinned active absent
plant b7f-moving
p b7f-moving 'plant_root; plant_managed v0.10.0'
a b7f-moving --check -e billet_gate_facts=true -e billet_version=latest
HOLDER=""; ns_case b7f-moving escalated; HOLDER=h1
expect_refused b7f-moving "Refuse an unpinned billet version" "must name one release"
expect_state b7f-moving active absent
echo "ok   B7: a dry run reports every shape through prepare --dry-run before any dispatch, needs no holder, then takes the read-only staging path"

# B7n. A dry run over a guard directory beside a managed binary that cannot
# answer for it (absent, or pre-R): the fallback reads the record, the
# recorded candidate reports, a transaction pointer is the interrupted
# transaction it is, and what the converge refuses the dry run refuses.
dry_via_record() { # name plant-line managed-role fragment...
  local name=$1 plant_line=$2 role=$3; shift 3
  plant "$name"
  p "$name" "$plant_line"
  a "$name" --check
  HOLDER=""; ns_case "$name" escalated; HOLDER=h1
  expect_allowed "$name"
  for frag in "$@"; do
    grep -qF -- "$frag" "$work/cases/$name/out" || fail "$name: the dry run did not report: $frag" "$work/cases/$name/out"
  done
  expect_ran "$name" "Read the guard's record"
  expect_calls "$name" candidate "converge-guard prepare --dry-run --json" 1
  expect_calls "$name" candidate "converge-guard prepare --validate" 0
  expect_calls "$name" candidate "converge-guard settle" 0
  [ "$role" = none ] || expect_calls "$name" "$role" "converge-guard prepare --dry-run --json" 1
}
dry_via_record b7i-absent-pointer "plant_root; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate; plant_pointer $REC_A" none "answered by the executable the guard records" "/usr/bin/billet is absent" "with a transaction pointer inside it"
expect_fact b7i-absent-pointer shape guard-pointer
expect_fact b7i-absent-pointer interrupted True
expect_fact b7i-absent-pointer recovery "$ROOT/$REC_A"
dry_via_record b7j-pre-r-pointer "plant_root; plant_pre_r; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate; plant_pointer $REC_A" pre-r "answered by the executable the guard records" "/usr/bin/billet predates the converge guard" "with a transaction pointer inside it"
expect_fact b7j-pre-r-pointer shape guard-pointer
expect_fact b7j-pre-r-pointer interrupted True
dry_via_record b7k-absent-guard "plant_root; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate" none "a guard held by h1 since 2026-09-09T12:00:00Z, settled"
expect_fact b7k-absent-guard shape guard
expect_fact b7k-absent-guard interrupted False
# An unpublished guard, and a record the fallback cannot trust: refused as the converge refuses them, and the candidate is never run.
plant b7l-absent-unpublished
p b7l-absent-unpublished "plant_root; mkdir -m 0700 $ROOT/active"
a b7l-absent-unpublished --check
HOLDER=""; ns_case b7l-absent-unpublished escalated; HOLDER=h1
expect_refused b7l-absent-unpublished "Refuse an unpublished guard no executable can recover" "never returned from publishing"
plant b7m-absent-corrupt
p b7m-absent-corrupt "plant_root; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate preparing"
p b7m-absent-corrupt "\"\$PYTHON\" - <<'PY'
import json
p = '/var/lib/billet/upgrades/active/guard.json'
r = json.load(open(p))
r['preparing'] = 'yes'
json.dump(r, open(p, 'w'))
PY"
a b7m-absent-corrupt --check
HOLDER=""; ns_case b7m-absent-corrupt escalated; HOLDER=h1
expect_refused b7m-absent-corrupt "Judge the guard's record" "preparing"
expect_calls b7m-absent-corrupt candidate "" 0
# The parser requires the guard object under converge-guard and reads its members by type.
for variant in no-guard null-error numeric-holder null-error-healthy numeric-pointer-problem; do
  plant "b7o-$variant"
  p "b7o-$variant" 'plant_root; plant_managed v0.10.0'
  a "b7o-$variant" --check
  case $variant in
    no-guard) printf '{"outcome": "reported", "shape": "converge-guard", "guard": null, "managed": {"path": "/usr/bin/billet", "present": true}}\n' ;;
    null-error) printf '{"outcome": "reported", "shape": "converge-guard", "guard": {"record_error": null, "pointer": true}, "managed": {"path": "/usr/bin/billet", "present": true}}\n' ;;
    numeric-holder) printf '{"outcome": "reported", "shape": "converge-guard", "guard": {"holder": 7, "claimed_at": "2026-09-09T12:00:00Z", "preparing": false, "pointer": true}, "managed": {"path": "/usr/bin/billet", "present": true}}\n' ;;
    null-error-healthy) printf '{"outcome": "reported", "shape": "converge-guard", "guard": {"holder": "h2", "claimed_at": "2026-09-09T12:00:00Z", "preparing": false, "record_error": null, "pointer": false}, "managed": {"path": "/usr/bin/billet", "present": true}}\n' ;;
    numeric-pointer-problem) printf '{"outcome": "reported", "shape": "converge-guard", "guard": {"holder": "h2", "claimed_at": "2026-09-09T12:00:00Z", "preparing": false, "pointer": true, "pointer_problem": 7}, "managed": {"path": "/usr/bin/billet", "present": true}}\n' ;;
  esac >"$work/cases/b7o-$variant/dry.json"
  e "b7o-$variant" "BILLET_GATE_ANSWER=prepare:1:$work/cases/b7o-$variant/dry.json"
  HOLDER=""; ns_case "b7o-$variant" escalated; HOLDER=h1
  expect_refused "b7o-$variant" "Judge the dry run's answer" "guard"
done
# A repeated member in a dry run's report is refused by the strict read, before any judgement.
plant b7o-repeated
p b7o-repeated 'plant_root; plant_managed v0.10.0'
a b7o-repeated --check
printf '{"outcome": "reported", "shape": "none", "guard": null, "managed": {"path": "/usr/bin/billet", "present": true}, "shape": "none"}\n' >"$work/cases/b7o-repeated/dry.json"
e b7o-repeated "BILLET_GATE_ANSWER=prepare:1:$work/cases/b7o-repeated/dry.json"
HOLDER=""; ns_case b7o-repeated escalated; HOLDER=h1
expect_refused b7o-repeated "Read the dry run's report" "repeated"
# A claim that appeared between the stat and the report is the report's: a legacy file, a Go transaction.
plant b7q-legacy-appeared
p b7q-legacy-appeared 'plant_root; plant_managed v0.10.0'
a b7q-legacy-appeared --check
e b7q-legacy-appeared "BILLET_GATE_HOOK=prepare:1:printf '%s\n' $ROOT/$LEGACY_DIR >$ROOT/active"
HOLDER=""; ns_case b7q-legacy-appeared escalated; HOLDER=h1
expect_allowed b7q-legacy-appeared
grep -qF "is legacy-role" "$work/cases/b7q-legacy-appeared/out" || fail "b7q: the dry run did not report the legacy claim" "$work/cases/b7q-legacy-appeared/out"
expect_fact b7q-legacy-appeared shape legacy-file
expect_fact b7q-legacy-appeared interrupted True
plant b7r-go-appeared
p b7r-go-appeared 'plant_root; plant_managed v0.10.0'
a b7r-go-appeared --check
e b7r-go-appeared "BILLET_GATE_HOOK=prepare:1:ln -s $ROOT/$LEGACY_DIR $ROOT/active"
HOLDER=""; ns_case b7r-go-appeared escalated; HOLDER=h1
expect_allowed b7r-go-appeared
expect_fact b7r-go-appeared shape host-upgrade
expect_fact b7r-go-appeared interrupted False
# A claim of a type the role does not know, beside no billet: the stat alone classifies it, and never as none.
plant b7s-fifo-absent
p b7s-fifo-absent 'plant_root; mkfifo -m 0600 $ROOT/active'
a b7s-fifo-absent --check
HOLDER=""; ns_case b7s-fifo-absent escalated; HOLDER=h1
expect_allowed b7s-fifo-absent
grep -qF "neither a directory nor anything this role writes; a converge would refuse it" "$work/cases/b7s-fifo-absent/out" || fail "b7s: the dry run did not report the FIFO" "$work/cases/b7s-fifo-absent/out"
expect_fact b7s-fifo-absent shape unknown
plant b7t-symlink-absent
p b7t-symlink-absent "plant_root; ln -s $ROOT/$LEGACY_DIR $ROOT/active"
a b7t-symlink-absent --check
HOLDER=""; ns_case b7t-symlink-absent escalated; HOLDER=h1
expect_allowed b7t-symlink-absent
expect_fact b7t-symlink-absent shape host-upgrade
echo "ok   B7n: a dry run over a guard the managed binary cannot answer for is reported by the recorded executable, keeps an interrupted transaction, refuses what the converge refuses, requires the guard object by type, and keeps every shape it observed"

# B8. The fallback: a pre-R or absent managed binary and a guard recording an
# R candidate; the calls run through the candidate.
plant b8-fallback
p b8-fallback "plant_root; plant_pre_r; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate"
a b8-fallback -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case b8-fallback escalated
expect_allowed b8-fallback
expect_fact b8-fallback route fallback
expect_fact b8-fallback acquired False
expect_fact b8-fallback executable "$ROOT/$REC_A/billet.candidate"
expect_calls b8-fallback candidate "converge-guard prepare --validate --holder h1 --json" 1
expect_calls b8-fallback candidate "converge-guard prepare --holder h1 --json --candidate" 1
expect_calls b8-fallback candidate "converge-guard settle" 0
expect_ran b8-fallback "Read the guard's record"
expect_state b8-fallback record_release_executable "$ROOT/$REC_A/billet.candidate"
# B8b. A five-member record is adopted; the role keeps the id and sends --expect-id afterwards.
plant b8b-adopt
p b8b-adopt "plant_root; plant_pre_r; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate legacy"
a b8b-adopt -e "billet_binary_src=$bins/wrap-candidate-v0.10.1" -e billet_gate_inclusions=2
ns_case b8b-adopt escalated
expect_allowed b8b-adopt
expect_fact b8b-adopt adopted False
grep -q '"adopted": true' "$work/cases/b8b-adopt/private" || fail "b8b: the first call did not adopt the record" "$work/cases/b8b-adopt/private"
expect_state b8b-adopt record_preparing False
expect_state b8b-adopt record_has_token False
adopted_id=$(state b8b-adopt record_id)
[ -n "$adopted_id" ] || fail "b8b: the adopted record carries no id"
expect_calls b8b-adopt candidate "--expect-id $adopted_id" 3
expect_calls b8b-adopt candidate "converge-guard prepare --validate --holder h1 --json --expect-id $adopted_id" 1
# B8c. A record with a malformed protocol member, or a protocol member without an id: the fallback refuses naming it, and prints no token.
for member in preparing token id no-id; do
  plant "b8c-$member"
  p "b8c-$member" "plant_root; plant_pre_r; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate preparing"
  p "b8c-$member" "\"\$PYTHON\" - <<'PY'
import json
p = '/var/lib/billet/upgrades/active/guard.json'
r = json.load(open(p))
if '$member' == 'no-id':
    del r['id']
else:
    r['$member'] = {'preparing': 'yes', 'token': 'FEDCBA9876543210FEDCBA9876543210', 'id': 'short'}['$member']
json.dump(r, open(p, 'w'))
PY"
  ns_case "b8c-$member" escalated
  expect_refused "b8c-$member" "Judge the guard's record" "$([ "$member" = no-id ] && echo 'without an id' || echo "$member")"
  ! grep -q "fedcba9876543210" "$work/cases/b8c-$member/out" || fail "b8c-$member: the record's token reached the output"
  expect_calls "b8c-$member" candidate "" 0
done
# B8d. The managed path absent: with a pointer and without, through the candidate.
plant b8d-absent-pointer
p b8d-absent-pointer "plant_root; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate; plant_pointer $REC_A"
ns_case b8d-absent-pointer escalated
expect_allowed b8d-absent-pointer
expect_fact b8d-absent-pointer route fallback
expect_fact b8d-absent-pointer interrupted True
expect_fact b8d-absent-pointer shape guard-pointer
expect_fact b8d-absent-pointer recovery "$ROOT/$REC_A"
expect_calls b8d-absent-pointer candidate "converge-guard prepare --holder h1 --json --recovery" 1
plant b8d-absent-none
p b8d-absent-none "plant_root; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate"
a b8d-absent-none -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case b8d-absent-none escalated
expect_allowed b8d-absent-none
expect_fact b8d-absent-none route fallback
expect_calls b8d-absent-none candidate "converge-guard prepare --holder h1 --json --candidate" 1
echo "ok   B8: the verified fallback answers through the recorded candidate, adopts a five-member record, refuses a malformed one, and covers an absent managed path"

# B9. This holder's settled guard with a valid pointer: validated, then
# --recovery, no staging, no re-binding, no settle.
plant b9-pointer
p b9-pointer "plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet; mkdir -m 0700 $ROOT/$REC_A; plant_pointer $REC_A"
a b9-pointer -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case b9-pointer escalated
expect_allowed b9-pointer
expect_fact b9-pointer shape guard-pointer
expect_fact b9-pointer interrupted True
expect_fact b9-pointer recovery "$ROOT/$REC_A"
expect_fact b9-pointer acquired False
expect_calls b9-pointer managed "converge-guard prepare --validate --holder h1 --json" 1
expect_calls b9-pointer managed "converge-guard prepare --holder h1 --json --recovery" 1
expect_calls b9-pointer managed "--candidate" 0
expect_calls b9-pointer managed "converge-guard settle" 0
expect_calls b9-pointer suffix "" 0
expect_no_task b9-pointer "Stage the immutable candidate binary inside its recovery journal"
expect_state b9-pointer record_release_executable /usr/bin/billet
expect_state b9-pointer pointer symlink
plant b9b-pointer-late
p b9b-pointer-late "plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet; mkdir -m 0700 $ROOT/$REC_A"
e b9b-pointer-late "BILLET_GATE_HOOK=prepare:1:ln -sT $ROOT/$REC_A $ROOT/active/recovery"
ns_case b9b-pointer-late escalated
expect_allowed b9b-pointer-late
expect_fact b9b-pointer-late interrupted True
expect_fact b9b-pointer-late recovery "$ROOT/$REC_A"
# B9e. An answer naming a recovery_dir beside its pointer_target that is not it: refused at the parser, no fact taken.
plant b9e-recovery-dir-other
p b9e-recovery-dir-other "plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet; mkdir -m 0700 $ROOT/$REC_A; plant_pointer $REC_A"
"$python" - "$work/corpus/validated-settled.json" "$work/cases/b9e-recovery-dir-other/answer.json" "$ROOT/$REC_A" "$ROOT/$REC_B" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["pointer"] = True
d["pointer_target"] = sys.argv[3]
d["recovery_dir"] = sys.argv[4]
json.dump(d, open(sys.argv[2], "w"), indent=2)
PY
e b9e-recovery-dir-other "BILLET_GATE_ANSWER=prepare:1:$work/cases/b9e-recovery-dir-other/answer.json"
ns_case b9e-recovery-dir-other escalated
expect_refused b9e-recovery-dir-other "Judge the preparation's answer" "recovery_dir"
expect_fact b9e-recovery-dir-other recovery ""
expect_calls b9e-recovery-dir-other managed "converge-guard prepare --holder h1 --json --recovery" 0
# B9f. A pointer_target with a trailing newline: refused at the parser, since the grammar ends at the end of the text.
plant b9f-pointer-target-newline
p b9f-pointer-target-newline "plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet; mkdir -m 0700 $ROOT/$REC_A; plant_pointer $REC_A"
"$python" - "$work/corpus/validated-settled.json" "$work/cases/b9f-pointer-target-newline/answer.json" "$ROOT/$REC_A" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d["pointer"] = True
d["pointer_target"] = sys.argv[3] + "\n"
d["recovery_dir"] = sys.argv[3] + "\n"
json.dump(d, open(sys.argv[2], "w"), indent=2)
PY
e b9f-pointer-target-newline "BILLET_GATE_ANSWER=prepare:1:$work/cases/b9f-pointer-target-newline/answer.json"
ns_case b9f-pointer-target-newline escalated
expect_refused b9f-pointer-target-newline "Judge the preparation's answer" "pointer_target"
expect_fact b9f-pointer-target-newline recovery ""
plant b9c-malformed
p b9c-malformed "plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet; touch $ROOT/active/recovery"
ns_case b9c-malformed escalated
expect_refused b9c-malformed "Refuse the preparation's answer" "pointer"
expect_state b9c-malformed active dir
# B9d. --recovery with the managed bytes absent, older, and equal to the candidate.
plant b9d-older
p b9d-older "plant_root; plant_managed v0.9.0; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate; plant_pointer $REC_A"
ns_case b9d-older escalated
expect_allowed b9d-older
expect_fact b9d-older route managed
expect_fact b9d-older interrupted True
plant b9d-equal
p b9d-equal "plant_root; plant_recovery $REC_A v0.10.1; plant_managed_file $ROOT/$REC_A/billet.candidate; plant_guard h1 $ROOT/$REC_A/billet.candidate; plant_pointer $REC_A"
ns_case b9d-equal escalated
expect_allowed b9d-equal
expect_fact b9d-equal interrupted True
echo "ok   B9: an interrupted transaction is validated by its pointer, declared as a recovery, and never staged over or re-bound"

# B10. A guard by h1 published between the stat and the first call: validated,
# not acquired, and a later refusal releases nothing; gone or replaced under
# --expect-id refuses.
plant b10-none-then-held
p b10-none-then-held 'plant_root; plant_managed v0.10.0'
a b10-none-then-held -e "billet_binary_src=$bins/wrap-candidate-v0.9.0"
e b10-none-then-held "BILLET_GATE_HOOK=prepare:1:$mnt/bin/billet-v0.10.0 converge-guard prepare --validate --holder h1 --json >/dev/null"
ns_case b10-none-then-held escalated
expect_refused b10-none-then-held "Refuse the preparation's answer" "downgrade"
expect_fact b10-none-then-held acquired False
expect_final b10-none-then-held "no cleanup was attempted" "release --holder h1"
expect_state b10-none-then-held record_holder h1
expect_state b10-none-then-held record_preparing True
plant b10b-gone
p b10b-gone 'plant_root; plant_managed v0.10.0'
a b10b-gone -e billet_gate_inclusions=2
e b10b-gone "BILLET_GATE_HOOK=prepare:3:rm -rf $ROOT/active"
ns_case b10b-gone escalated
expect_refused b10b-gone "Refuse the preparation's answer" "gone"
expect_state b10b-gone active absent
expect_final b10b-gone "no cleanup was attempted"
plant b10c-replaced
p b10c-replaced 'plant_root; plant_managed v0.10.0'
a b10c-replaced -e billet_gate_inclusions=2
e b10c-replaced "BILLET_GATE_HOOK=prepare:3:rm -rf $ROOT/active; $mnt/bin/billet-v0.10.0 converge-guard prepare --validate --holder h1 --json >/dev/null"
ns_case b10c-replaced escalated
expect_refused b10c-replaced "Refuse the preparation's answer" "replaced"
expect_final b10c-replaced "no cleanup was attempted"
expect_state b10c-replaced active dir
[ "$(state b10c-replaced record_id)" != "$(fact b10c-replaced id)" ] || fail "b10c: the replaced guard kept the id"
echo "ok   B10: a guard that appears under the same holder is validated and never released; a guard gone or replaced under this run refuses"

# B11. The record re-bound to X between the stat and the second call with Y: refused, kept.
plant b11-record-moved
p b11-record-moved "plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet; plant_recovery $REC_A v0.10.0"
a b11-record-moved -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
e b11-record-moved "BILLET_GATE_HOOK=prepare:2:$mnt/bin/billet-v0.10.0 converge-guard prepare --holder h1 --candidate $ROOT/$REC_A/billet.candidate --json >/dev/null"
ns_case b11-record-moved escalated
expect_refused b11-record-moved "Refuse the preparation's answer" "candidate"
expect_state b11-record-moved record_release_executable "$ROOT/$REC_A/billet.candidate"
expect_final b11-record-moved "no cleanup was attempted"
echo "ok   B11: a record re-bound under the second call refuses and is kept"

# B12. The answer corrupted, one member at a time: the parser names the
# member; a corrupted first answer leaves the guard (no token known); a
# corrupted second answer after `acquired` is cleaned up.
for member in outcome id holder preparing token record pointer managed downgrade next shape; do
  plant "b12-$member"
  p "b12-$member" 'plant_root; plant_managed v0.10.0'
  corrupt "$work/corpus/acquired.json" "$work/cases/b12-$member/answer.json" "$member"
  e "b12-$member" "BILLET_GATE_ANSWER=prepare:1:$work/cases/b12-$member/answer.json"
  ns_case "b12-$member" escalated
  expect_refused "b12-$member" "Judge the preparation's answer" "$member"
  expect_fact "b12-$member" token_known False
  expect_final "b12-$member" "no cleanup was attempted" "release --holder h1"
  expect_state "b12-$member" record_preparing True
  expect_calls "b12-$member" managed "converge-guard release" 0
done
# A success answer under a non-zero exit is refused at the member `exit`,
# before any fact is taken: no token known, nothing released.
# A repeated member, the valid value last: the strict read refuses before any fact is taken.
plant b12-repeated
p b12-repeated 'plant_root; plant_managed v0.10.0'
"$python" - "$work/corpus/acquired.json" "$work/cases/b12-repeated/answer.json" <<'PY'
import sys
text = open(sys.argv[1]).read().rstrip().rstrip("}")
open(sys.argv[2], "w").write(text + ', "pointer": true, "pointer": false}\n')
PY
e b12-repeated "BILLET_GATE_ANSWER=prepare:1:$work/cases/b12-repeated/answer.json"
ns_case b12-repeated escalated
expect_refused b12-repeated "Read the preparation's answer" "repeated"
expect_fact b12-repeated token_known False
expect_final b12-repeated "no cleanup was attempted" "release --holder h1"
expect_state b12-repeated record_preparing True
expect_calls b12-repeated managed "converge-guard release" 0
# A numeric token or id of 32 digits, or one with a trailing newline: refused as not the member's shape before any fact is taken, so the rescue reads no number and the settlement is never handed a token the command refuses.
for spec in token:number id:number token:newline id:newline; do
  member=${spec%%:*}; kind=${spec#*:}
  plant "b12n-$member-$kind"
  p "b12n-$member-$kind" 'plant_root; plant_managed v0.10.0'
  "$python" - "$work/corpus/acquired.json" "$work/cases/b12n-$member-$kind/answer.json" "$member" "$kind" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
d[sys.argv[3]] = 12345678901234567890123456789012 if sys.argv[4] == "number" else "0123456789abcdef0123456789abcdef\n"
json.dump(d, open(sys.argv[2], "w"), indent=2)
PY
  member="$member-$kind"
  e "b12n-$member" "BILLET_GATE_ANSWER=prepare:1:$work/cases/b12n-$member/answer.json"
  ns_case "b12n-$member" escalated
  expect_refused "b12n-$member" "Judge the preparation's answer" "${member%%-*}"
  expect_fact "b12n-$member" token_known False
  expect_final "b12n-$member" "no cleanup was attempted" "release --holder h1"
  expect_state "b12n-$member" record_preparing True
  expect_calls "b12n-$member" managed "converge-guard release" 0
done
plant b12-exit
p b12-exit 'plant_root; plant_managed v0.10.0'
e b12-exit "BILLET_GATE_FAIL=prepare:1:exit:1"
ns_case b12-exit escalated
expect_refused b12-exit "Judge the preparation's answer" "exit"
expect_fact b12-exit token_known False
expect_fact b12-exit held False
expect_final b12-exit "no cleanup was attempted" "release --holder h1"
expect_state b12-exit record_preparing True
expect_calls b12-exit managed "converge-guard release" 0
# A null `next` on a successful second answer: refused at the parser naming it, and the guard cleaned up.
plant b12-second-next
p b12-second-next 'plant_root; plant_managed v0.10.0'
corrupt "$work/corpus/no-change.json" "$work/cases/b12-second-next/answer.json" next
e b12-second-next "BILLET_GATE_ANSWER=prepare:2:$work/cases/b12-second-next/answer.json"
ns_case b12-second-next escalated
expect_refused b12-second-next "Judge the preparation's answer" "next"
expect_final b12-second-next "the cleanup released the guard" "is now none"
expect_fact b12-second-next released True
expect_state b12-second-next active absent
plant b12-second
p b12-second 'plant_root; plant_managed v0.10.0'
e b12-second "BILLET_GATE_ANSWER=prepare:2:$work/corpus/no-change-outcome.json"
a b12-second -vvv
ns_case b12-second escalated
expect_refused b12-second "Judge the preparation's answer" "outcome"
expect_final b12-second "the cleanup released the guard" "is now none"
expect_fact b12-second released True
expect_state b12-second active absent
expect_calls b12-second managed "converge-guard release --holder h1 --cleanup --token" 1
[ -n "$(tokens_of b12-second)" ] || fail "b12-second: no token was captured to grep for"
echo "ok   B12: a corrupted answer refuses at the one parser naming the member, cleans up only inside the acquirer's window, and no token reaches the output (one case under -vvv)"

# B13. The rerun shapes.
plant b13-inclusions
p b13-inclusions 'plant_root; plant_managed v0.10.0'
a b13-inclusions -e billet_gate_inclusions=2
ns_case b13-inclusions escalated
expect_allowed b13-inclusions
expect_fact b13-inclusions acquired False
expect_fact b13-inclusions held True
run_id=$(fact b13-inclusions id)
expect_calls b13-inclusions managed "converge-guard prepare --validate --holder h1 --json --expect-id $run_id" 1
expect_calls b13-inclusions managed "converge-guard prepare --validate --holder h1 --json" 2
expect_calls b13-inclusions managed "converge-guard settle" 1
expect_state b13-inclusions record_id "$run_id"
plant b13-plays
p b13-plays 'plant_root; plant_managed v0.10.0'
ns_case b13-plays escalated play2
expect_allowed b13-plays
run_id=$(fact b13-plays id)
expect_calls b13-plays managed "converge-guard prepare --validate --holder h1 --json --expect-id $run_id" 1
expect_calls b13-plays managed "converge-guard settle" 1
expect_fact b13-plays held True
plant b13-fresh-settled
p b13-fresh-settled 'plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet'
ns_case b13-fresh-settled escalated
expect_allowed b13-fresh-settled
expect_fact b13-fresh-settled acquired False
# A fresh process knows no id before its first call; the second call, inside
# the same inclusion, carries the id the first answered.
expect_calls b13-fresh-settled managed "converge-guard prepare --validate --holder h1 --json --expect-id" 0
expect_calls b13-fresh-settled managed "converge-guard prepare --validate --holder h1 --json" 1
expect_calls b13-fresh-settled managed "converge-guard settle" 0
expect_state b13-fresh-settled record_preparing False
plant b13-fresh-preparing
p b13-fresh-preparing 'plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet preparing'
ns_case b13-fresh-preparing escalated
expect_allowed b13-fresh-preparing
expect_fact b13-fresh-preparing acquired False
expect_fact b13-fresh-preparing token_known False
expect_calls b13-fresh-preparing managed "converge-guard settle" 0
expect_state b13-fresh-preparing record_preparing True
echo "ok   B13: a second inclusion and a second play validate under --expect-id and settle once; a fresh process validates a settled or a preparing guard and settles nothing"

# B14. A regular file at `active`: the pre-R role's claim, classified by the
# role's own stat whatever the managed binary is; no call, nothing acquired.
for variant in capable pre-r absent; do
  plant "b14-$variant"
  case $variant in
    capable) p "b14-$variant" 'plant_root; plant_managed v0.10.0' ;;
    pre-r) p "b14-$variant" 'plant_root; plant_pre_r' ;;
    absent) p "b14-$variant" 'plant_root' ;;
  esac
  p "b14-$variant" "printf '%s\n' $ROOT/$LEGACY_DIR >$ROOT/active"
  ns_case "b14-$variant" escalated
  expect_allowed "b14-$variant"
  expect_fact "b14-$variant" shape legacy-file
  expect_fact "b14-$variant" interrupted True
  expect_ran "b14-$variant" "Classify a legacy pointer"
  expect_calls "b14-$variant" managed "converge-guard" 0
  expect_calls "b14-$variant" pre-r "converge-guard" 0
  expect_state "b14-$variant" active file
done
plant b14b-moved
p b14b-moved 'plant_root; plant_managed v0.10.0'
# A value with spaces goes to -e as JSON: `-e k=v` splits on whitespace.
a b14b-moved -e billet_gate_inclusions=2 -e "{\"billet_gate_between\": \"rm -rf $ROOT/active; echo $ROOT/$LEGACY_DIR >$ROOT/active\"}"
ns_case b14b-moved escalated
expect_refused b14b-moved "Refuse a converge whose exclusion moved" "a regular file (a legacy converge transaction)"
expect_no_task b14b-moved "Classify a legacy pointer"
expect_calls b14b-moved managed "converge-guard release" 0
expect_state b14b-moved active file
plant b14c-moved-play
p b14c-moved-play 'plant_root; plant_managed v0.10.0'
a b14c-moved-play -e "{\"billet_gate_between_plays\": \"rm -rf $ROOT/active; echo $ROOT/$LEGACY_DIR >$ROOT/active\"}"
ns_case b14c-moved-play escalated play2
expect_refused b14c-moved-play "Refuse a converge whose exclusion moved" "a regular file (a legacy converge transaction)"
expect_calls b14c-moved-play managed "converge-guard release" 0
echo "ok   B14: a legacy claim is classified by the role's stat under any managed binary, and an exclusion that moved under a held run refuses without recovery or cleanup"

# =============================================================================
# C. The cleanup release.
# =============================================================================
# C1. Acquired, the second call refuses the downgrade: released.
plant c1-downgrade
p c1-downgrade 'plant_root; plant_managed v0.10.1'
a c1-downgrade -e "billet_binary_src=$bins/wrap-candidate-v0.9.0"
ns_case c1-downgrade escalated
expect_refused c1-downgrade "Refuse the preparation's answer" "downgrade" "v0.9.0"
expect_final c1-downgrade "Refuse the preparation's answer" "the cleanup released the guard" "is now none"
expect_fact c1-downgrade acquired True
expect_fact c1-downgrade released True
expect_fact c1-downgrade held False
expect_state c1-downgrade active absent
expect_calls c1-downgrade managed "converge-guard release --holder h1 --cleanup --token" 1
expect_calls c1-downgrade managed "converge-guard settle" 0
# C2. The floor: a pre-R candidate on a capable host is held over and released.
plant c2-floor
p c2-floor 'plant_root; plant_managed v0.10.0'
a c2-floor -e "billet_binary_src=$bins/wrap-candidate-pre-r"
ns_case c2-floor escalated
expect_refused c2-floor "Refuse the preparation's answer" "floor" "cannot take part in the exclusion"
expect_final c2-floor "the cleanup released the guard"
expect_state c2-floor active absent
grep -q '^recovery=' "$work/cases/c2-floor/state" || fail "c2-floor: the staged directory was not retained"
plant c2b-floor-pre-r
p c2b-floor-pre-r 'plant_root; plant_pre_r'
a c2b-floor-pre-r -e "billet_binary_src=$bins/wrap-candidate-pre-r"
ns_case c2b-floor-pre-r escalated
expect_refused c2b-floor-pre-r "Refuse a staged candidate that predates the guard" "cannot take part in the exclusion"
expect_final c2b-floor-pre-r "no cleanup was attempted"
expect_state c2b-floor-pre-r active absent
plant c2c-fetch
p c2c-fetch 'plant_root; plant_managed v0.10.0'
a c2c-fetch -e billet_gate_facts=true -e billet_version=v0.10.2 -e "billet_release_url_base=$origin_url" -e "billet_release_stage=$work/cases/c2c-fetch/stage" -e billet_fetch_retries=1 -e billet_fetch_retry_delay=0
ns_case c2c-fetch escalated
expect_refused c2c-fetch "Download the pinned billet release" "404"
expect_final c2c-fetch "the cleanup released the guard" "is now none"
expect_state c2c-fetch active absent
# C3. The PostgreSQL refusals: a capable host holds and releases; a pre-R host holds nothing.
plant c3-pg
p c3-pg 'plant_root; plant_managed v0.10.0'
a c3-pg -e '{"billet_config": {"server": {"state": {"backend": "postgres"}}}}' -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case c3-pg escalated
expect_refused c3-pg "Refuse a transactional binary change against an external ledger" "at or after the release that carries the converge guard"
expect_final c3-pg "the cleanup released the guard"
expect_state c3-pg active absent
plant c3b-pg-pre-r
p c3b-pg-pre-r 'plant_root; plant_pre_r'
a c3b-pg-pre-r -e '{"billet_config": {"server": {"state": {"backend": "postgres"}}}}'
ns_case c3b-pg-pre-r escalated
expect_refused c3b-pg-pre-r "Refuse a PostgreSQL controller whose binary predates the guard" "predates the converge guard" "billet host-upgrade"
expect_final c3b-pg-pre-r "no cleanup was attempted"
expect_state c3b-pg-pre-r active absent
plant c3c-pg-bootstrap
a c3c-pg-bootstrap -e '{"billet_config": {"server": {"state": {"backend": "postgres"}}}}' -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case c3c-pg-bootstrap escalated
expect_refused c3c-pg-bootstrap "Refuse a transactional binary change against an external ledger" "at or after the release that carries the converge guard"
expect_calls c3c-pg-bootstrap suffix "" 0
expect_state c3c-pg-bootstrap active absent
# C4. A fresh process over a preparing guard: validated; a refusal releases nothing.
plant c4-preparing-rerun
p c4-preparing-rerun 'plant_root; plant_managed v0.10.1; plant_guard h1 /usr/bin/billet preparing'
a c4-preparing-rerun -e "billet_binary_src=$bins/wrap-candidate-v0.9.0"
ns_case c4-preparing-rerun escalated
expect_refused c4-preparing-rerun "Refuse the preparation's answer" "downgrade"
expect_final c4-preparing-rerun "no cleanup was attempted"
expect_state c4-preparing-rerun record_preparing True
# C5. A settled guard naming X, this run staging Y: refused, kept.
plant c5-rerun-kept
p c5-rerun-kept "plant_root; plant_managed v0.10.0; plant_recovery $REC_A v0.10.0; plant_guard h1 $ROOT/$REC_A/billet.candidate"
a c5-rerun-kept -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case c5-rerun-kept escalated
expect_refused c5-rerun-kept "Refuse the preparation's answer" "intent" "this guard records"
expect_final c5-rerun-kept "no cleanup was attempted" "release --holder h1"
expect_state c5-rerun-kept record_release_executable "$ROOT/$REC_A/billet.candidate"
# C6. The answer of an acquired first call lost: nothing released, the guard remains preparing.
plant c6-answer-lost
p c6-answer-lost 'plant_root; plant_managed v0.10.0'
e c6-answer-lost "BILLET_GATE_DROP_ANSWER=prepare:1"
ns_case c6-answer-lost escalated
expect_refused c6-answer-lost "Refuse a preparation that did not answer" "did not answer"
expect_final c6-answer-lost "no cleanup was attempted" "release --holder h1"
expect_state c6-answer-lost record_preparing True
expect_calls c6-answer-lost managed "converge-guard release" 0
# C7. The release that fails, at each boundary.
c7_plant() { # name
  plant "$1"
  p "$1" 'plant_root; plant_managed v0.10.0'
  e "$1" "BILLET_GATE_ANSWER=prepare:2:$work/corpus/no-change-outcome.json"
}
c7_plant c7-refused
e c7-refused "BILLET_GATE_FAIL=release:refuse"
ns_case c7-refused escalated
expect_refused c7-refused "Judge the preparation's answer" "outcome"
expect_final c7-refused "the cleanup was refused" "refused by the gate's wrapper" "guard held by h1" "release --holder h1"
[ "$(backing_runs c7-refused release)" -eq 0 ] || fail "c7-refused: the backing release ran"
expect_state c7-refused record_preparing True
c7_plant c7b-after-unlink
e c7b-after-unlink "BILLET_GATE_FAIL=release:crash:rmdir $ROOT/active"
ns_case c7b-after-unlink escalated
expect_refused c7b-after-unlink "Judge the preparation's answer" "outcome"
expect_final c7b-after-unlink "is now unpublished-guard" "recover --unpublished"
expect_state c7b-after-unlink active dir
expect_state c7b-after-unlink record absent
c7_plant c7c-after-rmdir
e c7c-after-rmdir "BILLET_GATE_FAIL=release:crash:fsync $ROOT"
ns_case c7c-after-rmdir escalated
expect_refused c7c-after-rmdir "Judge the preparation's answer" "outcome"
expect_final c7c-after-rmdir "is now none" "durability is not proved"
expect_fact c7c-after-rmdir released False
expect_state c7c-after-rmdir active absent
# The remainder's answer is JSON that stops mid-object, so the rescue's
# decoder runs and fails, and the final diagnostic is reached regardless.
c7_plant c7d-status-malformed
printf '{"active": "converge-guard", "gu' >"$work/cases/c7d-status-malformed/status.txt"
e c7d-status-malformed "BILLET_GATE_ANSWER=prepare:2:$work/corpus/no-change-outcome.json;status:1:$work/cases/c7d-status-malformed/status.txt"
e c7d-status-malformed "BILLET_GATE_FAIL=release:refuse"
ns_case c7d-status-malformed escalated
expect_refused c7d-status-malformed "Judge the preparation's answer" "outcome"
expect_final c7d-status-malformed "Judge the preparation's answer" "is now unknown" "inspect the host by hand"
c7_plant c7f-unexpected-entry
e c7f-unexpected-entry "BILLET_GATE_HOOK=release:1:touch $ROOT/active/unexpected"
ns_case c7f-unexpected-entry escalated
expect_final c7f-unexpected-entry "is now unpublished-guard" "anything else is for the operator to inspect"
expect_state c7f-unexpected-entry active dir
c7_plant c7g-release-hangs
e c7g-release-hangs "BILLET_GATE_FAIL=release:hang"
a c7g-release-hangs -e billet_guard_timeout=2
ns_case c7g-release-hangs escalated
expect_final c7g-release-hangs "the cleanup was ended by its bound" "guard held by h1"
expect_state c7g-release-hangs record_preparing True
# C8. After the settlement no token is known: a later refusal invokes no cleanup.
plant c8-after-settle
p c8-after-settle 'plant_root; plant_managed v0.10.0'
a c8-after-settle -e billet_gate_inclusions=2 -e "{\"billet_gate_between\": \"touch $ROOT/mutated\"}"
e c8-after-settle "BILLET_GATE_ANSWER=prepare:4:$work/corpus/no-change-outcome.json"
post c8-after-settle 'tok=$(sed -n "s/^ *\"token\": \"\([0-9a-f]*\)\".*/\1/p" "$1/private" | head -n1); "$MNT/bin/billet-v0.10.0" converge-guard release --holder h1 --cleanup --token "$tok"; echo "direct=$?"'
sed -i "s#\"\$1/private\"#\"$work/cases/c8-after-settle/private\"#" "$work/cases/c8-after-settle/post.sh"
ns_case c8-after-settle escalated
expect_refused c8-after-settle "Judge the preparation's answer" "outcome"
expect_final c8-after-settle "no cleanup was attempted" "release --holder h1"
expect_calls c8-after-settle managed "converge-guard release" 0
expect_state c8-after-settle record_preparing False
grep -q "^direct=2$" "$work/cases/c8-after-settle/post" || fail "c8: a direct cleanup with the captured token after the settlement was not refused" "$work/cases/c8-after-settle/post"
grep -q "window has closed" "$work/cases/c8-after-settle/post" || fail "c8: the direct cleanup's refusal does not name the closed window" "$work/cases/c8-after-settle/post"
plant c8b-after-settle-play
p c8b-after-settle-play 'plant_root; plant_managed v0.10.0'
e c8b-after-settle-play "BILLET_GATE_ANSWER=prepare:4:$work/corpus/no-change-outcome.json"
ns_case c8b-after-settle-play escalated play2
expect_refused c8b-after-settle-play "Judge the preparation's answer" "outcome"
expect_final c8b-after-settle-play "no cleanup was attempted"
expect_calls c8b-after-settle-play managed "converge-guard release" 0
expect_state c8b-after-settle-play active dir
echo "ok   C: a refusal inside the acquirer's window releases the guard this run acquired, one outside it never does, and every release failure names the remainder"

# =============================================================================
# D. The intent, as the second call judges it, end to end through the role.
# =============================================================================
plant d3-same-candidate
p d3-same-candidate "plant_root; plant_managed v0.10.0; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate"
a d3-same-candidate -e "billet_binary_src=$bins/wrap-candidate-v0.10.1"
ns_case d3-same-candidate escalated
expect_allowed d3-same-candidate
expect_fact d3-same-candidate acquired False
expect_state d3-same-candidate record_release_executable "$ROOT/$REC_A/billet.candidate"
plant d5-committed
p d5-committed "plant_root; plant_recovery $REC_A v0.10.1; plant_managed_file $ROOT/$REC_A/billet.candidate; plant_guard h1 $ROOT/$REC_A/billet.candidate"
ns_case d5-committed escalated
expect_allowed d5-committed
expect_calls d5-committed candidate "converge-guard prepare --holder h1 --json --no-change" 1
plant d6-candidate-not-installed
p d6-candidate-not-installed "plant_root; plant_managed v0.10.0; plant_recovery $REC_A v0.10.1; plant_guard h1 $ROOT/$REC_A/billet.candidate"
ns_case d6-candidate-not-installed escalated
expect_refused d6-candidate-not-installed "Refuse the preparation's answer" "intent"
expect_final d6-candidate-not-installed "no cleanup was attempted"
plant d7-completed-then-other
p d7-completed-then-other "plant_root; plant_recovery $REC_A v0.10.1; plant_managed_file $ROOT/$REC_A/billet.candidate; plant_guard h1 $ROOT/$REC_A/billet.candidate"
# The downgrade is admitted by name so the refusal proved is the intent's, not the downgrade's.
a d7-completed-then-other -e "billet_binary_src=$bins/wrap-candidate-v0.10.0" -e billet_allow_downgrade=true
ns_case d7-completed-then-other escalated
expect_refused d7-completed-then-other "Refuse the preparation's answer" "intent" "this guard records"
expect_state d7-completed-then-other record_release_executable "$ROOT/$REC_A/billet.candidate"
plant d8-unverifiable
p d8-unverifiable "plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet; cp \$BINS/wrap-managed-v0.10.1 /usr/bin/billet"
ns_case d8-unverifiable escalated
expect_refused d8-unverifiable "Refuse the preparation's answer" "verification"
expect_final d8-unverifiable "no cleanup was attempted"
expect_state d8-unverifiable active dir
plant d10-recovery-without-pointer
p d10-recovery-without-pointer "plant_root; plant_managed v0.10.0; plant_guard h1 /usr/bin/billet; mkdir -m 0700 $ROOT/$REC_A; plant_pointer $REC_A"
e d10-recovery-without-pointer "BILLET_GATE_HOOK=prepare:2:rm $ROOT/active/recovery"
ns_case d10-recovery-without-pointer escalated
expect_refused d10-recovery-without-pointer "Refuse the preparation's answer" "pointer"
expect_final d10-recovery-without-pointer "no cleanup was attempted"
echo "ok   D: the second call judges the intent against the record under the lock, end to end through the role"

# =============================================================================
# U. The unescalated cases, as the gate's invoker inside their namespace.
# =============================================================================
plant u1-foreign-root
p u1-foreign-root 'plant_root; plant_managed v0.10.0'
a u1-foreign-root -e "billet_upgrade_root_owner_uid=$invoker_uid"
ns_case u1-foreign-root unescalated
expect_refused u1-foreign-root "Refuse an upgrade root that is not what the role makes" "owned by uid 0"
expect_calls u1-foreign-root managed "" 0
plant u2-denied
p u2-denied "plant_root; chmod 0700 /var/lib/billet; plant_managed v0.10.0"
a u2-denied -e "billet_upgrade_root_owner_uid=$invoker_uid"
ns_case u2-denied unescalated
expect_refused u2-denied "Refuse a path the role could not examine" "could not be examined"
expect_calls u2-denied managed "" 0
grep -q "^uid=$invoker_uid$" "$work/cases/u1-foreign-root/out" 2>/dev/null || true
echo "ok   U: an ordinary account is refused at the trust boundary before anything is asked"

# =============================================================================
# S. The staging: a pinned release fetched from the origin into an exclusive
# journal, and the allocator's collision.
# =============================================================================
plant s1-pinned
p s1-pinned 'plant_root; plant_managed v0.10.0'
a s1-pinned -e billet_gate_facts=true -e billet_version=v0.10.1 -e "billet_release_url_base=$origin_url" -e "billet_release_stage=$work/cases/s1-pinned/stage" -e billet_fetch_retries=1 -e "billet_recovery_dir_suffix_command=$fakes/suffix"
ns_case s1-pinned escalated
expect_allowed s1-pinned
expect_fact s1-pinned upgrade True
rec=$(fact s1-pinned recovery)
printf '%s' "$rec" | grep -Eq "^$ROOT/recovery-[0-9]{8}T[0-9]{6}-[0-9a-f]{8}$" || fail "s1: the recovery directory is not in the grammar: $rec"
expect_calls s1-pinned suffix "" 1
expect_calls s1-pinned mkdir "-m 0700 $rec" 1
expect_state s1-pinned record_release_executable "$rec/billet.candidate"
grep -q "^recovery=$(basename "$rec") $(sha256sum "$bins/wrap-candidate-v0.10.1" | cut -d' ' -f1)$" "$work/cases/s1-pinned/state" || fail "s1: the staged candidate is not the release's" "$work/cases/s1-pinned/state"
plant s3-collision
p s3-collision "plant_root; plant_managed v0.10.0; mkdir -m 0700 $ROOT/recovery-20260909T120000-0badcafe"
printf '0badcafe\n1badcafe\n' >"$work/cases/s3-collision/suffixes"
e s3-collision "BILLET_FAKE_DATE_MODE=frozen"
e s3-collision "BILLET_FAKE_SUFFIXES=$work/cases/s3-collision/suffixes"
a s3-collision -e "billet_binary_src=$bins/wrap-candidate-v0.10.1" -e "billet_recovery_dir_suffix_command=$fakes/suffix"
ns_case s3-collision escalated
expect_allowed s3-collision
expect_fact s3-collision recovery "$ROOT/$REC_B"
expect_calls s3-collision suffix "" 2
echo "ok   S: a pinned release is fetched and staged into an exclusive journal under the guard, and a collision retries"

echo "converge guard: every case passed"
