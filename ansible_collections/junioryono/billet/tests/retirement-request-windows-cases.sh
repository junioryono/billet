#!/usr/bin/env bash
# Sourced by retirement-cases.sh inside the request-windows section.

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
  PYTHONPATH="$here" "$python" -B - "$work/cases/$name/final-failure" "$work/retire-fixtures/$request_fixture.json" "$cleanup" "$work/retire-fixtures/$abandon_fixture.json" "$work/cases/$name/out" <<'PYWHY'
import json, pathlib, sys
from callback_result import callback_message
text = pathlib.Path(sys.argv[1]).read_text()
message = callback_message(text, pathlib.Path(sys.argv[1]).parent.name + ': final fatal task', failed=True)
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
# R6: R5 already proves the continuation entry and unprepared-survivor report.
# This adds the new-request entry, including the local-only post-ack document
# and observable marker/settlement effects at each of the three tail calls.
cat >"$work/retirement-tail-effects.py" <<'PYTAILSTATE'
import json, pathlib, sys
root = pathlib.Path('/var/lib/billet')
record = root / 'upgrades/active/guard.json'
guard = json.loads(record.read_text())
path = root / 'gate-tail-state.json'
state = json.loads(path.read_text()) if path.exists() else dict(row='reserved', acknowledged=False, settled=False, events=[])
action = sys.argv[1]
if action == 'request':
    if any(e['action'] == 'request' for e in state['events']):
        guard.pop('transition', None)
        state['settled'] = True
    else:
        guard['transition'] = {'kind': 'retirement', 'id': '0123456789abcdef0123456789abcdef'}
        state['row'] = 'pending'
elif action == 'complete':
    state['row'] = 'done'
elif action == 'acknowledge':
    state['acknowledged'] = True
else:
    sys.exit('unknown tail effect')
state['events'].append(dict(action=action, row=state['row'], acknowledged=state['acknowledged'], settled=state['settled'], marker=guard.get('transition')))
record.write_text(json.dumps(guard))
path.write_text(json.dumps(state))
PYTAILSTATE
cat >"$work/retirement-tail-wrapper" <<WRAP
#!/bin/bash
set -eu
status=0
/usr/bin/billet.tail-backing "\$@" || status=\$?
if [ "\$status" = 0 ]; then
  case "\$*" in
    'server retire --input '*) '$python' '$work/retirement-tail-effects.py' request ;;
    'server retire --complete-row '*) '$python' '$work/retirement-tail-effects.py' complete ;;
    'server retire --acknowledge-row '*) '$python' '$work/retirement-tail-effects.py' acknowledge ;;
  esac
fi
exit "\$status"
WRAP
chmod 0755 "$work/retirement-tail-wrapper"
r_new r6-request-handoff
p r6-request-handoff "mv /usr/bin/billet /usr/bin/billet.tail-backing; cp '$work/retirement-tail-wrapper' /usr/bin/billet"
r_answers r6-request-handoff 'control-a:classify:1:dry-run-new-request.json:0;control-a:reserve:1:reserved.json:0;control-a:request:1:retired-pending.json:0;control-b:complete:1:completed.json:0;control-a:acknowledge:1:acknowledged.json:0;control-a:request:2:retired-after-survivor-ack.json:0'
post r6-request-handoff "cp /var/lib/billet/gate-tail-state.json '$work/cases/r6-request-handoff/tail-after.json'; cp /var/lib/billet/upgrades/active/guard.json '$work/cases/r6-request-handoff/guard-after.json'"
r_run r6-request-handoff
expect_allowed r6-request-handoff
r_no_ordinary_after_request r6-request-handoff
expect_host_commands r6-request-handoff 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-request 1;control-b retire-complete 1;control-a retire-acknowledge 1;control-a retire-request 2;'
expect_no_task r6-request-handoff 'Report a row obligation whose survivor this converge did not prepare'
# expect_ran reads ok/changed results; include_tasks reports included instead.
# Assert the settlement command inside the include, after the exact call order.
expect_ran r6-request-handoff "Continue through the journal's recorded survivor"
PYTHONPATH="$here" "$python" -B - "$work/cases/r6-request-handoff" "$work/retire-fixtures" <<'PYTAIL'
import json, pathlib, sys
from callback_result import callback_message
case, fixtures = map(pathlib.Path, sys.argv[1:])
root = case / 'calls'
calls = [json.loads(line) for line in (root / 'index.jsonl').read_text().splitlines()]
retire = [c for c in calls if c['command'].startswith('retire-')]
request, complete, ack, continuation = retire[2:]
def document(call):
    if not call['has_stdin']:
        sys.exit('a pending-row call omitted its document')
    return json.loads((root / call['stdin']).read_text())
if complete['argv'] != ['server', 'retire', '--complete-row', '--json', '--run', 'ci-1', '--as-host', 'control-b', '--completion', '-', '--config', '/etc/billet/billet.yaml', '--environment-file', '/etc/billet/server.env']:
    sys.exit('pending completion argv differs')
if ack['argv'] != ['server', 'retire', '--acknowledge-row', '--json', '--run', 'ci-1', '--retiring-host', 'control-a', '--answer', '-', '--config', '/etc/billet/billet.yaml']:
    sys.exit('pending acknowledgement argv differs')
if continuation['argv'] != ['server', 'retire', '--input', '-', '--json', '--run', 'ci-1', '--retiring-host', 'control-a', '--survivor-host', 'control-b', '--config', '/etc/billet/billet.yaml', '--environment-file', '/etc/billet/server.env']:
    sys.exit('post-acknowledgement continuation argv differs')
if document(complete) != json.loads((fixtures / 'retired-pending.json').read_text())['completion']:
    sys.exit('the survivor did not receive the pending completion')
if document(ack) != json.loads((fixtures / 'completed.json').read_text()):
    sys.exit('acknowledgement did not carry the survivor answer')
original = document(request)
expected = dict(schema=1, round=original['round'], self=dict(original['self'], status=None), survivor=None, nodes={}, desired=None)
if document(continuation) != expected:
    sys.exit('the final continuation did not keep exactly the collected local envelope')
if any(c['sequence'] > request['sequence'] and c['command'] in ['release', 'status', 'migrate-endpoint', 'collection-clock', 'retire-reserve', 'retire-abandon'] for c in calls):
    sys.exit('the pending tail recollected, reserved or abandoned')
marker = dict(kind='retirement', id='0123456789abcdef0123456789abcdef')
expected_events = [dict(action=a, row=r, acknowledged=k, settled=s, marker=m) for a, r, k, s, m in [
    ('request', 'pending', False, False, marker),
    ('complete', 'done', False, False, marker),
    ('acknowledge', 'done', True, False, marker),
    ('request', 'done', True, True, None),
]]
state = json.loads((case / 'tail-after.json').read_text())
if state != dict(row='done', acknowledged=True, settled=True, events=expected_events):
    sys.exit('the three tail calls did not preserve the marker until final settlement: ' + repr(state))
if 'transition' in json.loads((case / 'guard-after.json').read_text()):
    sys.exit('the final continuation left the marker')
text = (case / 'out').read_text()
parts = text.split("TASK [junioryono.billet.host : Report the new retirement's settlement or obligation]")
if len(parts) != 2:
    sys.exit('the request did not report its final settlement')
message = callback_message(parts[1].split('TASK [', 1)[0], case.name + ': settlement')
if not message.startswith('Retirement is locally done; settled=True.') or 'Ordinary host tasks remain bypassed.' not in message:
    sys.exit('the request reported its pre-acknowledgement state: ' + message)
PYTAIL

# An older marked guard can stop preparation before retirement is dispatched.
# Keep that earlier obligation and do not claim the cancel route was reached.
r_plant r12-earlier-guard
p r12-earlier-guard "plant_guard ci-0 /usr/bin/billet; '$python' '$work/retirement-window.py' mark"
a r12-earlier-guard -e billet_converge_guard_holder=ci-1 -e billet_gate_cancel=true
post r12-earlier-guard "cp /var/lib/billet/upgrades/active/guard.json '$work/cases/r12-earlier-guard/guard-after.json'"
r_run r12-earlier-guard
expect_refused r12-earlier-guard "Refuse the preparation's answer" 'held by ci-0'
expect_final r12-earlier-guard 'old-driver-stopped'
expect_host_commands r12-earlier-guard ''
expect_no_task r12-earlier-guard 'Ask the retirement classifier'
expect_no_task r12-earlier-guard 'Report the retirement route'
expect_no_task r12-earlier-guard 'Report the prospective reservation cancellation'
expect_no_play_task r12-earlier-guard 'Ordinary convergence sentinel'
expect_no_ordinary r12-earlier-guard
expect_path_absent r12-earlier-guard /var/lib/billet/retired/journal.json
"$python" - "$work/cases/r12-earlier-guard/guard-after.json" <<'PYGUARD'
import json, sys
record = json.load(open(sys.argv[1]))
if record['holder'] != 'ci-0' or record.get('transition') != dict(kind='retirement', id='0123456789abcdef0123456789abcdef'):
    sys.exit('preparation changed the older guard or abandoned its marker')
PYGUARD

