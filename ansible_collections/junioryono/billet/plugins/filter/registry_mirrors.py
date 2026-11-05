# Copyright (c) 2026 junioryono
# Apache-2.0

"""Whether the host role runs a host's pull-through registry mirrors, and the node
configuration that names them.

ONE DECISION, THREE READERS: the ordinary converge renders a node's file from it, and
a controller retirement renders the same file twice more, for its own retained node
and for every node it collects, then compares digests with what is installed. Each
of the three deciding for itself refused every retirement of a deployment whose
nodes the role gave mirrors (found in review, 2026-11-04), so this is the only place
the rule is written.

`proved` is the evidence an earlier ordinary converge installed the mirrors, the
authority's presence: a retained converge and a retirement take up nothing new, so
without it a node keeps the file it has.
"""

from collections.abc import Mapping

from ansible.errors import AnsibleFilterError


def _decide(config, enabled, networks, instances, proved):
    if not isinstance(instances, (list, tuple)) or not instances:
        raise AnsibleFilterError('registry mirrors need the role\'s instances')
    node = config.get('node') if isinstance(config, Mapping) else None
    active = (bool(enabled) and bool(proved) and isinstance(networks, (list, tuple))
              and len(networks) > 0 and isinstance(node, Mapping)
              and node.get('provider') == 'firecracker'
              and 'registry_mirrors' not in node and 'registry_mirrors_ca' not in node)
    if not active:
        return False, config
    address = networks[0].get('address') if isinstance(networks[0], Mapping) else None
    if not isinstance(address, str) or not address:
        raise AnsibleFilterError('the first guest network has no address for the mirrors to serve on')
    mirrors = {}
    for instance in instances:
        mirrors[instance['upstream']] = 'https://%s:%s' % (address, instance['port'])
    out = dict(config)
    out['node'] = dict(node)
    out['node']['registry_mirrors'] = mirrors

    return True, out


def role_registry_mirrors_active(config, enabled, networks, instances, proved=True):
    """Whether the role runs this host's mirrors."""
    return _decide(config, enabled, networks, instances, proved)[0]


def role_registry_mirrors(config, enabled, networks, instances, proved=True):
    """The configuration, naming the mirrors the role runs on this host, if it runs any."""
    return _decide(config, enabled, networks, instances, proved)[1]


class FilterModule(object):
    def filters(self):
        return {'role_registry_mirrors': role_registry_mirrors,
                'role_registry_mirrors_active': role_registry_mirrors_active}
