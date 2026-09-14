#!/usr/bin/env python3
"""Prove the from_json_strict filter reads what the Go decoders read and
refuses what they refuse: one value per member at every depth, and nothing
that is not JSON text. Run by converge-guard-check.sh before the cases."""

import importlib.util
import pathlib
import sys

HERE = pathlib.Path(__file__).resolve().parent
FILTER = HERE.parent / "plugins" / "filter" / "strict_json.py"


def load():
    spec = importlib.util.spec_from_file_location("strict_json", FILTER)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def main():
    mod = load()
    from ansible.errors import AnsibleFilterError

    f = mod.FilterModule().filters()["from_json_strict"]
    failures = []

    admitted = {
        "an object": ('{"active": "none"}', {"active": "none"}),
        "a nested object": ('{"a": {"b": 1}, "c": [{"d": 2}]}', {"a": {"b": 1}, "c": [{"d": 2}]}),
        "the same name in two objects": ('{"a": {"x": 1}, "b": {"x": 2}}', {"a": {"x": 1}, "b": {"x": 2}}),
        "bytes": (b'{"active": "none"}', {"active": "none"}),
    }
    for name, (text, want) in admitted.items():
        try:
            got = f(text)
        except AnsibleFilterError as exc:
            failures.append("%s: refused: %s" % (name, exc))
            continue
        if got != want:
            failures.append("%s: read %r, want %r" % (name, got, want))

    refused = {
        "a repeated member, valid last": ('{"pointer": true, "pointer": false}', "pointer is repeated"),
        "a repeated member, valid first": ('{"pointer": false, "pointer": null}', "pointer is repeated"),
        "a repeated member inside an object": ('{"guard": {"record_error": "x", "record_error": "y"}}', "record_error is repeated"),
        "a repeated member inside a list": ('{"why": [{"a": 1, "a": 2}]}', "a is repeated"),
        "not JSON": ("nope", "from_json_strict"),
        "bytes after the object": ('{"a": 1} {}', "from_json_strict"),
        "not text": (7, "not text"),
        "not UTF-8": (b"\xff", "not UTF-8"),
    }
    for name, (text, words) in refused.items():
        try:
            got = f(text)
        except AnsibleFilterError as exc:
            if words not in str(exc):
                failures.append("%s: refused as %r, want %r" % (name, str(exc), words))
            continue
        failures.append("%s: admitted as %r" % (name, got))

    if failures:
        for line in failures:
            print("strict_json_check: " + line, file=sys.stderr)
        print("strict_json_check: %d failure(s)" % len(failures), file=sys.stderr)
        sys.exit(1)
    print("strict_json_check: unique members at every depth admitted, a repeated one and non-text refused")


if __name__ == "__main__":
    main()
