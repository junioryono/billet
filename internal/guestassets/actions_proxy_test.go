package guestassets

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// The guest proxy is a transparent TLS passthrough: only the results origin is
// remapped to it, and it always tunnels to the node's authenticated cache proxy.
// The probe drives the real script's functions against in-process sockets and
// asserts the properties that make the interception safe -- the CONNECT names the
// one results authority, the node credential is forwarded, a non-200 is refused,
// and a tunnel that cannot be established closes the client WITHOUT writing to it,
// because the client is mid-TLS with the node and any byte reads as corruption.
func TestGuestActionsProxyTunnelsOnlyTheResultsOriginToTheNode(t *testing.T) {
	t.Parallel()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on this development host")
	}
	path := filepath.Join(t.TempDir(), "actions_proxy.py")
	if err := os.WriteFile(path, []byte(ActionsProxyScript), 0o600); err != nil {
		t.Fatalf("write proxy: %v", err)
	}
	probe := `
import base64
import importlib.util
import socket
import sys
import threading

# Run a target in a thread and record whether it RETURNED or raised, so a test can
# tell an intended clean exit from a thread that merely died for another reason.
def capture(target, *args):
    outcome = {}
    def run():
        try:
            target(*args)
            outcome["result"] = "returned"
        except BaseException as exc:  # noqa: BLE001 -- record it, never swallow silently
            outcome["result"] = "raised: %r" % (exc,)
    worker = threading.Thread(target=run, daemon=True)
    worker.start()
    return worker, outcome

spec = importlib.util.spec_from_file_location("billet_actions_proxy", sys.argv[1])
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)

# A fake node records the first request it is sent and replies with a fixed status.
def fake_node(status):
    server = socket.create_server(("127.0.0.1", 0))
    recorded = {}
    def serve():
        conn, _ = server.accept()
        data = b""
        while b"\r\n\r\n" not in data:
            chunk = conn.recv(4096)
            if not chunk:
                break
            data += chunk
        recorded["request"] = data
        conn.sendall(status)
        try:
            conn.recv(1)
        except OSError:
            pass
        conn.close()
    threading.Thread(target=serve, daemon=True).start()
    return server.getsockname()[1], recorded

# The CONNECT names the results authority and carries the node credential.
port, recorded = fake_node(b"HTTP/1.1 200 Connection Established\r\n\r\n")
conn, remainder = proxy.node_tunnel("http://billet:sesh@127.0.0.1:%d" % port)
assert remainder == b"", remainder
request = recorded["request"].decode()
assert request.startswith(
    "CONNECT results-receiver.actions.githubusercontent.com:443 HTTP/1.1\r\n"), request
credential = base64.b64encode(b"billet:sesh").decode()
assert ("Proxy-Authorization: Basic %s\r\n" % credential) in request, request
conn.close()

# A refusal from the node is surfaced, never treated as an open tunnel.
port, _ = fake_node(b"HTTP/1.1 403 Forbidden\r\n\r\n")
try:
    proxy.node_tunnel("http://billet:sesh@127.0.0.1:%d" % port)
    raise AssertionError("a 403 was accepted as a tunnel")
except (ConnectionError, OSError):
    pass

# An upstream that is not an http URL with an explicit port is rejected.
for bad in ["https://node:443", "http://node", "ftp://node:70"]:
    try:
        proxy.node_tunnel(bad)
        raise AssertionError("bad upstream accepted: " + bad)
    except ValueError:
        pass

# Percent-encoded credentials are decoded before they are Base64-authenticated.
port, recorded = fake_node(b"HTTP/1.1 200 Connection Established\r\n\r\n")
conn, _ = proxy.node_tunnel("http://bil%%6Cet:se%%3Ash@127.0.0.1:%d" % port)
request = recorded["request"].decode()
credential = base64.b64encode(b"billet:se:sh").decode()
assert ("Proxy-Authorization: Basic %s\r\n" % credential) in request, request
conn.close()

# relay flushes the node's buffered prefix first, then copies both directions,
# propagates a half-close, and RETURNS once both sides close -- a relay that looped
# instead would leak, so the worker is captured, joined and required to have returned.
client_a, client_b = socket.socketpair()
node_a, node_b = socket.socketpair()
# Bound every receive up front, so a regression that fails to forward hangs THIS
# probe on a 5s recv rather than the outer Go test timeout, far past the join.
client_b.settimeout(5)
node_b.settimeout(5)
worker, outcome = capture(proxy.relay, client_a, node_a, b"PREFIX")
assert client_b.recv(64) == b"PREFIX"
client_b.sendall(b"ping")
assert node_b.recv(64) == b"ping"
node_b.sendall(b"pong")
assert client_b.recv(64) == b"pong"
node_b.shutdown(socket.SHUT_WR)
client_b.settimeout(5)
assert client_b.recv(64) == b"", "relay did not propagate the node's half-close to the client"
client_b.close()
node_b.close()
worker.join(5)
assert not worker.is_alive(), "relay did not return after both sides closed"
assert outcome.get("result") == "returned", "relay exited abnormally: %s" % outcome.get("result")

# The idle teardown reaps a half-open connection: after one side ends, an idle other
# side that never closes is torn down rather than looped on forever. The relay must
# RETURN on its own -- deleting the reap branch leaves it spinning and this join times
# out -- and it must return cleanly, not raise.
saved_drain = proxy.DRAIN_IDLE
proxy.DRAIN_IDLE = 0.3
client_a, client_b = socket.socketpair()
node_a, node_b = socket.socketpair()
worker, outcome = capture(proxy.relay, client_a, node_a, b"")
node_b.shutdown(socket.SHUT_WR)  # one side ends; client_b stays open and idle
worker.join(5)
assert not worker.is_alive(), "relay did not reap the idle half-open connection"
assert outcome.get("result") == "returned", "relay exited abnormally: %s" % outcome.get("result")
proxy.DRAIN_IDLE = saved_drain
client_b.close()
node_b.close()

# A dead node with a reachable fallback fails OPEN: the client's stream is relayed
# straight to the real origin instead of the client being closed.
github_seen = {}
def fake_github():
    server = socket.create_server(("127.0.0.1", 0))
    def serve():
        conn, _ = server.accept()
        github_seen["hello"] = conn.recv(64)
        conn.sendall(b"real-github")
        conn.close()
    threading.Thread(target=serve, daemon=True).start()
    return server.getsockname()[1]

proxy.RESULTS_PORT = fake_github()
dead = socket.socket()
dead.bind(("127.0.0.1", 0))
dead_node = dead.getsockname()[1]
dead.close()
client_a, client_b = socket.socketpair()
worker, outcome = capture(proxy.handle, client_a, "http://x:y@127.0.0.1:%d" % dead_node, ["127.0.0.1"])
client_b.settimeout(5)
client_b.sendall(b"clienthello")
assert client_b.recv(64) == b"real-github", "the fallback did not relay the real origin to the client"
assert github_seen.get("hello") == b"clienthello", "the client's stream did not reach the fallback origin"
client_b.close()
worker.join(5)
assert not worker.is_alive(), "the fallback handler did not return"
assert outcome.get("result") == "returned", "handle exited abnormally: %s" % outcome.get("result")
client_b.close()

# A tunnel that cannot be established, with NO fallback, closes the client and
# writes nothing to it (the client is mid-TLS; any byte reads as corruption).
client_a, client_b = socket.socketpair()
worker, outcome = capture(proxy.handle, client_a, "http://127.0.0.1:%d" % dead_node, [])
client_b.settimeout(5)
client_b.sendall(b"clienthello")
worker.join(5)
assert not worker.is_alive(), "handle did not return after the tunnel failed"
assert outcome.get("result") == "returned", "handle exited abnormally: %s" % outcome.get("result")
assert client_b.recv(1) == b"", "the mid-TLS client was written to or left open"
client_b.close()

# A peer that stops reading must not hang a relay thread forever. The node floods
# the client, which never reads, filling the send buffer; the finite socket timeout
# the handle sets (shrunk here) is what the idle select cannot do -- it bounds the
# blocked sendall. Restoring the socket to None would hang and this join would fail.
proxy.RELAY_TIMEOUT = 0.5
def flooding_node():
    server = socket.create_server(("127.0.0.1", 0))
    def serve():
        conn, _ = server.accept()
        data = b""
        while b"\r\n\r\n" not in data:
            chunk = conn.recv(4096)
            if not chunk:
                return
            data += chunk
        conn.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
        try:
            while True:
                conn.sendall(b"x" * 65536)
        except OSError:
            pass
    threading.Thread(target=serve, daemon=True).start()
    return server.getsockname()[1]

flood_port = flooding_node()
client_a, client_b = socket.socketpair()
worker, outcome = capture(proxy.handle, client_a, "http://x:y@127.0.0.1:%d" % flood_port, [])
client_b.sendall(b"clienthello")  # pass the first-byte gate
client_b.settimeout(5)
# Prove the stall PATH was reached -- flood bytes actually crossed to the client --
# before we stop reading, so this cannot pass because handle exited early.
assert len(client_b.recv(65536)) > 0, "no flood data reached the client; the stall path was not exercised"
worker.join(8)  # stop reading: the buffer fills and the bounded sendall must fire
assert not worker.is_alive(), "a stalled sendall was not bounded by the socket timeout"
assert outcome.get("result") == "returned", "handle exited abnormally: %s" % outcome.get("result")
client_b.close()

# A bare connect that never sends is dropped WITHOUT dialing the node.
proxy.CONNECT_TIMEOUT = 1
gate = socket.create_server(("127.0.0.1", 0))
gate_dialed = {"seen": False}
def gate_serve():
    try:
        gate.settimeout(3)
        gate.accept()
        gate_dialed["seen"] = True
    except OSError:
        pass
threading.Thread(target=gate_serve, daemon=True).start()
client_a, client_b = socket.socketpair()
worker, outcome = capture(proxy.handle, client_a, "http://x:y@127.0.0.1:%d" % gate.getsockname()[1], [])
worker.join(5)
assert not worker.is_alive(), "handle blocked on a silent client"
assert outcome.get("result") == "returned", "handle exited abnormally: %s" % outcome.get("result")
assert not gate_dialed["seen"], "a silent connect still spent a node tunnel"
client_b.close()
gate.close()
`
	run := exec.CommandContext(t.Context(), python, "-c", probe, path)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("guest proxy passthrough probe: %v\n%s", err, output)
	}
}
