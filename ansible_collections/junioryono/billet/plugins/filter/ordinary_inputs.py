# Copyright (c) 2026 junioryono
# Apache-2.0

"""Ordinary readers consume strict observations, never inventory substitutes."""

from ansible.errors import AnsibleFilterError
from ansible_collections.junioryono.billet.plugins.module_utils import ordinary_inputs


def checked(function):
    def call(*args):
        try:
            return function(*args)
        except ordinary_inputs.Refusal as exc:
            raise AnsibleFilterError(str(exc))
    return call


class FilterModule(object):
    def filters(self):
        return {name: checked(getattr(ordinary_inputs, name)) for name in
                ['environment_specs', 'unit_results', 'operation_document']}
