#!/usr/bin/env python3
"""Hold the collection's holder predicate (plugins/module_utils/holder.py, the
one the filter and the fallback reader share) to the vector table the Go
`checkHolder` wrote (tests/fixtures/holder-vectors.json), row by row, and
refuse a vacuous pass: an empty table, a table with no refused row or no
accepted row, or a row of the wrong shape fails before any assertion. The
filter is imported by the collection's dotted name too, so a filter that
stopped exposing the shared predicate fails here.
"""

import importlib
import json
import os
import pathlib
import sys
import tempfile

sys.dont_write_bytecode = True

HERE = pathlib.Path(__file__).resolve().parent
CHECKOUT = HERE.parents[3]
VECTORS = HERE / "fixtures" / "holder-vectors.json"


def load():
    if not (CHECKOUT / "ansible_collections" / "junioryono" / "billet").is_dir():
        raise SystemExit("holder_check: %s does not contain ansible_collections/junioryono/billet" % CHECKOUT)
    sys.path.insert(0, str(CHECKOUT))
    with tempfile.TemporaryDirectory() as elsewhere:
        cwd = os.getcwd()
        os.chdir(elsewhere)
        try:
            mod = importlib.import_module("ansible_collections.junioryono.billet.plugins.module_utils.holder")
            flt = importlib.import_module("ansible_collections.junioryono.billet.plugins.filter.holder")
        finally:
            os.chdir(cwd)
    for m in (mod, flt):
        where = pathlib.Path(m.__file__).resolve()
        if CHECKOUT not in where.parents:
            raise SystemExit("holder_check: imported %s from outside the checkout %s" % (where, CHECKOUT))
    if flt.FilterModule().filters().get("valid_holder") is not mod.valid_holder:
        raise SystemExit("holder_check: the filter does not expose the shared predicate")
    return mod


def main():
    mod = load()

    doc = json.loads(VECTORS.read_text(encoding="utf-8"))
    rows = doc.get("vectors")
    if not isinstance(rows, list) or not rows:
        sys.exit("holder_check: the vector table is empty or not a list")
    if not any(r.get("valid") is True for r in rows) or not any(r.get("valid") is False for r in rows):
        sys.exit("holder_check: the vector table carries no accepted or no refused row")

    failures = 0
    for r in rows:
        if set(r) != {"holder", "valid"} or not isinstance(r["holder"], str) or not isinstance(r["valid"], bool):
            sys.exit("holder_check: a row is not {holder: str, valid: bool}: %r" % (r,))
        got = mod.valid_holder(r["holder"])
        if got != r["valid"]:
            failures += 1
            print("FAIL %r: python says %s, checkHolder says %s" % (r["holder"], got, r["valid"]))
    for bad in (None, 7, ["h1"], {"h": 1}):
        if mod.valid_holder(bad):
            failures += 1
            print("FAIL %r: a non-string is not a holder" % (bad,))
    # LONE SURROGATES are text a JSON escape can produce and Go's decoder
    # never hands to checkHolder as valid UTF-8, so the table cannot carry
    # them; the twin refuses them without raising.
    for lone in ("\ud800", "a\udfffb", "\ud83d"):
        try:
            got = mod.valid_holder(lone)
        except Exception as exc:  # noqa: BLE001
            failures += 1
            print("FAIL %r: raised %r instead of refusing" % (lone, exc))
            continue
        if got:
            failures += 1
            print("FAIL %r: a lone surrogate is not a holder" % (lone,))
    if failures:
        print("%d failures" % failures)
        sys.exit(1)
    print("holder: every one of %d vectors agrees" % len(rows))


if __name__ == "__main__":
    main()
