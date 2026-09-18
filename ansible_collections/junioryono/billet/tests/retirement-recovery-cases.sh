#!/usr/bin/env bash
# Sourced by retirement-cases.sh inside its selected CI section.
# R11: each unexplained artefact keeps ordinary work and mutations closed.
for kind in status stage; do
  name=r11-$kind-only
  r_plant "$name"
  r_answers "$name" "control-a:classify:1:dry-run-hold-$kind-only.json:0"
  r_run "$name"
  r_held "$name" hold
  reason=$("$python" -c 'import json, sys; print(json.load(open(sys.argv[1]))["route_why"])' "$work/retire-fixtures/dry-run-hold-$kind-only.json")
  r_reported "$name" hold "$reason"
done

# R14: control-b sees control-a's row; dispatch still says refused.
for requested in false true; do
  name=r14-other-host-row-$requested
  fixture=dry-run-other-host-row
  if [ "$requested" = true ]; then fixture=$fixture-requested; fi
  r_plant "$name"
  a "$name" -e "billet_server_retire=$requested" -e billet_gate_retirement_host=control-b
  r_answers "$name" "control-b:classify:1:$fixture.json:0"
  r_run "$name" play-retirement control-b
  if [ "$requested" = true ]; then
    r_held "$name" hold 'Refuse a held or unavailable retirement route' control-b
    r_reported "$name" hold "this deployment's retirement row belongs to another host (control-a)"
  else
    expect_allowed "$name"
    r_reported "$name" ordinary
    expect_play_task_ran "$name" 'Ordinary convergence sentinel'
    expect_host_commands "$name" 'control-b retire-classify 1;'
    expect_no_ordinary "$name"
    expect_no_task "$name" 'Inspect the transaction claim before recovery'
  fi
  "$python" - "$work/cases/$name/calls/index.jsonl" "$requested" <<'PYREQUESTED'
import json, sys
calls = [json.loads(line) for line in open(sys.argv[1])]
classifiers = [call for call in calls if call['command'] == 'retire-classify']
if len(classifiers) != 1 or ('--requested' in classifiers[0]['argv']) != (sys.argv[2] == 'true'):
    sys.exit('the classifier did not receive the inventory request flag exactly')
PYREQUESTED
done

# R11's unexplained marker and R15's two unreadable-row holds, requested or not.
for fixture in dry-run-adopt dry-run-hold-unreadable-row dry-run-hold-damaged-identity; do
  for requested in false true; do
    name=r15-$fixture-$requested
    r_plant "$name"
    a "$name" -e "billet_server_retire=$requested"
    r_answers "$name" "control-a:classify:1:$fixture.json:0"
    r_run "$name"
    r_held "$name" hold
  done
done

# R15: both unreadable-row exceptions admit the ordinary boundary. Neither
# exception has a requested classifier fixture; keep that gap in the notes.
for kind in node-only-unreadable-row never-commissioned; do
  name=r15-ordinary-$kind
  r_plant "$name"
  a "$name" -e billet_server_retire=false
  r_answers "$name" "control-a:classify:1:dry-run-ordinary-$kind.json:0"
  r_run "$name"
  expect_allowed "$name"
  r_reported "$name" ordinary
  expect_play_task_ran "$name" 'Ordinary convergence sentinel'
  expect_host_commands "$name" 'control-a retire-classify 1;'
  expect_no_ordinary "$name"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

# R18: the existing post-preparation injection removes the published record.
# Reaching the classifier and preserving its reason excludes a preparation
# refusal or a parser failure masquerading as the expected hold.
r_plant r18-abnormal-claim
a r18-abnormal-claim -e billet_gate_abnormal=true
r_answers r18-abnormal-claim 'control-a:classify:1:dry-run-hold-abnormal-claim.json:0'
r_run r18-abnormal-claim
expect_play_task_ran r18-abnormal-claim 'Inject a claim change after preparation'
expect_state r18-abnormal-claim active dir
expect_state r18-abnormal-claim record absent
expect_ran r18-abnormal-claim 'Ask the retirement classifier'
r_held r18-abnormal-claim hold
r_reported r18-abnormal-claim hold "the guard's claim is unpublished-guard; its ownership could not be established"

# R21: the real recovery tasks own every prerequisite refusal. The fake
# classifier admits only the route; the durable manifest drives the recovery.
r_recovery() { # case guard|legacy committed|missing
  local name=$1 shape=$2 decision=$3
  r_plant "$name"
  r_services "$name" control-a
  p "$name" 'plant_recovery recovery-20260909T120000-12345678 v0.11.0
mkdir -p /var/lib/billet/server
printf fenced >/var/lib/billet/server/billet.maintenance'
  "$python" - "$work/cases/$name/manifest.yml" <<'PY'
import sys, yaml
paths = ['/etc/billet/billet.yaml', '/etc/systemd/system/billet-server.service', '/etc/systemd/system/billet-node.service']
backups = ['billet.yaml.previous', 'billet-server.service.previous', 'billet-node.service.previous']
doc = dict(version=2, server_state_dir='/var/lib/billet/server', node_state_dir='/var/lib/billet/node',
           service_user='root', service_group='root', binary_existed=True,
           server_was_active=False, node_was_active=False,
           candidate_server_active=False, candidate_node_active=False,
           server_enablement='disabled', node_enablement='disabled',
           inputs=[dict(path=p, backup=b, existed=True, owner='root', group='root', mode='0640') for p, b in zip(paths, backups)])
with open(sys.argv[1], 'w') as stream:
    yaml.safe_dump(doc, stream)
PY
  p "$name" "cp '$work/cases/$name/manifest.yml' \"\$ROOT/recovery-20260909T120000-12345678/manifest.yml\""
  if [ "$shape" = guard ]; then
    p "$name" "plant_guard '$HOLDER' /usr/bin/billet; plant_pointer recovery-20260909T120000-12345678"
  else
    p "$name" 'printf "%s\n" "$ROOT/recovery-20260909T120000-12345678" >"$ROOT/active"'
  fi
  if [ "$decision" = committed ]; then
    p "$name" 'printf "host upgrade committed\n" >"$ROOT/recovery-20260909T120000-12345678/commit.complete"'
  fi
  r_answers "$name" "control-a:classify:1:dry-run-recovery-$shape.json:0;control-a:classify:2:dry-run-ordinary.json:0"
}
for shape in guard legacy; do
  name=r21-$shape
  r_recovery "$name" "$shape" committed
  r_run "$name"
  expect_refused "$name" 'Require a fresh converge after interrupted-upgrade recovery' 'finished from its durable commit' 'Rerun the playbook'
  expect_ran "$name" 'Durably close the host-upgrade transaction'
  expect_no_ordinary "$name"
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
  expect_host_commands "$name" 'control-a retire-classify 1;'
  expect_path_absent "$name" /var/lib/billet/server/billet.maintenance
  if [ "$shape" = guard ]; then expect_state "$name" pointer absent; else expect_state "$name" active absent; fi
  expect_calls "$name" systemctl 'start billet-server.service' 0
  expect_calls "$name" systemctl 'restart billet-server.service' 0
done
r_recovery r21-check guard committed
a r21-check --check
r_run r21-check
expect_allowed r21-check
expect_host_commands r21-check 'control-a retire-classify 1;'
expect_state r21-check pointer symlink
expect_no_ordinary r21-check
expect_no_task r21-check 'Inspect the transaction claim before recovery'
expect_no_play_task r21-check 'Ordinary convergence sentinel'
r_reported r21-check recovery

# A legacy claim is not an automatic hold: ordinary is admitted, while a
# retirement continuation must wait for a later guard preparation.
for route in ordinary continue; do
  name=r21-legacy-$route
  r_recovery "$name" legacy committed
  p "$name" 'rm /var/lib/billet/server/billet.maintenance'
  r_answers "$name" "control-a:classify:1:dry-run-$route.json:0"
  r_run "$name"
  if [ "$route" = ordinary ]; then
    expect_allowed "$name"
    expect_play_task_ran "$name" 'Ordinary convergence sentinel'
    expect_host_commands "$name" 'control-a retire-classify 1;'
  else
    r_held "$name" hold
    r_reported "$name" hold
    expect_final "$name" 'A legacy claim must finish binary recovery'
  fi
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

r_recovery r21-missing guard missing
r_run r21-missing
expect_refused r21-missing 'Refuse recovery with a missing or redirected input copy' 'durable copy is missing or redirected'
expect_no_ordinary r21-missing
expect_no_task r21-missing 'Stop compute before recovering the authoritative ledger'
expect_no_task r21-missing 'Require a fresh converge after interrupted-upgrade recovery'
expect_state r21-missing pointer symlink
expect_host_commands r21-missing 'control-a retire-classify 1;'

r_recovery r21-next-converge guard committed
printf '%s\n' "$work/play-retirement.yml" >"$work/cases/r21-next-converge/second-play"
r_run r21-next-converge
expect_refused r21-next-converge 'Require a fresh converge after interrupted-upgrade recovery' 'Rerun the playbook'
[ "$(cat "$work/cases/r21-next-converge/status-second")" -eq 0 ] || fail 'r21-next-converge: the fresh invocation failed' "$work/cases/r21-next-converge/out-second"
expect_host_commands r21-next-converge 'control-a retire-classify 1;control-a retire-classify 2;'
"$python" - "$work/cases/r21-next-converge/calls-first.jsonl" <<'PY'
import json, sys
calls = [json.loads(line) for line in open(sys.argv[1])]
if sum(c['command'] == 'retire-classify' for c in calls) != 1:
    sys.exit('recovery classified more than once in one invocation')
PY
expect_no_ordinary r21-next-converge
expect_no_play_task r21-next-converge 'Ordinary convergence sentinel'
grep -q 'The ordinary boundary was reached' "$work/cases/r21-next-converge/out-second" || fail 'r21-next-converge: the fresh classifier did not admit ordinary convergence'

# The REAL main.yml boundary, twice in distinct Ansible processes in the SAME
# namespace. No configuration or identity directory exists on either entry.
r_plant r-boundary
p r-boundary "plant_guard '$HOLDER' /usr/bin/billet"
r_answers r-boundary 'control-a:classify:1:dry-run-continue.json:0;control-a:request:1:done-unchanged.json:0;control-a:classify:2:dry-run-continue.json:0;control-a:request:2:done-unchanged.json:0'
printf '%s\n' "$work/play-retirement-main.yml" >"$work/cases/r-boundary/second-play"
r_run r-boundary play-retirement-main
expect_allowed r-boundary
[ "$(cat "$work/cases/r-boundary/status-second")" -eq 0 ] || fail 'r-boundary: second converge failed' "$work/cases/r-boundary/out-second"
expect_no_ordinary r-boundary
expect_host_commands r-boundary 'control-a retire-classify 1;control-a retire-request 1;control-a retire-classify 2;control-a retire-request 2;'
r_continuation r-boundary 2
r_reported r-boundary continue
r_reported r-boundary continue '' out-second
expect_path_absent r-boundary /var/lib/billet/server
expect_path_absent r-boundary /etc/billet/billet.yaml
for unit in billet-server.service billet-node.service billet-upgrade.timer billet-backup.timer billet-backup.service var-lib-billet-server.mount; do
  expect_unit_absent r-boundary "$unit"
done
"$python" - "$work/cases/r-boundary/out-second" <<'PY'
import re, sys
text = open(sys.argv[1]).read().split('PLAY RECAP')
if len(text) != 2:
    sys.exit('the second converge has no single recap')
rows = [line for line in text[1].splitlines() if ' : ' in line]
if len(rows) != 1 or not rows[0].startswith('control-a '):
    sys.exit('the second converge did not report the retired host')
counts = dict(re.findall(r'(\w+)=(\d+)', rows[0]))
if any(counts.get(key) != '0' for key in ['changed', 'failed', 'unreachable', 'rescued', 'ignored']):
    sys.exit('the second converge changed or failed: ' + rows[0])
PY

