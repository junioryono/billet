#!/usr/bin/env bash
# Sourced by converge-guard-check.sh inside its selected CI section.
cat >"$work/play-retire-parser.yml" <<'PLAY'
---
- name: Exercise the retirement parser alone
  hosts: billet_hosts
  gather_facts: false
  tasks:
    - name: Read the invocation the case supplied
      ansible.builtin.set_fact:
        billet_retire_raw: "{{ billet_gate_retire_raw }}"
        # A refused second inclusion must not retain a preceding answer.
        billet_retire_answer: {stale: true}
        billet_retire_route: ordinary
        billet_retire_state_known: true
        billet_retire_valid: true
        billet_retire_reservation: released
    - name: Parse the retirement answer and prove its failure facts
      block:
        - name: Parse the retirement answer
          ansible.builtin.include_role:
            name: junioryono.billet.host
            tasks_from: retire-answer
      rescue:
        - name: Prove a failed parse discarded the preceding facts
          vars:
            # A validated refusal republishes its own state and cleanup word;
            # unreadable and unanswered calls keep those permissions reset.
            billet_gate_typed_refusal: "{{ ansible_failed_task.name == \"Refuse the retirement's answer\" }}"
          ansible.builtin.assert:
            that:
              - billet_retire_route == ''
              - billet_retire_valid is sameas billet_gate_typed_refusal
              - billet_retire_state_known is sameas (billet_retire_answer.state != 'unknown' if billet_gate_typed_refusal else false)
              - billet_retire_reservation == (billet_retire_answer.reservation | default('') if billet_gate_typed_refusal else '')
              - >-
                billet_retire_answer ==
                ({} if ansible_failed_task.name in ['Refuse a retirement call that did not answer', 'Refuse unreadable retirement JSON']
                 else billet_gate_retire_raw.stdout | junioryono.billet.from_json_strict)
            fail_msg: The retirement parser retained stale facts after failure.
            success_msg: Retirement failure facts verified.
        - name: Preserve the retirement parser's refusal
          ansible.builtin.fail:
            msg: "{{ ansible_failed_result.msg | default('The retirement parser refused this invocation.') }}"
    - name: Prove the parser published this invocation's typed operands
      ansible.builtin.assert:
        that:
          - billet_retire_valid is sameas true
          - billet_retire_answer == (billet_gate_retire_raw.stdout | junioryono.billet.from_json_strict)
          - billet_retire_route == (billet_retire_answer.route if billet_retire_call == 'classify' else '')
          - billet_retire_state_known is sameas (false if billet_retire_call == 'complete' else billet_retire_answer.state != 'unknown')
          - billet_retire_reservation == ''
        fail_msg: The retirement parser did not publish this call's own answer.
PLAY

# The result is built from a fixture, then ONE member of the answer or the
# invocation is corrupted. '-' removes it; JSON values preserve their types.
retire_parser_case() { # case call fixture rc [answer|raw member json]
  local name=$1 call=$2 fixture=$3 rc=$4; shift 4
  retirement_case_seen "$name"
  plant "$name"
  "$python" - "$work/retire-fixtures/$fixture.json" "$work/cases/$name/raw.json" "$call" "$rc" "$@" <<'PY'
import json, sys
source, dest, call, rc, *mutation = sys.argv[1:]
answer = json.load(open(source))
raw = {"rc": int(rc), "stdout": json.dumps(answer), "stderr": ""}
if mutation:
    target, member, value = mutation
    obj = answer if target == "answer" else raw
    if value == "-":
        del obj[member]
    else:
        obj[member] = json.loads(value)
    if target == "answer":
        raw["stdout"] = json.dumps(answer)
with open(dest, "w") as stream:
    json.dump({"billet_gate_retire_raw": raw, "billet_retire_call": call,
               "billet_retire_answered_by": "/usr/bin/billet"}, stream)
PY
  : >"$work/cases/$name/log"
  set +e
  env ANSIBLE_COLLECTIONS_PATH="$collections_path" ANSIBLE_STDOUT_CALLBACK=default \
    ANSIBLE_NOCOLOR=1 ANSIBLE_FORCE_COLOR=0 \
    ANSIBLE_LOCAL_TEMP="$work/cases/$name/tmp" ANSIBLE_REMOTE_TEMP="$work/cases/$name/tmp" \
    "$ansible_playbook" -i "$work/inventory.ini" "$work/play-retire-parser.yml" \
    -e "@$work/cases/$name/raw.json" -e ansible_become=false >"$work/cases/$name/out" 2>&1
  status=$?
  set -e
  # The first failure still supplies the refusal verdict. A second failure
  # in rescue must not pass merely because that first refusal was expected.
  if [ "$status" -ne 0 ]; then
    grep -qF '"msg": "Retirement failure facts verified."' "$work/cases/$name/out" || fail "$name: the failure facts were not verified" "$work/cases/$name/out"
  fi
  expect_no_ordinary "$name"
}
retire_member_refused() { # case member [task]
  local name=$1 member=$2 task=${3:-Judge each retirement member} count
  expect_refused "$name" "$task" "answered with a member this role cannot read: $member"
  count=$(grep -c '^failed: \[localhost\] (item=' "$work/cases/$name/out" || true)
  [ "$count" -eq 1 ] || fail "$name: $count failed parser items, want exactly one" "$work/cases/$name/out"
  grep -qF "failed: [localhost] (item=$member)" "$work/cases/$name/out" || fail "$name: the failed member is not $member" "$work/cases/$name/out"
  expect_no_play_task "$name" "Prove the parser published this invocation's typed operands"
}
retire_unanswered() { # case task
  expect_refused "$1" "$2" "did not answer retirement's" "state is unknown"
  expect_no_play_task "$1" "Prove the parser published this invocation's typed operands"
}

# R7's positive controls: every call and every committed success shape. A
# dry-run fixture's filename says dispatch, not route: request is ordinary,
# adopt is hold, and unknown-ledger with a readable journal is continue.
# Completion has no state member, so it must publish state_known false.
for spec in classify:dry-run-request classify:dry-run-adopt classify:dry-run-unknown-ledger \
  classify:dry-run-ordinary classify:dry-run-continue classify:dry-run-hold-unreadable-row \
  classify:dry-run-hold-damaged-identity classify:dry-run-recovery-guard classify:dry-run-recovery-legacy \
  classify:dry-run-new-request classify:dry-run-cancel classify:dry-run-unsupported-variant request:done-unchanged \
  reserve:reserved reserve:adopted request:retired-settled request:retired-pending \
  abandon:abandoned abandon:abandoned-marker-cleared complete:completed complete:completed-already \
  acknowledge:acknowledged acknowledge:acknowledged-already; do
  call=${spec%%:*}; fixture=${spec#*:}
  retire_parser_case "r7-pass-$fixture" "$call" "$fixture" 0
  expect_allowed "r7-pass-$fixture"
  expect_play_task_ran "r7-pass-$fixture" "Prove the parser published this invocation's typed operands"
done

for spec in missing:- null:null unknown:'"invented"'; do
  kind=${spec%%:*}; value=${spec#*:}
  retire_parser_case "r7-route-$kind" classify dry-run-request 0 answer route "$value"
  retire_member_refused "r7-route-$kind" route
done
retire_parser_case r7-route-why classify dry-run-adopt 0 answer route_why -
retire_member_refused r7-route-why route_why
retire_parser_case r7-roles-null classify dry-run-request 0 answer installed_roles null
retire_member_refused r7-roles-null installed_roles
# Changing config alone puts the fixture's non-null roles beside absence.
retire_parser_case r7-roles-absent classify dry-run-request 0 answer config '"absent"'
retire_member_refused r7-roles-absent installed_roles
retire_parser_case r7-extra classify dry-run-request 0 answer extra true
retire_member_refused r7-extra extra "Refuse a retirement member the producer does not write"
retire_parser_case r7-completion-outcome complete completed 0 answer outcome '"completed"'
retire_member_refused r7-completion-outcome outcome
retire_parser_case r7-reservation request refused-reservation 2 answer reservation '"lost"'
retire_member_refused r7-reservation reservation

# Every mismatch among the three protocol statuses: an otherwise well-formed
# success/refusal/unknown is unanswered, not an outcome under another exit.
for spec in dry-run-request:2 dry-run-request:3 refused-backend:0 refused-backend:3 unknown-marker:0 unknown-marker:2; do
  fixture=${spec%%:*}; rc=${spec#*:}
  retire_parser_case "r7-exit-$fixture-$rc" classify "$fixture" "$rc"
  retire_unanswered "r7-exit-$fixture-$rc" "Refuse a retirement exit that disagrees with its answer"
done
# The outcome-less success has the same exit contract.
retire_parser_case r7-exit-complete complete completed 2
retire_unanswered r7-exit-complete "Refuse a retirement exit that disagrees with its answer"

for spec in classify:dry-run-request reserve:reserved request:retired-settled \
  abandon:abandoned complete:completed acknowledge:acknowledged; do
  call=${spec%%:*}; fixture=${spec#*:}
  retire_parser_case "r7-no-rc-$call" "$call" "$fixture" 0 raw rc -
  retire_unanswered "r7-no-rc-$call" "Refuse a retirement call that did not answer"
  if [ "$call" = reserve ] || [ "$call" = abandon ] || [ "$call" = acknowledge ]; then
    retire_parser_case "r7-state-$call" "$call" "$fixture" 0 answer state '"unknown"'
    retire_member_refused "r7-state-$call" state
  fi
done
# The closing observation may be unknown even on success. The classifier
# guarantees hold; a terminal request still leaves its caller holding too.
retire_parser_case r7-state-classify classify dry-run-adopt 0 answer state '"unknown"'
expect_allowed r7-state-classify
expect_play_task_ran r7-state-classify "Prove the parser published this invocation's typed operands"
retire_parser_case r7-state-classify-non-hold classify dry-run-request 0 answer state '"unknown"'
retire_member_refused r7-state-classify-non-hold state
retire_parser_case r7-state-request request retired-settled 0 answer state '"unknown"'
expect_allowed r7-state-request
expect_play_task_ran r7-state-request "Prove the parser published this invocation's typed operands"
for rc in -9 1 124 137; do
  retire_parser_case "r7-unanswered-$rc" classify dry-run-request "$rc"
  retire_unanswered "r7-unanswered-$rc" "Refuse a retirement call that did not answer"
done
retire_parser_case r7-empty classify dry-run-request 0 raw stdout '""'
retire_unanswered r7-empty "Refuse a retirement call that did not answer"
retire_parser_case r7-escalation classify dry-run-request 0 raw failed true
retire_unanswered r7-escalation "Refuse a retirement call that did not answer"
retire_parser_case r7-schema classify dry-run-request 0 answer schema '"1"'
retire_member_refused r7-schema schema
# An optional next really is optional; the committed refusal omits it.
retire_parser_case r7-refused reserve refused-backend 2
expect_refused r7-refused "Refuse the retirement's answer" 'was refused' '(backend, state'
retire_parser_case r7-unknown request unknown-marker 3
expect_refused r7-unknown "Refuse the retirement's answer" 'was unknown' '(marker, state'
echo "ok   R7: all six calls, the route and member rules, every exit mismatch, and unanswered invocations"
