#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Decide whether two configurations name different node endpoints.

THE COMPATIBILITY RULE'S ONE COMPARISON: a converge that has no executable to
ask (a release before the converge guard, with no candidate that carries it)
cannot migrate an endpoint, so the role compares the pre-render configuration's
node endpoint with the rendering's through the one endpoint representation
(`module_utils/endpoint.py`, held to the same vector table as the Go one) and
refuses a difference before anything is stopped. Each side's endpoint is
derived as billet's config loader derives it: `node.server_addr` when it is
written, else the server section's `listen` (which itself defaults to
127.0.0.1:7717) when a server section exists, else none; the scheme is
`node.tls`'s. A CHANGE IS TWO ENDPOINTS THAT DIFFER: a side without a node
section (no node installed yet, or the node removed) names none, and a node's
first start or its removal is not a migration, so those compare as unchanged.

Everything arrives parsed: the role reads the installed file and renders the
desired one on the controller, so this module needs no YAML on the host and
never reads the installed file itself.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

from ansible.module_utils.basic import AnsibleModule
from ansible_collections.junioryono.billet.plugins.module_utils.endpoint import EndpointError, parse

DOCUMENTATION = r"""
---
module: endpoint_change
short_description: Decide whether two billet configurations name different node endpoints
description:
  - Derives each configuration's node endpoint as billet's loader does and compares the two
    through the collection's endpoint representation.
options:
  installed:
    description: The pre-render configuration, parsed (a mapping, empty for none).
    type: dict
    required: true
  desired:
    description: The rendering, parsed.
    type: dict
    required: true
author: junioryono
"""

RETURN = r"""
changed_endpoint:
  description: Whether the two configurations name different endpoints.
  type: bool
  returned: always
installed_endpoint:
  description: The pre-render configuration's endpoint in canonical text, or null.
  type: str
  returned: always
desired_endpoint:
  description: The rendering's endpoint in canonical text, or null.
  type: str
  returned: always
"""

DEFAULT_LISTEN = "127.0.0.1:7717"


class Refusal(Exception):
    pass


def _mapping(value, where):
    if value is None:
        return {}
    if not isinstance(value, dict):
        raise Refusal("%s is not a mapping" % where)
    return value


def endpoint_of(cfg, where):
    """The configuration's node endpoint, or None without a node section."""
    cfg = _mapping(cfg, where)
    node = cfg.get("node")
    if node is None:
        return None
    node = _mapping(node, where + ".node")
    addr = node.get("server_addr")
    if addr is None or addr == "":
        if "server" in cfg and cfg["server"] is not None:
            server = _mapping(cfg["server"], where + ".server")
            listen = server.get("listen")
            addr = listen if listen not in (None, "") else DEFAULT_LISTEN
        else:
            raise Refusal("%s.node names no server_addr and there is no server section to take one from" % where)
    if not isinstance(addr, str):
        raise Refusal("%s.node.server_addr is not text" % where)
    tls = node.get("tls") is not None
    try:
        return parse(addr, tls)
    except EndpointError as exc:
        raise Refusal("%s.node.server_addr %r: %s" % (where, addr, exc))


def compare(installed, desired):
    a = endpoint_of(installed, "the installed configuration")
    b = endpoint_of(desired, "the rendering")
    changed = a is not None and b is not None and a != b
    return changed, (a.canonical() if a is not None else None), (b.canonical() if b is not None else None)


def main():
    module = AnsibleModule(
        argument_spec=dict(
            installed=dict(type="dict", required=True),
            desired=dict(type="dict", required=True),
        ),
        supports_check_mode=True,
    )
    try:
        changed, a, b = compare(module.params["installed"], module.params["desired"])
    except Refusal as exc:
        module.fail_json(msg="the endpoints cannot be compared: %s" % exc)
        return
    module.exit_json(changed=False, changed_endpoint=changed, installed_endpoint=a, desired_endpoint=b)


if __name__ == "__main__":
    main()
