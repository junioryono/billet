#!/usr/bin/env python3
"""Fail-open guest forward proxy for Billet's selective Actions interception."""

import argparse
import base64
import select
import signal
import socket
import threading
import urllib.parse

RESULTS_HOST = "results-receiver.actions.githubusercontent.com"
HEADER_LIMIT = 64 * 1024
CONNECT_TIMEOUT = 10


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


def parse_authority(authority):
    parsed = urllib.parse.urlsplit("//" + authority)
    if not parsed.hostname or parsed.port is None:
        raise ValueError("CONNECT needs an explicit host and port")
    return parsed.hostname, parsed.port


def direct_connection(host, port):
    return socket.create_connection((host, port), CONNECT_TIMEOUT), b""


def intercepted_connection(upstream, authority):
    parsed = urllib.parse.urlsplit(upstream)
    if parsed.scheme != "http" or not parsed.hostname or parsed.port is None:
        raise ValueError("the Billet proxy must be an http URL with an explicit port")
    connection = socket.create_connection((parsed.hostname, parsed.port), CONNECT_TIMEOUT)
    try:
        headers = [
            f"CONNECT {authority} HTTP/1.1",
            f"Host: {authority}",
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
            raise ConnectionError("the Billet proxy refused CONNECT")
        return connection, remainder
    except Exception:
        connection.close()
        raise


def connect(upstream, host, port, authority):
    if host == RESULTS_HOST and port == 443:
        try:
            return intercepted_connection(upstream, authority)
        except (ConnectionError, OSError, ValueError):
            pass
    return direct_connection(host, port)


def relay(client, remote, remote_buffer):
    if remote_buffer:
        client.sendall(remote_buffer)
    peers = {client: remote, remote: client}
    while peers:
        readable, _, _ = select.select(list(peers), [], [], 60)
        if not readable:
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


def handle(client, upstream):
    remote = None
    try:
        client.settimeout(CONNECT_TIMEOUT)
        request, remainder = read_headers(client)
        if remainder:
            raise ValueError("CONNECT carried an unexpected body")
        first = request.split(b"\r\n", 1)[0].decode("ascii")
        method, authority, version = first.split()
        if method != "CONNECT" or version not in ("HTTP/1.0", "HTTP/1.1"):
            raise ValueError("only CONNECT is supported")
        host, port = parse_authority(authority)
        remote, remote_buffer = connect(upstream, host, port, authority)
        client.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        client.settimeout(None)
        remote.settimeout(None)
        relay(client, remote, remote_buffer)
    except (ConnectionError, OSError, UnicodeError, ValueError):
        try:
            client.sendall(b"HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n")
        except OSError:
            pass
    finally:
        client.close()
        if remote is not None:
            remote.close()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--listen", required=True)
    parser.add_argument("--upstream", required=True)
    args = parser.parse_args()
    listen_host, listen_port = parse_authority(args.listen)

    server = socket.create_server((listen_host, listen_port), reuse_port=False)
    server.settimeout(1)
    stopping = threading.Event()

    def stop(_signum, _frame):
        stopping.set()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    while not stopping.is_set():
        try:
            client, _ = server.accept()
        except socket.timeout:
            continue
        threading.Thread(target=handle, args=(client, args.upstream), daemon=True).start()
    server.close()


if __name__ == "__main__":
    main()
