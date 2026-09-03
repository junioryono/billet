#!/usr/bin/env python3
"""Billet's two guest-side entry points into the node's Actions cache.

`--mode passthrough` is a transparent TLS passthrough. Only the Actions results
origin is redirected to it, by a guest DNS remap
(results-receiver.actions.githubusercontent.com -> this listener). Every other
destination resolves normally and never touches this process, so bulk transfers
-- action tarballs, toolchains, artifact blob uploads -- go direct and are not
funnelled through a single Python relay. That routing is the whole point: an
earlier design proxied ALL of the runner's HTTPS through here, and large
transfers stalled and corrupted while small cache calls survived.

That listener does not terminate TLS. It accepts the client's raw connection,
opens the authenticated CONNECT tunnel to the node, and copies bytes both ways.
The node terminates TLS with its own results-receiver leaf and decides, per
request, whether to serve the cache locally or splice the rest upstream to
GitHub. The client's TLS session is therefore end to end with the node, which is
what lets the node present a certificate the guest already trusts.

`--mode cache-adapter` exists for the one client that arrangement cannot reach:
a BuildKit created with buildx's `docker-container` driver runs in its own image
with its own trust store, so the remapped origin presents it a node leaf it
cannot verify and `type=gha` dies with `x509: certificate signed by unknown
authority`. billet cannot edit that image. So this mode serves PLAINTEXT HTTP on
the guest's docker gateway address, where a container can reach it, and does the
TLS itself: it rewrites the request head -- the results Host the node requires,
plus the two headers that admit billet's own adapter and say which origin a
signed blob URL must name -- opens the same authenticated tunnel, writes the
request, and relays the answer. The guest's `docker` shim points a build at it,
and a workflow may name it with `url_v2=` as well. Only the BuildKit leg is
cleartext, and only inside the guest.

THE TWO MODES RELAY DIFFERENTLY AND HAVE TO. The passthrough copies both
directions at once because it carries whatever the results origin carries,
including a websocket. The adapter cannot: it OWNS a TLS session, and an
ssl.SSLSocket is not safe for a concurrent read and write, so it does one
request and one answer in order -- which is all the paths it admits ever need.

BOTH MODES ARE ONE FILE because they are one mechanism: the CONNECT tunnel, the
header reader, the fail-open dial and the timeouts are shared, and a second copy
of them is two things to keep in step.
"""

import argparse
import base64
import ipaddress
import os
import select
import signal
import socket
import threading
import time
import urllib.parse

# ONLY THE ADAPTER NEEDS TLS, AND THE PASSTHROUGH MUST NOT DEPEND ON IT. This runs
# under whichever interpreter the guest's toolcache carries; one built without
# _ssl would otherwise fail at import and take the passthrough down with it --
# every intercepted cache call in the guest -- for the sake of a mode it was not
# asked to run. Missing here is a cache adapter that refuses to start, which is
# the same degradation as any other reason it does not.
try:
    import ssl
except ImportError:
    ssl = None

RESULTS_HOST = "results-receiver.actions.githubusercontent.com"
RESULTS_AUTHORITY = RESULTS_HOST + ":443"
RESULTS_PORT = 443
HEADER_LIMIT = 64 * 1024
CONNECT_TIMEOUT = 10
# The first descriptor systemd passes to an activated service (sd-daemon's
# SD_LISTEN_FDS_START). Owning the listener in systemd, not here, is what closes
# the crash-restart window: a SIGKILL of this process leaves the socket bound, so
# new connections queue in its backlog instead of being refused, and the restarted
# process adopts the same descriptor and accepts them.
SD_LISTEN_FDS_START = 3
# After one direction of a relay closes, the other is given this long to keep
# draining. A live transfer resets it on every chunk, so only an IDLE half-open
# connection -- a client that ignores the node closing on it -- is torn down,
# which is what stops a malformed connection leaking a thread and two descriptors.
DRAIN_IDLE = 30
# A blocking sendall to a peer that stopped reading fills the socket buffer and
# then blocks the relay thread forever, out of reach of the idle select above. A
# finite socket timeout bounds that: a healthy chunk is handed to the kernel in
# milliseconds, so only a peer that has genuinely stalled for this long is cut,
# and that connection was already wedged. It is generous enough that no
# progressing transfer -- even a slow one -- ever trips it.
RELAY_TIMEOUT = 300
# How long the adapter waits for an answer that has not started arriving. It
# begins once the request body is written -- every read and write before that is
# bounded per operation by RELAY_TIMEOUT -- and every byte received resets it, so
# it is an inactivity bound rather than a transfer one. Deliberately longer than
# the node's own per-request budget (12m45s), because the node ends a request it
# cannot finish by ANSWERING, and a client given a response it can read beats a
# socket that died under it: a finalize is silent while it trims, unmounts,
# snapshots and publishes.
ADAPTER_IDLE = 900

# WHAT THE NODE READS TO ADMIT THIS ADAPTER, and it is not an identity
# credential: any process in the guest can set it, exactly as any of them could
# already send "@actions/cache-1.0" and be served locally. It exists because
# BuildKit cannot be told apart by user agent without admitting every client
# that types "buildkit/", and because a plaintext blob URL is only usable by a
# caller that reached billet over loopback. See internal/node/actions_cache.go.
CACHE_CLIENT_HEADER = "X-Billet-Cache-Client"
CACHE_ORIGIN_HEADER = "X-Billet-Cache-Origin"
CACHE_CLIENT_VALUE = "billet-loopback"
# THE ADAPTER SERVES BILLET'S CACHE AND NOTHING ELSE. The results origin also
# carries ArtifactService and the runner's live-log websocket; a plaintext port
# that forwarded those would be a general cleartext entry point to an origin
# this design deliberately keeps end to end. The prefix rather than the three
# exact methods, so a v2 method added later still reaches the node.
ADAPTER_PATHS = (
    "/twirp/github.actions.results.api.v1.CacheService/",
    "/_billet/actions-cache/",
)
ADAPTER_METHODS = frozenset({"GET", "HEAD", "POST", "PUT"})
# Replaced or supplied by the rewrite. Every other header the client sent --
# Authorization, Content-Length, Content-Type, the Azure x-ms-* set -- is
# forwarded untouched, because the node and GitHub both need it as it was.
ADAPTER_DROPPED_HEADERS = frozenset({
    "host",
    "connection",
    "proxy-connection",
    "keep-alive",
    CACHE_CLIENT_HEADER.lower(),
    CACHE_ORIGIN_HEADER.lower(),
})
ADAPTER_REFUSALS = {
    400: "Bad Request",
    403: "Forbidden",
    502: "Bad Gateway",
}
# How writing one request body ended. See forward_body.
BODY_SENT = "sent"
BODY_SHORT = "short"
BODY_UNREAD = "unread"
# Marks every response the adapter composes itself rather than relays, so a reader
# can tell one from the same status returned by GitHub.
ADAPTER_REFUSED_HEADER = "X-Billet-Cache-Adapter"
# What a non-blocking read raises when the record it consumed held no application
# data. Built here because the ssl import is optional: the passthrough runs
# without it, and the adapter refuses to start without it.
WOULD_BLOCK = (BlockingIOError,) if ssl is None else (
    BlockingIOError, ssl.SSLWantReadError, ssl.SSLWantWriteError)


def read_headers(connection):
    data = bytearray()
    while b"\r\n\r\n" not in data:
        chunk = connection.recv(min(4096, HEADER_LIMIT + 1 - len(data)))
        if not chunk:
            raise ConnectionError("connection closed before the headers")
        data.extend(chunk)
        if len(data) > HEADER_LIMIT:
            raise ValueError("headers exceed 64KiB")
    headers, remainder = bytes(data).split(b"\r\n\r\n", 1)
    return headers, remainder


def node_tunnel(upstream):
    """Open a CONNECT tunnel to the node's authenticated cache proxy."""
    parsed = urllib.parse.urlsplit(upstream)
    if parsed.scheme != "http" or not parsed.hostname or parsed.port is None:
        raise ValueError("the Billet node proxy must be an http URL with an explicit port")
    connection = socket.create_connection((parsed.hostname, parsed.port), CONNECT_TIMEOUT)
    try:
        headers = [
            f"CONNECT {RESULTS_AUTHORITY} HTTP/1.1",
            f"Host: {RESULTS_AUTHORITY}",
        ]
        if parsed.username is not None:
            username = urllib.parse.unquote(parsed.username)
            password = urllib.parse.unquote(parsed.password or "")
            credential = base64.b64encode(f"{username}:{password}".encode()).decode()
            headers.append(f"Proxy-Authorization: Basic {credential}")
        connection.sendall(("\r\n".join(headers) + "\r\n\r\n").encode())
        response, remainder = read_headers(connection)
        status = response.split(b"\r\n", 1)[0].split()
        if len(status) < 2 or status[1] != b"200":
            raise ConnectionError("the Billet node refused the cache tunnel")
        return connection, remainder
    except Exception:
        connection.close()
        raise


def direct_tunnel(addresses):
    """Connect straight to the real results origin, bypassing the node.

    This is the fail-open path. If the node cannot take the tunnel -- it is
    restarting, unreachable, or refuses the session -- the client's TLS still
    reaches GitHub directly, so its cache call degrades to an ordinary miss while
    the artifact, log-archive, step-update and live-log traffic that shares this
    origin keeps working. The client trusts GitHub's real certificate through the
    distribution roots in its bundle, exactly as it trusts the node's leaf through
    the node CA in the same bundle. The addresses were resolved before the guest
    remapped the origin to this listener, so they name GitHub and not this process.
    """
    last = None
    for address in addresses:
        try:
            return socket.create_connection((address, RESULTS_PORT), CONNECT_TIMEOUT), b""
        except OSError as error:
            last = error
    raise last or ConnectionError("no results-origin fallback address")


def relay(client, node, node_buffer):
    if node_buffer:
        client.sendall(node_buffer)
    peers = {client: node, node: client}
    half_closed = False
    while peers:
        readable, _, _ = select.select(list(peers), [], [], DRAIN_IDLE)
        if not readable:
            # No data for the idle interval. Before a half-close this is an
            # ordinarily quiet connection the node's own timeout will end; after
            # one the remaining side is not draining, so tear the whole thing down
            # rather than loop on it forever.
            if half_closed:
                break
            continue
        for source in readable:
            target = peers.get(source)
            if target is None:
                continue
            data = source.recv(64 * 1024)
            if data:
                target.sendall(data)
                continue
            try:
                target.shutdown(socket.SHUT_WR)
            except OSError:
                pass
            peers.pop(source, None)
            half_closed = True


def rewrite_adapter_request(head, origin):
    """Rewrite one plaintext request head into the one the node must receive.

    Returns the rewritten head and the length of the body that follows it, or
    (None, 0) for anything this adapter does not serve, which the caller refuses
    rather than forwards.

    THE BODY LENGTH IS PART OF THE ANSWER because the relay is sequential: the
    request is written in full and only then is the answer read, which is what
    keeps one thread on the TLS socket. So the framing has to be one this can
    count. `Content-Length` is what every client on this path sends -- the toolkit
    and BuildKit both post a sized buffer, and the Azure SDK a sized block -- and
    the two shapes that cannot be counted are refused rather than guessed at: a
    chunked body, and an `Expect: 100-continue` whose body does not arrive until
    an interim response this does not implement has been sent.

    THE HOST IS NOT OPTIONAL. The node answers 421 to an inner request that does
    not name the results origin (internal/node/actions_proxy.go), because a
    mismatched Host hands routing to whatever GitHub's edge does with it. A
    loopback client necessarily sends `Host: 127.0.0.1:<port>`, so the rewrite is
    what makes this path reachable at all.

    THE ORIGIN HEADER IS WHAT MOVES THE BLOB LEG. Served locally, a reservation
    answers with a signed URL, and billet builds that URL against the results
    host unless this says otherwise -- which would send the client back over TLS
    to the certificate it does not trust, one request later, after storage has
    already been allocated.

    `Connection: close` because the node writes one response per intercepted
    tunnel and closes; saying so up front keeps a client from pipelining a second
    request onto a connection that is already spliced somewhere.
    """
    # NO CONTROL BYTES BUT THE FRAMING ITSELF, and this is what makes the rewrite
    # mean what it says. A BARE LF is a line terminator to Go's parser and not to
    # this split, so a head carrying one would be rewritten as fewer headers than
    # the node then reads. A lone CR or a NUL is the same class one layer down: it
    # sits inside a target or a value here and is read differently there. A field
    # value is visible ASCII plus SP and HTAB, so everything else is refused
    # rather than forwarded to a parser that disagrees about where a line ends.
    body = head.replace(b"\r\n", b"")
    if any(byte < 0x20 and byte != 0x09 for byte in body) or 0x7F in body:
        return None, 0

    lines = head.split(b"\r\n")
    try:
        request_line = lines[0].decode("ascii")
    except UnicodeDecodeError:
        return None, 0

    fields = request_line.split(" ")
    if len(fields) != 3 or fields[2] != "HTTP/1.1" or fields[0] not in ADAPTER_METHODS:
        return None, 0
    target = fields[1]
    prefix = next((path for path in ADAPTER_PATHS if target.startswith(path)), "")
    if not prefix:
        return None, 0

    # ONE PLAIN SEGMENT BELOW THE PREFIX, because a prefix match is not a path
    # match. `…/CacheService/../ArtifactService/CreateArtifact` starts with the
    # cache prefix, is not one of the node's local methods, and is therefore
    # spliced upstream as written -- where a normaliser resolves it into the
    # service this listener says it does not carry. Dot segments, an empty
    # segment, a backslash and a percent escape all mean something to a parser
    # downstream, so none of them is admitted; the query is left alone, since an
    # Azure block id is base64 and lives there.
    segment = target[len(prefix):].split("?", 1)[0]
    if not segment or segment in (".", "..") or \
            not all(character.isalnum() or character in "-_." for character in segment):
        return None, 0

    # DECLARED ONCE OR NOT AT ALL. Two Content-Length headers are a request Go
    # refuses anyway, and the ambiguity is exactly the kind this must not resolve
    # by picking one.
    length = None
    rewritten = [
        request_line,
        "Host: " + RESULTS_HOST,
        "Connection: close",
        CACHE_CLIENT_HEADER + ": " + CACHE_CLIENT_VALUE,
        CACHE_ORIGIN_HEADER + ": " + origin,
    ]
    for raw in lines[1:]:
        try:
            header = raw.decode("ascii")
        except UnicodeDecodeError:
            return None, 0
        # An obsolete folded continuation belongs to the header above it, which
        # this loop has already decided about; refuse rather than forward a line
        # whose meaning depends on one that may have been dropped.
        if header[:1] in (" ", "\t") or ":" not in header:
            return None, 0
        name = header.split(":", 1)[0].strip().lower()
        value = header.split(":", 1)[1].strip()
        if name == "transfer-encoding" or (name == "expect" and value):
            return None, 0
        if name == "content-length":
            if length is not None or not value.isdigit():
                return None, 0
            length = int(value)
        if name in ADAPTER_DROPPED_HEADERS:
            continue
        rewritten.append(header)

    return ("\r\n".join(rewritten) + "\r\n\r\n").encode("ascii"), length or 0


def refuse(connection, status, message):
    """Answer the local client in plaintext.

    THE ONE THING THE PASSTHROUGH MUST NEVER DO. There the client is mid-TLS with
    whichever peer the listener reached, so any byte written to it reads as
    corruption. Here the client speaks cleartext HTTP to this process, so a
    refusal can say what happened instead of being an unexplained close.

    AND IT SAYS WHO WROTE IT. Every response the NODE composes carries
    X-Billet-Actions-Cache; these are composed here instead, so without a mark of
    their own a 400, a 403 or a 502 from this process is indistinguishable from
    the same status returned by GitHub -- which is the difference between "the
    cache is off" and "nothing reached GitHub", and a conformance gate has to be
    able to tell them apart without guessing at a list of statuses upstream may
    legitimately use.
    """
    body = (message + "\n").encode()
    head = (
        "HTTP/1.1 %d %s\r\n" % (status, ADAPTER_REFUSALS[status])
        + ADAPTER_REFUSED_HEADER + ": refused\r\n"
        + "Content-Type: text/plain; charset=utf-8\r\n"
        + "Content-Length: %d\r\n" % len(body)
        + "Connection: close\r\n\r\n"
    ).encode()
    try:
        connection.sendall(head + body)
    except OSError:
        pass


def tls_peer(connection, ca_file):
    """Wrap a connected socket in the TLS session the node (or GitHub) expects.

    ONE BUNDLE VERIFIES BOTH ENDS. The guest's bundle is the distribution roots
    plus this node's own CA, so the node's results leaf and GitHub's real
    certificate are each verified against it -- which is what makes the fail-open
    dial below a change of peer rather than a drop in assurance.
    """
    context = ssl.create_default_context(cafile=ca_file)
    context.minimum_version = ssl.TLSVersion.TLSv1_2

    try:
        return context.wrap_socket(connection, server_hostname=RESULTS_HOST)
    except BaseException:
        connection.close()
        raise


def adapter_peer(upstream, fallback, ca_file):
    """The node's authenticated tunnel, or the real origin when it cannot be had.

    THE FAIL-OPEN PATH, and it costs one substitution because this adapter
    rewrites a head and then copies bytes. GitHub answers the same v2 metadata
    call and hands back a signed blob URL on its own storage origin, which the
    client reaches directly. So a node that is restarting, unreachable, or
    refusing the session degrades a build to GitHub's cache instead of failing
    it -- the same trade the passthrough makes for the traffic that shares the
    results origin.
    """
    try:
        connection, remainder = node_tunnel(upstream)
        # Bytes before the TLS session are not the tunnel this expects. The
        # passthrough hands them to a client that is mid-handshake with the node;
        # here they would be prepended to a session this process owns.
        if remainder:
            connection.close()

            raise ConnectionError("the Billet node sent bytes before the tunnel")

        return tls_peer(connection, ca_file)
    except (ConnectionError, OSError, UnicodeError, ValueError):
        if not fallback:
            raise

    connection, _ = direct_tunnel(fallback)

    return tls_peer(connection, ca_file)


def peer_answer(peer):
    """Whatever the peer has begun answering with; b"" for nothing yet; None gone.

    THE THIRD ANSWER IS WHAT KEEPS THIS FROM SPINNING. A closed socket is readable
    forever, so "nothing to report" and "the peer has ended" cannot be the same
    value in a loop that waits on readability.

    A SERVER THAT ANSWERS BEFORE READING THE BODY IS NOT GOING TO READ IT -- the
    node refuses a malformed blob upload that way -- and writing the rest into it
    ends with an abortive close. On Linux an RST DISCARDS what the receive buffer
    already held, so continuing to write is how the answer gets lost; measured,
    the same case delivers the answer on macOS and delivers nothing in CI. So the
    write loop looks first and stops as soon as there is one.

    A READABLE SOCKET IS NOT AN ANSWER. A TLS 1.3 server sends session tickets
    right after the handshake, and those are records with no application data
    behind them -- taking them for a response would abandon the body of every
    ordinary upload. Only bytes the SSL layer yields count, which means asking it
    without blocking, since processing a ticket produces none.

    It narrows the window rather than closing it: an RST that overtakes the answer
    still loses it, and nothing on this side can order somebody else's segments.

    ANY application bytes count, including an interim 1xx, which would stop a body
    this had every reason to keep writing. That is accepted rather than handled:
    an interim response answers `Expect: 100-continue`, which this refuses on the
    way in, and the only server in this path is the node's Go one, which does not
    send them unsolicited. A client that needed one would have been refused
    already.
    """
    if not select.select([peer], [], [], 0)[0]:
        return b""

    peer.setblocking(False)
    try:
        data = peer.recv(64 * 1024)

        return data if data else None
    except WOULD_BLOCK:
        # A record with no application data behind it. Not an answer, and not the
        # end of anything: the body keeps going.
        return b""
    except OSError:
        return None
    finally:
        peer.settimeout(RELAY_TIMEOUT)


def forward_body(client, peer, prefix, length):
    """Write exactly the declared body to the peer, and say how that ended.

    THE THREE ENDINGS ARE NOT THE SAME REQUEST. Sent is the ordinary one.
    UNREAD means the peer stopped reading, which usually means it has ALREADY
    ANSWERED -- the node refuses a malformed blob upload without reading its
    gigabytes -- and that answer is the only explanation the client is going to
    get, so a failed write is not the end of the exchange and nothing is raised.
    SHORT means the body that was declared did not arrive -- the client ended, or
    stopped sending for a whole window -- and it is the one that has to be told
    apart: the peer is still waiting for bytes that can never
    arrive, so waiting for its answer would hold a node tunnel and this thread for
    the length of the node's own budget over a request that cannot complete.

    EXACTLY the declared length: a client that sent more has pipelined something
    onto a connection this told it would close, and forwarding those bytes would
    hand the node a second request nothing here has examined.

    Whatever the peer had already said comes back with the outcome, because
    reading it is how UNREAD was noticed and it is the client's whole explanation.
    """
    chunk = prefix[:length]
    remaining = length - len(chunk)
    while True:
        if chunk:
            try:
                peer.sendall(chunk)
            except OSError:
                return BODY_UNREAD, b""
        if remaining <= 0:
            return BODY_SENT, b""
        # WAIT ON BOTH, because the answer can arrive while the client is between
        # chunks and blocking on the client alone makes that pause the window in
        # which it is lost. The peer is looked at first: a client that is merely
        # slow can wait, while an answer that has arrived cannot.
        while True:
            readable, _, _ = select.select([client, peer], [], [], RELAY_TIMEOUT)
            if peer in readable:
                answered = peer_answer(peer)
                if answered is None:
                    return BODY_UNREAD, b""
                if answered:
                    return BODY_UNREAD, answered
            if client in readable:
                break
            if not readable:
                # Nothing from either side for a whole window: the body that was
                # declared is not coming, and the peer is still waiting for it.
                return BODY_SHORT, b""
            # The peer was readable and had nothing to say -- a session ticket,
            # which is consumed by the read above. Keep waiting for the client.
        chunk = client.recv(min(64 * 1024, remaining))
        if not chunk:
            return BODY_SHORT, b""
        remaining -= len(chunk)


def relay_response(peer, client, prefix=b""):
    """Copy the answer back until the peer closes.

    THE NODE MAY BE SILENT FOR A LONG TIME AND STILL BE WORKING: a finalize trims,
    unmounts, snapshots and publishes before it answers, and the node bounds that
    itself (12m45s at the time of writing). ADAPTER_IDLE is deliberately longer,
    so a slow request is ended by the node WITH A RESPONSE rather than by this
    with a dead socket. The socket's own timeout is the poll interval and stays at
    RELAY_TIMEOUT, because it is also the bound on a write to a client that has
    stopped reading -- one this cannot see the inside of, since an interrupted TLS
    write cannot be resumed.
    """
    if prefix:
        client.sendall(prefix)
    deadline = time.monotonic() + ADAPTER_IDLE
    while True:
        try:
            data = peer.recv(64 * 1024)
        except socket.timeout:
            if time.monotonic() < deadline:
                continue

            return
        if not data:
            return
        client.sendall(data)
        deadline = time.monotonic() + ADAPTER_IDLE


def handle_adapter(client, upstream, fallback, ca_file, origin):
    """Serve one request: write it all, then relay the answer.

    ONE THREAD, AND THAT IS A CORRECTNESS REQUIREMENT RATHER THAN A SIMPLIFICATION.
    A full-duplex relay would have a second thread writing this TLS socket while
    this one reads it, and an ssl.SSLSocket is NOT safe for that: CPython holds no
    lock across SSL_read and SSL_write, so the two race on one OpenSSL connection
    -- a race the interpreter's own tracker still carries and which would land as
    a corrupted upload rather than an error. The protocol here does not need
    duplex anyway: these are request-then-response exchanges with no upgrade and
    no interim status, which is exactly what the path allowlist keeps them to.
    """
    peer = None
    try:
        head, remainder = read_headers(client)
        rewritten, length = rewrite_adapter_request(head, origin)
        if rewritten is None:
            refuse(client, 403, "this endpoint serves Billet's Actions cache only")

            return
        try:
            peer = adapter_peer(upstream, fallback, ca_file)
        except (ConnectionError, OSError, UnicodeError, ValueError):
            refuse(client, 502, "Billet's Actions cache is unreachable")

            return
        # A finite timeout, not None: it bounds a write to a peer that has stopped
        # reading, and is the interval the response wait polls on.
        client.settimeout(RELAY_TIMEOUT)
        peer.settimeout(RELAY_TIMEOUT)
        try:
            peer.sendall(rewritten)
        except OSError:
            return
        outcome, answered = forward_body(client, peer, remainder, length)
        if outcome == BODY_SHORT:
            # SAID HERE RATHER THAN WAITED OUT. The peer is still waiting for
            # bytes this can no longer supply, so its answer would take the
            # node's whole budget for a request that cannot complete -- and the
            # client, which is the one that ended early, learns nothing from a
            # connection that simply stops.
            refuse(client, 400, "the request body did not arrive in full")

            return
        relay_response(peer, client, answered)
    except (ConnectionError, OSError, UnicodeError, ValueError):
        pass
    finally:
        client.close()
        if peer is not None:
            peer.close()


def handle(client, upstream, fallback):
    peer = None
    try:
        # WAIT FOR THE CLIENT'S FIRST BYTE before spending a tunnel. A bare connect
        # that sends nothing -- a health check, or a hostile step trying to burn the
        # node's listener connections -- is dropped here rather than dialed upstream;
        # MSG_PEEK leaves the ClientHello for relay to read. A real client sends it
        # immediately, so this costs nothing on the live path.
        ready, _, _ = select.select([client], [], [], CONNECT_TIMEOUT)
        if not ready or not client.recv(1, socket.MSG_PEEK):
            return
        # The node terminates TLS and decides per request whether to serve the
        # cache locally or splice the rest to GitHub. If it cannot take the tunnel
        # the client is failed OPEN to GitHub directly (direct_tunnel), never
        # answered here: the client is mid-TLS with whichever peer this reaches, so
        # any byte this process writes to it reads as TLS corruption. Only when no
        # fallback address is known -- and only then -- is a plain close the best
        # available outcome, degrading the cache call to its ordinary miss.
        try:
            peer, buffered = node_tunnel(upstream)
        except (ConnectionError, OSError, UnicodeError, ValueError):
            if not fallback:
                raise
            peer, buffered = direct_tunnel(fallback)
        # A finite timeout, not None: it bounds a sendall to a peer that has
        # stopped reading, which the idle select in relay cannot see.
        client.settimeout(RELAY_TIMEOUT)
        peer.settimeout(RELAY_TIMEOUT)
        relay(client, peer, buffered)
    except (ConnectionError, OSError, UnicodeError, ValueError):
        pass
    finally:
        client.close()
        if peer is not None:
            peer.close()


def systemd_listener():
    """Adopt the listening socket systemd bound for this activated service.

    Validated strictly: the environment must name THIS process and pass exactly
    one already-listening descriptor, or the service is misconfigured and must
    fail rather than fall back to binding a privileged port itself (which it can
    no longer do -- the capability is gone under socket activation).
    """
    if int(os.environ.get("LISTEN_PID", "0")) != os.getpid():
        raise SystemExit("LISTEN_PID does not name this process; not socket-activated")
    if int(os.environ.get("LISTEN_FDS", "0")) != 1:
        raise SystemExit("expected exactly one systemd listening socket")
    server = socket.socket(fileno=SD_LISTEN_FDS_START)
    # SO_ACCEPTCONN confirms systemd handed over a LISTENING socket, not a stray fd.
    # It is Linux-queryable (the guest); a platform that cannot report it (macOS in
    # unit tests) raises here, and there we trust the descriptor rather than refuse.
    try:
        is_listening = server.getsockopt(socket.SOL_SOCKET, socket.SO_ACCEPTCONN) == 1
    except OSError:
        is_listening = True
    if not is_listening:
        raise SystemExit("the inherited descriptor is not a listening socket")
    return server


def notify_ready():
    """Tell systemd (Type=notify) the service is up, once the listener is adopted.

    A Type=notify unit stays `activating` until this arrives, which is what makes
    `systemctl is-active` -- and the guest agent's readiness gate -- mean 'serving'
    rather than 'forked'. A failure to notify is a broken service, not swallowed.
    """
    address = os.environ.get("NOTIFY_SOCKET")
    if not address:
        return
    if address.startswith("@"):
        address = "\0" + address[1:]
    with socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM) as notifier:
        notifier.connect(address)
        notifier.sendall(b"READY=1")


def listener_origin(server):
    """The origin a signed blob URL must name, taken from the bound socket.

    READ BACK RATHER THAN DECLARED, so the port billet mints into a URL cannot
    disagree with the port this process is accepting on: there is one spelling of
    it, in the unit that binds the socket.
    """
    address = server.getsockname()
    host = address[0]
    ip = ipaddress.ip_address(host)
    # The node mints a URL naming a loopback or private address and nothing else,
    # so a listener bound anywhere else would be admitted, served, and handed a
    # URL it was told not to use. Refuse here too: this port is cleartext, and
    # the guest's docker gateway is as far as it may go -- a container builder
    # reaches it there with no network=host, and nothing outside the guest can.
    # THE NODE IS THE AUTHORITY on which addresses it will mint, and Go's
    # IsPrivate is narrower than Python's: Python counts 0.0.0.0/8 and the
    # link-local range as private, Go counts neither, and the node refuses
    # both -- so they are refused here by name, or a listener on one would be
    # served and handed a URL the node would not have minted.
    if ip.is_unspecified or ip.is_link_local or not (ip.is_loopback or ip.is_private):
        raise SystemExit("the cache adapter must listen on a loopback or private address")

    return "http://%s:%d" % (("[%s]" % host) if ip.version == 6 else host, address[1])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("passthrough", "cache-adapter"),
                        default="passthrough")
    parser.add_argument("--listen")
    parser.add_argument("--systemd-socket", action="store_true")
    parser.add_argument("--upstream", required=True)
    parser.add_argument("--fallback-addr", default="")
    parser.add_argument("--ca-file", default="")
    args = parser.parse_args()

    fallback = [addr for addr in args.fallback_addr.split(",") if addr]

    if args.mode == "cache-adapter":
        if ssl is None:
            raise SystemExit("this interpreter has no ssl module; the cache adapter cannot run")
        # BOTH ARE THE FAIL-OPEN PATH, so neither is optional. Without the trust
        # bundle nothing can verify the node's leaf; without an address resolved
        # before the guest remapped the origin there is nowhere to fail open TO,
        # and a node outage would fail the builds this adapter exists to serve.
        if not args.ca_file:
            raise SystemExit("--ca-file is required by the cache adapter")
        if not fallback:
            raise SystemExit("--fallback-addr is required by the cache adapter")
        # READ ONCE HERE AS WELL, so a bundle that is missing or is not
        # certificates fails the SERVICE rather than every request it accepts. A
        # listener that starts and then refuses everything is the shape the agent
        # cannot report on: it publishes the endpoint's URL only when this starts.
        try:
            ssl.create_default_context(cafile=args.ca_file)
        except OSError as error:
            raise SystemExit(
                "the cache adapter cannot use %s: %s" % (args.ca_file, error)) from error

    if args.systemd_socket:
        server = systemd_listener()
    else:
        if not args.listen:
            raise SystemExit("either --systemd-socket or --listen is required")
        parsed = urllib.parse.urlsplit("//" + args.listen)
        if not parsed.hostname or parsed.port is None:
            raise SystemExit("--listen needs an explicit host and port")
        server = socket.create_server((parsed.hostname, parsed.port), reuse_port=False)

    origin = listener_origin(server) if args.mode == "cache-adapter" else ""

    server.settimeout(1)
    stopping = threading.Event()

    def stop(_signum, _frame):
        stopping.set()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    if args.systemd_socket:
        notify_ready()
    while not stopping.is_set():
        try:
            client, _ = server.accept()
        except socket.timeout:
            continue
        client.settimeout(CONNECT_TIMEOUT)
        if args.mode == "cache-adapter":
            worker, arguments = handle_adapter, (
                client, args.upstream, fallback, args.ca_file, origin,
            )
        else:
            worker, arguments = handle, (client, args.upstream, fallback)
        threading.Thread(target=worker, args=arguments, daemon=True).start()
    server.close()


if __name__ == "__main__":
    main()
