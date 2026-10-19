"""A systemctl and a billet that answer node-restart.yml from a scripted case, and record it.

The case file names the node before the restart, the states each poll after the
stop reads (`stopping`, the last repeating), the states each poll after the
start reads (`after`, the last repeating), and what `billet node ending`
answers for each call (`ending`: a fixture of tests/fixtures/node-ending and an
exit status, or an older release's refusal). It answers exactly the requests
the task files make and refuses anything else, a restart and a waiting stop or
start included, so a regression to either fails the case rather than being
answered.
"""
import json
import pathlib
import sys

sys.dont_write_bytecode = True
tool = sys.argv[1]
case = pathlib.Path(sys.argv[2])
args = sys.argv[3:]
fixtures = pathlib.Path(__file__).resolve().parent / 'fixtures' / 'node-ending'
state = json.loads(case.read_text())
state['calls'].append([tool] + args)


def save():
    case.write_text(json.dumps(state))


def show(unit, properties):
    # systemd prints properties in its own order, never the requested one. A
    # raw answer stands in for one systemd should not give.
    if 'raw' in unit:
        sys.stdout.write(unit['raw'])
        return
    for name in ['MainPID', 'ActiveState', 'SubState', 'Result', 'Job', 'InvocationID']:
        if name in properties:
            print(name + '=' + unit.get(name, {'Job': '', 'Result': 'success', 'InvocationID': ''}.get(name, '')))


def phase_unit():
    if state['started']:
        after = state['after']
        unit = after[min(state['polls'], len(after) - 1)]
        state['polls'] += 1
        return unit
    if state['stopped']:
        stopping = state['stopping']
        unit = stopping[min(state['stop_polls'], len(stopping) - 1)]
        state['stop_polls'] += 1
        return unit
    return state['before']


if tool == 'older':
    # A RELEASE FROM BEFORE THE RECORD, which refuses the word as cmdNode does.
    save()
    sys.stderr.write('billet node: unexpected argument "ending"\n')
    sys.exit(1)

if tool in ('billet', 'candidate'):
    # billet node ending <call> --config ... [--invocation I] [--drain]
    # [--state-dir D] --json, answered for the executable asked: `ending` maps
    # a call to its answer, or an executable to such a map.
    call = args[2] if args[:2] == ['node', 'ending'] and len(args) > 2 else ''
    answers = state.get('ending', {})
    answer = answers.get(tool, {}).get(call) if tool in answers else answers.get(call)
    save()
    if answer is None:
        sys.exit('the node-restart ' + tool + ' fake does not answer ' + repr(args))
    if 'stderr' in answer:
        sys.stderr.write(answer['stderr'])
    if 'fixture' in answer:
        text = (fixtures / (answer['fixture'] + '.json')).read_text()
        doc = json.loads(text)
        # A CHECK ANSWERS THE QUESTION ASKED. The fixture's invocation becomes
        # the one asked, everywhere it is named, unless the case keeps it to
        # stand for an answer to another question; its drain is the CLI's own,
        # and a fixture that answers another drain question is a case's
        # mistake, refused here rather than rewritten.
        if call == 'check' and not answer.get('verbatim'):
            if ('--drain' in args) != doc.get('drain', False):
                sys.exit('fixture ' + answer['fixture'] + ' answers another drain question than ' + repr(args))
            asked = args[args.index('--invocation') + 1] if '--invocation' in args else None
            if (asked is None) != ('invocation' not in doc):
                sys.exit('fixture ' + answer['fixture'] + ' answers another invocation question than ' + repr(args))
            if asked is not None:
                text = text.replace('"' + doc['invocation'] + '"', '"' + asked + '"').replace(
                    '--invocation ' + doc['invocation'], '--invocation ' + asked)
        sys.stdout.write(text)
    if 'raw' in answer:
        sys.stdout.write(answer['raw'])
    sys.exit(answer.get('rc', 0))

properties = [a.split('=', 1)[1] for a in args if a.startswith('--property=')]
if args == ['show', '--property=MainPID', '--property=InvocationID', 'billet-node.service']:
    save()
    if state.get('before_rc', 0):
        sys.exit(state['before_rc'])
    show(state['before'], properties)
elif args == ['stop', '--no-block', 'billet-node.service']:
    state['stopped'] = True
    save()
elif args == ['start', '--no-block', 'billet-node.service']:
    state['started'] = True
    save()
elif args[:1] == ['show'] and args[-1] == 'billet-node.service' and properties and \
        len(properties) == len(args) - 2:
    unit = phase_unit()
    save()
    if unit.get('rc'):
        sys.exit(unit['rc'])
    show(unit, properties)
else:
    save()
    sys.exit('the node-restart systemctl fake does not answer ' + repr(args))
