#!/usr/bin/env python3
"""Refuse a network fetch in either role that is not bounded and retried.

WHY THIS IS A GATE AND NOT A CONVENTION. A get_url or uri task with the module
defaults runs once and waits for the kernel's own connect timeout, so one stalled
connection -- a CDN blip, a transient 5xx -- fails the whole converge at a
verification step. On a fleet converged from CI behind an approval gate that is
a red run and a spent approval for an outage nobody caused. Every fetch site
was fixed once; this is what stops the next one shipping without the fix.

THE RULE: every task whose module is get_url or uri -- as a task key, fully
qualified or short, or through `action:`/`local_action:` in either its mapping
or its string form -- carries `timeout` in its module arguments, and
`register`, `until`, `retries` and `delay` on the task itself. Tasks inside
block/rescue/always are walked; every .yml and .yaml under each role's tasks/
and handlers/ trees is read, because a fetch in a nested task file or a
handler is a fetch.

A VACUOUS PASS IS A FAILURE. Zero fetch tasks found means the walk stopped
seeing them -- a renamed module, a moved directory -- and every rule above
would pass without examining anything.

`--self-test` runs the rules against fixtures that must fail and fixtures that
must pass, because a gate that has never refused anything proves nothing.
"""

import pathlib
import shlex
import sys
import tempfile

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


def fetch_of(task):
    """Return (module, args) when the task is a fetch, else (None, None).

    Three spellings reach the same module: the module as the task key, and
    `action:`/`local_action:` holding either a mapping with `module` or the
    free-form "get_url url=... timeout=..." string.
    """
    for key in task:
        if key in FETCH_MODULES:
            args = task[key]
            return key, args if isinstance(args, dict) else {}
    for key in ("action", "local_action"):
        if key not in task:
            continue
        spec = task[key]
        if isinstance(spec, dict):
            module = spec.get("module")
            if module in FETCH_MODULES:
                return module, {k: v for k, v in spec.items() if k != "module"}
        elif isinstance(spec, str):
            words = shlex.split(spec)
            if words and words[0] in FETCH_MODULES:
                args = dict(w.split("=", 1) for w in words[1:] if "=" in w)
                return words[0], args
    return None, None


def check(task, where):
    module, args = fetch_of(task)
    if module is None:
        return None, []
    problems = []
    name = task.get("name", "<unnamed>")
    if "timeout" not in args:
        problems.append(f"{where} ({name!r}): {module} has no `timeout`; a stalled socket waits for the kernel")
    for key in TASK_KEYS:
        if key not in task:
            problems.append(f"{where} ({name!r}): no `{key}`; one failed attempt fails the converge")
    return module, problems


def scan(collection):
    """Return (fetches found, problems) for every task file under the roles."""
    roles = pathlib.Path(collection) / "roles"
    files = []
    for sub in ("tasks", "handlers"):
        for ext in ("*.yml", "*.yaml"):
            files.extend(roles.glob(f"*/{sub}/**/{ext}"))
    files = sorted(set(files))

    found = 0
    problems = []
    for path in files:
        with open(path, encoding="utf-8") as fh:
            doc = yaml.safe_load(fh)
        if not isinstance(doc, list):
            continue
        for task, where in tasks_in(doc, str(path.relative_to(roles.parent))):
            module, task_problems = check(task, where)
            if module is not None:
                found += 1
            problems.extend(task_problems)

    if not files:
        problems.append(f"no task files under {roles}")
    return found, problems


def verdict(found, problems, label="fetch-retry-check"):
    if found == 0:
        print(f"{label}: found no get_url or uri task at all; the rule would pass without "
              "examining anything", file=sys.stderr)
        return 1
    for p in problems:
        print(f"{label}: {p}", file=sys.stderr)
    if problems:
        return 1
    print(f"{label}: {found} fetch tasks, every one bounded and retried")
    return 0


GOOD_TASK = """
- name: bounded and retried
  ansible.builtin.get_url:
    url: https://example.invalid/a
    dest: /tmp/a
    timeout: 30
  register: r
  until: r is succeeded
  retries: 5
  delay: 5
"""

FIXTURES = {
    # (expected exit, files)
    "action mapping without retries is refused": (1, {
        "roles/x/tasks/main.yml": """
- name: action form
  action:
    module: get_url
    url: https://example.invalid/a
    dest: /tmp/a
    timeout: 30
  register: r
"""}),
    "action string without timeout is refused": (1, {
        "roles/x/tasks/main.yml": """
- name: string form
  action: uri url=https://example.invalid/a
  register: r
  until: r is succeeded
  retries: 5
  delay: 5
"""}),
    "a nested .yaml inside a rescue is seen": (1, {
        "roles/x/tasks/main.yml": GOOD_TASK,
        "roles/x/tasks/sub/more.yaml": """
- name: outer
  block:
    - name: noop
      ansible.builtin.debug:
        msg: hi
  rescue:
    - name: fetch without a bound
      ansible.builtin.uri:
        url: https://example.invalid/a
      register: r
      until: r is succeeded
      retries: 5
      delay: 5
"""}),
    "a handler is seen": (1, {
        "roles/x/tasks/main.yml": GOOD_TASK,
        "roles/x/handlers/main.yml": """
- name: refresh
  uri:
    url: https://example.invalid/a
    timeout: 30
"""}),
    "zero fetches is a failure": (1, {
        "roles/x/tasks/main.yml": """
- name: nothing
  ansible.builtin.debug:
    msg: hi
"""}),
    "every spelling done right passes": (0, {
        "roles/x/tasks/main.yml": GOOD_TASK + """
- name: action mapping
  action:
    module: ansible.builtin.uri
    url: https://example.invalid/a
    timeout: 30
  register: r2
  until: r2 is succeeded
  retries: 5
  delay: 5
- name: string form
  local_action: get_url url=https://example.invalid/a dest=/tmp/b timeout=30
  register: r3
  until: r3 is succeeded
  retries: 5
  delay: 5
"""}),
}


def self_test():
    failed = 0
    for label, (want, files) in FIXTURES.items():
        with tempfile.TemporaryDirectory() as tmp:
            for rel, body in files.items():
                path = pathlib.Path(tmp) / rel
                path.parent.mkdir(parents=True, exist_ok=True)
                path.write_text(body, encoding="utf-8")
            found, problems = scan(tmp)
            got = 1 if (found == 0 or problems) else 0
        if got != want:
            failed += 1
            print(f"fetch-retry-check self-test: {label}: exit {got}, want {want}"
                  f" (found={found}, problems={problems})", file=sys.stderr)
        else:
            print(f"ok   self-test: {label}")
    return 1 if failed else 0


def main(argv):
    if argv and argv[0] == "--self-test":
        return self_test()
    root = argv[0] if argv else "."
    found, problems = scan(root)
    return verdict(found, problems)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
