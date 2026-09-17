#!/usr/bin/env bash
# Sourced after the settled fixture cases; every command uses the recording fake.
cp "$here/retirement-activation.yml" "$work/play-retirement-activation.yml"
for kind in active inactive failed entry-refused entry-unknown node-refused node-unknown closing-refused check frozen-no-flag frozen-with-flag serverless-no-flag; do
  name=r3c-$kind
  if [ "$kind" = active ]; then name=r10-journal-done; fi
  r_plant "$name"
  mkdir -p "$work/cases/$name/answers"
  "$python" - "$here/fixtures" "$work/cases/$name" "$kind" <<'PYCASES'
import json, pathlib, sys
fixtures, case, kind = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3]
root = case / 'answers'
def read(group, name):
    return json.loads((fixtures / group / (name + '.json')).read_text())
classifier = read('server-retire', 'dry-run-continue-retained-done')
classifier['journal']['transition_id'] = classifier['row']['transition_id'] = '2' * 32
entry = read('retire-settled-entry', kind if kind in ['inactive', 'failed'] else 'active')
node = read('retire-node-config', kind if kind in ['inactive', 'failed'] else 'active')
closing = read('retire-settled-closing', 'active')
entry_rc = node_rc = closing_rc = 0
if kind == 'entry-refused':
    entry, entry_rc = read('retire-settled-entry', 'activating'), 2
elif kind == 'entry-unknown':
    entry, entry_rc = read('retire-settled-entry', 'unknown-job'), 3
elif kind in ['node-refused', 'node-unknown']:
    # Generic retirement refusals are the node-config command's refusal shape.
    node = read('server-retire', 'refused-input' if kind == 'node-refused' else 'unknown-marker')
    node_rc = 2 if kind == 'node-refused' else 3
elif kind == 'closing-refused':
    closing, closing_rc = read('retire-settled-closing', 'inactive'), 2
for name, answer in [('classifier', classifier), ('entry', entry), ('closing', closing)]:
    (root / (name + '.json')).write_text(json.dumps(answer))
(case / 'activation-vars.json').write_text(json.dumps(dict(
    billet_gate_frozen=kind in ['frozen-no-flag', 'frozen-with-flag'],
    billet_server_retire=kind == 'frozen-with-flag',
    billet_gate_node_answer=node, billet_gate_answer_root=str(root))))
specs = f'control-a:classify:1:classifier.json:0;control-a:settled-entry:1:entry.json:{entry_rc};control-a:node-config:1:node-config.json:{node_rc};control-a:settled-closing:1:closing.json:{closing_rc}'
(case / 'answers-spec').write_text(specs)
(case / 'expected.json').write_text(json.dumps(dict(entry=entry, node=node, closing=closing)))
PYCASES
  a "$name" -e "@$work/cases/$name/activation-vars.json"
  e "$name" "BILLET_GATE_RETIRE_FIXTURES=$work/cases/$name/answers"
  r_answers "$name" "$(cat "$work/cases/$name/answers-spec")"
  if [ "$kind" = check ]; then a "$name" --check; fi
  r_run "$name" play-retirement-activation
  case "$kind" in
    entry-refused|entry-unknown)
      expect_refused "$name" "Refuse the settled command's negative answer" "Retirement's settled-entry was"
      expect_no_play_task "$name" 'Ordinary retained work sentinel' ;;
    node-refused|node-unknown)
      expect_refused "$name" "Refuse the node configuration command's negative answer" "Retirement's node-config was"
      expect_no_play_task "$name" 'Ordinary retained work sentinel' ;;
    closing-refused)
      expect_refused "$name" "Refuse the settled command's negative answer" "Retirement's settled-closing was"
      expect_play_task_ran "$name" 'Ordinary retained work sentinel'
      expect_no_play_task "$name" 'Observe the successful caller result' ;;
    frozen-no-flag)
      expect_refused "$name" 'Refuse possible recommissioning from frozen controller configuration' 'possible recommissioning'
      expect_no_play_task "$name" 'Ordinary retained work sentinel' ;;
    check)
      expect_allowed "$name"
      expect_no_play_task "$name" 'Ordinary retained work sentinel' ;;
    *)
      expect_allowed "$name"
      expect_play_task_ran "$name" 'Ordinary retained work sentinel'
      expect_play_task_ran "$name" 'Observe the successful caller result' ;;
  esac
  expect_no_task "$name" "Read this host's clock for the continuation envelope"
  expect_no_task "$name" "Continue through the journal's recorded survivor"
  for mode in classify settled-entry node-config settled-closing; do
    [ "$(backing_runs "$name" "retire-$mode")" -eq 0 ] || fail "$name: $mode reached the backing binary"
  done
  "$python" - "$work/cases/$name" "$kind" <<'PYCALLS'
import hashlib, json, pathlib, sys
case, kind = pathlib.Path(sys.argv[1]), sys.argv[2]
calls = [json.loads(line) for line in (case / 'calls/index.jsonl').read_text().splitlines()]
retire = [call for call in calls if call['command'].startswith('retire-')]
expected = ['retire-classify']
if kind != 'check':
    expected += ['retire-settled-entry']
    if kind not in ['entry-refused', 'entry-unknown', 'frozen-no-flag']:
        expected += ['retire-node-config']
        if kind not in ['node-refused', 'node-unknown']:
            expected += ['retire-settled-closing']
if [call['command'] for call in retire] != expected:
    sys.exit('entry/node-config/closing order or refusal boundary differs: ' + repr(retire))
for call in retire[1:]:
    mode = call['command'].removeprefix('retire-')
    if mode == 'node-config':
        argv = ['server', 'retire', '--json', '--config', '/etc/billet/billet.yaml',
                '--run', 'ci-1', '--expected-holder', 'ci-1', '--expected-guard', '1' * 32,
                '--retiring-host', 'control-a', '--transition', '2' * 32, '--check-node-config', '--input', '-']
        raw = (case / 'calls' / call['stdin']).read_bytes()
        if raw != (case / 'answers/expected.stdin').read_bytes() or not call['has_stdin']:
            sys.exit('node-config stdin was changed or absent')
        document = json.loads(raw)
        if document['schema'] != 1 or document['run'] != 'ci-1' or document['transition_id'] != '2' * 32 or document['retiring'] != 'control-a':
            sys.exit('node-config document bindings differ')
        if hashlib.sha256(document['rendering'].encode()).hexdigest() != document['rendering_sha256']:
            sys.exit('node-config rendering digest differs')
        if document['operations']['services'] != [dict(verb=verb, unit='billet-node.service') for verb in ['enable', 'stop', 'start']]:
            sys.exit('node-config service plan differs')
        if [unit['unit'] for unit in document['operations']['units']] != ['billet-node.service']:
            sys.exit('node-config proposed units differ')
    else:
        argv = ['server', 'retire', '--check-' + mode, '--json', '--run', 'ci-1',
                '--expected-holder', 'ci-1', '--expected-guard', '1' * 32, '--retiring-host', 'control-a',
                '--transition', '2' * 32, '--config', '/etc/billet/billet.yaml']
        if call['has_stdin']:
            sys.exit('settled observation received stdin')
    if call['argv'] != argv:
        sys.exit('retained command argv differs: ' + repr(call))
# Diagnostics come from the selected producer answer, not just any failure.
branch = {'entry-refused': 'entry', 'entry-unknown': 'entry', 'node-refused': 'node', 'node-unknown': 'node', 'closing-refused': 'closing'}.get(kind)
if branch:
    answer = json.loads((case / 'expected.json').read_text())[branch]
    output = (case / 'out').read_text()
    if answer['reason'] not in output or answer['why'] not in output:
        sys.exit('the command reason or diagnostic was lost')
PYCALLS
done
