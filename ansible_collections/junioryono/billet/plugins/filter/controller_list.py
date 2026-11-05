#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""The `controller_list` filter: whether a value is a list of controllers as
billet's commands write one, holding the endpoint named beside it. A flat list of
distinct, non-empty strings: `flatten` or `select` over a member cannot say so,
since a list nested inside it, or a number among the strings, passes both, and an
answer the producer never writes is refused rather than read.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type


def controller_list(value, member=None):
    if not isinstance(value, (list, tuple)):
        return False
    if any(not isinstance(item, str) or not item for item in value):
        return False
    if len(set(value)) != len(value):
        return False
    return member in value


class FilterModule(object):
    def filters(self):
        return {"controller_list": controller_list}
