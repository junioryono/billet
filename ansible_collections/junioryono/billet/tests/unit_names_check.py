#!/usr/bin/env python3
"""Prove the unit_names module resolves aliases the way systemd would.

Each fixture is a directory tree standing in for the manager's lookup paths, in
the order the manager reports them. The cases are the ones a `find` of symlinks
gets wrong: a lower-priority alias SHADOWED by a higher-priority fragment of the
same name is never loaded; a mask is terminal and never an alias; a link out of
every lookup path is a linked unit and not an alias; a chain that crosses
directories is followed by unit name; a dangling target and a cycle refuse
rather than pass in silence.

A VACUOUS PASS IS A FAILURE: the fixture that must report an alias is asserted
to report exactly it, so a resolver that stopped seeing symlinks fails here.
"""

import importlib.util
import os
import pathlib
import sys
import tempfile
import types

HERE = pathlib.Path(__file__).resolve().parent
MODULE = HERE.parent / "plugins" / "modules" / "unit_names.py"


def load_module():
    # The module imports AnsibleModule at load; the resolver under test never
    # touches it, so a stand-in keeps this check runnable on an interpreter
    # without ansible-core installed.
    if "ansible.module_utils.basic" not in sys.modules:
        ansible = types.ModuleType("ansible")
        module_utils = types.ModuleType("ansible.module_utils")
        basic = types.ModuleType("ansible.module_utils.basic")
        basic.AnsibleModule = object
        sys.modules.setdefault("ansible", ansible)
        sys.modules.setdefault("ansible.module_utils", module_utils)
        sys.modules["ansible.module_utils.basic"] = basic
    spec = importlib.util.spec_from_file_location("unit_names", MODULE)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


BILLET = ["billet-server.service", "billet-node.service"]


class Tree:
    """Three lookup directories in priority order: etc, run, lib."""

    def __init__(self, root):
        self.root = pathlib.Path(root)
        self.etc = self.root / "etc/systemd/system"
        self.run = self.root / "run/systemd/system"
        self.lib = self.root / "usr/lib/systemd/system"
        for d in (self.etc, self.run, self.lib):
            d.mkdir(parents=True)
        self.unit_path = [str(self.etc), str(self.run), str(self.lib)]

    def fragment(self, directory, name):
        (directory / name).write_text("[Service]\nExecStart=/bin/true\n")

    def link(self, directory, name, target):
        os.symlink(target, directory / name)


def case(name):
    def wrap(fn):
        fn.case_name = name
        return fn
    return wrap


@case("an alias in /etc pointing at a billet unit is reported with its drop-in directories")
def plain_alias(mod, t):
    t.fragment(t.lib, "billet-server.service")
    t.link(t.etc, "runner.service", "billet-server.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert [a["name"] for a in result["aliases"]] == ["runner.service"], result
    alias = result["aliases"][0]
    assert alias["target"] == "billet-server.service"
    assert alias["chain"] == ["runner.service", "billet-server.service"]
    assert alias["dropin_dirs"] == [str(pathlib.Path(d) / "runner.service.d") for d in t.unit_path]


@case("an alias in a lower directory shadowed by a fragment in a higher one is not loaded and passes")
def shadowed_alias(mod, t):
    t.fragment(t.lib, "billet-server.service")
    t.fragment(t.etc, "runner.service")
    t.link(t.lib, "runner.service", "billet-server.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert result["aliases"] == [], result
    assert result["scanned"] == 3, result


@case("an alias in a higher directory shadows a fragment below it and is reported")
def alias_over_fragment(mod, t):
    t.fragment(t.lib, "billet-node.service")
    t.fragment(t.lib, "runner.service")
    t.link(t.etc, "runner.service", "billet-node.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert [a["name"] for a in result["aliases"]] == ["runner.service"], result


@case("a mask of an unrelated unit is terminal and passes")
def unrelated_mask(mod, t):
    t.fragment(t.lib, "billet-server.service")
    t.link(t.etc, "snapd.service", "/dev/null")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert result["aliases"] == [], result


@case("an alias whose chain ends at a mask reaches no billet unit and passes")
def alias_to_mask(mod, t):
    t.fragment(t.lib, "billet-server.service")
    t.link(t.etc, "other.service", "/dev/null")
    t.link(t.run, "runner.service", "other.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert result["aliases"] == [], result


@case("a link out of every lookup path is a linked unit and passes, even when it names billet")
def external_link(mod, t):
    t.fragment(t.lib, "billet-server.service")
    outside = t.root / "opt/vendor"
    outside.mkdir(parents=True)
    (outside / "billet-server.service").write_text("[Service]\n")
    t.link(t.etc, "vendor.service", str(outside / "billet-server.service"))
    t.link(t.run, "relative.service", "../../../opt/vendor/billet-server.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert result["aliases"] == [], result


@case("a chain across directories, with an absolute link into another lookup path, is followed by unit name")
def cross_directory_chain(mod, t):
    t.fragment(t.etc, "billet-server.service")
    t.link(t.lib, "b.service", str(t.etc / "billet-server.service"))
    t.link(t.etc, "a.service", "b.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    names = sorted(a["name"] for a in result["aliases"])
    assert names == ["a.service", "b.service"], result
    a = [x for x in result["aliases"] if x["name"] == "a.service"][0]
    assert a["chain"] == ["a.service", "b.service", "billet-server.service"], a


@case("a relative target with a directory component is resolved from the link's own directory")
def relative_with_dots(mod, t):
    t.fragment(t.lib, "billet-node.service")
    t.link(t.etc, "runner.service", "../../../usr/lib/systemd/system/billet-node.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert [a["name"] for a in result["aliases"]] == ["runner.service"], result


@case("a dangling alias whose target no lookup directory holds refuses")
def dangling(mod, t):
    t.fragment(t.lib, "billet-server.service")
    t.link(t.etc, "runner.service", "missing.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert len(problems) == 1 and "dangling" in problems[0], problems
    assert result["aliases"] == [], result


@case("a dangling link whose target name has an entry elsewhere is still an alias and is followed")
def dangling_link_with_entry(mod, t):
    # /run holds no billet-server.service, so the link is dangling as a path;
    # the NAME has an entry in /usr/lib, which is what systemd resolves.
    t.fragment(t.lib, "billet-server.service")
    t.link(t.etc, "runner.service", str(t.run / "billet-server.service"))
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert [a["name"] for a in result["aliases"]] == ["runner.service"], result


@case("a cycle refuses")
def cycle(mod, t):
    t.fragment(t.lib, "billet-server.service")
    t.link(t.etc, "a.service", "b.service")
    t.link(t.etc, "b.service", "a.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert len(problems) == 2 and all("cycle" in p for p in problems), problems


@case("a lookup path that does not exist is empty, not an error")
def missing_directory(mod, t):
    t.fragment(t.lib, "billet-server.service")
    result, problems = mod.resolve(t.unit_path + [str(t.root / "nope")], BILLET)
    assert problems == [], problems
    assert result["aliases"] == [], result


@case("a lookup directory that cannot be read refuses rather than answering empty")
def unreadable_directory(mod, t):
    if os.geteuid() == 0:
        return
    t.fragment(t.lib, "billet-server.service")
    locked = t.root / "locked"
    locked.mkdir()
    locked.chmod(0)
    try:
        try:
            mod.resolve([str(locked)] + t.unit_path, BILLET)
        except mod.Unreadable:
            return
        raise AssertionError("an unreadable directory answered")
    finally:
        locked.chmod(0o700)


@case("a target spelled through a symlinked lookup directory is an alias, as systemd chases it")
def symlinked_lookup_dir(mod, t):
    # /lib -> usr/lib, as on a merged-usr host; the manager lists the resolved
    # directory and a link spells its target through the other name.
    t.fragment(t.lib, "billet-server.service")
    os.symlink("usr/lib", t.root / "lib")
    t.link(t.etc, "runner.service", str(t.root / "lib/systemd/system/billet-server.service"))
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert [a["name"] for a in result["aliases"]] == ["runner.service"], result


@case("a target nested under a lookup directory is an alias to its basename, as systemd classifies it")
def nested_target(mod, t):
    t.fragment(t.lib, "billet-server.service")
    (t.lib / "subdir").mkdir()
    (t.lib / "subdir" / "billet-server.service").write_text("[Service]\n")
    t.link(t.etc, "runner.service", str(t.lib / "subdir" / "billet-server.service"))
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert [a["name"] for a in result["aliases"]] == ["runner.service"], result


@case("a target whose directory does not exist yet is chased as far as it goes and is still an alias")
def missing_components(mod, t):
    t.fragment(t.lib, "billet-server.service")
    t.link(t.etc, "runner.service", str(t.lib / "missing" / "billet-server.service"))
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert [a["name"] for a in result["aliases"]] == ["runner.service"], result


@case("a target that is itself a shadowed symlink out of the search path is the unit name it spells, not what it resolves to")
def final_component_kept(mod, t):
    # /etc holds the managed fragment; /usr/lib holds a lower-priority symlink
    # of the same name pointing out of every lookup path; other.service spells
    # its target through that lower-priority name. systemd keeps the final
    # component and classifies other.service an alias of billet-server.service;
    # a full chase would follow the shadowed link to /opt and call it external.
    t.fragment(t.etc, "billet-server.service")
    outside = t.root / "opt/vendor"
    outside.mkdir(parents=True)
    (outside / "server.service").write_text("[Service]\n")
    t.link(t.lib, "billet-server.service", str(outside / "server.service"))
    t.link(t.etc, "other.service", str(t.lib / "billet-server.service"))
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert [a["name"] for a in result["aliases"]] == ["other.service"], result
    assert result["aliases"][0]["target"] == "billet-server.service", result


@case("a target that climbs with .. through a component that does not exist refuses, as systemd's chase does")
def unsafe_remainder(mod, t):
    t.fragment(t.lib, "billet-server.service")
    # normpath would fold "missing/../billet-server.service" into the lookup
    # directory and call this an alias; systemd refuses the remainder.
    os.symlink(str(t.lib) + "/missing/../billet-server.service", t.etc / "runner.service")
    try:
        mod.resolve(t.unit_path, BILLET)
    except mod.Unreadable as exc:
        assert "climbs" in str(exc), exc
        return
    raise AssertionError("an unsafe remainder past a missing component was resolved")


@case("a target spelled with a trailing slash or a dot component refuses rather than reading through its unit-name component")
def trailing_component_spellings(mod, t):
    # The final unit-name component is itself a lower-priority symlink out of
    # the search path; spelled with a trailing "/." it would be followed as an
    # intermediate component and the alias relationship lost.
    t.fragment(t.etc, "billet-server.service")
    outside = t.root / "opt/vendor"
    outside.mkdir(parents=True)
    (outside / "server.service").write_text("[Service]\n")
    t.link(t.lib, "billet-server.service", str(outside / "server.service"))
    for suffix in ("/.", "/"):
        t.link(t.etc, "other%s.service" % suffix.replace("/", "s").replace(".", "d"),
               str(t.lib / "billet-server.service") + suffix)
    try:
        mod.resolve(t.unit_path, BILLET)
    except mod.Unreadable as exc:
        assert "separator" in str(exc), exc
        return
    raise AssertionError("a trailing separator or dot component was read through")


@case("the billet unit itself being a symlink is not an alias of itself")
def billet_is_link(mod, t):
    t.fragment(t.lib, "real.service")
    t.link(t.etc, "billet-server.service", "real.service")
    result, problems = mod.resolve(t.unit_path, BILLET)
    assert problems == [], problems
    assert result["aliases"] == [], result
    assert result["entries"]["billet-server.service"]["kind"] == "alias"


def main():
    mod = load_module()
    cases = [v for v in globals().values() if callable(v) and hasattr(v, "case_name")]
    assert len(cases) >= 12, len(cases)
    for fn in cases:
        with tempfile.TemporaryDirectory() as root:
            fn(mod, Tree(root))
        print("ok  ", fn.case_name)
    print("%d cases" % len(cases))


if __name__ == "__main__":
    main()
