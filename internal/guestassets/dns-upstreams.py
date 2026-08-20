#!/usr/bin/env python3
"""Emit the container DNS list for dockerd, as a JSON array on stdout.

The list is the pinned gateway first, then every real, usable IPv4 upstream in the
resolver file. It prints NOTHING when none qualifies, which the caller reads as "do
not remap container DNS" -- a resolver-of-one that a guest dnsmasq outage could take
down is worse than no remap at all.

The whole point is that a value dockerd's IP parser would reject NEVER reaches
daemon.json: a zoned fe80::1%eth0 (which systemd-resolved writes from RDNSS), a
malformed or out-of-range literal, a bare non-IP token, or another local stub. Any
of those in the "dns" list makes the daemon refuse to start, and starting it is a
hard precondition for the job -- so a lost cache remap must never become a dead job.
Loopback, link-local, unspecified, multicast and reserved are dropped too (in every
spelling, including IPv4-mapped IPv6), and the gateway itself is excluded so the list
can never be a self-loop. The upstreams are IPv4 ONLY: the Docker bridge is IPv4, so
a native IPv6 fallback is one a container cannot reach -- a dead fallthrough, which
is the one thing this list must never be. A container still reaches the IPv6 world
through the guest resolver, which forwards there; IPv6 just cannot be the fallback.
Validation is a real parser, not a regex, because the regexes that came before it
kept admitting a form net.ParseIP rejects.

Usage: billet-dns-upstreams <gateway-ip> <resolv-file>
"""

import ipaddress
import json
import sys

# 0.0.0.0/8 ("this network"): every address in it is a non-routable source-only
# form, but only 0.0.0.0 itself is is_unspecified, so the rest need naming here.
THIS_NETWORK = ipaddress.ip_network("0.0.0.0/8")


def unwrap(address):
    """The IPv4 an IPv4-mapped IPv6 address carries, else the address itself.

    Classification and identity must both see through the ::ffff: spelling, or a
    mapped form of a special address -- or of the gateway itself -- slips past a
    check written against the plain form.
    """
    mapped = getattr(address, "ipv4_mapped", None)
    return mapped if mapped is not None else address


def usable(address):
    """A routable unicast upstream a container can actually reach, in any spelling.

    is_private is deliberately allowed -- a site's own resolver (10/8, 192.168/16,
    ULA) is a real upstream -- but the non-unicast reserved forms are not: loopback,
    link-local, unspecified, multicast, the reserved blocks (240/4, the broadcast),
    and the rest of 0.0.0.0/8. Any of those in dockerd's dns list is either rejected
    by the daemon or simply unreachable, so it can never be the real fallthrough.
    """
    if (
        address.is_loopback
        or address.is_link_local
        or address.is_unspecified
        or address.is_multicast
        or address.is_reserved
    ):
        return False
    return not (
        isinstance(address, ipaddress.IPv4Address) and address in THIS_NETWORK
    )


def main():
    if len(sys.argv) != 3:
        sys.exit("usage: billet-dns-upstreams <gateway-ip> <resolv-file>")
    gateway_text, resolv = sys.argv[1], sys.argv[2]
    try:
        gateway = unwrap(ipaddress.ip_address(gateway_text))
    except ValueError:
        return

    try:
        with open(resolv, encoding="ascii", errors="replace") as handle:
            lines = handle.readlines()
    except OSError:
        return

    upstreams = []
    seen = set()
    for line in lines:
        fields = line.split()
        if len(fields) < 2 or fields[0] != "nameserver":
            continue
        token = fields[1]
        # A zoned address (fe80::1%eth0) is link-local and unparseable here anyway.
        if "%" in token:
            continue
        try:
            address = unwrap(ipaddress.ip_address(token))
        except ValueError:
            continue
        # IPv4 only: the Docker bridge is IPv4 (bip, no bip6), so a native IPv6
        # upstream in the dns list is a fallback a container cannot reach -- a dead
        # fallthrough when the guest resolver is down, which is the one thing this
        # list must never be. A container reaches the real IPv6 world through the
        # guest resolver, which forwards there; it just cannot be the fallback. An
        # IPv4-mapped form is unwrapped and emitted as the plain IPv4 it carries.
        if not isinstance(address, ipaddress.IPv4Address):
            continue
        if not usable(address) or address == gateway:
            continue
        key = address.compressed
        if key in seen:
            continue
        seen.add(key)
        upstreams.append(str(address))

    if not upstreams:
        return
    print(json.dumps([gateway_text] + sorted(upstreams)))


if __name__ == "__main__":
    main()
