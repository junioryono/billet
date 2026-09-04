#!/usr/bin/env python3
"""Refuse a network fetch in either role that is not bounded and retried.

WHY THIS IS A GATE AND NOT A CONVENTION. A get_url or uri task with the module
defaults runs once and waits for the kernel's own connect timeout, so one stalled
connection -- a CDN blip, a transient 5xx -- fails the whole converge at a
verification step. On a fleet converged from CI behind an approval gate that is
a red run and a spent approval for an outage nobody caused. Every fetch site
was fixed once; this is what stops the next one shipping without the fix.

THE RULE: every task whose module is get_url or uri (fully qualified or short)
carries `timeout` in its module arguments, and `register`, `until`, `retries`
and `delay` on the task itself. Tasks inside block/rescue/always are walked.

A VACUOUS PASS IS A FAILURE. Zero fetch tasks found means the walk stopped
seeing them -- a renamed module, a moved directory -- and every rule above
would pass without examining anything.
"""

import pathlib
import sys

import yaml

FETCH_MODULES = {
    "ansible.builtin.get_url",
    "get_url",
    "ansible.builtin.uri",
    "uri",
}

TASK_KEYS = ("register", "until", "retries", "delay")


def tasks_in(items, where):
    """Yield (task, where) for every task, descending into blocks."""
    for i, item in enumerate(items or []):
        if not isinstance(item, dict):
            continue
        here = f"{where}[{i}]"
        nested = False
        for key in ("block", "rescue", "always"):
            if key in item:
                nested = True
                yield from tasks_in(item[key], f"{here}.{key}")
        if not nested:
            yield item, here


def check(task, where):
    problems = []
    module = next((k for k in task if k in FETCH_MODULES), None)
    if module is None:
        return problems
    name = task.get("name", "<unnamed>")
    args = task.get(module)
    if not isinstance(args, dict) or "timeout" not in args:
        problems.append(f"{where} ({name!r}): {module} has no `timeout`; a stalled socket waits for the kernel")
    for key in TASK_KEYS:
        if key not in task:
            problems.append(f"{where} ({name!r}): no `{key}`; one failed attempt fails the converge")
    return problems


def main(root):
    roles = pathlib.Path(root) / "roles"
    files = sorted(roles.glob("*/tasks/*.yml"))
    if not files:
        print(f"fetch-retry-check: no task files under {roles}", file=sys.stderr)
        return 1

    found = 0
    problems = []
    for path in files:
        with open(path, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh)
        if not isinstance(doc, list):
            continue
        for task, where in tasks_in(doc, str(path.relative_to(roles.parent))):
            if any(k in FETCH_MODULES for k in task):
                found += 1
            problems.extend(check(task, where))

    if found == 0:
        print("fetch-retry-check: found no get_url or uri task at all; the rule would pass "
              "without examining anything", file=sys.stderr)
        return 1

    for p in problems:
        print(f"fetch-retry-check: {p}", file=sys.stderr)
    if problems:
        return 1

    print(f"fetch-retry-check: {found} fetch tasks, every one bounded and retried")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1] if len(sys.argv) > 1 else "."))
