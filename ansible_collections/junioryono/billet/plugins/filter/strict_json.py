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


class FilterModule(object):
    def filters(self):
        return {"from_json_strict": from_json_strict}
