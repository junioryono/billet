#!/usr/bin/env python3
"""Namespace adapters, not a service manager or a retirement implementation.

Physical mutations: the real role writes its files; the sysctl adapter writes
only its config file; systemctl maintains the existing harness's JSON unit map;
endpoint migration submits stop/start to that adapter; start/restart assigns a
modeled invocation; receipt writes a modeled record. Retirement answers are
transport fixtures rebound to the observed guard and requested document. They
never constitute proof of admission or closure. The independent watcher and
assertions live in retirement-role-observe.py.
"""
import hashlib
import importlib.util
import json
import os
import pathlib
import subprocess
import sys

import yaml

NODE = 'billet-node.service'
TRANSITION = '2' * 32
CONFIG = pathlib.Path('/etc/billet/billet.yaml')
GUARD = pathlib.Path('/var/lib/billet/upgrades/active/guard.json')
MODEL = pathlib.Path('/var/lib/billet/gate-retirement')
_observer_spec = importlib.util.spec_from_file_location('retirement_role_observer', pathlib.Path(__file__).with_name('retirement-role-observe.py'))
observer = importlib.util.module_from_spec(_observer_spec)
_observer_spec.loader.exec_module(observer)


def root():
    return pathlib.Path(os.environ['BILLET_GATE_ROLE'])


def read(path):
    return json.loads(pathlib.Path(path).read_text())


def write(path, value):
    pathlib.Path(path).write_text(json.dumps(value, sort_keys=True) + '\n')


def log(name, value):
    with (root() / name).open('a') as stream:
        stream.write(json.dumps(value, sort_keys=True) + '\n')


def settings():
    return read(root() / 'role-settings.json')


def fixture(group, name):
    return read(pathlib.Path(__file__).parent / 'fixtures' / group / (name + '.json'))


def node():
    return read(root() / 'services/control-a.json')[NODE]


def ctl(*args):
    env = dict(os.environ, BILLET_GATE_HOST='control-a')
    subprocess.run(['/usr/bin/systemctl', *args], env=env, check=True)


def activity():
    state = node()['ActiveState']
    return state if state == 'active' else 'quiet-' + state


def answer(command, source, argv):
    cfg = settings()
    def operand(flag):
        return argv[argv.index(flag) + 1]
    raw = pathlib.Path(source).read_text() if source else ''
    result = None
    if command.startswith('retire-'):
        if command == 'retire-classify':
            result = fixture('server-retire', 'dry-run-continue-retained-done' if cfg['retained'] else 'dry-run-ordinary')
            if cfg['retained']:
                result['journal'] = read(MODEL / 'journal.json')
                result['row']['transition_id'] = TRANSITION
        elif command in ['retire-settled-entry', 'retire-settled-closing', 'retire-node-config']:
            purpose = command.removeprefix('retire-')
            state = node()['ActiveState']
            # These are canned caller answers selected from adapter state, not
            # a copy of the production predicate. Closing deliberately remains
            # healthy so an independent operation/FS observer must catch harm.
            result = fixture('retire-' + purpose, state if purpose != 'settled-closing' else 'active')
            guard = read(GUARD)
            result.update(run=guard['holder'], guard=guard['id'], transition_id=TRANSITION)
            if purpose == 'node-config':
                document = json.loads(raw)
                result.update(rendering_sha256=document['rendering_sha256'], operations_sha256=hashlib.sha256(json.dumps(document['operations'], ensure_ascii=True, separators=(',', ':')).encode()).hexdigest())
        else:
            raise ValueError('ordinary main unexpectedly continued retirement: ' + command)
    elif command == 'release':
        result = fixture('release-inspect', 'postgres-controller-guarded')
        result['services']['node']['unit_present'] = True
        result['services']['node']['environment_file_specs'] = []
        result['services']['node']['environment_files'] = []
    elif command == 'migrate-endpoint':
        desired = yaml.safe_load(raw)['node']['server_addr']
        current = node().get('Endpoint', desired)
        active = node()['ActiveState'] == 'active'
        planned = active and current != desired
        if '--dry-run' in argv:
            result = fixture('node-migrate-endpoint', 'reported-planned' if planned else 'reported-unplanned' if active else 'reported-first-start')
            result.update(planned=planned, first_start=not active, **{'from': current if active else None, 'to': desired, 'effective': current if active else None})
            result['unit']['active_state'] = node()['ActiveState']
        elif planned:
            ctl('stop', NODE)
            ctl('start', NODE)
            result = fixture('node-migrate-endpoint', 'migrated')
            result.update(**{'from': current, 'to': desired}, installed_sha256=hashlib.sha256(CONFIG.read_bytes()).hexdigest(), invocation_id=node()['InvocationID'])
        else:
            result = fixture('node-migrate-endpoint', 'unchanged' if active else 'unchanged-stopped')
            result.update(endpoint=desired, effective=current if active else None)
    elif command == 'registration':
        result = fixture('rollout-registration', 'confirmed')
        result['incarnation'] = operand('--incarnation')
    elif command == 'receipt':
        result = fixture('node-receipt', 'written')
        result['receipt'].update(run=operand('--run'), installed_sha256=hashlib.sha256(CONFIG.read_bytes()).hexdigest(),
                                 installed_endpoint=yaml.safe_load(CONFIG.read_text())['node']['server_addr'],
                                 effective_endpoint=node()['Endpoint'], invocation_id=node()['InvocationID'])
        receipt = pathlib.Path('/var/lib/billet/node/gate-receipt.json')
        if receipt.exists() and '--refresh' in argv:
            previous = read(receipt)
            if all(previous[key] == value for key, value in result['receipt'].items() if key != 'run'):
                result.update(outcome='current', receipt=previous)
        if result['outcome'] == 'written':
            write(receipt, result['receipt'])
    elif command == 'check':
        log('answers.jsonl', dict(pass_name=cfg['pass'], command=command, argv=argv, adapter='validation omitted'))
        print('namespace check adapter: configuration validation is not exercised')
        return
    elif command == 'local':
        if argv[:2] == ['local', 'capability-probe']:
            log('answers.jsonl', dict(pass_name=cfg['pass'], command=command, argv=argv, adapter='local capability'))
            print('local commands: prepare', file=sys.stderr)
            return
        if argv[:2] != ['local', 'prepare']:
            raise ValueError('unexpected local command: ' + repr(argv))
        result = dict(status='open', closed=False, identity_dir_absent=False, repaired=[], created_identity_dir=False)
    else:
        raise ValueError('unknown role answer: ' + command)
    log('answers.jsonl', dict(pass_name=cfg['pass'], command=command, argv=argv, answer=result, node_activity=activity()))
    print(json.dumps(result))


def action(handler, original, tmp, task_vars):
    task = handler._task
    module = observer.builtin_module(task.action)
    args = dict(task.args)
    name = task.get_name().split(' : ', 1)[-1]
    cfg = settings()
    source = task.get_path()
    phase = observer.guard_phase(module, args, source) if cfg['pass'] != 'seed' else None
    row = dict(pass_name=cfg['pass'], task=name, module=module, args=args, source=source, guard_phase=phase)
    log('tasks.jsonl', dict(row, event='before'))
    if cfg['retained'] and cfg['pass'] != 'seed':
        observer.require_observable_command(module, args, phase)
    if module == 'ansible.builtin.apt':
        # Package scripts do not belong to the namespace model.
        result = dict(changed=False)
    elif module == 'ansible.posix.sysctl':
        path = pathlib.Path(args['sysctl_file'])
        body = args['name'] + ' = ' + str(args['value']) + '\n'
        changed = not path.exists() or path.read_text() != body
        if changed and not task.check_mode:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(body)
        result = dict(changed=changed)
    elif module == 'ansible.builtin.command' and (
            str(args.get('_raw_params', '')).startswith(('networkctl reload', 'ip link show dev ', '/usr/sbin/nft -f '))):
        result = dict(changed=False, rc=0, stdout='', stderr='', stdout_lines=[], stderr_lines=[])
    else:
        if phase:
            observer.boundary(root(), phase)
        try:
            result = original(tmp=tmp, task_vars=task_vars)
        finally:
            if phase:
                observer.boundary(root(), 'ordinary')
    log('tasks.jsonl', dict(row, event='after', failed=bool(result.get('failed')), changed=bool(result.get('changed'))))
    if module == 'ansible.builtin.set_fact' and 'billet_node_should_run' in args:
        log('node-policy.jsonl', dict(
            pass_name=cfg['pass'], task=name,
            billet_enable_node=task_vars.get('billet_enable_node'),
            billet_server_prepare_only=task_vars.get('billet_server_prepare_only'),
            ansible_check_mode=task_vars.get('ansible_check_mode'),
            billet_node_should_run=result.get('ansible_facts', {}).get('billet_node_should_run')))
    # The stop has returned and its state file has been written. Failing this
    # task stops Ansible before the next task without inventing quiet state.
    if cfg['interrupt'] and name == 'Drain billet compute before changing guest networking' and not result.get('failed'):
        write(root() / 'interrupted.json', dict(node=node(), task=name))
        result.update(failed=True, msg='gate interruption after completed shared network stop')
    return result


def base_variables():
    return dict(billet_binary_src='', billet_enable_server=False, billet_enable_node=True,
                billet_server_prepare_only=False,
                billet_firecracker_enabled=True, billet_ceph_enabled=False, billet_automatic_updates=False,
                billet_service_user='billetgate', billet_service_group='billetgate',
                billet_firecracker_version='v1.16.1', billet_firecracker_stage='/var/lib/billet/firecracker-stage',
                billet_migration_controller='control-b', billet_github_private_key_src='',
                billet_cache_tls_cert_src='', billet_cache_tls_key_src='',
                billet_guest_dns_servers=['1.1.1.1'],
                billet_config={'node': dict(name='node-a', provider='firecracker', server_addr='http://127.0.0.1:7717',
                                            state_dir='/var/lib/billet/node', lock_dir='/run/billet/locks',
                                            firecracker={'bridge': 'billet0'})})


def seed():
    # The namespace overlays /etc; these accounts never reach the host database.
    subprocess.run(['groupadd', '--system', 'billetgate'], check=True)
    # No --key CREATE_MAIL_SPOOL=no: the runner image's shadow build answers
    # "unknown item" and exits 3, which fails the seed before any case runs.
    subprocess.run(['useradd', '--system', '--no-log-init', '--gid', 'billetgate',
                    '--home-dir', '/var/lib/billet', '--no-create-home', 'billetgate'], check=True)
    import grp
    import pwd
    write('/var/lib/billet/service-account', dict(user='billetgate', group='billetgate', uid=pwd.getpwnam('billetgate').pw_uid, gid=grp.getgrnam('billetgate').gr_gid))
    units = {}
    for name in [NODE, 'billet-server.service', 'billet-network.service', 'billet-dnsmasq@billet0.service', 'billet-dnsmasq@billet1.service']:
        units[name] = dict(LoadState='loaded', ActiveState='active' if name == NODE else 'inactive',
                           SubState='running' if name == NODE else 'dead', MainPID='1' if name == NODE else '0',
                           ControlPID='0', Result='success', UnitFileState='enabled' if name == NODE else 'disabled',
                           NeedDaemonReload='no', Type='notify', User='root' if name == NODE else 'billetgate',
                           Group='root' if name == NODE else 'billetgate', Job='', InvocationID='1' * 32,
                           Endpoint='http://127.0.0.1:7717')
        # The first role reload must see the files behind these loaded units.
        # Otherwise the manager marks enablement not-found and keeps it even
        # after the role installs the unit, falsely answering is-enabled with 0.
        # The seed role replaces these fragments before either measured leg.
        fragment = name.split('@', 1)[0] + '@.service' if '@' in name else name
        pathlib.Path('/etc/systemd/system', fragment).write_text(
            '[Service]\nType=notify\nUser=' + units[name]['User'] + '\nGroup=' + units[name]['Group']
            + '\nExecStart=/bin/true\n[Install]\nWantedBy=multi-user.target\n')
    write(root() / 'services/control-a.json', units)
    write(root() / 'role-vars.json', base_variables())
    write(root() / 'role-settings.json', dict(**{'pass': 'seed'}, retained=False, interrupt=False, negative=''))


def prepare(scenario, retained):
    cfg = base_variables()
    if scenario in ['input', 'combined']:
        cfg['billet_config']['node']['max_vcpu'] = 2
    if scenario == 'migration':
        cfg['billet_config']['node']['server_addr'] = 'http://127.0.0.1:7719'
    if scenario in ['network', 'combined', 'resume']:
        cfg['billet_guest_dns_servers'] = ['9.9.9.9']
    if retained:
        # Fixture-owned retirement evidence is deliberately outside the real
        # retirement root. Guard recovery remains real and sees no forged journal.
        #
        # THE NODE-ONLY SEED KEEPS /var/lib/billet/server, and so does every host
        # that never ran a controller: its binary transaction's fence writes
        # inside that directory, which is why only the retained route empties the
        # alias. The retained leg must leave whatever the seed left there exactly
        # as it found it, so its state before the pass is recorded for the
        # comparison rather than asserted away.
        server_dir = pathlib.Path('/var/lib/billet/server')
        try:
            before = server_dir.lstat()
            write(root() / 'server-dir-before.json',
                  dict(present=True, mode=before.st_mode, uid=before.st_uid, gid=before.st_gid))
        except FileNotFoundError:
            write(root() / 'server-dir-before.json', dict(present=False))
        pathlib.Path('/etc/systemd/system/billet-server.service').unlink()
        MODEL.mkdir()
        (MODEL / 'archive').mkdir()
        (MODEL / 'archive/authority').write_text('archived authority sentinel\n')
        journal = fixture('server-retire', 'dry-run-continue-retained-done')['journal']
        journal['transition_id'] = TRANSITION
        write(MODEL / 'journal.json', journal)
        write(MODEL / 'closed.json', dict(phase='done', settled=True, original='/var/lib/billet/original-controller'))
        cfg['billet_config']['server'] = dict(identity_dir='/var/lib/billet/original-controller')
        cfg['billet_server_retire'] = True
        state = read(root() / 'services/control-a.json')
        state['billet-server.service']['LoadState'] = 'not-found'
        write(root() / 'services/control-a.json', state)
    if scenario in ['inactive', 'failed']:
        state = read(root() / 'services/control-a.json')
        state[NODE].update(ActiveState=scenario, SubState='dead' if scenario == 'inactive' else 'failed', MainPID='0')
        write(root() / 'services/control-a.json', state)
    write(root() / 'role-vars.json', cfg)
    write(root() / 'role-settings.json', dict(**{'pass': 'first'}, retained=retained, interrupt=retained and scenario == 'resume',
                                            negative=scenario.removeprefix('extra-') if scenario.startswith('extra-') else ''))
    write(root() / 'initial-node.json', node())


if __name__ == '__main__':
    mode, *arguments = sys.argv[1:]
    if mode == 'answer':
        answer(arguments[0], arguments[1], arguments[2:])
    elif mode == 'seed':
        seed()
    elif mode == 'prepare':
        prepare(arguments[0], arguments[1] == 'retained')
    elif mode == 'next':
        cfg = settings()
        cfg.update(**{'pass': 'second'}, interrupt=False, negative='')
        write(root() / 'role-settings.json', cfg)
    elif mode == 'mutant':
        target, operation = pathlib.Path(arguments[0]), arguments[1]
        require_operation = operation in ['start', 'stop', 'restart']
        if not require_operation:
            raise ValueError('unknown lifecycle plant')
        source = target.read_text()
        anchor = '- name: Enable and start the billet node\n'
        if source.count(anchor) != 1:
            raise ValueError('shared start boundary is not unique')
        plant = ('- name: Planted retained-only extra ' + operation + '\n'
                 '  ansible.builtin.command:\n'
                 '    argv: [systemctl, ' + operation + ', billet-node.service]\n'
                 '  changed_when: true\n'
                 '  when: billet_retirement_node_ordinary | bool\n\n')
        modified = source.replace(anchor, plant + anchor)
        if hashlib.sha256(source.encode()).digest() == hashlib.sha256(modified.encode()).digest():
            raise ValueError('lifecycle plant did not change the private role')
        target.write_text(modified)
    else:
        raise ValueError('unknown host mode: ' + mode)
