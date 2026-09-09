#!/usr/bin/env python3
"""Refuse a unit-file operation in the host role that does not notify the closing reload.

WHY THIS IS A GATE. On systemd 255 every bus unit-file operation that changes
something (`systemctl enable`, `disable`, `mask`, `preset`, `link`, `revert`)
sets the manager-wide `unit_file_state_outdated` flag, which every unit then
reports as `NeedDaemonReload=yes` until a daemon-reload clears it. The role
reloads when it starts and its enables set the flag again; left set, the next
dry run has to compare across it. So a real converge ends with the reload its
own operations left pending, and the mechanism is a handler every one of those
operations notifies. An enable added without the notify leaves the flag set
and this gate red, which is the point.

THE RULE: every task under the host role's tasks/ and handlers/ trees, at any
depth, whose module is systemd_service (fully qualified or short, `systemd`
included) and whose arguments carry `enabled` or `masked` notifies
`billet unit files changed`, directly or in a list. Handlers are held to it too:
a handler that enables a unit sets the flag like any task. Tasks inside
block/rescue/always are walked. The closing reload handlers themselves carry no
`enabled` or `masked` and are outside the rule by construction.

A VACUOUS PASS IS A FAILURE: zero such tasks means the walk stopped seeing them.

`--self-test` runs the rule against fixtures that must fail (a task without the
notify, a handler without it, a task in a nested directory without it) and one
that must pass, because a gate that has never refused anything proves nothing.
"""

import pathlib
import sys
import tempfile

import yaml

MODULES = {
    "ansible.builtin.systemd_service",
    "ansible.builtin.systemd",
    "systemd_service",
    "systemd",
}
HANDLER = "billet unit files changed"


def tasks_in(node):
    if isinstance(node, list):
        for item in node:
            for task in tasks_in(item):
                yield task
    elif isinstance(node, dict):
        yield node
        for key in ("block", "rescue", "always"):
            if key in node:
                for task in tasks_in(node[key]):
                    yield task


def unit_file_tasks(doc):
    for task in tasks_in(doc):
        for module in MODULES:
            args = task.get(module)
            if isinstance(args, dict) and ("enabled" in args or "masked" in args):
                yield task, module


def notifies(task):
    notify = task.get("notify")
    if isinstance(notify, str):
        return notify == HANDLER
    if isinstance(notify, list):
        return HANDLER in notify
    return False


def check(role_dir):
    found = 0
    failures = []
    for sub in ("tasks", "handlers"):
        for path in sorted((role_dir / sub).rglob("*.yml")):
            doc = yaml.safe_load(path.read_text()) or []
            for task, module in unit_file_tasks(doc):
                found += 1
                if not notifies(task):
                    failures.append(
                        "%s: task %r (%s) changes unit-file state and does not notify %r"
                        % (path.relative_to(role_dir), task.get("name", "<unnamed>"), module, HANDLER)
                    )
    if found == 0:
        failures.append("no systemd_service task with enabled/masked was found under %s; the walk is broken" % role_dir)
    return failures


def self_test():
    with tempfile.TemporaryDirectory() as root:
        role = pathlib.Path(root)
        (role / "tasks" / "nested").mkdir(parents=True)
        (role / "handlers").mkdir()
        good = "- name: ok\n  ansible.builtin.systemd_service:\n    name: a\n    enabled: true\n  notify: billet unit files changed\n"
        (role / "tasks/main.yml").write_text(good)
        (role / "handlers/main.yml").write_text("- name: Reload systemd\n  ansible.builtin.systemd_service:\n    daemon_reload: true\n")
        assert check(role) == [], check(role)

        (role / "tasks/main.yml").write_text(
            good + "- name: nested\n  block:\n    - name: bad\n      systemd:\n        name: b\n        masked: true\n"
        )
        failures = check(role)
        assert len(failures) == 1 and "'bad'" in failures[0], failures

        (role / "tasks/main.yml").write_text(good)
        (role / "tasks/nested/deep.yml").write_text("- name: deep\n  ansible.builtin.systemd_service:\n    name: c\n    enabled: false\n")
        failures = check(role)
        assert len(failures) == 1 and "'deep'" in failures[0] and "nested" in failures[0], failures
        (role / "tasks/nested/deep.yml").unlink()

        (role / "handlers/main.yml").write_text("- name: handler enable\n  ansible.builtin.systemd_service:\n    name: d\n    enabled: true\n")
        failures = check(role)
        assert len(failures) == 1 and "'handler enable'" in failures[0], failures
        (role / "handlers/main.yml").write_text("")

        (role / "tasks/main.yml").write_text(
            "- name: ok\n  ansible.builtin.systemd_service:\n    name: a\n    enabled: true\n  notify:\n    - other\n    - billet unit files changed\n"
        )
        assert check(role) == [], check(role)

        (role / "tasks/main.yml").write_text("- name: none\n  ansible.builtin.debug:\n    msg: hi\n")
        failures = check(role)
        assert len(failures) == 1 and "walk is broken" in failures[0], failures
    print("self-test ok")


def main(argv):
    if argv[1:] == ["--self-test"]:
        self_test()
        return 0
    if len(argv) != 2:
        print("usage: unit_file_notify_check.py <role dir> | --self-test", file=sys.stderr)
        return 2
    failures = check(pathlib.Path(argv[1]))
    for f in failures:
        print("FAIL: " + f, file=sys.stderr)
    if failures:
        return 1
    print("unit-file notify: every enable, disable and mask notifies the closing reload")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv))
