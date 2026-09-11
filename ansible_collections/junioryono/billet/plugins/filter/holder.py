#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Whether a text is a converge guard holder, as `billet converge-guard`
judges one (`checkHolder`): non-empty, at most 200 BYTES of UTF-8, and no
character that is a Unicode space or control, a slash, or the replacement
character. The role reads a receipt's `run` through this and nothing else,
because a receipt the command wrote carries a holder it accepted, and a
character class approximated by a regular expression admitted what the
command refuses (a count of characters rather than bytes, U+FFFD, the C1
controls). The vector table both languages are held to is
tests/fixtures/holder-vectors.json, written by the Go test from
`checkHolder`'s own verdicts.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

import unicodedata

MAX_HOLDER_BYTES = 200

# Go's unicode.IsSpace for Latin-1, beside the White_Space property the Zs, Zl
# and Zp categories carry outside it.
_LATIN1_SPACES = frozenset("\t\n\v\f\r  ")


def valid_holder(value):
    """True when value is a holder the command would accept."""
    if not isinstance(value, str):
        return False
    # A lone surrogate (which a JSON escape can produce) is text Go refuses
    # as invalid UTF-8; here it has no encoding, and it is not a holder.
    try:
        encoded = value.encode("utf-8")
    except UnicodeEncodeError:
        return False
    if value == "" or len(encoded) > MAX_HOLDER_BYTES:
        return False
    for ch in value:
        if ch == "/" or ch == "�" or ch in _LATIN1_SPACES:
            return False
        if unicodedata.category(ch) in ("Zs", "Zl", "Zp", "Cc"):
            return False
    return True


class FilterModule(object):
    def filters(self):
        return {"valid_holder": valid_holder}
