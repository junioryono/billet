# Copyright (c) 2026 junioryono
# Apache-2.0

"""Literal grammars for inventory consumed by configuration interpreters."""

import ipaddress
import re
import unicodedata


def single_line(value):
    return isinstance(value, str) and not any(
        unicodedata.category(char).startswith('C') or char in '\u2028\u2029'
        for char in value)


def ip_literal(value, version=None):
    if not isinstance(value, str) or '%' in value:
        return False
    try:
        address = ipaddress.ip_address(value)
        return version is None or address.version == version
    except ValueError:
        return False


def smtp_host(value):
    if not single_line(value) or not value:
        return False
    if ip_literal(value):
        return True
    return len(value) <= 253 and all(re.fullmatch(r'[A-Za-z0-9](?:[A-Za-z0-9-]{0,61}[A-Za-z0-9])?', label)
                                     for label in (value[:-1] if value.endswith('.') else value).split('.'))


def port(value):
    return isinstance(value, (str, int)) and not isinstance(value, bool) and re.fullmatch(r'[1-9][0-9]{0,4}', str(value)) is not None and int(value) <= 65535


def firewall_port(value):
    """A port nftables reads inside a set: decimal, a decimal range, or a service name."""
    if isinstance(value, int) and not isinstance(value, bool):
        return port(value)
    if not single_line(value):
        return False
    if '-' in value and re.fullmatch(r'[0-9]+-[0-9]+', value):
        low, high = value.split('-')
        return port(low) and port(high) and int(low) <= int(high)
    return port(value) or re.fullmatch(r'[A-Za-z][A-Za-z0-9_+.-]{0,31}', value) is not None


def mailbox(value):
    if not single_line(value) or value.count('@') != 1:
        return False
    local, host = value.split('@')
    return bool(re.fullmatch(r"[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+(?:\.[A-Za-z0-9!#$%&'*+/=?^_`{|}~-]+)*", local)) and smtp_host(host)


def network(value, version):
    if not isinstance(value, str):
        return False
    try:
        return ipaddress.ip_network(value, strict=False).version == version
    except ValueError:
        return False


class FilterModule(object):
    def filters(self):
        return {'single_line': single_line, 'ip_literal': ip_literal, 'smtp_host': smtp_host,
                'port': port, 'firewall_port': firewall_port, 'mailbox': mailbox, 'ip_network_literal': network}
