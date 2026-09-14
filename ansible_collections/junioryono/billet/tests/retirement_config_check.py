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

seed = Path(__file__).resolve().parents[4] / 'deploy/billet.yaml'

for name, text, kind, roles, locator in [
    ('absent', None, 'absent', 'unknown', '/var/lib/billet/server'),
    ('identity-wins', 'server: {identity_dir: /srv/identity, state_dir: /srv/state}', 'present', 'server', '/srv/identity'),
    ('state-dir', 'server: {state_dir: /srv/state}', 'present', 'server', '/srv/state'),
    ('state-block-identity', 'server: {state: {backend: postgres}, identity_dir: /srv/identity}', 'present', 'server', '/srv/identity'),
    ('state-block-state-dir', 'server: {state: {}, state_dir: /srv/state}', 'present', 'server', '/srv/state'),
    ('state-block-identity-wins', 'server: {state: {}, identity_dir: /srv/identity, state_dir: /srv/state}', 'present', 'server', '/srv/identity'),
    ('seeded', 'server: {state_dir: /var/lib/billet/server, max_vcpu: 0}', 'present', 'server', '/var/lib/billet/server'),
    ('package-seed', seed.read_bytes(), 'present', 'server', '/var/lib/billet/server'),
    ('node-only', 'node: {server_addr: control-b:7717}', 'present', 'node', ''),
    ('named-node-only', 'node: {name: x}', 'present', 'node', ''),
    # KnownFields rejects note in Go. This is only a locator hint; section R
    # requires independent service-manager proof before admitting this host.
    ('unknown-field-locator-hint', 'note: "&anchor *alias << !tag"\nserver: {identity_dir: /srv/controller}', 'present', 'server', '/srv/controller'),
    ('yaml-looking-comment', '# &anchor *alias << !tag\nserver: {identity_dir: /srv/controller}', 'present', 'server', '/srv/controller'),
    ('quoted-locator', 'server: {identity_dir: "/srv/controller"}', 'present', 'server', '/srv/controller'),
    ('utf8', 'server: {identity_dir: /srv/contrôleur}'.encode('utf-8'), 'present', 'server', '/srv/contrôleur'),
    ('yaml-1.1', b'%YAML 1.1\n---\nnode: {}\n', 'present', 'node', ''),
]:
    result = module.retirement_config(text)
    expected = dict(config=kind, roles=roles, identity_dir=locator)
    if result != expected:
        sys.exit('%s: %r, want %r' % (name, result, expected))

holds = [
    ('default', 'server: {}', 'unreadable', "Go's default depends on the running account"),
    ('null-state-default', 'server: {state: ~}', 'unreadable', "Go's default depends on the running account"),
    ('empty-mapping', '{}', 'unreadable', 'defines neither a server nor a node section'),
    ('no-role', 'note: no roles', 'unreadable', 'defines neither a server nor a node section'),
    ('null-node', 'node: ~', 'unreadable', 'node must be a mapping'),
    ('sequence-node', 'node: []', 'unreadable', 'node must be a mapping'),
    ('scalar-node', 'node: x', 'unreadable', 'node must be a mapping'),
    ('cyclic-merge', 'server: &s {<<: *s, identity_dir: /srv/clean}', 'malformed', 'alias *s'),
    ('merge', 'server: {<<: {}, identity_dir: /srv/clean}', 'malformed', 'merge key <<'),
    ('quoted-merge-key', 'server: {"<<": {}, identity_dir: /srv/clean}', 'malformed', 'merge key <<'),
    ('nested-merge', 'node: {extra: [{<<: {}}]}', 'malformed', 'merge key <<'),
    ('anchor', 'server: {identity_dir: &location /srv/clean}', 'malformed', 'anchor &location'),
    ('alias-anywhere', 'server: {identity_dir: /srv/clean}\nextra: [&x value, *x]', 'malformed', 'alias *x'),
    ('alias-without-anchor', 'server: {identity_dir: /srv/clean}\nextra: *x', 'malformed', 'alias *x'),
    ('tag', 'server: {identity_dir: /srv/clean}\nextra: !custom value', 'malformed', 'explicit tag !custom'),
    ('default-tag', 'server: {identity_dir: !!str /srv/clean}', 'malformed', 'explicit tag tag:yaml.org,2002:str'),
    ('mapping-tag', 'server: !!map {identity_dir: /srv/clean}', 'malformed', 'explicit tag tag:yaml.org,2002:map'),
    ('non-specific-tag', 'server: {identity_dir: ! /srv/clean}', 'malformed', 'explicit tag !'),
    ('duplicate-server', 'server: {identity_dir: /srv/first}\nserver: {identity_dir: /srv/second}', 'malformed', "duplicate key 'server'"),
    ('duplicate', 'server: {identity_dir: /srv/first, identity_dir: /srv/second}', 'malformed', "duplicate key 'identity_dir'"),
    ('duplicate-nested', 'server: {identity_dir: /srv/clean}\nextra: [{key: one, key: two}]', 'malformed', "duplicate key 'key'"),
    ('non-string-key', 'server: {identity_dir: /srv/clean}\nextra: {5: value}', 'malformed', 'non-string mapping key'),
    ('complex-key', 'server: {identity_dir: /srv/clean}\nextra: {[a, b]: value}', 'malformed', 'non-string mapping key'),
    ('undecodable', 'server: [', 'malformed', 'cannot be decoded'),
    ('invalid-utf8', b'server: {identity_dir: /srv/\xff}', 'malformed', 'UTF-8'),
    ('surrogate-text', 'server: {identity_dir: /srv/\udcff}', 'malformed', 'UTF-8'),
    ('oversized-escape', b'node: {name: "\\UFFFFFFFF"}\n', 'malformed', 'cannot be decoded'),
    ('yaml-1.0', b'%YAML 1.0\n---\nnode: {}\n', 'malformed', 'unsupported YAML version'),
    ('yaml-1.2', b'%YAML 1.2\n---\nnode: {}\n', 'malformed', 'unsupported YAML version'),
    ('yaml-1.2-locator', b'%YAML 1.2\n---\nserver: {identity_dir: /srv/clean}\n', 'malformed', 'unsupported YAML version'),
    ('escaped-high-surrogate', b'node: {name: "\\uD800"}\n', 'malformed', 'decoded surrogate code point'),
    ('escaped-low-surrogate', b'node: {name: "\\uDFFF"}\n', 'malformed', 'decoded surrogate code point'),
    ('escaped-surrogate-key', b'node: {"\\uD800": value}\n', 'malformed', 'decoded surrogate code point'),
    ('escaped-surrogate-sequence', b'node: {extra: ["\\uD800"]}\n', 'malformed', 'decoded surrogate code point'),
    ('utf16', 'server: {identity_dir: /srv/clean}'.encode('utf-16'), 'malformed', 'UTF-8'),
    ('empty', '', 'malformed', 'single YAML document'),
    ('null', 'null', 'malformed', 'root is not a mapping'),
    ('sequence-root', '[]', 'malformed', 'root is not a mapping'),
    ('two-documents', 'server: {identity_dir: /srv/first}\n---\nserver: {identity_dir: /srv/second}', 'malformed', 'single YAML document'),
    ('null-server', 'server: ~\nnode: {}', 'unreadable', 'server must be a mapping'),
    ('sequence-server', 'server: []', 'unreadable', 'server must be a mapping'),
    ('state-block-no-locator', 'server: {state: {backend: postgres}}', 'unreadable', 'server.state supplies no default'),
    ('empty-state-block', 'server: {state: {}}', 'unreadable', 'server.state supplies no default'),
    ('invalid-state', 'server: {state: false, identity_dir: /srv/clean}', 'unreadable', 'server.state must be a mapping or null'),
    ('sequence-state', 'server: {state: [], identity_dir: /srv/clean}', 'unreadable', 'server.state must be a mapping or null'),
]
for key, other in [('identity_dir', 'state_dir'), ('state_dir', 'identity_dir')]:
    for value in ['false', '[]', '~', '5', '{}', '""']:
        holds.append(('%s-%s' % (key, value), 'server: {%s: %s, %s: /srv/clean}' % (key, value, other),
                      'unreadable', 'server.%s must be a non-empty string scalar' % key))
    for value in ['relative/path', '" /srv/controller"', '"/srv/controller "']:
        holds.append(('%s-%s' % (key, value), 'server: {%s: %s, %s: /srv/clean}' % (key, value, other),
                      'unreadable', 'server.%s must be absolute with no leading or trailing whitespace' % key))

for name, text, kind, reason in holds:
    result = module.retirement_config(text)
    expected = dict(config=kind, roles='unknown', identity_dir='')
    if {key: result.get(key) for key in expected} != expected:
        sys.exit('%s: %r, want %r' % (name, result, expected))
    if reason not in result.get('why', ''):
        sys.exit('%s: %r does not name %r' % (name, result, reason))

# Future exceptions after parsing must cross the same boundary as scanner
# overflow. A broken helper must never turn into an Ansible templating error.
original_reader = module._read_subset
try:
    module._read_subset = lambda text: None
    result = module.retirement_config('node: {}')
finally:
    module._read_subset = original_reader
if result != dict(config='malformed', roles='unknown', identity_dir='',
                  why='The installed configuration cannot be decoded as the supported YAML subset.'):
    sys.exit('unexpected post-parse failure did not return the named hold: %r' % result)
print('ok   retirement locator: restricted YAML, named holds and locator precedence')
