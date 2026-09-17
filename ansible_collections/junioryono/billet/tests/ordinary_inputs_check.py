#!/usr/bin/env python3
"""Exercise the production readers with discriminating operands and refusals."""

import copy
import importlib.util
import json
import os
import pathlib
import sys
import tempfile
import types

sys.dont_write_bytecode = True
HERE = pathlib.Path(__file__).resolve().parent
ROOT = HERE.parent


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


inputs = load('ordinary_inputs', ROOT / 'plugins/module_utils/ordinary_inputs.py')


def refuses(call, reason):
    try:
        call()
    except inputs.Refusal as exc:
        assert reason in str(exc), (reason, str(exc))
    else:
        raise AssertionError('reader admitted: ' + reason)


def main():
    known = dict(unit_present=True, environment_file_specs=[])
    assert inputs.environment_specs(known) == []
    assert inputs.environment_specs(dict(unit_present=False, environment_file_specs=None)) is None
    for optional in (True, False):
        spec = dict(path='/etc/billet/installed-node.env', ignore_errors=optional)
        assert inputs.environment_specs(dict(known, environment_file_specs=[spec])) == [spec]
    for specs, reason in [
        (None, 'zero or one'), ({'unknown': 'malformed suffix'}, 'zero or one'),
        ([{'path': '/etc/a'}], 'optionality'),
        ([{'path': '/etc/a', 'ignore_errors': 'yes'}], 'optionality'),
        ([{'path': '/etc/a', 'ignore_errors': 1}], 'optionality'),
        ([{'path': '/etc/a', 'ignore_errors': True}] * 2, 'zero or one'),
    ]:
        refuses(lambda: inputs.environment_specs(dict(known, environment_file_specs=specs)), reason)
    for path in ['relative', '/etc/a b', '/etc/a\n', '/etc/a\t', '/etc/%n', '/etc/a\\x20', '/etc/"a"', '/etc/*', '/etc/a\x7f']:
        refuses(lambda: inputs.environment_specs(dict(known, environment_file_specs=[dict(path=path, ignore_errors=False)])), 'unsupported')
    literal = dict(path='/etc/café/$literal=!.env', ignore_errors=False)
    assert inputs.environment_specs(dict(known, environment_file_specs=[literal])) == [literal]
    keyed = inputs.unit_results([dict(item='billet-node.service', rc=0, stdout='notify'), dict(item='billet-server.service', rc=0, stdout='exec')])
    assert keyed['billet-node.service']['stdout'] == 'notify'
    assert keyed['billet-server.service']['stdout'] == 'exec'
    replacements = inputs.unit_results([dict(item={'key': 'billet-node.service'}, changed=False), dict(item={'key': 'billet-server.service'}, changed=True)])
    assert replacements['billet-node.service']['changed'] is False
    assert replacements['billet-server.service']['changed'] is True
    refuses(lambda: inputs.unit_results([dict(item='node'), dict(item='node')]), 'repeated')
    assert inputs.unit_results([dict(item='node', skipped=True)]) == {}

    operations = dict(filesystem=[dict(kind='write', path='/etc/billet/billet.yaml')], services=[dict(verb='start', unit='billet-node.service')], units=[dict(unit='billet-node.service', path='/etc/systemd/system/billet-node.service', contents='unit\n', sha256=inputs.digest('unit\n'))])
    original = inputs.operation_document('node: {}\n', operations, 'ci-1', 'control-a', '2' * 32)
    assert json.loads(original['stdin']) == original['document']
    assert original['operations_sha256'] == inputs.digest(json.dumps(operations, separators=(',', ':')))
    assert original['rendering_sha256'] == inputs.digest('node: {}\n')
    for member, mutate in [
        ('destination', lambda o: o['filesystem'][0].update(path='/etc/billet/moved.yaml')),
        ('operation', lambda o: o['filesystem'][0].update(kind='delete')),
        ('unit name', lambda o: o['services'][0].update(unit='other.service')),
        ('unit destination', lambda o: o['units'][0].update(path='/etc/systemd/system/other.service')),
        ('unit rendering', lambda o: o['units'][0].update(contents='unit changed\n', sha256=inputs.digest('unit changed\n'))),
    ]:
        changed = copy.deepcopy(operations)
        mutate(changed)
        after = inputs.operation_document('node: {}\n', changed, 'ci-1', 'control-a', '2' * 32)
        assert after['operations_sha256'] != original['operations_sha256'], member
    assert inputs.operation_document('node: {name: changed}\n', operations, 'ci-1', 'control-a', '2' * 32)['rendering_sha256'] != original['rendering_sha256']
    changed = copy.deepcopy(operations)
    changed['units'][0]['contents'] = 'unbound'
    refuses(lambda: inputs.operation_document('node: {}\n', changed, 'ci-1', 'control-a', '2' * 32), 'digest')

    # The environment module owns credentials only while reading or executing;
    # the test writes empty files, so no fixture contains credential contents.
    package = 'ansible_collections.junioryono.billet.plugins.module_utils'
    for name in ['ansible', 'ansible.module_utils', 'ansible.module_utils.basic', 'ansible_collections', 'ansible_collections.junioryono', 'ansible_collections.junioryono.billet', 'ansible_collections.junioryono.billet.plugins', package]:
        sys.modules.setdefault(name, types.ModuleType(name))
    sys.modules['ansible.module_utils.basic'].AnsibleModule = object
    sys.modules[package + '.ordinary_inputs'] = inputs
    env = load('environment_files', ROOT / 'plugins/modules/environment_files.py')
    with tempfile.TemporaryDirectory() as directory:
        path = pathlib.Path(directory) / 'environment'
        path.write_bytes(b'')
        for optional in [True, False]:
            assert env.read_environment(dict(path=str(path), ignore_errors=optional)) == b''
        path.unlink()
        assert env.read_environment(dict(path=str(path), ignore_errors=True)) is None
        refuses(lambda: env.read_environment(dict(path=str(path), ignore_errors=False)), 'readable')
        os.mkfifo(path)
        refuses(lambda: env.read_environment(dict(path=str(path), ignore_errors=True)), 'regular')
        path.unlink()
        path.symlink_to(path.parent / 'absent')
        refuses(lambda: env.read_environment(dict(path=str(path), ignore_errors=True)), 'readable')
    account = load('retired_account', ROOT / 'plugins/modules/retired_account.py')
    account.pwd.getpwnam = lambda name: types.SimpleNamespace(pw_name='billet', pw_uid=2345, pw_gid=3456, pw_dir='/var/lib/billet')
    account.grp.getgrnam = lambda name: types.SimpleNamespace(gr_name='billet', gr_gid=3456)
    with tempfile.TemporaryDirectory() as directory:
        path = pathlib.Path(directory) / 'account'
        record = dict(user='billet', group='billet', uid=2345, gid=3456)
        path.write_text(json.dumps(record))
        path.chmod(0o644)
        account.verify('billet', 'billet', '/var/lib/billet', str(path))
        for change in [dict(uid=2346), dict(gid=3457), dict(user='other'), dict(group='other'), dict(uid=0), dict(gid=True)]:
            path.write_text(json.dumps(dict(record, **change)))
            try:
                account.verify('billet', 'billet', '/var/lib/billet', str(path))
            except ValueError:
                pass
            else:
                raise AssertionError('retired account admitted contradictory evidence: ' + str(change))
        path.write_text(json.dumps(record))
        path.chmod(0o666)
        try:
            account.verify('billet', 'billet', '/var/lib/billet', str(path))
        except ValueError:
            pass
        else:
            raise AssertionError('retired account admitted an unprotected record')
    print('ordinary inputs: typed environments, keyed results, verified controllers and binding changes passed')


if __name__ == '__main__':
    main()
