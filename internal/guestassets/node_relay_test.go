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
// probe drives the real script: a client connected before the node listens gets
// the node's answer once it does, a node that never returns still ends the wait,
// and an endpoint the relay cannot serve (https, whose certificate names the
// node) is refused before anything listens.
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
import time

spec = importlib.util.spec_from_file_location("billet_actions_proxy", sys.argv[1])
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)

def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port

# 1. A NODE THAT COMES BACK IS REACHED: the dial is refused until the listener
# exists, and the client's request is answered by it.
proxy.NODE_RELAY_WAIT = 20
proxy.NODE_RELAY_RETRY = 0.1
node_port = free_port()

relay_server = socket.create_server(("127.0.0.1", 0))
relay_port = relay_server.getsockname()[1]

def serve_relay():
    client, _ = relay_server.accept()
    proxy.handle_node_relay(client, ("127.0.0.1", node_port))

threading.Thread(target=serve_relay, daemon=True).start()

client = socket.create_connection(("127.0.0.1", relay_port), 5)
client.sendall(b"GET /v1/git/x HTTP/1.0\r\n\r\n")

time.sleep(1.0)  # the node is away: every dial so far was refused
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

# 2. A NODE THAT NEVER COMES BACK STILL ENDS THE WAIT, with the failure the
# client would have had at once.
clock = [0.0]
slept = []
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

# 3. ONLY A PLAIN-HTTP ENDPOINT WITH A PORT.
assert proxy.node_endpoint("http://172.31.0.1:7718") == ("172.31.0.1", 7718)
for refused in ("https://172.31.0.1:7718", "http://172.31.0.1", "172.31.0.1:7718"):
    try:
        proxy.node_endpoint(refused)
        raise AssertionError("the relay accepted " + refused)
    except ValueError:
        pass
print("ok")
`
	run := exec.CommandContext(t.Context(), python, "-c", probe, script)
	if out, err := run.CombinedOutput(); err != nil {
		t.Fatalf("node relay probe: %v\n%s", err, out)
	}
}
