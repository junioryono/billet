#!/usr/bin/env bash
# The host role's PREPARATION, proved case by case: the converge guard's
# refusal of a billet-managed runner (the file's original purpose), and the
# exclusion the role prepares before anything else (prepare-exclusion.yml):
# the holder, the trust boundary of the upgrade root, the classification of
# the claim through `converge-guard status`, the verified fallback to the
# executable a guard records, the same-holder rerun, the interrupted
# transaction, the binary change decided and its candidate staged before the
# hold, the exclusive allocation of the recovery directory, the capability
# floor, and the hold itself.
#
# WHY A GATE OF ITS OWN. Every other suite here runs with no RUNNER_NAME and no
# holder in the environment, so none of them reaches these branches: delete the
# guard, the holder refusal or the hold and they all stay green.
#
# HOW IT RUNS. The role is pointed at a temporary tree through the two role
# defaults that exist for this purpose (billet_upgrade_root and
# billet_managed_binary), with FAKES first on PATH: a `billet` that records
# every invocation (argv, cwd, uid, PATH) to a log and answers per the
# environment, a candidate (a second executable that writes a marker before
# anything else, so "never run" is asserted by a file), and recording
# wrappers for `timeout`, `date`, `mkdir`, `systemctl` and the recovery
# directory's suffix generator. The fake's own statuses are the answers the
# REAL command produced over planted shapes (tests/fixtures/guard-status/,
# written by cmd/billet's test), corrupted one member at a time; a fake never
# invents a shape the command does not produce.
#
# THE LAUNCH IS ESCALATED: `sudo -n env <every variable the role or a fake
# reads> ansible-playbook ...`, nothing inherited through sudo, and the role
# runs with ansible_become=false because the whole process is already root and
# sudo's env_reset would otherwise scrub the fakes' PATH and variables from
# every escalated task. The launch is PROVED first (M8): a probe play runs the
# fake `billet version` under become and the log must show uid 0, the fake's
# own path as argv[0] and the holder the role default resolved. In CI
# (BILLET_GATE_REQUIRE_ROOT=1) a failed launch fails the gate; elsewhere the
# escalated cases skip with the reason. The cases that need a NON-ROOT
# identity (a denied stat, a Mac's unescalated hold, an unwritable root) run
# without sudo as the invoking user over fixtures the escalated leg planted,
# and prove their uid with a separate unescalated task before the role runs.
# Every tree an escalated case reads is root-owned by then, because the
# preparation's trust boundary requires it; what the gate reads back from
# such a tree it reads with the same escalation.
#
# EVERY ALLOWED CASE MUST EXIT SUCCESSFULLY; every refusal is judged by the
# FAILING TASK'S NAME and its diagnostic; a prohibited effect (a hold, an
# allocation, an execution of an untrusted candidate) is asserted directly
# from the log, the marker and the tree.
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
collection_root=$(cd "$here/../../../.." && pwd)
role_tasks="$here/../roles/host/tasks"
fixtures="$here/fixtures/guard-status"

work=$(mktemp -d)
have_root=0
if sudo -n true 2>/dev/null; then have_root=1; fi
# rd CMD...: read or remove what an escalated case left root-owned.
rd() { if [ "$have_root" = 1 ]; then sudo -n "$@"; else "$@"; fi; }
cleanup() { rd rm -rf "$work"; }
trap cleanup EXIT

# ANSIBLE'S OWN INTERPRETER, which has PyYAML and is the one the modules run
# under; the first python3 on PATH need not have it.
python=$(ansible --version 2>/dev/null | sed -n 's/.*python version.*(\(.*\)).*/\1/p' | head -n1)
if [ -z "$python" ] || [ ! -x "$python" ]; then
  python=$(command -v python3) || { echo "converge-guard-check: no python3" >&2; exit 1; }
fi
ansible_playbook=$(command -v ansible-playbook) || { echo "converge-guard-check: no ansible-playbook" >&2; exit 1; }
collections_path="$collection_root:$HOME/.ansible/collections:/usr/share/ansible/collections"

fail() {
  echo "FAIL: $1" >&2
  if [ -n "${2:-}" ] && [ -f "$2" ]; then
    # The failing task and its diagnostic, when there is one; else the tail.
    if grep -Eq "^(fatal|failed): " "$2"; then
      grep -En -B8 -A25 -m1 "^(fatal|failed): " "$2" >&2
    else
      tail -n 60 "$2" >&2
    fi
  fi
  exit 1
}
# json_var NAME VALUE: an -e argument whose value may carry spaces or quotes.
json_var() { "$python" -c 'import json,sys; print(json.dumps({sys.argv[1]: sys.argv[2]}))' "$1" "$2"; }

# --- the modules' own checks -------------------------------------------------
"$python" "$here/guard_status_check.py"
"$python" "$here/guard_fallback_check.py"

# --- the guard runs first ----------------------------------------------------
#
# A REFUSAL IS ONLY USEFUL BEFORE THE TRANSACTION IT PROTECTS HAS STARTED, and
# nothing below can observe that ordering: the cases prove the guard fires, not
# that it fires first. A task inserted above it would leave them all green.
first_task=$(grep -n '^- name:' "$role_tasks/main.yml" | head -1 | cut -d: -f1)
guard_task=$(grep -n '^- name: Refuse a converge that would destroy the job running it' \
  "$role_tasks/main.yml" | head -1 | cut -d: -f1)
if [ -z "$guard_task" ] || [ "$first_task" != "$guard_task" ]; then
  fail "the converge guard is not the first task in main.yml (first task at line ${first_task:-none}, guard at line ${guard_task:-none})"
fi
second_task=$(grep -n '^- name:' "$role_tasks/main.yml" | sed -n 2p | cut -d: -f1)
prepare_task=$(grep -n '^- name: Prepare the exclusion before anything changes this host' "$role_tasks/main.yml" | head -1 | cut -d: -f1)
if [ -z "$prepare_task" ] || [ "$second_task" != "$prepare_task" ]; then
  fail "the exclusion's preparation is not the second task in main.yml (second at line ${second_task:-none}, preparation at line ${prepare_task:-none}); a task between the guard and the hold runs on a host a rollout may be moving"
fi
first_prep=$(grep '^- name:' "$role_tasks/prepare-exclusion.yml" | head -1)
case "$first_prep" in
  *"Refuse a converge that would destroy the job running it") ;;
  *) fail "prepare-exclusion.yml's first task is not the converge guard's import: $first_prep" ;;
esac
echo "ok   the guard is the first task in the role, the preparation the second, and the guard the preparation's first import"

# --- the fakes ---------------------------------------------------------------
fakes="$work/fakes"
mkdir -p "$fakes"

# THE FAKE billet. Generated twice, as the MANAGED binary and as the CANDIDATE
# (which writes its marker first and answers its own variables), so the two
# differ by digest and the log says which one answered.
write_billet() { # path role
  cat >"$1" <<'FAKE'
#!/bin/bash
# The fake billet: records every invocation and answers per the environment.
set -u
ROLE=__ROLE__
U=$(printf '%s' "$ROLE" | tr a-z A-Z)
if [ "$ROLE" = candidate ] && [ -n "${BILLET_FAKE_MARKER:-}" ]; then : >"$BILLET_FAKE_MARKER"; fi
log=${BILLET_FAKE_LOG:-/dev/null}
{
  printf 'role=%s\n' "$ROLE"
  printf 'argv0=%s\n' "$0"
  printf 'argv=%s\n' "$*"
  printf 'cwd=%s\n' "$PWD"
  printf 'uid=%s\n' "$(id -u)"
  printf 'path=%s\n' "$PATH"
  printf -- '---\n'
} >>"$log"
var() { eval "printf '%s' \"\${BILLET_FAKE_${U}_$1:-}\""; }
root=${BILLET_FAKE_ROOT:-/nonexistent}
case "${1:-}" in
  version)
    v=$(var VERSION)
    seq=$(var VERSION_SEQUENCE)
    if [ -n "$seq" ] && [ -s "$seq" ]; then
      v=$(head -n1 "$seq")
      tail -n +2 "$seq" >"$seq.next" && mv "$seq.next" "$seq"
    fi
    printf 'billet %s linux/amd64\n' "${v:-v0.10.0}"
    exit 0 ;;
  converge-guard) ;;
  *) exit 0 ;;
esac
shift
if [ -n "$(var PRE_R)" ]; then
  echo 'unknown command "converge-guard"' >&2
  exit 2
fi
if [ -n "$(var HANG)" ]; then sleep 3600; fi
case "${1:-}" in
  status)
    f=$(var STATUS)
    if [ "$f" = exit1 ]; then echo 'cannot examine the claim' >&2; exit 1; fi
    if [ -n "$f" ]; then cat "$f"; exit 0; fi
    exec "$BILLET_FAKE_PYTHON" "$BILLET_FAKE_STATUS_EMULATOR" "$root"
    ;;
  hold)
    if [ -n "$(var HOLD_HANG)" ]; then sleep 3600; fi
    r=$(var HOLD_REFUSE)
    if [ -n "$r" ]; then printf '%s\n' "$r" >&2; exit 1; fi
    holder=""; candidate=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --holder) holder=$2; shift 2 ;;
        --candidate) candidate=$2; shift 2 ;;
        *) shift ;;
      esac
    done
    exe=${candidate:-$(readlink -f "$0")}
    exec "$BILLET_FAKE_PYTHON" "$BILLET_FAKE_HOLD_EMULATOR" "$root" "$holder" "$exe"
    ;;
  *)
    echo "the fake does not emulate converge-guard ${1:-}" >&2
    exit 1 ;;
esac
FAKE
  sed -i.bak "s/__ROLE__/$2/" "$1" && rm -f "$1.bak"
  chmod 0755 "$1"
}
write_billet "$fakes/billet-managed" managed
write_billet "$fakes/billet-candidate" candidate

cat >"$fakes/status-emulator.py" <<'PY'
"""Answer `converge-guard status --json` the way the command does, over the
planted tree: the shape by lstat, the record's members, the pointer's
presence by any entry named `recovery`, and the recorded executable's digest
now against the one recorded."""
import hashlib, json, os, stat as s, sys
root = sys.argv[1]
active = os.path.join(root, "active")
try:
    st = os.lstat(active)
except FileNotFoundError:
    print(json.dumps({"active": "none"})); sys.exit(0)
if s.S_ISLNK(st.st_mode):
    print(json.dumps({"active": "host-upgrade"})); sys.exit(0)
if s.S_ISREG(st.st_mode):
    print(json.dumps({"active": "legacy-role"})); sys.exit(0)
if not s.S_ISDIR(st.st_mode):
    print(json.dumps({"active": "unknown", "why": "the claim is neither a symlink, a file nor a directory"})); sys.exit(0)
record = os.path.join(active, "guard.json")
if not os.path.exists(record):
    print(json.dumps({"active": "unpublished-guard"})); sys.exit(0)
pointer = os.path.lexists(os.path.join(active, "recovery"))
try:
    g = json.load(open(record))
    if not g.get("holder"):
        raise ValueError("the record names no holder")
except Exception as exc:
    print(json.dumps({"active": "converge-guard", "guard": {
        "holder": "", "claimed_at": "", "hostname": "", "recovery_pointer": pointer,
        "release_executable": "", "release_executable_sha256": "",
        "release_executable_verified": {"unknown": "the record is not JSON: %s" % exc},
        "record_error": "the record is not JSON: %s" % exc}}))
    sys.exit(0)
exe = g.get("release_executable", "")
try:
    digest = hashlib.sha256(open(exe, "rb").read()).hexdigest()
    verified = digest == g.get("release_executable_sha256")
except OSError as exc:
    verified = {"unknown": "cannot read the executable: %s" % exc}
print(json.dumps({"active": "converge-guard", "guard": {
    "holder": g.get("holder", ""), "claimed_at": g.get("claimed_at", ""),
    "hostname": g.get("hostname", ""), "recovery_pointer": pointer,
    "release_executable": exe, "release_executable_sha256": g.get("release_executable_sha256", ""),
    "release_executable_verified": verified}}))
PY

cat >"$fakes/hold-emulator.py" <<'PY'
"""Emulate `converge-guard hold`: a same-holder guard validates and touches
nothing; another holder's refuses; no claim creates the root when absent and
publishes the five-member record naming the executable and its digest."""
import hashlib, json, os, socket, sys, time
root, holder, exe = sys.argv[1:4]
active = os.path.join(root, "active")
if os.path.lexists(active):
    record = os.path.join(active, "guard.json")
    if os.path.isdir(active) and os.path.exists(record):
        g = json.load(open(record))
        if g.get("holder") == holder:
            sys.exit(0)
        sys.stderr.write("converge-guard: a converge guard is held on this host: held by %s since %s\n" % (g.get("holder"), g.get("claimed_at")))
        sys.exit(1)
    sys.stderr.write("converge-guard: %s exists and is not a guard\n" % active)
    sys.exit(1)
os.makedirs(root, mode=0o700, exist_ok=True)
os.mkdir(active, 0o700)
# An updater finishing its install in the last instant before the hold
# excludes it (S10): the managed binary gains a byte.
replace = os.environ.get("BILLET_FAKE_HOLD_REPLACE", "")
if replace:
    with open(replace, "ab") as f:
        f.write(b"# installed by an updater during the hold\n")
digest = hashlib.sha256(open(exe, "rb").read()).hexdigest()
record = {"holder": holder, "claimed_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
          "hostname": socket.gethostname(), "release_executable": exe, "release_executable_sha256": digest}
tmp = os.path.join(active, "guard.json.tmp")
with open(tmp, "w") as f:
    json.dump(record, f, indent=2); f.write("\n")
os.chmod(tmp, 0o600)
os.rename(tmp, os.path.join(active, "guard.json"))
PY

# RECORDING WRAPPERS around the real tools the preparation runs, so the log
# shows they were reached and in what order. The mkdir wrapper records only
# the allocator's own form (`-m 0700 <dir>`), because Ansible creates its
# temporary directories with the same command on the same PATH.
for tool in timeout date mkdir systemctl sync; do
  real=$(command -v "$tool" || true)
  if [ "$tool" = mkdir ]; then
    cat >"$fakes/$tool" <<FAKE
#!/bin/sh
case "\$1 \$2" in
  "-m 0700") printf 'role=mkdir\nargv0=%s\nargv=%s\nuid=%s\n---\n' "\$0" "\$*" "\$(id -u)" >>"\${BILLET_FAKE_LOG:-/dev/null}" ;;
esac
exec "$real" "\$@"
FAKE
    chmod 0755 "$fakes/$tool"
    continue
  fi
  cat >"$fakes/$tool" <<FAKE
#!/bin/sh
if [ "$tool" = systemctl ] && [ "\${1:-}" = --version ]; then exec "$real" "\$@"; fi
printf 'role=$tool\nargv0=%s\nargv=%s\nuid=%s\n---\n' "\$0" "\$*" "\$(id -u)" >>"\${BILLET_FAKE_LOG:-/dev/null}"
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
      # THE FLUSH OF A FRESHLY MADE ROOT'S PARENT is the one thing the
      # preparation runs between its inspection of the managed binary and
      # its inspection of the claim, so a case stages an updater finishing
      # there: the wrapper puts a binary back before the real flush.
      cat >>"$fakes/$tool" <<FAKE
if [ -n "\${BILLET_FAKE_SYNC_RESTORE:-}" ]; then cp "\$BILLET_FAKE_SYNC_RESTORE" "\$BILLET_FAKE_SYNC_RESTORE_TO"; fi
exec "$real" "\$@"
FAKE
      ;;
    systemctl)
      # NEVER REACHED BY THE PREPARATION; a call is a finding. Fact gathering
      # on a systemd host asks `systemctl --version` for the service manager
      # (measured on CI's runner), which is not the preparation's and passes
      # through unrecorded.
      echo 'echo "systemctl was called by the preparation" >&2; exit 97' >>"$fakes/$tool" ;;
    *)
      echo "exec \"$real\" \"\$@\"" >>"$fakes/$tool" ;;
  esac
  chmod 0755 "$fakes/$tool"
done

# THE SUFFIX GENERATOR: records each draw and pops the next line of the
# case's sequence file, or draws from urandom when there is none.
cat >"$fakes/suffix" <<'FAKE'
#!/bin/sh
printf 'role=suffix\nargv0=%s\nargv=%s\nuid=%s\n---\n' "$0" "$*" "$(id -u)" >>"${BILLET_FAKE_LOG:-/dev/null}"
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

# --- the plays ---------------------------------------------------------------
cat >"$work/inventory.ini" <<'INV'
[billet_hosts]
localhost ansible_connection=local
INV

# ONE PLAY FOR EVERY CASE: the entry point the case names (the preparation by
# default; the allocator, the staging or the fallback reader driven directly),
# a hook between two inclusions, and a report of the facts the gate reads.
cat >"$work/play.yml" <<'PLAY'
---
- name: Exercise the host role's preparation
  hosts: billet_hosts
  gather_facts: "{{ billet_gate_facts | default(false) | bool }}"
  vars:
    billet_gate_entry: prepare-exclusion
    billet_gate_inclusions: 1
    billet_gate_between: ""
  tasks:
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
          - (billet_gate_expect_root | default(true) | bool) == (billet_gate_uid.stdout | trim == '0')
        fail_msg: "this case runs as uid {{ billet_gate_uid.stdout | trim }}, want {{ 'root' if billet_gate_expect_root | default(true) | bool else 'an ordinary account' }}"

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

    - name: Report the facts the gate reads
      ansible.builtin.debug:
        msg: >-
          GATE shape={{ billet_upgrade_claim_shape | default('undef') }}
          interrupted={{ billet_interrupted_upgrade | default('undef') }}
          recovery={{ billet_upgrade_recovery_dir | default('undef') }}
          upgrade={{ billet_binary_upgrade | default('undef') }}
          held={{ billet_exclusion_held | default('undef') }}
          executable={{ billet_exclusion_executable | default('undef') }}
          holder={{ billet_exclusion_holder | default('undef') }}
          version={{ billet_version | default('undef') }}
          resolved={{ billet_resolved_version | default('undef') }}
          end=.
PLAY

cat >"$work/probe.yml" <<'PLAY'
---
- name: Prove the gate's launch
  hosts: billet_hosts
  gather_facts: false
  tasks:
    - name: Run the fake billet under escalation
      ansible.builtin.command:
        argv: ["{{ billet_gate_managed }}", version]
      environment:
        BILLET_FAKE_LOG: "{{ billet_gate_log }}"
      changed_when: false
      become: true
    # The role's defaults, made visible to the play, so the holder is the one
    # the role default resolves and not a copy of its expression.
    - name: Load the role's defaults
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: converge-guard
        public: true
    - name: Report the holder the role default resolved
      ansible.builtin.debug:
        msg: "GATE holder={{ billet_converge_guard_holder }}"
PLAY

# --- the launch --------------------------------------------------------------
require_root=${BILLET_GATE_REQUIRE_ROOT:-0}
if [ "$have_root" = 0 ]; then
  if [ "$require_root" = 1 ]; then
    fail "BILLET_GATE_REQUIRE_ROOT=1 and sudo -n is not available; the escalated cases cannot run"
  fi
  echo "skip the escalated cases: sudo -n is not available here (set BILLET_GATE_REQUIRE_ROOT=1 to make that a failure)"
fi

# THE HOLDER AND THE RUNNER every case launches with; a case that needs another
# sets them before its launch and puts them back after, never through a prefix
# assignment on the function call, whose scope a shell need not honour.
HOLDER=h1
RUNNER=""
KEEP_OWNER=0
CASE_DIR_MODE=0755
status=0

# run_case NAME escalated|unescalated [NAME=VALUE ...] -- [ansible args ...]
# Every variable the role or a fake reads is passed after the escalation;
# nothing is inherited through sudo. The case's tree is $work/cases/NAME, and
# an escalated case's tree is root-owned before the launch (the trust
# boundary the preparation requires), unless the case planted its own owners.
run_case() {
  local name=$1 mode=$2; shift 2
  local case_dir=$work/cases/$name
  mkdir -p "$case_dir"
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  local launcher=()
  # The out and log files are the invoker's own, so the redirections below
  # can truncate them; a case launched twice has a root-owned directory by its
  # second launch, so they are made through the escalation and handed back.
  for f in out log; do
    if [ ! -e "$case_dir/$f" ]; then
      if [ -w "$case_dir" ]; then : >"$case_dir/$f"; else rd touch "$case_dir/$f"; rd chown "$(id -u)" "$case_dir/$f"; fi
    fi
    : >"$case_dir/$f"
  done
  if [ "$mode" = escalated ]; then
    launcher=(sudo -n)
    # THE ANCESTORS OF AN ESCALATED TREE ARE ROOT'S, as the fallback module
    # requires of every directory above the root's parent (owned by root or the
    # root's owner, writable by others only under the sticky bit). The two
    # directories the invoker keeps creating cases under are sticky and
    # world-writable; the case directory itself is 0755, because the kernel's
    # fs.protected_regular refuses even root an O_CREAT open of another
    # account's file inside a sticky world-writable directory, and the fakes
    # append to the invoker's log there (measured: an empty log under 1777).
    sudo -n chown root "$work" "$work/cases"
    sudo -n chmod 1777 "$work" "$work/cases"
    sudo -n chown root "$case_dir"
    sudo -n chmod "${CASE_DIR_MODE:-0755}" "$case_dir"
    if [ -d "$case_dir/lib" ] && [ "$KEEP_OWNER" = 0 ]; then sudo -n chown -R root:root "$case_dir/lib"; fi
  fi
  # Ansible's temporary directory: under the case directory while the invoker
  # can write there, else beside it (an unescalated case over a root-owned
  # case directory).
  local tmp="$case_dir/tmp"
  if [ ! -w "$case_dir" ]; then tmp="$work/tmp-$name"; mkdir -p "$tmp"; fi
  set +e
  "${launcher[@]+"${launcher[@]}"}" env \
    PATH="$fakes:$PATH" \
    HOME="$HOME" \
    ANSIBLE_COLLECTIONS_PATH="$collections_path" \
    ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_NOCOLOR=1 ANSIBLE_FORCE_COLOR=0 \
    ANSIBLE_LOCAL_TEMP="$tmp" ANSIBLE_REMOTE_TEMP="$tmp" \
    RUNNER_NAME="$RUNNER" \
    BILLET_CONVERGE_GUARD_HOLDER="$HOLDER" \
    BILLET_FAKE_LOG="$case_dir/log" \
    BILLET_FAKE_ROOT="$case_dir/lib/billet/upgrades" \
    BILLET_FAKE_MARKER="$case_dir/marker" \
    BILLET_FAKE_PYTHON="$python" \
    BILLET_FAKE_STATUS_EMULATOR="$fakes/status-emulator.py" \
    BILLET_FAKE_HOLD_EMULATOR="$fakes/hold-emulator.py" \
    "${envs[@]+"${envs[@]}"}" \
    "$ansible_playbook" -i "$work/inventory.ini" "$work/play.yml" \
    -e billet_upgrade_root="$case_dir/lib/billet/upgrades" \
    -e billet_managed_binary="$case_dir/bin/billet" \
    -e ansible_become=false \
    -e billet_gate_expect_root="$([ "$mode" = escalated ] && echo true || echo false)" \
    "$@" >"$case_dir/out" 2>&1
  status=$?
  set -e
}

# The FAILING TASK'S NAME: the last TASK header before the first fatal line,
# without the role prefix.
failed_at() {
  awk '/^TASK \[/ { t=$0; sub(/^TASK \[/, "", t); sub(/\] \*+$/, "", t); sub(/^junioryono\.billet\.host : /, "", t) }
       /^(fatal|failed): / { print t; exit }' "$work/cases/$1/out"
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
expect_calls() { # case role fragment count
  local n
  n=$(count_calls "$1" "$2" "$3")
  [ "$n" -eq "$4" ] || fail "$1: $2 '$3' called $n times, want $4" "$work/cases/$1/log"
}
# A statically imported task prints its header even when its `when` skips it,
# so "did not run" is: no header, or a header followed by a skip.
expect_no_task() { # case task
  local verdict
  verdict=$(awk -v want="TASK [junioryono.billet.host : $2]" '
    index($0, want) == 1 { seen = 1; next }
    seen && /^skipping: / { seen = 0; next }
    seen && /^(ok|changed|fatal|failed): / { print "ran"; exit }
    seen && /^TASK \[/ { seen = 0 }' "$work/cases/$1/out")
  if [ "$verdict" = ran ]; then
    fail "$1: the task \"$2\" ran, and must not have" "$work/cases/$1/out"
  fi
}
expect_task() { grep -qF "TASK [junioryono.billet.host : $2]" "$work/cases/$1/out" || fail "$1: the task \"$2\" did not run" "$work/cases/$1/out"; }
log_empty() { [ ! -s "$work/cases/$1/log" ] || fail "$1: a fake was called, and none may be" "$work/cases/$1/log"; }
marker_absent() { [ ! -e "$work/cases/$1/marker" ] || fail "$1: the candidate was RUN (its marker exists), and it must never be"; }
marker_present() { [ -e "$work/cases/$1/marker" ] || fail "$1: the candidate was never run, and this case expects it to answer"; }
sha() { rd sha256sum "$1" | cut -d' ' -f1; }
root_of() { printf '%s' "$work/cases/$1/lib/billet/upgrades"; }
root_ls() { rd ls "$(root_of "$1")"; }

# plant NAME: a fresh case tree with the managed fake and the candidate source.
plant() {
  local case_dir=$work/cases/$1
  rd rm -rf "$case_dir"
  mkdir -p "$case_dir/bin" "$case_dir/lib/billet/upgrades" "$case_dir/src"
  cp "$fakes/billet-managed" "$case_dir/bin/billet"
  cp "$fakes/billet-candidate" "$case_dir/src/billet"
  chmod 0755 "$case_dir/lib/billet"
  chmod 0700 "$case_dir/lib/billet/upgrades"
}
as_root() { sudo -n "$@"; }
# write_guard CASE HOLDER EXE [CLAIMED_AT]: a published guard recording EXE.
write_guard() {
  local case_dir=$work/cases/$1 holder=$2 exe=$3 at=${4:-2026-09-09T12:00:00Z}
  mkdir -p "$case_dir/lib/billet/upgrades/active"
  chmod 0700 "$case_dir/lib/billet/upgrades/active"
  "$python" - "$case_dir/lib/billet/upgrades/active/guard.json" "$holder" "$exe" "$at" "$(sha "$exe")" <<'PY'
import json, os, sys
path, holder, exe, at, digest = sys.argv[1:6]
with open(path, "w") as f:
    json.dump({"holder": holder, "claimed_at": at, "hostname": "billet-control-01",
               "release_executable": exe, "release_executable_sha256": digest}, f, indent=2)
    f.write("\n")
os.chmod(path, 0o600)
PY
}
# plant_recorded CASE: a recovery directory holding the candidate, recorded by a guard of h1.
plant_recorded() {
  local case_dir=$work/cases/$1 dir
  dir=$case_dir/lib/billet/upgrades/recovery-20260909T120000-0badcafe
  mkdir -p "$dir"
  cp "$case_dir/src/billet" "$dir/billet.candidate"
  write_guard "$1" "${2:-h1}" "$dir/billet.candidate"
}
# A corrupted status answer from the real command's fixtures (P5).
mkstatus() { # out-file case-id
  "$python" - "$fixtures" "$1" "$2" <<'PY'
import copy, json, sys
fixtures, out, case = sys.argv[1:4]
def fx(n): return json.load(open("%s/%s.json" % (fixtures, n)))
h = fx("healthy")
def corrupt(path, value=None, delete=False):
    r = copy.deepcopy(h); d = r
    for k in path[:-1]: d = d[k]
    if delete: del d[path[-1]]
    else: d[path[-1]] = value
    return r
aa_pointer = corrupt(["guard", "recovery_pointer"], True)
aa_pointer["guard"]["release_executable_verified"] = False
table = {
    "d": [], "e": "x",
    "f": corrupt(["active"], delete=True), "g": corrupt(["active"], 1), "h": corrupt(["active"], "held"),
    "i": fx("none"), "j": fx("host-upgrade"), "k": fx("legacy-file"),
    "l": {"active": "unknown", "why": "the claim is neither a symlink, a file nor a directory"},
    "l2": {"active": "unknown"}, "l3": {"active": "unknown", "why": None},
    "l4": {"active": "unknown", "why": 1}, "l5": {"active": "unknown", "why": ""},
    "m": corrupt(["guard"], delete=True), "n": corrupt(["guard"], "x"),
    "o": corrupt(["guard", "holder"], delete=True), "o2": corrupt(["guard", "holder"], 1),
    "p": corrupt(["guard", "holder"], ""), "p2": corrupt(["guard", "holder"], "h 1"),
    "p3": corrupt(["guard", "holder"], "h\t1"), "p4": corrupt(["guard", "holder"], "h\x011"),
    "p5": corrupt(["guard", "holder"], "h" * 300), "q": corrupt(["guard", "holder"], "h/1"),
    "q2": corrupt(["guard", "record_error"], 1), "r": corrupt(["guard", "claimed_at"], "yesterday"),
    "s": corrupt(["guard", "recovery_pointer"], delete=True), "t": corrupt(["guard", "recovery_pointer"], "false"),
    "u": corrupt(["guard", "release_executable"], "billet"),
    "v": corrupt(["guard", "release_executable_sha256"], "a" * 63),
    "w": corrupt(["guard", "release_executable_verified"], delete=True),
    "x": corrupt(["guard", "release_executable_verified"], "unknown"),
    "x2": corrupt(["guard", "release_executable_verified"], {}),
    "x3": corrupt(["guard", "release_executable_verified"], {"unknown": 1}),
    "x4": corrupt(["guard", "release_executable_verified"], {"unknown": "x", "more": 1}),
    "y": fx("unpublished"), "z": fx("malformed-record"),
    "aa-no-pointer": fx("verification-false"),
    "aa-pointer": aa_pointer,
    "ab": corrupt(["guard", "release_executable_verified"], {"unknown": "cannot read the executable"}),
}
if case == "c":
    open(out, "w").write("not json\n")
else:
    json.dump(table[case], open(out, "w"))
PY
}

# =============================================================================
# The converge guard's own cases (the file's original purpose), through the
# unescalated launch: the guard reads RUNNER_NAME on the controller.
# =============================================================================
plant guard-refused
RUNNER="billet-lease-abc123"; run_case guard-refused unescalated -- -e billet_gate_entry=converge-guard; RUNNER=""
expect_refused guard-refused "Refuse a converge driven from a billet-managed runner" "runner billet itself manages"

plant guard-override
RUNNER="billet-lease-abc123"; run_case guard-override unescalated -- -e billet_gate_entry=converge-guard -e billet_allow_converge_from_billet_runner=true; RUNNER=""
expect_allowed guard-override

plant guard-plain
RUNNER="gh-deploy-runner-1"; run_case guard-plain unescalated -- -e billet_gate_entry=converge-guard; RUNNER=""
expect_allowed guard-plain

plant guard-workstation
run_case guard-workstation unescalated -- -e billet_gate_entry=converge-guard
expect_allowed guard-workstation

plant guard-substring
RUNNER="ci-billet-deploy"; run_case guard-substring unescalated -- -e billet_gate_entry=converge-guard; RUNNER=""
expect_allowed guard-substring

# THE GUARD IS THE PREPARATION'S FIRST REFUSAL TOO: through the whole
# preparation, a managed runner is refused before any stat, the log empty.
plant guard-in-preparation
RUNNER="billet-lease-abc123"; run_case guard-in-preparation unescalated --; RUNNER=""
expect_refused guard-in-preparation "Refuse a converge driven from a billet-managed runner" "runner billet itself manages"
expect_no_task guard-in-preparation "Inspect the durable claim"
log_empty guard-in-preparation

if [ "$have_root" = 0 ]; then
  echo "converge guard: the guard's cases pass; the preparation's escalated cases were skipped (no sudo -n)"
  exit 0
fi

# =============================================================================
# M8: the escalated launch is proved before it is relied on.
# =============================================================================
plant launch
set +e
sudo -n env PATH="$fakes:$PATH" HOME="$HOME" ANSIBLE_COLLECTIONS_PATH="$collections_path" \
  ANSIBLE_STDOUT_CALLBACK=default ANSIBLE_NOCOLOR=1 ANSIBLE_FORCE_COLOR=0 \
  ANSIBLE_LOCAL_TEMP="$work/cases/launch/tmp" ANSIBLE_REMOTE_TEMP="$work/cases/launch/tmp" \
  RUNNER_NAME="" BILLET_CONVERGE_GUARD_HOLDER=from-the-environment \
  "$ansible_playbook" -i "$work/inventory.ini" "$work/probe.yml" \
  -e billet_gate_managed="$work/cases/launch/bin/billet" -e billet_gate_log="$work/cases/launch/log" \
  >"$work/cases/launch/out" 2>&1
status=$?
set -e
[ "$status" -eq 0 ] || fail "M8: the escalated launch failed" "$work/cases/launch/out"
grep -q '^uid=0$' "$work/cases/launch/log" || fail "M8: the fake did not run as root under become" "$work/cases/launch/log"
grep -qF "argv0=$work/cases/launch/bin/billet" "$work/cases/launch/log" || fail "M8: argv[0] is not the fake's own path" "$work/cases/launch/log"
grep -qF "GATE holder=from-the-environment" "$work/cases/launch/out" || fail "M8: the role default did not resolve the holder from the environment" "$work/cases/launch/out"
echo "ok   M8: the escalated launch runs the fake as root by its own path and resolves the holder from the environment"

# =============================================================================
# P. The preparation.
# =============================================================================

# P1. No holder outside check mode; -e and the environment each admit; -e wins.
plant p1-none
HOLDER=""; run_case p1-none escalated --; HOLDER=h1
expect_refused p1-none "Refuse a converge without a holder" "BILLET_CONVERGE_GUARD_HOLDER"
expect_no_task p1-none "Inspect the durable claim"
log_empty p1-none

plant p1-env
HOLDER="env-holder"; run_case p1-env escalated --; HOLDER=h1
expect_allowed p1-env
rd grep -q '"holder": "env-holder"' "$(root_of p1-env)/active/guard.json" || fail "p1-env: the guard does not name the environment's holder"

plant p1-extra
HOLDER="env-holder"; run_case p1-extra escalated -- -e billet_converge_guard_holder=extra-holder; HOLDER=h1
expect_allowed p1-extra
rd grep -q '"holder": "extra-holder"' "$(root_of p1-extra)/active/guard.json" || fail "p1-extra: -e did not win over the environment"
echo "ok   P1: the holder is required, comes from the environment or -e, and -e wins"

# P2. An absent claim holds through the managed binary; a second inclusion
# classifies again and never holds again; the later classification governs.
plant p2
run_case p2 escalated -- -e billet_gate_inclusions=2
expect_allowed p2
expect_calls p2 managed "converge-guard status --json" 2
expect_calls p2 managed "converge-guard hold --holder h1" 1
expect_fact p2 shape guard
expect_fact p2 held True
rd cat "$(root_of p2)/active/guard.json" | "$python" -c 'import json,sys; g=json.load(sys.stdin); assert sorted(g)==["claimed_at","holder","hostname","release_executable","release_executable_sha256"], g' || fail "p2: guard.json does not carry exactly the five members"
[ "$(calls p2 managed | awk '{print $3}' | tr '\n' ' ')" = "status hold status " ] || fail "p2: the managed binary's calls are not status, hold, status" "$work/cases/p2/log"
echo "ok   P2: an absent claim is held once through the managed binary and re-classified on every inclusion"

for moved in absent foreign legacy symlink; do
  plant "p2-moved-$moved"
  root=$(root_of "p2-moved-$moved")
  case $moved in
    absent) between="rm -rf $root/active" ;;
    foreign) between="printf '{\"holder\": \"h2\", \"claimed_at\": \"2026-09-09T12:00:00Z\", \"hostname\": \"x\", \"release_executable\": \"$work/cases/p2-moved-$moved/bin/billet\", \"release_executable_sha256\": \"$(sha "$work/cases/p2-moved-$moved/bin/billet")\"}' > $root/active/guard.json" ;;
    legacy) between="rm -rf $root/active; printf '%s\\n' $root/20260909T120000000000000 > $root/active" ;;
    symlink) between="rm -rf $root/active; ln -s $root/upgrade-x $root/active" ;;
  esac
  run_case "p2-moved-$moved" escalated -- -e billet_gate_inclusions=2 -e "$(json_var billet_gate_between "$between")"
  case $moved in
    foreign) expect_refused "p2-moved-$moved" "Refuse a converge whose exclusion moved to another holder" "as h1" "a guard held by h2" ;;
    *) expect_refused "p2-moved-$moved" "Refuse a converge whose exclusion moved" "as h1" ;;
  esac
  expect_calls "p2-moved-$moved" managed "converge-guard hold" 1
  expect_no_task "p2-moved-$moved" "Allocate a recovery directory exclusively"
done
echo "ok   P2: a later inclusion whose claim moved refuses without reacquiring"

# P3. A symlink at active.
plant p3
ln -s "$(root_of p3)/upgrade-20260909" "$(root_of p3)/active"
run_case p3 escalated --
expect_refused p3 "Refuse to converge over a host upgrade billet itself is running" "billet host-upgrade --status" "--resume"
log_empty p3

# P4. A regular file at active is a legacy pointer: nothing held, nothing logged.
plant p4
printf '%s\n' "$(root_of p4)/20260909T120000000000000" >"$(root_of p4)/active"
run_case p4 escalated --
expect_allowed p4
expect_fact p4 shape legacy-file
expect_fact p4 interrupted True
log_empty p4
grep -q "predates the converge guard" "$work/cases/p4/out" || fail "p4: the report does not name the pre-R race"
echo "ok   P3, P4: a Go claim refuses and a legacy pointer holds nothing"

# P5. The status schema, one member corrupted per case, over a directory claim.
p5_case() { # id expected-task fragment...
  local id=$1 task=$2; shift 2
  plant "p5-$id"
  write_guard "p5-$id" h1 "$work/cases/p5-$id/bin/billet"
  mkstatus "$work/cases/p5-$id/status.json" "$id"
  run_case "p5-$id" escalated BILLET_FAKE_MANAGED_STATUS="$work/cases/p5-$id/status.json" --
  expect_refused "p5-$id" "$task" "$@"
  expect_calls "p5-$id" managed "converge-guard hold" 0
}
plant p5-a
write_guard p5-a h1 "$work/cases/p5-a/bin/billet"
run_case p5-a escalated BILLET_FAKE_MANAGED_STATUS=exit1 --
expect_refused p5-a "Judge the claim's status" "exited 1"
plant p5-b
write_guard p5-b h1 "$work/cases/p5-b/bin/billet"
run_case p5-b escalated BILLET_FAKE_MANAGED_HANG=1 -- -e billet_guard_timeout=2
expect_refused p5-b "Judge the claim's status" "did not answer within the bound"
p5_case c "Judge the claim's status" "not JSON"
p5_case d "Judge the claim's status" "not a JSON object"
p5_case e "Judge the claim's status" "not a JSON object"
p5_case f "Judge the claim's status" "active is missing"
p5_case g "Judge the claim's status" "active is missing or not a string"
p5_case h "Judge the claim's status" "not a word this role knows"
p5_case i "Judge the claim's status" "disagrees with the directory"
p5_case j "Judge the claim's status" "disagrees with the directory"
p5_case k "Judge the claim's status" "disagrees with the directory"
p5_case l "Refuse a claim that cannot be classified" "the claim is neither a symlink, a file nor a directory" "if one is there, is kept"
p5_case l2 "Judge the claim's status" "why is missing"
p5_case l3 "Judge the claim's status" "why is not a string"
p5_case l4 "Judge the claim's status" "why is not a string"
p5_case l5 "Judge the claim's status" "why is missing or empty"
p5_case m "Judge the claim's status" "guard: missing"
p5_case n "Judge the claim's status" "guard: missing or not an object"
p5_case o "Judge the claim's status" "holder is missing"
p5_case o2 "Judge the claim's status" "holder is missing or not a string"
p5_case p "Judge the claim's status" "holder '' is not a name"
p5_case p2 "Judge the claim's status" "is not a name"
p5_case p3 "Judge the claim's status" "is not a name"
p5_case p4 "Judge the claim's status" "is not a name"
p5_case p5 "Judge the claim's status" "is not a name"
p5_case q "Judge the claim's status" "is not a name"
p5_case q2 "Judge the claim's status" "record_error is not a non-empty string"
p5_case r "Judge the claim's status" "claimed_at is not an RFC 3339 time"
p5_case s "Judge the claim's status" "recovery_pointer is missing"
p5_case t "Judge the claim's status" "recovery_pointer is missing or not a boolean"
p5_case u "Judge the claim's status" "not an absolute path"
p5_case v "Judge the claim's status" "not 64 lowercase hex"
p5_case w "Judge the claim's status" "release_executable_verified is missing"
p5_case x "Judge the claim's status" "neither true, false nor an object"
p5_case x2 "Judge the claim's status" "neither true, false nor an object"
p5_case x3 "Judge the claim's status" "neither true, false nor an object"
p5_case x4 "Judge the claim's status" "neither true, false nor an object"
p5_case y "Refuse a hold that never returned from publishing" "$work/cases/p5-y/bin/billet converge-guard recover --unpublished"
p5_case z "Refuse a guard whose record cannot be read" "the record is not JSON" "The guard is kept" "guard.json by hand"
if grep -q "converge-guard recover" "$work/cases/p5-z/out"; then fail "p5-z: a recover command was named for a record nothing clears"; fi
if grep -q "holder is missing" "$work/cases/p5-z/out"; then fail "p5-z: the healthy schema was asked of a record the command could not read"; fi
p5_case aa-no-pointer "Refuse a guard whose recorded executable does not verify" "the file's digest differs" "The guard is kept" "converge-guard release --holder ci-1"
if grep -q -- "--recover-from" "$work/cases/p5-aa-no-pointer/out"; then fail "p5-aa-no-pointer: a takeover was named for a guard whose binding is bad"; fi
p5_case aa-pointer "Refuse a guard whose recorded executable does not verify" "the file's digest differs" "recover its transaction before the guard can be released"
if grep -q -- "release --holder" "$work/cases/p5-aa-pointer/out"; then fail "p5-aa-pointer: a release was named beside a pointer"; fi
if grep -q -- "--recover-from" "$work/cases/p5-aa-pointer/out"; then fail "p5-aa-pointer: a takeover was named for a guard whose binding is bad"; fi
p5_case ab "Refuse a guard whose recorded executable does not verify" "cannot read the executable" "The guard is kept"
echo "ok   P5: every corrupted status member refuses naming it, and the decision table names the right way out"

# P6. A special file at active; a denied stat is could-not-tell.
plant p6-fifo
mkfifo "$(root_of p6-fifo)/active"
run_case p6-fifo escalated --
expect_refused p6-fifo "Refuse a claim of a type the role does not know" "a FIFO"
log_empty p6-fifo
plant p6-socket
"$python" -c 'import socket,sys; s=socket.socket(socket.AF_UNIX); s.bind(sys.argv[1])' "$(root_of p6-socket)/active"
run_case p6-socket escalated --
expect_refused p6-socket "Refuse a claim of a type the role does not know" "a socket"
plant p6-denied
# The chain above the root is root's (the ancestors judgement runs before the
# claim's stat); only the root's contents are denied to the invoker.
as_root chown root:root "$work/cases/p6-denied" "$work/cases/p6-denied/lib" "$work/cases/p6-denied/lib/billet" "$(root_of p6-denied)"
as_root chmod 0755 "$work/cases/p6-denied" "$work/cases/p6-denied/lib"
as_root chmod 0700 "$(root_of p6-denied)"
run_case p6-denied unescalated --
expect_refused p6-denied "Refuse a claim that could not be examined" "could not be examined" "not one that is absent"
log_empty p6-denied
echo "ok   P6: a FIFO, a socket and a denied stat each refuse without reading or running anything"

# P7. Another holder's guard, two days old, with and without a pointer.
two_days_ago=$("$python" -c 'import time; print(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(time.time()-2*86400)))')
plant p7
write_guard p7 h2 "$work/cases/p7/bin/billet" "$two_days_ago"
run_case p7 escalated --
expect_refused p7 "Refuse a guard another converge holds" "h2 holds this host since $two_days_ago" "(2 days ago)" "$work/cases/p7/bin/billet converge-guard recover --holder h2 --old-driver-stopped" "Nothing expires a guard"
expect_calls p7 managed "converge-guard hold" 0
plant p7-pointer
write_guard p7-pointer h2 "$work/cases/p7-pointer/bin/billet" "$two_days_ago"
mkdir -p "$(root_of p7-pointer)/recovery-20260909T120000-0badcafe"
ln -s "$(root_of p7-pointer)/recovery-20260909T120000-0badcafe" "$(root_of p7-pointer)/active/recovery"
run_case p7-pointer escalated --
expect_refused p7-pointer "Refuse a guard another converge holds" "h2 holds this host" "$work/cases/p7-pointer/bin/billet converge-guard hold --holder h1 --recover-from h2 --old-driver-stopped"
expect_calls p7-pointer managed "converge-guard hold" 0
echo "ok   P7: a foreign guard refuses naming its holder, its age and the right command for its pointer state"

# P8. The same-holder rerun.
plant p8-a
write_guard p8-a h1 "$work/cases/p8-a/bin/billet"
run_case p8-a escalated -- -e billet_gate_inclusions=2
expect_allowed p8-a
expect_calls p8-a managed "converge-guard hold --holder h1" 1
expect_calls p8-a managed "--candidate" 0
expect_calls p8-a suffix "" 0
expect_fact p8-a upgrade False
expect_fact p8-a shape guard
plant p8-b
mkdir -p "$(root_of p8-b)/recovery-20260909T120000-0badcafe"
cp "$work/cases/p8-b/bin/billet" "$(root_of p8-b)/recovery-20260909T120000-0badcafe/billet.candidate"
write_guard p8-b h1 "$(root_of p8-b)/recovery-20260909T120000-0badcafe/billet.candidate"
cp "$work/cases/p8-b/bin/billet" "$work/cases/p8-b/src/billet"
run_case p8-b escalated -- -e billet_binary_src="$work/cases/p8-b/src/billet"
expect_allowed p8-b
expect_calls p8-b managed "converge-guard hold --holder h1" 1
expect_calls p8-b suffix "" 0
expect_fact p8-b upgrade False
plant p8-c
planted=$(root_of p8-c)/recovery-20260909T120000-0badcafe
mkdir -p "$planted/server"
cp "$work/cases/p8-c/src/billet" "$planted/billet.candidate"
echo "host upgrade committed" >"$planted/commit.complete"
echo "version: 2" >"$planted/manifest.yml"
write_guard p8-c h1 "$planted/billet.candidate"
before_candidate=$(stat -c %Y "$planted/billet.candidate")
before_record=$(sha "$(root_of p8-c)/active/guard.json")
run_case p8-c escalated -- -e billet_binary_src="$work/cases/p8-c/src/billet" -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_allowed p8-c
expect_calls p8-c managed "converge-guard hold --holder h1" 1
expect_calls p8-c candidate "converge-guard hold" 0
expect_calls p8-c suffix "" 1
expect_fact p8-c upgrade True
fresh=$(fact p8-c recovery)
[ "$fresh" != "$planted" ] || fail "p8-c: the recorded directory was reused as the journal"
case "$fresh" in "$(root_of p8-c)/recovery-"*) ;; *) fail "p8-c: the fresh journal is not under the root: $fresh" ;; esac
[ "$(sha "$fresh/billet.candidate")" = "$(sha "$work/cases/p8-c/src/billet")" ] || fail "p8-c: the fresh journal's candidate is not this run's"
[ "$(rd stat -c %Y "$planted/billet.candidate")" = "$before_candidate" ] || fail "p8-c: the planted candidate was touched"
[ "$(sha "$(root_of p8-c)/active/guard.json")" = "$before_record" ] || fail "p8-c: the record changed"
for word in commit.complete manifest.yml "$planted/server"; do
  if grep -qF "$word" "$work/cases/p8-c/log"; then fail "p8-c: the planted journal's $word appears in an argv"; fi
done
hold_line=$(calls p8-c | grep -n 'managed: converge-guard hold' | head -1 | cut -d: -f1)
suffix_line=$(calls p8-c | grep -n '^suffix:' | head -1 | cut -d: -f1)
[ "$hold_line" -lt "$suffix_line" ] || fail "p8-c: the validation hold did not precede the allocation" "$work/cases/p8-c/log"
plant p8-d
write_guard p8-d h1 "$work/cases/p8-d/bin/billet"
run_case p8-d escalated -- -e billet_binary_src="$work/cases/p8-d/src/billet"
expect_refused p8-d "Refuse a same-holder rerun with a different intent" "$(sha "$work/cases/p8-d/bin/billet")" "$(sha "$work/cases/p8-d/src/billet")" "converge-guard release --holder h1"
expect_calls p8-d suffix "" 0
plant p8-e
mkdir -p "$(root_of p8-e)/recovery-20260909T120000-0badcafe"
printf '#!/bin/sh\nexit 0\n' >"$(root_of p8-e)/recovery-20260909T120000-0badcafe/billet.candidate"
chmod 0755 "$(root_of p8-e)/recovery-20260909T120000-0badcafe/billet.candidate"
write_guard p8-e h1 "$(root_of p8-e)/recovery-20260909T120000-0badcafe/billet.candidate"
run_case p8-e escalated -- -e billet_binary_src="$work/cases/p8-e/src/billet"
expect_refused p8-e "Refuse a same-holder rerun with a different intent" "converge-guard release --holder h1"
plant p8-f
plant_recorded p8-f
run_case p8-f escalated --
expect_refused p8-f "Refuse a same-holder rerun with a different intent" "intends no binary change" "converge-guard release --holder h1"
plant p8-h
plant_recorded p8-h
cp "$work/cases/p8-h/src/billet" "$work/cases/p8-h/bin/billet"
cp "$fakes/billet-managed" "$work/cases/p8-h/src/other"
run_case p8-h escalated -- -e billet_binary_src="$work/cases/p8-h/src/other"
expect_refused p8-h "Refuse a same-holder rerun with a different intent" "$(sha "$work/cases/p8-h/bin/billet")" "$(sha "$work/cases/p8-h/src/other")" "converge-guard release --holder h1"
plant p8-i
rm -f "$work/cases/p8-i/bin/billet"
plant_recorded p8-i
run_case p8-i escalated -- -e billet_binary_src="$work/cases/p8-i/src/billet"
expect_allowed p8-i
marker_present p8-i
expect_calls p8-i candidate "converge-guard hold --holder h1" 1
expect_calls p8-i candidate "--candidate" 0
expect_fact p8-i upgrade True
[ "$(fact p8-i recovery)" != "$(root_of p8-i)/recovery-20260909T120000-0badcafe" ] || fail "p8-i: the recorded directory was reused"
plant p8-i2
rm -f "$work/cases/p8-i2/bin/billet"
plant_recorded p8-i2
cp "$fakes/billet-managed" "$work/cases/p8-i2/src/other"
run_case p8-i2 escalated -- -e billet_binary_src="$work/cases/p8-i2/src/other"
expect_refused p8-i2 "Refuse a same-holder rerun with a different intent" "converge-guard release --holder h1"
for refusal in "holds a guard.json.tmp beside its record, which a hold does not leave" "the guard directory is mode 0755, want 0700" "flush the guard directory: input/output error"; do
  plant p8-g
  write_guard p8-g h1 "$work/cases/p8-g/bin/billet"
  run_case p8-g escalated BILLET_FAKE_MANAGED_HOLD_REFUSE="converge-guard: $refusal" -- -e billet_binary_src="$work/cases/p8-g/src/billet"
  expect_refused p8-g "Hold this host for the converge" "$refusal"
  expect_calls p8-g suffix "" 0
  expect_no_task p8-g "Allocate a recovery directory exclusively"
done
echo "ok   P8: a same-holder rerun validates, keeps its record, refuses a different intent and never reuses a journal"

# P9. This holder's guard with a pointer: the interrupted transaction.
plant p9
write_guard p9 h1 "$work/cases/p9/bin/billet"
mkdir -p "$(root_of p9)/recovery-20260909T120000-0badcafe"
ln -s "$(root_of p9)/recovery-20260909T120000-0badcafe" "$(root_of p9)/active/recovery"
run_case p9 escalated -- -e billet_gate_inclusions=2 -e billet_binary_src="$work/cases/p9/src/billet"
expect_allowed p9
expect_fact p9 shape guard-pointer
expect_fact p9 interrupted True
expect_fact p9 recovery "$(root_of p9)/recovery-20260909T120000-0badcafe"
expect_calls p9 managed "converge-guard hold --holder h1" 1
expect_calls p9 managed "--candidate" 0
expect_calls p9 suffix "" 0
expect_no_task p9 "Stage the immutable candidate binary inside its recovery journal"
for refusal in "the guard directory is owned by uid 1000, want 0" "holds a guard.json.tmp beside its record" "flush the guard directory: input/output error"; do
  plant p9-refused
  write_guard p9-refused h1 "$work/cases/p9-refused/bin/billet"
  mkdir -p "$(root_of p9-refused)/recovery-20260909T120000-0badcafe"
  ln -s "$(root_of p9-refused)/recovery-20260909T120000-0badcafe" "$(root_of p9-refused)/active/recovery"
  run_case p9-refused escalated BILLET_FAKE_MANAGED_HOLD_REFUSE="converge-guard: $refusal" --
  expect_refused p9-refused "Hold this host for the converge" "$refusal"
  expect_no_task p9-refused "Inspect the transaction claim before recovery"
done
echo "ok   P9: an interrupted transaction is classified by its pointer, validated once and staged over never"

# P10. The pointer's target outside both grammars, dangling, a file, a directory.
p10_case() { # id plant-command fragment
  plant "p10-$1"
  write_guard "p10-$1" h1 "$work/cases/p10-$1/bin/billet"
  (cd "$(root_of "p10-$1")" && eval "$2")
  run_case "p10-$1" escalated --
  expect_refused "p10-$1" "Refuse a transaction pointer outside the recovery grammar" "$3"
}
p10_case grammar 'mkdir notes; ln -s "$PWD/notes" active/recovery' "not a recovery directory this role allocates"
p10_case dangling 'ln -s "$PWD/recovery-20260909T120000-0badcafe" active/recovery' "does not exist"
p10_case file 'touch active/recovery' "a regular file, not the symlink pointer"
p10_case dir 'mkdir active/recovery' "a directory, not the symlink pointer"
echo "ok   P10: a pointer is trusted by grammar, existence and type"

# P11. A guard with a pointer and no managed binary: the same holder continues
# through the recorded candidate; another holder is refused naming the takeover.
plant p11
rm -f "$work/cases/p11/bin/billet"
plant_recorded p11
ln -s "$(root_of p11)/recovery-20260909T120000-0badcafe" "$(root_of p11)/active/recovery"
run_case p11 escalated --
expect_allowed p11
marker_present p11
expect_fact p11 shape guard-pointer
expect_calls p11 candidate "converge-guard status --json" 1
expect_calls p11 candidate "converge-guard hold --holder h1" 1
plant p11-h3
rm -f "$work/cases/p11-h3/bin/billet"
plant_recorded p11-h3
ln -s "$(root_of p11-h3)/recovery-20260909T120000-0badcafe" "$(root_of p11-h3)/active/recovery"
HOLDER=h3; run_case p11-h3 escalated --; HOLDER=h1
expect_refused p11-h3 "Refuse a guard another converge holds" "$(root_of p11-h3)/recovery-20260909T120000-0badcafe/billet.candidate converge-guard hold --holder h3 --recover-from h1 --old-driver-stopped"
echo "ok   P11: a guarded interrupted bootstrap continues for its holder and names the takeover for another"

# P12. The verified fallback: a pre-R managed binary and a guard naming the candidate.
plant p12
plant_recorded p12
run_case p12 escalated BILLET_FAKE_MANAGED_PRE_R=1 -- -e billet_binary_src="$work/cases/p12/src/billet"
expect_allowed p12
marker_present p12
[ "$(calls p12 | grep 'converge-guard status' | head -2 | awk '{print $1}' | tr '\n' ' ')" = "managed: candidate: " ] || fail "p12: the managed binary's failed status was not followed by the candidate's" "$work/cases/p12/log"
grep -q "answered by the executable it records" "$work/cases/p12/out" || fail "p12: the report does not name the fallback"
expect_calls p12 candidate "converge-guard hold --holder h1" 1
expect_calls p12 managed "converge-guard hold" 0
echo "ok   P12: a pre-R managed binary defers to the verified recorded candidate, which every later guard operation runs"

# P13. The fallback reader's phases, through tasks_from: guard-fallback over an
# otherwise-valid tree (the component × property table is the module's own
# check, guard_fallback_check.py, run above).
p13_args() {
  printf -- '-e billet_gate_entry=guard-fallback -e billet_exclusion_root=%s -e billet_exclusion_become=false -e billet_exclusion_owner_uid=0 -e billet_exclusion_binary_present=false -e billet_exclusion_binary=%s -e billet_exclusion_darwin=false' \
    "$(root_of "$1")" "$work/cases/$1/bin/billet"
}
p13_plant() { plant "$1"; rm -f "$work/cases/$1/bin/billet"; plant_recorded "$1"; }
p13_plant p13-pass
# shellcheck disable=SC2046
run_case p13-pass escalated -- $(p13_args p13-pass)
expect_allowed p13-pass
marker_present p13-pass
p13_plant p13-record-mode
chmod 0644 "$(root_of p13-record-mode)/active/guard.json"
# shellcheck disable=SC2046
run_case p13-record-mode escalated -- $(p13_args p13-record-mode)
expect_refused p13-record-mode "Judge the guard's record" "want 0600"
marker_absent p13-record-mode
expect_no_task p13-record-mode "Judge the recorded executable"
p13_plant p13-record-content
echo "not json" >"$(root_of p13-record-content)/active/guard.json"
# shellcheck disable=SC2046
run_case p13-record-content escalated -- $(p13_args p13-record-content)
expect_refused p13-record-content "Judge the guard's record" "is not JSON"
marker_absent p13-record-content
p13_plant p13-candidate-digest
printf '\n' >>"$(root_of p13-candidate-digest)/recovery-20260909T120000-0badcafe/billet.candidate"
# shellcheck disable=SC2046
run_case p13-candidate-digest escalated -- $(p13_args p13-candidate-digest)
expect_refused p13-candidate-digest "Judge the recorded executable" "has digest"
marker_absent p13-candidate-digest
p13_plant p13-candidate-writable
chmod 0775 "$(root_of p13-candidate-writable)/recovery-20260909T120000-0badcafe"
# shellcheck disable=SC2046
run_case p13-candidate-writable escalated -- $(p13_args p13-candidate-writable)
expect_refused p13-candidate-writable "Judge the recorded executable" "writable by its group"
marker_absent p13-candidate-writable
p13_plant p13-hardlinks
ln "$(root_of p13-hardlinks)/recovery-20260909T120000-0badcafe/billet.candidate" "$(root_of p13-hardlinks)/recovery-20260909T120000-0badcafe/second-name"
ln "$(root_of p13-hardlinks)/active/guard.json" "$(root_of p13-hardlinks)/active/record-second-name"
# shellcheck disable=SC2046
run_case p13-hardlinks escalated -- $(p13_args p13-hardlinks)
expect_allowed p13-hardlinks
p13_plant p13-unpublished
rm -f "$(root_of p13-unpublished)/active/guard.json"
# shellcheck disable=SC2046
run_case p13-unpublished escalated -- $(p13_args p13-unpublished)
expect_refused p13-unpublished "Refuse an unpublished guard no executable can recover" "Install a billet at or after the release that carries the converge guard at $work/cases/p13-unpublished/bin/billet" "converge-guard recover --unpublished"
marker_absent p13-unpublished
echo "ok   P13: the fallback's refusals come from the phase that judges them, hard links are admitted, and a refused candidate is never run"

# P14. Check mode: status only, nothing created; a foreign guard reported and
# the preparation continues; a legacy pointer reported.
plant p14-absent
HOLDER=""; run_case p14-absent escalated -- --check; HOLDER=h1
expect_allowed p14-absent
expect_calls p14-absent managed "converge-guard status --json" 1
expect_calls p14-absent managed "converge-guard hold" 0
if rd test -e "$(root_of p14-absent)/active"; then fail "p14-absent: a dry run created the claim"; fi
plant p14-foreign
write_guard p14-foreign h2 "$work/cases/p14-foreign/bin/billet"
HOLDER=""; run_case p14-foreign escalated -- --check; HOLDER=h1
expect_allowed p14-foreign
grep -q "a guard held by h2" "$work/cases/p14-foreign/out" || fail "p14-foreign: the dry run did not report the foreign guard"
expect_calls p14-foreign managed "converge-guard hold" 0
plant p14-legacy
printf '%s\n' "$(root_of p14-legacy)/20260909T120000000000000" >"$(root_of p14-legacy)/active"
HOLDER=""; run_case p14-legacy escalated -- --check; HOLDER=h1
expect_allowed p14-legacy
expect_fact p14-legacy shape legacy-file
echo "ok   P14: a dry run asks, reports and holds nothing"

# P15. Darwin (fake facts), unescalated: the launch agent's account owns the tree.
me=$(id -u)
plant p15-a
rm -rf "$work/cases/p15-a/lib" "$work/cases/p15-a/bin/billet"
run_case p15-a unescalated -- -e billet_exclusion_platform=Darwin
expect_allowed p15-a
grep -q "nothing to hold" "$work/cases/p15-a/out" || fail "p15-a: a pristine Mac was not reported as nothing to hold"
log_empty p15-a
plant p15-b
rm -f "$work/cases/p15-b/bin/billet"
plant_recorded p15-b
ln -s "$(root_of p15-b)/recovery-20260909T120000-0badcafe" "$(root_of p15-b)/active/recovery"
run_case p15-b unescalated -- -e billet_exclusion_platform=Darwin
expect_allowed p15-b
expect_fact p15-b shape guard-pointer
marker_present p15-b
grep -q "^uid=$me$" "$work/cases/p15-b/log" || fail "p15-b: the candidate did not run as the agent's account"
plant p15-b2
rm -f "$work/cases/p15-b2/bin/billet"
plant_recorded p15-b2
ln -s "$(root_of p15-b2)/recovery-20260909T120000-0badcafe" "$(root_of p15-b2)/active/recovery"
as_root chown root "$(root_of p15-b2)/active/guard.json"
run_case p15-b2 unescalated -- -e billet_exclusion_platform=Darwin
expect_refused p15-b2 "Judge the guard's record" "is owned by uid 0, want $me"
marker_absent p15-b2
plant p15-c
rm -f "$work/cases/p15-c/bin/billet"
mkdir -p "$(root_of p15-c)/active"
chmod 0700 "$(root_of p15-c)/active"
run_case p15-c unescalated -- -e billet_exclusion_platform=Darwin
expect_refused p15-c "Refuse an unpublished guard no executable can recover" "Install a billet at or after the release that carries the converge guard at $work/cases/p15-c/bin/billet" "converge-guard recover --unpublished"
plant p15-d
run_case p15-d unescalated -- -e billet_exclusion_platform=Darwin
expect_allowed p15-d
expect_calls p15-d managed "converge-guard hold --holder h1" 1
grep -q "^uid=$me$" "$work/cases/p15-d/log" || fail "p15-d: the hold did not run as the agent's account"
expect_calls p15-d timeout "" 0
grep -q "ASYNC" "$work/cases/p15-d/out" || fail "p15-d: the darwin hold did not run under the task's async bound" "$work/cases/p15-d/out"
plant p15-e
run_case p15-e unescalated BILLET_FAKE_MANAGED_HANG=1 -- -e billet_exclusion_platform=Darwin -e billet_guard_timeout=1
expect_refused p15-e "Judge the claim's status" "did not answer within the bound"
plant p15-e2
write_guard p15-e2 h1 "$work/cases/p15-e2/bin/billet"
run_case p15-e2 unescalated BILLET_FAKE_MANAGED_HOLD_HANG=1 -- -e billet_exclusion_platform=Darwin -e billet_guard_timeout=1
[ "$status" -ne 0 ] || fail "p15-e2: a hung darwin hold was not bounded" "$work/cases/p15-e2/out"
[ "$(failed_at p15-e2)" = "Hold this host for the converge" ] || fail "p15-e2: the hung hold failed elsewhere: $(failed_at p15-e2)" "$work/cases/p15-e2/out"
plant p15-f
run_case p15-f unescalated -- -e billet_exclusion_platform=Darwin -e billet_binary_src="$work/cases/p15-f/src/billet" -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_allowed p15-f
expect_calls p15-f suffix "" 0
expect_calls p15-f candidate "" 0
marker_absent p15-f
expect_calls p15-f managed "converge-guard hold --holder h1" 1
expect_calls p15-f managed "--candidate" 0
plant p15-g
run_case p15-g unescalated -- -e billet_exclusion_platform=Darwin -e billet_gate_inclusions=2 -e "$(json_var billet_gate_between "rm -rf $work/cases/p15-g/lib $work/cases/p15-g/bin/billet")"
expect_refused p15-g "Refuse a converge whose exclusion moved" "as h1" "absent"
expect_calls p15-g managed "converge-guard hold" 1
echo "ok   P15: a Mac classifies and holds as the agent's account, bounded, stages nothing, and a pristine Mac holds nothing unless this run already held it"

# P17. The root established on a fresh Linux host; an unsafe existing root refuses.
plant p17-fresh
rm -rf "$work/cases/p17-fresh/lib" "$work/cases/p17-fresh/bin/billet"
run_case p17-fresh escalated -- -e billet_binary_src="$work/cases/p17-fresh/src/billet"
expect_allowed p17-fresh
[ "$(rd stat -c %a:%u "$work/cases/p17-fresh/lib/billet")" = "755:0" ] || fail "p17-fresh: the parent is not 0755 root"
[ "$(rd stat -c %a:%u "$(root_of p17-fresh)")" = "700:0" ] || fail "p17-fresh: the root is not 0700 root"
expect_calls p17-fresh candidate "converge-guard hold --holder h1 --candidate" 1
expect_task p17-fresh "Flush the upgrade root's parent"
p17_unsafe() { # id plant-command fragment
  plant "p17-$1"
  as_root chown -R root:root "$work/cases/p17-$1/lib"
  (cd "$work/cases/p17-$1/lib/billet" && eval "$2")
  KEEP_OWNER=1; run_case "p17-$1" escalated -- -e billet_binary_src="$work/cases/p17-$1/src/billet"; KEEP_OWNER=0
  expect_refused "p17-$1" "Refuse an upgrade root that is not what the role makes" "$3"
  expect_calls "p17-$1" suffix "" 0
}
p17_unsafe symlink 'sudo -n rmdir upgrades; sudo -n mkdir elsewhere; sudo -n ln -s elsewhere upgrades' "a symlink"
p17_unsafe file 'sudo -n rmdir upgrades; sudo -n touch upgrades' "not a directory"
p17_unsafe owner 'sudo -n chown 1000 upgrades' "owned by uid 1000"
p17_unsafe group-writable 'sudo -n chmod 0770 upgrades' "writable by its group or by others"
# THE CHAIN ABOVE THE ROOT: an ancestor another account can rename refuses
# before anything is allocated, staged, executed or held.
plant p17-ancestor
CASE_DIR_MODE=0775; run_case p17-ancestor escalated -- -e billet_binary_src="$work/cases/p17-ancestor/src/billet" -e billet_recovery_dir_suffix_command="$fakes/suffix"; CASE_DIR_MODE=0755
expect_refused p17-ancestor "Refuse an upgrade root whose ancestors another account can rename" "writable by group or others without the sticky bit"
expect_calls p17-ancestor suffix "" 0
marker_absent p17-ancestor
log_empty p17-ancestor
plant p17-ancestor-fresh
rm -rf "$work/cases/p17-ancestor-fresh/lib"
CASE_DIR_MODE=0775; run_case p17-ancestor-fresh escalated -- -e billet_binary_src="$work/cases/p17-ancestor-fresh/src/billet" -e billet_recovery_dir_suffix_command="$fakes/suffix"; CASE_DIR_MODE=0755
expect_refused p17-ancestor-fresh "Refuse an upgrade root whose ancestors another account can rename" "writable by group or others without the sticky bit"
[ ! -e "$work/cases/p17-ancestor-fresh/lib" ] || fail "p17-ancestor-fresh: the root's parent was made under an unsafe ancestor"
expect_no_task p17-ancestor-fresh "Establish the upgrade root's parent"
log_empty p17-ancestor-fresh
plant p17-module-error
run_case p17-module-error escalated -- -e billet_upgrade_root=relative/upgrades -e billet_binary_src="$work/cases/p17-module-error/src/billet"
expect_refused p17-module-error "Refuse an upgrade root whose ancestors another account can rename" "not an absolute path"
log_empty p17-module-error
plant p17-ancestor-sticky
rm -rf "$work/cases/p17-ancestor-sticky/lib"
CASE_DIR_MODE=1777; run_case p17-ancestor-sticky escalated -- -e billet_binary_src="$work/cases/p17-ancestor-sticky/src/billet" -e billet_recovery_dir_suffix_command="$fakes/suffix"; CASE_DIR_MODE=0755
expect_refused p17-ancestor-sticky "Refuse an upgrade root whose ancestors another account can rename" "is absent under" "writable by group or others, so another account could create it"
[ ! -e "$work/cases/p17-ancestor-sticky/lib" ] || fail "p17-ancestor-sticky: the root's parent was made under a sticky world-writable ancestor"
expect_no_task p17-ancestor-sticky "Establish the upgrade root's parent"
log_empty p17-ancestor-sticky
echo "ok   P17: the root is established 0755/0700 root on a fresh host, an unsafe one refuses before any allocation, and so does an unsafe ancestor, before the parent is made, an absent parent under a sticky directory, and a judgement that did not complete"

# P18. THE MANAGED BINARY APPEARS between the preparation's inspection and the
# claim's: an updater that finished and released its claim in that window.
# The preparation saw no binary and asked nothing for the guard's status; the
# staging sees one and refuses rather than converging unheld beside it.
plant p18-appeared
mv "$work/cases/p18-appeared/bin/billet" "$work/cases/p18-appeared/bin/billet.aside"
rm -rf "$(root_of p18-appeared)"
run_case p18-appeared escalated BILLET_FAKE_SYNC_RESTORE="$work/cases/p18-appeared/bin/billet.aside" BILLET_FAKE_SYNC_RESTORE_TO="$work/cases/p18-appeared/bin/billet" --
expect_refused p18-appeared "Refuse a converge whose managed binary appeared or vanished before the staging" "appeared between the preparation's inspection and this staging"
expect_task p18-appeared "Flush the upgrade root's parent"
[ -e "$work/cases/p18-appeared/bin/billet" ] || fail "p18-appeared: the binary was never put back, so the case staged nothing"
! grep -q 'converge-guard' "$work/cases/p18-appeared/log" || fail "p18-appeared: the managed binary was asked or held" "$work/cases/p18-appeared/log"
expect_no_task p18-appeared "Hold this host for the converge"
marker_absent p18-appeared
echo "ok   P18: a managed binary that appears between the preparation's inspection and the claim's refuses the converge before anything is staged or held"

# =============================================================================
# S. The staging.
# =============================================================================

# A fake release origin for the pinned and channel cases: the candidate in a
# release-shaped archive with its checksums.txt, and a channel statement.
origin=$work/origin
mkdir -p "$origin/v0.10.0"
arch=$(uname -m); case $arch in x86_64) rel_arch=amd64 ;; aarch64|arm64) rel_arch=arm64 ;; *) rel_arch=$arch ;; esac
(cd "$fakes" && cp billet-candidate billet && tar -czf "$origin/v0.10.0/billet_0.10.0_linux_${rel_arch}.tar.gz" billet && rm -f billet)
(cd "$origin/v0.10.0" && sha256sum "billet_0.10.0_linux_${rel_arch}.tar.gz" >checksums.txt)
printf '{"tag": "v0.10.0"}\n' >"$origin/stable.json"
printf '{"tag": "latest"}\n' >"$origin/moving.json"
"$python" -u -m http.server --bind 127.0.0.1 --directory "$origin" 0 >"$work/origin.log" 2>&1 &
origin_pid=$!
cleanup() { kill "$origin_pid" 2>/dev/null || true; rd rm -rf "$work"; }
port=""
for _ in $(seq 1 100); do
  port=$(sed -n 's/.*port \([0-9]*\).*/\1/p' "$work/origin.log" | head -1)
  [ -n "$port" ] && break
  sleep 0.1
done
[ -n "$port" ] || fail "the fake release origin did not start" "$work/origin.log"
origin_url="http://127.0.0.1:$port"

# S1. A pin with a differing digest: staged, asked as the staged copy, held through it, in order.
plant s1
run_case s1 escalated -- -e billet_gate_facts=true -e billet_version=v0.10.0 -e billet_release_url_base="$origin_url" -e billet_release_stage="$work/cases/s1/stage" -e billet_fetch_retries=1 -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_allowed s1
expect_fact s1 upgrade True
journal=$(fact s1 recovery)
[ "$(rd stat -c %a:%u "$journal")" = "700:0" ] || fail "s1: the recovery directory is not 0700 root: $journal"
[ "$(sha "$journal/billet.candidate")" = "$(sha "$fakes/billet-candidate")" ] || fail "s1: the staged candidate is not the release's"
expect_calls s1 candidate "version @$journal/billet.candidate" 1
expect_calls s1 candidate "converge-guard status --json @$journal/billet.candidate" 1
expect_calls s1 candidate "converge-guard hold --holder h1 --candidate $journal/billet.candidate @$journal/billet.candidate" 1
order=$(calls s1 | grep -v '^timeout\|^mkdir\|^date' | awk -F'[: ]' '{print $1 ":" $3}' | tr '\n' ' ')
[ "$order" = "managed:converge-guard suffix: managed:version candidate:version candidate:converge-guard candidate:converge-guard managed:version " ] || fail "s1: the order is not status, allocation, versions, capability, hold, the release re-read: $order" "$work/cases/s1/log"
expect_calls s1 systemctl "" 0
echo "ok   S1: a pinned release is staged into an exclusive journal and asked, proved and held as the staged copy, in order"

# S2. A candidate whose digest equals the managed binary's changes nothing.
plant s2
cp "$work/cases/s2/bin/billet" "$work/cases/s2/src/billet"
run_case s2 escalated -- -e billet_binary_src="$work/cases/s2/src/billet" -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_allowed s2
expect_fact s2 upgrade False
expect_calls s2 suffix "" 0
expect_calls s2 managed "converge-guard hold --holder h1" 1
expect_calls s2 managed "--candidate" 0
echo "ok   S2: an unchanged binary stages nothing and holds through the managed binary"

# S3. The allocator, driven directly.
s3_run() { # case [env...] -- [extra args]
  local name=$1; shift
  local envs=()
  while [ "$1" != -- ]; do envs+=("$1"); shift; done
  shift
  run_case "$name" escalated "${envs[@]+"${envs[@]}"}" -- -e billet_gate_entry=allocate-recovery -e billet_exclusion_root="$(root_of "$name")" -e billet_exclusion_become=false "$@"
}
plant s3-a0
s3_run s3-a0 --
expect_allowed s3-a0
root_ls s3-a0 | grep -Eq '^recovery-[0-9]{8}T[0-9]{6}-[0-9a-f]{8}$' || fail "s3-a0: the default generator did not produce one recovery directory: $(root_ls s3-a0)"
[ "$(root_ls s3-a0 | wc -l)" -eq 1 ] || fail "s3-a0: more than one directory was allocated"
plant s3-a
s3_run s3-a BILLET_FAKE_DATE_MODE=frozen -- -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_allowed s3-a
expect_calls s3-a date "-u +%Y%m%dT%H%M%S" 1
expect_calls s3-a suffix "" 1
root_ls s3-a | grep -Eq '^recovery-20260909T120000-[0-9a-f]{8}$' || fail "s3-a: no directory under the frozen stamp: $(root_ls s3-a)"
plant s3-a1-fail
s3_run s3-a1-fail BILLET_FAKE_DATE_MODE=fail -- -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_refused s3-a1-fail "Allocate a recovery directory exclusively" "the stamp could not be drawn"
expect_calls s3-a1-fail mkdir "" 0
plant s3-a1-short
s3_run s3-a1-short BILLET_FAKE_DATE_MODE=short -- -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_refused s3-a1-short "Allocate a recovery directory exclusively" "is not <YYYYMMDD>T<HHMMSS>"
expect_calls s3-a1-short mkdir "" 0
# (b) two stagers through stage-candidate.yml, one draw sequence shared.
plant s3-b
printf '0badcafe\n0badcafe\n1badcafe\n' >"$work/cases/s3-b/suffixes"
{ cat "$fakes/billet-candidate"; echo "# candidate B"; } >"$work/cases/s3-b/src/other"; chmod 0755 "$work/cases/s3-b/src/other"
# THE BOOLEANS AS JSON: a `-e name=false` is the string "false", which Jinja
# reads as true in `not billet_interrupted_upgrade`.
s3b_json='{"billet_exclusion_become": false, "billet_exclusion_darwin": false, "billet_interrupted_upgrade": false, "billet_exclusion_managed_pre_r": false, "billet_exclusion_binary_present": true, "billet_upgrade_claim_shape": "none", "billet_resolved_version": ""}'
s3b_args="-e billet_gate_entry=stage-candidate -e billet_exclusion_root=$(root_of s3-b) -e billet_exclusion_binary=$work/cases/s3-b/bin/billet -e billet_recovery_dir_suffix_command=$fakes/suffix"
# shellcheck disable=SC2086
run_case s3-b escalated BILLET_FAKE_DATE_MODE=frozen BILLET_FAKE_SUFFIXES="$work/cases/s3-b/suffixes" -- $s3b_args -e "$s3b_json" -e billet_binary_src="$work/cases/s3-b/src/billet"
expect_allowed s3-b
first=$(root_of s3-b)/recovery-20260909T120000-0badcafe
[ "$(fact s3-b recovery)" = "$first" ] || fail "s3-b: the first stager did not land in 0badcafe: $(fact s3-b recovery)" "$work/cases/s3-b/out"
[ "$(sha "$first/billet.candidate")" = "$(sha "$work/cases/s3-b/src/billet")" ] || fail "s3-b: the first stager's candidate is not A"
first_mtime=$(rd stat -c %Y "$first/billet.candidate")
rd mv "$work/cases/s3-b/log" "$work/cases/s3-b/log.first"
# shellcheck disable=SC2086
run_case s3-b escalated BILLET_FAKE_DATE_MODE=frozen BILLET_FAKE_SUFFIXES="$work/cases/s3-b/suffixes" -- $s3b_args -e "$s3b_json" -e billet_binary_src="$work/cases/s3-b/src/other"
expect_allowed s3-b
second=$(root_of s3-b)/recovery-20260909T120000-1badcafe
[ "$(fact s3-b recovery)" = "$second" ] || fail "s3-b: the second stager did not land in 1badcafe: $(fact s3-b recovery)" "$work/cases/s3-b/out"
[ "$(sha "$second/billet.candidate")" = "$(sha "$work/cases/s3-b/src/other")" ] || fail "s3-b: the second stager's candidate is not B"
expect_calls s3-b suffix "" 2
expect_calls s3-b mkdir "-m 0700 $first" 1
expect_calls s3-b mkdir "-m 0700 $second" 1
[ "$(sha "$first/billet.candidate")" = "$(sha "$work/cases/s3-b/src/billet")" ] || fail "s3-b: the loser's write changed the winner's candidate"
[ "$(rd stat -c %Y "$first/billet.candidate")" = "$first_mtime" ] || fail "s3-b: the winner's candidate was touched by the loser"
[ ! -s "$work/cases/s3-b/suffixes" ] || fail "s3-b: the draw sequence was not consumed: $(cat "$work/cases/s3-b/suffixes")"
# (c) a dangling symlink at the first name is a collision.
plant s3-c
printf '0badcafe\n1badcafe\n' >"$work/cases/s3-c/suffixes"
ln -s "$work/cases/s3-c/nowhere" "$(root_of s3-c)/recovery-20260909T120000-0badcafe"
s3_run s3-c BILLET_FAKE_DATE_MODE=frozen BILLET_FAKE_SUFFIXES="$work/cases/s3-c/suffixes" -- -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_allowed s3-c
rd test -d "$(root_of s3-c)/recovery-20260909T120000-1badcafe" || fail "s3-c: a dangling symlink was not treated as a collision"
expect_calls s3-c suffix "" 2
# (d) a generator's bad output.
for bad in "0badcaf" "0BADCAFE" ""; do
  plant s3-d
  printf '%s\n' "$bad" >"$work/cases/s3-d/suffixes"
  s3_run s3-d BILLET_FAKE_DATE_MODE=frozen BILLET_FAKE_SUFFIXES="$work/cases/s3-d/suffixes" -- -e billet_recovery_dir_suffix_command="$fakes/suffix"
  expect_refused s3-d "Allocate a recovery directory exclusively" "not eight lowercase hex digits"
  expect_calls s3-d mkdir "" 0
done
# (e) a generator exiting non-zero while printing valid digits.
plant s3-e
printf '0badcafe\n' >"$work/cases/s3-e/suffixes"
s3_run s3-e BILLET_FAKE_DATE_MODE=frozen BILLET_FAKE_SUFFIXES="$work/cases/s3-e/suffixes" BILLET_FAKE_SUFFIX_STATUS=1 -- -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_refused s3-e "Allocate a recovery directory exclusively" "the suffix generator exited non-zero"
expect_calls s3-e mkdir "" 0
# (f) five collisions exhaust.
plant s3-f
mkdir "$(root_of s3-f)/recovery-20260909T120000-0badcafe"
printf '0badcafe\n0badcafe\n0badcafe\n0badcafe\n0badcafe\n0badcafe\n' >"$work/cases/s3-f/suffixes"
s3_run s3-f BILLET_FAKE_DATE_MODE=frozen BILLET_FAKE_SUFFIXES="$work/cases/s3-f/suffixes" -- -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_refused s3-f "Allocate a recovery directory exclusively" "five names collided" "exhausted"
expect_calls s3-f mkdir "" 5
# (g) a non-collision failure: the root unwritable, as the invoking user.
plant s3-g
as_root chown root:root "$work/cases/s3-g/lib/billet" "$(root_of s3-g)"
as_root chmod 0555 "$(root_of s3-g)"
run_case s3-g unescalated BILLET_FAKE_DATE_MODE=frozen -- -e billet_gate_entry=allocate-recovery -e billet_exclusion_root="$(root_of s3-g)" -e billet_exclusion_become=false -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_refused s3-g "Allocate a recovery directory exclusively" "attempt 1" "the name is absent" "Permission denied"
expect_calls s3-g mkdir "" 1
echo "ok   S3: the allocator draws inside its loop, reads a collision by the name's presence and refuses everything else at once"

# S4. The floor and the staging failures.
plant s4-a
run_case s4-a escalated BILLET_FAKE_CANDIDATE_PRE_R=1 -- -e billet_binary_src="$work/cases/s4-a/src/billet"
expect_refused s4-a "Judge the candidate's capability" "predates the converge guard" "legacy collection"
expect_calls s4-a candidate "converge-guard hold" 0
expect_calls s4-a managed "converge-guard hold" 0
root_ls s4-a | grep -q '^recovery-' || fail "s4-a: the staged directory was not retained"
plant s4-b
run_case s4-b escalated BILLET_FAKE_CANDIDATE_HANG=1 -- -e billet_binary_src="$work/cases/s4-b/src/billet" -e billet_guard_timeout=2
expect_refused s4-b "Judge the candidate's capability" "could not be determined" "did not answer within the bound"
expect_calls s4-b candidate "converge-guard hold" 0
plant s4-c
echo "not json" >"$work/cases/s4-c/status.json"
run_case s4-c escalated BILLET_FAKE_CANDIDATE_STATUS="$work/cases/s4-c/status.json" -- -e billet_binary_src="$work/cases/s4-c/src/billet"
expect_refused s4-c "Judge the candidate's capability" "could not be determined" "not JSON"
for id in d e f g h l2 l3 l4 l5 m n o o2 p p2 p3 p4 p5 q q2 r s t u v w x x2 x3 x4; do
  plant "s4-d-$id"
  mkstatus "$work/cases/s4-d-$id/status.json" "$id"
  run_case "s4-d-$id" escalated BILLET_FAKE_CANDIDATE_STATUS="$work/cases/s4-d-$id/status.json" -- -e billet_binary_src="$work/cases/s4-d-$id/src/billet"
  expect_refused "s4-d-$id" "Judge the candidate's capability" "could not be determined"
  expect_calls "s4-d-$id" candidate "converge-guard hold" 0
  root_ls "s4-d-$id" | grep -q '^recovery-' || fail "s4-d-$id: the staged directory was not retained"
done
plant s4-e
run_case s4-e escalated BILLET_FAKE_CANDIDATE_VERSION=v0.9.0 -- -e billet_binary_src="$work/cases/s4-e/src/billet" -e billet_allow_downgrade=true
expect_allowed s4-e
expect_calls s4-e candidate "converge-guard hold --holder h1 --candidate" 1
plant s4-f
run_case s4-f escalated -- -e billet_gate_facts=true -e billet_version=v0.10.0 -e billet_release_url_base=http://127.0.0.1:1 -e billet_release_stage="$work/cases/s4-f/stage" -e billet_fetch_retries=1 -e billet_fetch_timeout=2 -e billet_fetch_retry_delay=0 -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_refused s4-f "Download the pinned billet release"
expect_calls s4-f suffix "" 0
expect_calls s4-f managed "converge-guard hold" 0
expect_no_task s4-f "Configure billet account and files"
echo "ok   S4: a pre-R candidate, an unreadable capability and a failed fetch each end the play before the hold"

# S5. A pre-R managed binary and no candidate: the legacy protocol; PostgreSQL refuses.
plant s5
run_case s5 escalated BILLET_FAKE_MANAGED_PRE_R=1 --
expect_allowed s5
expect_fact s5 shape legacy
grep -q "runs the legacy protocol" "$work/cases/s5/out" || fail "s5: the legacy protocol was not warned about"
expect_calls s5 managed "converge-guard hold" 0
plant s5-pg
run_case s5-pg escalated BILLET_FAKE_MANAGED_PRE_R=1 -- -e '{"billet_config": {"server": {"state": {"backend": "postgres"}}}}'
expect_refused s5-pg "Refuse a PostgreSQL controller whose binary predates the guard" "predates the converge guard" "billet host-upgrade"
plant s5-pg-bootstrap
rm -f "$work/cases/s5-pg-bootstrap/bin/billet"
run_case s5-pg-bootstrap escalated -- -e '{"billet_config": {"server": {"state": {"backend": "postgres"}}}}' -e billet_binary_src="$work/cases/s5-pg-bootstrap/src/billet" -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_refused s5-pg-bootstrap "Refuse a transactional binary change against an external ledger" "at or after the release that carries the converge guard"
expect_calls s5-pg-bootstrap suffix "" 0
echo "ok   S5: a pre-R binary runs the legacy protocol with a warning, and a PostgreSQL controller refuses instead"

# S6. Bootstrap eligibility.
plant s6
rm -f "$work/cases/s6/bin/billet"
touch "$(root_of s6)/transaction.lock"
mkdir "$(root_of s6)/20260908T120000000000000"
run_case s6 escalated -- -e billet_binary_src="$work/cases/s6/src/billet"
expect_allowed s6
expect_calls s6 candidate "converge-guard hold --holder h1 --candidate" 1
plant s6-stray
rm -f "$work/cases/s6-stray/bin/billet"
touch "$(root_of s6-stray)/notes"
run_case s6-stray escalated -- -e billet_binary_src="$work/cases/s6-stray/src/billet" -e billet_recovery_dir_suffix_command="$fakes/suffix"
expect_refused s6-stray "Refuse a bootstrap over a root that holds something the role did not write" "notes"
expect_calls s6-stray suffix "" 0
echo "ok   S6: a bootstrap admits the role's own entries and refuses a stray"

# S7. A channel writes the resolved version and leaves the pin empty.
plant s7
run_case s7 escalated -- -e billet_gate_facts=true -e billet_release_channel=stable -e billet_release_channel_base="$origin_url" -e billet_release_url_base="$origin_url" -e billet_release_stage="$work/cases/s7/stage" -e billet_fetch_retries=1
expect_allowed s7
[ "$(fact s7 version)" != "v0.10.0" ] || fail "s7: the public billet_version was assigned the resolved tag" "$work/cases/s7/out"
expect_fact s7 resolved v0.10.0
plant s7-moving
run_case s7-moving escalated -- -e billet_gate_facts=true -e billet_release_channel=moving -e billet_release_channel_base="$origin_url" -e billet_release_url_base="$origin_url" -e billet_release_stage="$work/cases/s7-moving/stage" -e billet_fetch_retries=1
expect_refused s7-moving "Refuse an unpinned billet version" "not latest"
echo "ok   S7: a channel resolves into the private variable and the public pin is never assigned"

# S8. A controller-side source: the version and the status are asked of the staged copy.
plant s8
run_case s8 escalated -- -e billet_binary_src="$work/cases/s8/src/billet"
expect_allowed s8
expect_calls s8 candidate "@$work/cases/s8/src/billet" 0
journal=$(fact s8 recovery)
expect_calls s8 candidate "version @$journal/billet.candidate" 1
echo "ok   S8: a controller-side source is never executed; the staged copy is"

# S9. The downgrade refusal precedes the hold; the preparation never reaches systemctl.
plant s9
run_case s9 escalated BILLET_FAKE_CANDIDATE_VERSION=v0.9.0 -- -e billet_binary_src="$work/cases/s9/src/billet"
expect_refused s9 "Refuse a converge that would downgrade this host" "v0.9.0" "which is older" "Nothing was drained"
expect_calls s9 candidate "converge-guard hold" 0
expect_calls s9 managed "converge-guard hold" 0
for c in "$work"/cases/*/log; do
  [ -f "$c" ] || continue
  if grep -q '^role=systemctl' "$c"; then fail "the preparation called systemctl in $(basename "$(dirname "$c")")"; fi
done
echo "ok   S9: a downgrade refuses before the hold, and no preparation case reached systemctl"

# S10. The installed binary replaced between the decision and the hold.
plant s10
run_case s10 escalated BILLET_FAKE_HOLD_REPLACE="$work/cases/s10/bin/billet" -- -e billet_binary_src="$work/cases/s10/src/billet"
expect_refused s10 "Refuse a converge whose installed binary moved before the hold" "changed between this converge's decision and its hold" "converge-guard release --holder h1"
expect_calls s10 candidate "converge-guard hold --holder h1 --candidate" 1
rd test -f "$(root_of s10)/active/guard.json" || fail "s10: the guard was not left held"
plant s10-absent
rm -f "$work/cases/s10-absent/bin/billet"
run_case s10-absent escalated BILLET_FAKE_HOLD_REPLACE="$work/cases/s10-absent/bin/billet" -- -e billet_binary_src="$work/cases/s10-absent/src/billet"
expect_refused s10-absent "Refuse a converge whose installed binary moved before the hold" "absent when the binary change was decided"
echo "ok   S10: a binary that moved between the decision and the hold refuses under the guard"

# S11. The installed release read as B between the digest (A) and the hold,
# then A again under the guard: the digest agrees and the decision does not.
plant s11
printf 'v0.9.0\nv0.10.0\n' >"$work/cases/s11/versions"
run_case s11 escalated BILLET_FAKE_MANAGED_VERSION_SEQUENCE="$work/cases/s11/versions" BILLET_FAKE_CANDIDATE_VERSION=v0.9.5 -- -e billet_binary_src="$work/cases/s11/src/billet"
expect_refused s11 "Refuse a converge whose installed release moved before the hold" "billet v0.9.0" "billet v0.10.0" "converge-guard release --holder h1"
expect_calls s11 managed "version" 2
echo "ok   S11: an installed release that moved behind an unchanged digest refuses under the guard"

echo "converge guard: every guard, preparation and staging case passes"
