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
        if set(op) != {'kind', 'path'} or op['kind'] not in ['read', 'write', 'mkdir', 'metadata', 'delete', 'recursive-chown', 'recursive-write', 'recursive-delete', 'recursive-walk']:
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
