#!/usr/bin/env bash
# Sourced only by the retirement routes shard, after r_plant and the wrapper.
# Two plays amortize namespace setup; every answer still runs the real caller.
cp "$here/retirement-settled.yml" "$work/play-retirement-settled.yml"
cp "$here/retirement-settled-case.yml" "$work/retirement-settled-case.yml"
for mode in entry closing; do
  name=r-settled-$mode
  r_plant "$name"
  mkdir -p "$work/cases/$name/answers"
  git -C "$repo_root" ls-tree -r --name-only HEAD -- "ansible_collections/junioryono/billet/tests/fixtures/retire-settled-$mode/" >"$work/cases/$name/fixtures"
  [ -s "$work/cases/$name/fixtures" ] || fail "$name: no committed settled fixtures"
  while IFS= read -r fixture; do
    git -C "$repo_root" show "HEAD:$fixture" >"$work/cases/$name/answers/${fixture##*/}" || fail "$name: cannot read committed fixture $fixture"
  done <"$work/cases/$name/fixtures"
  "$python" - "$work/cases/$name" "$mode" <<'PYCASES'
import json, pathlib, sys
case, mode = pathlib.Path(sys.argv[1]), sys.argv[2]
root = case / 'answers'
cases, specs = [], []
def add(name, raw, rc, refuses, reason='', why='', answer=None):
    filename = name + '.json'
    (root / filename).write_text(raw)
    cases.append(dict(name=name, refuses=refuses, reason=reason, why=why, answer=answer))
    specs.append(f'control-a:settled-{mode}:{len(cases)}:{filename}:{rc}')
for fixture in sorted(root.glob('*.json')):
    raw = fixture.read_text()
    answer = json.loads(raw)
    rc = {'refused': 2, 'unknown': 3}.get(answer['outcome'], 0)
    add(fixture.stem, raw, rc, rc != 0, answer.get('reason', ''), answer.get('why', ''), answer)
active = json.loads((root / 'active.json').read_text())
add('classifier-variant-mismatch', json.dumps(active), 0, True, 'does not bind')
cases[-1]['classified_variant'] = 'server-only'
# All corruptions stay in this case's private copies of producer answers.
for label, member, value, reason in [
    ('transition-mismatch', 'transition_id', '33333333333333333333333333333333', 'does not bind'),
    ('variant-mismatch', 'variant', 'server-only', 'settled retained contract'),
    ('run-mismatch', 'run', 'ci-0', 'does not bind'),
    ('guard-mismatch', 'guard', '33333333333333333333333333333333', 'does not bind'),
    ('host-mismatch', 'retiring', 'another-host', 'does not bind'),
    ('purpose-mismatch', 'purpose', 'settled-closing' if mode == 'entry' else 'settled-entry', 'purpose'),
    ('boolean-schema', 'schema', True, 'schema'),
    ('string-row', 'row_done', 'true', 'settled retained contract'),
    ('unknown-member', 'extra', True, 'unknown members'),
    # Each success invariant gets its own corruption, or removing its check
    # would leave every case's verdict unchanged. The other purpose's success
    # outcome is the one that matters: entry must never read as verified.
    ('wrong-outcome', 'outcome', 'verified' if mode == 'entry' else 'admitted', 'outcome or exit'),
    ('unsettled', 'settled', False, 'settled retained contract'),
    ('not-done', 'phase', 'node-restarted', 'settled retained contract'),
    ('wrong-state', 'state', 'done', 'outcome or exit'),
]:
    answer = dict(active)
    answer[member] = value
    add(label, json.dumps(answer), 0, True, reason)
missing = dict(active)
del missing['completed_by']
add('missing-member', json.dumps(missing), 0, True, 'missing or unknown members')
raw = json.dumps(active)
add('duplicate-member', raw[:-1] + ', "schema": 1}', 0, True, 'malformed or repeated JSON')
add('truncated', raw[:-1], 0, True, 'malformed or repeated JSON')
add('oversized', raw + ' ' * 16384, 0, True, 'bounded complete answer')
# Newlines, not spaces: an output-stripping command would trim these before
# the bound measures them, which is the defect this case exists to catch.
add('oversized-newlines', raw + '\n' * 16384, 0, True, 'bounded complete answer')
add('exit-mismatch', raw, 2, True, 'exit')
add('unanswered', '', 0, True, 'bounded complete answer')
if mode == 'closing':
    wrong = dict(active, deployment='eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee')
    add('deployment-mismatch', json.dumps(wrong), 0, True, 'does not bind')
    quiet = dict(active, node_activity='quiet-inactive')
    add('quiet-closing', json.dumps(quiet), 0, True, 'required node activity')
(case / 'settled-vars.json').write_text(json.dumps(dict(billet_gate_settled_mode=mode, billet_gate_settled_cases=cases)))
(case / 'answers-spec').write_text(';'.join(specs))
PYCASES
  e "$name" "BILLET_GATE_RETIRE_FIXTURES=$work/cases/$name/answers"
  r_answers "$name" "$(cat "$work/cases/$name/answers-spec")"
  a "$name" -e "@$work/cases/$name/settled-vars.json"
  ns_case "$name" escalated play-retirement-settled
  expect_allowed "$name"
  expect_play_task_ran "$name" 'Record completion of every settled case'
  expect_no_ordinary "$name"
  [ "$(backing_runs "$name" "retire-settled-$mode")" -eq 0 ] || fail "$name: settled call reached the backing binary"
  "$python" - "$work/cases/$name" "$mode" <<'PYCALLS'
import json, pathlib, sys
case, mode = pathlib.Path(sys.argv[1]), sys.argv[2]
cases = json.loads((case / 'settled-vars.json').read_text())['billet_gate_settled_cases']
calls = [json.loads(line) for line in (case / 'calls/index.jsonl').read_text().splitlines()]
expected = ['server', 'retire', '--check-settled-' + mode, '--json', '--run', 'ci-1',
            '--expected-holder', 'ci-1', '--expected-guard', '11111111111111111111111111111111',
            '--retiring-host', 'control-a', '--transition', '22222222222222222222222222222222',
            '--config', '/etc/billet/billet.yaml']
if len(calls) != len(cases):
    sys.exit('not every settled case invoked exactly one command')
for call in calls:
    if call['host'] != 'control-a' or call['command'] != 'retire-settled-' + mode or call['argv'] != expected or call['has_stdin']:
        sys.exit('settled call used unexpected operands or stdin: ' + repr(call))
PYCALLS
done

# Connected caller cases share this shard and the same stdin-recording fake.
# shellcheck source=retirement-activation-cases.sh
source "$here/retirement-activation-cases.sh"
