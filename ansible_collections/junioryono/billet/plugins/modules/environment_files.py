#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Read bounded regular environment files without returning credential bytes."""

DOCUMENTATION = r'''
---
module: environment_files
short_description: Prove typed environment file readability without returning contents
description:
  - Opens regular files by descriptor with a one MiB bound and preserves optional absence.
  - Optionally runs billet check with the observed environment without shell evaluation.
options:
  specs:
    description: Strict path and Boolean ignore_errors entries from release inspect.
    type: list
    elements: dict
    required: true
  check:
    description: Run the ordinary installed billet validation with this environment.
    type: bool
    default: false
author: [junioryono]
'''
EXAMPLES = r'''
- name: Prove retained environment readability
  junioryono.billet.environment_files:
    specs: "{{ billet_node_environment_files }}"
'''
RETURN = r'''
presence:
  description: Ordered presence observations, with no contents or hashes.
  type: list
  returned: success
'''

import errno
import os
import re
import stat

from ansible.module_utils.basic import AnsibleModule
from ansible_collections.junioryono.billet.plugins.module_utils.ordinary_inputs import environment_specs, Refusal

LIMIT = 1 << 20


def read_environment(spec):
    path = spec['path']
    try:
        identity = os.open(path, os.O_PATH | os.O_CLOEXEC)
    except OSError as exc:
        if exc.errno == errno.ENOENT and spec['ignore_errors']:
            try:
                os.lstat(path)
            except OSError as absent:
                if absent.errno == errno.ENOENT:
                    return None
        raise Refusal('environment file is not positively readable')
    try:
        before = os.fstat(identity)
        if not stat.S_ISREG(before.st_mode) or before.st_size > LIMIT:
            raise Refusal('environment file is not a bounded regular file')
        fd = os.open('/proc/self/fd/%d' % identity, os.O_RDONLY | os.O_CLOEXEC | os.O_NONBLOCK)
        try:
            with os.fdopen(fd, 'rb') as stream:
                body = stream.read(LIMIT + 1)
                after = os.fstat(stream.fileno())
            current = os.stat(path)
            if len(body) > LIMIT or (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns, before.st_ctime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns, after.st_ctime_ns) or (after.st_dev, after.st_ino) != (current.st_dev, current.st_ino):
                raise Refusal('environment file changed during observation')
            return body
        except OSError:
            raise Refusal('environment file could not be read consistently')
    finally:
        os.close(identity)


def parse_environment(body):
    # This is releaseinspect.go's restricted line grammar. Values are never
    # expanded by a shell, even when they contain dollars or command syntax.
    try:
        text = body.decode('utf-8')
    except UnicodeError:
        raise Refusal('environment encoding is unsupported')
    if any((ord(c) < 32 and c not in '\n\t') or 127 <= ord(c) <= 159 or 0xFDD0 <= ord(c) <= 0xFDEF or ord(c) & 0xFFFE == 0xFFFE for c in text):
        raise Refusal('environment encoding is unsupported')
    result = {}
    for line in text.split('\n'):
        if not line.strip() or line.startswith(('#', ';')):
            continue
        name, sep, value = line.partition('=')
        if name in result:
            raise Refusal('environment assignment is repeated')
        if not sep or not re.fullmatch('[A-Za-z_][A-Za-z0-9_]*', name):
            raise Refusal('environment assignment is unsupported')
        if len(value) >= 2 and value[0] in "\"'" and value[-1] == value[0]:
            inner = value[1:-1]
            if value[0] in inner or '\\' in inner:
                raise Refusal('environment value is unsupported')
            value = inner
        elif value != value.strip() or any(c in value for c in "\"'\\#"):
            raise Refusal('environment value is unsupported')
        result[name] = value
    return result


def main():
    module = AnsibleModule(argument_spec=dict(specs=dict(type='list', elements='dict', required=True), check=dict(type='bool', default=False)), supports_check_mode=True)
    try:
        specs = environment_specs(dict(unit_present=True, environment_file_specs=module.params['specs']))
        presence, environment = [], {}
        for spec in specs:
            body = read_environment(spec)
            presence.append('absent' if body is None else 'present')
            if body is not None:
                environment.update(parse_environment(body))
        if module.params['check'] and not module.check_mode:
            rc, _, _ = module.run_command(['/usr/bin/billet', 'check', '--config', '/etc/billet/billet.yaml'], environ_update=environment)
            if rc != 0:
                raise Refusal('billet validation with the observed node environment refused')
        module.exit_json(changed=False, presence=presence)
    except (OSError, Refusal):
        module.fail_json(msg='EnvironmentFile observation or validation refused; no credential contents are reported.')


if __name__ == '__main__':
    main()
