# Copyright (c) 2026 junioryono
# Apache-2.0

"""The one representation of a control-plane endpoint, in Python.

`internal/endpoint` is billet's Go representation: a scheme decided by the
node's TLS state, a typed host (a literal address canonicalised, or a DNS
spelling under a pinned ASCII grammar with its rootedness kept), a numeric
port, one canonical text, and equality by value. This module is the same
representation for the collection's Python, and BOTH are held to one vector
table, `tests/fixtures/endpoint-vectors.json`, row by row, never to each
other's defaults.

WHY NOT `urlsplit` ALONE. Python's `urlsplit` decides less than Go's `url.Parse`
and decides some of it differently (measured on Python 3.14.6 against Go 1.26,
2026-09-09): it strips a tab from the text where Go refuses any control
character; it keeps every `%` escape undecoded and unvalidated where Go
validates the escapes of the userinfo, the host, the path and the fragment
(never the query) and decodes the host's; it cannot tell an empty query or
fragment (`?`, `#`) from none; it answers no port for `host:` and for a
portless host alike; and `ipaddress` accepts a scoped address where Go refuses
a zone. So this module GATHERS the raw spelling's facts first, without
refusing, and then refuses in Go's order: syntax, scheme, host, route, port,
then the typed host (zone, grammar). A row the two languages could read apart
is a row of the table.
"""

from __future__ import absolute_import, division, print_function

__metaclass__ = type

import ipaddress
import re
from urllib.parse import urlsplit

KIND_LITERAL = "literal"
KIND_DNS = "dns"

_SYNTAX = "syntax"
_SCHEME = "scheme"
_HOST = "host"
_ROUTE = "route"
_ZONE = "zone"
_PORT = "port"
_GRAMMAR = "grammar"
_NOT_CANONICAL = "not_canonical"

KINDS = (_SYNTAX, _SCHEME, _HOST, _ROUTE, _ZONE, _PORT, _GRAMMAR, _NOT_CANONICAL)


class EndpointError(ValueError):
    """A spelling refused, with the one kind it is refused for."""

    def __init__(self, kind, message):
        if kind not in KINDS:
            raise ValueError("endpoint: unknown refusal kind %r" % (kind,))
        super(EndpointError, self).__init__("endpoint: %s: %s" % (kind, message))
        self.kind = kind
        self.message = message


class Endpoint(object):
    """A scheme, a typed host and a port; equal by value."""

    __slots__ = ("scheme", "kind", "host", "rooted", "port")

    def __init__(self, scheme, kind, host, rooted, port):
        self.scheme = scheme
        self.kind = kind
        self.host = host
        self.rooted = rooted
        self.port = port

    def canonical(self):
        """The canonical spelling: the scheme, the host (a literal in RFC 5952
        text, an IPv6 one bracketed; a name lower-cased with its trailing dot
        when rooted) and the port, always written."""
        if self.kind == KIND_LITERAL and ":" in self.host:
            host = "[" + self.host + "]"
        elif self.kind == KIND_DNS and self.rooted:
            host = self.host + "."
        else:
            host = self.host
        return "%s://%s:%d" % (self.scheme, host, self.port)

    def __eq__(self, other):
        if not isinstance(other, Endpoint):
            return NotImplemented
        return (self.scheme == other.scheme and self.kind == other.kind and self.host == other.host
                and self.rooted == other.rooted and self.port == other.port)

    def __ne__(self, other):
        eq = self.__eq__(other)
        if eq is NotImplemented:
            return eq
        return not eq

    def __hash__(self):
        return hash((self.scheme, self.kind, self.host, self.rooted, self.port))

    def __repr__(self):
        return "Endpoint(%r)" % (self.canonical(),)


_HEX = "0123456789abcdefABCDEF"


def _escapes(segment, where):
    """Every `%` in the segment starts a two-hex-digit escape, as Go's URL
    parser requires of the userinfo, the path and the fragment; the host's
    stricter rule is in `_host_escapes`."""
    i = 0
    while i < len(segment):
        if segment[i] == "%":
            if len(segment) - i < 3 or segment[i + 1] not in _HEX or segment[i + 2] not in _HEX:
                raise EndpointError(_SYNTAX, "not a URL: invalid URL escape %r in the %s" % (segment[i:i + 3], where))
            i += 3
        else:
            i += 1


def _host_escapes(host):
    """Go's host rule: an escape is admitted only for `%25` and for a byte
    that is not ASCII (the way a non-ASCII name is written percent-encoded),
    and every admitted escape is DECODED before the host is typed."""
    out = bytearray()
    i = 0
    while i < len(host):
        if host[i] == "%":
            if len(host) - i < 3 or host[i + 1] not in _HEX or host[i + 2] not in _HEX:
                raise EndpointError(_SYNTAX, "not a URL: invalid URL escape %r in the host" % (host[i:i + 3],))
            value = int(host[i + 1:i + 3], 16)
            if value != 0x25 and value < 0x80:
                raise EndpointError(_SYNTAX, "not a URL: invalid URL escape %r in the host" % (host[i:i + 3],))
            out.append(value)
            i += 3
        else:
            out.extend(host[i].encode("utf-8"))
            i += 1
    return out.decode("utf-8", "surrogateescape")


def parse(text, tls):
    """Read a spelling under the node's TLS state: a bare `host:port`, a bare
    host (the scheme's default port), or a URL whose scheme must agree with
    the TLS state. Raises EndpointError with one kind."""
    if not isinstance(text, str):
        raise EndpointError(_SYNTAX, "not a URL: %r is not text" % (text,))

    scheme = "https" if tls else "http"
    raw = text if "://" in text else scheme + "://" + text

    # SYNTAX, gathered from the raw spelling before anything is refused for a
    # later reason, in the order Go's parser meets it.
    for c in raw:
        if ord(c) < 0x20 or ord(c) == 0x7F:
            raise EndpointError(_SYNTAX, "not a URL: %r carries a control character" % (text,))

    explicit, _, rest = raw.partition("://")
    if not explicit or not re.match(r"^[A-Za-z][A-Za-z0-9+.-]*$", explicit):
        raise EndpointError(_SYNTAX, "not a URL: %r has no scheme" % (text,))
    explicit = explicit.lower()

    # The authority ends at the first `/`, `?` or `#`; what follows is routing.
    end = len(rest)
    for stop in "/?#":
        at = rest.find(stop)
        if at != -1 and at < end:
            end = at
    authority, remainder = rest[:end], rest[end:]

    userinfo = None
    hostport = authority
    if "@" in authority:
        userinfo, _, hostport = authority.rpartition("@")
        _escapes(userinfo, "userinfo")

    if hostport.startswith("["):
        close = hostport.find("]")
        if close == -1:
            raise EndpointError(_SYNTAX, "not a URL: %r has an unclosed bracket" % (text,))
        host_raw, after = hostport[1:close], hostport[close + 1:]
        if after and not after.startswith(":"):
            raise EndpointError(_SYNTAX, "not a URL: %r has text after the bracketed host" % (text,))
        port_raw = after[1:] if after else None
        bracketed = True
    else:
        if hostport.count(":") > 1:
            raise EndpointError(_SYNTAX, "not a URL: %r has an invalid port after its host" % (text,))
        host_raw, colon, port_raw = hostport.partition(":")
        if not colon:
            port_raw = None
        bracketed = False

    if port_raw and not port_raw.isdigit():
        raise EndpointError(_SYNTAX, "not a URL: %r has an invalid port %r after its host" % (text, ":" + port_raw))

    if bracketed:
        # Go's parser validates a bracketed host as an IP literal, a zone
        # written `%25...` with a non-empty name; the escape is decoded here.
        host_decoded = _host_escapes(host_raw) if "%" in host_raw else host_raw
        if "%" in host_decoded:
            addr_text, _, zone = host_decoded.partition("%")
            if zone == "":
                raise EndpointError(_SYNTAX, "not a URL: %r: zone must be a non-empty string" % (text,))
        else:
            addr_text = host_decoded
        try:
            literal = ipaddress.ip_address(addr_text)
        except ValueError:
            raise EndpointError(_SYNTAX, "not a URL: %r has an invalid host %r" % (text, host_raw))
        if not isinstance(literal, ipaddress.IPv6Address):
            raise EndpointError(_SYNTAX, "not a URL: %r has an invalid IP-literal" % (text,))
    else:
        host_decoded = _host_escapes(host_raw) if "%" in host_raw else host_raw

    # Try the whole spelling through urlsplit too: what it cannot parse is a
    # syntax refusal here as it is in Go.
    try:
        split = urlsplit(raw)
        _ = split.hostname
    except ValueError as exc:
        raise EndpointError(_SYNTAX, "not a URL: %r: %s" % (text, exc))

    if remainder.startswith("/"):
        path, _, tail = remainder.partition("?")
        path, _, _ = path.partition("#")
        _escapes(path, "path")
    fragment_at = remainder.find("#")
    if fragment_at != -1:
        _escapes(remainder[fragment_at + 1:], "fragment")

    # SCHEME.
    if explicit not in ("http", "https"):
        raise EndpointError(_SCHEME, "%r must be http or https" % (text,))
    if explicit != scheme:
        raise EndpointError(_SCHEME, "%r is %s, which contradicts a node that %s; drop the scheme or write %s"
                            % (text, explicit, "has a certificate to present" if tls else "has no certificate to present", scheme))

    # HOST, the authority as a whole.
    if authority == "":
        raise EndpointError(_HOST, "%r names no host" % (text,))

    # ROUTE.
    if userinfo is not None:
        raise EndpointError(_ROUTE, "%r carries userinfo" % (text,))
    if remainder.startswith("/"):
        raise EndpointError(_ROUTE, "%r carries a path" % (text,))
    if remainder.startswith("?"):
        raise EndpointError(_ROUTE, "%r carries a query" % (text,))
    if remainder.startswith("#"):
        raise EndpointError(_ROUTE, "%r carries a fragment" % (text,))

    # PORT.
    if port_raw == "":
        raise EndpointError(_PORT, "%r has an empty port" % (text,))
    if host_decoded == "":
        raise EndpointError(_HOST, "%r names no host" % (text,))
    if port_raw is None:
        port = 443 if scheme == "https" else 80
    else:
        port = int(port_raw, 10)
        if port < 1 or port > 65535:
            raise EndpointError(_PORT, "%r: port %d is outside 1 to 65535" % (text, port))

    kind, host, rooted = _parse_host(host_decoded, text)
    return Endpoint(scheme, kind, host, rooted, port)


_LABEL = re.compile(r"^[a-z0-9-]*$")


def _parse_host(host, text):
    """Type a host: a literal when `ipaddress` accepts it (a zone refused
    before any unmapping, never stripped; a mapped address unmapped), else a
    DNS spelling under the grammar."""
    try:
        literal = ipaddress.ip_address(host)
    except ValueError:
        literal = None

    if literal is not None:
        zone = getattr(literal, "scope_id", None)
        if zone:
            raise EndpointError(_ZONE, "%r carries the zone %r, which names an interface of one host and is not an endpoint (%r)"
                                % (host, zone, text))
        mapped = getattr(literal, "ipv4_mapped", None)
        if mapped is not None:
            literal = mapped
        if isinstance(literal, ipaddress.IPv6Address):
            return KIND_LITERAL, literal.compressed, False
        return KIND_LITERAL, str(literal), False

    name = host
    rooted = False
    if name.endswith("."):
        rooted = True
        name = name[:-1]

    for c in name:
        if ord(c) >= 0x80:
            raise EndpointError(_GRAMMAR, "name: %r is not ASCII; an internationalised name is admitted only as its xn-- form (%r)"
                                % (host, text))

    name = name.lower()

    for label in name.split("."):
        if label == "":
            raise EndpointError(_GRAMMAR, "name: %r has an empty label (%r)" % (host, text))
        if label.startswith("-"):
            raise EndpointError(_GRAMMAR, "name: the label %r of %r begins with a hyphen (%r)" % (label, host, text))
        if label.endswith("-"):
            raise EndpointError(_GRAMMAR, "name: the label %r of %r ends with a hyphen (%r)" % (label, host, text))
        for c in label:
            if c == "_":
                raise EndpointError(_GRAMMAR, "name: %r carries an underscore, which is not a hostname (%r)" % (host, text))
            if not _LABEL.match(c):
                raise EndpointError(_GRAMMAR, "name: %r carries %r, which is not a hostname character (%r)" % (host, c, text))

    return KIND_DNS, name, rooted


def parse_canonical(text):
    """Read a text that must be an endpoint's own canonical spelling, under
    the scheme it carries."""
    if not isinstance(text, str):
        raise EndpointError(_NOT_CANONICAL, "%r is not text" % (text,))
    tls = text.startswith("https://")
    if not tls and not text.startswith("http://"):
        raise EndpointError(_NOT_CANONICAL, "%r carries no scheme" % (text,))
    endpoint = parse(text, tls)
    if endpoint.canonical() != text:
        raise EndpointError(_NOT_CANONICAL, "%r spells %r" % (text, endpoint.canonical()))
    return endpoint
