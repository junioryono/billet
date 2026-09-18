#!/usr/bin/env bash
# Called only inside ns_case after its overlays, wrappers and environment exist.
set -euo pipefail
case_dir=$1
play=$2
helper=$BILLET_GATE_ROLE_HELPER
observer=${helper%/*}/retirement-role-observe.py
read -r scenario variant <"$case_dir/role-case"
export ANSIBLE_STRATEGY_PLUGINS="${helper%/*}/strategy_plugins"
export BILLET_GATE_SERVICES="$case_dir/services"
mkdir -p "$BILLET_GATE_SERVICES"
"$PYTHON" "$helper" seed
run_role() {
  env BILLET_CONVERGE_GUARD_HOLDER="$1" "$ANSIBLE_PLAYBOOK" -i "$INVENTORY" "$play" \
    -e ansible_become=false -e "@$case_dir/role-vars.json"
}
# Establish node-side resources with the same role before measuring either leg.
if run_role "$HOLDER" >"$case_dir/out-seed" 2>&1; then
  :
else
  seed_rc=$?
  cat "$case_dir/out-seed"
  exit "$seed_rc"
fi
"$PYTHON" "$observer" seed
"$PYTHON" "$helper" prepare "$scenario" "$variant"
"$PYTHON" "$observer" snapshot >"$case_dir/protected-before.json"
"$PYTHON" "$observer" initial >"$case_dir/initial-files.json"
"$PYTHON" "$observer" watch &
watch_pid=$!
finish_watch() {
  touch "$case_dir/watch-stop"
  wait "$watch_pid"
}
trap 'touch "$case_dir/watch-stop"; wait "$watch_pid"' EXIT
for _ in $(seq 1 100); do
  [ ! -f "$case_dir/watch-ready" ] || break
  kill -0 "$watch_pid"
  sleep 0.05
done
[ -f "$case_dir/watch-ready" ]
printf observed >/etc/billet/gate-observer-canary
rm /etc/billet/gate-observer-canary
set +e
run_role "$HOLDER" >"$case_dir/out-first" 2>&1
first_rc=$?
set -e
printf '%s\n' "$first_rc" >"$case_dir/role-status-first"
cat "$case_dir/out-first"
if [ "$scenario" = resume ] && [ "$variant" = retained ]; then
  [ "$first_rc" -ne 0 ]
  [ -f "$case_dir/interrupted.json" ]
  "$PYTHON" "$observer" interrupted
else
  [ "$first_rc" -eq 0 ]
  [ ! -e "$case_dir/interrupted.json" ]
fi
if [ "$scenario" = resume ] || [ "$scenario" = stable ]; then
  cp /var/lib/billet/upgrades/active/guard.json "$case_dir/guard-before.json"
  # The preceding Ansible process has exited. A settled, pointer-free guard
  # has nothing to take over: the existing authorized recovery removes it, and
  # the next main.yml obtains and settles its own guard through prepare.
  "$PYTHON" "$observer" recover "$HOLDER" >"$case_dir/recovery-out" 2>&1
  [ ! -e /var/lib/billet/upgrades/active ]
  "$PYTHON" "$helper" next
  if run_role "${HOLDER}-next" >"$case_dir/out-second" 2>&1; then
    :
  else
    second_rc=$?
    cat "$case_dir/out-second"
    exit "$second_rc"
  fi
  cp /var/lib/billet/upgrades/active/guard.json "$case_dir/guard-after.json"
  cat "$case_dir/out-second"
fi
finish_watch
trap - EXIT
"$PYTHON" "$observer" snapshot >"$case_dir/protected-after.json"
"$PYTHON" "$observer" finish "$scenario" "$variant"
