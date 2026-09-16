# Copyright (c) 2026 junioryono
# Apache-2.0

"""Typed operands shared by the ordinary readers and their detached preflight."""

import hashlib
import json
import re
import unicodedata


class Refusal(ValueError):
    pass


def environment_specs(service):
    """None is an absent unit; [] is a loaded unit with no EnvironmentFile."""
    if not isinstance(service, dict) or type(service.get('unit_present')) is not bool:
        raise Refusal('node environment observation is unknown')
    specs = service.get('environment_file_specs')
    if service['unit_present'] is False:
        if specs is not None:
            raise Refusal('absent unit carries environment specifications')
        return None
    if not isinstance(specs, list) or len(specs) > 1:
        raise Refusal('environment specifications require zero or one typed entry')
    for spec in specs:
        if not isinstance(spec, dict) or set(spec) != {'path', 'ignore_errors'} or type(spec['ignore_errors']) is not bool:
            raise Refusal('environment optionality is unknown')
        path = spec['path']
        # This is the literal unquoted template grammar, not systemd's full
        # language. Refuse escapes, specifiers, quotes and glob expansion.
        if not isinstance(path, str) or not path.startswith('/') or any(
                c.isspace() or unicodedata.category(c) == 'Cc' or c in "%\\\"'*?[]" for c in path):
            raise Refusal('environment path is unsupported')
    return specs


def unit_results(results):
    out = {}
    for result in results:
        if result.get('skipped', False):
            continue
        unit = result.get('item')
        if isinstance(unit, dict):
            unit = unit.get('key')
        if not isinstance(unit, str) or unit in out:
            raise Refusal('unit observation is unkeyed or repeated')
        out[unit] = result
    return out


def digest(text):
    return hashlib.sha256(text.encode('utf-8')).hexdigest()


def operation_document(rendering, operations, run, retiring, transition):
    """Serialize operations once; the command hashes their exact JSON bytes."""
    if set(operations) != {'filesystem', 'services', 'units'} or any(not isinstance(v, list) for v in operations.values()):
        raise Refusal('operation document requires three explicit arrays')
    for op in operations['filesystem']:
        if set(op) != {'kind', 'path'} or op['kind'] not in ['read', 'write', 'mkdir', 'chown', 'delete', 'recursive-chown', 'recursive-delete', 'recursive-walk']:
            raise Refusal('unsupported filesystem operand')
    for op in operations['services']:
        if set(op) != {'verb', 'unit'}:
            raise Refusal('unsupported service operand')
    for unit in operations['units']:
        if set(unit) != {'unit', 'path', 'contents', 'sha256'} or digest(unit['contents']) != unit['sha256']:
            raise Refusal('proposed unit digest differs from its bytes')
    body = dict(schema=1, run=run, retiring=retiring, transition_id=transition,
                rendering=rendering, rendering_sha256=digest(rendering), operations=operations)
    text = json.dumps(body, ensure_ascii=True, separators=(',', ':'))
    raw_operations = json.dumps(operations, ensure_ascii=True, separators=(',', ':'))
    return dict(document=body, stdin=text, rendering_sha256=digest(rendering), operations_sha256=digest(raw_operations))


def controller_candidate(inspect, status, host, executable, config, deployment, holder):
    """Select only installed, running and process-bound controller evidence."""
    try:
        cfg, service, tx = inspect['installed_config'], inspect['services']['server'], inspect['transaction']
        if type(inspect['schema']) is not int or inspect['schema'] != 1 or inspect['config_binding'] is not True or cfg['has_server'] is not True:
            raise Refusal('installed controller configuration is not bound')
        if cfg['path'] != config or cfg['ledger_backend'] not in ['sqlite', 'postgres']:
            raise Refusal('controller configuration operand is unsupported')
        observed_config = inspect['config']
        if observed_config['presence'] != 'present' or observed_config['readable'] is not True or observed_config['same_as_installed'] is not True or observed_config['path'] != config:
            raise Refusal('controller configuration is not positively installed and readable')
        if cfg['controllers'] not in ['single', 'active-passive'] or not isinstance(cfg['sha256'], str) or not re.fullmatch('[0-9a-f]{64}', cfg['sha256']):
            raise Refusal('controller configuration shape or digest is unknown')
        if inspect['host']['retirement'] is not None or inspect['host']['deployment_id'] != deployment:
            raise Refusal('controller is retiring, retired or from another deployment')
        if not isinstance(deployment, str) or not re.fullmatch('[0-9a-f]{32}', deployment):
            raise Refusal('local deployment is unproved')
        image = inspect['executable']
        if image['image'] != executable or image['installed_path'] != executable or image['installed_path_same'] is not True or image['process_bound'] is not True:
            raise Refusal('controller executable is not bound')
        if not isinstance(image['sha256'], str) or not re.fullmatch('[0-9a-f]{64}', image['sha256']):
            raise Refusal('controller executable digest is unknown')
        if any(service[k] is not True for k in ['unit_present', 'same_as_executable', 'cmdline_matches_unit']):
            raise Refusal('controller unit is not bound')
        if service['active_state'] != 'active' or service['sub_state'] != 'running' or service['shape'] != 'supported':
            raise Refusal('controller is not running in a supported shape')
        if type(service['main_pid']) is not int or service['main_pid'] <= 0 or service['running_sha256'] != image['sha256']:
            raise Refusal('controller process identity is not proved')
        if not isinstance(service['invocation_id'], str) or not re.fullmatch('[0-9a-f]{32}', service['invocation_id']):
            raise Refusal('controller invocation is unproved')
        specs = environment_specs(service)
        if service['environment_file_changed_since_start'] is not False and not (specs == [] and service['environment_file_changed_since_start'] is None):
            raise Refusal('controller environment changed or is unknown')
        if any(service[k] is not False for k in ['need_daemon_reload', 'config_changed_since_start']):
            raise Refusal('controller operands changed or are unknown')
        if service['cmdline_config_path'] != config or service['config_sha256'] != cfg['sha256']:
            raise Refusal('controller configuration differs from its process')
        guard = tx['converge_guard']
        if tx['active'] != 'converge-guard' or tx['lock_held'] is not False or tx['journal'] is not None:
            raise Refusal('controller preparation is not settled')
        if guard['holder'] != holder or guard['recovery_pointer'] is not False or guard['release_executable_verified'] is not True:
            raise Refusal('controller guard is not verified for this holder')
        if guard['release_executable'] != executable or guard['release_executable_sha256'] != image['sha256']:
            raise Refusal('controller guard executable differs from the observed image')
        if set(status) != {'schema', 'deployment', 'nodes', 'registrations', 'retirement', 'rollout'} or type(status['schema']) is not int or status['schema'] != 1 or status['deployment']['bound'] is not True or status['deployment']['id'] != deployment:
            raise Refusal('controller ledger is unbound')
        if status['retirement'] is not None or status['rollout'] is not None or not isinstance(status['registrations'], list):
            raise Refusal('controller ledger is busy or unreadable')
        for row in status['registrations']:
            if not isinstance(row, dict) or set(row) != {'name', 'incarnation', 'epoch', 'live', 'release', 'highest_release', 'digest'}:
                raise Refusal('controller registration is malformed')
            if type(row['epoch']) is not int or row['epoch'] < 1 or type(row['live']) is not bool:
                raise Refusal('controller registration is untyped')
            if any(not isinstance(row[k], str) for k in ['name', 'incarnation', 'release', 'highest_release', 'digest']):
                raise Refusal('controller registration is untyped')
        return dict(host=host, executable=executable, config=config, environment_files=specs,
                    deployment=deployment, executable_sha256=image['sha256'], config_sha256=cfg['sha256'])
    except (KeyError, TypeError):
        raise Refusal('controller observation is missing or malformed')
