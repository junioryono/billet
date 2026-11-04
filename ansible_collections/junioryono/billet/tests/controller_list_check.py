#!/usr/bin/env python3
"""Hold the `controller_list` filter to what the commands write, by the collection's
dotted name, so a filter that stopped exposing the function fails here.
"""

import importlib
import os
import pathlib
import sys
import tempfile

sys.dont_write_bytecode = True

CHECKOUT = pathlib.Path(__file__).resolve().parents[4]

A, B = "http://127.0.0.1:7717", "http://127.0.0.1:7719"

ROWS = [
    ("a list holding its member", [A, B], A, True),
    ("the other member", [A, B], B, True),
    ("a list of one", [A], A, True),
    ("a list without its member", [B], A, False),
    ("no member to hold", [A, B], None, False),
    ("a null", None, A, False),
    ("a string, whose characters are not controllers", A, A, False),
    ("a mapping", {A: B}, A, False),
    ("an empty list", [], A, False),
    ("a nested list", [[A], [B]], A, False),
    ("a nested list beside its member", [A, [B]], A, False),
    ("a number among the strings", [A, 17], A, False),
    ("a null among the strings", [A, None], A, False),
    ("an empty string", [A, ""], A, False),
    ("one controller twice", [A, A], A, False),
]


def main():
    sys.path.insert(0, str(CHECKOUT))
    with tempfile.TemporaryDirectory() as elsewhere:
        cwd = os.getcwd()
        os.chdir(elsewhere)
        try:
            mod = importlib.import_module("ansible_collections.junioryono.billet.plugins.filter.controller_list")
        finally:
            os.chdir(cwd)
    where = pathlib.Path(mod.__file__).resolve()
    if CHECKOUT not in where.parents:
        raise SystemExit("controller_list_check: imported %s from outside the checkout %s" % (where, CHECKOUT))
    judged = mod.FilterModule().filters()["controller_list"]
    for name, value, member, want in ROWS:
        got = judged(value, member)
        if got is not want:
            raise SystemExit("controller_list_check: %s: got %r, want %r" % (name, got, want))
    print("ok   controller_list: %d rows" % len(ROWS))


if __name__ == "__main__":
    main()
