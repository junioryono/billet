#!/usr/bin/env bash
# Sourced by retirement-cases.sh inside the request-collection section.

# R16: both hosts prepare successfully in this play, then the survivor fails
# before the old reset position or loses its real SSH connection there. A third
# case removes it before reinclusion, leaving settled facts to test active-host
# membership independently of the reset. Shared namespace preparation is serial.
cat >"$work/play-retirement-survivor.yml" <<'PLAY'
---
- name: Refuse a survivor removed after its first successful preparation
  hosts: control-a:control-b
  gather_facts: false
  vars:
    billet_exclusion_platform: Linux
    billet_binary_src: ''
    billet_requested_server_should_run: false
  tasks:
    - name: Prepare the retiring transport first
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
      vars:
        billet_allow_converge_from_billet_runner: true
      when: inventory_hostname == 'control-a'
    - name: Prepare the survivor transport first
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
      vars:
        billet_allow_converge_from_billet_runner: true
      when: inventory_hostname == 'control-b'
    - name: Prove the survivor completed its first preparation
      ansible.builtin.assert:
        that:
          - billet_exclusion_settled is sameas true
          - billet_exclusion_held is sameas true
          - billet_exclusion_holder == 'ci-1'
          - billet_upgrade_claim_shape == 'guard'
          - billet_exclusion_answerer == '/usr/bin/billet'
          - billet_exclusion_id | length > 0
      when: inventory_hostname == 'control-b'
    - name: Save the held survivor's run facts
      ansible.builtin.set_fact:
        billet_gate_first_guard:
          held: "{{ billet_exclusion_held }}"
          holder: "{{ billet_exclusion_holder }}"
          id: "{{ billet_exclusion_id }}"
          shape: "{{ billet_upgrade_claim_shape }}"
          interrupted: "{{ billet_interrupted_upgrade }}"
          recovery: "{{ billet_upgrade_recovery_dir }}"
      when: inventory_hostname == 'control-b'
    - name: Remove the survivor before a second inclusion
      ansible.builtin.fail:
        msg: The survivor failed after preparation and before reinclusion.
      when: inventory_hostname == 'control-b' and billet_gate_survivor_failure == 'inactive'
    - name: Lose the survivor's connection before its second preparation
      ansible.builtin.set_fact:
        ansible_connection: ssh
        ansible_host: 127.0.0.1
        ansible_port: 0
        ansible_connect_timeout: 1
        ansible_ssh_common_args: '-o BatchMode=yes -o ConnectionAttempts=1'
      when: inventory_hostname == 'control-b' and billet_gate_survivor_failure == 'unreachable'
    - name: Prepare the survivor a second time
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
      vars:
        billet_allow_converge_from_billet_runner: "{{ billet_gate_survivor_failure != 'failed' }}"
      when: inventory_hostname == 'control-b'
    - name: Prove the removed survivor's publication and held identity
      vars:
        peer: "{{ hostvars['control-b'] }}"
      ansible.builtin.assert:
        that:
          - "'control-b' not in ansible_play_hosts"
          - "'control-b' in ansible_play_hosts_all"
          - peer.billet_exclusion_settled is sameas (billet_gate_survivor_failure == 'inactive')
          - billet_gate_survivor_failure == 'inactive' or peer.billet_exclusion_answerer == ''
          - billet_gate_survivor_failure == 'inactive' or peer.billet_exclusion_answered_now is sameas false
          - billet_gate_survivor_failure == 'inactive' or peer.billet_exclusion_answerer_version == {'type':'unreadable'}
          - peer.billet_exclusion_held == peer.billet_gate_first_guard.held
          - peer.billet_exclusion_holder == peer.billet_gate_first_guard.holder
          - peer.billet_exclusion_id == peer.billet_gate_first_guard.id
          - peer.billet_upgrade_claim_shape == peer.billet_gate_first_guard.shape
          - peer.billet_interrupted_upgrade == peer.billet_gate_first_guard.interrupted
          - peer.billet_upgrade_recovery_dir == peer.billet_gate_first_guard.recovery
      when: inventory_hostname == 'control-a'
    - name: Route the retiring host with the failed survivor still in hostvars
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: retirement
      when: inventory_hostname == 'control-a'
PLAY
for failure in failed unreachable inactive; do
  name=r16-second-$failure
  r_new "$name"
  e "$name" RUNNER_NAME=billet-survivor-failure
  a "$name" -e "billet_gate_survivor_failure=$failure"
  r_run "$name" play-retirement-survivor
  case "$failure" in
    failed) task='Refuse a converge driven from a billet-managed runner'; why='runner billet itself manages' ;;
    unreachable) task='Inspect the managed binary and the upgrade root'; why='UNREACHABLE!' ;;
    inactive) task='Remove the survivor before a second inclusion'; why='before reinclusion' ;;
  esac
  expect_refused "$name" "$task" "$why"
  "$python" - "$work/cases/$name/out" <<'PYPREPARED'
import pathlib, sys
text = pathlib.Path(sys.argv[1]).read_text()
for task, host in [("Prove the survivor completed its first preparation", 'control-b'),
                   ("Prove the removed survivor's publication and held identity", 'control-a')]:
    parts = text.split('TASK [' + task + ']')
    if len(parts) != 2 or ('ok: [' + host + ']') not in parts[1].split('TASK [', 1)[0]:
        sys.exit('R16 did not prove preparation/publication on ' + host + ': ' + task)
PYPREPARED
  expect_final "$name" 'Retirement precondition: survivor control-b is no longer active in this play (failure, unreachability, or an intentional end of that host).' 'Resolve the cause, then converge the survivor in the same play as the retiring host.'
  r_no_collection "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;'
done

# R16: successful preparation in an earlier play leaves settled hostvars but
# supplies no evidence that the survivor stayed healthy outside this play.
cat >"$work/play-retirement-earlier-survivor.yml" <<'PLAY'
---
- name: Prepare the survivor in an earlier play
  hosts: control-b
  gather_facts: false
  vars:
    billet_exclusion_platform: Linux
    billet_binary_src: ''
  tasks:
    - name: Prepare the survivor transport
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
- name: Attempt retirement in a separate play
  hosts: control-a
  gather_facts: false
  vars:
    billet_exclusion_platform: Linux
    billet_binary_src: ''
    billet_requested_server_should_run: false
  tasks:
    - name: Prepare the retiring transport
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: prepare-exclusion
    - name: Prove the earlier survivor's settled facts remain outside this play
      vars:
        peer: "{{ hostvars['control-b'] }}"
      ansible.builtin.assert:
        that:
          - "'control-b' not in ansible_play_hosts_all"
          - "'control-b' not in ansible_play_hosts"
          - peer.billet_exclusion_settled is sameas true
          - peer.billet_exclusion_held is sameas true
          - peer.billet_exclusion_holder == billet_exclusion_holder
          - peer.billet_upgrade_claim_shape == 'guard'
          - peer.billet_exclusion_answerer == '/usr/bin/billet'
          - peer.billet_exclusion_id | length > 0
    - name: Route retirement with only earlier-play survivor evidence
      ansible.builtin.include_role:
        name: junioryono.billet.host
        tasks_from: retirement
PLAY
r_new r16-earlier-play
r_run r16-earlier-play play-retirement-earlier-survivor
expect_refused r16-earlier-play "Require the survivor in the retiring host's play" \
  'Retirement precondition: survivor control-b is outside this play.' \
  'Preparation in an earlier play cannot prove the survivor stayed healthy; converge the survivor in the same play as the retiring host.'
expect_play_task_ran r16-earlier-play "Prove the earlier survivor's settled facts remain outside this play"
r_no_collection r16-earlier-play
expect_host_commands r16-earlier-play 'control-a retire-classify 1;'

# R16: the other clauses remain true, so settled alone, a skipped settlement,
# another holder and a binary pointer each have an independent witness.
for clause in unsettled holder pointer unheld; do
  name=r16-$clause
  r_new "$name"
  a "$name" -e "billet_gate_peer_clause=$clause"
  r_run "$name"
  expect_refused "$name" "Require this converge's settled survivor guard" 'needs a settled, held, pointer-free guard'
  r_no_collection "$name"
  expect_host_commands "$name" 'control-a retire-classify 1;'
done
# R13: node-a's connection plugin really cannot connect, after the row was
# reserved and the survivor was collected. No module exit stands for this.
r_new r13-unreachable
e r13-unreachable BILLET_GATE_RETIRE_OBLIGATION=/var/lib/billet/gate-reservation
cat >>"$work/cases/r13-unreachable/inventory.yml" <<'INV'
      ansible_connection: ssh
      ansible_host: 127.0.0.1
      ansible_port: 0
      ansible_connect_timeout: 1
      ansible_ssh_common_args: '-o BatchMode=yes -o ConnectionAttempts=1'
INV
r_run r13-unreachable
expect_refused r13-unreachable "Collect the host's release inspection" 'node-a release inspection was unsuccessful or unreachable'
expect_final r13-unreachable 'The reservation was abandoned.'
expect_host_commands r13-unreachable 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-abandon 1;'
r_no_ordinary_after_request r13-unreachable
expect_path_absent r13-unreachable /var/lib/billet/gate-reservation
"$python" - "$work/cases/r13-unreachable" <<'PY'
import json, pathlib, sys
case = pathlib.Path(sys.argv[1])
text = (case / 'out').read_text()
if 'UNREACHABLE!' not in text or 'node-a' not in text:
    sys.exit('R13 did not establish connection-layer UNREACHABLE')
header = 'TASK [junioryono.billet.host : Require a successful delegated release inspection]'
blocks = text.split(header)[1:]
failures = [b.split('TASK [', 1)[0] for b in blocks if 'fatal: [control-a]:' in b.split('TASK [', 1)[0]]
if len(failures) != 1 or 'node-a release inspection was unsuccessful or unreachable' not in failures[0] or 'UNREACHABLE!' in failures[0]:
    sys.exit('R13 did not convert unreachable into an ordinary retiring-host assertion failure')
calls = [json.loads(line) for line in (case / 'calls/index.jsonl').read_text().splitlines()]
if any(c['host'] == 'node-a' for c in calls):
    sys.exit('an unreachable transport ran a module')
if sum(c['host'] == 'control-b' and c['command'] == 'release' for c in calls) != 1:
    sys.exit('R13 failed before reaching delegated collection')
abandon = [c for c in calls if c['command'] == 'retire-abandon']
if len(abandon) != 1 or abandon[0]['argv'] != ['server', 'retire', '--abandon-reservation', '--json', '--run', 'ci-1', '--retiring-host', 'control-a', '--config', '/etc/billet/billet.yaml', '--environment-file', '/etc/billet/server.env']:
    sys.exit('R13 cleanup did not carry exact local abandonment argv')
PY

# Missing desired rendering, missing prepared answerer and failed status may
# not silently remove a node or survivor from the collected document.
for clause in rendering answerer status; do
  name=r-collection-$clause
  r_new "$name"
  case "$clause" in
    rendering)
      "$python" - "$work/cases/$name/inventory.yml" <<'PYRENDER'
import pathlib, sys, yaml
p = pathlib.Path(sys.argv[1])
d = yaml.safe_load(p.read_text())
d['all']['hosts']['node-a'].update(billet_config={}, billet_enable_node=True)
p.write_text(yaml.safe_dump(d))
PYRENDER
      task="Require this node's complete desired configuration"; why='node node-a has no complete desired rendering' ;;
    answerer)
      a "$name" -e billet_gate_no_answerer=true
      task='Require a prepared answerer for every collected host'; why='node-a has no prepared answerer' ;;
    status)
      e "$name" "BILLET_GATE_REPORT_ANSWERS=control-a:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-a:status:1:$here/fixtures/rollout-status/retirement-reserved.json:0;control-b:release:1:$here/fixtures/release-inspect/postgres-controller-guarded.json:0;control-b:status:1:$here/fixtures/rollout-status/no-rollout.json:3"
      task='Require a successful delegated ledger status'; why='control-b ledger status was unsuccessful or unreachable' ;;
  esac
  r_run "$name"
  expect_refused "$name" "$task" "$why"
  expect_final "$name" 'The reservation was abandoned.'
  expect_host_commands "$name" 'control-a retire-classify 1;control-a retire-reserve 1;control-a retire-abandon 1;'
  r_no_ordinary_after_request "$name"
done

