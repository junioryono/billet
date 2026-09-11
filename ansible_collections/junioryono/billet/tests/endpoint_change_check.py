#!/usr/bin/env python3
"""Prove the endpoint_change module derives each side's endpoint as billet's
loader does and compares through the endpoint representation: an explicit
address equal to the derived default is no change, a changed `server.listen`
under two omitted `server_addr`s is one, a TLS state that changes the scheme is
one, a side without a node section (a first start, a removal, no installed
configuration) is not a change, and a node section that names no endpoint at
all is refused rather than read as none.
"""

import importlib.util
import pathlib
import sys
import types

sys.dont_write_bytecode = True

HERE = pathlib.Path(__file__).resolve().parent
COLLECTION = HERE.parent
MODULE = COLLECTION / "plugins" / "modules" / "endpoint_change.py"
ENDPOINT = COLLECTION / "plugins" / "module_utils" / "endpoint.py"

failures = []


def fail(msg):
    failures.append(msg)
    print("FAIL " + msg)


def load():
    # The module imports its module_utils by the collection's dotted name and
    # AnsibleModule from ansible's basic; both are stood in for here so the
    # comparison is tested as a function, without an Ansible runtime.
    endpoint_spec = importlib.util.spec_from_file_location("billet_endpoint", ENDPOINT)
    endpoint = importlib.util.module_from_spec(endpoint_spec)
    endpoint_spec.loader.exec_module(endpoint)

    pkg = "ansible_collections.junioryono.billet.plugins.module_utils"
    for name in ("ansible_collections", "ansible_collections.junioryono", "ansible_collections.junioryono.billet",
                 "ansible_collections.junioryono.billet.plugins", pkg):
        sys.modules.setdefault(name, types.ModuleType(name))
    sys.modules[pkg + ".endpoint"] = endpoint
    basic = types.ModuleType("ansible.module_utils.basic")
    basic.AnsibleModule = object
    sys.modules.setdefault("ansible", types.ModuleType("ansible"))
    sys.modules.setdefault("ansible.module_utils", types.ModuleType("ansible.module_utils"))
    sys.modules["ansible.module_utils.basic"] = basic

    spec = importlib.util.spec_from_file_location("endpoint_change", MODULE)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def main():
    mod = load()

    def node(addr=None, tls=False, server_listen=None, with_server=True):
        cfg = {}
        if with_server:
            cfg["server"] = {"listen": server_listen} if server_listen else {"state_dir": "/var/lib/billet/server"}
        n = {"name": "node-a"}
        if addr is not None:
            n["server_addr"] = addr
        if tls:
            n["tls"] = {"cert": "/c", "key": "/k", "ca": "/ca"}
        cfg["node"] = n
        return cfg

    cases = [
        ("an explicit address equal to the derived default is no change",
         node(), node(addr="127.0.0.1:7717"), False),
        ("a changed server.listen under two omitted server_addrs is a change",
         node(server_listen="127.0.0.1:7717"), node(server_listen="10.0.0.5:7717"), True),
        ("a changed server_addr is a change",
         node(addr="127.0.0.1:7717"), node(addr="127.0.0.1:7719"), True),
        ("the same address spelled as a URL is no change",
         node(addr="127.0.0.1:7717"), node(addr="http://127.0.0.1:7717"), False),
        ("a TLS state that changes the scheme is a change",
         node(addr="10.0.0.5:7717"), node(addr="10.0.0.5:7717", tls=True), True),
        ("two configurations without a node section are equal",
         {"server": {}}, {"server": {}}, False),
        ("a node added is a first start, not a change",
         {"server": {}}, node(addr="127.0.0.1:7717"), False),
        ("a node removed is a stop, not a change",
         node(addr="127.0.0.1:7717"), {"server": {}}, False),
        ("no installed configuration beside a rendering with a node is a first start",
         {}, node(addr="127.0.0.1:7717"), False),
    ]
    for name, a, b, want in cases:
        try:
            changed, ea, eb = mod.compare(a, b)
        except mod.Refusal as exc:
            fail("%s: refused: %s" % (name, exc))
            continue
        if changed != want:
            fail("%s: changed=%s (%s vs %s), want %s" % (name, changed, ea, eb, want))
        else:
            print("ok   " + name)

    refused = [
        ("a node with no server_addr and no server section is refused",
         node(with_server=False), node(addr="127.0.0.1:7717")),
        ("an address that is not text is refused",
         node(addr=7717), node(addr="127.0.0.1:7717")),
        ("an address with a zone is refused as the representation refuses it",
         node(addr="[fe80::1%eth0]:7717"), node(addr="127.0.0.1:7717")),
        ("a node section that is not a mapping is refused",
         {"server": {}, "node": "nope"}, node(addr="127.0.0.1:7717")),
    ]
    for name, a, b in refused:
        try:
            mod.compare(a, b)
        except mod.Refusal:
            print("ok   " + name)
        else:
            fail("%s: compared, want a refusal" % name)

    if failures:
        print("%d failures" % len(failures))
        sys.exit(1)
    print("endpoint_change: every case passed")


if __name__ == "__main__":
    main()
