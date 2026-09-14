# Copyright (c) 2026 junioryono
# Apache-2.0

"""Locate an identity for the incapable-answerer screen, granting no route."""

import yaml


class _UniqueLoader(yaml.SafeLoader):
    pass


def _mapping(loader, node):
    result = {}
    for key_node, value_node in node.value:
        key = loader.construct_object(key_node)
        if key in result:
            raise ValueError("repeated configuration member")
        result[key] = loader.construct_object(value_node)
    return result


_UniqueLoader.add_constructor(yaml.resolver.BaseResolver.DEFAULT_MAPPING_TAG, _mapping)


def retirement_config(text):
    try:
        cfg = yaml.load(text, Loader=_UniqueLoader)
    except (yaml.YAMLError, ValueError, TypeError):
        return {"config": "malformed", "roles": "unknown", "identity_dir": "/var/lib/billet/server"}
    if not isinstance(cfg, dict):
        return {"config": "malformed", "roles": "unknown", "identity_dir": "/var/lib/billet/server"}
    server, node = cfg.get("server"), cfg.get("node")
    if server is None and isinstance(node, dict):
        return {"config": "present", "roles": "node", "identity_dir": "/var/lib/billet/server"}
    if not isinstance(server, dict):
        return {"config": "unreadable", "roles": "unknown", "identity_dir": ""}
    # An omitted locator on a decoded server is not the packaged default:
    # Go derives its default from the invoking account. Do not guess it.
    identity = server.get("identity_dir") or server.get("state_dir")
    if (not isinstance(identity, str) or not identity.startswith("/") or identity.strip() != identity
            or (server.get("identity_dir") and server.get("state_dir"))):
        return {"config": "unreadable", "roles": "server", "identity_dir": ""}
    return {"config": "present", "roles": "server", "identity_dir": identity}


class FilterModule(object):
    def filters(self):
        return {"retirement_config": retirement_config}
