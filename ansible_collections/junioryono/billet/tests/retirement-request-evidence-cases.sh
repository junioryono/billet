#!/usr/bin/env bash
# Sourced by retirement-cases.sh inside the request-evidence section.

# R2: each static clause fails alone, in both modes. Missing preparation is
# a real-run precondition, separately exercised by R16 below.
for clause in upgrade policy survivor; do
  for mode in normal check; do
    name=r2-$clause-$mode
    r_new "$name"
    case "$clause" in
      upgrade) a "$name" -e billet_gate_binary_upgrade=true; task='Require retirement outside a binary upgrade'; why='inside a binary upgrade' ;;
      policy) a "$name" -e '{"billet_requested_server_should_run":true}'; task='Require a policy that leaves the retiring server stopped'; why='the policy will start the server' ;;
      survivor) a "$name" -e billet_retirement_survivor_host=; task='Require a named survivor in this inventory'; why='no distinct inventory survivor' ;;
    esac
    if [ "$mode" = check ]; then a "$name" --check; fi
    r_run "$name"
    expect_refused "$name" "$task" "Retirement precondition: $why"
    r_no_collection "$name"
    expect_host_commands "$name" 'control-a retire-classify 1;'
  done
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

# §4.4 on the fresh request: its first answer can already be settled, so it
# never reaches the continuation's pairing. A server-only answer claiming a
# receipt passes the strict parser; only the request's own pairing refuses it.
r_new r19-server-with-receipt
"$python" - "$work/retire-fixtures" "$work/cases/r19-server-with-receipt" <<'PYRECEIPT'
import json, pathlib, shutil, sys
fixtures, case = map(pathlib.Path, sys.argv[1:3])
for name in ['dry-run-new-request', 'reserved', 'abandoned']:
    shutil.copyfile(fixtures / (name + '.json'), case / (name + '.json'))
answer = json.loads((fixtures / 'retired-settled.json').read_text())
if answer.get('variant') != 'server-only' or answer.get('receipt') != 'none':
    sys.exit('retired-settled.json is no longer a server-only answer with no receipt')
answer['receipt'] = 'written'
(case / 'retired-settled.json').write_text(json.dumps(answer))
PYRECEIPT
e r19-server-with-receipt "BILLET_GATE_RETIRE_FIXTURES=$work/cases/r19-server-with-receipt"
r_run r19-server-with-receipt
# failed_at names the first failure; the rescue's final fail carries the
# original task name and the cleanup result after it.
expect_refused r19-server-with-receipt 'Require the request to confirm the selected retirement variant' \
  'none for server-only' 'Afterwards: ' 'Ordinary host tasks remain bypassed.'
expect_no_task r19-server-with-receipt "Keep the new retirement's result"
r_no_ordinary_after_request r19-server-with-receipt

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
            template_vars={'billet_config_document':
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

