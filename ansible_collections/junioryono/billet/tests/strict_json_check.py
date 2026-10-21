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

    # THE PROVENANCE READER, which a backfill decides from: True only for the
    # record billet writes for exactly these bytes, False (never an error) for
    # everything else.
    proves = mod.FilterModule().filters()["provenance_proves"]
    sha = "a" * 64
    record = '{\n  "version": "v0.10.1",\n  "manifest_digest": "%s",\n  "binary_sha256": "%s"\n}\n' % ("b" * 64, sha)
    for name, (text, binary, want) in {
        "the record of these bytes": (record, sha, True),
        "a record of other bytes": (record, "c" * 64, False),
        "no manifest digest": ('{"manifest_digest": "", "binary_sha256": "%s"}' % sha, sha, False),
        "no binary hash": ('{"manifest_digest": "%s"}' % ("b" * 64), sha, False),
        "a hash that is not text": ('{"manifest_digest": "x", "binary_sha256": 7}', sha, False),
        "a repeated member": ('{"manifest_digest": "x", "binary_sha256": "%s", "binary_sha256": "%s"}' % ("c" * 64, sha), sha, False),
        "not JSON": ("installed", sha, False),
        "not an object": ('["%s"]' % sha, sha, False),
        "an empty binary hash": ('{"manifest_digest": "x", "binary_sha256": ""}', "", False),
        # What Go's typed decode refuses, so the node cannot use either.
        "a version that is not text": ('{"version": 7, "manifest_digest": "x", "binary_sha256": "%s"}' % sha, sha, False),
        "a digest that is not text": ('{"manifest_digest": 7, "binary_sha256": "%s"}' % sha, sha, False),
        "a record over 64 KiB": ('{"manifest_digest": "x", "binary_sha256": "%s", "pad": "%s"}' % (sha, "x" * (64 << 10)), sha, False),
        "deep nesting, which some decoders raise on": ('[' * 20000 + ']' * 20000, sha, False),
        "a member Go decodes whatever its case": ('{"VERSION": 7, "manifest_digest": "x", "binary_sha256": "%s"}' % sha, sha, False),
        "a NaN, which is Python's and not JSON's": ('{"manifest_digest": "x", "binary_sha256": "%s", "note": NaN}' % sha, sha, False),
        "not UTF-8": (b'{"manifest_digest": "x", "binary_sha256": "\xff"}', sha, False),
        "bytes": (record.encode("utf-8"), sha, True),
        "an unknown member, which Go ignores": ('{"manifest_digest": "x", "binary_sha256": "%s", "note": 1}' % sha, sha, True),
    }.items():
        try:
            got = proves(text, binary)
        except Exception as exc:  # the reader must never raise
            failures.append("provenance_proves: %s: raised %r" % (name, exc))
            continue
        if got is not want:
            failures.append("provenance_proves: %s: %r, want %r" % (name, got, want))

    if failures:
        for line in failures:
            print("strict_json_check: " + line, file=sys.stderr)
        print("strict_json_check: %d failure(s)" % len(failures), file=sys.stderr)
        sys.exit(1)
    print("strict_json_check: unique members at every depth admitted, a repeated one and non-text refused")


if __name__ == "__main__":
    main()
