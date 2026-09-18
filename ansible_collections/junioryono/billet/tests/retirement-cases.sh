#!/usr/bin/env bash
# Sourced by section R after its namespace runner and committed corpus exist.
# The isolated entry proves routing; the boundary invokes the real main.yml.

cat >"$work/play-retirement.yml" <<'PLAY'
---
- name: Capture the ordinary template in each node's context
  hosts: "{{ 'control-a:control-b:node-a' if billet_gate_retained_request | default(false) | bool else 'control-b:node-a' }}"
  gather_facts: false
  tasks:
    - name: Capture the complete ordinary role rendering
      vars:
        billet_config_document: "{{ billet_config }}"
      ansible.builtin.template:
        src: "{{ billet_gate_ordinary_template }}"
        dest: "{{ billet_gate_render_dir }}/{{ inventory_hostname }}.yaml"
        mode: "0600"
      when: billet_gate_capture_rendering | default(false) | bool
      no_log: true
    - name: Capture the independently projected retained rendering
      vars:
        billet_config_document: "{{ billet_config | dict2items | rejectattr('key', 'in', ['server', 'github', 'targets', 'backup']) | items2dict }}"
      ansible.builtin.template:
        src: "{{ billet_gate_ordinary_template }}"
        dest: "{{ billet_gate_render_dir }}/control-a-serverless.yaml"
        mode: "0600"
      when:
        - billet_gate_capture_rendering | default(false) | bool
        - billet_gate_retained_request | default(false) | bool
        - inventory_hostname == 'control-a'
      no_log: true
- name: Exercise the retirement entry after real preparation
  hosts: "{{ [billet_gate_retirement_host | default('control-a'), 'control-b'] if billet_gate_new | default(false) | bool else billet_gate_retirement_host | default('control-a') }}"
  gather_facts: "{{ billet_gate_new | default(false) | bool }}"
  vars:
    billet_exclusion_platform: Linux
    billet_binary_src: ''
    billet_requested_server_should_run: false
  tasks:
    - name: Exercise only the selected retiring host
      ansible.builtin.include_tasks: retirement-entry-tasks.yml
      when: inventory_hostname == billet_gate_retirement_host | default('control-a')
PLAY
cat >"$work/retirement-entry-tasks.yml" <<'PLAY'
---
- name: Drive the selected retirement entry
  block:
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
    - name: Publish new-request collection answerers on the node transports
      ansible.builtin.set_fact:
        billet_exclusion_answerer: "{{ billet_exclusion_answerer }}"
        billet_exclusion_darwin: false
      delegate_to: "{{ item }}"
      delegate_facts: true
      loop: [control-b, node-a]
      when:
        - billet_gate_new | default(false) | bool
        - item != 'node-a' or not billet_gate_no_answerer | default(false) | bool
    - name: Vary only the named survivor guard clause
      ansible.builtin.set_fact:
        billet_exclusion_settled: "{{ false if billet_gate_peer_clause == 'unsettled' else true }}"
        billet_exclusion_held: "{{ false if billet_gate_peer_clause == 'unheld' else true }}"
        billet_exclusion_holder: "{{ 'someone-else' if billet_gate_peer_clause == 'holder' else billet_exclusion_holder }}"
        billet_upgrade_claim_shape: "{{ 'guard-pointer' if billet_gate_peer_clause == 'pointer' else 'guard' }}"
      delegate_to: control-b
      delegate_facts: true
      when: billet_gate_peer_clause is defined
    - name: Vary the binary-upgrade precondition after preparation
      ansible.builtin.set_fact:
        billet_binary_upgrade: true
      when: billet_gate_binary_upgrade | default(false) | bool
    - name: Inject an unfinished own guard after preparation
      ansible.builtin.command:
        argv: ["{{ ansible_playbook_python }}", "{{ billet_gate_own_guard_script }}", "{{ billet_gate_own_guard }}"]
      changed_when: true
      when: billet_gate_own_guard is defined
    - name: Route this host before ordinary work
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: retirement
    - name: Prove the fresh retained answer was accepted
      ansible.builtin.assert:
        that:
          - billet_retirement_requested_variant == 'retained-node'
          - billet_retirement_result.variant == 'retained-node'
          - billet_retirement_result.receipt == 'written'
          - billet_retirement_result.settled is sameas true
          - billet_retirement_bypass is sameas true
      when: billet_gate_expect_retained_result | default(false) | bool
    - name: Ordinary convergence sentinel
      ansible.builtin.debug:
        msg: The ordinary boundary was reached.
      when: not billet_retirement_bypass | bool
    - name: Record the ordinary cancellation effect
      ansible.builtin.command:
        argv: [/bin/sh, -ec, 'printf reached > /var/lib/billet/gate-cancel-ordinary']
      changed_when: true
      when:
        - billet_gate_cancel | default(false) | bool
        - not billet_retirement_bypass | bool
    - name: Prove the route left the bypass in its terminal state
      ansible.builtin.assert:
        that:
          - >-
            billet_retirement_bypass is sameas
            (billet_retirement_route != 'ordinary'
             and not (billet_retirement_route == 'cancel' and not ansible_check_mode)
             and not (ansible_check_mode and billet_retirement_route == 'unverified-check-mode'))
        fail_msg: The route left the ordinary boundary open.
  always:
    - name: Observe the retained continuation's terminal boundary
      ansible.builtin.assert:
        that:
          - billet_retirement_bypass is sameas true
          - billet_retirement_node_ordinary is sameas false
      when: billet_gate_retained_continuation | default(false) | bool
    - name: Observe cancellation's terminal boundary
      ansible.builtin.debug:
        msg: "Cancellation bypass={{ billet_retirement_bypass | default(true) }}."
      when: billet_gate_cancel | default(false) | bool
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
  PYTHONPATH="$here" "$python" -B - "$work/cases/$1/${4:-out}" "$2" "${3:-}" <<'PYREPORT'
import pathlib, sys
from callback_result import callback_message
text = pathlib.Path(sys.argv[1]).read_text()
header = 'TASK [junioryono.billet.host : Report the retirement route]'
blocks = text.split(header)
if len(blocks) != 2:
    sys.exit('the retirement route was not reported exactly once')
block = blocks[1].split('TASK [', 1)[0]
message = callback_message(block, pathlib.Path(sys.argv[1]).parent.name + ': retirement route')
if not message.startswith('Retirement route ' + sys.argv[2] + ': '):
    sys.exit('the reported retirement route differs: ' + repr(message))
if sys.argv[3] and sys.argv[3] not in message:
    sys.exit('the reported retirement reason differs: ' + repr(message))
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

r_done_report() { # case True|False ok|changed
  PYTHONPATH="$here" "$python" -B - "$work/cases/$1/out" "$2" "$3" <<'PYDONE'
import pathlib, sys
from callback_result import callback_message
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
message = callback_message(report, pathlib.Path(sys.argv[1]).parent.name + ': retirement settlement')
if not message.startswith('Retirement is locally done; settled=' + sys.argv[2] + '.'):
    sys.exit('the done settlement report differs: ' + repr(message))
PYDONE
}

# Each source file belongs to exactly one CI shard. Shared helpers stay above.
if [ "${BILLET_GATE_ONLY:-}" != retirement-request ]; then
"$python" "$here/executable_version_check.py"

# Detached settled callers use the same recording fixture answerer as routing.
if retirement_section settled; then
  . "$here/retirement-settled-cases.sh"
  retirement_section_finished
fi
if retirement_section routes; then
  . "$here/retirement-routes-cases.sh"
  retirement_section_finished
fi
if retirement_section compatibility; then
  . "$here/retirement-compatibility-cases.sh"
  retirement_section_finished
fi
if retirement_section retained; then
  . "$here/retirement-retained-cases.sh"
  retirement_section_finished
fi
if retirement_section parity-basic; then
  . "$here/retirement-parity-basic-cases.sh"
  retirement_section_finished
fi
if retirement_section parity-network; then
  . "$here/retirement-parity-network-cases.sh"
  retirement_section_finished
fi
if retirement_section parity-negative; then
  . "$here/retirement-parity-negative-cases.sh"
  retirement_section_finished
fi
if retirement_section resume; then
  . "$here/retirement-resume-cases.sh"
  retirement_section_finished
fi
if retirement_section recovery; then
  . "$here/retirement-recovery-cases.sh"
  retirement_section_finished
fi
else
  echo "converge guard: retirement routes (R) skipped"
fi

# 5c.d1: new requests, with two node-bearing transports (including the survivor).
if [ "$skip_retirement_request" = 0 ]; then
# The namespace retains the invoker's account; use its non-root primary group
# too, because local prepare refuses either numeric identity resolving to zero.
[ "$invoker_gid" != 0 ] || fail 'retirement requests need a non-root invoker primary group'
r_service_group=$(id -gn "$invoker_name")
# Only the ordinary account/file/service block is forbidden: the explicit early
# service-account import is required on this route.
r_no_ordinary_after_request() {
  local task
  while IFS= read -r task; do
    expect_no_task "$1" "$task"
  done < <(sed -n 's/^[[:space:]]*- name: //p' "$role_tasks/account.yml" "$role_tasks/services.yml")
  expect_no_play_task "$1" 'Ordinary convergence sentinel'
  expect_no_task "$1" 'Inspect the transaction claim before recovery'
}
r_no_collection() {
  "$python" - "$work/cases/$1/calls/index.jsonl" <<'PY'
import json, sys
calls = [json.loads(line) for line in open(sys.argv[1])]
if any(c['command'] in ['release', 'status', 'migrate-endpoint', 'retire-reserve', 'retire-request', 'retire-abandon'] for c in calls):
    sys.exit('a precondition failure collected evidence or touched a reservation')
PY
  expect_no_task "$1" "Read the installed configuration's reservation digest"
  expect_no_task "$1" "Read the retiring host's clock before the first collection delegation"
  expect_no_ordinary "$1"
  expect_no_play_task "$1" 'Ordinary convergence sentinel'
}
r_new() { # case [reserved|adopted]
  local name=$1 outcome=${2:-reserved}
  r_plant "$name"
  r_services "$name" control-a
  # Root runs the real local prepare inside the namespace, recording the
  # invoker as the unprivileged service account without changing host accounts.
  "$python" - "$work/cases/$name/services/control-a.json" "$invoker_name" "$r_service_group" <<'PY'
import json, sys
p = sys.argv[1]
d = json.load(open(p))
for unit in d.values():
    unit.update(User=sys.argv[2], Group=sys.argv[3])
with open(p, 'w') as stream:
    json.dump(d, stream)
PY
  ep_plant_config "$name" 127.0.0.1:7717 server-only
  a "$name" -e billet_converge_guard_holder=ci-1 -e billet_gate_new=true -e billet_gate_peer=true \
    -e billet_retirement_survivor_host=control-b \
    -e "billet_service_user=$invoker_name" -e "billet_service_group=$r_service_group" -e '{"billet_adopt_existing_service_account": true}'
  cat >"$work/cases/$name/inventory.yml" <<'INV'
all:
  hosts:
    control-a:
      billet_server_retire: true
    control-b:
      billet_server_retire: false
      node_wire_address: 127.0.0.1:7717
      billet_effective_config: {node: {name: wrong-effective-survivor, server_addr: wrong-effective:7717, provider: docker}}
      billet_config:
        server: {listen: '127.0.0.1:7717', state_dir: /var/lib/billet/server, max_vcpu: 8, max_memory: 32GiB}
        node: {name: '{{ inventory_hostname }}', server_addr: '{{ node_wire_address }}', provider: docker}
    node-a:
      billet_server_retire: false
      billet_config_path: /etc/billet/node-a.yaml
      node_wire_address: 127.0.0.1:7719
      billet_effective_config: {node: {name: wrong-effective-node, server_addr: wrong-effective:7719, provider: docker}}
      billet_config:
        node: {name: '{{ inventory_hostname }}', server_addr: '{{ node_wire_address }}', provider: docker}
INV
  a "$name" -i "$work/cases/$name/inventory.yml"
  e "$name" BILLET_GATE_RETIRE_CLOCK=1
  e "$name" 'BILLET_GATE_RETIRE_ENV=/etc/billet/server.env (ignore_errors=yes)'
  e "$name" "BILLET_GATE_REPORT_ANSWERS=control-a:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-a:status:1:$here/fixtures/rollout-status/retirement-reserved.json:0;control-b:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-b:status:1:$here/fixtures/rollout-status/no-rollout.json:0;control-b:migrate-endpoint:1:$here/fixtures/node-migrate-endpoint/reported-unplanned.json:0;node-a:release:1:$here/fixtures/release-inspect/node-with-bundle.json:0;node-a:migrate-endpoint:1:$here/fixtures/node-migrate-endpoint/reported-planned.json:0"
  r_answers "$name" "control-a:classify:1:dry-run-new-request.json:0;control-a:reserve:1:$outcome.json:0;control-a:request:1:retired-settled.json:0;control-a:abandon:1:abandoned.json:0"
}

# Assert the entire reservation/request argv, all delegated argv, the exact
# document member sets, raw rendering hashes (including the final newline),
# host-specific variable expansion and both positive and forbidden effects.
r_new_document() { # case reserved|adopted [server-only|retained-node]
  "$python" - "$work/cases/$1" "$here/fixtures" "$2" "${3:-server-only}" <<'PY'
import datetime, hashlib, json, pathlib, re, sys, yaml
case, fixtures = map(pathlib.Path, sys.argv[1:3])
outcome = sys.argv[3]
retained = sys.argv[4] == 'retained-node'
root = case / 'calls'
calls = [json.loads(line) for line in (root / 'index.jsonl').read_text().splitlines()]
def one(host, command):
    found = [c for c in calls if c['host'] == host and c['command'] == command]
    if len(found) != 1:
        sys.exit(f'{host} {command}: expected one call, got {len(found)}')
    return found[0]
def fixture(command, name):
    return json.loads((fixtures / command / (name + '.json')).read_text())
reserve, request = one('control-a', 'retire-reserve'), one('control-a', 'retire-request')
digest = hashlib.sha256((case / 'installed.yaml.plant').read_bytes()).hexdigest()
common = ['--json', '--run', 'ci-1', '--retiring-host', 'control-a', '--survivor-host', 'control-b', '--installed-sha256', digest]
if reserve['argv'] != ['server', 'retire', '--reserve'] + common + ['--config', '/etc/billet/billet.yaml', '--environment-file', '/etc/billet/server.env']:
    sys.exit('reserve argv differs: ' + repr(reserve['argv']))
expected = ['server', 'retire', '--input', '-'] + common
if not retained:
    expected += ['--server-only']
expected += ['--config', '/etc/billet/billet.yaml']
if outcome == 'reserved':
    expected += ['--reservation-fresh']
expected += ['--shared-address', '192.0.2.10', '--shared-address', '192.0.2.11', '--endpoint-failover-verified', '--report-max-age', '10m', '--environment-file', '/etc/billet/server.env']
# The retiring request flag is host-local inventory; it must not flag its peer.
if request['argv'] != expected:
    sys.exit('request argv differs: ' + repr(request['argv']))
if not request['has_stdin']:
    sys.exit('request has no stdin')
doc = json.loads((root / request['stdin']).read_text())
if set(doc) != {'schema', 'round', 'self', 'survivor', 'nodes', 'desired', 'desired_nodes'} or type(doc['schema']) is not int or doc['schema'] != 1:
    sys.exit('new-request document shape differs')
if retained:
    if not isinstance(doc['desired'], str):
        sys.exit('retained desired is not a rendering')
    serverless = doc['desired'].encode('utf-8')
    ordinary = (case / 'ordinary-render/control-a.yaml').read_bytes()
    projected = (case / 'ordinary-render/control-a-serverless.yaml').read_bytes()
    if serverless != projected:
        sys.exit('retained B differs from independently projected ordinary template bytes')
    if serverless == ordinary or not serverless.endswith(b'\n') or not ordinary.endswith(b'\n'):
        sys.exit('retained B and full self rendering must be distinct bytes with final newlines')
    full, reduced = yaml.safe_load(ordinary), yaml.safe_load(serverless)
    removed = {'server', 'github', 'targets', 'backup'}
    if not isinstance(full, dict) or not removed <= set(full) or 'node' not in full:
        sys.exit('full self rendering lost a server member or its node')
    if not isinstance(reduced, dict) or 'node' not in reduced or removed & set(reduced):
        sys.exit('retained B has the wrong member set')
    if reduced != {key: value for key, value in full.items() if key not in removed}:
        sys.exit('retained B changed a member outside the four removed keys')
    if yaml.safe_load((case / 'installed.yaml.plant').read_bytes()) != full:
        sys.exit('retained installed and full desired configurations differ')
elif doc['desired'] is not None:
    sys.exit('server-only request carried a desired rendering')
if set(doc['round']) != {'id', 'started_at'} or doc['round']['id'] != 'ci-1-' + doc['round']['started_at']:
    sys.exit('round binding differs')
def timestamp(s):
    if not isinstance(s, str) or not re.fullmatch(r'\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{9}Z', s):
        sys.exit('collection time is not the retiring clock\'s nanosecond RFC 3339 answer')
    return datetime.datetime.fromisoformat(s.replace('Z', '+00:00'))
started = timestamp(doc['round']['started_at'])
reserved_at = datetime.datetime.fromisoformat(fixture('server-retire', outcome)['reserved_at'].replace('Z', '+00:00'))
if started < reserved_at:
    sys.exit('round precedes reservation')
node_hosts = {'control-a', 'control-b', 'node-a'} if retained else {'control-b', 'node-a'}
if set(doc['nodes']) != node_hosts or set(doc['desired_nodes']) != node_hosts:
    sys.exit('collection omitted a deployment node or collected another host')
if doc['nodes']['control-b'] != doc['survivor']:
    sys.exit('the node-bearing survivor needs its own nodes entry')
if retained and doc['nodes']['control-a'] != doc['self']:
    sys.exit('the retained host needs its full self envelope in nodes')
for host, envelope, inspect_name, status_name in [
    ('control-a', doc['self'], 'postgres-controller-guarded', 'retirement-reserved'),
    ('control-b', doc['survivor'], 'postgres-controller-guarded', 'no-rollout'),
    ('node-a', doc['nodes']['node-a'], 'node-with-bundle', None),
]:
    if set(envelope) != {'host', 'collected_at', 'inspect', 'status'} or envelope['host'] != host:
        sys.exit('envelope binding differs for ' + host)
    if timestamp(envelope['collected_at']) < started:
        sys.exit('envelope precedes round for ' + host)
    if envelope['inspect'] != fixture('release-inspect', inspect_name):
        sys.exit('inspection came from another host or was rewritten')
    expected_status = fixture('rollout-status', status_name) if status_name else None
    if envelope['status'] != expected_status:
        sys.exit('ledger document differs for ' + host)
    path = '/etc/billet/node-a.yaml' if host == 'node-a' else '/etc/billet/billet.yaml'
    if retained and host == 'control-a':
        path = '/etc/billet/control-a.yaml'
    inspection = one(host, 'release')
    if inspection['argv'] != ['release', 'inspect', '--json', '--config', path]:
        sys.exit('inspect argv differs for ' + host)
    if not reserve['sequence'] < inspection['sequence'] < request['sequence']:
        sys.exit('collection is outside the reserved request window')
    if status_name:
        status = one(host, 'status')
        if status['argv'] != ['rollout', 'status', '--json', '--config', path, '--environment-file', '/var/lib/billet-fixture/server.env']:
            sys.exit('status did not use its executing host\'s observed environment')
        if not inspection['sequence'] < status['sequence'] < request['sequence']:
            sys.exit('status collected out of order')
    if host in node_hosts:
        migration = one(host, 'migrate-endpoint')
        if migration['argv'] != ['node', 'migrate-endpoint', '--config', path, '--desired', '-', '--wait', '120s', '--dry-run', '--json']:
            sys.exit('endpoint dry-run argv differs for ' + host)
        rendering = (root / migration['stdin']).read_bytes()
        if not migration['has_stdin'] or not rendering.endswith(b'\n'):
            sys.exit('node did not receive exact configuration bytes')
        parsed = yaml.safe_load(rendering)
        address = '127.0.0.1:7719' if host == 'node-a' else '127.0.0.1:7717'
        if parsed['node'] != dict(name=host, server_addr=address, provider='docker'):
            sys.exit('configuration was not rendered in its own host context')
        ordinary = (case / 'ordinary-render' / (host + '.yaml')).read_bytes()
        if rendering != ordinary:
            sys.exit('collected rendering differs from ordinary role template bytes for ' + host)
        endpoint_name = 'reported-planned' if host == 'node-a' else 'reported-unplanned'
        desired = doc['desired_nodes'][host]
        if desired != {'sha256': hashlib.sha256(ordinary).hexdigest(), 'endpoint': fixture('node-migrate-endpoint', endpoint_name)['to']}:
            sys.exit('desired node evidence was not computed from exact stdin and its own answer')
        if not inspection['sequence'] < migration['sequence'] < request['sequence']:
            sys.exit('endpoint collected out of order')
        if status_name and not status['sequence'] < migration['sequence']:
            sys.exit('endpoint preceded ledger status for ' + host)
if any(c['host'] == 'node-a' and c['command'] == 'status' for c in calls):
    sys.exit('node with no ledger was asked for status')
if any(c['command'] in ['retire-abandon', 'retire-complete', 'retire-acknowledge'] for c in calls):
    sys.exit('a settled success ran cleanup or a pending-row helper')
clocks = [c for c in calls if c['command'] == 'collection-clock']
if len(clocks) != 4 or any(c['host'] != 'control-a' or c['argv'] != ['-u', '+%Y-%m-%dT%H:%M:%S.%NZ'] for c in clocks):
    sys.exit('collection did not read the retiring host clock once per round/envelope')
if not reserve['sequence'] < clocks[0]['sequence'] < one('control-a', 'release')['sequence']:
    sys.exit('round clock was not read freshly after reserve and before delegation')
for clock, host in zip(clocks[1:], ['control-a', 'control-b', 'node-a']):
    last_report = one(host, 'migrate-endpoint' if host in node_hosts else 'status')
    if not last_report['sequence'] < clock['sequence'] < request['sequence']:
        sys.exit('envelope clock was not read after its reports')
# local prepare is a real early import effect, before the reservation.
prepared = [c for c in calls if c['argv'][:2] == ['local', 'prepare']]
if len(prepared) != 1 or prepared[0]['host'] != 'control-a' or prepared[0]['sequence'] >= reserve['sequence']:
    sys.exit('early service-account preparation did not precede reservation')
PY
}

# Shared marker setup for the request failure and earlier-guard witnesses.
cat >"$work/retirement-window.py" <<'PY'
import json, pathlib, sys
root = pathlib.Path('/var/lib/billet')
record = root / 'upgrades/active/guard.json'
d = json.loads(record.read_text())
if (root / 'retired/journal.json').exists():
    sys.exit('R17 requires the journal still absent')
marker = {'kind': 'retirement', 'id': '0123456789abcdef0123456789abcdef'}
if sys.argv[1] == 'mark':
    if 'transition' in d:
        sys.exit('R17 started with a marker')
    d['transition'] = marker
elif sys.argv[1] == 'abandon':
    if d.get('transition') != marker:
        sys.exit('R17 abandonment did not see the request\'s marker')
    del d['transition']
else:
    sys.exit('unknown retirement window action')
record.write_text(json.dumps(d))
PY

if retirement_section request-evidence; then
  . "$here/retirement-request-evidence-cases.sh"
  retirement_section_finished
fi
if retirement_section request-collection; then
  . "$here/retirement-request-collection-cases.sh"
  retirement_section_finished
fi
if retirement_section request-windows; then
  . "$here/retirement-request-windows-cases.sh"
  retirement_section_finished
fi
if retirement_section request-cancellation; then
  . "$here/retirement-request-cancellation-cases.sh"
  retirement_section_finished
fi
if retirement_section request-retained; then
  . "$here/retirement-request-retained-cases.sh"
  retirement_section_finished
fi

sections_ran="$sections_ran, retirement requests (R)"
else
  echo "converge guard: retirement requests (R) skipped"
fi
