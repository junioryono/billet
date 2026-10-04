package guestassets

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// THE NODE RELAY HOLDS A CONNECTION WHILE THE NODE IS AWAY (#374).
//
// A node handing over leaves its guests running, and its cache listener is gone
// until the next process starts. Pointed straight at the node, a guest's `git
// fetch` in that gap is refused and fails, because nothing in Git falls back. The
// probe runs the real entry point, `--mode node-relay --upstream`, and starts the
// node only after the relay's first dial has been refused, so the answer can come
// only from a retry. It also proves the relay carries no more connections than its
// bound, stops waiting for a client that has gone, gives up on a node that never
// returns, and refuses an endpoint it cannot serve.
func TestTheNodeRelayWaitsOutANodeRestart(t *testing.T) {
	t.Parallel()

	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on this development host")
	}

	script := filepath.Join(t.TempDir(), "actions_proxy.py")
	if err := os.WriteFile(script, []byte(ActionsProxyScript), 0o600); err != nil {
		t.Fatal(err)
	}

	probe := `
import importlib.util
import socket
import sys
import threading

spec = importlib.util.spec_from_file_location("billet_actions_proxy", sys.argv[1])
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)

def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port

# The real entry point, in a thread: signal handlers are a main-thread affair, and
# the probe is the main thread.
proxy.signal.signal = lambda *_: None
proxy.NODE_RELAY_RETRY = 0.05
proxy.NODE_RELAY_WAIT = 30
proxy.NODE_RELAY_LIMIT = 1

refused = threading.Event()
real_connect = proxy.socket.create_connection
def observed_connect(address, timeout=None):
    try:
        return real_connect(address, timeout)
    except OSError:
        refused.set()
        raise
proxy.socket.create_connection = observed_connect

node_port, relay_port = free_port(), free_port()
sys.argv = ["billet-actions-proxy", "--mode", "node-relay",
            "--listen", "127.0.0.1:%d" % relay_port,
            "--upstream", "http://127.0.0.1:%d" % node_port]
threading.Thread(target=proxy.main, daemon=True).start()

def connect():
    for _ in range(100):
        try:
            return real_connect(("127.0.0.1", relay_port), 5)
        except OSError:
            threading.Event().wait(0.05)
    raise AssertionError("the relay never listened")

# 1. A NODE THAT COMES BACK IS REACHED, and only by a retry.
client = connect()
client.sendall(b"GET /v1/git/x HTTP/1.0\r\n\r\n")
assert refused.wait(10), "the relay never dialled the absent node"

# 2. THE BOUND HOLDS: with its one slot taken, a second client is closed at once.
second = connect()
second.settimeout(5)
second.sendall(b"GET / HTTP/1.0\r\n\r\n")
try:
    assert second.recv(1) == b"", "a connection past the bound was carried"
except ConnectionResetError:
    pass
second.close()

node = socket.create_server(("127.0.0.1", node_port))
def serve_node():
    conn, _ = node.accept()
    request = conn.recv(4096)
    assert request.startswith(b"GET /v1/git/x"), request
    conn.sendall(b"HTTP/1.0 200 OK\r\nContent-Length: 5\r\n\r\nhello")
    conn.close()
threading.Thread(target=serve_node, daemon=True).start()

client.settimeout(15)
answer = b""
while True:
    chunk = client.recv(4096)
    if not chunk:
        break
    answer += chunk
assert answer.endswith(b"hello"), answer
client.close()

# 3. A CLIENT THAT SENT ITS REQUEST AND HUNG UP GIVES ITS SLOT BACK while the node
# is still away, so it cannot hold the relay's bound through an outage. Its own
# relay, toward a node that is not there, with the one slot the bound allows.
if hasattr(proxy.select, "POLLRDHUP"):
    away_port, other_relay = free_port(), free_port()
    sys.argv = ["billet-actions-proxy", "--mode", "node-relay",
                "--listen", "127.0.0.1:%d" % other_relay,
                "--upstream", "http://127.0.0.1:%d" % away_port]
    threading.Thread(target=proxy.main, daemon=True).start()
    relay_port = other_relay

    refused.clear()
    gone = connect()
    gone.sendall(b"GET /v1/git/gone HTTP/1.0\r\n\r\n")
    assert refused.wait(10), "the relay never dialled the absent node"
    gone.close()
    threading.Event().wait(1)

    waiting = connect()
    waiting.sendall(b"GET /v1/git/x HTTP/1.0\r\n\r\n")
    node = socket.create_server(("127.0.0.1", away_port))
    threading.Thread(target=serve_node, daemon=True).start()
    waiting.settimeout(15)
    answer = b""
    while True:
        chunk = waiting.recv(4096)
        if not chunk:
            break
        answer += chunk
    assert answer.endswith(b"hello"), ("a client that hung up kept its slot", answer)
    waiting.close()

# 4. A CLIENT THAT HAS GONE STOPS THE WAIT at once.
slept = []
try:
    proxy.dial_node(("127.0.0.1", free_port()), wait=60, retry=1,
                    sleep=slept.append, abandoned=lambda: True)
    raise AssertionError("a dial for a client that had gone was completed")
except OSError:
    pass
assert slept == [], slept

# 5. A NODE THAT NEVER COMES BACK STILL ENDS THE WAIT.
clock = [0.0]
def now():
    return clock[0]
def sleep(seconds):
    slept.append(seconds)
    clock[0] += seconds
try:
    proxy.dial_node(("127.0.0.1", free_port()), wait=5, retry=1, now=now, sleep=sleep)
    raise AssertionError("a node that never answered was reached")
except OSError:
    pass
assert 3 <= len(slept) <= 5, slept

# 6. ONLY A PLAIN-HTTP ENDPOINT WITH A PORT.
assert proxy.node_endpoint("http://172.31.0.1:7718") == ("172.31.0.1", 7718)
for bad in ("https://172.31.0.1:7718", "http://172.31.0.1", "172.31.0.1:7718"):
    try:
        proxy.node_endpoint(bad)
        raise AssertionError("the relay accepted " + bad)
    except ValueError:
        pass
print("ok")
`
	run := exec.CommandContext(t.Context(), python, "-c", probe, script)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("node relay probe: %v\n%s", err, out)
	}
}
