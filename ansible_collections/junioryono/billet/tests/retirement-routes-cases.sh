#!/usr/bin/env bash
# Sourced by retirement-cases.sh inside its selected CI section.
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

# R3/R5 lazy preparation: real main must reach the journal-only command with
# unusable D. The unsettled case reports its pending row and ends as before.
# R10 separately exercises retained continuation and its settled-entry boundary.
for kind in null-server malformed undefined-config; do
  name=r3-lazy-$kind
  r_plant "$name"
  case "$kind" in
    null-server) desired='{"billet_config":{"server":null}}' ;;
    malformed) desired='{"billet_config":["not-a-config-mapping"]}' ;;
    undefined-config) desired='{"billet_config":"{{ billet_gate_undefined_desired }}"}' ;;
  esac
  a "$name" -e "$desired" -e billet_retirement_survivor_host=unreachable
  if [ "$kind" = undefined-config ]; then
    r_answers "$name" 'control-a:classify:1:dry-run-continue-done-unsettled.json:0;control-a:request:1:retired-pending.json:0'
  else
    r_answers "$name" 'control-a:classify:1:dry-run-continue.json:0;control-a:request:1:retired-settled.json:0'
  fi
  r_run "$name" play-retirement-main
  expect_allowed "$name"
  expect_no_ordinary "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-request 1;'
  r_continuation "$name" 1
  r_reported "$name" continue
  expect_no_task "$name" 'Resolve only the desired ledger operand for preparation'
  expect_no_task "$name" 'Stage the immutable candidate binary inside its recovery journal'
  if [ "$kind" = undefined-config ]; then
    expect_ran "$name" "Report a row obligation whose survivor this converge did not prepare"
  fi
done

# Judge the task's own changed result, not preparation's recap. The settled
# controls must report ok; a republished status must report changed even when
# the outcome word is unchanged. Also require the final settlement message.
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
    PYTHONPATH="$here" "$python" -B - "$work/cases/$name/out" <<'PYOBLIGATION'
import pathlib, sys
from callback_result import callback_message
text = pathlib.Path(sys.argv[1]).read_text()
parts = text.split('TASK [junioryono.billet.host : Report a row obligation whose survivor this converge did not prepare]')
if len(parts) != 2:
    sys.exit('the unprepared survivor obligation was not reported exactly once')
message = callback_message(parts[1].split('TASK [', 1)[0], 'pending survivor obligation')
for clause in ['pending row. Survivor control-b', 'has no settled, pointer-free guard held by this converge; no handoff was attempted.', 'Prepare that survivor in this converge and rerun the retirement to complete and acknowledge the row.']:
    if clause not in message:
        sys.exit('the pending report omitted its recovery obligation: ' + clause)
PYOBLIGATION
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
r_answers r5-handoff 'control-a:classify:1:dry-run-continue.json:0;control-a:request:1:retired-pending.json:0;control-b:complete:1:completed.json:0;control-a:acknowledge:1:acknowledged.json:0;control-a:request:2:retired-after-survivor-ack.json:0'
a r5-handoff -e billet_gate_peer=true
r_run r5-handoff
expect_allowed r5-handoff
expect_host_commands r5-handoff 'control-a retire-classify 1;control-a retire-request 1;control-b retire-complete 1;control-a retire-acknowledge 1;control-a retire-request 2;'
expect_no_ordinary r5-handoff
expect_no_play_task r5-handoff 'Ordinary convergence sentinel'
expect_ran r5-handoff 'Require a known completed retirement matching its variant and receipt'
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

