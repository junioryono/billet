#!/usr/bin/python
# Copyright (c) 2026 junioryono
# Apache-2.0

"""Decide whether two configurations name different node controllers.

THE COMPATIBILITY RULE'S ONE COMPARISON: a converge that has no executable to
ask (a release before the converge guard, with no candidate that carries it)
cannot migrate an endpoint, so the role compares the pre-render configuration's
node controllers with the rendering's through the one endpoint representation
(`module_utils/endpoint.py`, held to the same vector table as the Go one) and
refuses a difference before anything is stopped. Each side's controllers are
derived as billet's config loader derives them: `node.server_addrs`, or
`node.server_addr` as a list of one, else the server section's
`tls_router.node_addr` or its `listen` (which itself defaults to 127.0.0.1:7717)
when a server section exists, else none; the scheme is `node.tls`'s. A CHANGE IS
TWO SETS THAT DIFFER, in any order, as the Go judgement has it, since an order
matters only when a process starts: a side without a node section (no node
installed yet, or the node removed) names none, and a node's first start or its
removal is not a migration, so those compare as unchanged.

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
short_description: Decide whether two billet configurations name different node controllers
description:
  - Derives each configuration's node controllers as billet's loader does and compares the two
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
  description: Whether the two configurations name different controllers.
  type: bool
  returned: always
installed_endpoint:
  description: The pre-render configuration's controllers in canonical text, comma-separated, or null.
  type: str
  returned: always
desired_endpoint:
  description: The rendering's controllers in canonical text, comma-separated, or null.
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


def controllers_of(cfg, where):
    """The configuration's node controllers in order, or None without a node section."""
    cfg = _mapping(cfg, where)
    node = cfg.get("node")
    if node is None:
        return None
    node = _mapping(node, where + ".node")
    addr = node.get("server_addr")
    addrs = node.get("server_addrs")
    if addrs is not None:
        if not isinstance(addrs, list) or len(addrs) == 0:
            raise Refusal("%s.node.server_addrs names %s" % (where, "no controller" if isinstance(addrs, list) else "no list"))
        if addr not in (None, ""):
            raise Refusal("%s.node sets both server_addr and server_addrs" % where)
    elif addr is None or addr == "":
        if "server" in cfg and cfg["server"] is not None:
            server = _mapping(cfg["server"], where + ".server")
            listen = server.get("listen")
            addr = listen if listen not in (None, "") else DEFAULT_LISTEN
            router = server.get("tls_router")
            if router is not None:
                routed = _mapping(router, where + ".server.tls_router").get("node_addr")
                if routed not in (None, ""):
                    addr = routed
            addrs = [addr]
        else:
            raise Refusal("%s.node names no server_addr and there is no server section to take one from" % where)
    else:
        addrs = [addr]
    tls = node.get("tls") is not None
    out = []
    for one in addrs:
        if not isinstance(one, str):
            raise Refusal("%s.node.server_addr is not text" % where)
        try:
            parsed = parse(one, tls)
        except EndpointError as exc:
            raise Refusal("%s.node.server_addr %r: %s" % (where, one, exc))
        if parsed in out:
            raise Refusal("%s.node.server_addrs names %s twice" % (where, parsed.canonical()))
        out.append(parsed)
    return out


def _text(controllers):
    return ", ".join(c.canonical() for c in controllers) if controllers is not None else None


def compare(installed, desired):
    a = controllers_of(installed, "the installed configuration")
    b = controllers_of(desired, "the rendering")
    changed = a is not None and b is not None and set(a) != set(b)
    return changed, _text(a), _text(b)


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
