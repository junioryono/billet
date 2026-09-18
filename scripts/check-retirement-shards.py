#!/usr/bin/env python3
"""Require complete, disjoint retirement CI selection and nonempty execution.

The cross-shard execution inventory covers both route and fresh-request sections; requests were previously outside that union.
"""
import argparse
import collections
import json
import pathlib
import re
import subprocess
import sys
import time

ROOT = pathlib.Path(__file__).resolve().parents[1]
TESTS = ROOT / 'ansible_collections/junioryono/billet/tests'
COMMAND_SHARDS = ('boundaries', 'settled', 'node-config', 'registration', 'inert', 'effects', 'operations')
ROLE_SHARDS = {
    'retirement': 'parser',
    'retirement-routes': 'routes',
    'retirement-compatibility': 'compatibility',
    'retirement-node': 'retained',
    'retirement-recovery': 'recovery',
    'retirement-node-settled': 'settled',
    'retirement-parity-basic': 'parity-basic',
    'retirement-parity-network': 'parity-network',
    'retirement-parity-negative': 'parity-negative',
    'retirement-resume': 'resume',
    'retirement-request-evidence': 'request-evidence',
    'retirement-request-collection': 'request-collection',
    'retirement-request-windows': 'request-windows',
    'retirement-request-cancellation': 'request-cancellation',
    'retirement-request-retained': 'request-retained',
}
# Count invocations of the case runner, not protocol calls inside a case. The
# settled parser has two batched cases; recovery keeps both passes in one case.
ROLE_CASES = {'parser': 60, 'routes': 15, 'compatibility': 74,
              'retained': 10, 'recovery': 21, 'settled': 14,
              'parity-basic': 8, 'parity-network': 8, 'parity-negative': 4, 'resume': 2,
              'request-evidence': 12, 'request-collection': 12,
              'request-windows': 13, 'request-cancellation': 17, 'request-retained': 10}


def require_partition(expected, groups):
    counts = collections.Counter(name for group in groups.values() for name in group)
    if set(counts) != expected or any(count != 1 for count in counts.values()):
        raise ValueError('unselected or multiply selected cases: ' + repr(counts))
    if any(not group for group in groups.values()):
        raise ValueError('an empty shard cannot establish coverage')


def partition():
    groups = {name: [] for name in COMMAND_SHARDS}
    seen = set()
    for path in sorted((ROOT / 'cmd/billet').glob('*_test.go')):
        for name in re.findall(r'^func (TestRetirement\w*)\(t \*testing\.T\)', path.read_text(), re.M):
            if name in seen:
                raise ValueError('duplicate test declaration: ' + name)
            seen.add(name)
            if name == 'TestRetirementReprovesEachStoppedBoundary':
                shard = 'boundaries'
            elif path.stem == 'serverretiresettled_test':
                shard = 'settled'
            elif path.stem in ('serverretirecheck_test', 'serverretirecheckinput_test'):
                shard = 'node-config'
            elif path.stem == 'serverretireregistration_test':
                shard = 'registration'
            elif path.stem == 'serverretireinert_test':
                shard = 'inert'
            elif path.stem in ('serverretireadmission_test', 'serverretireadmissionorder_test',
                               'serverretirenodepath_test', 'serverretirewalk_test', 'serverretirepublication_test'):
                shard = 'effects'
            else:
                shard = 'operations'
            groups[shard].append(name)
    require_partition(seen, groups)
    return groups


def check_role_partition(workflow):
    matrix = re.search(r'^        group: \[(.*)\]$', workflow, re.M)
    if matrix is None:
        raise ValueError('host-lifecycle matrix missing')
    names = [name.strip() for name in matrix[1].split(',')]
    for name in [*ROLE_SHARDS, 'retirement-node-shape']:
        if names.count(name) != 1:
            raise ValueError('missing or duplicate host group: ' + name)
    if 'retirement-request' in names:
        raise ValueError('unsharded request group remains in the workflow')
    assignment = re.search(r"BILLET_RETIREMENT_SHARD: \$\{\{ fromJSON\('(.*?)'\)\[matrix.group\] \}\}", workflow)
    if assignment is None or json.loads(assignment[1]) != ROLE_SHARDS:
        raise ValueError('workflow does not pass the complete role shard map')
    condition = "if: contains(fromJSON('" + json.dumps(list(ROLE_SHARDS)) + "'), matrix.group)"
    if workflow.count(condition) != 2:
        raise ValueError('the role run and selection artifact must select exactly the mapped groups')
    driver = (TESTS / 'converge-guard-check.sh').read_text()
    routes = (TESTS / 'retirement-cases.sh').read_text()
    sources = driver + '\n' + routes
    selected = re.findall(r'^if retirement_section (\S+); then$', sources, re.M)
    if collections.Counter(selected) != collections.Counter(ROLE_SHARDS.values()):
        raise ValueError('role sections are missing or selected twice')
    owned = {'retirement-activation-cases.sh', 'retirement-cases.sh'}
    for section in ROLE_CASES:
        name = 'retirement-' + section + '-cases.sh'
        expected = (f'if retirement_section {section}; then\n'
                    f'  . "$here/{name}"\n  retirement_section_finished\n')
        if sources.count(expected) != 1 or sources.count(f'. "$here/{name}"') != 1:
            raise ValueError('source is not owned by exactly one section: ' + name)
        owned.add(name)
    declared = {path.name for path in TESTS.glob('retirement-*-cases.sh')} | {'retirement-cases.sh'}
    if declared != owned:
        raise ValueError('unselected retirement case source: ' + repr(declared ^ owned))
    settled = (TESTS / 'retirement-settled-cases.sh').read_text()
    activation = 'source "$here/retirement-activation-cases.sh"'
    if settled.count(activation) != 1:
        raise ValueError('settled shard lost its connected activation callers')
    for name in owned:
        source = (TESTS / name).read_text()
        if name != 'retirement-settled-cases.sh' and activation in source:
            raise ValueError('activation callers have a second owner')
        # R36-R41 belong to the separately designed lifecycle work.
        if any(36 <= int(number) <= 41 for number in re.findall(r'\br(\d+)[-\w]*', source)):
            raise ValueError('reserved lifecycle case selected as retirement: ' + name)
    imports = ('retained-guards.yml', 'retained-drift.yml', 'interpreted-inputs.yml', 'retained-operands.yml')
    old = (TESTS / 'host-upgrade-order.yml').read_text()
    new = (TESTS / 'retirement-node-shape.yml').read_text()
    for name in imports:
        if name in old or new.count('ansible.builtin.import_playbook: ' + name) != 1:
            raise ValueError('retained shape play is unselected or duplicated: ' + name)


def validate_cases(section, cases):
    if len(cases) != ROLE_CASES[section] or len(cases) != len(set(cases)):
        raise ValueError(f'{section}: expected {ROLE_CASES[section]} distinct cases; got {len(cases)}: {cases}')


def role_selection(args):
    if args.section is None or args.selection is None:
        raise ValueError('--section and --selection are required together')
    rows = [line.split('\t') for line in args.selection.read_text().splitlines()]
    if any(len(row) != 2 or row[0] not in ROLE_CASES or not row[1] for row in rows):
        raise ValueError('malformed role selection record')
    cases = [name for section, name in rows if section == args.section]
    validate_cases(args.section, cases)
    if args.report:
        if any(section != args.section for section, _ in rows):
            raise ValueError('a single-shard report contains another shard')
        args.report.write_text(json.dumps(dict(section=args.section, cases=cases), indent=2) + '\n')
    print(f'{args.section}: {len(cases)} distinct cases completed', file=sys.stderr)


def completed_roles(directory):
    groups = {}
    for path in sorted(directory.rglob('retirement-selection.json')):
        report = json.loads(path.read_text())
        section, cases = report['section'], report['cases']
        if section not in ROLE_CASES or section in groups:
            raise ValueError('unknown or repeated completed section: ' + section)
        validate_cases(section, cases)
        groups[section] = cases
    if set(groups) != set(ROLE_CASES):
        raise ValueError('missing completed sections: ' + repr(set(ROLE_CASES) - set(groups)))
    require_partition({case for cases in groups.values() for case in cases}, groups)
    print(f'{sum(map(len, groups.values()))} distinct retirement cases completed across {len(groups)} shards')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--run', choices=COMMAND_SHARDS)
    parser.add_argument('--section', choices=ROLE_CASES)
    parser.add_argument('--selection', type=pathlib.Path)
    parser.add_argument('--report', type=pathlib.Path)
    parser.add_argument('--completed-roles', type=pathlib.Path)
    args = parser.parse_args()
    if args.section or args.selection or args.report:
        role_selection(args)
        return 0
    groups = partition()
    workflow = (ROOT / '.github/workflows/ci.yml').read_text()
    job = workflow.split('  test-postgres-retirement:', 1)[1].split('\n  lint:', 1)[0]
    if re.findall(r'^          - shard: (\S+)$', job, re.M) != list(COMMAND_SHARDS):
        raise ValueError('PostgreSQL matrix differs from the complete partition')
    check_role_partition(workflow)
    required = workflow.split('  verify-all-checks:', 1)[1]
    if '        retirement-selection,' not in required:
        raise ValueError('retirement selection is absent from the required aggregate check')
    require_partition({'a', 'b'}, {'one': ['a'], 'two': ['b']})
    # Each negative preserves the other invariant: duplication keeps coverage;
    # omission contains no duplicate. Both must be rejected independently.
    for planted in ({'one': ['a', 'b'], 'two': ['a']}, {'one': ['a']}):
        try:
            require_partition({'a', 'b'}, planted)
        except ValueError:
            pass
        else:
            raise ValueError('partition validator accepted a planted omission/duplicate')
    if args.completed_roles:
        completed_roles(args.completed_roles)
    for name, tests in groups.items():
        print(f'{name}: {len(tests)} command tests', file=sys.stderr)
    if args.run:
        # The compiled inventory catches declarations the source scan missed;
        # neither a changed function signature nor a parser false positive skips.
        listing = subprocess.run(['go', 'test', '-list', '^TestRetirement', './cmd/billet/'],
                                 cwd=ROOT, text=True, stdout=subprocess.PIPE, check=True)
        discovered = set(re.findall(r'^TestRetirement\w*$', listing.stdout, re.M))
        require_partition(discovered, groups)
        pattern = '^(' + '|'.join(groups[args.run]) + ')$'
        start = time.monotonic()
        result = subprocess.run(['go', 'test', '-race', '-count=1', '-timeout', '60m', '-vet=off',
                                 '-run', pattern, '-json', './cmd/billet/'], cwd=ROOT, check=False)
        print(f'{args.run}: {len(groups[args.run])} tests; elapsed={time.monotonic() - start:.1f}s; exit={result.returncode}', file=sys.stderr)
        return result.returncode
    return 0


if __name__ == '__main__':
    try:
        sys.exit(main())
    except (ValueError, KeyError, IndexError, subprocess.CalledProcessError) as error:
        sys.exit(str(error))
