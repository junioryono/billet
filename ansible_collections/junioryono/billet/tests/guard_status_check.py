#!/usr/bin/env python3
"""Prove the guard_status module judges the status protocol member by member
(fixtures-c4a.md, P5), over the answers the REAL command produced
(tests/fixtures/guard-status/*.json, written by cmd/billet's own test) with
one member corrupted per case, and over the executable's failure shapes: a
non-zero exit, a hang past the bound, "unknown command", text that is not
JSON.

A VACUOUS PASS IS A FAILURE: the healthy fixtures must be judged capable
before any corruption is tried, and the malformed-record fixture must be
judged capable WITH its record_error, so a judge that refused everything
fails here too.
"""

import copy
import importlib.util
import json
import os
import pathlib
import stat
import sys

# NO BYTECODE CACHE: a mutation run once left a __pycache__ compiled from a
# mutant beside a restored source of the same size and mtime, and Python
# executed the mutant while the traceback showed the restored lines.
sys.dont_write_bytecode = True
import tempfile
import types

HERE = pathlib.Path(__file__).resolve().parent
MODULE = HERE.parent / "plugins" / "modules" / "guard_status.py"
FIXTURES = HERE / "fixtures" / "guard-status"

failures = []


def fail(msg):
    failures.append(msg)
    print("FAIL " + msg)


def load_module():
    if "ansible.module_utils.basic" not in sys.modules:
        ansible = types.ModuleType("ansible")
        module_utils = types.ModuleType("ansible.module_utils")
        basic = types.ModuleType("ansible.module_utils.basic")
        basic.AnsibleModule = object
        sys.modules.setdefault("ansible", ansible)
        sys.modules.setdefault("ansible.module_utils", module_utils)
        sys.modules["ansible.module_utils.basic"] = basic
    spec = importlib.util.spec_from_file_location("guard_status", MODULE)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def fixture(name):
    return json.loads((FIXTURES / (name + ".json")).read_text())


def judge_report(mod, report, expect="any"):
    body = json.dumps(report).encode("utf-8")
    try:
        mod.judge(0, body, b"", False, expect)
        return "capable", ""
    except mod.Refused as exc:
        return "refused", str(exc)


def check_healthy(mod):
    for name in ["none", "healthy", "healthy-with-pointer", "candidate", "verification-false", "unpublished", "legacy-file", "host-upgrade"]:
        outcome, problem = judge_report(mod, fixture(name))
        if outcome != "capable":
            fail("%s: judged %s (%s), want capable" % (name, outcome, problem))
    outcome, problem = judge_report(mod, fixture("malformed-record"))
    if outcome != "capable":
        fail("malformed-record: judged %s (%s); a record the command could not read is a well-formed answer" % (outcome, problem))
    rec = fixture("malformed-record")["guard"]
    if not rec.get("record_error") or rec.get("holder") != "":
        fail("malformed-record: the fixture is not the command's shape (empty holder beside record_error)")
    # The directory expectation: a directory's answers agree, a file's do not.
    for name in ["healthy", "unpublished"]:
        outcome, problem = judge_report(mod, fixture(name), expect="directory")
        if outcome != "capable":
            fail("%s under expect=directory: %s" % (name, problem))
    for name in ["none", "legacy-file", "host-upgrade"]:
        outcome, problem = judge_report(mod, fixture(name), expect="directory")
        if outcome != "refused" or "disagrees" not in problem:
            fail("%s under expect=directory: %s %s, want a disagreement" % (name, outcome, problem))


def corrupt(report, path, value, delete=False):
    out = copy.deepcopy(report)
    target = out
    for key in path[:-1]:
        target = target[key]
    if delete:
        del target[path[-1]]
    else:
        target[path[-1]] = value
    return out


def check_schema(mod):
    healthy = fixture("healthy")
    unknown = {"active": "unknown", "why": "cannot examine"}
    cases = [
        # (name, report, expected problem fragment)
        ("not an object: a list", [], "not a JSON object"),
        ("not an object: a string", "x", "not a JSON object"),
        ("no active", corrupt(healthy, ["active"], None, delete=True), "active is missing"),
        ("active a number", corrupt(healthy, ["active"], 1), "active is missing or not a string"),
        ("active an unfamiliar word", corrupt(healthy, ["active"], "held"), "not a word this role knows"),
        ("unknown without why", {"active": "unknown"}, "why is missing"),
        ("unknown with why null", {"active": "unknown", "why": None}, "why is not a string"),
        ("unknown with why a number", {"active": "unknown", "why": 1}, "why is not a string"),
        ("unknown with why empty", {"active": "unknown", "why": ""}, "why is missing or empty"),
        ("converge-guard without guard", corrupt(healthy, ["guard"], None, delete=True), "guard: missing"),
        ("guard a string", corrupt(healthy, ["guard"], "x"), "guard: missing or not an object"),
        ("record_error a number", corrupt(healthy, ["guard", "record_error"], 1), "record_error is not a non-empty string"),
        ("holder missing", corrupt(healthy, ["guard", "holder"], None, delete=True), "holder is missing"),
        ("holder a number", corrupt(healthy, ["guard", "holder"], 1), "holder is missing or not a string"),
        ("holder empty", corrupt(healthy, ["guard", "holder"], ""), "holder '' is not a name"),
        ("holder with a space", corrupt(healthy, ["guard", "holder"], "ci 1"), "is not a name"),
        ("holder with a tab", corrupt(healthy, ["guard", "holder"], "ci\t1"), "is not a name"),
        ("holder with a control character", corrupt(healthy, ["guard", "holder"], "ci\x011"), "is not a name"),
        ("holder of 300 bytes", corrupt(healthy, ["guard", "holder"], "c" * 300), "is not a name"),
        ("holder with a slash", corrupt(healthy, ["guard", "holder"], "ci/1"), "is not a name"),
        ("claimed_at not RFC 3339", corrupt(healthy, ["guard", "claimed_at"], "yesterday"), "claimed_at is not an RFC 3339 time"),
        ("hostname a number", corrupt(healthy, ["guard", "hostname"], 7), "hostname is missing or not a string"),
        ("recovery_pointer missing", corrupt(healthy, ["guard", "recovery_pointer"], None, delete=True), "recovery_pointer is missing"),
        ("recovery_pointer a string", corrupt(healthy, ["guard", "recovery_pointer"], "false"), "recovery_pointer is missing or not a boolean"),
        ("release_executable relative", corrupt(healthy, ["guard", "release_executable"], "billet"), "not an absolute path"),
        ("sha256 of 63 hex", corrupt(healthy, ["guard", "release_executable_sha256"], "a" * 63), "not 64 lowercase hex"),
        ("sha256 uppercase", corrupt(healthy, ["guard", "release_executable_sha256"], "A" * 64), "not 64 lowercase hex"),
        ("verified missing", corrupt(healthy, ["guard", "release_executable_verified"], None, delete=True), "release_executable_verified is missing"),
        ("verified the string unknown", corrupt(healthy, ["guard", "release_executable_verified"], "unknown"), "neither true, false nor an object"),
        ("verified an empty object", corrupt(healthy, ["guard", "release_executable_verified"], {}), "neither true, false nor an object"),
        ("verified unknown a number", corrupt(healthy, ["guard", "release_executable_verified"], {"unknown": 1}), "neither true, false nor an object"),
        ("verified unknown with more", corrupt(healthy, ["guard", "release_executable_verified"], {"unknown": "x", "more": 1}), "neither true, false nor an object"),
    ]
    for name, report, want in cases:
        outcome, problem = judge_report(mod, report)
        if outcome != "refused":
            fail("%s: judged %s, want refused" % (name, outcome))
        elif want not in problem:
            fail("%s: %r does not say %r" % (name, problem, want))
    # The unknown shape with a why is capable.
    outcome, problem = judge_report(mod, unknown)
    if outcome != "capable":
        fail("unknown with a why: %s %s" % (outcome, problem))


def check_execution(mod):
    """The executable's own shapes, through the real subprocess runner."""
    with tempfile.TemporaryDirectory() as tmp:
        def script(name, body):
            path = pathlib.Path(tmp, name)
            path.write_text("#!/bin/sh\n" + body)
            path.chmod(path.stat().st_mode | stat.S_IXUSR)
            return str(path)

        healthy = json.dumps(fixture("healthy"))
        cases = [
            ("a healthy answer", script("ok", "printf '%s' '" + healthy + "'\n"), "capable", ""),
            ("unknown command", script("pre-r", "echo 'unknown command \"converge-guard\"' >&2; exit 2\n"), "pre_r", ""),
            ("exit 1", script("fail", "echo 'cannot examine' >&2; exit 1\n"), "refused", "exited 1"),
            ("not JSON", script("garbage", "echo 'not json'\n"), "refused", "not JSON"),
            ("a hang past the bound", script("hang", "sleep 30\n"), "refused", "did not answer within the bound"),
        ]
        for name, exe, want_outcome, want_problem in cases:
            outcome, problem, _ = mod.classify(exe, 2, "any")
            if outcome != want_outcome or want_problem not in problem:
                fail("%s: %s (%s), want %s (%s)" % (name, outcome, problem, want_outcome, want_problem))


def main():
    mod = load_module()
    if not FIXTURES.is_dir():
        raise SystemExit("guard_status_check: %s is missing; run the Go test that writes the fixtures" % FIXTURES)
    check_healthy(mod)
    check_schema(mod)
    check_execution(mod)
    if failures:
        print("guard_status_check: %d failure(s)" % len(failures))
        return 1
    print("guard_status_check: the schema's cases, the fixtures and the executable's shapes pass")
    return 0


if __name__ == "__main__":
    sys.exit(main())
