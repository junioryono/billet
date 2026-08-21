#!/usr/bin/env python3
"""Transparent TLS passthrough for Billet's Actions cache interception.

Only the Actions results origin is redirected here, by a guest DNS remap
(results-receiver.actions.githubusercontent.com -> this listener). Every other
destination resolves normally and never touches this process, so bulk transfers
-- action tarballs, toolchains, artifact blob uploads -- go direct and are not
funnelled through a single Python relay. That routing is the whole point: an
earlier design proxied ALL of the runner's HTTPS through here, and large
transfers stalled and corrupted while small cache calls survived.

This listener does not terminate TLS. It accepts the client's raw connection,
opens the authenticated CONNECT tunnel to the node, and copies bytes both ways.
The node terminates TLS with its own results-receiver leaf and decides, per
request, whether to serve the cache locally or splice the rest upstream to
GitHub. The client's TLS session is therefore end to end with the node, which is
what lets the node present a certificate the guest already trusts.
"""

import argparse
import base64
import os
import select
import signal
import socket
import threading
import time
import urllib.parse

RESULTS_AUTHORITY = "results-receiver.actions.githubusercontent.com:443"
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


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen")
    parser.add_argument("--systemd-socket", action="store_true")
    parser.add_argument("--upstream", required=True)
    parser.add_argument("--fallback-addr", default="")
    args = parser.parse_args()

    fallback = [addr for addr in args.fallback_addr.split(",") if addr]

    if args.systemd_socket:
        server = systemd_listener()
    else:
        if not args.listen:
            raise SystemExit("either --systemd-socket or --listen is required")
        parsed = urllib.parse.urlsplit("//" + args.listen)
        if not parsed.hostname or parsed.port is None:
            raise SystemExit("--listen needs an explicit host and port")
        server = socket.create_server((parsed.hostname, parsed.port), reuse_port=False)

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
        threading.Thread(
            target=handle, args=(client, args.upstream, fallback), daemon=True
        ).start()
    server.close()


if __name__ == "__main__":
    main()
