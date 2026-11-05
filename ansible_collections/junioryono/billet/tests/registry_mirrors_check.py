#!/usr/bin/env python3
"""Prove the role_registry_mirrors filter decides as the host role means it to, and that
every rendering of a node's file applies it: the ordinary converge's and a retirement's
two. One of them deciding alone refused every retirement of a deployment whose nodes the
role gave mirrors. Run by converge-guard-check.sh before the cases."""

import importlib.util
import pathlib
import re
import sys

HERE = pathlib.Path(__file__).resolve().parent
FILTER = HERE.parent / "plugins" / "filter" / "registry_mirrors.py"
TASKS = HERE.parent / "roles" / "host" / "tasks"

INSTANCES = [
    {"name": "docker-io", "upstream": "docker.io", "port": 7740},
    {"name": "ghcr-io", "upstream": "ghcr.io", "port": 7741},
    {"name": "quay-io", "upstream": "quay.io", "port": 7742},
]
NETWORKS = [{"name": "billet0", "address": "172.31.0.1"}, {"name": "billet1", "address": "172.31.1.1"}]
NAMED = {
    "docker.io": "https://172.31.0.1:7740",
    "ghcr.io": "https://172.31.0.1:7741",
    "quay.io": "https://172.31.0.1:7742",
}


def load():
    spec = importlib.util.spec_from_file_location("registry_mirrors", FILTER)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def firecracker(**node):
    return {"node": dict({"name": "n1", "provider": "firecracker"}, **node), "tiers": []}


def main():
    filters = load().FilterModule().filters()
    render, active = filters["role_registry_mirrors"], filters["role_registry_mirrors_active"]
    failures = []

    got = render(firecracker(), True, NETWORKS, INSTANCES, True)
    if got["node"].get("registry_mirrors") != NAMED:
        failures.append("a Firecracker node was given %r" % got["node"].get("registry_mirrors"))
    if got.get("tiers") != [] or got["node"].get("name") != "n1":
        failures.append("the rest of the configuration moved: %r" % got)
    original = firecracker()
    render(original, True, NETWORKS, INSTANCES, True)
    if "registry_mirrors" in original["node"]:
        failures.append("the filter changed the configuration it was given")
    if not active(firecracker(), True, NETWORKS, INSTANCES):
        failures.append("an ordinary converge does not need the authority's proof")

    unchanged = {
        "turned off": (firecracker(), False, NETWORKS, True),
        "not yet proved": (firecracker(), True, NETWORKS, False),
        "no guest network": (firecracker(), True, [], True),
        "another provider": ({"node": {"name": "n1", "provider": "docker"}}, True, NETWORKS, True),
        "no node": ({"server": {}}, True, NETWORKS, True),
        "an operator's own mirrors": (firecracker(registry_mirrors={"docker.io": "https://m"}), True, NETWORKS, True),
        "an operator's own authority": (firecracker(registry_mirrors_ca="/etc/ca.pem"), True, NETWORKS, True),
    }
    for name, (config, enabled, networks, proved) in unchanged.items():
        if render(config, enabled, networks, INSTANCES, proved) != config:
            failures.append("%s: the configuration was changed" % name)
        if active(config, enabled, networks, INSTANCES, proved):
            failures.append("%s: said to be active" % name)

    # EVERY RENDERING OF A NODE'S FILE APPLIES IT.
    for name, pattern in {
        "registry-mirrors-config.yml": r"junioryono\.billet\.role_registry_mirrors\(",
        "retirement-request.yml": r"serverless_config\s*\|\s*junioryono\.billet\.role_registry_mirrors\(",
        "retirement-collect-host.yml": r"billet_config\s*\|\s*junioryono\.billet\.role_registry_mirrors\(",
    }.items():
        if not re.search(pattern, (TASKS / name).read_text()):
            failures.append("%s renders a node's file without role_registry_mirrors" % name)

    if failures:
        sys.exit("registry mirrors: " + "; ".join(failures))
    print("registry mirrors: one decision, applied wherever a node's file is rendered")


if __name__ == "__main__":
    main()
