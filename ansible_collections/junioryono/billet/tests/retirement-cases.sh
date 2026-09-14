#!/usr/bin/env bash
# Sourced by section R after its namespace runner and committed corpus exist.
# The isolated entry proves routing; the boundary invokes the real main.yml.
"$python" "$here/executable_version_check.py"

cat >"$work/play-retirement.yml" <<'PLAY'
---
- name: Exercise the retirement entry after real preparation
  hosts: control-a
  gather_facts: false
  vars:
    billet_exclusion_platform: Linux
    billet_binary_src: ''
    billet_server_should_run: false
  tasks:
    - name: Prepare this host
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
    - name: Inject a claim change after preparation
      ansible.builtin.command:
        argv: [/bin/sh, -c, 'rm -f /var/lib/billet/upgrades/active/guard.json']
      when: billet_gate_abnormal | default(false) | bool
      changed_when: true
    - name: Inject an invalid typed version after preparation
      ansible.builtin.set_fact:
        billet_exclusion_answerer_version: "{{ billet_gate_version }}"
      when: billet_gate_version is defined
    # The two inventory transports share this namespace's real prepared guard.
    # Publish that observation on the survivor for the fake row helper; fleet
    # preparation across separate hosts is the fleet playbook's gate.
    - name: Publish the shared namespace guard on the survivor transport
      ansible.builtin.set_fact:
        billet_exclusion_settled: "{{ billet_exclusion_settled }}"
        billet_exclusion_held: "{{ billet_exclusion_held }}"
        billet_exclusion_holder: "{{ billet_exclusion_holder }}"
        billet_upgrade_claim_shape: "{{ billet_upgrade_claim_shape }}"
        billet_exclusion_answerer: "{{ billet_exclusion_answerer }}"
      delegate_to: control-b
      delegate_facts: true
      when: billet_gate_peer | default(false) | bool
    - name: Route this host before ordinary work
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: retirement
    - name: Ordinary convergence sentinel
      ansible.builtin.debug:
        msg: The ordinary boundary was reached.
      when: not billet_retirement_bypass | bool
    - name: Prove the route left the bypass in its terminal state
      ansible.builtin.assert:
        that:
          - billet_retirement_bypass is sameas (billet_retirement_route != 'ordinary')
        fail_msg: The route left the ordinary boundary open.
PLAY
cat >"$work/play-retirement-main.yml" <<'PLAY'
---
- name: Exercise the real host entry on a retired server-only host
  hosts: control-a
  gather_facts: false
  vars:
    ansible_facts: {system: Linux, service_mgr: systemd}
    billet_binary_src: ''
    billet_config: {}
    billet_enable_server: false
    billet_enable_node: false
  roles:
    - junioryono.billet.host
PLAY

r_plant() { # case [version]
  plant "$1"
  p "$1" "plant_root; plant_managed ${2:-v0.10.1}"
  a "$1" -e billet_exclusion_platform=Linux -e billet_binary_src=
  e "$1" BILLET_GATE_RETIRE_ENV_SET=1
  e "$1" "BILLET_GATE_ANSWER=release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json;release:2:$here/fixtures/release-inspect/postgres-controller-guarded.json"
}
r_answers() { e "$1" "BILLET_GATE_RETIRE_ANSWERS=$2"; }
r_run() {
  ns_case "$1" escalated "${2:-play-retirement}"
  "$python" - "$work/cases/$1/calls/index.jsonl" "$work/cases/$1/args" <<'PYARGS'
import json, pathlib, sys
check = '--check' in pathlib.Path(sys.argv[2]).read_text().splitlines()
for line in open(sys.argv[1]):
    call = json.loads(line)
    if call['command'] != 'retire-classify':
        continue
    args = call['argv']
    if args[:2] != ['server', 'retire'] or '--dry-run' not in args or '--json' not in args:
        sys.exit('the classifier was not called in dry-run JSON mode')
    if args[args.index('--retiring-host') + 1] != 'control-a' or args[args.index('--config') + 1] != '/etc/billet/billet.yaml':
        sys.exit('the classifier did not bind its executing host and configuration')
    if check and ('--expected-holder' in args or '--expected-guard' in args):
        sys.exit('a check-mode classifier was given an invented held guard')
PYARGS
}
r_held() { # case route
  expect_refused "$1" 'Refuse a held or unavailable retirement route' "Retirement holds this host ($2)"
  expect_no_ordinary "$1"
  expect_no_play_task "$1" 'Ordinary convergence sentinel'
  expect_host_commands "$1" 'control-a retire-classify 1;'
  expect_no_task "$1" 'Inspect the transaction claim before recovery'
}
r_continuation() { # case request-count
  "$python" - "$work/cases/$1/calls" "$2" <<'PY'
import json, pathlib, sys
root, count = pathlib.Path(sys.argv[1]), int(sys.argv[2])
records = [json.loads(line) for line in (root / 'index.jsonl').read_text().splitlines()]
requests = [r for r in records if r['command'] == 'retire-request']
if len(requests) != count:
    sys.exit('the continuation count differs')
for r in requests:
    args = r['argv']
    if r['host'] != 'control-a' or '--installed-sha256' in args or '--server-only' in args:
        sys.exit('the continuation obtained a digest or fresh-request operand')
    if args[args.index('--survivor-host') + 1] != 'control-b':
        sys.exit('the continuation did not use the recorded survivor')
    if args[args.index('--retiring-host') + 1] != 'control-a':
        sys.exit('the retiring host was not independently bound')
    doc = json.loads((root / r['stdin']).read_text())
    if set(doc) != {'schema', 'round', 'self', 'survivor', 'nodes', 'desired'}:
        sys.exit('the continuation document has the wrong member set')
    if doc['schema'] != 1 or doc['survivor'] is not None or doc['nodes'] != {} or doc['desired'] is not None:
        sys.exit('the continuation collected fleet evidence')
    if doc['self']['host'] != 'control-a' or not isinstance(doc['self']['inspect'], dict):
        sys.exit('the continuation lacks this host report')
    if not doc['round']['id'] or doc['round']['started_at'] != doc['self']['collected_at']:
        sys.exit('the continuation lacks its round')
# A fleet collection may run a command other than retirement; see ALL calls.
if any(r['host'] != 'control-a' for r in records):
    sys.exit('the continuation contacted another host')
if any(r['command'] in ['status', 'migrate-endpoint', 'registration'] for r in records):
    sys.exit('the continuation collected fleet or installed configuration evidence')
PY
}

# R1: one classifier, ordinary admitted, no retirement mutation or recovery.
r_plant r1-ordinary
r_answers r1-ordinary 'control-a:classify:1:dry-run-ordinary.json:0'
r_run r1-ordinary
expect_allowed r1-ordinary
expect_play_task_ran r1-ordinary 'Ordinary convergence sentinel'
expect_host_commands r1-ordinary 'control-a retire-classify 1;'
expect_no_task r1-ordinary 'Inspect the transaction claim before recovery'

# R3: the existing journal wins even beside unreadable or disagreeing rows.
# Only intent-phase classifiers exist in the corpus; later phases are a gap.
for fixture in dry-run-continue dry-run-unknown-ledger dry-run-unknown-journal; do
  name=r3-$fixture
  r_plant "$name"
  r_answers "$name" "control-a:classify:1:$fixture.json:0;control-a:request:1:retired-settled.json:0"
  # If the caller reads the desired survivor, it reaches a real SSH failure.
  a "$name" -e billet_retirement_survivor_host=unreachable
  r_run "$name"
  expect_allowed "$name"
  expect_no_ordinary "$name"
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-request 1;'
  r_continuation "$name" 1
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

# R4/R5: the command's settled shortcut and a locally done pending obligation.
for spec in r4-settled:done-unchanged r5-pending:retired-pending; do
  name=${spec%%:*}; answer=${spec#*:}
  r_plant "$name"
  r_answers "$name" "control-a:classify:1:dry-run-continue.json:0;control-a:request:1:$answer.json:0"
  r_run "$name"
  expect_allowed "$name"
  expect_no_ordinary "$name"
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-request 1;'
  r_continuation "$name" 1
  if [ "$name" = r5-pending ]; then
    expect_ran "$name" "Report a row obligation whose survivor this converge did not prepare"
    expect_no_task "$name" 'Complete the row on the recorded survivor'
  fi
done

# R6's caller tail is included here; the request/reservation cases remain 5c.d.
r_plant r5-handoff
e r5-handoff 'BILLET_GATE_RETIRE_ENV=/etc/billet/server.env (ignore_errors=yes)'
r_answers r5-handoff 'control-a:classify:1:dry-run-continue.json:0;control-a:request:1:retired-pending.json:0;control-b:complete:1:completed.json:0;control-a:acknowledge:1:acknowledged.json:0;control-a:request:2:retired-settled.json:0'
a r5-handoff -e billet_gate_peer=true
r_run r5-handoff
expect_allowed r5-handoff
expect_host_commands r5-handoff 'control-a retire-classify 1;control-a retire-request 1;control-b retire-complete 1;control-a retire-acknowledge 1;control-a retire-request 2;'
expect_no_ordinary r5-handoff
expect_no_play_task r5-handoff 'Ordinary convergence sentinel'
expect_ran r5-handoff 'Settle locally after acknowledging the row'

"$python" - "$work/cases/r5-handoff/calls" "$work/retire-fixtures" <<'PYTAIL'
import json, pathlib, sys
root, fixtures = map(pathlib.Path, sys.argv[1:])
records = [json.loads(line) for line in (root / 'index.jsonl').read_text().splitlines()]
retire = [r for r in records if r['command'].startswith('retire-')]
for call in retire:
    args = call['argv']
    if '--installed-sha256' in args:
        sys.exit('the pending-row tail obtained an installed digest')
    if call['command'] == 'retire-acknowledge':
        if '--environment-file' in args or '--survivor-host' in args:
            sys.exit('acknowledgement was given ledger operands')
        expected = json.loads((fixtures / 'completed.json').read_text())
    elif call['command'] == 'retire-complete':
        if '--survivor-host' in args or args[args.index('--as-host') + 1] != 'control-b':
            sys.exit('completion was not bound to the survivor')
        expected = json.loads((fixtures / 'retired-pending.json').read_text())['completion']
    else:
        expected = None
    if call['command'] != 'retire-acknowledge' and args[args.index('--environment-file') + 1] != '/etc/billet/server.env':
        sys.exit('the installed environment file was not forwarded')
    if expected is not None and json.loads((root / call['stdin']).read_text()) != expected:
        sys.exit('the pending-row handoff carried the wrong document')
if any(r['host'] != 'control-a' and r['command'] != 'retire-complete' for r in records):
    sys.exit('a pending continuation collected the survivor fleet')
PYTAIL

# R8: both desired policies beside installed controller, unknown, node-only
# and never-commissioned evidence. A skipped classifier is asserted as skipped.
for policy in true false; do
  for shape in controller unknown node fresh; do
    for requested in false true; do
      name=r8-$shape-$policy-$requested
      r_plant "$name" v0.10.0
      a "$name" -e "billet_enable_server=$policy" -e "billet_server_retire=$requested"
      case "$shape" in
        controller) p "$name" 'mkdir -p /etc/billet /var/lib/billet/server; printf "server:\n  state_dir: /var/lib/billet/server\n" >/etc/billet/billet.yaml; printf minted >/var/lib/billet/server/deployment-id' ;;
        unknown) p "$name" 'mkdir -p /etc/billet; ln -s /missing/config /etc/billet/billet.yaml' ;;
        node) p "$name" 'mkdir -p /etc/billet; printf "node:\n  server_addr: control-b:7717\n" >/etc/billet/billet.yaml' ;;
        fresh) p "$name" 'mkdir -p /var/lib/billet/server' ;;
      esac
      r_run "$name"
      if [ "$requested" = false ] && { [ "$shape" = node ] || [ "$shape" = fresh ]; }; then
        expect_allowed "$name"
        expect_play_task_ran "$name" 'Ordinary convergence sentinel'
      else
        expect_refused "$name" 'Refuse a held or unavailable retirement route' 'Retirement holds this host (hold)'
        if [ "$requested" = true ]; then expect_final "$name" '--requested retirement requires a capable answerer'; fi
        expect_no_ordinary "$name"
        expect_no_play_task "$name" 'Ordinary convergence sentinel'
      fi
      expect_host_commands "$name" ''
      expect_no_task "$name" 'Ask the retirement classifier'
    done
  done
done

# A journal remains a veto even when the remaining local evidence is fresh.
for artefact in journal.json config-serverless.yaml authority-status; do
  name=r8-artefact-$artefact
  r_plant "$name" v0.10.0
  p "$name" 'mkdir -p /var/lib/billet/retired'
  case "$artefact" in authority-status) path=/var/lib/billet/authority-status ;; *) path=/var/lib/billet/retired/$artefact ;; esac
  p "$name" "printf present >'$path'"
  r_run "$name"
  expect_refused "$name" 'Refuse a held or unavailable retirement route' "$path is present or could not be examined"
  expect_host_commands "$name" ''
  expect_no_ordinary "$name"
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
done

# Empty answerers are skipped calls, not attempted-and-unanswered ones.
for artefact in absent present; do
  name=r8-empty-$artefact
  r_plant "$name"
  p "$name" 'rm /usr/bin/billet'
  if [ "$artefact" = present ]; then p "$name" 'mkdir -p /var/lib/billet/retired; printf present >/var/lib/billet/retired/journal.json'; fi
  r_run "$name"
  if [ "$artefact" = absent ]; then
    expect_allowed "$name"
    expect_play_task_ran "$name" 'Ordinary convergence sentinel'
  else
    expect_refused "$name" 'Refuse a held or unavailable retirement route' 'journal.json is present'
    expect_no_play_task "$name" 'Ordinary convergence sentinel'
  fi
  expect_host_commands "$name" ''
  expect_no_task "$name" 'Ask the retirement classifier'
done

# The missing/unknown version type cases enter the caller, not the JSON parser.
for spec in 'missing:{}' 'unknown:{"type":"invented"}' 'unreadable:{"type":"unreadable"}' 'development:{"type":"development"}'; do
  name=r8-version-${spec%%:*}; value=${spec#*:}
  r_plant "$name"
  a "$name" -e "{\"billet_gate_version\":$value}"
  r_answers "$name" 'control-a:classify:1:dry-run-ordinary.json:0'
  r_run "$name"
  if [ "${spec%%:*}" = development ]; then
    expect_allowed "$name"
    expect_play_task_ran "$name" 'Ordinary convergence sentinel'
    expect_host_commands "$name" 'control-a retire-classify 1;'
  else
    expect_refused "$name" 'Refuse a held or unavailable retirement route' 'version type'
    expect_no_ordinary "$name"
    expect_no_play_task "$name" 'Ordinary convergence sentinel'
    expect_host_commands "$name" ''
  fi
done
r_plant r8-unanswered
r_answers r8-unanswered 'control-a:classify:1:dry-run-ordinary.json:0'
e r8-unanswered 'BILLET_GATE_DROP_ANSWER=retire-classify:1'
r_run r8-unanswered
[ "$status" -ne 0 ] || fail 'r8-unanswered: an unanswered classifier converged'
expect_final r8-unanswered 'did not answer retirement' 'state is unknown'
expect_host_commands r8-unanswered 'control-a retire-classify 1;'
expect_no_ordinary r8-unanswered
expect_no_play_task r8-unanswered 'Ordinary convergence sentinel'

for kind in empty unreadable development; do
  name=r8-requested-$kind
  r_plant "$name"
  a "$name" -e billet_server_retire=true
  if [ "$kind" = empty ]; then
    p "$name" 'rm /usr/bin/billet'
  else
    printf 'billet %s linux/amd64\n' "$([ "$kind" = development ] && printf '(devel)' || printf broken)" >"$work/cases/$name/version-line"
    e "$name" "BILLET_GATE_ANSWER=version:3:$work/cases/$name/version-line"
    r_answers "$name" 'control-a:classify:1:dry-run-hold-unreadable-row.json:0'
  fi
  r_run "$name"
  expect_refused "$name" 'Refuse a held or unavailable retirement route' 'Retirement holds this host (hold)'
  expect_no_ordinary "$name"
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
  if [ "$kind" = development ]; then
    expect_host_commands "$name" 'control-a retire-classify 1;'
  else
    expect_final "$name" '--requested retirement requires a capable answerer'
    expect_host_commands "$name" ''
  fi
done

# R9: every route in check mode, ordinary alone reaches its ordinary boundary.
for spec in ordinary:dry-run-ordinary hold:dry-run-hold-unreadable-row continue:dry-run-continue recovery:dry-run-recovery-guard new-request:dry-run-new-request cancel:dry-run-cancel unsupported-variant:dry-run-unsupported-variant; do
  route=${spec%%:*}; fixture=${spec#*:}; name=r9-$route
  r_plant "$name"
  a "$name" --check
  r_answers "$name" "control-a:classify:1:$fixture.json:0"
  r_run "$name"
  expect_allowed "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;'
  expect_ran "$name" 'Report the retirement route'
  if [ "$route" = ordinary ]; then expect_play_task_ran "$name" 'Ordinary convergence sentinel'; else expect_no_play_task "$name" 'Ordinary convergence sentinel'; fi
  expect_no_ordinary "$name"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

# R10's installed-both request fixture; the desired configuration has no node.
r_plant r10-installed-both
a r10-installed-both -e billet_server_retire=true -e '{"billet_config":{"server":{"state_dir":"/var/lib/billet/server"}}}'
r_answers r10-installed-both 'control-a:classify:1:dry-run-unsupported-variant.json:0'
r_run r10-installed-both
r_held r10-installed-both unsupported-variant

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

# R18 needs a producer fixture over an abnormal claim. dry-run-adopt is an
# unexplained marker, not that observation, so it is not substituted for one.

# R21: the real recovery tasks own every prerequisite refusal. The fake
# classifier admits only the route; the durable manifest drives the recovery.
r_recovery() { # case guard|legacy committed|missing
  local name=$1 shape=$2 decision=$3
  r_plant "$name"
  r_services "$name" control-a
  p "$name" 'plant_recovery recovery-20260909T120000-12345678 v0.10.1
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
expect_ran r21-check 'Report the retirement route'

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
