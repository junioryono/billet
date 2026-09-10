#!/usr/bin/env python3
"""Prove the guard_fallback module admits exactly a trusted record and
candidate, and refuses one invalid property at a time in the phase that
judges it (fixtures-c4a.md, P13): the component × property table, an
otherwise-valid tree for every case, and the passing control.

Run as an ordinary account: the owner the module expects is this account's
uid, so a foreign owner is any other uid (which no chown as this account can
plant, and which the shell gate's sudo leg plants for real); everything else,
the types, the modes, the links, the sizes and the members, is planted here.
"""

import hashlib
import importlib.util
import json
import os
import pathlib
import shutil
import stat
import sys

# NO BYTECODE CACHE: a mutation run once left a __pycache__ compiled from a
# mutant beside a restored source of the same size and mtime, and Python
# executed the mutant while the traceback showed the restored lines.
sys.dont_write_bytecode = True
import tempfile
import types

HERE = pathlib.Path(__file__).resolve().parent
MODULE = HERE.parent / "plugins" / "modules" / "guard_fallback.py"

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
    spec = importlib.util.spec_from_file_location("guard_fallback", MODULE)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


CANDIDATE_BODY = b"#!/bin/sh\nexit 0\n"


class Tree:
    """A valid tree: parent/upgrades/{active/guard.json, recovery-X/billet.candidate}."""

    def __init__(self, base):
        self.parent = pathlib.Path(base) / "billet"
        self.root = self.parent / "upgrades"
        self.active = self.root / "active"
        self.recovery = self.root / "recovery-20260909T120000-0badcafe"
        self.candidate = self.recovery / "billet.candidate"
        self.record = self.active / "guard.json"
        for d in (self.parent, self.root, self.active, self.recovery):
            d.mkdir(mode=0o700)
            d.chmod(0o700)
        self.candidate.write_bytes(CANDIDATE_BODY)
        self.candidate.chmod(0o755)
        self.write_record(self.valid_record())

    def valid_record(self):
        return {
            "holder": "ci-1",
            "claimed_at": "2026-09-09T12:00:00Z",
            "hostname": "billet-control-01",
            "release_executable": str(self.candidate),
            "release_executable_sha256": hashlib.sha256(CANDIDATE_BODY).hexdigest(),
        }

    def write_record(self, record, raw=None):
        body = raw if raw is not None else (json.dumps(record, indent=2) + "\n").encode("utf-8")
        self.record.write_bytes(body)
        self.record.chmod(0o600)


def expect_refusal(mod, tree, owner, name, phase, words):
    try:
        mod.find(str(tree.root), owner)
    except mod.Refusal as exc:
        if exc.phase != phase:
            fail("%s: refused in phase %s (%s), want %s" % (name, exc.phase, exc, phase))
        elif words not in str(exc):
            fail("%s: %r does not say %r" % (name, str(exc), words))
        return
    fail("%s: admitted" % name)


def main():
    mod = load_module()
    owner = os.getuid()

    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        try:
            executable, digest = mod.find(str(tree.root), owner)
        except mod.Refusal as exc:
            raise SystemExit("guard_fallback_check: the valid tree was refused: %s" % exc)
        if executable != str(tree.candidate) or digest != hashlib.sha256(CANDIDATE_BODY).hexdigest():
            fail("the valid tree answered %r %r" % (executable, digest))
        # A foreign owner is refused at the parent, the first component judged.
        expect_refusal(mod, tree, owner + 1, "a foreign owner", "metadata", "owned by uid")

    # THE COMPONENT × PROPERTY TABLE, one invalid property per case over a
    # fresh valid tree, refused in the phase that judges it.
    def component_cases():
        for comp in ["parent", "root", "active", "recovery", "candidate"]:
            for prop in ["group-writable", "other-writable", "symlink", "wrong-type"]:
                yield comp, prop

    for comp, prop in component_cases():
        with tempfile.TemporaryDirectory() as base:
            tree = Tree(base)
            path = getattr(tree, comp)
            phase = "candidate" if comp in ("recovery", "candidate") else "metadata"
            if prop == "group-writable":
                path.chmod(path.stat().st_mode | stat.S_IWGRP)
                words = "writable by its group"
            elif prop == "other-writable":
                path.chmod(path.stat().st_mode | stat.S_IWOTH)
                words = "writable by others"
            elif prop == "symlink":
                real = pathlib.Path(base) / ("real-" + comp)
                if path.is_dir():
                    shutil.move(str(path), str(real))
                else:
                    real.write_bytes(path.read_bytes())
                    real.chmod(0o755)
                    path.unlink()
                path.symlink_to(real)
                words = "is a symlink"
            else:
                if path.is_dir():
                    shutil.rmtree(path)
                    path.write_bytes(b"x")
                    words = "is not a directory"
                else:
                    path.unlink()
                    path.mkdir(mode=0o700)
                    words = "is not a regular file"
            if comp == "candidate" and prop in ("symlink", "wrong-type", "group-writable", "other-writable"):
                # The record's own metadata and content pass; the candidate's judgement is what refuses.
                pass
            expect_refusal(mod, tree, owner, "%s %s" % (comp, prop), phase, words)

    # The record's own properties.
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.record.chmod(0o644)
        expect_refusal(mod, tree, owner, "record 0644", "metadata", "want 0600")
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.record.chmod(0o640)
        expect_refusal(mod, tree, owner, "record 0640", "metadata", "want 0600")
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.write_record(None, raw=(json.dumps(tree.valid_record()) + " " * 4097).encode("utf-8"))
        expect_refusal(mod, tree, owner, "record 4097 bytes", "metadata", "over the record's 4096")
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.record.unlink()
        os.mkfifo(str(tree.record), 0o600)
        expect_refusal(mod, tree, owner, "record a FIFO", "metadata", "not a regular file")
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.record.unlink()
        tree.record.mkdir(mode=0o700)
        expect_refusal(mod, tree, owner, "record a directory", "metadata", "not a regular file")

    # The record's content, judged after the read and before the candidate.
    content_cases = []
    valid = None
    with tempfile.TemporaryDirectory() as base:
        valid = Tree(base).valid_record()
    for name in mod.RECORD_MEMBERS:
        missing = dict(valid)
        del missing[name]
        content_cases.append(("%s missing" % name, missing, "%s is missing" % name))
        content_cases.append(("%s null" % name, dict(valid, **{name: None}), "%s is null" % name))
        content_cases.append(("%s a number" % name, dict(valid, **{name: 1}), "%s is not a string" % name))
        content_cases.append(("%s empty" % name, dict(valid, **{name: ""}), "%s is empty" % name))
    content_cases.append(("a sixth member", dict(valid, recovery_pointer=False), "recovery_pointer is not a member"))
    content_cases.append(("a 63-hex digest", dict(valid, release_executable_sha256="a" * 63), "not 64 lowercase hex"))
    content_cases.append(("an uppercase digest", dict(valid, release_executable_sha256="A" * 64), "not 64 lowercase hex"))
    content_cases.append(("a relative executable", dict(valid, release_executable="billet"), "not an absolute path"))
    for name, record, words in content_cases:
        with tempfile.TemporaryDirectory() as base:
            tree = Tree(base)
            # The valid record names THIS tree's candidate; re-point the case's copy.
            case = dict(record)
            if "release_executable" in case and case["release_executable"] == valid["release_executable"]:
                case["release_executable"] = str(tree.candidate)
            tree.write_record(case)
            expect_refusal(mod, tree, owner, name, "content", words)
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.write_record(None, raw=b"not json\n")
        expect_refusal(mod, tree, owner, "not JSON", "content", "is not JSON")
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.write_record(None, raw=b"[]\n")
        expect_refusal(mod, tree, owner, "a JSON list", "content", "not a JSON object")

    # The candidate's place and digest.
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        elsewhere = tree.root / "notes" / "billet.candidate"
        elsewhere.parent.mkdir(mode=0o700)
        elsewhere.write_bytes(CANDIDATE_BODY)
        tree.write_record(dict(tree.valid_record(), release_executable=str(elsewhere)))
        expect_refusal(mod, tree, owner, "a candidate outside recovery-*", "candidate", "is not inside a recovery-")
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.write_record(dict(tree.valid_record(), release_executable_sha256="0" * 64))
        expect_refusal(mod, tree, owner, "a digest one off", "candidate", "has digest")
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.write_record(dict(tree.valid_record(), release_executable=str(tree.recovery) + "/../recovery-20260909T120000-0badcafe/billet.candidate"))
        expect_refusal(mod, tree, owner, "a dot segment in the candidate", "candidate", "not a normalised path")

    # THE ANCESTORS ABOVE THE ROOT'S PARENT are judged as the Go boundary judges
    # them: a writable one refuses unless sticky, a foreign-owned one refuses, a
    # link on the way is admitted when the owner made it.
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        os.chmod(base, 0o777)
        expect_refusal(mod, tree, owner, "an other-writable ancestor", "metadata", "writable by group or others without the sticky bit")
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        os.chmod(base, 0o1777)
        try:
            mod.find(str(tree.root), owner)
        except mod.Refusal as exc:
            fail("a sticky world-writable ancestor was refused: %s" % exc)
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        link = pathlib.Path(base) / "via"
        link.symlink_to(base)
        # The root is named through the link, and so is the candidate the
        # record names: the recorded path must lie inside the root as spelled.
        via_root = link / "billet" / "upgrades"
        tree.write_record(dict(tree.valid_record(),
                               release_executable=str(via_root / tree.recovery.name / "billet.candidate")))
        try:
            mod.find(str(via_root), owner)
        except mod.Refusal as exc:
            fail("a link the owner made on the way was refused: %s" % exc)
        # Its ancestors are examined through the link, not the link's name.
        os.chmod(base, 0o777)
        try:
            mod.find(str(via_root), owner)
        except mod.Refusal as exc:
            if "writable by group or others" not in str(exc):
                fail("the writable ancestor behind a link refused for the wrong reason: %s" % exc)
        else:
            fail("a writable ancestor behind a link was admitted")

    # THE FILESYSTEM ROOT ITSELF is judged, through a stat the check makes
    # answer as a world-writable `/` without the sticky bit.
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        real_lstat = mod.os.lstat

        def writable_root(path, *args, **kwargs):
            st = real_lstat(path, *args, **kwargs)
            if path == os.sep:
                fields = list(st)
                fields[0] = stat.S_IFDIR | 0o777
                return os.stat_result(fields)
            return st

        mod.os.lstat = writable_root
        try:
            expect_refusal(mod, tree, owner, "a writable filesystem root", "metadata", "/ is mode 0777")
        finally:
            mod.os.lstat = real_lstat

    # THE ANCESTORS ALONE, as the preparation asks before it executes anything
    # under the root: a fresh host's absent parent is admitted, an unsafe
    # ancestor refused, an unsafe existing root refused.
    with tempfile.TemporaryDirectory() as base:
        try:
            mod.judge_ancestors(os.path.join(base, "billet", "upgrades"), owner)
        except mod.Refusal as exc:
            fail("a fresh host's absent parent was refused: %s" % exc)
        os.chmod(base, 0o777)
        try:
            mod.judge_ancestors(os.path.join(base, "billet", "upgrades"), owner)
        except mod.Refusal as exc:
            if exc.phase != "ancestors" or "writable by group or others" not in str(exc):
                fail("the unsafe ancestor refused for the wrong reason: %s" % exc)
        else:
            fail("an unsafe ancestor was admitted by the ancestors judgement")
    with tempfile.TemporaryDirectory() as base:
        # An absent ancestor above the parent ends the walk: nothing under it
        # exists to be renamed, and the caller's own stat answers for what it
        # asked about.
        try:
            mod.judge_ancestors(os.path.join(base, "missing", "billet", "upgrades"), owner)
        except mod.Refusal as exc:
            fail("an absent ancestor was refused: %s" % exc)
    with tempfile.TemporaryDirectory() as base:
        # An absent ancestor under a directory other accounts can write is
        # refused even under the sticky bit, which reserves no absent name.
        os.chmod(base, 0o1777)
        try:
            mod.judge_ancestors(os.path.join(base, "missing", "billet", "upgrades"), owner)
        except mod.Refusal as exc:
            if "is absent under" not in str(exc) or exc.phase != "ancestors":
                fail("the absent ancestor under a sticky directory refused for the wrong reason: %s" % exc)
        else:
            fail("an absent ancestor under a sticky world-writable directory was admitted")
        try:
            mod.judge_ancestors(os.path.join(base, "billet", "upgrades"), owner)
        except mod.Refusal as exc:
            if "is absent under" not in str(exc):
                fail("the absent parent under a sticky directory refused for the wrong reason: %s" % exc)
        else:
            fail("an absent parent under a sticky world-writable directory was admitted")
        os.chmod(base, 0o700)
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        tree.root.chmod(0o770)
        try:
            mod.judge_ancestors(str(tree.root), owner)
        except mod.Refusal as exc:
            if "writable by its group" not in str(exc):
                fail("the group-writable root refused for the wrong reason: %s" % exc)
        else:
            fail("a group-writable root was admitted by the ancestors judgement")

    # HARD LINKS ARE ADMITTED: the command imposes no one-link rule.
    with tempfile.TemporaryDirectory() as base:
        tree = Tree(base)
        os.link(str(tree.candidate), str(tree.recovery / "second-name"))
        os.link(str(tree.record), str(tree.active / "record-second-name"))
        try:
            mod.find(str(tree.root), owner)
        except mod.Refusal as exc:
            fail("a hard-linked candidate and record were refused: %s" % exc)

    if failures:
        print("guard_fallback_check: %d failure(s)" % len(failures))
        return 1
    print("guard_fallback_check: the valid tree, every component and property, the record's content and the candidate pass")
    return 0


if __name__ == "__main__":
    sys.exit(main())
