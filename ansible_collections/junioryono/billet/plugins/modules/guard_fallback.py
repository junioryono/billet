#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Find the executable a converge guard records, when the managed billet
cannot answer for it, without trusting a byte the trust boundary has not
admitted.

THE ONE FALLBACK. The role's preparation asks the managed billet for the
guard's status. When that binary is absent (an interrupted bootstrap) or
predates the guard (it answers "unknown command"), the only executable that
can answer is the candidate the guard's record names, staged in a recovery
directory under the upgrade root. This module is the reader of that record,
and it reads in THREE PHASES so that nothing is believed before the thing it
rests on is proved: the METADATA of every ancestor from the filesystem root
down to the root's parent (owned by root or the root's owner, writable by
others only under the sticky bit, a link on the way owned by one of them and
walked as written, the Go boundary's rule, because the candidate is later
executed by pathname and a writable ancestor lets another account rename the
chain the name resolves through), then the root's parent, the root, the guard
directory and the record (each examined without following a link, owned by
the root's owner, writable by nobody else, the record a regular file of mode
0600 and at most 4 KiB) before the record is read; the record's CONTENT
(exactly the five members the command writes, each a non-empty string, the
digest 64 lowercase hex, the executable an absolute path) before the
candidate is looked at; and the CANDIDATE (its parent a direct `recovery-*`
child of the root, every component from the root's parent down owned and
unwritable by others and never a link, the file regular, its sha256 taken
through the descriptor AFTER its metadata was admitted) before the path is
answered. The module executes nothing; the caller runs what it answers, and
the residual the plan accepts stands: between the digest and the exec only
the root's owner can swap the file, and that owner is the executor.

NO ONE-LINK RULE: the command imposes none on a candidate or a record, and a
rule the command does not have is not invented here.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

DOCUMENTATION = r"""
---
module: guard_fallback
short_description: Find the executable a converge guard records, under its trust boundary
description:
  - Reads C(<root>/active/guard.json) in three phases (metadata, content, the
    candidate) and answers the recorded executable's path when every check
    admits it. Fails naming the phase, the component and the property otherwise.
options:
  root:
    description: The upgrade root.
    type: path
    required: true
  owner:
    description: The uid that must own every component (root on Linux; the launch agent's account on a Mac).
    type: int
    required: true
  judge:
    description:
      - C(record) reads the guard's record and answers the recorded executable;
        C(ancestors) judges only the chain above and including the root's
        parent (and the root when it exists), which the preparation asks before
        anything under the root is executed.
    type: str
    choices: [record, ancestors]
    default: record
author:
  - junioryono
"""

EXAMPLES = r"""
- name: Judge the guard's record
  junioryono.billet.guard_fallback:
    root: /var/lib/billet/upgrades
    owner: 0
  register: billet_exclusion_fallback
"""

RETURN = r"""
executable:
  description: The recorded executable, admitted.
  type: str
  returned: success
sha256:
  description: The admitted executable's digest, equal to the record's.
  type: str
  returned: success
phase:
  description: metadata, content or candidate, when refused.
  type: str
  returned: failure
"""

import hashlib
import json
import os
import re
import stat
import unicodedata

from ansible.module_utils.basic import AnsibleModule

RECORD_MEMBERS = ("holder", "claimed_at", "hostname", "release_executable", "release_executable_sha256")
# THE PROTOCOL'S MEMBERS, admitted and typed when present: a record written
# before the protocol carries none of them and reads as settled. The token is
# judged as a string and never printed by this module.
# `note` is the holder's one line, typed as the command types it: a non-empty
# string of at most MAX_NOTE_BYTES with no control character.
OPTIONAL_MEMBERS = {"id": "hex32", "token": "hex32", "preparing": "bool", "note": "note"}
MAX_NOTE_BYTES = 200
# MATCHED WHOLE (fullmatch): `$` also matches before a final newline, so a
# digest or an id followed by one passed here and was refused by the command.
# MATCHED WHOLE (fullmatch): `$` also matches before a final newline, so a
# digest or an id followed by one passed here and was refused by the command.
_HEX32 = re.compile(r"^[0-9a-f]{32}$")
MAX_RECORD_BYTES = 4096
RECOVERY_PREFIX = "recovery-"
_SHA256 = re.compile(r"^[0-9a-f]{64}$")


class Refusal(Exception):
    def __init__(self, phase, message):
        super(Refusal, self).__init__("%s: %s" % (phase, message))
        self.phase = phase


def lstat_or_refuse(phase, path):
    try:
        return os.lstat(path)
    except FileNotFoundError:
        raise Refusal(phase, "%s does not exist" % path)
    except OSError as exc:
        raise Refusal(phase, "%s could not be examined: %s" % (path, exc))


def require_dir(phase, path, owner):
    st = lstat_or_refuse(phase, path)
    if stat.S_ISLNK(st.st_mode):
        raise Refusal(phase, "%s is a symlink" % path)
    if not stat.S_ISDIR(st.st_mode):
        raise Refusal(phase, "%s is not a directory" % path)
    require_owned(phase, path, st, owner)


def require_owned(phase, path, st, owner):
    if st.st_uid != owner:
        raise Refusal(phase, "%s is owned by uid %d, want %d" % (path, st.st_uid, owner))
    if st.st_mode & stat.S_IWGRP:
        raise Refusal(phase, "%s is writable by its group" % path)
    if st.st_mode & stat.S_IWOTH:
        raise Refusal(phase, "%s is writable by others" % path)


def require_regular(phase, path, owner):
    st = lstat_or_refuse(phase, path)
    if stat.S_ISLNK(st.st_mode):
        raise Refusal(phase, "%s is a symlink" % path)
    if not stat.S_ISREG(st.st_mode):
        raise Refusal(phase, "%s is not a regular file" % path)
    require_owned(phase, path, st, owner)
    return st


MAX_ANCESTOR_LINKS = 32


def require_trusted_ancestors(phase, parent, owner):
    """Every directory above the root's parent, from the filesystem root down,
    judged as the Go boundary judges it: a directory owned by root or by the
    owner, writable by group or others only under the sticky bit (a writable
    ancestor lets another account rename the whole chain, and a sticky one
    keeps it from renaming entries it does not own); a link on the way admitted
    only when the link itself is owned by root or by the owner, its target then
    walked as written. The candidate's later execution is by PATHNAME, so the
    chain the name resolves through is what must hold, not only the inode
    that was hashed."""
    components = [c for c in parent.split(os.sep) if c not in ("", ".")]
    # The root's parent itself is judged by its own stricter rule; here its
    # ancestors.
    components = components[:-1]
    at = os.sep
    # THE FILESYSTEM ROOT ITSELF FIRST: a `/` another account could write is the
    # one ancestor a walk over children never reaches.
    at_st = lstat_or_refuse(phase, at)
    require_ancestor(phase, at, at_st, owner)
    links = 0
    pending = list(components)
    while pending:
        name = pending.pop(0)
        if name == "..":
            at = os.path.dirname(at.rstrip(os.sep)) or os.sep
            at_st = lstat_or_refuse(phase, at)
            require_ancestor(phase, at, at_st, owner)
            continue
        candidate = os.path.join(at, name)
        # AN ABSENT ANCESTOR ENDS THE WALK when nobody else can supply it:
        # nothing lies under it to rename, and what the caller asks about below
        # it is answered by its own stat (a fresh host's parent not yet made; a
        # held converge whose tree vanished, which the claim's stat then refuses
        # as the exclusion having moved). But the sticky bit protects the
        # entries a directory holds and reserves no absent name, so an absent
        # component under a directory other accounts can write is one another
        # account creates between this judgement and the role's own creation.
        try:
            st = os.lstat(candidate)
        except FileNotFoundError:
            refuse_absent_under(phase, candidate, at, at_st)
            return at, at_st
        except OSError as exc:
            raise Refusal(phase, "%s could not be examined: %s" % (candidate, exc))
        if stat.S_ISLNK(st.st_mode):
            if st.st_uid not in (0, owner):
                raise Refusal(phase, "the link %s is owned by uid %d, want root or uid %d, and a link another account made is a name it can repoint" % (candidate, st.st_uid, owner))
            links += 1
            if links > MAX_ANCESTOR_LINKS:
                raise Refusal(phase, "%s: more than %d links on the way" % (candidate, MAX_ANCESTOR_LINKS))
            target = os.readlink(candidate)
            target_components = [c for c in target.split(os.sep) if c not in ("", ".")]
            if target.startswith(os.sep):
                at = os.sep
                at_st = lstat_or_refuse(phase, at)
            pending = target_components + pending
            continue
        require_ancestor(phase, candidate, st, owner)
        at, at_st = candidate, st
    return at, at_st


def refuse_absent_under(phase, absent, at, at_st):
    if at_st.st_mode & (stat.S_IWGRP | stat.S_IWOTH):
        raise Refusal(phase, "%s is absent under %s, which is mode %04o and writable by group or others, so another account could create it before the role does" % (absent, at, stat.S_IMODE(at_st.st_mode)))


def require_ancestor(phase, path, st, owner):
    if not stat.S_ISDIR(st.st_mode):
        raise Refusal(phase, "%s is not a directory" % path)
    if st.st_mode & (stat.S_IWGRP | stat.S_IWOTH) and not st.st_mode & stat.S_ISVTX:
        raise Refusal(phase, "%s is mode %04o, writable by group or others without the sticky bit, so another account could rename what lies under it" % (path, stat.S_IMODE(st.st_mode)))
    if st.st_uid not in (0, owner):
        raise Refusal(phase, "%s is owned by uid %d, want root or uid %d" % (path, st.st_uid, owner))


def read_record(root, owner):
    """The metadata phase, then the content phase; answers the decoded record."""
    parent = os.path.dirname(root)
    active = os.path.join(root, "active")
    record = os.path.join(active, "guard.json")

    require_trusted_ancestors("metadata", parent, owner)
    require_dir("metadata", parent, owner)
    require_dir("metadata", root, owner)
    require_dir("metadata", active, owner)
    st = require_regular("metadata", record, owner)
    if stat.S_IMODE(st.st_mode) != 0o600:
        raise Refusal("metadata", "%s is mode %04o, want 0600" % (record, stat.S_IMODE(st.st_mode)))
    if st.st_size > MAX_RECORD_BYTES:
        raise Refusal("metadata", "%s is %d bytes, over the record's %d" % (record, st.st_size, MAX_RECORD_BYTES))

    # THE READ, through a descriptor whose identity is the admitted inode's:
    # a file swapped at the name between the examination and the open is not
    # the one that was admitted.
    fd = os.open(record, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        opened = os.fstat(fd)
        if (opened.st_dev, opened.st_ino) != (st.st_dev, st.st_ino):
            raise Refusal("metadata", "%s changed between its examination and its read" % record)
        body = os.read(fd, MAX_RECORD_BYTES + 1)
    finally:
        os.close(fd)
    if len(body) > MAX_RECORD_BYTES:
        raise Refusal("metadata", "%s grew past %d bytes while it was read" % (record, MAX_RECORD_BYTES))

    # A REPEATED MEMBER REFUSES, as the Go decoder refuses it: json.loads
    # keeps the last value, so a record ending `"id": null, "id": "<hex>"`
    # would pass here and be refused there, one guard usable through one
    # route and not the other.
    try:
        decoded = json.loads(body.decode("utf-8"), object_pairs_hook=_one_of_each)
    except (UnicodeDecodeError, ValueError) as exc:
        raise Refusal("content", "%s is not JSON: %s" % (record, exc))
    if not isinstance(decoded, dict):
        raise Refusal("content", "%s is not a JSON object" % record)
    for name in RECORD_MEMBERS:
        if name not in decoded:
            raise Refusal("content", "%s: %s is missing" % (record, name))
    for name in decoded:
        if name not in RECORD_MEMBERS and name not in OPTIONAL_MEMBERS:
            raise Refusal("content", "%s: %s is not a member the command writes" % (record, name))
    for name, kind in OPTIONAL_MEMBERS.items():
        if name not in decoded:
            continue
        value = decoded[name]
        if kind == "bool":
            if not isinstance(value, bool):
                raise Refusal("content", "%s: %s is not a boolean" % (record, name))
        elif kind == "note":
            # The category check comes first: a lone surrogate is category Cs,
            # and encoding it would raise before any refusal.
            if (not isinstance(value, str) or value == ""
                    or any(unicodedata.category(ch).startswith("C") for ch in value)
                    or any(ch.isspace() and ch != " " for ch in value)
                    or "\ufffd" in value
                    or len(value.encode("utf-8", "surrogatepass")) > MAX_NOTE_BYTES):
                raise Refusal("content", "%s: note is not one line of printable text" % record)
        elif not isinstance(value, str) or not _HEX32.fullmatch(value):
            raise Refusal("content", "%s: %s is not 32 lowercase hex digits" % (record, name))
    # A RECORD FROM BEFORE THE PROTOCOL IS EXACTLY THE FIVE MEMBERS: a token or
    # a preparing flag without an id is a protocol record that lost its id, and
    # the command refuses it rather than adopting it.
    if "id" not in decoded:
        for name in ("token", "preparing"):
            if name in decoded:
                raise Refusal("content", "%s: %s without an id is neither a legacy record nor a whole one" % (record, name))
    for name in RECORD_MEMBERS:
        value = decoded[name]
        if value is None:
            raise Refusal("content", "%s: %s is null" % (record, name))
        if not isinstance(value, str):
            raise Refusal("content", "%s: %s is not a string" % (record, name))
        if value == "":
            raise Refusal("content", "%s: %s is empty" % (record, name))
    if not _SHA256.fullmatch(decoded["release_executable_sha256"]):
        raise Refusal("content", "%s: release_executable_sha256 is not 64 lowercase hex digits" % record)
    if not os.path.isabs(decoded["release_executable"]):
        raise Refusal("content", "%s: release_executable is not an absolute path" % record)
    return decoded


def admit_candidate(root, owner, executable, digest):
    """The candidate phase: the path's place, every component, then the digest
    through the descriptor after the metadata was admitted."""
    normalised = os.path.normpath(executable)
    if normalised != executable:
        raise Refusal("candidate", "%s is not a normalised path" % executable)
    recovery = os.path.dirname(executable)
    if os.path.dirname(recovery) != root or not os.path.basename(recovery).startswith(RECOVERY_PREFIX):
        raise Refusal("candidate", "%s is not inside a %s* child of %s" % (executable, RECOVERY_PREFIX, root))
    parent = os.path.dirname(root)
    require_trusted_ancestors("candidate", parent, owner)
    require_dir("candidate", parent, owner)
    require_dir("candidate", root, owner)
    require_dir("candidate", recovery, owner)
    st = require_regular("candidate", executable, owner)

    fd = os.open(executable, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK)
    try:
        opened = os.fstat(fd)
        if (opened.st_dev, opened.st_ino) != (st.st_dev, st.st_ino):
            raise Refusal("candidate", "%s changed between its examination and its read" % executable)
        digester = hashlib.sha256()
        while True:
            chunk = os.read(fd, 1 << 20)
            if not chunk:
                break
            digester.update(chunk)
    finally:
        os.close(fd)
    actual = digester.hexdigest()
    if actual != digest:
        raise Refusal("candidate", "%s has digest %s and the record names %s" % (executable, actual, digest))
    return actual


def judge_ancestors(root, owner):
    """The chain a candidate's pathname resolves through, judged before the
    preparation executes anything under the root: every ancestor from `/`,
    the root's parent when it exists (a fresh host's does not yet), and the
    root when it exists."""
    parent = os.path.dirname(root)
    at, at_st = require_trusted_ancestors("ancestors", parent, owner)
    for path in (parent, root):
        try:
            os.lstat(path)
        except FileNotFoundError:
            # The parent the role is about to make must be one nobody else
            # can make first: it is judged against the deepest directory the
            # walk reached (its own container, or the container of the first
            # absent component above it, whose maker owns everything below).
            # An absent root lies under a parent judged owned and writable by
            # nobody else, or under an absent parent.
            if path == parent:
                refuse_absent_under("ancestors", parent, at, at_st)
            continue
        require_dir("ancestors", path, owner)


def _one_of_each(pairs):
    """Build an object from its members, refusing a member that repeats."""
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError("the member %s is repeated" % key)
        out[key] = value
    return out


def find(root, owner):
    record = read_record(root, owner)
    digest = admit_candidate(root, owner, record["release_executable"], record["release_executable_sha256"])
    return record["release_executable"], digest


def main():
    module = AnsibleModule(
        argument_spec=dict(
            root=dict(type="path", required=True),
            owner=dict(type="int", required=True),
            judge=dict(type="str", default="record", choices=["record", "ancestors"]),
        ),
        supports_check_mode=True,
    )
    root = os.path.normpath(module.params["root"])
    if not os.path.isabs(root):
        module.fail_json(msg="root %s is not an absolute path" % root)
        return
    if module.params["judge"] == "ancestors":
        try:
            judge_ancestors(root, module.params["owner"])
        except Refusal as exc:
            module.fail_json(msg="the upgrade root's chain cannot be trusted: %s" % exc, phase=exc.phase)
            return
        module.exit_json(changed=False, judged=True, executable="", sha256="")
        return
    try:
        executable, digest = find(root, module.params["owner"])
    except Refusal as exc:
        module.fail_json(msg="the guard's recorded executable cannot be trusted: %s" % exc, phase=exc.phase)
        return
    module.exit_json(changed=False, executable=executable, sha256=digest)


if __name__ == "__main__":
    main()
