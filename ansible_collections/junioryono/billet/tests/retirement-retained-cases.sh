#!/usr/bin/env bash
# Sourced by retirement-cases.sh inside its selected CI section.
# R10: the word holds the host, whatever observation produced it. The corpus
# fixture is a requested node-only host, the one observation that reaches
# unsupported-variant now that an installed pair keeping a node is requestable;
# the role branches on the route word alone, so this case asserts that alone.
# R10's fresh installed-both request is the request section's.
r_plant r10-unsupported-variant
a r10-unsupported-variant -e billet_server_retire=true -e '{"billet_config":{"server":{"state_dir":"/var/lib/billet/server"}}}'
r_answers r10-unsupported-variant 'control-a:classify:1:dry-run-unsupported-variant.json:0'
r_run r10-unsupported-variant
r_held r10-unsupported-variant unsupported-variant

# R10: retained journals continue with their recorded operands and paired
# receipts, then end even when settled. Desired inventory supplies no operands.
for phase in intent stopped archived config-rewritten node-restarted; do
  name=r10-journal-$phase
  r_plant "$name"
  a "$name" -e billet_gate_retained_continuation=true -e billet_retirement_survivor_host=unreachable
  e "$name" 'BILLET_GATE_RETIRE_ENV=/etc/billet/node.env (ignore_errors=yes)'
  answer=retired-retained-settled
  if [ "$phase" = archived ]; then answer=retired-retained-pending; fi
  case "$phase" in
    stopped|config-rewritten|node-restarted)
      # Caller-only phase variants of a producer classification. Real phase
      # transitions and physical state witnesses remain 3d's responsibility.
      "$python" - "$work/retire-fixtures" "$work/cases/$name" "$phase" "$answer" <<'PYPHASE'
import json, pathlib, shutil, sys
fixtures, case, phase, answer = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2]), sys.argv[3], sys.argv[4]
value = json.loads((fixtures / 'dry-run-continue-retained-intent.json').read_text())
value['journal']['phase'] = value['status']['phase'] = value['state'] = phase
(case / 'classifier.json').write_text(json.dumps(value))
shutil.copyfile(fixtures / (answer + '.json'), case / 'answer.json')
PYPHASE
      e "$name" "BILLET_GATE_RETIRE_FIXTURES=$work/cases/$name"
      r_answers "$name" "control-a:classify:1:classifier.json:0;control-a:request:1:answer.json:0" ;;
    *) r_answers "$name" "control-a:classify:1:dry-run-continue-retained-$phase.json:0;control-a:request:1:$answer.json:0" ;;
  esac
  r_run "$name"
  expect_allowed "$name"
  expect_no_ordinary "$name"
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
  expect_play_task_ran "$name" "Observe the retained continuation's terminal boundary"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-request 1;'
  r_continuation "$name" 1 /etc/billet/node.env
  r_reported "$name" continue 'continues the retained-node retirement it records'
  if [ "$phase" = archived ]; then
    r_done_report "$name" False changed
    expect_ran "$name" "Report a row obligation whose survivor this converge did not prepare"
  else
    r_done_report "$name" True changed
    expect_no_task "$name" "Report a row obligation whose survivor this converge did not prepare"
  fi
  expect_no_task "$name" 'Complete the row on the recorded survivor'
done

# DONE BUT UNSETTLED IS NOT SETTLED ENTRY. Deleting the settled clause from the
# entry predicate would strand this host, so it must still reach its continuation.
# A repeated pending tail reports its receipt current, the other accepted pairing.
r_plant r10-journal-done-unsettled
"$python" - "$work/retire-fixtures" "$work/cases/r10-journal-done-unsettled" <<'PYUNSETTLED'
import json, pathlib, shutil, sys
fixtures, case = map(pathlib.Path, sys.argv[1:3])
classifier = json.loads((fixtures / 'dry-run-continue-retained-done-unsettled.json').read_text())
if classifier['journal']['phase'] != 'done' or classifier['journal']['settled'] is not False:
    sys.exit('the unsettled retained classification is not done and unsettled')
shutil.copyfile(fixtures / 'dry-run-continue-retained-done-unsettled.json', case / 'classifier.json')
answer = json.loads((fixtures / 'retired-retained-pending.json').read_text())
answer['receipt'] = 'current'
(case / 'answer.json').write_text(json.dumps(answer))
PYUNSETTLED
a r10-journal-done-unsettled -e billet_gate_retained_continuation=true -e billet_retirement_survivor_host=unreachable
e r10-journal-done-unsettled 'BILLET_GATE_RETIRE_ENV=/etc/billet/node.env (ignore_errors=yes)'
e r10-journal-done-unsettled "BILLET_GATE_RETIRE_FIXTURES=$work/cases/r10-journal-done-unsettled"
r_answers r10-journal-done-unsettled 'control-a:classify:1:classifier.json:0;control-a:request:1:answer.json:0'
r_run r10-journal-done-unsettled
expect_allowed r10-journal-done-unsettled
expect_no_ordinary r10-journal-done-unsettled
expect_no_play_task r10-journal-done-unsettled 'Ordinary convergence sentinel'
expect_play_task_ran r10-journal-done-unsettled "Observe the retained continuation's terminal boundary"
expect_no_task r10-journal-done-unsettled 'Check settled entry through the retirement command'
expect_ran r10-journal-done-unsettled "Continue through the journal's recorded survivor"
expect_host_commands r10-journal-done-unsettled 'control-a retire-classify 1;control-a retire-request 1;'
r_continuation r10-journal-done-unsettled 1 /etc/billet/node.env
r_reported r10-journal-done-unsettled continue 'continues the retained-node retirement it records'
r_done_report r10-journal-done-unsettled False changed
expect_ran r10-journal-done-unsettled "Report a row obligation whose survivor this converge did not prepare"

# r10-journal-done now runs the connected entry, node-config and closing callers
# in retirement-activation-cases.sh; the normal continuation must stay unused.

# Each bad answer still passes the strict parser. Only binding the variant
# and pairing its receipt can refuse these case-local corruptions.
for kind in retained-no-receipt server-with-receipt wrong-variant; do
  name=r10-$kind
  r_plant "$name"
  a "$name" -e billet_gate_retained_continuation=true
  classifier=dry-run-continue-retained-intent
  answer=retired-retained-settled
  if [ "$kind" = server-with-receipt ]; then
    classifier=dry-run-continue
    answer=retired-settled
  fi
  "$python" - "$work/retire-fixtures" "$work/cases/$name" "$classifier" "$answer" "$kind" <<'PYPAIR'
import json, pathlib, shutil, sys
fixtures, case = map(pathlib.Path, sys.argv[1:3])
classifier, source, kind = sys.argv[3:]
shutil.copyfile(fixtures / (classifier + '.json'), case / 'classifier.json')
answer = json.loads((fixtures / (source + '.json')).read_text())
if kind == 'retained-no-receipt':
    answer['receipt'] = 'none'
elif kind == 'server-with-receipt':
    answer['receipt'] = 'written'
else:
    answer['variant'], answer['receipt'] = 'server-only', 'none'
(case / 'answer.json').write_text(json.dumps(answer))
PYPAIR
  e "$name" "BILLET_GATE_RETIRE_FIXTURES=$work/cases/$name"
  r_answers "$name" 'control-a:classify:1:classifier.json:0;control-a:request:1:answer.json:0'
  r_run "$name"
  expect_refused "$name" 'Require a known completed retirement matching its variant and receipt' \
    'The continuation did not confirm a known done state with the recorded variant and its paired receipt'
  expect_no_ordinary "$name"
  expect_no_play_task "$name" 'Ordinary convergence sentinel'
  expect_play_task_ran "$name" "Observe the retained continuation's terminal boundary"
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-request 1;'
  r_continuation "$name" 1
  r_reported "$name" continue
  expect_no_task "$name" "Record the continuation's result and changes"
  expect_no_task "$name" 'Complete the row on the recorded survivor'
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
done

