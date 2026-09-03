package guestassets

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/wirecert"
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
# The client sent a byte to pass the first-byte gate; handle closes without reading
# it, which on Linux sends a RST, so the peer sees ConnectionResetError rather than a
# clean EOF. Both are "closed with nothing written" -- what would FAIL is receiving an
# actual byte, which means the mid-TLS client was written to.
try:
    leftover = client_b.recv(1)
except ConnectionResetError:
    leftover = b""
assert leftover == b"", "the mid-TLS client was written to or left open"
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

// The passthrough is launched under systemd socket activation: PID 1 owns the
// listening socket (so it survives a service crash and needs no privileged bind
// capability) and hands it over as fd 3, and Type=notify means the service is
// "active" only after READY=1. This probe exercises the adoption contract and the
// notifier directly, without systemd.
func TestGuestActionsProxyAdoptsTheSystemdListenerAndNotifies(t *testing.T) {
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
import importlib.util
import os
import socket
import sys
import tempfile

spec = importlib.util.spec_from_file_location("billet_actions_proxy", sys.argv[1])
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)

# A listening socket handed over as fd 3, named for this process, is adopted and
# is genuinely listening -- a client connects and is accepted through it.
listener = socket.create_server(("127.0.0.1", 0))
port = listener.getsockname()[1]
os.dup2(listener.fileno(), proxy.SD_LISTEN_FDS_START)
os.environ["LISTEN_PID"] = str(os.getpid())
os.environ["LISTEN_FDS"] = "1"
adopted = proxy.systemd_listener()
# The connect-and-accept proves it is listening cross-platform (SO_ACCEPTCONN,
# which systemd_listener checks, is not queryable on every test host).
client = socket.create_connection(("127.0.0.1", port), 5)
conn, _ = adopted.accept()
client.close()
conn.close()

# A wrong LISTEN_PID, the wrong descriptor count, or a non-listening descriptor
# each FAIL closed -- the service cannot silently fall back to a privileged bind.
os.environ["LISTEN_PID"] = str(os.getpid() + 1)
try:
    proxy.systemd_listener()
    raise AssertionError("a foreign LISTEN_PID was accepted")
except SystemExit:
    pass
os.environ["LISTEN_PID"] = str(os.getpid())
os.environ["LISTEN_FDS"] = "2"
try:
    proxy.systemd_listener()
    raise AssertionError("LISTEN_FDS != 1 was accepted")
except SystemExit:
    pass
os.environ["LISTEN_FDS"] = "1"
# The non-listening rejection depends on SO_ACCEPTCONN, which the Linux guest can
# report but some test hosts (macOS) cannot; only assert it where it is queryable.
so_acceptconn_queryable = True
try:
    socket.create_server(("127.0.0.1", 0)).getsockopt(socket.SOL_SOCKET, socket.SO_ACCEPTCONN)
except OSError:
    so_acceptconn_queryable = False
if so_acceptconn_queryable:
    paired_a, paired_b = socket.socketpair()
    os.dup2(paired_a.fileno(), proxy.SD_LISTEN_FDS_START)
    try:
        proxy.systemd_listener()
        raise AssertionError("a non-listening descriptor was accepted")
    except SystemExit:
        pass
    paired_a.close()
    paired_b.close()

# notify_ready sends exactly READY=1 to NOTIFY_SOCKET, and is a no-op without one.
notify_path = os.path.join(tempfile.mkdtemp(), "notify.sock")
receiver = socket.socket(socket.AF_UNIX, socket.SOCK_DGRAM)
receiver.bind(notify_path)
os.environ["NOTIFY_SOCKET"] = notify_path
proxy.notify_ready()
receiver.settimeout(5)
assert receiver.recv(64) == b"READY=1"
receiver.close()
del os.environ["NOTIFY_SOCKET"]
proxy.notify_ready()
`
	run := exec.CommandContext(t.Context(), python, "-c", probe, path)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("guest proxy socket-activation probe: %v\n%s", err, output)
	}
}

// adapterFiles is the script under test plus the two authorities the guest's own
// trust bundle is made of.
type adapterFiles struct {
	script string
	// bundle is the node CA and the public one together, which is the shape of
	// the file the guest builds: the distribution roots with the node's CA
	// appended.
	bundle string
	// The node's results leaf, and a leaf for the SAME name from a DIFFERENT
	// authority standing in for GitHub's.
	nodeCert, nodeKey     string
	publicCert, publicKey string
}

// adapterFixture writes the proxy script and two real authorities beside it, so
// the probes can stand up a fake node that terminates TLS the way the real one
// does AND a fake origin that does not share its CA.
//
// TWO AUTHORITIES BECAUSE ONE PROVES LESS THAN IT LOOKS. The fail-open dial's
// whole claim is that it verifies GitHub against the distribution roots the guest
// bundle carries; served a leaf from the node's own CA, that path stays green
// even if the bundle lost those roots -- which is the one condition that would
// make the real dial fail. The leaves come from wirecert rather than a checked-in
// fixture because that is exactly what the node issues itself.
func adapterFixture(t *testing.T) adapterFiles {
	t.Helper()

	dir := t.TempDir()
	files := adapterFiles{
		script:     filepath.Join(dir, "actions_proxy.py"),
		bundle:     filepath.Join(dir, "bundle.pem"),
		nodeCert:   filepath.Join(dir, "node.crt"),
		nodeKey:    filepath.Join(dir, "node.key"),
		publicCert: filepath.Join(dir, "public.crt"),
		publicKey:  filepath.Join(dir, "public.key"),
	}

	node, public := issueResultsLeaf(t, dir, "node"), issueResultsLeaf(t, dir, "public")
	for path, body := range map[string][]byte{
		files.script:     []byte(ActionsProxyScript),
		files.bundle:     append(append([]byte(nil), public.ca...), node.ca...),
		files.nodeCert:   node.cert,
		files.nodeKey:    node.key,
		files.publicCert: public.cert,
		files.publicKey:  public.key,
	} {
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatalf("write %s: %v", filepath.Base(path), err)
		}
	}

	return files
}

type resultsLeaf struct{ ca, cert, key []byte }

func issueResultsLeaf(t *testing.T, parent, name string) resultsLeaf {
	t.Helper()

	dir := filepath.Join(parent, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("make the %s authority directory: %v", name, err)
	}
	ca, err := wirecert.LoadOrCreateCA(dir, "billet guest adapter test "+name)
	if err != nil {
		t.Fatalf("create the %s authority: %v", name, err)
	}
	bundle, err := ca.IssueServer([]string{"results-receiver.actions.githubusercontent.com"})
	if err != nil {
		t.Fatalf("issue the %s results leaf: %v", name, err)
	}

	return resultsLeaf{ca: ca.CertPEM(), cert: bundle.CertPEM, key: bundle.KeyPEM}
}

// The cache adapter is the plaintext half of the interception: a container-driver
// BuildKit cannot verify the node's leaf, so it speaks cleartext HTTP to this
// listener on loopback and the listener does the TLS. What has to hold is the
// rewrite -- the node answers 421 to an inner request that does not name the
// results host, and mints an https results-host blob URL unless the origin header
// says otherwise, which is the x509 failure this path exists to remove arriving
// one request later. The probe drives the real functions against real sockets and
// a real handshake against billet's own authority.
func TestGuestCacheAdapterRewritesTheHeadAndTunnelsOnlyBilletsCache(t *testing.T) {
	t.Parallel()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on this development host")
	}
	files := adapterFixture(t)
	probe := `
import importlib.util
import socket
import ssl
import sys
import threading

script, ca_file, cert_file, key_file, public_cert, public_key = sys.argv[1:7]

spec = importlib.util.spec_from_file_location("billet_actions_proxy", script)
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)

ORIGIN = "http://127.0.0.1:41321"
CACHE_PATH = "/twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry"
RESPONSE = (b"HTTP/1.1 200 OK\r\nX-Billet-Actions-Cache: local\r\n"
            b"Content-Length: 4\r\nConnection: close\r\n\r\nokay")

def capture(target, *args):
    outcome = {}
    def run():
        try:
            target(*args)
            outcome["result"] = "returned"
        except BaseException as exc:  # noqa: BLE001 -- record it, never swallow
            outcome["result"] = "raised: %r" % (exc,)
    worker = threading.Thread(target=run, daemon=True)
    worker.start()
    return worker, outcome

# A fake results endpoint. With connect=True it is the node -- CONNECT first, then
# TLS with the leaf the node issues itself; with connect=False it is GitHub, which
# the fail-open dial reaches with TLS and no tunnel.
def results_endpoint(record, connect=True):
    server = socket.create_server(("127.0.0.1", 0))
    # The NODE presents the leaf the node itself issues; GITHUB presents one from
    # an authority the node has nothing to do with, so the fail-open dial has to
    # be verifying against the whole bundle rather than against the node's CA.
    chain = (cert_file, key_file) if connect else (public_cert, public_key)
    def serve():
        conn, _ = server.accept()
        try:
            conn.settimeout(10)
            if connect:
                tunnel = b""
                while b"\r\n\r\n" not in tunnel:
                    chunk = conn.recv(4096)
                    if not chunk:
                        return
                    tunnel += chunk
                record["connect"] = tunnel
                conn.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.load_cert_chain(*chain)
            tls = context.wrap_socket(conn, server_side=True)
            data = b""
            while b"\r\n\r\n" not in data:
                chunk = tls.recv(4096)
                if not chunk:
                    return
                data += chunk
            head, body = data.split(b"\r\n\r\n", 1)
            declared = 0
            for line in head.split(b"\r\n")[1:]:
                name, _, value = line.partition(b":")
                if name.strip().lower() == b"content-length":
                    declared = int(value.strip())
            while len(body) < declared:
                chunk = tls.recv(4096)
                if not chunk:
                    break
                body += chunk
            record["head"] = head
            record["body"] = body
            tls.sendall(RESPONSE)
            tls.close()
        except OSError:
            pass
        finally:
            try:
                conn.close()
            except OSError:
                pass
    threading.Thread(target=serve, daemon=True).start()
    return server.getsockname()[1]

def drive(port, request, fallback=None):
    client_a, client_b = socket.socketpair()
    client_a.settimeout(10)
    worker, outcome = capture(
        proxy.handle_adapter, client_a, "http://billet:sesh@127.0.0.1:%d" % port,
        fallback if fallback else [], ca_file, ORIGIN)
    client_b.sendall(request)
    # READ THE ANSWER, THEN HANG UP, which is what an HTTP client told the
    # connection will close does. Waiting for EOF instead would be waiting for the
    # adapter to close, which waits for this side to end -- both stuck until the
    # drain, and the drain is not what this case is about.
    client_b.settimeout(5)
    answer = b""
    while True:
        try:
            chunk = client_b.recv(4096)
        except OSError:
            break
        if not chunk:
            break
        answer += chunk
        client_b.settimeout(0.3)
    client_b.close()
    worker.join(10)
    assert not worker.is_alive(), "handle_adapter did not return"
    assert outcome.get("result") == "returned", "handle_adapter: %s" % outcome.get("result")
    return answer

# THE HAPPY PATH. The loopback client's own Host, and any billet header it tried
# to supply for itself, are replaced; everything the node and GitHub need is kept.
record = {}
port = results_endpoint(record)
request = (
    ("POST %s HTTP/1.1\r\n" % CACHE_PATH)
    + "Host: 127.0.0.1:41321\r\n"
    + "X-Billet-Cache-Client: forged\r\n"
    + "X-Billet-Cache-Origin: http://evil.example\r\n"
    + "User-Agent: buildkit/0.27.0\r\n"
    + "Authorization: Bearer runtime-token\r\n"
    + "Content-Type: application/json\r\n"
    + "Content-Length: 13\r\n\r\n"
).encode() + b'{"key":"one"}'
answer = drive(port, request)

connect = record["connect"].decode()
assert connect.startswith(
    "CONNECT results-receiver.actions.githubusercontent.com:443 HTTP/1.1\r\n"), connect
assert "Proxy-Authorization: Basic " in connect, connect

head = record["head"].decode()
lines = head.split("\r\n")
assert lines[0] == "POST %s HTTP/1.1" % CACHE_PATH, lines[0]
assert "Host: results-receiver.actions.githubusercontent.com" in lines, head
assert not [l for l in lines if l.lower().startswith("host:")
            and l != "Host: results-receiver.actions.githubusercontent.com"], head
assert "X-Billet-Cache-Client: billet-loopback" in lines, head
assert "X-Billet-Cache-Origin: " + ORIGIN in lines, head
assert len([l for l in lines if l.lower().startswith("x-billet-cache-client:")]) == 1, head
assert len([l for l in lines if l.lower().startswith("x-billet-cache-origin:")]) == 1, head
assert "forged" not in head and "evil.example" not in head, head
assert "Connection: close" in lines, head
assert "Authorization: Bearer runtime-token" in lines, head
assert "User-Agent: buildkit/0.27.0" in lines, head
assert record["body"] == b'{"key":"one"}', record["body"]
assert b"X-Billet-Actions-Cache: local" in answer, answer
assert answer.endswith(b"okay"), answer

# THE BLOB LEG TOO. An upload is a PUT to billet's own signed path, and it is the
# leg the origin rewrite exists for -- admitting the metadata call alone would
# move the x509 failure one request later rather than removing it.
record = {}
port = results_endpoint(record)
blob = (
    "PUT /_billet/actions-cache/abc123?sig=deadbeef HTTP/1.1\r\n"
    + "Host: 127.0.0.1:41321\r\n"
    + "X-Ms-Blob-Type: BlockBlob\r\n"
    + "Content-Length: 7\r\n\r\n"
).encode() + b"payload"
answer = drive(port, blob)
lines = record["head"].decode().split("\r\n")
assert lines[0] == "PUT /_billet/actions-cache/abc123?sig=deadbeef HTTP/1.1", lines[0]
assert "Host: results-receiver.actions.githubusercontent.com" in lines, lines
assert "X-Ms-Blob-Type: BlockBlob" in lines, lines
assert record["body"] == b"payload", record["body"]
assert answer.endswith(b"okay"), answer

# A REQUEST THIS ADAPTER DOES NOT SERVE IS REFUSED WITHOUT SPENDING A TUNNEL. The
# results origin also carries ArtifactService and the runner's live-log websocket;
# a cleartext port that forwarded those would be a general entry point into an
# origin this design keeps end to end.
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

for name, raw in [
    ("artifact service",
     "POST /twirp/github.actions.results.api.v1.ArtifactService/x HTTP/1.1\r\nHost: h\r\n\r\n"),
    ("bare root", "GET / HTTP/1.1\r\nHost: h\r\n\r\n"),
    ("absolute form",
     "POST https://results-receiver.actions.githubusercontent.com%s HTTP/1.1\r\nHost: h\r\n\r\n" % CACHE_PATH),
    ("unsupported method", "DELETE %s HTTP/1.1\r\nHost: h\r\n\r\n" % CACHE_PATH),
    ("http/1.0", "POST %s HTTP/1.0\r\nHost: h\r\n\r\n" % CACHE_PATH),
    ("bare lf", "POST %s HTTP/1.1\r\nHost: h\nX-Smuggled: 1\r\n\r\n" % CACHE_PATH),
    # A PREFIX MATCH IS NOT A PATH MATCH. Each of these starts with a path the
    # adapter serves and names one it does not.
    ("dot segment escape",
     "POST /twirp/github.actions.results.api.v1.CacheService/../ArtifactService/x HTTP/1.1\r\nHost: h\r\n\r\n"),
    ("encoded dot segment",
     "POST /twirp/github.actions.results.api.v1.CacheService/%2e%2e/ArtifactService/x HTTP/1.1\r\nHost: h\r\n\r\n"),
    ("a second segment below the blob prefix",
     "GET /_billet/actions-cache/abc/../../etc HTTP/1.1\r\nHost: h\r\n\r\n"),
    ("nothing below the prefix", "POST %s HTTP/1.1\r\nHost: h\r\n\r\n" % CACHE_PATH[:-len("CreateCacheEntry")]),
    # A BODY THIS CANNOT COUNT. The relay writes the request in full and only then
    # reads the answer, which is what keeps one thread on the TLS socket, so a
    # chunked body and one held back for an interim response are refused rather
    # than guessed at. No client on this path sends either.
    ("chunked body",
     "POST %s HTTP/1.1\r\nHost: h\r\nTransfer-Encoding: chunked\r\n\r\n" % CACHE_PATH),
    ("expect 100-continue",
     "POST %s HTTP/1.1\r\nHost: h\r\nExpect: 100-continue\r\nContent-Length: 2\r\n\r\n" % CACHE_PATH),
    ("two content lengths",
     "POST %s HTTP/1.1\r\nHost: h\r\nContent-Length: 2\r\nContent-Length: 3\r\n\r\n" % CACHE_PATH),
    ("a content length that is not a number",
     "POST %s HTTP/1.1\r\nHost: h\r\nContent-Length: 2, 3\r\n\r\n" % CACHE_PATH),
    ("lone cr in the target", "POST %s\rsmuggled HTTP/1.1\r\nHost: h\r\n\r\n" % CACHE_PATH),
    ("nul in a header value", "POST %s HTTP/1.1\r\nHost: h\r\nX-A: b\x00c\r\n\r\n" % CACHE_PATH),
    ("folded header", "POST %s HTTP/1.1\r\nHost: h\r\n\tcontinued\r\n\r\n" % CACHE_PATH),
]:
    answer = drive(gate.getsockname()[1], raw.encode())
    assert answer.startswith(b"HTTP/1.1 403 "), "%s: %s" % (name, answer)
    assert b"Actions cache only" in answer, "%s: %s" % (name, answer)
    # BILLET SIGNS WHAT BILLET WROTE. Unmarked, this is indistinguishable from
    # the same status coming back from GitHub through the tunnel.
    assert b"X-Billet-Cache-Adapter: refused" in answer, "%s: %s" % (name, answer)
assert not gate_dialed["seen"], "a refused request still spent a node tunnel"
gate.close()

# FAIL OPEN. A node that cannot take the tunnel must not fail the build: the same
# rewritten head goes to the real origin over verified TLS, and its answer -- a
# signed URL on GitHub's own storage -- reaches the client unchanged.
dead = socket.socket()
dead.bind(("127.0.0.1", 0))
dead_port = dead.getsockname()[1]
dead.close()

record = {}
proxy.RESULTS_PORT = results_endpoint(record, connect=False)
answer = drive(dead_port, request, fallback=["127.0.0.1"])
assert "head" in record, \
    "the fail-open dial never completed a handshake with the public authority's leaf"
lines = record["head"].decode().split("\r\n")
assert "Host: results-receiver.actions.githubusercontent.com" in lines, lines
assert "Authorization: Bearer runtime-token" in lines, lines
assert record["body"] == b'{"key":"one"}', record["body"]
assert answer.endswith(b"okay"), answer

# ...and with nowhere to fail open to, the client is TOLD. It is not mid-TLS with
# anything -- that is the whole difference from the passthrough -- so a silent
# close would leave a build with no explanation at all.
answer = drive(dead_port, request)
assert answer.startswith(b"HTTP/1.1 502 "), answer
assert b"unreachable" in answer, answer
assert b"X-Billet-Cache-Adapter: refused" in answer, answer
`
	run := exec.CommandContext(t.Context(), python, "-c", probe,
		files.script, files.bundle, files.nodeCert, files.nodeKey, files.publicCert, files.publicKey)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("guest cache adapter probe: %v\n%s", err, output)
	}
}

// THE REQUEST IS WRITTEN IN FULL BEFORE THE ANSWER IS READ, and that ordering is
// what keeps ONE thread on the TLS socket -- an ssl.SSLSocket is not safe for a
// concurrent read and write, so a duplex relay would race two threads on one
// OpenSSL connection and land as a corrupted upload rather than an error. What
// has to hold on top of that ordering: a body the client drips out still arrives
// complete, a peer that answers early is still heard, and a node that is silent
// while it works is waited for.
func TestGuestCacheAdapterWritesTheWholeRequestBeforeItReadsTheAnswer(t *testing.T) {
	t.Parallel()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on this development host")
	}
	files := adapterFixture(t)
	probe := `
import importlib.util
import socket
import ssl
import sys
import threading
import time

script, ca_file, cert_file, key_file = sys.argv[1:5]

spec = importlib.util.spec_from_file_location("billet_actions_proxy", script)
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)

ORIGIN = "http://127.0.0.1:41321"
BLOB = "/_billet/actions-cache/abc123?sig=deadbeef"

def capture(target, *args):
    outcome = {}
    def run():
        try:
            target(*args)
            outcome["result"] = "returned"
        except BaseException as exc:  # noqa: BLE001 -- record it, never swallow
            outcome["result"] = "raised: %r" % (exc,)
    worker = threading.Thread(target=run, daemon=True)
    worker.start()
    return worker, outcome

# A node that reads the declared body, then answers -- and can be told to answer
# EARLY, before reading anything, which is what the real node does to a blob
# upload it refuses.
def fake_node(record, answer, early=False, delay=0.0, answered=None):
    server = socket.create_server(("127.0.0.1", 0))
    def serve():
        conn, _ = server.accept()
        try:
            conn.settimeout(15)
            data = b""
            while b"\r\n\r\n" not in data:
                chunk = conn.recv(4096)
                if not chunk:
                    return
                data += chunk
            conn.sendall(b"HTTP/1.1 200 Connection Established\r\n\r\n")
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.load_cert_chain(cert_file, key_file)
            tls = context.wrap_socket(conn, server_side=True)
            data = b""
            while b"\r\n\r\n" not in data:
                chunk = tls.recv(4096)
                if not chunk:
                    return
                data += chunk
            head, body = data.split(b"\r\n\r\n", 1)
            if early:
                # ANSWER WITHOUT READING THE BODY, which is what the node does to a
                # blob upload it refuses. Then DRAIN rather than close: an abortive
                # close discards what the client's receive buffer already held, so
                # a fixture that raced it would be testing the kernel, and a node
                # that stopped reading would make "the adapter stopped writing"
                # indistinguishable from "the adapter was blocked".
                tls.sendall(answer)
                record["body"] = body
                if answered is not None:
                    answered.set()
                drained = len(body)
                tls.settimeout(1.5)
                try:
                    while True:
                        chunk = tls.recv(65536)
                        if not chunk:
                            break
                        drained += len(chunk)
                except OSError:
                    pass
                record["drained"] = drained
                tls.close()
                return
            declared = 0
            for line in head.split(b"\r\n")[1:]:
                name, _, value = line.partition(b":")
                if name.strip().lower() == b"content-length":
                    declared = int(value.strip())
            while len(body) < declared:
                chunk = tls.recv(65536)
                if not chunk:
                    break
                body += chunk
            record["body"] = body
            if delay:
                time.sleep(delay)
            tls.sendall(answer)
            tls.close()
        except OSError as error:
            record["error"] = repr(error)
        finally:
            if hold is None or not hold:
                try:
                    conn.close()
                except OSError:
                    pass
    threading.Thread(target=serve, daemon=True).start()
    return server.getsockname()[1]

def request(length):
    return (
        ("PUT %s HTTP/1.1\r\n" % BLOB)
        + "Host: 127.0.0.1:41321\r\n"
        + "X-Ms-Blob-Type: BlockBlob\r\n"
        + "Content-Length: %d\r\n\r\n" % length
    ).encode()

def read_answer(sock):
    sock.settimeout(10)
    answer = b""
    while True:
        try:
            chunk = sock.recv(4096)
        except OSError:
            break
        if not chunk:
            break
        answer += chunk
        sock.settimeout(0.3)
    return answer

ANSWER = b"HTTP/1.1 201 Created\r\nContent-Length: 2\r\nConnection: close\r\n\r\nok"

# A BODY THE CLIENT DRIPS OUT ARRIVES COMPLETE. The head goes first, then the
# body in pieces with gaps, which is a client streaming a blob rather than one
# handing over a buffer -- and the adapter must keep reading to the declared
# length instead of forwarding what happened to have arrived.
record = {}
port = fake_node(record, ANSWER)
payload = b"".join(b"%06d-" % index for index in range(2000))  # ~14 KiB
client_a, client_b = socket.socketpair()
client_a.settimeout(10)
worker, outcome = capture(
    proxy.handle_adapter, client_a, "http://billet:sesh@127.0.0.1:%d" % port,
    [], ca_file, ORIGIN)
client_b.sendall(request(len(payload)))
for index in range(0, len(payload), 1024):
    client_b.sendall(payload[index:index + 1024])
    time.sleep(0.01)
answer = read_answer(client_b)
client_b.close()
worker.join(10)
assert not worker.is_alive(), "handle_adapter did not return"
assert outcome.get("result") == "returned", "handle_adapter: %s" % outcome.get("result")
assert record.get("body") == payload, \
    "the node received %d of %d body bytes" % (len(record.get("body") or b""), len(payload))
assert answer.endswith(b"ok"), answer

# EXACTLY THE DECLARED LENGTH, NOT WHATEVER ARRIVED WITH IT. A client that
# pipelines onto a connection it was told would close has sent a second request
# nothing here has examined, and forwarding those bytes hands them to the node as
# though this had.
record = {}
port = fake_node(record, ANSWER)
client_a, client_b = socket.socketpair()
client_a.settimeout(10)
worker, outcome = capture(
    proxy.handle_adapter, client_a, "http://billet:sesh@127.0.0.1:%d" % port,
    [], ca_file, ORIGIN)
client_b.sendall(request(4) + b"dataGET /_billet/actions-cache/x HTTP/1.1\r\n\r\n")
answer = read_answer(client_b)
client_b.close()
worker.join(10)
assert not worker.is_alive(), "handle_adapter did not return"
assert record.get("body") == b"data", \
    "the node was handed %r rather than the declared body" % record.get("body")
assert answer.endswith(b"ok"), answer

# A BODY THAT ENDS SHORT IS SAID SO, PROMPTLY. The peer is still waiting for bytes
# that can never arrive, so waiting for its answer would hold a node tunnel and
# this thread for the node's whole budget over a request that cannot complete --
# and the client, which is the one that ended early, would learn nothing from a
# connection that simply stops.
record = {}
port = fake_node(record, ANSWER)
client_a, client_b = socket.socketpair()
client_a.settimeout(10)
worker, outcome = capture(
    proxy.handle_adapter, client_a, "http://billet:sesh@127.0.0.1:%d" % port,
    [], ca_file, ORIGIN)
client_b.sendall(request(4096) + b"only-a-few")
client_b.shutdown(socket.SHUT_WR)
answer = read_answer(client_b)
client_b.close()
start = time.monotonic()
worker.join(10)
assert not worker.is_alive(), "handle_adapter waited on a request that could not complete"
assert time.monotonic() - start < 5, "the short body was waited out rather than refused"
assert answer.startswith(b"HTTP/1.1 400 "), answer
assert b"X-Billet-Cache-Adapter: refused" in answer, answer
assert b"did not arrive in full" in answer, answer

# A PEER THAT ANSWERS EARLY IS STILL HEARD. The node refuses a malformed blob
# upload without reading its gigabytes, so the write fails partway; treating that
# as the failure would swallow the only explanation the client is going to get.
# NO ARRANGED TIMEOUTS HERE. The adapter stops writing when the answer arrives,
# so nothing blocks and nothing is reset; the bounds this case used to need were
# the cost of letting the write run into a peer that had stopped reading.
record = {}
# THE BODY WAITS FOR THE ANSWER, so this measures the adapter rather than the
# scheduler: a fixture that started feeding immediately could have handed over
# the whole body before the node's thread ran, and called that a failure.
answered = threading.Event()
port = fake_node(record, b"HTTP/1.1 405 Method Not Allowed\r\nContent-Length: 7\r\n"
                        b"Connection: close\r\n\r\nrefused", early=True, answered=answered)
big = b"z" * (4 << 20)
client_a, client_b = socket.socketpair()
client_a.settimeout(10)
worker, outcome = capture(
    proxy.handle_adapter, client_a, "http://billet:sesh@127.0.0.1:%d" % port,
    [], ca_file, ORIGIN)
client_b.sendall(request(len(big)))
def feed():
    assert answered.wait(10), "the node never answered"
    try:
        client_b.sendall(big)
    except OSError:
        pass
threading.Thread(target=feed, daemon=True).start()
answer = read_answer(client_b)
client_b.close()
worker.join(15)
assert not worker.is_alive(), "handle_adapter did not return after an early answer"
assert answer.endswith(b"refused"), \
    "the early answer did not reach the client: %r" % answer
# ...AND THE REST OF THE BODY WAS NOT WRITTEN. A peer that has answered is not
# going to read the remaining gigabytes, and writing them is how the answer gets
# lost: the abortive close that ends it discards what this side already held.
assert record.get("drained", 0) < len(big) // 2, \
    "%d of %d body bytes went to a peer that had already answered" % (
        record.get("drained", 0), len(big))

# A CLIENT THAT STALLS IS TOLD THE SAME THING AS ONE THAT ENDED. It declared a
# body and then went quiet, so the peer is waiting for bytes that are not coming,
# and the two cases differ only in whether a FIN was sent.
proxy.RELAY_TIMEOUT = 0.3
record = {}
port = fake_node(record, ANSWER)
client_a, client_b = socket.socketpair()
client_a.settimeout(10)
worker, outcome = capture(
    proxy.handle_adapter, client_a, "http://billet:sesh@127.0.0.1:%d" % port,
    [], ca_file, ORIGIN)
client_b.sendall(request(64))  # the head, and then nothing at all
answer = read_answer(client_b)
client_b.close()
start = time.monotonic()
worker.join(10)
assert not worker.is_alive(), "handle_adapter waited on a body that never came"
assert time.monotonic() - start < 5, "the stalled body was waited out rather than refused"
assert answer.startswith(b"HTTP/1.1 400 "), answer
assert b"X-Billet-Cache-Adapter: refused" in answer, answer

# A NODE THAT IS SILENT WHILE IT WORKS IS WAITED FOR. A finalize trims, unmounts,
# snapshots and publishes before it says anything, and the node bounds that
# itself -- so the wait here spans more than one poll window rather than ending at
# the first one.
proxy.RELAY_TIMEOUT = 0.2
proxy.ADAPTER_IDLE = 3.0
record = {}
port = fake_node(record, ANSWER, delay=3 * proxy.RELAY_TIMEOUT)
client_a, client_b = socket.socketpair()
client_a.settimeout(10)
worker, outcome = capture(
    proxy.handle_adapter, client_a, "http://billet:sesh@127.0.0.1:%d" % port,
    [], ca_file, ORIGIN)
client_b.sendall(request(4) + b"data")
answer = read_answer(client_b)
client_b.close()
worker.join(10)
assert not worker.is_alive(), "handle_adapter did not return"
assert answer.endswith(b"ok"), \
    "a node that was silent for %.1fs was cut off: %r" % (3 * proxy.RELAY_TIMEOUT, answer)

# ...and a node that says nothing at ALL is not waited for forever.
proxy.ADAPTER_IDLE = 0.5
record = {}
port = fake_node(record, b"", delay=30)
client_a, client_b = socket.socketpair()
client_a.settimeout(10)
worker, outcome = capture(
    proxy.handle_adapter, client_a, "http://billet:sesh@127.0.0.1:%d" % port,
    [], ca_file, ORIGIN)
client_b.sendall(request(4) + b"data")
start = time.monotonic()
worker.join(10)
elapsed = time.monotonic() - start
assert not worker.is_alive(), "a silent node was waited on past the idle bound"
assert elapsed < 10, elapsed
client_b.close()
`
	run := exec.CommandContext(t.Context(), python, "-c", probe,
		files.script, files.bundle, files.nodeCert, files.nodeKey)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("guest cache adapter body probe: %v\n%s", err, output)
	}
}

// The origin billet mints into a signed blob URL is read back from the BOUND
// socket, so the port in that URL cannot disagree with the port this process
// accepts on. A bind anywhere but loopback or a private address (the guest's
// docker gateway, where a container builder reaches it) is refused outright:
// the node will not mint a URL naming one, so serving there would be admitted,
// served, and handed a URL it was told not to use -- on a cleartext port.
func TestGuestCacheAdapterTakesItsOriginFromTheBoundSocket(t *testing.T) {
	t.Parallel()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on this development host")
	}
	files := adapterFixture(t)
	probe := `
import importlib.util
import socket
import subprocess
import sys

script, ca_file = sys.argv[1:3]

spec = importlib.util.spec_from_file_location("billet_actions_proxy", script)
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)

# AN INTERPRETER WITHOUT _ssl RUNS THE PASSTHROUGH AND NOT THE ADAPTER. The guest
# runs this under whichever python its toolcache carries, and importing ssl at
# module scope would make one built without it take every intercepted cache call
# down with the mode it was not asked for. The import is guarded, so the module
# loads and only the adapter refuses.
assert proxy.ssl is not None, "the development host's python has no ssl module"
saved_ssl = proxy.ssl
proxy.ssl = None
try:
    sys.argv = ["billet-actions-proxy", "--mode", "cache-adapter", "--listen", "127.0.0.1:0",
                "--upstream", "http://n:1", "--fallback-addr", "127.0.0.1",
                "--ca-file", ca_file]
    try:
        proxy.main()
        raise AssertionError("the adapter started on an interpreter with no TLS")
    except SystemExit:
        pass
finally:
    proxy.ssl = saved_ssl

server = socket.create_server(("127.0.0.1", 0))
expected = "http://127.0.0.1:%d" % server.getsockname()[1]
assert proxy.listener_origin(server) == expected, proxy.listener_origin(server)
server.close()

# An IPv6 loopback listener is one too, and the brackets have to survive into the
# URL: the node parses this and refuses a host it cannot read as an address.
try:
    server = socket.create_server(("::1", 0), family=socket.AF_INET6)
except OSError:
    server = None
if server is not None:
    expected = "http://[::1]:%d" % server.getsockname()[1]
    assert proxy.listener_origin(server) == expected, proxy.listener_origin(server)
    server.close()

# The guest's docker gateway is a private address and is accepted; it is where a
# container-driver builder reaches this listener. A public or link-local bind is
# refused, and so is the unspecified address, which Python counts as private.
class Bound:
    def __init__(self, address):
        self.address = address
    def getsockname(self):
        return self.address

assert proxy.listener_origin(Bound(("172.17.0.1", 41321))) == "http://172.17.0.1:41321"
for refused in (("8.8.8.8", 41321), ("169.254.169.254", 41321), ("0.0.0.0", 41321)):
    try:
        proxy.listener_origin(Bound(refused))
        raise AssertionError("a listener on %s was accepted" % (refused,))
    except SystemExit:
        pass

# Anywhere but loopback or a private address is refused rather than served.
server = socket.create_server(("0.0.0.0", 0))
try:
    proxy.listener_origin(server)
    raise AssertionError("a listener off the loopback interface was accepted")
except SystemExit:
    pass
server.close()

# The fail-open path is not optional, so none of what it needs is: no trust
# bundle, no address to fail open to, and a bundle that is not certificates are
# each a service that does not start -- which is what keeps the agent from
# publishing an endpoint that would refuse every request it accepted.
#
# RUN AS THE REAL ENTRY POINT, with a bound. A guard that is removed does not
# raise here -- it falls through to the accept loop and serves forever -- so an
# in-process call would hang until the outer test timeout rather than fail.
base = ["--mode", "cache-adapter", "--listen", "127.0.0.1:0", "--upstream", "http://n:1"]
for name, argv in [
    ("no trust bundle", base + ["--fallback-addr", "127.0.0.1"]),
    ("nowhere to fail open to", base + ["--ca-file", ca_file]),
    ("a bundle that is not certificates",
     base + ["--fallback-addr", "127.0.0.1", "--ca-file", script]),
]:
    try:
        run = subprocess.run([sys.executable, script] + argv, capture_output=True, timeout=20)
    except subprocess.TimeoutExpired:
        raise AssertionError("the adapter served on despite %s" % name)
    assert run.returncode != 0, "%s was accepted: %s" % (name, run.stdout + run.stderr)
`
	run := exec.CommandContext(t.Context(), python, "-c", probe, files.script, files.bundle)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("guest cache adapter origin probe: %v\n%s", err, output)
	}
}
