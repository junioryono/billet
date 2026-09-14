#!/usr/bin/env python3
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Exercise the selected-answerer observation, not the installation pin."""

import importlib.util
from pathlib import Path
import sys

sys.dont_write_bytecode = True
source = Path(__file__).resolve().parents[1] / 'plugins/filter/executable_version.py'
spec = importlib.util.spec_from_file_location('executable_version', source)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

for token, kind in [
    ('v0.10.1', 'release'), ('0.10.1', 'release'),
    ('(devel)', 'development'), ('(unknown)', 'development'),
    ('v0.10.1+dirty', 'development'), ('0.0.0-SNAPSHOT-abcdef1', 'development'),
    ('v0.0.0-20260909120000-012345abcdef', 'development'),
    ('v0.10.1-0.20260909120000-012345abcdef', 'development'),
    ('v0.10.1-rc.1.0.20260909120000-012345abcdef', 'development'),
    ('v0.10.1-20260909120000-012345abcdef', 'unreadable'),
    ('v0.10.0-0.20260909120000-012345abcdef', 'unreadable'),
    ('v0.0.0-20261309120000-012345abcdef', 'unreadable'),
    ('v0.0.0-20260909120000-abc', 'unreadable'),
    ('v00.10.1', 'unreadable'), ('banana', 'unreadable'),
]:
    raw = dict(rc=0, stdout='billet ' + token + ' linux/amd64\n')
    result = module.executable_version(raw)
    if result.get('type') != kind:
        sys.exit('%s: %r, want %s' % (token, result, kind))
    if kind == 'release' and result.get('value') != 'v0.10.1':
        sys.exit('the observed release was not canonical')
for raw in [{}, dict(stdout='billet v0.10.1'), dict(rc=0, stdout='billet\n v0.10.1'),
            dict(rc=0, stdout='billet v0.10.1', skipped=True),
            dict(rc=0, stdout='billet v0.10.1', failed=True),
            dict(rc=0, stdout='billet v0.10.1', unreachable=True)]:
    if module.executable_version(raw) != {'type': 'unreadable'}:
        sys.exit('an unanswered version was admitted: %r' % raw)
for rc in [-9, 1, 124, 137]:
    if module.executable_version(dict(rc=rc, stdout='billet v0.10.1')) != {'type': 'unreadable'}:
        sys.exit('exit %d beside a version was admitted' % rc)
print('ok   selected-answerer version: release, development and unreadable')
