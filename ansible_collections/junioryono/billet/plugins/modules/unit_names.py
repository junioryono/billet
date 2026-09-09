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
    # An absolute target is read as written; a relative one is interpreted
    # from the link's own directory. Either names a unit only when the
    # directory it reduces to is a lookup path.
    raw = target if os.path.isabs(target) else os.path.join(os.path.dirname(path), target)
    # systemd consumes a trailing separator or `.` component when it identifies
    # components and keeps the final symlink; this classifier does not model
    # that and refuses such a spelling as could-not-tell, a restricted admission
    # of its own that no shipped policy needs.
    if raw.endswith("/") or raw.endswith("/."):
        raise Unreadable("%s: target %s ends in a separator or a `.` component, which this classifier does not read through" % (path, target))
    # THE TARGET IS CHASED COMPONENT BY COMPONENT, as systemd's chase is: an
    # intermediate symlink is resolved when it is reached, `..` then applies to
    # the RESOLVED parent (normpath first would fold `link/../x` into the link's
    # own directory and lose an alias that lives where the link points), a
    # component that does not exist is tolerated and everything after it is
    # kept as written, a `..` after one refuses (systemd refuses that unsafe
    # remainder rather than normalising it away), and the FINAL component is
    # kept as written: the unit name the target spells.
    resolved = _chase(raw, path, target)
    # AS SYSTEMD CLASSIFIES IT (CHASE_NOFOLLOW | CHASE_NONEXISTENT): the chased
    # path above keeps its final component as written, so a target that is
    # itself a symlink is the unit NAME it spells and not what that link
    # resolves to; the lookup paths are kept in both their original and
    # resolved spellings, and the target is an alias when it lies UNDER any of
    # them, nested or not, with the basename as the unit name. Equality of the
    # immediate directory would call a nested or a not-yet-existing target
    # external, and a full chase would follow a shadowed lower-priority symlink
    # out of the search path, missing the drop-ins searched under the alias's
    # name either way.
    chased = resolved
    # A MASK IS DECIDED AFTER THE CHASE, never from the target's text: systemd
    # chases every symlink first and skips one whose chase fails, so a spelling
    # such as /missing/../dev/null is not a mask that shadows a lower alias but
    # an entry systemd never maps (the chase above refuses it), and only a
    # target that chases to /dev/null itself is the mask.
    if chased == "/dev/null":
        return {"kind": "mask", "path": path, "target": target}
    if not any(chased.startswith(p.rstrip("/") + "/") for p in unit_path):
        return {"kind": "external", "path": path, "target": target}
    target_name = os.path.basename(chased)
    _validate_alias(path, target_name)
    return {"kind": "alias", "path": path, "target": target,
            "target_name": target_name}


_NAME_CHARS = frozenset("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789:_.\\-@")


def _name_parts(name):
    """(prefix, instance) of a .service unit name, or None when it is not one.

    instance is None for a plain name, "" for a template (x@.service) and the
    text after the @ for an instance (x@y.service).
    """
    if not name.endswith(".service") or len(name) > 255 or not set(name) <= _NAME_CHARS:
        return None
    stem = name[:-len(".service")]
    if not stem or stem[0] == "@":
        return None
    if "@" not in stem:
        return stem, None
    prefix, instance = stem.split("@", 1)
    if "@" in instance:
        return None
    return prefix, instance


def _validate_alias(path, target_name):
    """Refuse an alias systemd's name map would not accept.

    systemd (unit_validate_alias_symlink_or_warn, v255) skips a symlink inside
    the search path whose names do not fit: a self-alias, a target that is not a
    unit name, a target of another unit type, a name type that differs (a plain
    name may alias a plain name, a template a template, an instance an
    instance of the same instance name or a template), and it then loads the
    next lower entry of that name. This classifier refuses such a link as
    could-not-tell rather than deciding which entry then wins, because an
    accepted-but-invalid alias would claim the name and hide that lower entry.
    """
    src = os.path.basename(path)
    if src == target_name:
        raise Unreadable("%s: a self-alias, which systemd ignores; the next lower entry of that name would be loaded" % path)
    sp = _name_parts(src)
    tp = _name_parts(target_name)
    if sp is None or tp is None:
        raise Unreadable("%s: alias target %s is not a .service unit name systemd would accept for this link" % (path, target_name))
    src_kind = "plain" if sp[1] is None else ("template" if sp[1] == "" else "instance")
    dst_kind = "plain" if tp[1] is None else ("template" if tp[1] == "" else "instance")
    if not (src_kind == dst_kind or (src_kind == "instance" and dst_kind == "template")):
        raise Unreadable("%s: alias target %s is a %s name while the link is a %s name, which systemd rejects; the next lower entry of that name would be loaded" % (path, target_name, dst_kind, src_kind))
    if dst_kind == "instance" and sp[1] != tp[1]:
        raise Unreadable("%s: alias target %s names another instance than the link, which systemd rejects" % (path, target_name))


def _chase(raw, path, target):
    """The target as systemd chases it (CHASE_NOFOLLOW | CHASE_NONEXISTENT).

    Component by component, over ONE QUEUE: an intermediate symlink met on the
    way is read and its own components are put at the front of the queue (an
    absolute one restarting at the root, a relative one continuing from the
    link's directory), so what lies inside a link's target is walked by the same
    rules as what was written, missing components and all; `..` applies to the
    resolved parent reached so far and is itself examined there, since systemd
    opens it relative to the directory it holds and a directory that cannot be
    searched fails the climb; a component that does not exist is tolerated
    and everything after it is kept as written; a `..` after one refuses, as
    systemd refuses that unsafe remainder rather than normalising it away; a
    read that fails for any reason but absence refuses, the final component's
    included (systemd opens it O_PATH|O_NOFOLLOW and keeps every error but
    ENOENT); an existing intermediate component must be a directory, since
    systemd cannot walk through a file and a lexical `..` past one would; the
    thirty-second symlink refuses, as systemd 255's CHASE_MAX of 32 does; and
    the FINAL component is kept as written, the unit name the target spells,
    whatever it is a link to. lexists and realpath would collapse an error into
    "absent" and fold a missing component inside a link's target away, which is
    why neither is used here.
    """
    queue = [c for c in raw.split("/") if c not in ("", ".")]
    cur, missing, links = "/", False, 0
    while queue:
        component = queue.pop(0)
        last = not queue
        if component == "..":
            if missing:
                raise Unreadable("%s: target %s climbs through a component that does not exist" % (path, target))
            # systemd opens `..` relative to the directory it holds, so a
            # directory it may not search fails the climb; a lexical dirname
            # would climb out of it without looking.
            up = os.path.join(cur, "..")
            try:
                st = os.lstat(up)
            except OSError as exc:
                raise Unreadable("%s: target %s: %s: %s" % (path, target, up, exc.strerror))
            if not stat.S_ISDIR(st.st_mode):
                raise Unreadable("%s: target %s: %s is not a directory" % (path, target, up))
            cur = os.path.dirname(cur) if cur != "/" else "/"
            continue
        nxt = os.path.join(cur, component)
        if missing:
            cur = nxt
            continue
        try:
            st = os.lstat(nxt)
        except OSError as exc:
            if exc.errno == errno.ENOENT:
                missing = True
                cur = nxt
                continue
            raise Unreadable("%s: target %s: %s: %s" % (path, target, nxt, exc.strerror))
        if last:
            return nxt
        if stat.S_ISLNK(st.st_mode):
            links += 1
            if links >= 32:
                raise Unreadable("%s: target %s: the thirty-second symlink on the way, where systemd stops" % (path, target))
            try:
                link = os.readlink(nxt)
            except OSError as exc:
                raise Unreadable("%s: target %s: %s: %s" % (path, target, nxt, exc.strerror))
            if os.path.isabs(link):
                cur = "/"
            queue = [c for c in link.split("/") if c not in ("", ".")] + queue
            continue
        if not stat.S_ISDIR(st.st_mode):
            raise Unreadable("%s: target %s: %s is not a directory" % (path, target, nxt))
        cur = nxt
    return cur


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
            entry = _classify(os.path.join(directory, name), lookup)
            # systemd builds its name map from regular files and symlinks only:
            # a directory or a device named like a unit claims no name, so a
            # lower-priority alias of that name is the one it loads.
            if entry["kind"] == "other":
                continue
            entries[name] = entry
    return entries, scanned


FOLLOW_MAX = 8


def follow(name, entries):
    """The unit name an alias chain ends at, or why it cannot be followed.

    As systemd's unit_ids_map_get (v255): a target that is an instance name
    with no entry falls back to its template's entry (other@x.service ->
    actual@x.service resolves through actual@.service), and at most eight hops
    are followed, a longer chain failing to load.
    """
    chain = [name]
    seen = {name}
    current = name
    hops = 0
    while True:
        entry = entries.get(current)
        if entry is None and hops > 0:
            parts = _name_parts(current)
            if parts is not None and parts[1]:
                template = "%s@.service" % parts[0]
                entry = entries.get(template)
                if entry is not None:
                    chain.append(template)
                    current = template
        if entry is None:
            return None, chain, "dangling: no lookup directory holds %s" % current
        if entry["kind"] != "alias":
            return current, chain, None
        hops += 1
        if hops >= FOLLOW_MAX:
            return None, chain, "more than %d hops through %s, which systemd does not follow" % (FOLLOW_MAX - 1, " -> ".join(chain))
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
