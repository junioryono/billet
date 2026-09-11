#!/usr/bin/env python3
"""Hold the Python endpoint representation to the vector table the Go one is
held to (tests/fixtures/endpoint-vectors.json), row by row.

THE IMPORT IS THE ONE A MODULE USES: the checkout root (the directory that
contains `ansible_collections`) goes on sys.path, the working directory is a
temporary one elsewhere, and the module is imported as
`ansible_collections.junioryono.billet.plugins.module_utils.endpoint`, so a
module_utils importable only from its own directory fails here.

A VACUOUS PASS IS A FAILURE: an empty table, a duplicated row, or a row of
neither shape fails before any assertion runs.
"""

import contextlib
import importlib
import ipaddress
import json
import os
import pathlib
import sys

# NO BYTECODE CACHE: a mutation run once left a __pycache__ compiled from a
# mutant beside a restored source of the same size and mtime, and Python
# executed the mutant while the traceback showed the restored lines.
sys.dont_write_bytecode = True
import tempfile

HERE = pathlib.Path(__file__).resolve().parent
COLLECTION = HERE.parent
CHECKOUT = COLLECTION.parents[2]
VECTORS = HERE / "fixtures" / "endpoint-vectors.json"

failures = []


def fail(msg):
    failures.append(msg)
    print("FAIL " + msg)


def load_module():
    if not (CHECKOUT / "ansible_collections" / "junioryono" / "billet").is_dir():
        raise SystemExit("endpoint_check: %s does not contain ansible_collections/junioryono/billet" % CHECKOUT)
    sys.path.insert(0, str(CHECKOUT))
    with tempfile.TemporaryDirectory() as elsewhere:
        cwd = os.getcwd()
        os.chdir(elsewhere)
        try:
            mod = importlib.import_module("ansible_collections.junioryono.billet.plugins.module_utils.endpoint")
        finally:
            os.chdir(cwd)
    where = pathlib.Path(mod.__file__).resolve()
    if CHECKOUT not in where.parents:
        raise SystemExit("endpoint_check: imported %s from outside the checkout %s" % (where, CHECKOUT))
    return mod


def load_vectors():
    doc = json.loads(VECTORS.read_text(encoding="utf-8"))
    rows = doc.get("vectors")
    if not rows:
        raise SystemExit("endpoint_check: the vector table is empty")
    seen = set()
    for row in rows:
        key = (row["input"], row["tls"])
        if key in seen:
            raise SystemExit("endpoint_check: %r appears twice" % (key,))
        seen.add(key)
        accepted = {"scheme", "kind", "host", "rooted", "port", "canonical"} <= set(row)
        refused = "refused" in row and "words" in row
        if accepted == refused:
            raise SystemExit("endpoint_check: %r is of neither shape" % (row["input"],))
    return rows


def check_rows(ep, rows):
    for row in rows:
        inp, tls = row["input"], row["tls"]
        if "refused" in row:
            try:
                got = ep.parse(inp, tls)
            except ep.EndpointError as exc:
                if exc.kind != row["refused"]:
                    fail("parse(%r, tls=%r) refused %s (%s), want %s" % (inp, tls, exc.kind, exc, row["refused"]))
                    continue
                for word in row["words"]:
                    if word not in str(exc):
                        fail("parse(%r, tls=%r): %s does not say %r" % (inp, tls, exc, word))
                continue
            fail("parse(%r, tls=%r) = %r, want a %s refusal" % (inp, tls, got, row["refused"]))
            continue

        try:
            got = ep.parse(inp, tls)
        except ep.EndpointError as exc:
            fail("parse(%r, tls=%r): %s" % (inp, tls, exc))
            continue
        want = (row["scheme"], row["kind"], row["host"], row["rooted"], row["port"], row["canonical"])
        have = (got.scheme, got.kind, got.host, got.rooted, got.port, got.canonical())
        if have != want:
            fail("parse(%r, tls=%r) = %r, want %r" % (inp, tls, have, want))
        try:
            again = ep.parse_canonical(row["canonical"])
        except ep.EndpointError as exc:
            fail("parse_canonical(%r): %s" % (row["canonical"], exc))
            continue
        if again != got or got != again:
            fail("parse_canonical(%r) is not equal to parse(%r)" % (row["canonical"], inp))
        if inp != row["canonical"]:
            # A text that is not its own canonical spelling (a bare one has no
            # scheme; a URL one differs in case, port or address text) refuses
            # as not canonical and nothing else.
            try:
                ep.parse_canonical(inp)
            except ep.EndpointError as exc:
                if exc.kind != "not_canonical":
                    fail("parse_canonical(%r) refused %s, want not_canonical" % (inp, exc.kind))
            else:
                fail("parse_canonical(%r) admitted a text that is not canonical" % (inp,))


def check_equality(ep):
    equal = [
        ("https://[::ffff:192.0.2.1]:8443", "https://192.0.2.1:8443"),
        ("https://[2001:DB8::1]:8443", "https://[2001:db8::1]:8443"),
        ("https://[2001:db8:0:0:0:0:0:1]:8443", "https://[2001:db8::1]:8443"),
        ("https://[2001:db8:0:0:1::1]:8443", "https://[2001:db8::1:0:0:1]:8443"),
        ("https://CONTROL.example:8443", "https://control.example:8443"),
        ("https://control.example:08443", "https://control.example:8443"),
    ]
    for a, b in equal:
        if ep.parse(a, True) != ep.parse(b, True):
            fail("%r and %r are not equal" % (a, b))

    different = [
        ("control.example:8443", True, "control.example:8444", True, "the port"),
        ("control.example:8443", True, "control.example:8443", False, "the scheme"),
        ("control.example.:8443", True, "control.example:8443", True, "rootedness"),
        ("localhost:8443", True, "127.0.0.1:8443", True, "a name and a literal"),
        ("control-a.example:8443", True, "control-b.example:8443", True, "two names"),
        ("192.0.2.1:8443", True, "192.0.2.2:8443", True, "two IPv4 literals"),
        ("[2001:db8::1]:8443", True, "[2001:db8::2]:8443", True, "two IPv6 literals"),
        ("192.0.2.1.:8443", True, "192.0.2.1:8443", True, "a rooted name and a literal"),
    ]
    for a, ta, b, tb, why in different:
        x, y = ep.parse(a, ta), ep.parse(b, tb)
        if x == y or y == x:
            fail("%r and %r are equal; they differ by %s" % (a, b, why))


def check_precedence(ep):
    # The raw text is judged before urlsplit is consulted: a tab is refused as
    # a control character even though urlsplit would strip it silently.
    saved = ep.urlsplit

    def boom(_):
        raise AssertionError("urlsplit was consulted before the raw text was judged")

    ep.urlsplit = boom
    try:
        try:
            ep.parse("con\ttrol.example:8443", True)
        except ep.EndpointError as exc:
            if exc.kind != "syntax":
                fail("a tab refused as %s, want syntax" % exc.kind)
        except AssertionError as exc:
            fail(str(exc))
        else:
            fail("a tab in the host was admitted")
    finally:
        ep.urlsplit = saved

    # The zone is refused BEFORE any unmapping: with ipv4_mapped made to raise,
    # the scoped mapped spelling is still refused as a zone.
    prop = ipaddress.IPv6Address.ipv4_mapped

    def raise_on_unmap(self):
        raise AssertionError("ipv4_mapped consulted before the zone was refused")

    ipaddress.IPv6Address.ipv4_mapped = property(raise_on_unmap)
    try:
        try:
            ep.parse("[::ffff:192.0.2.1%25eth0]:8443", True)
        except ep.EndpointError as exc:
            if exc.kind != "zone":
                fail("the scoped mapped address refused as %s, want zone" % exc.kind)
        except AssertionError as exc:
            fail(str(exc))
        else:
            fail("the scoped mapped address was admitted")
    finally:
        ipaddress.IPv6Address.ipv4_mapped = prop


def main():
    ep = load_module()
    rows = load_vectors()
    check_rows(ep, rows)
    check_equality(ep)
    check_precedence(ep)
    if failures:
        print("endpoint_check: %d failure(s)" % len(failures))
        return 1
    print("endpoint_check: %d rows, equality and precedence pass" % len(rows))
    return 0


if __name__ == "__main__":
    sys.exit(main())
