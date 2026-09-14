#!/usr/bin/env bash
# Sourced by section R after its namespace runner and committed corpus exist.
# The isolated entry proves routing; the boundary invokes the real main.yml.

cat >"$work/play-retirement.yml" <<'PLAY'
---
- name: Capture the ordinary template in each node's context
  hosts: control-b:node-a
  gather_facts: false
  tasks:
    - name: Capture the complete ordinary role rendering
      ansible.builtin.template:
        src: "{{ billet_gate_ordinary_template }}"
        dest: "{{ billet_gate_render_dir }}/{{ inventory_hostname }}.yaml"
        mode: "0600"
      when: billet_gate_capture_rendering | default(false) | bool
      no_log: true
- name: Exercise the retirement entry after real preparation
  hosts: "{{ [billet_gate_retirement_host | default('control-a'), 'control-b'] if billet_gate_new | default(false) | bool else billet_gate_retirement_host | default('control-a') }}"
  gather_facts: "{{ billet_gate_new | default(false) | bool }}"
  vars:
    billet_exclusion_platform: Linux
    billet_binary_src: ''
    billet_server_should_run: false
  tasks:
    - name: Exercise only the selected retiring host
      ansible.builtin.include_tasks: retirement-entry-tasks.yml
      when: inventory_hostname == billet_gate_retirement_host | default('control-a')
PLAY
cat >"$work/retirement-entry-tasks.yml" <<'PLAY'
---
- name: Drive the selected retirement entry
  block:
    - name: Prepare this host
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
    - name: Prove legacy preparation published a dry-run answerer
      ansible.builtin.assert:
        that:
          - ansible_check_mode
          - billet_upgrade_claim_shape == 'legacy-file'
          - billet_exclusion_answerer == '/usr/bin/billet'
      when: billet_gate_legacy_answerer is defined
    # Damage the executable only after real preparation published its path.
    # A non-executable file planted earlier cannot answer preparation at all.
    - name: Invalidate the legacy executable after preparation
      ansible.builtin.command:
        argv:
          - /bin/sh
          - -ec
          - |
            case "$1" in
              symlink)
                mv /usr/bin/billet /usr/bin/billet.legacy-target
                ln -s /usr/bin/billet.legacy-target /usr/bin/billet
                ;;
              other-owner) chown 65534 /usr/bin/billet ;;
              group-writable) chmod 0775 /usr/bin/billet ;;
              not-executable) chmod 0644 /usr/bin/billet ;;
              *) exit 1 ;;
            esac
          - legacy-answerer
          - "{{ billet_gate_legacy_answerer }}"
      changed_when: true
      check_mode: false
      become: true
      when:
        - billet_gate_legacy_answerer is defined
        - billet_gate_legacy_answerer != 'verified'
    - name: Inject a claim change after preparation
      ansible.builtin.command:
        argv: [/bin/sh, -ec, 'test -f /var/lib/billet/upgrades/active/guard.json && rm /var/lib/billet/upgrades/active/guard.json']
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
    - name: Publish new-request collection answerers on the node transports
      ansible.builtin.set_fact:
        billet_exclusion_answerer: "{{ billet_exclusion_answerer }}"
        billet_exclusion_darwin: false
      delegate_to: "{{ item }}"
      delegate_facts: true
      loop: [control-b, node-a]
      when:
        - billet_gate_new | default(false) | bool
        - item != 'node-a' or not billet_gate_no_answerer | default(false) | bool
    - name: Vary only the named survivor guard clause
      ansible.builtin.set_fact:
        billet_exclusion_settled: "{{ false if billet_gate_peer_clause == 'unsettled' else true }}"
        billet_exclusion_held: "{{ false if billet_gate_peer_clause == 'unheld' else true }}"
        billet_exclusion_holder: "{{ 'someone-else' if billet_gate_peer_clause == 'holder' else billet_exclusion_holder }}"
        billet_upgrade_claim_shape: "{{ 'guard-pointer' if billet_gate_peer_clause == 'pointer' else 'guard' }}"
      delegate_to: control-b
      delegate_facts: true
      when: billet_gate_peer_clause is defined
    - name: Vary the binary-upgrade precondition after preparation
      ansible.builtin.set_fact:
        billet_binary_upgrade: true
      when: billet_gate_binary_upgrade | default(false) | bool
    - name: Inject an unfinished own guard after preparation
      ansible.builtin.command:
        argv: ["{{ ansible_playbook_python }}", "{{ billet_gate_own_guard_script }}", "{{ billet_gate_own_guard }}"]
      changed_when: true
      when: billet_gate_own_guard is defined
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
          - >-
            billet_retirement_bypass is sameas
            (billet_retirement_route != 'ordinary'
             and not (ansible_check_mode and billet_retirement_route == 'unverified-check-mode'))
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
  p "$1" "plant_root; plant_managed ${2:-v0.11.0}"
  a "$1" -e billet_binary_src=
  e "$1" BILLET_GATE_RETIRE_ENV_SET=1
  e "$1" "BILLET_GATE_ANSWER=release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json;release:2:$here/fixtures/release-inspect/postgres-controller-guarded.json"
}
r_answers() { e "$1" "BILLET_GATE_RETIRE_ANSWERS=$2"; }
r_run() {
  ns_case "$1" escalated "${2:-play-retirement}"
  "$python" - "$work/cases/$1/calls/index.jsonl" "$work/cases/$1/args" "${3:-control-a}" <<'PYARGS'
import json, pathlib, sys
check = '--check' in pathlib.Path(sys.argv[2]).read_text().splitlines()
for line in open(sys.argv[1]):
    call = json.loads(line)
    if call['command'] != 'retire-classify':
        continue
    args = call['argv']
    if args[:2] != ['server', 'retire'] or '--dry-run' not in args or '--json' not in args:
        sys.exit('the classifier was not called in dry-run JSON mode')
    if args[args.index('--retiring-host') + 1] != sys.argv[3] or args[args.index('--config') + 1] != '/etc/billet/billet.yaml':
        sys.exit('the classifier did not bind its executing host and configuration')
    if check and ('--expected-holder' in args or '--expected-guard' in args):
        sys.exit('a check-mode classifier was given an invented held guard')
PYARGS
}
r_held() { # case route [first-failure-task [host]]
  expect_refused "$1" "${3:-Refuse a held or unavailable retirement route}" "Retirement holds this host ($2)"
  expect_no_ordinary "$1"
  expect_no_play_task "$1" 'Ordinary convergence sentinel'
  expect_host_commands "$1" "${4:-control-a} retire-classify 1;"
  expect_no_task "$1" 'Inspect the transaction claim before recovery'
}
r_reported() { # case route [reason [output-file]]
  expect_ran "$1" 'Report the retirement route'
  "$python" - "$work/cases/$1/${4:-out}" "$2" "${3:-}" <<'PYREPORT'
import json, pathlib, sys
text = pathlib.Path(sys.argv[1]).read_text()
header = 'TASK [junioryono.billet.host : Report the retirement route]'
blocks = text.split(header)
if len(blocks) != 2:
    sys.exit('the retirement route was not reported exactly once')
block = blocks[1].split('TASK [', 1)[0]
messages = [json.loads(line.strip().removeprefix('"msg": ').removesuffix(','))
            for line in block.splitlines() if line.strip().startswith('"msg": ')]
if len(messages) != 1 or not messages[0].startswith('Retirement route ' + sys.argv[2] + ': '):
    sys.exit('the reported retirement route differs: ' + repr(messages))
if sys.argv[3] and sys.argv[3] not in messages[0]:
    sys.exit('the reported retirement reason differs: ' + repr(messages))
PYREPORT
}
r_continuation() { # case request-count [environment-file [handoff]]
  "$python" - "$work/cases/$1/calls" "$2" "$HOLDER" "$here/fixtures/release-inspect/postgres-controller-guarded.json" "${3:-}" "${4:-}" <<'PY'
import datetime, decimal, json, pathlib, re, sys
root, count = pathlib.Path(sys.argv[1]), int(sys.argv[2])
holder, inspect_fixture, environment_file, handoff = sys.argv[3:]
expected_inspect = json.loads(pathlib.Path(inspect_fixture).read_text())
records = [json.loads(line) for line in (root / 'index.jsonl').read_text().splitlines()]
requests = [r for r in records if r['command'] == 'retire-request']
if len(requests) != count:
    sys.exit('the continuation count differs')
# All continuation fixtures record control-b; inventory independently names control-a.
expected_args = ['server', 'retire', '--input', '-', '--json', '--run', holder,
                 '--retiring-host', 'control-a', '--survivor-host', 'control-b',
                 '--config', '/etc/billet/billet.yaml']
if environment_file:
    expected_args += ['--environment-file', environment_file]
# The handoff reuses its envelope; a second converge must collect its own.
converges = [records]
first_index = root.parent / 'calls-first.jsonl'
if first_index.exists():
    first = [json.loads(line) for line in first_index.read_text().splitlines()]
    if records[:len(first)] != first:
        sys.exit('the first converge call snapshot differs')
    converges = [first, records[len(first):]]
for calls in converges:
    inspections = [r for r in calls if r['argv'][:1] == ['release']]
    if len(inspections) != 1:
        sys.exit('each continuation converge must inspect locally exactly once')
    inspection = inspections[0]
    if inspection['host'] != 'control-a' or inspection['argv'] != ['release', 'inspect', '--json', '--config', '/etc/billet/billet.yaml']:
        sys.exit('the local continuation inspection argv differs')
    local_requests = [r for r in calls if r['command'] == 'retire-request']
    if not local_requests or any(r['sequence'] <= inspection['sequence'] for r in local_requests):
        sys.exit('the continuation did not collect its inspection before requesting')

def timestamp(value, member):
    # Require RFC 3339 syntax before parsing the calendar and timezone; retain
    # fractional precision when comparing, beyond datetime's microseconds.
    match = re.fullmatch(r'([0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})(\.[0-9]+)?(Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])', value) if isinstance(value, str) else None
    if match is None:
        sys.exit(member + ' is not an RFC 3339 timestamp')
    seconds, fraction, zone = match.groups()
    try:
        parsed = datetime.datetime.fromisoformat(seconds + zone.replace('Z', '+00:00'))
    except ValueError:
        sys.exit(member + ' is not an RFC 3339 timestamp')
    return parsed, decimal.Decimal('0' + (fraction or ''))

for r in requests:
    if r['host'] != 'control-a' or r['argv'] != expected_args:
        sys.exit('the complete continuation argv differs: ' + repr(r['argv']))
    doc = json.loads((root / r['stdin']).read_text())
    if set(doc) != {'schema', 'round', 'self', 'survivor', 'nodes', 'desired'}:
        sys.exit('the continuation document has the wrong member set')
    if type(doc['schema']) is not int or doc['schema'] != 1 or doc['survivor'] is not None or doc['nodes'] != {} or doc['desired'] is not None:
        sys.exit('the continuation collected fleet evidence')
    if set(doc['self']) != {'host', 'collected_at', 'inspect', 'status'} or set(doc['round']) != {'id', 'started_at'}:
        sys.exit('the continuation envelope or round has the wrong member set')
    if doc['self']['host'] != 'control-a' or doc['self']['inspect'] != expected_inspect:
        sys.exit('the continuation did not carry the local producer inspection')
    if doc['self']['status'] is not None:
        sys.exit('the continuation must carry a null self status')
    started = timestamp(doc['round']['started_at'], 'round.started_at')
    collected = timestamp(doc['self']['collected_at'], 'self.collected_at')
    if started > collected:
        sys.exit('the continuation inspection predates its round')
    if doc['round']['id'] != holder + '-' + doc['round']['started_at']:
        sys.exit('the continuation round does not bind this holder and start')
# A fleet collection may run a command other than retirement; see ALL calls.
if any(arg == '--installed-sha256' or arg.startswith('--installed-sha256=') for r in records for arg in r['argv']):
    sys.exit('a continuation case passed an installed digest')
if any(r['host'] != 'control-a' and not (handoff == 'handoff' and r['host'] == 'control-b' and r['command'] == 'retire-complete') for r in records):
    sys.exit('the continuation contacted another host')
if any(r['command'] in ['status', 'migrate-endpoint', 'registration'] for r in records):
    sys.exit('the continuation called a fleet-evidence command')
PY
}

# R1: one classifier, ordinary admitted, no retirement mutation or recovery.
if [ "${BILLET_GATE_ONLY:-}" != retirement-request ]; then
"$python" "$here/executable_version_check.py"
r_plant r1-ordinary
r_answers r1-ordinary 'control-a:classify:1:dry-run-ordinary.json:0'
r_run r1-ordinary
expect_allowed r1-ordinary
expect_play_task_ran r1-ordinary 'Ordinary convergence sentinel'
expect_host_commands r1-ordinary 'control-a retire-classify 1;'
expect_no_task r1-ordinary 'Inspect the transaction claim before recovery'

# R3: the existing journal wins even beside unreadable or disagreeing rows.
# Later phases still collect only this host, including past the archive.
for fixture in dry-run-continue dry-run-unknown-ledger dry-run-unknown-journal dry-run-continue-stopped dry-run-continue-archived; do
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
  r_reported "$name" continue
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

# Judge the task's own changed result, not preparation's recap. The settled
# controls must report ok; a republished status must report changed even when
# the outcome word is unchanged. Also require the final settlement message.
r_done_report() { # case True|False ok|changed
  "$python" - "$work/cases/$1/out" "$2" "$3" <<'PYDONE'
import json, pathlib, sys
text = pathlib.Path(sys.argv[1]).read_text()
def block(task):
    parts = text.split('TASK [junioryono.billet.host : ' + task + ']')
    if len(parts) != 2:
        sys.exit('the done task did not appear exactly once: ' + task)
    return parts[1].split('TASK [', 1)[0]
result = block("Record the continuation's result and changes")
verdicts = [line.split(':', 1)[0] for line in result.splitlines()
            if line.startswith(('ok:', 'changed:', 'fatal:', 'failed:', 'skipping:'))]
if verdicts != [sys.argv[3]]:
    sys.exit('the done change report differs: ' + repr(verdicts))
report = block("Report the retirement's settlement or remaining obligation")
messages = [json.loads(line.strip().removeprefix('"msg": ').removesuffix(','))
            for line in report.splitlines() if line.strip().startswith('"msg": ')]
if len(messages) != 1 or not messages[0].startswith('Retirement is locally done; settled=' + sys.argv[2] + '.'):
    sys.exit('the done settlement report differs: ' + repr(messages))
PYDONE
}

# R4/R5: done observations reach both settlement paths. An absent status on
# the settled journal is repaired; an already closed status stays unchanged.
for spec in r4-settled:dry-run-continue:done-unchanged r5-pending:dry-run-continue:retired-pending r4-done-settled:dry-run-continue-done-settled:done-unchanged-republished r4-closed-status:dry-run-settled-closed-status:done-unchanged r5-done-unsettled:dry-run-continue-done-unsettled:retired-pending; do
  name=${spec%%:*}; rest=${spec#*:}; fixture=${rest%%:*}; answer=${rest#*:}
  r_plant "$name"
  r_answers "$name" "control-a:classify:1:$fixture.json:0;control-a:request:1:$answer.json:0"
  r_run "$name"
  expect_allowed "$name"
  expect_no_ordinary "$name"
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-request 1;'
  r_continuation "$name" 1
  r_reported "$name" continue
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
  if [ "$answer" = retired-pending ]; then
    r_done_report "$name" False changed
    expect_ran "$name" "Report a row obligation whose survivor this converge did not prepare"
    expect_no_task "$name" 'Complete the row on the recorded survivor'
  else
    if [ "$answer" = done-unchanged-republished ]; then
      r_done_report "$name" True changed
    else
      r_done_report "$name" True ok
    fi
    expect_no_task "$name" "Report a row obligation whose survivor this converge did not prepare"
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
expect_ran r5-handoff 'Require a known completed server-only retirement'
r_continuation r5-handoff 2 /etc/billet/server.env handoff
r_reported r5-handoff continue

"$python" - "$work/cases/r5-handoff/calls" "$work/retire-fixtures" <<'PYTAIL'
import json, pathlib, sys
root, fixtures = map(pathlib.Path, sys.argv[1:])
records = [json.loads(line) for line in (root / 'index.jsonl').read_text().splitlines()]
retire = [r for r in records if r['command'].startswith('retire-')]
for call in retire:
    args = call['argv']
    if '--installed-sha256' in args:
        sys.exit('the pending-row tail passed an installed digest')
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

# R8: incapable or absent answerers without a candidate hold, independent of policy,
# requested retirement, installed node-only bytes or a fresh-host shape.
# v0.10.1 is explicitly below the complete-classifier floor, v0.11.0.
# Check mode reports the same hold and succeeds, without ordinary work.
for kind in below-floor unreadable absent; do
  for policy in true false; do
    for requested in false true; do
      for mode in normal check; do
        for shape in node fresh; do
          name=r8-$kind-$policy-$requested-$mode-$shape
          if [ "$kind" = below-floor ]; then r_plant "$name" v0.10.1; else r_plant "$name"; fi
          a "$name" -e "{\"billet_enable_server\": $policy}" -e "billet_server_retire=$requested"
          case "$kind" in
            unreadable) a "$name" -e '{"billet_gate_version":{"type":"unreadable"}}' ;;
            absent) p "$name" 'rm /usr/bin/billet' ;;
          esac
          case "$shape" in
            node) p "$name" 'mkdir -p /etc/billet; printf "node: {name: x}\n" >/etc/billet/billet.yaml' ;;
            fresh) p "$name" 'mkdir -p /var/lib/billet/server' ;;
          esac
          if [ "$mode" = check ]; then a "$name" --check; fi
          r_run "$name"
          if [ "$mode" = check ]; then
            expect_allowed "$name"
          else
            expect_refused "$name" 'Refuse a held or unavailable retirement route' 'Retirement holds this host (hold)'
          fi
          r_reported "$name" hold 'This collection needs billet at or above v0.11.0 on the host'
          r_reported "$name" hold 'upgrade the managed binary or converge with an older collection'
          if [ "$requested" = true ]; then
            r_reported "$name" hold '--requested retirement is refused without a capable answerer'
          fi
          expect_no_ordinary "$name"
          expect_no_play_task "$name" 'Ordinary convergence sentinel'
          expect_host_commands "$name" ''
          expect_no_task "$name" 'Ask the retirement classifier'
          expect_no_task "$name" 'Inspect the transaction claim before recovery'
        done
      done
    done
  done
done

# An unknown route must traverse the caller's rescue and ordinary boundary,
# not only R7's parser harness. Corrupt a case-local copy, never the corpus.
r_plant r7-caller-unknown-route
"$python" - "$work/retire-fixtures/dry-run-ordinary.json" "$work/cases/r7-caller-unknown-route/unknown-route.json" <<'PYROUTE'
import json, sys
answer = json.load(open(sys.argv[1]))
answer['route'] = 'invented'
with open(sys.argv[2], 'w') as stream:
    json.dump(answer, stream)
PYROUTE
e r7-caller-unknown-route "BILLET_GATE_RETIRE_FIXTURES=$work/cases/r7-caller-unknown-route"
r_answers r7-caller-unknown-route 'control-a:classify:1:unknown-route.json:0'
r_run r7-caller-unknown-route
r_held r7-caller-unknown-route hold 'Judge each retirement member'
expect_final r7-caller-unknown-route 'Retirement holds this host (hold)'
grep -qF 'answered with a member this role cannot read: route' "$work/cases/r7-caller-unknown-route/out" || fail 'r7-caller-unknown-route: the parser did not name the unknown route'
r_reported r7-caller-unknown-route hold

# The classifier already normalizes an unknown state into hold. Its reason
# must survive intact; the caller may not replace it with a state judgement.
r_plant r7-caller-unknown-state
"$python" - "$work/retire-fixtures/dry-run-hold-unreadable-row.json" "$work/cases/r7-caller-unknown-state/unknown-state.json" <<'PYSTATE'
import json, sys
answer = json.load(open(sys.argv[1]))
answer['state'] = 'unknown'
with open(sys.argv[2], 'w') as stream:
    json.dump(answer, stream)
PYSTATE
e r7-caller-unknown-state "BILLET_GATE_RETIRE_FIXTURES=$work/cases/r7-caller-unknown-state"
r_answers r7-caller-unknown-state 'control-a:classify:1:unknown-state.json:0'
r_run r7-caller-unknown-state
r_held r7-caller-unknown-state hold
reason=$("$python" -c 'import json, sys; print(json.load(open(sys.argv[1]))["route_why"])' "$work/cases/r7-caller-unknown-state/unknown-state.json")
r_reported r7-caller-unknown-state hold "$reason"

# The missing/unknown version type cases enter the caller, not the JSON parser.
for spec in 'missing:{}' 'unknown:{"type":"invented"}' 'development:{"type":"development"}'; do
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
    expect_refused "$name" 'Refuse a held or unavailable retirement route' 'This collection needs billet at or above v0.11.0 on the host'
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

# A development answerer attempts classification, including with --requested.
r_plant r8-requested-development
a r8-requested-development -e billet_server_retire=true
printf 'billet (devel) linux/amd64\n' >"$work/cases/r8-requested-development/version-line"
e r8-requested-development "BILLET_GATE_ANSWER=version:3:$work/cases/r8-requested-development/version-line"
r_answers r8-requested-development 'control-a:classify:1:dry-run-hold-unreadable-row.json:0'
r_run r8-requested-development
r_held r8-requested-development hold

# R9: every route in check mode, ordinary alone reaches its ordinary boundary.
for spec in ordinary:dry-run-ordinary hold:dry-run-hold-unreadable-row continue:dry-run-continue recovery:dry-run-recovery-guard new-request:dry-run-new-request cancel:dry-run-cancel unsupported-variant:dry-run-unsupported-variant; do
  route=${spec%%:*}; fixture=${spec#*:}; name=r9-$route
  r_plant "$name"
  if [ "$route" = new-request ]; then a "$name" -e billet_retirement_survivor_host=control-b; fi
  a "$name" --check
  r_answers "$name" "control-a:classify:1:$fixture.json:0"
  r_run "$name"
  expect_allowed "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;'
  r_reported "$name" "$route"
  if [ "$route" = ordinary ]; then expect_play_task_ran "$name" 'Ordinary convergence sentinel'; else expect_no_play_task "$name" 'Ordinary convergence sentinel'; fi
  expect_no_ordinary "$name"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

# R9: the same fresh host previews without a classifier in check mode, and
# classifies through the candidate that real preparation stages in a real run.
for mode in check normal; do
  name=r9-fresh-candidate-$mode
  r_plant "$name"
  p "$name" 'rm /usr/bin/billet'
  a "$name" -e "billet_binary_src=$bins/wrap-candidate-v0.11.0"
  if [ "$mode" = check ]; then a "$name" --check; fi
  r_answers "$name" 'control-a:classify:1:dry-run-ordinary-never-commissioned.json:0'
  r_run "$name"
  expect_allowed "$name"
  expect_play_task_ran "$name" 'Ordinary convergence sentinel'
  expect_state "$name" managed absent
  expect_no_ordinary "$name"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
  if [ "$mode" = check ]; then
    r_reported "$name" unverified-check-mode 'The retirement route is unverified in check mode'
    r_reported "$name" unverified-check-mode 'A real run classifies with the staged candidate'
    r_reported "$name" unverified-check-mode 'Previewing ordinary tasks may differ from that classified route.'
    expect_host_commands "$name" ''
    expect_no_task "$name" 'Ask the retirement classifier'
    expect_no_task "$name" 'Stage the immutable candidate binary inside its recovery journal'
    expect_calls "$name" candidate '' 0
    expect_state "$name" active absent
  else
    r_reported "$name" ordinary
    expect_ran "$name" 'Stage the immutable candidate binary inside its recovery journal'
    expect_ran "$name" 'Ask the staged candidate to prepare'
    expect_host_commands "$name" 'control-a retire-classify 1;'
    expect_calls "$name" candidate 'server retire --dry-run' 1
    expect_state "$name" record_preparing False
  fi
done

# A newer candidate does not replace a managed answerer that carries the
# guard. The managed version remains below the retirement floor in both modes.
for mode in check normal; do
  name=r8-managed-with-candidate-$mode
  r_plant "$name" v0.10.1
  a "$name" -e "billet_binary_src=$bins/wrap-candidate-v0.11.0"
  if [ "$mode" = check ]; then a "$name" --check; fi
  r_run "$name"
  if [ "$mode" = check ]; then
    expect_allowed "$name"
    expect_no_task "$name" 'Stage the immutable candidate binary inside its recovery journal'
  else
    expect_refused "$name" 'Refuse a held or unavailable retirement route' 'Retirement holds this host (hold)'
    expect_ran "$name" 'Stage the immutable candidate binary inside its recovery journal'
  fi
  r_reported "$name" hold 'This collection needs billet at or above v0.11.0 on the host'
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
  expect_host_commands "$name" ''
  expect_no_task "$name" 'Ask the retirement classifier'
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
  expect_no_ordinary "$name"
done

# R8: the real entry refuses missing installation input BEFORE reporting any
# retirement route. Its empty config must not become the first refusal either.
for mode in check normal; do
  name=r8-missing-source-$mode
  r_plant "$name"
  p "$name" 'rm /usr/bin/billet'
  a "$name" -e billet_version= -e billet_release_channel=
  if [ "$mode" = check ]; then a "$name" --check; fi
  r_run "$name" play-retirement-main
  expect_refused "$name" 'Validate the billet binary source before retirement routing' 'Name the binary with billet_binary_src'
  expect_no_task "$name" 'Report the retirement route'
  expect_no_task "$name" 'Ask the retirement classifier'
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
  expect_host_commands "$name" ''
  expect_no_ordinary "$name"
done

# Legacy verification must discard preparation's non-empty dry-run answerer.
# The ordinary answer would open the sentinel if an unverified path survived.
for kind in symlink other-owner group-writable not-executable verified; do
  name=r9-legacy-$kind
  r_plant "$name"
  p "$name" 'printf "%s\n" "$ROOT/recovery-20260909T120000-12345678" >"$ROOT/active"'
  a "$name" --check -e "billet_gate_legacy_answerer=$kind" -e "billet_binary_src=$bins/wrap-candidate-v0.11.0"
  r_answers "$name" 'control-a:classify:1:dry-run-ordinary.json:0'
  r_run "$name"
  expect_allowed "$name"
  expect_play_task_ran "$name" 'Prove legacy preparation published a dry-run answerer'
  expect_ran "$name" "Examine a legacy claim's read-only answerer"
  if [ "$kind" = verified ]; then
    expect_no_play_task "$name" 'Invalidate the legacy executable after preparation'
    expect_ran "$name" 'Select a verified legacy answerer'
    r_reported "$name" ordinary
    expect_host_commands "$name" 'control-a retire-classify 1;'
    expect_play_task_ran "$name" 'Ordinary convergence sentinel'
  else
    expect_play_task_ran "$name" 'Invalidate the legacy executable after preparation'
    expect_no_task "$name" 'Select a verified legacy answerer'
    r_reported "$name" hold 'This collection needs billet at or above v0.11.0 on the host'
    r_reported "$name" hold 'upgrade the managed binary or converge with an older collection'
    expect_host_commands "$name" ''
    expect_no_task "$name" 'Ask the retirement classifier'
    expect_no_play_task "$name" 'Ordinary convergence sentinel'
  fi
  expect_no_ordinary "$name"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

# An attempted but unanswered classification also reports hold in check mode.
r_plant r9-unanswered
a r9-unanswered --check
r_answers r9-unanswered 'control-a:classify:1:dry-run-ordinary.json:0'
e r9-unanswered 'BILLET_GATE_DROP_ANSWER=retire-classify:1'
r_run r9-unanswered
expect_allowed r9-unanswered
r_reported r9-unanswered hold 'did not answer retirement'
expect_no_ordinary r9-unanswered
expect_no_play_task r9-unanswered 'Ordinary convergence sentinel'
expect_no_task r9-unanswered 'Inspect the transaction claim before recovery'
expect_host_commands r9-unanswered 'control-a retire-classify 1;'

# R10's installed-both request fixture; the desired configuration has no node.
r_plant r10-installed-both
a r10-installed-both -e billet_server_retire=true -e '{"billet_config":{"server":{"state_dir":"/var/lib/billet/server"}}}'
r_answers r10-installed-both 'control-a:classify:1:dry-run-unsupported-variant.json:0'
r_run r10-installed-both
r_held r10-installed-both unsupported-variant

# R10: a retained node in the journal refuses at every supported phase.
for phase in intent archived done; do
  name=r10-journal-$phase
  r_plant "$name"
  r_answers "$name" "control-a:classify:1:dry-run-unsupported-variant-$phase.json:0"
  r_run "$name"
  r_held "$name" unsupported-variant
  r_reported "$name" unsupported-variant 'the journal records a retained-node retirement'
done

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

sections_ran="$sections_ran, retirement routes (R)"
else
  echo "converge guard: retirement routes (R) skipped"
fi

# 5c.d1: new requests, with two node-bearing transports (including the survivor).
if [ "$skip_retirement_request" = 0 ]; then
# The namespace retains the invoker's account; use its non-root primary group
# too, because local prepare refuses either numeric identity resolving to zero.
[ "$invoker_gid" != 0 ] || fail 'retirement requests need a non-root invoker primary group'
r_service_group=$(id -gn "$invoker_name")
# Only the ordinary account/file/service block is forbidden: the explicit early
# service-account import is required on this route.
r_no_ordinary_after_request() {
  local task
  while IFS= read -r task; do
    expect_no_task "$1" "$task"
  done < <(sed -n 's/^[[:space:]]*- name: //p' "$role_tasks/account.yml" "$role_tasks/services.yml")
  expect_no_play_task "$1" 'Ordinary convergence sentinel'
  expect_no_task "$1" 'Inspect the transaction claim before recovery'
}
r_no_collection() {
  "$python" - "$work/cases/$1/calls/index.jsonl" <<'PY'
import json, sys
calls = [json.loads(line) for line in open(sys.argv[1])]
if any(c['command'] in ['release', 'status', 'migrate-endpoint', 'retire-reserve', 'retire-request', 'retire-abandon'] for c in calls):
    sys.exit('a precondition failure collected evidence or touched a reservation')
PY
  expect_no_task "$1" "Read the installed configuration's reservation digest"
  expect_no_task "$1" "Read the retiring host's clock before the first collection delegation"
  expect_no_ordinary "$1"
  expect_no_play_task "$1" 'Ordinary convergence sentinel'
}
r_new() { # case [reserved|adopted]
  local name=$1 outcome=${2:-reserved}
  r_plant "$name"
  r_services "$name" control-a
  # Root runs the real local prepare inside the namespace, recording the
  # invoker as the unprivileged service account without changing host accounts.
  "$python" - "$work/cases/$name/services/control-a.json" "$invoker_name" "$r_service_group" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p))
for unit in d.values():
    unit.update(User=sys.argv[2], Group=sys.argv[3])
with open(p, 'w') as stream:
    json.dump(d, stream)
PY
  ep_plant_config "$name" 127.0.0.1:7717 server-only
  a "$name" -e billet_converge_guard_holder=ci-1 -e billet_gate_new=true -e billet_gate_peer=true \
    -e billet_retirement_survivor_host=control-b \
    -e "billet_service_user=$invoker_name" -e "billet_service_group=$r_service_group" -e '{"billet_adopt_existing_service_account": true}'
  cat >"$work/cases/$name/inventory.yml" <<'INV'
all:
  hosts:
    control-a:
      billet_server_retire: true
    control-b:
      billet_server_retire: false
      node_wire_address: 127.0.0.1:7717
      billet_config:
        server: {listen: '127.0.0.1:7717', state_dir: /var/lib/billet/server, max_vcpu: 8, max_memory: 32GiB}
        node: {name: '{{ inventory_hostname }}', server_addr: '{{ node_wire_address }}', provider: docker}
    node-a:
      billet_server_retire: false
      billet_config_path: /etc/billet/node-a.yaml
      node_wire_address: 127.0.0.1:7719
      billet_config:
        node: {name: '{{ inventory_hostname }}', server_addr: '{{ node_wire_address }}', provider: docker}
INV
  a "$name" -i "$work/cases/$name/inventory.yml"
  e "$name" BILLET_GATE_RETIRE_CLOCK=1
  e "$name" 'BILLET_GATE_RETIRE_ENV=/etc/billet/server.env (ignore_errors=yes)'
  e "$name" "BILLET_GATE_REPORT_ANSWERS=control-a:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-a:status:1:$here/fixtures/rollout-status/retirement-reserved.json:0;control-b:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-b:status:1:$here/fixtures/rollout-status/no-rollout.json:0;control-b:migrate-endpoint:1:$here/fixtures/node-migrate-endpoint/reported-unplanned.json:0;node-a:release:1:$here/fixtures/release-inspect/node-with-bundle.json:0;node-a:migrate-endpoint:1:$here/fixtures/node-migrate-endpoint/reported-planned.json:0"
  r_answers "$name" "control-a:classify:1:dry-run-new-request.json:0;control-a:reserve:1:$outcome.json:0;control-a:request:1:retired-settled.json:0;control-a:abandon:1:abandoned.json:0"
}

# Assert the entire reservation/request argv, all delegated argv, the exact
# document member sets, raw rendering hashes (including the final newline),
# host-specific variable expansion and both positive and forbidden effects.
r_new_document() { # case reserved|adopted
  "$python" - "$work/cases/$1" "$here/fixtures" "$2" <<'PY'
import datetime, hashlib, json, pathlib, re, sys, yaml
case, fixtures = map(pathlib.Path, sys.argv[1:3])
outcome = sys.argv[3]
root = case / 'calls'
calls = [json.loads(line) for line in (root / 'index.jsonl').read_text().splitlines()]
def one(host, command):
    found = [c for c in calls if c['host'] == host and c['command'] == command]
    if len(found) != 1:
        sys.exit(f'{host} {command}: expected one call, got {len(found)}')
    return found[0]
def fixture(command, name):
    return json.loads((fixtures / command / (name + '.json')).read_text())
reserve, request = one('control-a', 'retire-reserve'), one('control-a', 'retire-request')
digest = hashlib.sha256((case / 'installed.yaml.plant').read_bytes()).hexdigest()
common = ['--json', '--run', 'ci-1', '--retiring-host', 'control-a', '--survivor-host', 'control-b', '--installed-sha256', digest]
if reserve['argv'] != ['server', 'retire', '--reserve'] + common + ['--config', '/etc/billet/billet.yaml', '--environment-file', '/etc/billet/server.env']:
    sys.exit('reserve argv differs: ' + repr(reserve['argv']))
expected = ['server', 'retire', '--input', '-'] + common + ['--server-only', '--config', '/etc/billet/billet.yaml']
if outcome == 'reserved':
    expected += ['--reservation-fresh']
expected += ['--shared-address', '192.0.2.10', '--shared-address', '192.0.2.11', '--endpoint-failover-verified', '--report-max-age', '10m', '--environment-file', '/etc/billet/server.env']
# The retiring request flag is host-local inventory; it must not flag its peer.
if request['argv'] != expected:
    sys.exit('request argv differs: ' + repr(request['argv']))
if not request['has_stdin']:
    sys.exit('request has no stdin')
doc = json.loads((root / request['stdin']).read_text())
if set(doc) != {'schema', 'round', 'self', 'survivor', 'nodes', 'desired', 'desired_nodes'} or type(doc['schema']) is not int or doc['schema'] != 1 or doc['desired'] is not None:
    sys.exit('new-request document shape differs')
if set(doc['round']) != {'id', 'started_at'} or doc['round']['id'] != 'ci-1-' + doc['round']['started_at']:
    sys.exit('round binding differs')
def timestamp(s):
    if not isinstance(s, str) or not re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z', s):
        sys.exit('collection time is not the retiring clock\'s nanosecond RFC 3339 answer')
    return datetime.datetime.fromisoformat(s.replace('Z', '+00:00'))
started = timestamp(doc['round']['started_at'])
reserved_at = datetime.datetime.fromisoformat(fixture('server-retire', outcome)['reserved_at'].replace('Z', '+00:00'))
if started < reserved_at:
    sys.exit('round precedes reservation')
if set(doc['nodes']) != {'control-b', 'node-a'} or set(doc['desired_nodes']) != set(doc['nodes']):
    sys.exit('collection omitted a deployment node or collected another host')
if doc['nodes']['control-b'] != doc['survivor']:
    sys.exit('the node-bearing survivor needs its own nodes entry')
for host, envelope, inspect_name, status_name in [
    ('control-a', doc['self'], 'postgres-controller-guarded', 'retirement-reserved'),
    ('control-b', doc['survivor'], 'postgres-controller-guarded', 'no-rollout'),
    ('node-a', doc['nodes']['node-a'], 'node-with-bundle', None),
]:
    if set(envelope) != {'host', 'collected_at', 'inspect', 'status'} or envelope['host'] != host:
        sys.exit('envelope binding differs for ' + host)
    if timestamp(envelope['collected_at']) < started:
        sys.exit('envelope precedes round for ' + host)
    if envelope['inspect'] != fixture('release-inspect', inspect_name):
        sys.exit('inspection came from another host or was rewritten')
    expected_status = fixture('rollout-status', status_name) if status_name else None
    if envelope['status'] != expected_status:
        sys.exit('ledger document differs for ' + host)
    path = '/etc/billet/node-a.yaml' if host == 'node-a' else '/etc/billet/billet.yaml'
    inspection = one(host, 'release')
    if inspection['argv'] != ['release', 'inspect', '--json', '--config', path]:
        sys.exit('inspect argv differs for ' + host)
    if not reserve['sequence'] < inspection['sequence'] < request['sequence']:
        sys.exit('collection is outside the reserved request window')
    if status_name:
        status = one(host, 'status')
        if status['argv'] != ['rollout', 'status', '--json', '--config', path, '--environment-file', '/var/lib/billet-fixture/server.env']:
            sys.exit('status did not use its executing host\'s observed environment')
        if not inspection['sequence'] < status['sequence'] < request['sequence']:
            sys.exit('status collected out of order')
    if host != 'control-a':
        migration = one(host, 'migrate-endpoint')
        if migration['argv'] != ['node', 'migrate-endpoint', '--config', path, '--desired', '-', '--wait', '120s', '--dry-run', '--json']:
            sys.exit('endpoint dry-run argv differs for ' + host)
        rendering = (root / migration['stdin']).read_bytes()
        if not migration['has_stdin'] or not rendering.endswith(b'\n'):
            sys.exit('node did not receive exact configuration bytes')
        parsed = yaml.safe_load(rendering)
        address = '127.0.0.1:7717' if host == 'control-b' else '127.0.0.1:7719'
        if parsed['node'] != dict(name=host, server_addr=address, provider='docker'):
            sys.exit('configuration was not rendered in its own host context')
        ordinary = (case / 'ordinary-render' / (host + '.yaml')).read_bytes()
        if rendering != ordinary:
            sys.exit('collected rendering differs from ordinary role template bytes for ' + host)
        endpoint_name = 'reported-unplanned' if host == 'control-b' else 'reported-planned'
        desired = doc['desired_nodes'][host]
        if desired != {'sha256': hashlib.sha256(ordinary).hexdigest(), 'endpoint': fixture('node-migrate-endpoint', endpoint_name)['to']}:
            sys.exit('desired node evidence was not computed from exact stdin and its own answer')
        if not inspection['sequence'] < migration['sequence'] < request['sequence']:
            sys.exit('endpoint collected out of order')
if any(c['host'] == 'node-a' and c['command'] == 'status' for c in calls):
    sys.exit('node with no ledger was asked for status')
if any(c['command'] in ['retire-abandon', 'retire-complete', 'retire-acknowledge'] for c in calls):
    sys.exit('a settled success ran cleanup or a pending-row helper')
clocks = [c for c in calls if c['command'] == 'collection-clock']
if len(clocks) != 4 or any(c['host'] != 'control-a' or c['argv'] != ['-u', '+%Y-%m-%dT%H:%M:%S.%NZ'] for c in clocks):
    sys.exit('collection did not read the retiring host clock once per round/envelope')
if not reserve['sequence'] < clocks[0]['sequence'] < one('control-a', 'release')['sequence']:
    sys.exit('round clock was not read freshly after reserve and before delegation')
for clock, host in zip(clocks[1:], ['control-a', 'control-b', 'node-a']):
    last_report = one(host, 'status' if host == 'control-a' else 'migrate-endpoint')
    if not last_report['sequence'] < clock['sequence'] < request['sequence']:
        sys.exit('envelope clock was not read after its reports')
# local prepare is a real early import effect, before the reservation.
prepared = [c for c in calls if c['argv'][:2] == ['local', 'prepare']]
if len(prepared) != 1 or prepared[0]['host'] != 'control-a' or prepared[0]['sequence'] >= reserve['sequence']:
    sys.exit('early service-account preparation did not precede reservation')
PY
}

# R2: each static clause fails alone, in both modes. Missing preparation is
# a real-run precondition, separately exercised by R16 below.
for clause in upgrade policy survivor; do
  for mode in normal check; do
    name=r2-$clause-$mode
    r_new "$name"
    case "$clause" in
      upgrade) a "$name" -e billet_gate_binary_upgrade=true; task='Require retirement outside a binary upgrade'; why='inside a binary upgrade' ;;
      policy) a "$name" -e '{"billet_server_should_run":true}'; task='Require a policy that leaves the retiring server stopped'; why='the policy will start the server' ;;
      survivor) a "$name" -e billet_retirement_survivor_host=; task='Require a named survivor in this inventory'; why='no distinct inventory survivor' ;;
    esac
    if [ "$mode" = check ]; then a "$name" --check; fi
    r_run "$name"
    expect_refused "$name" "$task" "Retirement precondition: $why"
    r_no_collection "$name"
    expect_host_commands "$name" 'control-a retire-classify 1;'
  done
done

# R16: both hosts prepare successfully in this play, then the survivor fails
# before the old reset position or loses its real SSH connection there. A third
# case removes it before reinclusion, leaving settled facts to test active-host
# membership independently of the reset. Shared namespace preparation is serial.
cat >"$work/play-retirement-survivor.yml" <<'PLAY'
---
- name: Refuse a survivor removed after its first successful preparation
  hosts: control-a:control-b
  gather_facts: false
  vars:
    billet_exclusion_platform: Linux
    billet_binary_src: ''
    billet_server_should_run: false
  tasks:
    - name: Prepare the retiring transport first
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
      vars:
        billet_allow_converge_from_billet_runner: true
      when: inventory_hostname == 'control-a'
    - name: Prepare the survivor transport first
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
      vars:
        billet_allow_converge_from_billet_runner: true
      when: inventory_hostname == 'control-b'
    - name: Prove the survivor completed its first preparation
      ansible.builtin.assert:
        that:
          - billet_exclusion_settled is sameas true
          - billet_exclusion_held is sameas true
          - billet_exclusion_holder == 'ci-1'
          - billet_upgrade_claim_shape == 'guard'
          - billet_exclusion_answerer == '/usr/bin/billet'
          - billet_exclusion_id | length > 0
      when: inventory_hostname == 'control-b'
    - name: Save the held survivor's run facts
      ansible.builtin.set_fact:
        billet_gate_first_guard:
          held: "{{ billet_exclusion_held }}"
          holder: "{{ billet_exclusion_holder }}"
          id: "{{ billet_exclusion_id }}"
          shape: "{{ billet_upgrade_claim_shape }}"
          interrupted: "{{ billet_interrupted_upgrade }}"
          recovery: "{{ billet_upgrade_recovery_dir }}"
      when: inventory_hostname == 'control-b'
    - name: Remove the survivor before a second inclusion
      ansible.builtin.fail:
        msg: The survivor failed after preparation and before reinclusion.
      when: inventory_hostname == 'control-b' and billet_gate_survivor_failure == 'inactive'
    - name: Lose the survivor's connection before its second preparation
      ansible.builtin.set_fact:
        ansible_connection: ssh
        ansible_host: 127.0.0.1
        ansible_port: 0
        ansible_connect_timeout: 1
        ansible_ssh_common_args: '-o BatchMode=yes -o ConnectionAttempts=1'
      when: inventory_hostname == 'control-b' and billet_gate_survivor_failure == 'unreachable'
    - name: Prepare the survivor a second time
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
      vars:
        billet_allow_converge_from_billet_runner: "{{ billet_gate_survivor_failure != 'failed' }}"
      when: inventory_hostname == 'control-b'
    - name: Prove the removed survivor's publication and held identity
      vars:
        peer: "{{ hostvars['control-b'] }}"
      ansible.builtin.assert:
        that:
          - "'control-b' not in ansible_play_hosts"
          - "'control-b' in ansible_play_hosts_all"
          - peer.billet_exclusion_settled is sameas (billet_gate_survivor_failure == 'inactive')
          - billet_gate_survivor_failure == 'inactive' or peer.billet_exclusion_answerer == ''
          - billet_gate_survivor_failure == 'inactive' or peer.billet_exclusion_answered_now is sameas false
          - billet_gate_survivor_failure == 'inactive' or peer.billet_exclusion_answerer_version == {'type':'unreadable'}
          - peer.billet_exclusion_held == peer.billet_gate_first_guard.held
          - peer.billet_exclusion_holder == peer.billet_gate_first_guard.holder
          - peer.billet_exclusion_id == peer.billet_gate_first_guard.id
          - peer.billet_upgrade_claim_shape == peer.billet_gate_first_guard.shape
          - peer.billet_interrupted_upgrade == peer.billet_gate_first_guard.interrupted
          - peer.billet_upgrade_recovery_dir == peer.billet_gate_first_guard.recovery
      when: inventory_hostname == 'control-a'
    - name: Route the retiring host with the failed survivor still in hostvars
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: retirement
      when: inventory_hostname == 'control-a'
PLAY
for failure in failed unreachable inactive; do
  name=r16-second-$failure
  r_new "$name"
  e "$name" RUNNER_NAME=billet-survivor-failure
  a "$name" -e "billet_gate_survivor_failure=$failure"
  r_run "$name" play-retirement-survivor
  case "$failure" in
    failed) task='Refuse a converge driven from a billet-managed runner'; why='runner billet itself manages' ;;
    unreachable) task='Inspect the managed binary and the upgrade root'; why='UNREACHABLE!' ;;
    inactive) task='Remove the survivor before a second inclusion'; why='before reinclusion' ;;
  esac
  expect_refused "$name" "$task" "$why"
  "$python" - "$work/cases/$name/out" <<'PYPREPARED'
import pathlib, sys
text = pathlib.Path(sys.argv[1]).read_text()
for task, host in [("Prove the survivor completed its first preparation", 'control-b'),
                   ("Prove the removed survivor's publication and held identity", 'control-a')]:
    parts = text.split('TASK [' + task + ']')
    if len(parts) != 2 or ('ok: [' + host + ']') not in parts[1].split('TASK [', 1)[0]:
        sys.exit('R16 did not prove preparation/publication on ' + host + ': ' + task)
PYPREPARED
  expect_final "$name" 'Retirement precondition: survivor control-b is no longer active in this play (failure, unreachability, or an intentional end of that host).' 'Resolve the cause, then converge the survivor in the same play as the retiring host.'
  r_no_collection "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;'
done

# R16: successful preparation in an earlier play leaves settled hostvars but
# supplies no evidence that the survivor stayed healthy outside this play.
cat >"$work/play-retirement-earlier-survivor.yml" <<'PLAY'
---
- name: Prepare the survivor in an earlier play
  hosts: control-b
  gather_facts: false
  vars:
    billet_exclusion_platform: Linux
    billet_binary_src: ''
  tasks:
    - name: Prepare the survivor transport
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
- name: Attempt retirement in a separate play
  hosts: control-a
  gather_facts: false
  vars:
    billet_exclusion_platform: Linux
    billet_binary_src: ''
    billet_server_should_run: false
  tasks:
    - name: Prepare the retiring transport
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
    - name: Prove the earlier survivor's settled facts remain outside this play
      vars:
        peer: "{{ hostvars['control-b'] }}"
      ansible.builtin.assert:
        that:
          - "'control-b' not in ansible_play_hosts_all"
          - "'control-b' not in ansible_play_hosts"
          - peer.billet_exclusion_settled is sameas true
          - peer.billet_exclusion_held is sameas true
          - peer.billet_exclusion_holder == billet_exclusion_holder
          - peer.billet_upgrade_claim_shape == 'guard'
          - peer.billet_exclusion_answerer == '/usr/bin/billet'
          - peer.billet_exclusion_id | length > 0
    - name: Route retirement with only earlier-play survivor evidence
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: retirement
PLAY
r_new r16-earlier-play
r_run r16-earlier-play play-retirement-earlier-survivor
expect_refused r16-earlier-play "Require the survivor in the retiring host's play" \
  'Retirement precondition: survivor control-b is outside this play.' \
  'Preparation in an earlier play cannot prove the survivor stayed healthy; converge the survivor in the same play as the retiring host.'
expect_play_task_ran r16-earlier-play "Prove the earlier survivor's settled facts remain outside this play"
r_no_collection r16-earlier-play
expect_host_commands r16-earlier-play 'control-a retire-classify 1;'

# R16: the other clauses remain true, so settled alone, a skipped settlement,
# another holder and a binary pointer each have an independent witness.
for clause in unsettled holder pointer unheld; do
  name=r16-$clause
  r_new "$name"
  a "$name" -e "billet_gate_peer_clause=$clause"
  r_run "$name"
  expect_refused "$name" "Require this converge's settled survivor guard" 'needs a settled, held, pointer-free guard'
  r_no_collection "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;'
done
r_new r2-unprepared
# A real run has no published survivor guard at all; setting an answerer is
# insufficient. The check-mode partner reports the prerequisite and succeeds.
a r2-unprepared -e billet_gate_peer=false
r_run r2-unprepared
expect_refused r2-unprepared "Require this converge's settled survivor guard" 'needs a settled, held, pointer-free guard'
r_no_collection r2-unprepared
r_new r2-preview
a r2-preview --check -e billet_gate_peer=false
r_run r2-preview
expect_allowed r2-preview
expect_ran r2-preview 'Report the prospective new retirement'
expect_host_commands r2-preview 'control-a retire-classify 1;'
r_no_collection r2-preview
grep -qF 'A real run needs that survivor prepared' "$work/cases/r2-preview/out" || fail 'r2-preview: missing survivor preparation report'

# R19 and the end-to-end success: the fake answers without judging the flag,
# and exact argv assertions kill passing it after adoption or omitting it fresh.
for outcome in reserved adopted; do
  name=r19-$outcome
  r_new "$name" "$outcome"
  mkdir -p "$work/cases/$name/ordinary-render"
  a "$name" -e billet_gate_capture_rendering=true -e "billet_gate_ordinary_template=$role_tasks/../templates/billet.yaml.j2" -e "billet_gate_render_dir=$work/cases/$name/ordinary-render"
  # Per-host retirement policy must remain distinct in hostvars.
  a "$name" -e '{"billet_retirement_shared_addresses":["192.0.2.10","192.0.2.11"],"billet_retirement_endpoint_failover_verified":true}'
  r_run "$name"
  expect_allowed "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-request 1;'
  expect_ran "$name" "Prepare this host's authority exclusion"
  expect_ran "$name" "Keep the new retirement's result"
  r_no_ordinary_after_request "$name"
  r_new_document "$name" "$outcome"
done

# R19 mutation: override only collection's rendering, retaining a server map
# but dropping one member. The ordinary Ansible render remains independent.
r_new r19-dropped-server-member
mkdir -p "$work/cases/r19-dropped-server-member/ordinary-render"
a r19-dropped-server-member -e billet_gate_capture_rendering=true -e "billet_gate_ordinary_template=$role_tasks/../templates/billet.yaml.j2" -e "billet_gate_render_dir=$work/cases/r19-dropped-server-member/ordinary-render"
cat >"$work/cases/r19-dropped-server-member/mutation.yml" <<'VARS'
billet_retirement_shared_addresses: [192.0.2.10, 192.0.2.11]
billet_retirement_endpoint_failover_verified: true
billet_retirement_collect_rendering: >-
  {{ lookup('ansible.builtin.template', 'billet.yaml.j2',
            template_vars={'billet_config':
              (billet_retirement_collect_vars.billet_config | combine({'server':
                billet_retirement_collect_vars.billet_config.server | dict2items |
                rejectattr('key', 'equalto', 'max_vcpu') | items2dict}))
              if billet_retirement_collect_host == 'control-b'
              else billet_retirement_collect_vars.billet_config}) }}
VARS
a r19-dropped-server-member -e "@$work/cases/r19-dropped-server-member/mutation.yml"
r_run r19-dropped-server-member
expect_allowed r19-dropped-server-member
if r_new_document r19-dropped-server-member reserved >"$work/cases/r19-dropped-server-member/mutation-out" 2>&1; then
  fail 'R19 accepted a collected rendering missing a server member'
fi
grep -qFx 'collected rendering differs from ordinary role template bytes for control-b' "$work/cases/r19-dropped-server-member/mutation-out" || fail 'R19 mutation failed for a reason other than ordinary-render byte equality' "$work/cases/r19-dropped-server-member/mutation-out"

# R13: node-a's connection plugin really cannot connect, after the row was
# reserved and the survivor was collected. No module exit stands for this.
r_new r13-unreachable
e r13-unreachable BILLET_GATE_RETIRE_OBLIGATION=/var/lib/billet/gate-reservation
cat >>"$work/cases/r13-unreachable/inventory.yml" <<'INV'
      ansible_connection: ssh
      ansible_host: 127.0.0.1
      ansible_port: 0
      ansible_connect_timeout: 1
      ansible_ssh_common_args: '-o BatchMode=yes -o ConnectionAttempts=1'
INV
r_run r13-unreachable
expect_refused r13-unreachable "Collect the host's release inspection" 'node-a release inspection was unsuccessful or unreachable'
expect_final r13-unreachable 'The reservation was abandoned.'
expect_host_commands r13-unreachable 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-abandon 1;'
r_no_ordinary_after_request r13-unreachable
expect_path_absent r13-unreachable /var/lib/billet/gate-reservation
"$python" - "$work/cases/r13-unreachable" <<'PY'
import json, pathlib, sys
case = pathlib.Path(sys.argv[1])
text = (case / 'out').read_text()
if 'UNREACHABLE!' not in text or 'node-a' not in text:
    sys.exit('R13 did not establish connection-layer UNREACHABLE')
header = 'TASK [junioryono.billet.host : Require a successful delegated release inspection]'
blocks = text.split(header)[1:]
failures = [b.split('TASK [', 1)[0] for b in blocks if 'fatal: [control-a]:' in b.split('TASK [', 1)[0]]
if len(failures) != 1 or 'node-a release inspection was unsuccessful or unreachable' not in failures[0] or 'UNREACHABLE!' in failures[0]:
    sys.exit('R13 did not convert unreachable into an ordinary retiring-host assertion failure')
calls = [json.loads(line) for line in (case / 'calls/index.jsonl').read_text().splitlines()]
if any(c['host'] == 'node-a' for c in calls):
    sys.exit('an unreachable transport ran a module')
if sum(c['host'] == 'control-b' and c['command'] == 'release' for c in calls) != 1:
    sys.exit('R13 failed before reaching delegated collection')
abandon = [c for c in calls if c['command'] == 'retire-abandon']
if len(abandon) != 1 or abandon[0]['argv'] != ['server', 'retire', '--abandon-reservation', '--json', '--run', 'ci-1', '--retiring-host', 'control-a', '--config', '/etc/billet/billet.yaml', '--environment-file', '/etc/billet/server.env']:
    sys.exit('R13 cleanup did not carry exact local abandonment argv')
PY

# Missing desired rendering, missing prepared answerer and failed status may
# not silently remove a node or survivor from the collected document.
for clause in rendering answerer status; do
  name=r-collection-$clause
  r_new "$name"
  case "$clause" in
    rendering)
      "$python" - "$work/cases/$name/inventory.yml" <<'PYRENDER'
import pathlib, sys, yaml
p = pathlib.Path(sys.argv[1])
d = yaml.safe_load(p.read_text())
d['all']['hosts']['node-a'].update(billet_config={}, billet_enable_node=True)
p.write_text(yaml.safe_dump(d))
PYRENDER
      task="Require this node's complete desired configuration"; why='node node-a has no complete desired rendering' ;;
    answerer)
      a "$name" -e billet_gate_no_answerer=true
      task='Require a prepared answerer for every collected host'; why='node-a has no prepared answerer' ;;
    status)
      e "$name" "BILLET_GATE_REPORT_ANSWERS=control-a:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-a:status:1:$here/fixtures/rollout-status/retirement-reserved.json:0;control-b:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-b:status:1:$here/fixtures/rollout-status/no-rollout.json:3"
      task='Require a successful delegated ledger status'; why='control-b ledger status was unsuccessful or unreachable' ;;
  esac
  r_run "$name"
  expect_refused "$name" "$task" "$why"
  expect_final "$name" 'The reservation was abandoned.'
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-abandon 1;'
  r_no_ordinary_after_request "$name"
done

# Binding corruption stays case-local; the corpus always comes from Go.
for member in run retiring survivor transition_id; do
  name=r-reservation-binding-$member
  r_new "$name" adopted
  mkdir -p "$work/cases/$name/answers"
  cp "$work/retire-fixtures/"*.json "$work/cases/$name/answers/"
  "$python" - "$work/cases/$name/answers" "$member" <<'PY'
import json, pathlib, sys
root, member = pathlib.Path(sys.argv[1]), sys.argv[2]
p = root / 'adopted.json'
d = json.loads(p.read_text())
d[member] = 'fedcba9876543210fedcba9876543210' if member == 'transition_id' else 'another'
p.write_text(json.dumps(d))
# The classifier's reservation observation supplies the old transition id.
p = root / 'dry-run-new-request.json'
d = json.loads(p.read_text())
a = json.loads((root / 'reserved.json').read_text())
d.update(row={k: a[k] for k in ['retiring', 'survivor', 'run', 'transition_id', 'reserved_at']}, row_fact='reserved-mine', dispatch='adopt')
d['row']['state'] = 'reserved'
p.write_text(json.dumps(d))
PY
  e "$name" "BILLET_GATE_RETIRE_FIXTURES=$work/cases/$name/answers"
  r_run "$name"
  expect_refused "$name" 'Bind the reservation before collecting anything' 'reservation binding disagrees'
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-abandon 1;'
  expect_no_task "$name" "Read the retiring host's clock before the first collection delegation"
  r_no_ordinary_after_request "$name"
done

# R20: the classifier sees the actual damage AFTER preparation; it may not
# enter the early account import, settle it, or remove the interrupted rewrite.
cat >"$work/retirement-own-guard.py" <<'PY'
import json, pathlib, sys
p = pathlib.Path('/var/lib/billet/upgrades/active/guard.json')
d = json.loads(p.read_text())
if d.get('preparing', False) or d['holder'] != 'ci-1':
    sys.exit('R20 must start with this converge\'s settled guard')
if sys.argv[1] == 'preparing':
    d['preparing'] = True
    d['token'] = 'a' * 32
    p.write_text(json.dumps(d))
elif sys.argv[1] == 'interrupted-rewrite':
    p.with_name('guard.json.tmp').write_text('interrupted guard record')
else:
    sys.exit('unknown own guard damage')
PY
for damage in preparing interrupted-rewrite; do
  name=r20-$damage
  r_new "$name"
  a "$name" -e "billet_gate_own_guard=$damage" -e "billet_gate_own_guard_script=$work/retirement-own-guard.py"
  r_answers "$name" "control-a:classify:1:dry-run-hold-$damage.json:0"
  r_run "$name"
  r_held "$name" hold
  r_reported "$name" hold "${damage//-/ }"
  r_no_collection "$name"
  expect_play_task_ran "$name" 'Inject an unfinished own guard after preparation'
  if [ "$damage" = preparing ]; then
    expect_state "$name" record_preparing True
  else
    expect_state "$name" record_preparing False
    expect_state "$name" record_tmp file
  fi
done

# R17: the fake establishes the marker-before-journal window at the request;
# its cleanup effect is gated on an actual abandon invocation, never on failure
# alone. The unknown-intent-journal answer must be produced/harvested by Go.
cat >"$work/retirement-window.py" <<'PY'
import json, pathlib, sys
root = pathlib.Path('/var/lib/billet')
record = root / 'upgrades/active/guard.json'
d = json.loads(record.read_text())
if (root / 'retired/journal.json').exists():
    sys.exit('R17 requires the journal still absent')
marker = {'kind': 'retirement', 'id': '0123456789abcdef0123456789abcdef'}
if sys.argv[1] == 'mark':
    if 'transition' in d:
        sys.exit('R17 started with a marker')
    d['transition'] = marker
elif sys.argv[1] == 'abandon':
    if d.get('transition') != marker:
        sys.exit('R17 abandonment did not see the request\'s marker')
    del d['transition']
else:
    sys.exit('unknown retirement window action')
record.write_text(json.dumps(d))
PY
# A per-case executable wrapper around the already-recording managed wrapper
# plants the marker and observes cleanup; preparation still runs the real Go
# guard. It has no retirement answer of its own.
cat >"$work/retirement-window-wrapper" <<WRAP
#!/bin/bash
set -eu
case "\$*" in
  'server retire --input '*)
    '$python' '$work/retirement-window.py' mark
    if [ "\${BILLET_GATE_WINDOW_CLEANUP:-}" = unreachable ]; then touch /var/lib/billet/gate-disconnect; fi ;;
  'server retire --abandon-reservation '*)
    if [ "\${BILLET_GATE_WINDOW_CLEANUP:-}" = success ]; then '$python' '$work/retirement-window.py' abandon; fi ;;
esac
exec /usr/bin/billet.window-backing "\$@"
WRAP
chmod 0755 "$work/retirement-window-wrapper"
for cleanup in success refused unanswered unreachable released; do
  name=r17-$cleanup
  r_new "$name"
  request_fixture=unknown-intent-journal
  request_exit=3
  abandon_fixture=abandoned-marker-cleared
  abandon_exit=0
  if [ "$cleanup" = released ]; then
    request_fixture=refused-request-released
    request_exit=2
  else
    p "$name" "mv /usr/bin/billet /usr/bin/billet.window-backing; cp '$work/retirement-window-wrapper' /usr/bin/billet"
  fi
  if [ "$cleanup" = refused ]; then abandon_fixture=refused-abandon-journal; abandon_exit=2; fi
  r_answers "$name" "control-a:classify:1:dry-run-new-request.json:0;control-a:reserve:1:reserved.json:0;control-a:request:1:$request_fixture.json:$request_exit;control-a:abandon:1:$abandon_fixture.json:$abandon_exit"
  e "$name" "BILLET_GATE_WINDOW_CLEANUP=$cleanup"
  if [ "$cleanup" = unreachable ]; then
    "$python" - "$work/cases/$name/inventory.yml" <<'PYCONNECTION'
import pathlib, sys, yaml
p = pathlib.Path(sys.argv[1])
d = yaml.safe_load(p.read_text())
d['all']['hosts']['control-a'].update(
    ansible_connection="{{ 'ssh' if lookup('ansible.builtin.fileglob', '/var/lib/billet/gate-disconnect') | length > 0 else 'local' }}",
    ansible_host='127.0.0.1', ansible_port=0, ansible_connect_timeout=1,
    ansible_ssh_common_args='-o BatchMode=yes -o ConnectionAttempts=1')
p.write_text(yaml.safe_dump(d))
PYCONNECTION
  fi
  if [ "$cleanup" = unanswered ]; then e "$name" 'BILLET_GATE_DROP_ANSWER=retire-abandon:1'; fi
  post "$name" "cp /var/lib/billet/upgrades/active/guard.json '$work/cases/$name/guard-after.json'"
  r_run "$name"
  "$python" - "$work/cases/$name/guard-after.json" "$cleanup" <<'PYMARKER'
import json, sys
d = json.load(open(sys.argv[1]))
kept = sys.argv[2] in ['refused', 'unanswered', 'unreachable']
if ('transition' in d) != kept:
    sys.exit('R17 left the wrong marker state after cleanup')
if kept and d['transition'] != {'kind': 'retirement', 'id': '0123456789abcdef0123456789abcdef'}:
    sys.exit('R17 changed an unproved marker')
PYMARKER
  expect_refused "$name" "Refuse the retirement's answer" "was $([ "$request_exit" = 3 ] && echo unknown || echo refused)"
  # Compare the decoded final diagnostic with the producer's whole reason;
  # a common word such as journal could come from cleanup alone.
  final_fatal "$name" >"$work/cases/$name/final-failure"
  "$python" - "$work/cases/$name/final-failure" "$work/retire-fixtures/$request_fixture.json" "$cleanup" "$work/retire-fixtures/$abandon_fixture.json" "$work/cases/$name/out" <<'PYWHY'
import json, pathlib, sys
text = pathlib.Path(sys.argv[1]).read_text()
messages = [json.loads(line.strip().removeprefix('"msg": ').removesuffix(','))
            for line in text.splitlines() if line.strip().startswith('"msg": ')]
if len(messages) != 1:
    sys.exit('R17 final failure did not contain one decoded diagnostic')
message = messages[0]
original = json.loads(pathlib.Path(sys.argv[2]).read_text())['why']
if original not in message:
    sys.exit('R17 lost the original request reason during cleanup')
if sys.argv[3] == 'refused':
    cleanup = json.loads(pathlib.Path(sys.argv[4]).read_text())['why']
    if cleanup not in message or message.index(original) >= message.index(cleanup):
        sys.exit('R17 did not report cleanup refusal after the original reason')
if sys.argv[3] in ['unanswered', 'unreachable'] and "did not answer retirement's abandon call" not in message:
    sys.exit('R17 did not report unanswered cleanup beside the original reason')
if sys.argv[3] == 'unreachable':
    output = pathlib.Path(sys.argv[5]).read_text()
    parts = output.split('TASK [junioryono.billet.host : Ask abandonment to discharge the reservation]')
    if len(parts) != 2 or 'fatal: [control-a]: UNREACHABLE!' not in parts[1].split('TASK [', 1)[0]:
        sys.exit('R17 did not lose the connection specifically at abandonment')
    for clause in ['reservation for retiring host control-a', 'survivor control-b', 'run ci-1',
                   'transition 0123456789abcdef0123456789abcdef', 'is not proved released']:
        if clause not in message:
            sys.exit('R17 did not name its outstanding reservation: ' + clause)
PYWHY
  r_no_ordinary_after_request "$name"
  expect_path_absent "$name" /var/lib/billet/retired/journal.json
  if [ "$cleanup" = released ]; then
    expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-request 1;'
    expect_final "$name" 'reservation released; abandonment was skipped'
  else
    if [ "$cleanup" = unreachable ]; then
      expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-request 1;'
      grep -qF 'UNREACHABLE!' "$work/cases/$name/out" || fail "$name: abandonment did not lose its connection"
      expect_ran "$name" 'Keep the cleanup refusal beside the original failure'
    else
      expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-request 1;control-a retire-abandon 1;'
    fi
    if [ "$cleanup" = success ]; then
      expect_final "$name" 'The reservation was abandoned.'
    else
      expect_final "$name" 'Cleanup remains outstanding:'
    fi
    expect_final "$name" 'journal'
  fi
done
sections_ran="$sections_ran, retirement requests (R)"
else
  echo "converge guard: retirement requests (R) skipped"
fi
