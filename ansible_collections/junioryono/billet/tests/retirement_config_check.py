#!/usr/bin/env python3
# Copyright (c) 2026 junioryono
# Apache-2.0

"""The compatibility locator keeps undecodable content distinct from absence."""

import importlib.util
from pathlib import Path
import sys

sys.dont_write_bytecode = True
source = Path(__file__).resolve().parents[1] / 'plugins/filter/retirement_config.py'
spec = importlib.util.spec_from_file_location('retirement_config', source)
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)

for name, text, kind, roles, locator in [
    ('absent', None, 'absent', 'unknown', '/var/lib/billet/server'),
    ('merge-empty', 'server: {<<: {}, identity_dir: /srv/controller}', 'present', 'server', '/srv/controller'),
    ('merge-locator', 'server: {<<: {identity_dir: /srv/controller}}', 'present', 'server', '/srv/controller'),
    ('merge-override', 'server: {<<: {identity_dir: /srv/base}, identity_dir: /srv/controller}', 'present', 'server', '/srv/controller'),
    ('merge-order', 'server: {<<: [{identity_dir: /srv/first}, {identity_dir: /srv/second}]}', 'present', 'server', '/srv/first'),
    ('merge-alias', 'base: &base {identity_dir: /srv/controller}\nserver: {<<: *base}', 'present', 'server', '/srv/controller'),
    ('merged-alias', 'base: &base {<<: {identity_dir: /srv/old}, identity_dir: /srv/controller}\nserver: {<<: *base}', 'present', 'server', '/srv/controller'),
    ('nested-merge', 'server: {<<: {<<: {identity_dir: /srv/controller}}}', 'present', 'server', '/srv/controller'),
    ('seeded', 'server: {state_dir: /var/lib/billet/server, max_vcpu: 0}', 'present', 'server', '/var/lib/billet/server'),
    ('node-only', 'node: {server_addr: control-b:7717}', 'present', 'node', ''),
    ('undecodable', 'server: [', 'malformed', 'unknown', ''),
    ('empty', '', 'malformed', 'unknown', ''),
    ('null', 'null', 'malformed', 'unknown', ''),
    ('two-documents', 'server: {identity_dir: /srv/first}\n---\nserver: {identity_dir: /srv/second}', 'malformed', 'unknown', ''),
    ('duplicate-server', 'server: {identity_dir: /srv/first}\nserver: {identity_dir: /srv/second}', 'malformed', 'unknown', ''),
    ('duplicate', 'server: {identity_dir: /srv/first, identity_dir: /srv/second}', 'malformed', 'unknown', ''),
    ('duplicate-merged', 'server: {<<: {identity_dir: /srv/first, identity_dir: /srv/second}}', 'malformed', 'unknown', ''),
    ('duplicate-merge-key', 'server: {<<: {}, <<: {identity_dir: /srv/controller}}', 'malformed', 'unknown', ''),
    ('missing-locator', 'server: {}', 'unreadable', 'unknown', ''),
    ('padded-locator', 'server: {identity_dir: " /srv/controller"}', 'unreadable', 'unknown', ''),
    ('ambiguous-locator', 'server: {identity_dir: /srv/identity, state_dir: /srv/state}', 'unreadable', 'unknown', ''),
]:
    result = module.retirement_config(text)
    expected = dict(config=kind, roles=roles, identity_dir=locator)
    if {key: result.get(key) for key in expected} != expected:
        sys.exit('%s: %r, want %r' % (name, result, expected))
    if kind in ('malformed', 'unreadable') and not result.get('why'):
        sys.exit('%s: unknown location has no hold reason' % name)
print('ok   retirement locator: merge precedence, duplicate refusal and absence-only fallback')
