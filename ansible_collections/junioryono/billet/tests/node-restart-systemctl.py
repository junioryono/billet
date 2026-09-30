"""A systemctl that answers node-restart.yml from a scripted case, and records it.

The case file names the node before the restart and the states each poll after
it reads, the last repeating. It answers exactly the three requests the task
file makes and refuses anything else, a restart that waits included, so a
regression to a blocking restart fails the case rather than being answered.
"""
import json
import pathlib
import sys

sys.dont_write_bytecode = True
case = pathlib.Path(sys.argv[1])
args = sys.argv[2:]
state = json.loads(case.read_text())
state['calls'].append(args)


def save():
    case.write_text(json.dumps(state))


def show(unit):
    # systemd prints properties in its own order, never the requested one.
    print('MainPID=' + unit['MainPID'])
    print('ActiveState=' + unit['ActiveState'])
    print('SubState=' + unit['SubState'])


if args == ['show', '--property=MainPID', '--value', 'billet-node.service']:
    save()
    if state.get('before_rc', 0):
        sys.exit(state['before_rc'])
    print(state['before']['MainPID'])
elif args == ['restart', '--no-block', 'billet-node.service']:
    state['restarted'] = True
    save()
elif args == ['show', '--property=ActiveState', '--property=SubState', '--property=MainPID', 'billet-node.service']:
    unit = state['before']
    if state['restarted']:
        after = state['after']
        unit = after[min(state['polls'], len(after) - 1)]
        state['polls'] += 1
    save()
    if unit.get('rc'):
        sys.exit(unit['rc'])
    show(unit)
else:
    save()
    sys.exit('the node-restart systemctl fake does not answer ' + repr(args))
