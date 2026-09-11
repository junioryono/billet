#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""The `valid_holder` filter: whether a text is a converge guard holder, as
`billet converge-guard` judges one. The predicate is the collection's one,
`module_utils/holder.py`, shared with the fallback reader and held to
tests/fixtures/holder-vectors.json; this file only exposes it to templates.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

from ansible_collections.junioryono.billet.plugins.module_utils.holder import MAX_HOLDER_BYTES, valid_holder

__all__ = ["MAX_HOLDER_BYTES", "valid_holder"]


class FilterModule(object):
    def filters(self):
        return {"valid_holder": valid_holder}
