#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Read a JSON document the way billet's readers read one: an object's
members are unique, at every depth, or the document is refused.

`from_json` keeps the last of a repeated member, so an answer ending
`"pointer": true, "pointer": false` reads as settled where the Go decoder of
the same text refuses it, and the role's facts would be taken from a value
its own parser never judged. Every answer of the converge guard's protocol the
host role reads (`prepare`'s two calls, the dry run's report, the remainder
after a cleanup) goes through this filter and nothing else.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

import json

from ansible.errors import AnsibleFilterError


def _one_of_each(pairs):
    """Build an object from its members, refusing a member that repeats."""
    out = {}
    for key, value in pairs:
        if key in out:
            raise ValueError("the member %s is repeated" % key)
        out[key] = value
    return out


def from_json_strict(text):
    """Decode text as JSON, refusing a repeated member at any depth."""
    if isinstance(text, bytes):
        try:
            text = text.decode("utf-8")
        except UnicodeDecodeError as exc:
            raise AnsibleFilterError("from_json_strict: not UTF-8: %s" % exc)
    if not isinstance(text, str):
        raise AnsibleFilterError("from_json_strict: the input is %s, not text" % type(text).__name__)
    try:
        return json.loads(text, object_pairs_hook=_one_of_each)
    except ValueError as exc:
        raise AnsibleFilterError("from_json_strict: %s" % exc)


def provenance_proves(text, binary_sha256):
    """Whether a provenance record proves which manifest produced these bytes.

    billet's own reading (internal/provenance: Read, then Installed's hash
    comparison): an object naming a manifest digest and the sha256 of exactly
    this binary. Anything else is False, never an error, because a record that
    cannot be read is a host that reports nothing, which a backfill repairs,
    and failing a converge over it would trade a working host for a diagnostic.
    """
    try:
        record = from_json_strict(text)
    except AnsibleFilterError:
        return False
    if not isinstance(record, dict):
        return False
    digest = record.get("manifest_digest")
    recorded = record.get("binary_sha256")
    return (
        isinstance(digest, str)
        and digest != ""
        and isinstance(recorded, str)
        and isinstance(binary_sha256, str)
        and binary_sha256 != ""
        and recorded == binary_sha256
    )


class FilterModule(object):
    def filters(self):
        return {"from_json_strict": from_json_strict, "provenance_proves": provenance_proves}
