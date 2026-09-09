#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Resolve which on-disk unit names systemd would treat as aliases of billet's.

WHY A MODULE AND NOT A FIND. `systemctl show --property=Names` is the alias set
the manager loaded; an alias created on disk after that load is not in it, and
a drop-in under `<alias>.d` is not in `DropInPaths` either, while the next
daemon-reload adopts both. A dry run that compares the loaded state with the
fragment therefore has to know what a reload WOULD find, and that is a question
about unit names, not about filesystem paths: systemd looks a name up through
its lookup paths in order, takes the first directory that holds it, and reads a
symlink's target as another unit name when it reduces to one. A `realpath` of
the link answers a different question (a path can leave the lookup paths and
come back), and a `find` of links cannot see that a lower-priority symlink is
SHADOWED by a higher-priority fragment of the same name, which systemd never
loads.

THE ALGORITHM, in two steps, as systemd resolves. First the manager's effective
SOURCE-NAME MAP is built in `unit_path` order: for every entry named
`*.service` in any lookup directory, the FIRST directory to hold that name
wins, and the entry is CLASSIFIED as a `fragment` (a regular file), a `mask` (a
symlink to /dev/null: terminal, and never an alias), an `external` link (a
symlink whose target lies outside every lookup path: a linked unit, not an
alias), or an `alias` (a symlink whose target, read as given and a relative one
interpreted from its own directory, reduces to a unit name inside a lookup
path). Then alias chains are followed through that map to a fixed point: a
dangling target is still an alias when its name has an entry in the map, a
target with no entry is dangling and refused, and a cycle is refused. Every
alias whose chain reaches one of the billet unit names is reported, with the
drop-in directories (`<alias>.d`) a reload would search for it.

A read that fails (an unreadable directory, an unreadable link) is could-not-
tell and refuses, never an empty answer: the one failure this module exists to
prevent is a dry run that continued because it did not see something.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

DOCUMENTATION = r"""
---
module: unit_names
short_description: Resolve on-disk aliases of billet's systemd units the way the manager would
description:
  - Builds the running manager's effective source-name map over the lookup paths
    it reports (C(Manager.UnitPath), in order), classifies every C(*.service)
    entry as a fragment, a mask, an external link or an alias, follows alias
    chains by unit name, and reports every alias that reaches one of the given
    units.
  - Fails on a lookup directory or link it could not read, on a dangling alias
    target that no lookup directory holds, and on a cycle, because a dry run
    must not continue past a question it could not answer.
options:
  unit_path:
    description: The manager's unit lookup paths, in the manager's own order.
    type: list
    elements: path
    required: true
  units:
    description: The unit names whose aliases are wanted.
    type: list
    elements: str
    required: true
author:
  - junioryono
"""

EXAMPLES = r"""
- name: Resolve on-disk aliases of the billet units
  junioryono.billet.unit_names:
    unit_path: "{{ (unit_path_property.stdout | from_json).data }}"
    units:
      - billet-server.service
      - billet-node.service
  register: billet_aliases
"""

RETURN = r"""
aliases:
  description: Every alias whose chain reaches one of the given units.
  returned: success
  type: list
  elements: dict
  contains:
    name:
      description: The alias's unit name.
      type: str
    path:
      description: The winning entry's path.
      type: str
    target:
      description: The unit name the chain reaches.
      type: str
    chain:
      description: The unit names visited, alias first.
      type: list
    dropin_dirs:
      description: The C(<alias>.d) directories a reload would search for it, in lookup order.
      type: list
entries:
  description: The effective source-name map for the given units and their aliases.
  returned: success
  type: dict
scanned:
  description: How many C(*.service) entries were classified.
  returned: success
  type: int
"""

import errno
import os
import stat

from ansible.module_utils.basic import AnsibleModule


class Unreadable(Exception):
    """A lookup directory or a link that could not be read; never an answer."""


def _entries_in(directory):
    try:
        names = os.listdir(directory)
    except OSError as exc:
        if exc.errno in (errno.ENOENT, errno.ENOTDIR):
            return []
        raise Unreadable("%s: %s" % (directory, exc.strerror))
    return [n for n in names if n.endswith(".service")]


def _classify(path, unit_path):
    """One entry's class as systemd reads it, from its own lstat.

    unit_path holds the lookup directories in their original AND resolved
    spellings, and a target is chased before the comparison, because systemd
    chases the link before deciding whether it stays inside the search path: on a
    merged-usr host /lib/systemd/system is a symlink to /usr/lib/systemd/system,
    and a target spelled through the former is an alias, not a linked unit.
    """
    try:
        st = os.lstat(path)
    except OSError as exc:
        raise Unreadable("%s: %s" % (path, exc.strerror))
    if not stat.S_ISLNK(st.st_mode):
        if stat.S_ISREG(st.st_mode):
            return {"kind": "fragment", "path": path}
        return {"kind": "other", "path": path, "mode": oct(stat.S_IFMT(st.st_mode))}
    try:
        target = os.readlink(path)
    except OSError as exc:
        raise Unreadable("%s: %s" % (path, exc.strerror))
    # A mask is a symlink to /dev/null, by target text, as systemd tests it.
    if os.path.normpath(target) == "/dev/null":
        return {"kind": "mask", "path": path, "target": target}
    # An absolute target is read as written; a relative one is interpreted
    # from the link's own directory. Either names a unit only when the
    # directory it reduces to is a lookup path.
    raw = target if os.path.isabs(target) else os.path.join(os.path.dirname(path), target)
    # systemd identifies components skipping `.` and redundant separators, so a
    # target spelled `<unit>/` or `<unit>/.` would make the unit-name component
    # an intermediate one to follow through; that spelling is refused rather
    # than read through the symlink the unit name may be.
    if raw.endswith("/") or raw.endswith("/.") or "/./" in raw:
        raise Unreadable("%s: target %s ends in a separator or a `.` component, which this classifier does not read through" % (path, target))
    # systemd refuses an unsafe remainder past a component that does not exist
    # (a `..` there has nothing to apply to) rather than normalising it away, so
    # the check runs on the target AS WRITTEN, before normpath folds it.
    cur, missing = "/", False
    for component in [c for c in raw.split("/") if c not in ("", ".")]:
        if missing and component == "..":
            raise Unreadable("%s: target %s climbs through a component that does not exist" % (path, target))
        cur = os.path.join(cur, component)
        if not missing and not os.path.lexists(cur):
            missing = True
    resolved = os.path.normpath(raw)
    # AS SYSTEMD CLASSIFIES IT (CHASE_NOFOLLOW | CHASE_NONEXISTENT): the
    # target's DIRECTORY is chased (realpath tolerates components that do not
    # exist, as systemd's chase does) and its FINAL COMPONENT is kept as
    # written, so a target that is itself a symlink is the unit NAME it spells
    # and not what that link resolves to; the lookup paths are kept in both
    # their original and resolved spellings, and the target is an alias when
    # it lies UNDER any of them, nested or not, with the basename as the unit
    # name. Equality of the immediate directory would call a nested or a
    # not-yet-existing target external, and a full chase would follow a
    # shadowed lower-priority symlink out of the search path, missing the
    # drop-ins searched under the alias's name either way.
    chased = os.path.join(os.path.realpath(os.path.dirname(resolved)), os.path.basename(resolved))
    if not any(chased.startswith(p.rstrip("/") + "/") for p in unit_path):
        return {"kind": "external", "path": path, "target": target}
    return {"kind": "alias", "path": path, "target": target,
            "target_name": os.path.basename(chased)}


def source_name_map(unit_path):
    """The first lookup directory to hold each name wins; the rest are shadowed."""
    entries = {}
    scanned = 0
    lookup = list(unit_path)
    for p in unit_path:
        resolved = os.path.realpath(p)
        if resolved not in lookup:
            lookup.append(resolved)
    for directory in unit_path:
        for name in _entries_in(directory):
            scanned += 1
            if name in entries:
                continue
            entries[name] = _classify(os.path.join(directory, name), lookup)
    return entries, scanned


def follow(name, entries):
    """The unit name an alias chain ends at, or why it cannot be followed."""
    chain = [name]
    seen = {name}
    current = name
    while True:
        entry = entries.get(current)
        if entry is None:
            return None, chain, "dangling: no lookup directory holds %s" % current
        if entry["kind"] != "alias":
            return current, chain, None
        nxt = entry["target_name"]
        chain.append(nxt)
        if nxt in seen:
            return None, chain, "cycle through %s" % " -> ".join(chain)
        seen.add(nxt)
        current = nxt


def resolve(unit_path, units):
    entries, scanned = source_name_map(unit_path)
    aliases = []
    problems = []
    for name in sorted(entries):
        entry = entries[name]
        if entry["kind"] != "alias" or name in units:
            continue
        end, chain, why = follow(name, entries)
        if why is not None:
            problems.append("%s (%s): %s" % (name, entry["path"], why))
            continue
        if end in units:
            aliases.append({
                "name": name,
                "path": entry["path"],
                "target": end,
                "chain": chain,
                "dropin_dirs": [os.path.join(d, name + ".d") for d in unit_path],
            })
    wanted = set(units) | set(a["name"] for a in aliases)
    return {
        "aliases": aliases,
        "entries": dict((k, v) for k, v in entries.items() if k in wanted),
        "scanned": scanned,
    }, problems


def main():
    module = AnsibleModule(
        argument_spec=dict(
            unit_path=dict(type="list", elements="path", required=True),
            units=dict(type="list", elements="str", required=True),
        ),
        supports_check_mode=True,
    )
    unit_path = [os.path.normpath(p) for p in module.params["unit_path"]]
    units = module.params["units"]
    if not unit_path:
        module.fail_json(msg="unit_path is empty: the manager's lookup paths could not be read")
    try:
        result, problems = resolve(unit_path, units)
    except Unreadable as exc:
        module.fail_json(msg="could not read the unit lookup paths: %s" % exc)
        return
    if problems:
        module.fail_json(
            msg="an alias chain could not be followed to a unit: %s" % "; ".join(problems),
            **result
        )
        return
    module.exit_json(changed=False, **result)


if __name__ == "__main__":
    main()
