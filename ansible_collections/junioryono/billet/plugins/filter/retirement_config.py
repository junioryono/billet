# Copyright (c) 2026 junioryono
# Apache-2.0

"""Locate an identity for the incapable-answerer screen, granting no route."""

import yaml


class _UniqueLoader(yaml.SafeLoader):
    def __init__(self, stream):
        super().__init__(stream)
        self._checked_mappings = set()

    def flatten_mapping(self, node):
        # Check explicit keys BEFORE flattening: an explicit value may override
        # a merged value, and earlier maps in a merge sequence win, as in yaml.v3.
        # flatten_mapping also visits merge sources that have no constructor call.
        if node not in self._checked_mappings:
            keys = set()
            for key_node, _ in node.value:
                key = ("merge",) if key_node.tag == "tag:yaml.org,2002:merge" else self.construct_object(key_node)
                if key in keys:
                    raise ValueError("repeated configuration member")
                keys.add(key)
            self._checked_mappings.add(node)
        super().flatten_mapping(node)


def _unknown(kind, why):
    return {"config": kind, "roles": "unknown", "identity_dir": "", "why": why}


def retirement_config(text):
    # None is supplied only after a successful stat proves the file absent;
    # empty bytes or YAML null are installed content, not that observation.
    if text is None:
        return {"config": "absent", "roles": "unknown", "identity_dir": "/var/lib/billet/server"}
    try:
        cfg = yaml.load(text, Loader=_UniqueLoader)
    except (yaml.YAMLError, ValueError, TypeError, RecursionError):
        return _unknown("malformed", "The installed configuration cannot be decoded uniquely.")
    if not isinstance(cfg, dict):
        return _unknown("malformed", "The installed configuration is not a mapping.")
    server, node = cfg.get("server"), cfg.get("node")
    if server is None and isinstance(node, dict):
        return {"config": "present", "roles": "node", "identity_dir": ""}
    if not isinstance(server, dict):
        return _unknown("unreadable", "The installed configuration establishes neither a server locator nor a node-only installation.")
    # An omitted locator on a decoded server is not the packaged default:
    # Go derives its default from the invoking account. Do not guess it.
    identity = server.get("identity_dir") or server.get("state_dir")
    if (not isinstance(identity, str) or not identity.startswith("/") or identity.strip() != identity
            or (server.get("identity_dir") and server.get("state_dir"))):
        return _unknown("unreadable", "The installed server identity locator is missing, ambiguous or invalid.")
    return {"config": "present", "roles": "server", "identity_dir": identity}


class FilterModule(object):
    def filters(self):
        return {"retirement_config": retirement_config}
