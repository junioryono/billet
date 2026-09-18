# Copyright (c) 2026 junioryono
# Apache-2.0

"""The retained configuration removes only the controller's top-level members."""

from collections.abc import Mapping

from ansible.errors import AnsibleFilterError


SERVER_MEMBERS = frozenset(('server', 'github', 'targets', 'backup'))


def serverless_config(value):
    if not isinstance(value, Mapping):
        raise AnsibleFilterError(
            'serverless_config requires a mapping; got %s' % type(value).__name__)
    return {key: member for key, member in value.items() if key not in SERVER_MEMBERS}


class FilterModule(object):
    def filters(self):
        return {'serverless_config': serverless_config}
