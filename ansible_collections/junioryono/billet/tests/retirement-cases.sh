#!/usr/bin/env bash
# Sourced by section R after its namespace runner and committed corpus exist.
# The isolated entry proves routing; the boundary invokes the real main.yml.
"$python" "$here/executable_version_check.py"

cat >"$work/play-retirement.yml" <<'PLAY'
---
- name: Exercise the retirement entry after real preparation
  hosts: "{{ billet_gate_retirement_host | default('control-a') }}"
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
          a "$name" -e "billet_enable_server=$policy" -e "billet_server_retire=$requested"
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
