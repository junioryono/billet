"""Refuse a commit that silently drops a test.

A scripted edit deleted TestAnOverrunningPhaseCannotPushTheNextPastTheBudget: the
range being replaced happened to contain a test a previous round had inserted
just before the anchor. Every gate stayed green, because a deleted test cannot
fail. Only the mutation run noticed, and only because it happened to name that
test.

This compares the Test function names in the working tree against HEAD and
reports any that are gone. Renames are reported too — deliberately. A rename is
cheap to confirm and a deletion is expensive to miss.

Run it before committing any scripted edit to a _test.go file:

    make tests-kept

Usage: python3 scripts/check-tests-kept.py [<path>...]  (default: every _test.go)
"""

import re
import subprocess
import sys

ROOT = subprocess.run(["git", "rev-parse", "--show-toplevel"],
                      capture_output=True, text=True, check=True).stdout.strip()
NAME = re.compile(r"^func (Test\w+)\(", re.MULTILINE)


def names(text):
    return set(NAME.findall(text))


def at_head(path):
    r = subprocess.run(["git", "show", "HEAD:" + path],
                       cwd=ROOT, capture_output=True, text=True)
    if r.returncode != 0:
        return set()

    return names(r.stdout)


def in_tree(path):
    try:
        with open(f"{ROOT}/{path}", encoding="utf-8") as f:
            return names(f.read())
    except FileNotFoundError:
        return set()


def tracked_test_files():
    r = subprocess.run(["git", "ls-files", "*_test.go"],
                       cwd=ROOT, capture_output=True, text=True, check=True)

    return [p for p in r.stdout.split("\n") if p]


paths = sys.argv[1:] or tracked_test_files()

missing = 0

for path in paths:
    gone = at_head(path) - in_tree(path)
    for name in sorted(gone):
        print(f"DROPPED: {path}: {name}")
        missing += 1

if missing:
    print(f"\n{missing} test(s) present at HEAD are gone from the working tree.")
    print("If a rename was intended, say so in the commit message. If not, this is")
    print("the failure that stays green: a deleted test cannot fail.")
    sys.exit(1)

print(f"no tests dropped across {len(paths)} file(s)")
