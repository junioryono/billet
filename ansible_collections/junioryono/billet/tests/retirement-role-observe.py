#!/usr/bin/env python3
"""Independent filesystem/request assertions for the whole-role adapters."""
import ctypes
import hashlib
import json
import os
import pathlib
import select
import shlex
import stat
import struct
import subprocess
import sys
import time
import uuid

import yaml

NODE = 'billet-node.service'
MUTATIONS = {'enable', 'disable', 'start', 'stop', 'restart', 'reload', 'mask', 'unmask', 'try-restart', 'reload-or-restart'}
CONTROLLERS = ('billet-server.', 'billet-backup.', 'billet-upgrade.', 'var-lib-billet-server.mount', 'var-lib-billet-original\\x2dcontroller.mount')
# internal/lifeops/retiredcondition.go: Inspector.OperationUnitDirectories.
UNIT_DIRS = tuple(pathlib.Path(path) for path in (
    '/etc/systemd/system', '/run/systemd/system', '/etc/systemd/system.control',
    '/run/systemd/system.control', '/run/systemd/transient', '/run/systemd/generator.early',
    '/run/systemd/generator', '/run/systemd/generator.late', '/usr/local/lib/systemd/system',
    '/usr/lib/systemd/system', '/lib/systemd/system'))
UPGRADES = pathlib.Path('/var/lib/billet/upgrades')
PROTECTED = [pathlib.Path(path) for path in [
    '/var/lib/billet/server', '/var/lib/billet/original-controller', '/var/lib/billet/gate-retirement',
    '/var/lib/billet/upgrades', '/var/lib/billet/service-account', '/etc/billet/app-private-key.pem',
    '/etc/billet/server.env', '/etc/billet/server.env-managed', '/etc/billet/rds-ca-bundle.pem']]


def read(path):
    return json.loads(pathlib.Path(path).read_text())


def rows(path):
    return [json.loads(line) for line in pathlib.Path(path).read_text().splitlines()]


def require(ok, why):
    if not ok:
        raise ValueError(why)


def beneath(path, parent):
    return path == parent or parent in path.parents


def protected(path):
    path = pathlib.Path(os.path.normpath(path))
    return (any(beneath(path, parent) for parent in PROTECTED)
            or any(beneath(path, base) and any(part.startswith(CONTROLLERS) for part in path.relative_to(base).parts)
                   for base in UNIT_DIRS)
            or (beneath(path, pathlib.Path('/etc/billet')) and path.name.startswith('app-private-key-')))


def relevant_directory(path):
    # Every unit subdirectory can contain a controller link, including a new
    # target.wants/target.requires tree. Ancestors cover currently absent roots.
    bases = (*UNIT_DIRS, *PROTECTED, pathlib.Path('/etc/billet'))
    return any(beneath(base, path) or beneath(path, base) for base in bases)


def walk_directories(path):
    yield path
    with os.scandir(path) as entries:
        for entry in entries:
            child = pathlib.Path(entry.path)
            if not relevant_directory(child):
                continue
            if entry.is_symlink():
                require(not entry.is_dir(), 'observer cannot cover a directory symlink: ' + str(child))
            elif entry.is_dir(follow_symlinks=False):
                yield from walk_directories(child)


def snapshot():
    paths = set(PROTECTED)
    for base in (*PROTECTED, *UNIT_DIRS, pathlib.Path('/etc/billet')):
        if base.is_dir():
            for directory in walk_directories(base):
                with os.scandir(directory) as entries:
                    paths.update(pathlib.Path(entry.path) for entry in entries if protected(entry.path))
    result = {}
    for path in sorted(paths):
        try:
            info = path.lstat()
        except FileNotFoundError:
            result[str(path)] = None
            continue
        entry = dict(mode=info.st_mode, uid=info.st_uid, gid=info.st_gid, inode=info.st_ino, mtime=info.st_mtime_ns)
        if stat.S_ISREG(info.st_mode):
            entry['sha256'] = hashlib.sha256(path.read_bytes()).hexdigest()
        elif stat.S_ISLNK(info.st_mode):
            entry['target'] = os.readlink(path)
        result[str(path)] = entry
    return result


def outside_upgrades(state):
    return {path: value for path, value in state.items() if not beneath(pathlib.Path(path), UPGRADES)}


def boundary(root, phase):
    # The single-host linear strategy and driver wait for the drain ACK before
    # starting a guard command or returning to ordinary tasks. No time guesses.
    request = dict(id=uuid.uuid4().hex, phase=phase)
    temporary = root / ('watch-request-' + request['id'])
    temporary.write_text(json.dumps(request))
    temporary.replace(root / 'watch-request.json')
    deadline = time.monotonic() + 15
    while time.monotonic() < deadline:
        if (root / 'watch-error').exists():
            raise ValueError((root / 'watch-error').read_text())
        try:
            if read(root / 'watch-ack.json') == request:
                return
        except FileNotFoundError:
            pass
        time.sleep(0.01)
    raise ValueError('filesystem observer did not acknowledge ' + phase)


def command_argv(args):
    argv = args.get('argv') or shlex.split(args.get('_raw_params', ''))
    if argv[:3] == ['timeout', '-k', '10'] and len(argv) > 4 and str(argv[3]).isdigit():
        argv = argv[4:]
    return argv


def builtin_module(module):
    if module.startswith('ansible.legacy.'):
        return module.replace('ansible.legacy.', 'ansible.builtin.', 1)
    return 'ansible.builtin.' + module if '.' not in module else module


def guard_phase(module, args, source):
    module = builtin_module(module)
    argv = command_argv(args) if module == 'ansible.builtin.command' else []
    if (pathlib.Path(source.rsplit(':', 1)[0]).name == 'prepare-exclusion.yml'
            and argv[:2] == ['/usr/bin/billet', 'converge-guard']
            and len(argv) > 2 and argv[2] in ['prepare', 'settle', 'release']):
        return 'guard-' + argv[2]
    return None


def require_observable_command(module, args, phase):
    # Arbitrary programs (including command: sh -c) can use mmap, alternate
    # mount paths, or deferred children outside inotify's request model.
    # Admit only the namespace's recorded adapters and read-only utilities.
    module = builtin_module(module)
    if module not in ['ansible.builtin.shell', 'ansible.builtin.command', 'ansible.builtin.raw', 'ansible.builtin.script']:
        return
    require(module == 'ansible.builtin.command' and not args.get('_uses_shell'), 'opaque shell/script cannot prove protected-path inactivity')
    argv = command_argv(args)
    require(bool(argv), 'empty command cannot be classified')
    if phase:
        return
    if argv[0] in ['systemctl', '/usr/bin/systemctl']:
        return  # Exact requests and controller operands are judged separately.
    if argv[0] == '/usr/bin/billet':
        require(argv[1:] in [['version'], ['--version']]
                or argv[1:3] in [['release', 'inspect'], ['server', 'retire'], ['node', 'migrate-endpoint'],
                                 ['node', 'receipt'], ['rollout', 'registration']],
                'unmodeled billet command cannot prove protected-path inactivity')
        return  # Unknown subcommands are refused by the recording answerer.
    require(argv in [['ip', '-json', 'link', 'show'], ['networkctl', 'reload'],
                     ['/usr/sbin/nft', '-f', '/etc/billet/network.nft']]
            or argv[:4] == ['ip', 'link', 'show', 'dev']
            or argv == ['pgrep', '-u', 'billetgate']
            or (len(argv) == 3 and argv[:2] == ['realpath', '--canonicalize-missing'] and argv[2].startswith('/')),
            'unclassified command cannot prove protected-path inactivity: ' + str(argv[0]))


def watch(root):
    libc = ctypes.CDLL(None, use_errno=True)
    fd = libc.inotify_init1(os.O_NONBLOCK | os.O_CLOEXEC)
    if fd < 0:
        raise OSError(ctypes.get_errno(), 'inotify_init1')
    masks = 0x2 | 0x4 | 0x8 | 0x40 | 0x80 | 0x100 | 0x200 | 0x400 | 0x800
    watches = {}
    identities = {}
    phase = 'ordinary'
    transitions = []
    guard_gaps = []
    retained = read(root / 'role-settings.json')['retained']
    # Resolve the standard merged-/usr aliases once; watch their parents too.
    aliases = {path: path.resolve() for path in (*UNIT_DIRS, pathlib.Path('/etc/billet'), pathlib.Path('/var/lib/billet'))}
    require(all(path == target or (path in UNIT_DIRS and target in UNIT_DIRS)
                for path, target in aliases.items()), 'observer root redirects outside the protected namespace')
    bases = set(aliases.values())

    def authorized(path):
        return phase != 'ordinary' and beneath(path, UPGRADES)

    def inventory():
        wanted = set()
        for base in bases:
            for parent in reversed((base, *base.parents)):
                if parent.is_dir():
                    wanted.add(parent)
            if base.is_dir():
                wanted.update(walk_directories(base))
        return wanted

    def add(path):
        info = path.stat()
        wd = libc.inotify_add_watch(fd, os.fsencode(path), masks | 0x01000000)  # IN_ONLYDIR
        if wd < 0:
            raise OSError(ctypes.get_errno(), 'inotify_add_watch ' + str(path))
        require(wd not in watches or watches[wd] == path, 'observer watch descriptor was reused')
        watches[wd] = path
        identities[path] = (info.st_dev, info.st_ino)

    def audit():
        require(all(path.resolve() == target for path, target in aliases.items()), 'observer root alias changed')
        for path in inventory():
            info = path.stat()
            require(identities.get(path) == (info.st_dev, info.st_ino),
                    'directory existed unwatched or was replaced: ' + str(path))

    with (root / 'filesystem.jsonl').open('w') as output:
        def drain():
            while select.select([fd], [], [], 0)[0]:
                data = os.read(fd, 1024 * 1024)
                offset = 0
                while offset < len(data):
                    wd, mask, cookie, size = struct.unpack_from('iIII', data, offset)
                    name = os.fsdecode(data[offset + 16:offset + 16 + size].split(b'\0', 1)[0])
                    offset += 16 + size
                    require(not mask & 0x4000, 'filesystem observer overflowed')
                    require(wd in watches, 'filesystem observer lost a watch')
                    path = watches[wd] / name if name else watches[wd]
                    event = dict(path=str(path), mask=mask, cookie=cookie, phase=phase)
                    output.write(json.dumps(event) + '\n')
                    output.flush()
                    if name and mask & (0x40 | 0x80 | 0x100 | 0x200):
                        require(not any(beneath(base, path) for base in (*aliases, *bases)),
                                'observer root or ancestor changed: ' + str(path))
                    if mask & (0x2000 | 0x8000 | 0x400 | 0x800):  # unmount/ignored/delete-self/move-self
                        require(not mask & 0x2000, 'filesystem observer lost a mount')
                        require(authorized(path), 'filesystem observer lost continuous coverage: ' + str(path))
                        guard_gaps.append(event)
                        if mask & 0x8000:
                            identities.pop(watches.pop(wd), None)
                    if mask & 0x40000000 and mask & (0x100 | 0x80) and relevant_directory(path):
                        # Subscribe the WHOLE incoming tree, but never call the
                        # pre-subscription interval observed, even if now empty.
                        if path.is_dir():
                            for child in walk_directories(path):
                                add(child)
                        require(authorized(path), 'late directory subscription leaves an unobserved interval: ' + str(path))
                        guard_gaps.append(event)
                    if mask & 0x40000000 and mask & (0x40 | 0x200) and relevant_directory(path):
                        require(authorized(path), 'directory left observation: ' + str(path))

        for path in sorted(inventory(), key=lambda path: len(path.parts)):
            add(path)
        drain()
        audit()
        baseline = snapshot()
        drain()
        (root / 'watch-ready').touch()
        handled = None
        while True:
            select.select([fd], [], [], 0.01)
            drain()
            try:
                request = read(root / 'watch-request.json')
            except FileNotFoundError:
                request = None
            if request and request['id'] != handled:
                target = request['phase']
                require(target in ['ordinary', 'guard-prepare', 'guard-settle', 'guard-release', 'guard-recover'], 'unknown observation phase')
                require((phase == 'ordinary') != (target == 'ordinary'), 'overlapping guard observation windows')
                audit()
                current = snapshot()
                drain()
                if retained and phase == 'ordinary':
                    require(current == baseline, 'protected state changed during ordinary work')
                elif retained:
                    require(outside_upgrades(current) == outside_upgrades(baseline), 'guard command changed non-guard protected state')
                transitions.append(dict(phase=phase, next=target, before=baseline, after=current))
                baseline = current
                phase = target
                handled = request['id']
                ack = root / 'watch-ack.tmp'
                ack.write_text(json.dumps(request))
                ack.replace(root / 'watch-ack.json')
            if (root / 'watch-stop').exists():
                require(phase == 'ordinary', 'observer stopped during an authorized guard operation')
                audit()
                current = snapshot()
                drain()
                if retained:
                    require(current == baseline, 'protected state changed after the last guard boundary')
                (root / 'watch-complete.json').write_text(json.dumps(dict(
                    ordinary_coverage='continuous', guard_windows=transitions,
                    authorized_guard_subscription_gaps=guard_gaps, final=current)))
                break
    os.close(fd)


def requests(root, pass_name):
    return [row['argv'] for row in rows(root / 'manager.jsonl')
            if row['pass_name'] == pass_name and any(arg in MUTATIONS for arg in row['argv'])
            and any(arg == NODE or arg == 'billet-network.service' or arg.startswith('billet-dnsmasq@') for arg in row['argv'])]


def require_control(trace, scenario, pass_name):
    # These controls are written independently of the retained trace and the
    # task conditions. A shared regression cannot pass by changing both legs.
    expected = {
        'active': [], 'inactive': [['start', NODE]], 'failed': [['start', NODE]],
        'input': [['restart', NODE]], 'migration': [['stop', NODE], ['start', NODE]],
        'stable': [],
    }
    network = [['stop', NODE], ['restart', 'billet-dnsmasq@billet0.service'],
               ['restart', 'billet-dnsmasq@billet1.service'], ['start', NODE]]
    expected.update(network=network, combined=network,
                    resume=network if pass_name == 'first' else [])
    require(trace == expected[scenario], f'independent node-only {scenario}/{pass_name}: {trace!r} != {expected[scenario]!r}')


def require_node_ready(root, pass_name):
    unit = read(root / 'services/control-a.json')[NODE]
    failures = []
    for field, expected in [('ActiveState', 'active'), ('UnitFileState', 'enabled'), ('NeedDaemonReload', 'no')]:
        if unit.get(field) != expected:
            failures.append(f'{field}={unit.get(field)!r}, expected {expected!r}')
    if 'MainPID' not in unit or unit['MainPID'] == '0':
        failures.append(f'MainPID={unit.get("MainPID")!r}, expected a recorded nonzero PID')
    require(not failures, f'{root.name}/{pass_name}: {NODE}: ' + '; '.join(failures))


def failure_diagnostic(root, mode, error):
    # Report only the manager's public argv and the activation-policy scalars;
    # the command capture and task arguments can contain guard tokens.
    lines = [f'whole-role observer failure: case={root.name} phase={mode}']
    for filename, label in [('node-policy.jsonl', 'recorded node policy'),
                            ('manager.jsonl', 'recorded systemctl request trace')]:
        lines.append(label + ':')
        try:
            lines.append((root / filename).read_text().rstrip())
        except OSError as diagnostic_error:
            lines.append(f'unavailable: {diagnostic_error}')
    try:
        unit = read(root / 'services/control-a.json')[NODE]
        lines.append(f'{NODE} recorded state: {json.dumps(unit, sort_keys=True)}')
    except (ValueError, KeyError, OSError) as diagnostic_error:
        lines.append(f'{NODE} state unavailable: {diagnostic_error}')
    lines.append(f'whole-role observer failure: case={root.name} phase={mode}: {error}')
    diagnostic = '\n'.join(lines) + '\n'
    print(diagnostic, file=sys.stderr, end='')
    (root / 'role-failure.txt').write_text(diagnostic)


def interrupted(root):
    observed = read(root / 'interrupted.json')
    require(observed['node']['ActiveState'] == 'inactive' and observed['node']['MainPID'] == '0', 'interruption did not preserve the completed stop')
    require(requests(root, 'first') == [['stop', NODE]], 'interruption crossed the shared stop/start boundary')
    answers = [row['command'] for row in rows(root / 'answers.jsonl') if row['pass_name'] == 'first']
    require('receipt' not in answers and 'retire-settled-closing' not in answers, 'interrupted pass receipted or closed')
    output = (root / 'out-first').read_text()
    require('gate interruption after completed shared network stop' in output, 'interrupted for an unrelated reason')


def require_retained_order(all_tasks, pass_name, interrupted_pass):
    # Meta and completed tasks share this journal, so the final flush can be
    # ordered against the actual command, including on the later resume pass.
    tasks = [row for row in all_tasks if row['pass_name'] == pass_name and row['event'] in ['after', 'meta']]

    def position(name):
        matches = [index for index, row in enumerate(tasks) if row['task'] == name]
        require(len(matches) == 1, 'missing or repeated phase task: ' + name)
        return matches[0]

    def command_positions(flag):
        return [index for index, row in enumerate(tasks)
                if row['event'] == 'after' and row['module'] == 'ansible.builtin.command'
                and command_argv(row['args'])[:3] == ['/usr/bin/billet', 'server', 'retire']
                and flag in command_argv(row['args'])]

    entry = command_positions('--check-settled-entry')
    admission = command_positions('--check-node-config')
    closing = command_positions('--check-settled-closing')
    require(len(entry) == len(admission) == 1, 'entry or node-config command missing or repeated')
    entry_bound = position('Record the bound settled entry observation')
    admission_bound = position('Record current node configuration admission')
    require(entry[0] < entry_bound < admission[0] < admission_bound, 'entry/node-config binding order differs')
    require(not any(row.get('changed') for row in tasks[entry[0]:admission_bound + 1]),
            'ordinary mutation preceded bound node-config admission')
    if interrupted_pass:
        require(not closing, 'interrupted pass reached closing')
        require(admission_bound < position('Drain billet compute before changing guest networking'),
                'interrupted stop preceded node-config admission')
        return
    require(len(closing) == 1, 'closing command missing or repeated')
    start = position('Enable and start the billet node')
    flush = position("Apply the reload the role's own unit-file changes left pending")
    reset = position('Forget the preceding settled closing result')
    bound = position('Record the bound settled closing observation')
    require(admission_bound < start < flush < reset < closing[0] < bound,
            'ordinary work/final handler flush/closing order differs')
    mutations = [row['task'] for row in tasks[closing[0]:] if row.get('changed')]
    require(not mutations, 'a mutation followed closing: ' + repr(mutations))


def finish(root, scenario, variant):
    retained = variant == 'retained'
    passes = ['first', 'second'] if scenario in ['resume', 'stable'] else ['first']
    all_tasks = rows(root / 'tasks.jsonl')
    all_answers = rows(root / 'answers.jsonl')
    calls = rows(root / 'calls/index.jsonl')
    for call in calls:
        if call['command'] != 'retire-node-config':
            continue
        require(call['has_stdin'], 'node-config lost stdin')
        document = json.loads((root / 'calls' / call['stdin']).read_text())
        require(document['schema'] == 1 and document['retiring'] == 'control-a' and document['transition_id'] == '2' * 32, 'node-config document binding differs')
        require(document['run'] == call['argv'][call['argv'].index('--run') + 1], 'node-config holder differs')
        require(document['rendering_sha256'] == hashlib.sha256(document['rendering'].encode()).hexdigest(), 'rendering digest differs')
        require(document['operations']['services'] == [dict(verb=verb, unit=NODE) for verb in ['enable', 'stop', 'start']], 'node service admission superset differs')
    for pass_name in passes:
        answers = [row for row in all_answers if row['pass_name'] == pass_name]
        commands = [row['command'] for row in answers]
        retire = [command for command in commands if command.startswith('retire-')]
        expected = ['retire-classify']
        if retained:
            expected += ['retire-settled-entry', 'retire-node-config']
            if not (scenario == 'resume' and pass_name == 'first'):
                expected += ['retire-settled-closing']
        require(retire == expected, 'retained route/admission/closing call order differs: ' + repr(retire))
        if retained:
            require('local' not in commands, 'retained route executed controller bootstrap/ownership preparation')
            entry = next(row for row in answers if row['command'] == 'retire-settled-entry')
            want_activity = ('quiet-inactive' if scenario == 'resume' and pass_name == 'second'
                             else 'quiet-' + scenario if scenario in ['inactive', 'failed'] else 'active')
            require(entry['answer']['node_activity'] == want_activity, 'entry did not admit the observed activity')
            admission = next(row for row in answers if row['command'] == 'retire-node-config')
            require(admission['answer']['node_activity'] == want_activity and admission['answer']['outcome'] == 'admitted', 'quiet node-config admission missing')
            classified = answers[0]['answer']['journal']
            require(classified['phase'] == 'done' and classified['settled'] is True, 'settled classification missing')
        if retained:
            executed = [row['task'] for row in all_tasks if row['pass_name'] == pass_name and row['event'] == 'after' and not row['failed']]
            require('Record the bound settled entry observation' in executed and 'Record current node configuration admission' in executed, 'entry or node-config answer was not bound by the real caller')
            require_retained_order(all_tasks, pass_name, scenario == 'resume' and pass_name == 'first')
        if retained and scenario == 'resume' and pass_name == 'first':
            continue
        tasks = [row for row in all_tasks if row['pass_name'] == pass_name and row['event'] == 'after']
        names = [row['task'] for row in tasks]
        require('Enable and start the billet node' in names, 'shared start was never reached')
        require('Refuse the receipt\'s refresh' in names, 'ordinary receipt parser was never reached')
        receipt = [row for row in answers if row['command'] == 'receipt' and '--refresh' in row['argv']]
        require(len(receipt) == 1 and receipt[0]['answer']['outcome'] in ['written', 'current'], 'ordinary refresh missing')
        if scenario == 'resume' and (retained or pass_name == 'first'):
            require(receipt[0]['answer']['outcome'] == 'written', 'resume did not refresh its modeled receipt')
        # Meta is executed by the strategy itself, outside action adapters.
        # Observe its scheduling directly as well as the handler actions.
        output = (root / ('out-' + pass_name)).read_text()
        flushes = [row for row in rows(root / 'meta.jsonl') if row['pass_name'] == pass_name
                   and row['task'] == "Apply the reload the role's own unit-file changes left pending"]
        require(len(flushes) == 1 and flushes[0]['action'] == 'flush_handlers'
                and flushes[0]['run_state'] == 'HANDLERS' and not flushes[0]['pending'], 'final flush did not schedule all pending handlers')
        if retained:
            require(commands[-1] == 'retire-settled-closing', 'a command followed closing')
            require(answers[-1]['answer']['outcome'] == 'verified' and 'Record the bound settled closing observation' in names, 'closing was not verified by the real caller')
            require('Prove settled closing after all ordinary work and handlers' in output, 'closing include missing')
            if scenario.startswith('extra-'):
                plant = 'Planted retained-only extra ' + scenario.removeprefix('extra-')
                require(names.count(plant) == 1 and names.index(plant) + 1 == names.index('Enable and start the billet node'),
                        'lifecycle plant did not execute immediately before the shared start')
        if not retained and not scenario.startswith('extra-'):
            require_control(requests(root, pass_name), scenario, pass_name)
    units = read(root / 'services/control-a.json')
    require_node_ready(root, passes[-1])
    events = rows(root / 'filesystem.jsonl')
    coverage = read(root / 'watch-complete.json')
    require(coverage['ordinary_coverage'] == 'continuous', 'absence of events has no continuous observation proof')
    canary = [event for event in events if event['path'] == '/etc/billet/gate-observer-canary']
    require(any(event['mask'] & 0x100 for event in canary) and any(event['mask'] & 0x200 for event in canary), 'transient filesystem observer did not see its create/delete calibration')
    if retained:
        require(calls[-1]['command'] == 'retire-settled-closing', 'a recorded command followed final closing')
        before, after = read(root / 'protected-before.json'), read(root / 'protected-after.json')
        require(coverage['final'] == after, 'protected state changed after observer completion')
        require(outside_upgrades(before) == outside_upgrades(after), 'protected bytes/identity/metadata changed')
        require(not [event for event in events if protected(event['path'])
                     and not (event['phase'].startswith('guard-') and beneath(pathlib.Path(event['path']), UPGRADES))],
                'transient controller/archive/ordinary-upgrade write observed')
        for row in rows(root / 'manager.jsonl'):
            if row['pass_name'] == 'seed' or not any(arg in MUTATIONS for arg in row['argv']):
                continue
            require(not any(arg.startswith(CONTROLLERS) for arg in row['argv']), 'controller service operation observed')
        for row in all_tasks:
            if row['pass_name'] == 'seed' or row['event'] != 'before':
                continue
            require_observable_command(row['module'], row['args'], row['guard_phase'])
            if row['module'] in ['ansible.builtin.file', 'ansible.builtin.copy', 'ansible.builtin.template']:
                destination = row['args'].get('dest', row['args'].get('path', row['args'].get('name', '')))
                require(not destination or not protected(destination), 'controller write/ownership repair task executed: ' + row['task'])
                if destination and row['args'].get('recurse'):
                    require(not relevant_directory(pathlib.Path(os.path.normpath(destination))), 'recursive ownership repair covers a protected descendant')
    # Installed bytes are read independently of the answerer and producer.
    installed = yaml.safe_load(pathlib.Path('/etc/billet/billet.yaml').read_text())
    desired = read(root / 'role-vars.json')['billet_config']
    expected = {key: value for key, value in desired.items() if key not in ['server', 'github', 'targets', 'backup']}
    require(installed == expected, 'installed YAML is not E')
    registration = read('/var/lib/billet/node/gate-registration.json')
    receipt = read('/var/lib/billet/node/gate-receipt.json')
    require(registration['invocation'] == units[NODE]['InvocationID'] == receipt['invocation_id'], 'modeled registration/receipt is stale')
    require(receipt['installed_sha256'] == hashlib.sha256(pathlib.Path('/etc/billet/billet.yaml').read_bytes()).hexdigest(), 'receipt digest is stale')
    if scenario in ['resume', 'stable']:
        old, new = read(root / 'guard-before.json'), read(root / 'guard-after.json')
        require(old['holder'] != new['holder'] and old['id'] != new['id'], 'later holder was not independently acquired')
        require(not new.get('preparing') and not new.get('transition'), 'next guard is not settled and pointer-free')
        calls = rows(root / 'calls/index.jsonl')
        recovery = [call for call in calls if call['command'] == 'recover']
        require(len(recovery) == 1 and recovery[0]['argv'] == ['converge-guard', 'recover', '--holder', old['holder'], '--old-driver-stopped'], 'authorized guard recovery not observed')
        private = (root / 'private').read_text()
        require('backing=1 cmd=recover status=0' in private, 'recovery did not execute the real command')
    report = dict(scenario=scenario, variant=variant, installed=installed,
                  traces={pass_name: requests(root, pass_name) for pass_name in passes},
                  initial=read(root / 'initial-node.json'), initial_files=read(root / 'initial-files.json'), final=units[NODE], receipt=receipt)
    (root / 'role-observation.json').write_text(json.dumps(report, indent=2) + '\n')


class ParityMismatch(ValueError):
    pass


def compare_traces(control, retained):
    if control != retained:
        raise ParityMismatch('lifecycle request parity differs: ' + repr((control, retained)))


def compare(control_path, retained_path, negative=None):
    control, retained = read(control_path / 'role-observation.json'), read(retained_path / 'role-observation.json')
    require(control['installed'] == retained['installed'], 'paired E differs')
    # PIDs are incidental; activity, enablement, source fixture and invocation
    # history are not normalized away. Seed passes create the same node state.
    before_control = {k: v for k, v in control['initial'].items() if k != 'MainPID'}
    before_retained = {k: v for k, v in retained['initial'].items() if k != 'MainPID'}
    require(before_control == before_retained, 'paired initial node evidence differs')
    require(control['initial_files'] == retained['initial_files'], 'paired initial installed node inputs differ')
    if negative:
        got, expected = retained['traces']['first'], control['traces']['first']
        require(control['scenario'] == 'active' and retained['scenario'] == 'extra-' + negative,
                'planted negative was not paired with its independent active control')
        require_control(expected, 'active', 'first')
        planted = [[negative, NODE]] + ([['start', NODE]] if negative == 'stop' else [])
        require(got == planted, 'planted request/shared-start trace differs: ' + repr(got))
        try:
            compare_traces({'first': expected}, {'first': got})
        except ParityMismatch:
            pass
        else:
            raise ValueError('parity comparison accepted extra ' + negative)
        print('ok: parity rejects retained-only extra ' + negative + ' despite active final state and closing')
    else:
        if control['scenario'] == 'resume':
            # The control completes uninterrupted. Only the retained leg splits
            # this same ordered request sequence across the stop boundary.
            compare_traces(control['traces']['first'] + control['traces']['second'],
                           retained['traces']['first'] + retained['traces']['second'])
        else:
            compare_traces(control['traces'], retained['traces'])
        require(control['final']['InvocationID'] == retained['final']['InvocationID'], 'modeled invocation histories differ')
        print('ok: exact lifecycle request parity ' + control['scenario'])


if __name__ == '__main__':
    mode, *args = sys.argv[1:]
    try:
        if mode in ['compare', 'negative']:
            compare(pathlib.Path(args[0]), pathlib.Path(args[1]), args[2] if mode == 'negative' else None)
        else:
            root = pathlib.Path(os.environ['BILLET_GATE_ROLE'])
            if mode == 'snapshot':
                print(json.dumps(snapshot(), sort_keys=True))
            elif mode == 'initial':
                paths = [pathlib.Path('/etc/billet/billet.yaml'), pathlib.Path('/etc/systemd/system/billet-node.service'),
                         pathlib.Path('/etc/billet/network.nft')]
                paths += sorted(pathlib.Path('/etc/billet/network').glob('*.conf'))
                print(json.dumps({str(path): hashlib.sha256(path.read_bytes()).hexdigest() for path in paths}))
            elif mode == 'seed':
                require_node_ready(root, 'seed')
            elif mode == 'watch':
                try:
                    watch(root)
                except Exception as error:
                    (root / 'watch-error').write_text(str(error))
                    raise
            elif mode == 'recover':
                boundary(root, 'guard-recover')
                try:
                    subprocess.run(['/usr/bin/billet', 'converge-guard', 'recover', '--holder', args[0], '--old-driver-stopped'], check=True)
                finally:
                    boundary(root, 'ordinary')
            elif mode == 'interrupted':
                interrupted(root)
            elif mode == 'finish':
                finish(root, *args)
            else:
                raise ValueError('unknown observer mode: ' + mode)
    except (ValueError, KeyError, OSError) as error:
        if mode in ['seed', 'interrupted', 'finish']:
            failure_diagnostic(root, mode, error)
            sys.exit(1)
        sys.exit(str(error))
