#!/usr/bin/env python3
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Fingerprint the retained task graph as data; never evaluate lookup strings."""

import argparse
import copy
import json
from pathlib import Path
import re
import sys

import jinja2
from jinja2 import nodes
import yaml

sys.dont_write_bytecode = True
READ_ONLY = {'stat', 'find', 'slurp', 'set_fact', 'assert', 'debug', 'fail', 'meta'}
INCLUDES = {'ansible.builtin.import_tasks', 'ansible.builtin.include_tasks'}
KEYWORDS = set('name when vars become become_user become_method register changed_when failed_when check_mode '
               'no_log ignore_errors ignore_unreachable notify loop loop_control until retries delay environment '
               'delegate_to delegate_facts run_once tags listen async poll connection throttle timeout diff '
               'block rescue always'.split())


def normalized(value):
    return json.dumps(value, sort_keys=True, ensure_ascii=True, separators=(',', ':'))


def load_tree(root):
    return {str(path.relative_to(root)): yaml.safe_load(path.read_text())
            for folder in ('tasks', 'handlers', 'vars', 'defaults')
            for path in sorted((root / folder).glob('*.yml'))}


def inventory(tree):
    rows = {}
    def record(key, task, ancestry):
        row = rows.setdefault(key, dict(task=normalized(task), contexts=[]))
        if row['task'] != normalized(task):
            raise ValueError('duplicate task name with different mapping: ' + key)
        context = normalized(ancestry)
        if context not in row['contexts']:
            row['contexts'].append(context)

    def walk(tasks, file, ancestry, stack):
        if not isinstance(tasks, list):
            raise ValueError('unrecognized task list: ' + file)
        for task in tasks:
            if not isinstance(task, dict) or not isinstance(task.get('name'), str):
                raise ValueError('unrecognized unnamed task: ' + file)
            wh = task.get('when', [])
            wh = [wh] if isinstance(wh, str) else wh
            if 'not billet_retirement_node_ordinary | bool' in wh:
                continue
            key = file + ' / ' + task['name']
            modules = [key for key in task if key not in KEYWORDS]
            branches = [key for key in ('block', 'rescue', 'always') if key in task]
            # Delegation on a structural task needs its own review, even when
            # the included list contains only modules otherwise exempted.
            if 'delegate_to' in task and (branches or any(module in INCLUDES for module in modules)):
                record(key, task, ancestry)
            if branches:
                if modules:
                    raise ValueError('unrecognized block form: ' + key)
                context = {key: value for key, value in task.items() if key not in branches}
                for branch in branches:
                    walk(task[branch], file, ancestry + [dict(file=file, branch=branch, task=context)], stack)
                continue
            if len(modules) != 1:
                raise ValueError('unrecognized module form: ' + key)
            module = modules[0]
            if module in INCLUDES:
                operand = task[module]
                if not isinstance(operand, str) or '{{' in operand or not re.fullmatch(r'[a-z0-9-]+\.yml', operand):
                    raise ValueError('unrecognized task include: ' + key)
                target = 'tasks/' + operand
                if target in stack:
                    raise ValueError('recursive task include: ' + key)
                walk(tree[target], target, ancestry + [dict(file=file, task=task)], stack + [target])
                continue
            # Unknown modules, include_role and delegation require explicit rows.
            if module.startswith('ansible.builtin.') and module.split('.')[-1] in READ_ONLY and 'delegate_to' not in task:
                continue
            record(key, task, ancestry)
    main = tree['tasks/main.yml']
    ordinary = [task for task in main if task['name'] == 'Converge the ordinary host only after retirement admits it']
    if len(ordinary) != 1:
        raise ValueError('ordinary block is missing or repeated')
    walk(ordinary, 'tasks/main.yml', [], ['tasks/main.yml'])
    walk(tree['handlers/main.yml'], 'handlers/main.yml', [], ['handlers/main.yml'])
    return rows


def definitions(tree):
    # Keep every operand definition, including transitive aliases and registered
    # observations. A whole normalized defining task binds how a fact is obtained.
    result = {}
    def walk(tasks, file):
        for task in tasks:
            if 'vars' in task or 'ansible.builtin.set_fact' in task or 'register' in task:
                result[file + ' / ' + task['name']] = normalized(task)
            for branch in ('block', 'rescue', 'always'):
                if branch in task:
                    walk(task[branch], file)
    for file, value in tree.items():
        if file.startswith(('vars/', 'defaults/')):
            result[file] = normalized(value)
        else:
            walk(value, file)
    return result


def template_names(text):
    # Include loop locals and condition variables as well as output expressions;
    # a new interpolated variable must not hide behind an existing for-loop.
    parsed = jinja2.Environment().parse(text)
    return sorted({node.name for node in parsed.find_all(nodes.Name)})


def judge(root, catalog, tree=None, templates=None):
    tree = load_tree(root) if tree is None else tree
    actual = inventory(tree)
    expected = catalog['rows']
    if set(actual) != set(expected):
        raise ValueError('unlisted or stale task rows: ' + repr(sorted(set(actual) ^ set(expected))))
    for key, row in actual.items():
        listed = expected[key]
        if row['task'] != listed['task'] or sorted(row['contexts']) != sorted(listed['contexts']):
            raise ValueError('changed full task mapping: ' + key)
        if not listed['sources'] or any(not re.match(r'^(E|const|inventory|loop|parity):.+', source) for source in listed['sources']):
            raise ValueError('missing operand source: ' + key)
    # Binding validation code as text prevents removing a type check while
    # leaving its YAML call and every template unchanged.
    for name, body in catalog['validation_sources'].items():
        if (root.parent.parent / name).read_text() != body:
            raise ValueError('changed operand validator: ' + name)
    if definitions(tree) != catalog['definitions']:
        raise ValueError('changed operand-producing definition')
    discovered = set()
    for key, listed in expected.items():
        task = json.loads(listed['task'])
        if 'ansible.builtin.template' in task:
            src = task['ansible.builtin.template']['src']
            if '{{' not in src:
                discovered.add(src)
            elif key not in catalog['template_tasks']:
                raise ValueError('unlisted dynamic template task: ' + key)
            else:
                discovered.update(catalog['template_tasks'][key])
    discovered.update(catalog['lookup_templates'])
    if discovered != set(catalog['templates']):
        raise ValueError('unlisted or stale template source')
    for name, review in catalog['templates'].items():
        body = (root / 'templates' / name).read_text() if templates is None else templates[name]
        if set(template_names(body)) != set(review['variables']):
            raise ValueError('unlisted template variable: ' + name)
        if body != review['text']:
            raise ValueError('changed template or fixed-path write: ' + name)
        if not review['writes'] or any(not value.startswith(('typed:', 'E:', 'constant:')) for value in review['variables'].values()):
            raise ValueError('incomplete template sweep: ' + name)
        if name in catalog['node_templates']:
            for line in body.splitlines():
                if re.match(r'\s*(BindsTo|Requires|After)\s*=', line) and re.search(r'\bbillet-(server|backup|upgrade)\.(service|timer)\b', line):
                    raise ValueError('node template depends on protected unit: ' + name)


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--case', choices=['positive', 'unlisted-task', 'changed-argument', 'changed-read-argv', 'unlisted-template-variable'], required=True)
    args = parser.parse_args()
    here = Path(__file__).resolve().parent
    root = here.parent / 'roles/host'
    catalog = json.loads((here / 'retained-drift.json').read_text())
    tree = load_tree(root)
    templates = {name: (root / 'templates' / name).read_text() for name in catalog['templates']}
    expected = None
    if args.case == 'unlisted-task':
        tree['tasks/main.yml'][-1]['block'].insert(1, {'name': 'Planted unlisted mutation', 'ansible.builtin.file': {'path': '/var/lib/billet/protected', 'state': 'absent'}})
        expected = 'unlisted or stale task rows'
    elif args.case == 'changed-argument':
        task = next(task for task in tree['tasks/account.yml'] if task['name'] == 'Render billet configuration')
        task['ansible.builtin.template']['mode'] = '0666'
        expected = 'changed full task mapping'
    elif args.case == 'changed-read-argv':
        tree['tasks/unit-readiness.yml'][0]['ansible.builtin.command']['argv'].append('--unexpected')
        expected = 'changed full task mapping'
    elif args.case == 'unlisted-template-variable':
        templates['msmtprc.j2'] += '\n{{ planted_unlisted_destination }}\n'
        expected = 'unlisted template variable'
    try:
        judge(root, catalog, copy.deepcopy(tree), templates)
    except ValueError as exc:
        if expected and expected in str(exc):
            print(args.case + ': refused as required: ' + str(exc))
            return
        raise
    if expected:
        raise ValueError('planted counterexample survived: ' + args.case)
    print('retained drift: every task, definition and template is reviewed')


if __name__ == '__main__':
    main()
