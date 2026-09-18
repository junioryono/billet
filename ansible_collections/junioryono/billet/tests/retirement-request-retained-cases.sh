#!/usr/bin/env bash
# Sourced by retirement-cases.sh inside the request-retained section.
#
# WHAT THESE CASES PROVE AND WHAT THEY DO NOT. The role is real, the files are real and the calls are real, but the retirement answers are committed corpus fixtures a fake answerer replays. The corpus has no release inspection of a host carrying BOTH roles, so `control-a`'s own inspection here is a controller's: `has_node` is false, its node name and endpoint are null and it has no node unit. The document the role assembles therefore names `control-a` as a node host whose own report says it bears no node, which the real command would refuse on the node chain. What these cases establish is the ROLE's construction — the variant it selects, the refusals it reaches before reserving, the operands it passes, the bytes it renders and the answers it accepts — not that a real deployment shaped this way is eligible. A both-role inspection fixture and the command's own judgement of it belong to commit 5's fleet acceptance.

r_new_retained() {
  local name=$1
  r_new "$name"
  # The four removable keys must all be present, or the projection proves nothing about removing them. The command answers remain committed fixtures, not invented inspections.
  "$python" - "$work/cases/$name" <<'PYCONFIG'
import pathlib, sys, yaml
case = pathlib.Path(sys.argv[1])
installed = yaml.safe_load((case / 'installed.yaml.plant').read_text())
installed.update(node=dict(name='control-a', server_addr='127.0.0.1:7717', provider='docker'),
                 targets=[], backup={})
(case / 'installed.yaml.plant').write_text(yaml.safe_dump(installed, sort_keys=False))
inventory = yaml.safe_load((case / 'inventory.yml').read_text())
self = inventory['all']['hosts']['control-a']
self.update(billet_config=installed, billet_enable_node=True, billet_server_prepare_only=False,
            billet_config_path='/etc/billet/control-a.yaml', node_wire_address='127.0.0.1:7717')
# The node members must stay unresolved here: each host renders its own desired document in its own context, and a literal name would make control-a's digest that of a document no host would render.
self['billet_config']['node'].update(name='{{ inventory_hostname }}', server_addr='{{ node_wire_address }}')
(case / 'inventory.yml').write_text(yaml.safe_dump(inventory, sort_keys=False))
PYCONFIG
  p "$name" 'cp /etc/billet/billet.yaml /etc/billet/control-a.yaml'
  # This entry skips main.yml's policy producer, so supply its expression rather than a fixed true fact, which would conceal both public policy refusals.
  a "$name" -e billet_gate_retained_request=true \
    -e '{"billet_node_should_run":"{{ billet_enable_node and not (billet_server_prepare_only | default(false)) }}"}'
  e "$name" "BILLET_GATE_REPORT_ANSWERS=control-a:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-a:status:1:$here/fixtures/rollout-status/retirement-reserved.json:0;control-a:migrate-endpoint:1:$here/fixtures/node-migrate-endpoint/reported-unplanned.json:0;control-b:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-b:status:1:$here/fixtures/rollout-status/no-rollout.json:0;control-b:migrate-endpoint:1:$here/fixtures/node-migrate-endpoint/reported-unplanned.json:0;node-a:release:1:$here/fixtures/release-inspect/node-with-bundle.json:0;node-a:migrate-endpoint:1:$here/fixtures/node-migrate-endpoint/reported-planned.json:0"
  r_answers "$name" 'control-a:classify:1:dry-run-new-request-retained.json:0;control-a:reserve:1:reserved.json:0;control-a:request:1:retired-retained-settled.json:0;control-a:abandon:1:abandoned.json:0'
}
r_retained_capture() {
  mkdir -p "$work/cases/$1/ordinary-render"
  a "$1" -e billet_gate_capture_rendering=true -e billet_gate_expect_retained_result=true \
    -e "billet_gate_ordinary_template=$role_tasks/../templates/billet.yaml.j2" \
    -e "billet_gate_render_dir=$work/cases/$1/ordinary-render" \
    -e '{"billet_retirement_shared_addresses":["192.0.2.10","192.0.2.11"],"billet_retirement_endpoint_failover_verified":true}'
}

# R19: B and the full self rendering have separate byte oracles, and a settled first answer must pass the request's own retained variant and receipt binding.
r_new_retained r19-retained-fresh-self-evidence
r_retained_capture r19-retained-fresh-self-evidence
r_run r19-retained-fresh-self-evidence
expect_allowed r19-retained-fresh-self-evidence
expect_host_commands r19-retained-fresh-self-evidence 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-request 1;'
expect_ran r19-retained-fresh-self-evidence "Prepare this host's authority exclusion"
expect_ran r19-retained-fresh-self-evidence 'Render the retained configuration after binding the reservation'
expect_ran r19-retained-fresh-self-evidence "Keep the new retirement's result"
expect_play_task_ran r19-retained-fresh-self-evidence 'Prove the fresh retained answer was accepted'
r_no_ordinary_after_request r19-retained-fresh-self-evidence
r_new_document r19-retained-fresh-self-evidence reserved retained-node

# The installed role and inventory predicate disagree in opposite directions.
r_new_retained r2-retained-installed-node-missing
"$python" - "$work/cases/r2-retained-installed-node-missing/inventory.yml" <<'PYMISSING'
import pathlib, sys, yaml
path = pathlib.Path(sys.argv[1])
inventory = yaml.safe_load(path.read_text())
self = inventory['all']['hosts']['control-a']
del self['billet_config']['node']
self['billet_enable_node'] = False
path.write_text(yaml.safe_dump(inventory, sort_keys=False))
PYMISSING
r_run r2-retained-installed-node-missing
expect_refused r2-retained-installed-node-missing 'Require inventory to retain the installed node' \
  'the installed configuration has both server and node, and inventory bears no node' \
  'Restore the installed node in billet_config to retain it' \
  'first converge that removal as an ordinary change with billet_server_retire false'
r_no_collection r2-retained-installed-node-missing
expect_host_commands r2-retained-installed-node-missing 'control-a retire-classify 1;'

r_new_retained r2-retained-inventory-node-not-installed
"$python" - "$work/cases/r2-retained-inventory-node-not-installed/installed.yaml.plant" <<'PYUNINSTALLED'
import pathlib, sys, yaml
path = pathlib.Path(sys.argv[1])
installed = yaml.safe_load(path.read_text())
del installed['node']
path.write_text(yaml.safe_dump(installed, sort_keys=False))
PYUNINSTALLED
r_answers r2-retained-inventory-node-not-installed 'control-a:classify:1:dry-run-new-request.json:0'
r_run r2-retained-inventory-node-not-installed
expect_refused r2-retained-inventory-node-not-installed 'Refuse an inventory node that is not installed' \
  'the installed configuration is server-only, and inventory bears a node' \
  'Remove the uninstalled node from inventory for server-only retirement' \
  'first converge its installation as an ordinary change with billet_server_retire false'
r_no_collection r2-retained-inventory-node-not-installed
expect_host_commands r2-retained-inventory-node-not-installed 'control-a retire-classify 1;'

for policy in node-off prepare-only; do
  name=r2-retained-$policy
  r_new_retained "$name"
  case "$policy" in
    node-off)
      a "$name" -e '{"billet_enable_node":false}'
      why='billet_enable_node=False and billet_server_prepare_only=False' ;;
    prepare-only)
      a "$name" -e '{"billet_server_prepare_only":true}'
      why='billet_enable_node=True and billet_server_prepare_only=True' ;;
  esac
  r_run "$name"
  expect_refused "$name" 'Require a policy that keeps the retained node running' \
    'retained-node retirement requires billet_node_should_run true' "$why" \
    'Set billet_enable_node true and billet_server_prepare_only false before requesting retirement'
  r_no_collection "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;'
done

r_new_retained r2-retained-preview-keeps-node
a r2-retained-preview-keeps-node --check -e billet_gate_peer=false
r_run r2-retained-preview-keeps-node
expect_allowed r2-retained-preview-keeps-node
expect_ran r2-retained-preview-keeps-node 'Report the prospective new retirement'
expect_host_commands r2-retained-preview-keeps-node 'control-a retire-classify 1;'
r_no_collection r2-retained-preview-keeps-node
PYTHONPATH="$here" "$python" -B - "$work/cases/r2-retained-preview-keeps-node/out" <<'PYPREVIEW'
import pathlib, sys
from callback_result import callback_message
text = pathlib.Path(sys.argv[1]).read_text()
parts = text.split('TASK [junioryono.billet.host : Report the prospective new retirement]')
if len(parts) != 2:
    sys.exit('retained preview was not reported exactly once')
message = callback_message(parts[1].split('TASK [', 1)[0], 'retained preview')
for fragment in ['Would request a retained-node retirement through survivor control-b.',
                 'This host keeps its node.', 'A real run needs that survivor prepared',
                 'check mode reserves and collects nothing.']:
    if fragment not in message:
        sys.exit('retained preview omitted: ' + fragment)
PYPREVIEW

# Both corrupted answers remain parser-valid, and each fails exactly one clause. The second keeps the receipt this converge's own variant pairs with, so only the variant equality can refuse it; corrupting the receipt too would leave that equality unproved.
for kind in no-receipt wrong-variant; do
  name=r19-retained-$kind
  r_new_retained "$name"
  "$python" - "$work/retire-fixtures" "$work/cases/$name" "$kind" <<'PYPAIR'
import json, pathlib, shutil, sys
fixtures, case = map(pathlib.Path, sys.argv[1:3])
for name in ['dry-run-new-request-retained', 'reserved', 'abandoned']:
    shutil.copyfile(fixtures / (name + '.json'), case / (name + '.json'))
answer = json.loads((fixtures / 'retired-retained-settled.json').read_text())
if answer.get('variant') != 'retained-node' or answer.get('receipt') != 'written':
    sys.exit('the retained success fixture no longer has the expected pairing')
if sys.argv[3] == 'wrong-variant':
    answer['variant'] = 'server-only'
else:
    answer['receipt'] = 'none'
(case / 'retired-retained-settled.json').write_text(json.dumps(answer))
PYPAIR
  e "$name" "BILLET_GATE_RETIRE_FIXTURES=$work/cases/$name"
  r_run "$name"
  expect_refused "$name" 'Require the request to confirm the selected retirement variant' \
    'did not confirm this retained-node retirement' 'written or current for retained-node'
  expect_final "$name" 'The reservation was abandoned.' 'Ordinary host tasks remain bypassed.'
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-request 1;control-a retire-abandon 1;'
  expect_no_task "$name" "Keep the new retirement's result"
  r_no_ordinary_after_request "$name"
done

# These are comparator negatives rather than command-refusal witnesses: the corpus has no retained rendering-drift refusal, and the fake still answers success.
for rendering in serverless self; do
  name=r19-retained-dropped-$rendering-member
  r_new_retained "$name"
  r_retained_capture "$name"
  case "$rendering" in
    serverless)
      cat >"$work/cases/$name/mutation.yml" <<'VARS'
billet_retirement_serverless_rendering: >-
  {{ lookup('ansible.builtin.template', 'billet.yaml.j2',
            template_vars={'billet_config_document':
              billet_config | junioryono.billet.serverless_config | dict2items |
              rejectattr('key', 'equalto', 'tiers') | items2dict}) }}
VARS
      why='retained B differs from independently projected ordinary template bytes' ;;
    self)
      cat >"$work/cases/$name/mutation.yml" <<'VARS'
billet_retirement_collect_rendering: >-
  {{ lookup('ansible.builtin.template', 'billet.yaml.j2',
            template_vars={'billet_config_document':
              (billet_retirement_collect_vars.billet_config | combine({'server':
                billet_retirement_collect_vars.billet_config.server | dict2items |
                rejectattr('key', 'equalto', 'max_vcpu') | items2dict}))
              if billet_retirement_collect_host == 'control-a'
              else billet_retirement_collect_vars.billet_config}) }}
VARS
      why='collected rendering differs from ordinary role template bytes for control-a' ;;
  esac
  a "$name" -e "@$work/cases/$name/mutation.yml"
  r_run "$name"
  expect_allowed "$name"
  expect_play_task_ran "$name" 'Prove the fresh retained answer was accepted'
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-request 1;'
  r_no_ordinary_after_request "$name"
  if r_new_document "$name" reserved retained-node >"$work/cases/$name/mutation-out" 2>&1; then
    fail "$name: the byte comparator accepted a rendering with a missing member"
  fi
  grep -qFx "$why" "$work/cases/$name/mutation-out" || fail "$name: the comparator failed for another reason" "$work/cases/$name/mutation-out"
done
