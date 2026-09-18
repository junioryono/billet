#!/usr/bin/env bash
# Sourced by retirement-cases.sh inside the request-cancellation section.

# R12: cancellation uses the observed survivor even when inventory names an
# unreachable alternative. The row starts under ci-0; this converge is ci-1.
for scenario in success check adopt-refused adopt-unanswered adopt-unreachable adopt-fresh adopt-fresh-refused adopt-fresh-unanswered adopt-fresh-transition adopt-run adopt-retiring adopt-survivor adopt-transition abandon-refused abandon-unanswered abandon-transition abandon-unreachable; do
  name=r12-$scenario
  r_plant "$name"
  ep_plant_config "$name" 127.0.0.1:7717 server-only
  a "$name" -e billet_converge_guard_holder=ci-1 -e billet_gate_cancel=true -e billet_server_retire=false -e billet_retirement_survivor_host=unreachable
  e "$name" 'BILLET_GATE_RETIRE_ENV=/etc/billet/server.env (ignore_errors=yes)'
  e "$name" BILLET_GATE_RETIRE_OBLIGATION=/var/lib/billet/gate-cancel-reservation
  p "$name" 'printf "ci-0\n" >/var/lib/billet/gate-cancel-reservation'
  mkdir -p "$work/cases/$name/answers"
  cp "$work/retire-fixtures/"*.json "$work/cases/$name/answers/"
  e "$name" "BILLET_GATE_RETIRE_FIXTURES=$work/cases/$name/answers"
  adopted=adopted; adopt_exit=0; abandoned=abandoned; abandon_exit=0
  step=adoption; task='Bind cancellation adoption to the classified reservation'; why='did not confirm the observed binding'
  case "$scenario" in
    check) a "$name" --check ;;
    adopt-refused) adopted=refused-guard-holder; adopt_exit=2; task="Refuse the retirement's answer"; why="was refused" ;;
    adopt-unanswered) e "$name" BILLET_GATE_DROP_ANSWER=retire-reserve:1; task='Refuse a retirement call that did not answer'; why="did not answer retirement's reserve call" ;;
    adopt-unreachable)
      # A registered step fact changes only the connection of the first
      # mutation; classification and the digest observation used local transport.
      cat >"$work/cases/$name/inventory.yml" <<'INV'
all:
  hosts:
    control-a:
      ansible_connection: "{{ 'ssh' if billet_retirement_cancel_step | default('') == 'adoption' else 'local' }}"
      ansible_host: 127.0.0.1
      ansible_port: 0
      ansible_connect_timeout: 1
      ansible_ssh_common_args: '-o BatchMode=yes -o ConnectionAttempts=1'
INV
      a "$name" -i "$work/cases/$name/inventory.yml"
      task='Adopt the cancellation reservation under this holder'; why='UNREACHABLE!'
      ;;
    adopt-fresh*)
      adopted=reserved
      task='Require cancellation to adopt an existing reservation'; why='unexpectedly answered reserved instead of adopted'
      # A fresh row has its own transition, not the classifier's old one.
      "$python" - "$work/cases/$name/answers" "$scenario" <<'PYFRESH'
import json, pathlib, sys
root, scenario = pathlib.Path(sys.argv[1]), sys.argv[2]
for name in ['reserved', 'abandoned']:
    if name == 'abandoned' and scenario == 'adopt-fresh-transition':
        continue
    path = root / (name + '.json')
    answer = json.loads(path.read_text())
    answer['transition_id'] = 'fedcba9876543210fedcba9876543210'
    path.write_text(json.dumps(answer))
PYFRESH
      if [ "$scenario" = adopt-fresh-refused ]; then abandoned=refused-abandon-journal; abandon_exit=2; fi
      if [ "$scenario" = adopt-fresh-unanswered ]; then e "$name" BILLET_GATE_DROP_ANSWER=retire-abandon:1; fi
      ;;
    adopt-run|adopt-retiring|adopt-survivor|adopt-transition|abandon-transition)
      "$python" - "$work/cases/$name/answers" "$scenario" <<'PYBIND'
import json, pathlib, sys
root, scenario = pathlib.Path(sys.argv[1]), sys.argv[2]
path = root / ('abandoned.json' if scenario.startswith('abandon-') else 'adopted.json')
answer = json.loads(path.read_text())
member = scenario.split('-', 1)[1]
member = 'transition_id' if member == 'transition' else member
answer[member] = 'fedcba9876543210fedcba9876543210' if member == 'transition_id' else 'another'
path.write_text(json.dumps(answer))
PYBIND
      ;;
    abandon-refused) abandoned=refused-abandon-journal; abandon_exit=2; task="Refuse the retirement's answer"; why='was refused' ;;
    abandon-unanswered) e "$name" BILLET_GATE_DROP_ANSWER=retire-abandon:1; task='Refuse a retirement call that did not answer'; why="did not answer retirement's abandon call" ;;
    abandon-unreachable)
      # The reserve wrapper runs this hook after its transport is established;
      # later tasks resolve the real SSH connection to the closed port.
      cat >"$work/cases/$name/disconnect" <<'HOOK'
#!/bin/sh
set -eu
touch /var/lib/billet/gate-cancel-disconnect
HOOK
      chmod 0755 "$work/cases/$name/disconnect"
      e "$name" "BILLET_GATE_HOOK=retire-reserve:1:$work/cases/$name/disconnect"
      cat >"$work/cases/$name/inventory.yml" <<'INV'
all:
  hosts:
    control-a:
      ansible_connection: "{{ 'ssh' if lookup('ansible.builtin.fileglob', '/var/lib/billet/gate-cancel-disconnect') | length > 0 else 'local' }}"
      ansible_host: 127.0.0.1
      ansible_port: 0
      ansible_connect_timeout: 1
      ansible_ssh_common_args: '-o BatchMode=yes -o ConnectionAttempts=1'
INV
      a "$name" -i "$work/cases/$name/inventory.yml"
      task='Abandon the adopted cancellation reservation'; why='UNREACHABLE!'
      ;;
  esac
  case "$scenario" in
    abandon-*) step=abandonment ;;
  esac
  if [ "$scenario" = abandon-transition ]; then
    task='Bind cancellation abandonment to the classified reservation'; why='answered for another transition'
  fi
  r_answers "$name" "control-a:classify:1:dry-run-cancel-previous-run.json:0;control-a:reserve:1:$adopted.json:$adopt_exit;control-a:abandon:1:$abandoned.json:$abandon_exit"
  post "$name" "if [ -f /var/lib/billet/gate-cancel-reservation ]; then cp /var/lib/billet/gate-cancel-reservation '$work/cases/$name/reservation-after'; fi"
  post "$name" "if [ -f /var/lib/billet/gate-cancel-ordinary ]; then cp /var/lib/billet/gate-cancel-ordinary '$work/cases/$name/ordinary-after'; fi"
  r_run "$name"
  r_reported "$name" cancel
  expect_no_ordinary "$name"
  expect_no_task "$name" 'Inspect the transaction claim before recovery'
  if [ "$scenario" = success ]; then
    expect_allowed "$name"
    expect_play_task_ran "$name" 'Ordinary convergence sentinel'
    expect_play_task_ran "$name" 'Record the ordinary cancellation effect'
    expect_ran "$name" 'Admit ordinary work after confirmed cancellation'
    expect_path_absent "$name" /var/lib/billet/gate-cancel-reservation
  else
    expect_path_absent "$name" /var/lib/billet/gate-cancel-ordinary
    expect_no_play_task "$name" 'Ordinary convergence sentinel'
    expect_no_task "$name" 'Admit ordinary work after confirmed cancellation'
    if [ "$scenario" = check ]; then
      expect_allowed "$name"
      expect_ran "$name" 'Report the prospective reservation cancellation'
      expect_no_task "$name" "Read the installed configuration's cancellation digest"
    else
      expect_refused "$name" "$task" "$why"
      expect_final "$name" "Reservation cancellation failed at $step" 'Ordinary host tasks remain bypassed.'
    fi
  fi
  case "$scenario" in
    adopt-fresh*)
      expect_final "$name" 'unexpectedly answered reserved instead of adopted'
      expect_ran "$name" 'Ask abandonment to discharge the fresh cancellation reservation'
      expect_no_task "$name" 'Abandon the adopted cancellation reservation'
      if [ "$scenario" = adopt-fresh ]; then
        expect_ran "$name" 'Bind cancellation cleanup to the fresh transition'
        expect_final "$name" 'Afterwards: The fresh reservation was abandoned.'
        expect_path_absent "$name" /var/lib/billet/gate-cancel-reservation
      else
        expect_ran "$name" 'Keep the fresh cancellation cleanup refusal'
        expect_final "$name" 'Cleanup remains outstanding: reservation for retiring host control-a,' 'survivor control-b, run ci-1,' 'transition fedcba9876543210fedcba9876543210 is not proved released.'
        case "$scenario" in
          adopt-fresh-refused) expect_final "$name" "Retirement's abandon" 'was refused' ;;
          adopt-fresh-unanswered) expect_final "$name" "did not answer retirement's abandon call" ;;
          adopt-fresh-transition) expect_final "$name" 'The abandonment answered for another transition; cleanup is unproved.' ;;
        esac
      fi
      ;;
  esac
  case "$scenario" in
    check|adopt-unreachable) calls='control-a retire-classify 1;' ;;
    adopt-fresh*) calls='control-a retire-classify 1;control-a retire-reserve 1;control-a retire-abandon 1;' ;;
    adopt-*|abandon-unreachable) calls='control-a retire-classify 1;control-a retire-reserve 1;' ;;
    *) calls='control-a retire-classify 1;control-a retire-reserve 1;control-a retire-abandon 1;' ;;
  esac
  expect_host_commands "$name" "$calls"
  PYTHONPATH="$here" "$python" -B - "$work/cases/$name" "$scenario" <<'PYCANCEL'
import hashlib, json, pathlib, sys
from callback_result import callback_message
case, scenario = pathlib.Path(sys.argv[1]), sys.argv[2]
records = [json.loads(line) for line in (case / 'calls/index.jsonl').read_text().splitlines()]
for call in records:
    if call['host'] != 'control-a' or call['command'] in ['release', 'status', 'migrate-endpoint'] or call['argv'][:2] == ['local', 'prepare']:
        sys.exit('cancellation collected evidence, contacted inventory survivor or imported the service account')
    if call['command'] not in ['retire-reserve', 'retire-abandon']:
        continue
    common = ['--json', '--run', 'ci-1', '--retiring-host', 'control-a']
    if call['command'] == 'retire-reserve':
        expected = ['server', 'retire', '--reserve'] + common + ['--survivor-host', 'control-b', '--installed-sha256', hashlib.sha256((case / 'installed.yaml.plant').read_bytes()).hexdigest()]
    else:
        expected = ['server', 'retire', '--abandon-reservation'] + common
    expected += ['--config', '/etc/billet/billet.yaml', '--environment-file', '/etc/billet/server.env']
    if call['argv'] != expected or call['has_stdin']:
        sys.exit('cancellation argv/stdin differs: ' + repr(call))
text = (case / 'out').read_text()
parts = text.split("TASK [Observe cancellation's terminal boundary]")
if len(parts) != 2:
    sys.exit('cancellation did not report its actual bypass after the result')
message = callback_message(parts[1].split('TASK [', 1)[0], case.name + ': bypass')
if message != 'Cancellation bypass=' + ('False' if scenario == 'success' else 'True') + '.':
    sys.exit('cancellation left the wrong actual bypass: ' + message)
if scenario == 'success':
    if (case / 'ordinary-after').read_text() != 'reached':
        sys.exit('confirmed cancellation did not produce its ordinary effect')
    binding = text.index('TASK [junioryono.billet.host : Bind cancellation abandonment to the classified reservation]')
    admission = text.index('TASK [junioryono.billet.host : Admit ordinary work after confirmed cancellation]')
    ordinary = text.index('TASK [Ordinary convergence sentinel]')
    if not binding < admission < ordinary:
        sys.exit('ordinary work ran before confirmed and bound abandonment')
if scenario == 'check':
    parts = text.split('TASK [junioryono.billet.host : Report the prospective reservation cancellation]')
    if len(parts) != 2:
        sys.exit('check mode did not report prospective cancellation exactly once')
    preview = callback_message(parts[1].split('TASK [', 1)[0], case.name + ': preview')
    for clause in ['recorded survivor control-b under holder ci-1', 'validate its binding, then abandon it.', 'Check mode cancels nothing; ordinary host tasks remain bypassed.']:
        if clause not in preview:
            sys.exit('cancellation preview omitted ' + clause)
if scenario in ['check', 'adopt-refused', 'adopt-unreachable']:
    if (case / 'reservation-after').read_text() != 'ci-0\n':
        sys.exit('cancellation changed an unadopted reservation')
elif (scenario.startswith('adopt-') and scenario not in ['adopt-fresh', 'adopt-fresh-unanswered', 'adopt-fresh-transition']) or scenario in ['abandon-refused', 'abandon-unreachable']:
    if (case / 'reservation-after').read_text() != 'reserved\n':
        sys.exit('unconfirmed cancellation lost the reserved obligation')
else:
    if (case / 'reservation-after').exists():
        sys.exit('the successful fake abandonment did not remove its row')
if scenario in ['adopt-unreachable', 'abandon-unreachable']:
    task, call = ('Adopt the cancellation reservation under this holder', 'reserve') if scenario == 'adopt-unreachable' else ('Abandon the adopted cancellation reservation', 'abandon')
    parts = text.split('TASK [junioryono.billet.host : ' + task + ']')
    if len(parts) != 2 or 'fatal: [control-a]: UNREACHABLE!' not in parts[1].split('TASK [', 1)[0]:
        sys.exit('the cancellation did not lose its connection at ' + task)
    if "did not answer retirement's " + call + " call" not in text:
        sys.exit('unreachable cancellation did not reach the ordinary parser failure')
PYCANCEL
done

