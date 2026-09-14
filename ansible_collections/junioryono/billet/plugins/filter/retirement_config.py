# Copyright (c) 2026 junioryono
# Apache-2.0

"""Read role and locator hints; only the caller's combined evidence admits work."""

import yaml


_PACKAGED_IDENTITY_DIR = "/var/lib/billet/server"
_STRING_TAG = "tag:yaml.org,2002:str"
_NULL_TAG = "tag:yaml.org,2002:null"


class _OutsideSubset(ValueError):
    """A named reason the compatibility reader cannot establish a locator."""


def _read_subset(text):
    # This pre-release compatibility path accepts one UTF-8 mapping document,
    # string mapping keys, no duplicates, anchors, aliases, merge keys or
    # explicit tags anywhere. It does not implement yaml.v3 in Python. A capable
    # answerer uses the Go classifier instead. Unknown fields and Go's typed
    # decoding can reject this subset; roles and locators remain hints only.
    # The reference controller's read-only inspection on 2026-09-14 found none
    # of those YAML features and state_dir: /var/lib/billet/server. The role's
    # to_nice_yaml emits aliases only for repeated object identity; a rendering
    # that does so holds on this compatibility path only, naming the alias.
    forbidden = []
    documents = 0
    for event in yaml.parse(text, Loader=yaml.SafeLoader):
        if isinstance(event, yaml.events.DocumentStartEvent):
            documents += 1
            if event.version is not None and event.version != (1, 1):
                raise _OutsideSubset("The installed configuration has an unsupported YAML version; only 1.1 is accepted.")
        if isinstance(event, yaml.events.AliasEvent):
            forbidden.append("alias *%s" % event.anchor)
        elif getattr(event, "anchor", None) is not None:
            forbidden.append("anchor &%s" % event.anchor)
        if getattr(event, "tag", None) is not None:
            forbidden.append("explicit tag %s" % event.tag)
    if forbidden:
        raise _OutsideSubset("The installed configuration contains forbidden YAML: %s." % ", ".join(forbidden))
    if documents != 1:
        raise _OutsideSubset("The installed configuration must contain a single YAML document.")

    root = yaml.compose(text, Loader=yaml.SafeLoader)
    if not isinstance(root, yaml.nodes.MappingNode):
        raise _OutsideSubset("The installed configuration root is not a mapping.")
    pending = [root]
    while pending:
        node = pending.pop()
        if isinstance(node, yaml.nodes.MappingNode):
            keys = set()
            for key, value in node.value:
                if isinstance(key, yaml.nodes.ScalarNode) and key.value == "<<":
                    raise _OutsideSubset("The installed configuration contains a forbidden merge key <<.")
                if not isinstance(key, yaml.nodes.ScalarNode) or key.tag != _STRING_TAG:
                    raise _OutsideSubset("The installed configuration contains a non-string mapping key.")
                if key.value in keys:
                    raise _OutsideSubset("The installed configuration contains duplicate key %r." % key.value)
                keys.add(key.value)
                pending.append(key)
                pending.append(value)
        elif isinstance(node, yaml.nodes.SequenceNode):
            pending.extend(node.value)
        elif isinstance(node, yaml.nodes.ScalarNode):
            if any(0xD800 <= ord(char) <= 0xDFFF for char in node.value):
                raise _OutsideSubset("The installed configuration contains a decoded surrogate code point.")
    return {key.value: value for key, value in root.value}


def _unknown(kind, why):
    return {"config": kind, "roles": "unknown", "identity_dir": "", "why": why}


def retirement_config(text):
    # The entire filter is the boundary: scanner overflow and future failures
    # must return a named hold, including failures after composing the YAML.
    try:
        return _retirement_config(text)
    except Exception:
        return _unknown("malformed", "The installed configuration cannot be decoded as the supported YAML subset.")


def _retirement_config(text):
    # None is supplied only after a successful stat proves the file absent;
    # empty bytes or YAML null are installed content, not that observation.
    if text is None:
        return {"config": "absent", "roles": "unknown", "identity_dir": _PACKAGED_IDENTITY_DIR}
    try:
        if isinstance(text, bytes):
            text = text.decode("utf-8")
        elif isinstance(text, str):
            text.encode("utf-8")
        else:
            return _unknown("malformed", "The installed configuration is not UTF-8 text.")
    except UnicodeError:
        return _unknown("malformed", "The installed configuration is not valid UTF-8.")
    try:
        cfg = _read_subset(text)
    except _OutsideSubset as exc:
        return _unknown("malformed", str(exc))
    if "server" not in cfg:
        if "node" not in cfg:
            return _unknown("unreadable", "The installed configuration defines neither a server nor a node section.")
        if not isinstance(cfg["node"], yaml.nodes.MappingNode):
            return _unknown("unreadable", "The installed node must be a mapping.")
        return {"config": "present", "roles": "node", "identity_dir": ""}
    if not isinstance(cfg["server"], yaml.nodes.MappingNode):
        return _unknown("unreadable", "The installed server must be a mapping.")
    server = {key.value: value for key, value in cfg["server"].value}
    for key in ("identity_dir", "state_dir"):
        if key not in server:
            continue
        value = server[key]
        if not isinstance(value, yaml.nodes.ScalarNode) or value.tag != _STRING_TAG or not value.value:
            return _unknown("unreadable", "The installed server.%s must be a non-empty string scalar." % key)
        if not value.value.startswith("/") or value.value.strip() != value.value:
            return _unknown("unreadable", "The installed server.%s must be absolute with no leading or trailing whitespace." % key)

    state = server.get("state")
    state_absent = state is None or (isinstance(state, yaml.nodes.ScalarNode) and state.tag == _NULL_TAG)
    if not state_absent and not isinstance(state, yaml.nodes.MappingNode):
        return _unknown("unreadable", "The installed server.state must be a mapping or null.")
    # Match internal/config/config.go applyStateDefaults: preserve identity_dir,
    # else inherit state_dir, including beside a non-null state block (semantic
    # validation later refuses that pairing). A state block invents no locator.
    # Without either locator, Go's default depends on the running account,
    # which this reader cannot know. Only a positively absent configuration
    # uses the packaged path.
    if "identity_dir" in server:
        identity = server["identity_dir"].value
    elif "state_dir" in server:
        identity = server["state_dir"].value
    elif state_absent:
        return _unknown("unreadable", "The installed server has no identity_dir or state_dir; Go's default depends on the running account (os.UserConfigDir(), falling back to .billet/server), so its locator is unknown.")
    else:
        return _unknown("unreadable", "The installed server.state supplies no default server.identity_dir or server.state_dir.")
    return {"config": "present", "roles": "server", "identity_dir": identity}


class FilterModule(object):
    def filters(self):
        return {"retirement_config": retirement_config}
