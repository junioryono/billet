package guestassets

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// THE NPM PASSTHROUGH TUNNELS ONLY THE GUEST'S OWN PROCESSES. The real script's
// guest_process is driven against a /proc built for each verdict it can reach,
// and handle is driven with an admit that says yes and one that says no: a
// process of this user in this mount namespace is tunnelled to the node, and
// root, another namespace, an unlisted socket, a holder that cannot be found and
// a scan past either bound are each relayed to the origin's own addresses and
// never answered here.
func TestTheNpmPassthroughTunnelsOnlyTheGuestsOwnProcesses(t *testing.T) {
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
import threading

spec = importlib.util.spec_from_file_location("billet_actions_proxy", sys.argv[1])
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)

UID = os.getuid()
LISTENER = ("127.0.2.1", 443)
PEER = ("127.0.0.1", 40000)
HEADER = "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"

class Client:
    def __init__(self, local, remote):
        self.local, self.remote = local, remote
    def getsockname(self):
        return self.local
    def getpeername(self):
        return self.remote

def row(local, remote, uid, inode):
    return "   0: %s %s 01 00000000:00000000 00:00000000 00000000 %5d        0 %s 1 0 0 10 0\n" % (
        proxy.proc_address(local), proxy.proc_address(remote), uid, inode)

def fake_proc(tcp="", tcp6="", holders=(), ours="mnt:[1]"):
    root = tempfile.mkdtemp()
    os.makedirs(os.path.join(root, "self", "ns"))
    os.symlink(ours, os.path.join(root, "self", "ns", "mnt"))
    os.makedirs(os.path.join(root, "net"))
    with open(os.path.join(root, "net", "tcp"), "w") as table:
        table.write(HEADER + tcp)
    with open(os.path.join(root, "net", "tcp6"), "w") as table:
        table.write(HEADER + tcp6)
    for pid, descriptors, namespace in holders:
        os.makedirs(os.path.join(root, str(pid), "fd"))
        os.makedirs(os.path.join(root, str(pid), "ns"))
        os.symlink(namespace, os.path.join(root, str(pid), "ns", "mnt"))
        for number, target in enumerate(descriptors):
            os.symlink(target, os.path.join(root, str(pid), "fd", str(number)))
    return root

client = Client(LISTENER, PEER)
peer_row = row(PEER, LISTENER, UID, 777)

def verdict(root, who=client, now=None):
    if now is None:
        return proxy.guest_process(who, proc=root)
    return proxy.guest_process(who, proc=root, now=now)

def expect(name, got, trusted, reason):
    assert got[0] is trusted, "%s: trusted=%r, want %r (%s)" % (name, got[0], trusted, got[1])
    assert reason in got[1], "%s: the log says %r, want it to say %r" % (name, got[1], reason)

# The runner's own process, in the relay's own namespace, holding the socket.
own = fake_proc(tcp=peer_row, holders=[(100, ["pipe:[1]", "socket:[777]"], "mnt:[1]")])
expect("the guest's own process", verdict(own), True, "namespace matched")

# THE PEER'S ROW, NOT THE LISTENER'S: the same pair the other way round is the
# relay's own end of the connection and must not be taken for the client's.
reversed_only = fake_proc(tcp=row(LISTENER, PEER, UID, 777),
                          holders=[(100, ["socket:[777]"], "mnt:[1]")])
expect("only the relay's own end listed", verdict(reversed_only), False, "peer socket not listed")

# Another user's socket, root's in a guest: not this user's, so not looked at
# further. Whichever user this test runs as, the socket's is a different one.
OTHER = 0 if UID != 0 else 65534
root_owned = fake_proc(tcp=row(PEER, LISTENER, OTHER, 777), holders=[(100, ["socket:[777]"], "mnt:[1]")])
expect("another user", verdict(root_owned), False, "uid %d" % OTHER)

# This user's socket, held by a process in another mount namespace: a container's.
elsewhere = fake_proc(tcp=peer_row, holders=[(100, ["socket:[777]"], "mnt:[2]")])
expect("another mount namespace", verdict(elsewhere), False, "namespace mnt:[2] not mnt:[1]")

# Listed, but no process of this user's holds it any more: it exited, or forked
# into something this cannot read.
gone = fake_proc(tcp=peer_row, holders=[(100, ["socket:[778]"], "mnt:[1]")])
expect("the holder is gone", verdict(gone), False, "holder not found")

# Not in either table at all.
unlisted = fake_proc(holders=[(100, ["socket:[777]"], "mnt:[1]")])
expect("an unlisted socket", verdict(unlisted), False, "peer socket not listed")

# An IPv6 socket dialling the mapped address is listed in tcp6.
mapped_row = row((proxy.mapped(PEER[0]), PEER[1]), (proxy.mapped(LISTENER[0]), LISTENER[1]), UID, 777)
assert mapped_row.split()[1] == "0000000000000000FFFF00000100007F:9C40", mapped_row
via_tcp6 = fake_proc(tcp6=mapped_row, holders=[(100, ["socket:[777]"], "mnt:[1]")])
expect("an IPv6 socket on the mapped address", verdict(via_tcp6), True, "namespace matched")

# A socketpair or anything else without an address: could-not-tell.
expect("no peer address", verdict(own, who=Client("", "")), False, "no peer address")

# BOTH BOUNDS RELAY: past the descriptor count, and past the time. A directory
# is listed in no particular order, so each bound is set where the first
# descriptor already passes it, wherever the socket falls.
many = fake_proc(tcp=peer_row, holders=[(100, ["pipe:[1]"] * 5 + ["socket:[777]"], "mnt:[1]")])
saved = proxy.PEER_SCAN_DESCRIPTORS
proxy.PEER_SCAN_DESCRIPTORS = 0
expect("past the descriptor bound", verdict(many), False, "scan bound reached")
proxy.PEER_SCAN_DESCRIPTORS = saved
clock = iter([0.0] + [proxy.PEER_SCAN_SECONDS + 1.0] * 100)
expect("past the time bound", verdict(many, now=lambda: next(clock)), False, "scan bound reached")
expect("within both bounds", verdict(many), True, "namespace matched")

# A PROCESS WHOSE DESCRIPTORS CANNOT BE READ COUNTS TOO, root's and a container's
# in a guest: a guest of many such processes reaches the bound rather than being
# walked whole. Here the holder is not among them, so only the bound can end it.
unreadable = fake_proc(tcp=peer_row, holders=[(100, ["socket:[778]"], "mnt:[1]")])
for pid in range(200, 210):
    os.makedirs(os.path.join(unreadable, str(pid)))
proxy.PEER_SCAN_DESCRIPTORS = 5
expect("many processes it cannot read", verdict(unreadable), False, "scan bound reached")
proxy.PEER_SCAN_DESCRIPTORS = saved
expect("the same within the bound", verdict(unreadable), False, "holder not found")

# HANDLE: a client admit refuses goes to the origin's own addresses and the node
# is never dialled; one it admits is tunnelled.
def fake_origin(answer):
    server = socket.create_server(("127.0.0.1", 0))
    seen = {}
    def serve():
        server.settimeout(5)
        try:
            conn, _ = server.accept()
        except OSError:
            return
        data = b""
        while b"\r\n\r\n" not in data and b"hello" not in data:
            chunk = conn.recv(4096)
            if not chunk:
                break
            data += chunk
        seen["data"] = data
        conn.sendall(answer)
        conn.close()
    threading.Thread(target=serve, daemon=True).start()
    return server.getsockname()[1], seen

def run_handle(admit, node_port, fallback):
    client_a, client_b = socket.socketpair()
    client_b.settimeout(5)
    worker = threading.Thread(target=proxy.handle, args=(
        client_a, "http://x:y@127.0.0.1:%d" % node_port, fallback, "registry.npmjs.org:443", admit), daemon=True)
    worker.start()
    client_b.sendall(b"hello")
    try:
        answer = client_b.recv(64)
    except OSError:
        answer = b""
    client_b.close()
    worker.join(5)
    assert not worker.is_alive(), "handle did not return"
    return answer

import io
origin_port, origin_seen = fake_origin(b"the-registry")
node_port, node_seen = fake_origin(b"HTTP/1.1 200 Connection Established\r\n\r\nthe-node")
proxy.RESULTS_PORT = origin_port
logged = io.StringIO()
sys.stderr = logged
answer = run_handle(lambda _client: (False, "uid 0"), node_port, ["127.0.0.1"])
sys.stderr = sys.__stderr__
assert answer == b"the-registry", "a refused client was not relayed to the origin: %r" % answer
assert origin_seen.get("data") == b"hello", origin_seen
assert "data" not in node_seen, "a refused client still dialled the node: %r" % node_seen
# ONE LINE SAYS WHY, so a miss in a job can be explained from the unit's log.
assert logged.getvalue() == "relayed a connection to registry.npmjs.org:443 directly: uid 0\n", logged.getvalue()

origin_port, origin_seen = fake_origin(b"the-registry")
node_port, node_seen = fake_origin(b"HTTP/1.1 200 Connection Established\r\n\r\nthe-node")
proxy.RESULTS_PORT = origin_port
logged = io.StringIO()
sys.stderr = logged
answer = run_handle(lambda _client: (True, "namespace matched"), node_port, ["127.0.0.1"])
sys.stderr = sys.__stderr__
assert logged.getvalue() == "", "an admitted connection was logged as relayed: %r" % logged.getvalue()
assert answer == b"the-node", "an admitted client was not tunnelled to the node: %r" % answer
assert node_seen["data"].startswith(b"CONNECT registry.npmjs.org:443 HTTP/1.1\r\n"), node_seen
assert "data" not in origin_seen, "an admitted client was relayed to the origin: %r" % origin_seen

# Refused with nowhere to relay it: closed, nothing written to a client mid-TLS.
answer = run_handle(lambda _client: (False, "uid 0"), node_port, [])
assert answer == b"", "a refused client with no fallback was written to: %r" % answer
`
	run := exec.CommandContext(t.Context(), python, "-c", probe, path)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("npm passthrough peer probe: %v\n%s", err, output)
	}
}

// THE SAME DECISION AGAINST THE REAL KERNEL'S TABLES, where it has to read them:
// a separate process of this user in this mount namespace is the guest's own, and
// one of root's, or of this user's in another mount namespace, is not, the second
// for the namespace and nothing else. The peers that need privilege run where the
// host gives it (CI's guests do), and each run says which it had. It runs as an
// unprivileged user, as the relay does: as root every peer would be this user's.
func TestTheNpmPassthroughReadsTheKernelsOwnTables(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("/proc/net/tcp is Linux's")
	}
	if os.Geteuid() == 0 {
		t.Skip("the relay runs as the runner, never root, and as root every peer is this user's")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on this host")
	}
	files := peerFiles(t)
	probe := peerProbePrelude + `
server = socket.create_server(("127.0.0.1", 0))
port = server.getsockname()[1]
server.settimeout(10)

def verdict_of(prefix):
    child = subprocess.Popen(prefix + [python, peer_script, "127.0.0.1", str(port), "x"],
                             stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        accepted, _ = server.accept()
    except OSError:
        child.kill()
        _, err = child.communicate()
        return None, err.decode(errors="replace").strip()
    accepted.recv(1)
    verdict = proxy.guest_process(accepted)
    child.stdin.close()
    child.wait(10)
    accepted.close()
    return verdict, ""

# A SEPARATE PROCESS of this user in this namespace, as a step of the runner's is.
verdict, err = verdict_of([])
assert verdict is not None, "the same-user peer did not connect: " + err
assert verdict[0], "a process of this user in this namespace was not admitted: " + verdict[1]
assert "namespace matched" in verdict[1], verdict[1]
print("same user, same namespace: admitted (%s)" % verdict[1])

ran = ["same"]
if can(["sudo", "-n", "true"]):
    verdict, err = verdict_of(["sudo", "-n"])
    assert verdict is not None, "the root peer did not connect: " + err
    assert not verdict[0], "a root peer was admitted: %s" % verdict[1]
    assert verdict[1] == "uid 0", verdict[1]
    print("root: relayed (%s)" % verdict[1])
    ran.append("root")
if other_namespace:
    verdict, err = verdict_of(other_namespace)
    assert verdict is not None, "the other-namespace peer did not connect: " + err
    assert not verdict[0], "a peer in another mount namespace was admitted: %s" % verdict[1]
    assert " namespace mnt:[" in verdict[1] and " not mnt:[" in verdict[1], \
        "the other-namespace peer was relayed for another reason: " + verdict[1]
    print("this user, another mount namespace: relayed (%s)" % verdict[1])
    ran.append("namespace")
print("RAN " + " ".join(ran))
`
	output, err := exec.CommandContext(t.Context(), python, "-c", probe, files.script, files.peer).CombinedOutput()
	if err != nil {
		t.Fatalf("kernel-table probe: %v\n%s", err, output)
	}
	t.Logf("%s", strings.TrimSpace(string(output)))
	requireCIPeers(t, string(output), "RAN same root namespace")
}

// THE FLAG IS WHAT DOES IT. The real script's main runs as a passthrough of the
// registry in another mount namespace than its client, as a relay would be to a
// container's process, once with --guest-processes-only and once without: with
// it the client is relayed to the registry's own address, without it tunnelled
// to the node. That is the wiring the agent depends on, through the argument
// parser and the worker dispatch, not the functions called by hand.
func TestTheNpmPassthroughsFlagRelaysWhatItCannotVouchFor(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("/proc/net/tcp is Linux's")
	}
	if os.Geteuid() == 0 {
		t.Skip("the relay runs as the runner, never root")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is not installed on this host")
	}
	files := peerFiles(t)
	probe := peerProbePrelude + `
if not other_namespace:
    print("RAN none: no way to start a process in another mount namespace here")
    sys.exit(0)

def fake(answer):
    server = socket.create_server(("127.0.0.1", 0))
    seen = {}
    def serve():
        server.settimeout(20)
        try:
            conn, _ = server.accept()
        except OSError:
            return
        data = b""
        while b"\r\n\r\n" not in data and b"hello" not in data:
            chunk = conn.recv(4096)
            if not chunk:
                break
            data += chunk
        seen["data"] = data
        conn.sendall(answer)
        conn.close()
    threading.Thread(target=serve, daemon=True).start()
    return server.getsockname()[1], seen

def free_port():
    probe = socket.socket()
    probe.bind(("127.0.0.1", 0))
    port = probe.getsockname()[1]
    probe.close()
    return port

def through_main(extra):
    origin_port, origin_seen = fake(b"the-registry")
    node_port, node_seen = fake(b"HTTP/1.1 200 Connection Established\r\n\r\nthe-node")
    listen = free_port()
    launch = ("import importlib.util, sys\n"
              "spec = importlib.util.spec_from_file_location('p', %r)\n"
              "p = importlib.util.module_from_spec(spec)\n"
              "spec.loader.exec_module(p)\n"
              "p.RESULTS_PORT = %d\n"
              "sys.argv = ['billet-actions-proxy'] + %r\n"
              "p.main()\n") % (script, origin_port, [
                  "--listen", "127.0.0.1:%d" % listen, "--upstream", "http://x:y@127.0.0.1:%d" % node_port,
                  "--origin", "registry.npmjs.org", "--fallback-addr", "127.0.0.1"] + extra)
    relay = subprocess.Popen(other_namespace + [python, "-c", launch],
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    try:
        client = None
        for _ in range(100):
            try:
                client = socket.create_connection(("127.0.0.1", listen), 1)
                break
            except OSError:
                time.sleep(0.1)
        assert client is not None, "the relay never listened"
        client.settimeout(10)
        client.sendall(b"hello")
        answer = client.recv(64)
        client.close()
    finally:
        relay.terminate()
        _, err = relay.communicate(timeout=10)
    return answer, origin_seen, node_seen, err.decode(errors="replace")

answer, origin_seen, node_seen, logged = through_main(["--guest-processes-only"])
assert answer == b"the-registry", "with the flag, a client the relay cannot vouch for got %r" % answer
assert "data" not in node_seen, "with the flag, the node was still dialled: %r" % node_seen
assert "relayed a connection to registry.npmjs.org:443 directly: uid " in logged, logged
print("with the flag: relayed to the registry (%s)" % logged.strip().splitlines()[-1])

answer, origin_seen, node_seen, logged = through_main([])
assert answer == b"the-node", "without the flag, the client was not tunnelled to the node: %r" % answer
assert "data" not in origin_seen, "without the flag, the client went to the registry: %r" % origin_seen
print("without the flag: tunnelled to the node")
print("RAN flag")
`
	output, err := exec.CommandContext(t.Context(), python, "-c", probe, files.script, files.peer).CombinedOutput()
	if err != nil {
		t.Fatalf("flag probe: %v\n%s", err, output)
	}
	t.Logf("%s", strings.TrimSpace(string(output)))
	requireCIPeers(t, string(output), "RAN flag")
}

// peerFiles writes the real passthrough script and a peer that dials, sends a
// byte, and holds the connection open until its stdin closes, so the socket is
// listed and held while a verdict is read.
func peerFiles(t *testing.T) struct{ script, peer string } {
	t.Helper()

	dir := t.TempDir()
	files := struct{ script, peer string }{filepath.Join(dir, "actions_proxy.py"), filepath.Join(dir, "peer.py")}
	if err := os.WriteFile(files.script, []byte(ActionsProxyScript), 0o644); err != nil {
		t.Fatalf("write proxy: %v", err)
	}
	if err := os.WriteFile(files.peer, []byte(
		"import socket, sys\n"+
			"s = socket.create_connection((sys.argv[1], int(sys.argv[2])))\n"+
			"s.sendall(sys.argv[3].encode())\n"+
			"sys.stdin.read()\n"), 0o644); err != nil {
		t.Fatalf("write peer: %v", err)
	}

	return files
}

// requireCIPeers fails a run in CI that could not start the privileged peers,
// which CI's guests can, so the namespace case is never silently skipped there.
func requireCIPeers(t *testing.T, output, want string) {
	t.Helper()

	if !strings.Contains(output, want) && os.Getenv("CI") != "" {
		t.Errorf("in CI this must run %q, and this run had: %s", want, strings.TrimSpace(output))
	}
}

// peerProbePrelude loads the real script and finds how this host can start a
// process of this user in another mount namespace: through sudo, or through a
// user namespace of its own; neither leaves other_namespace empty.
const peerProbePrelude = `
import importlib.util
import os
import shlex
import socket
import subprocess
import sys
import threading
import time

script = sys.argv[1]
peer_script = sys.argv[2]
spec = importlib.util.spec_from_file_location("billet_actions_proxy", script)
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)
python = sys.executable

def can(command):
    try:
        return subprocess.run(command, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL,
                              timeout=20).returncode == 0
    except (OSError, subprocess.SubprocessError):
        return False

other_namespace = []
drop = ["setpriv", "--reuid=%d" % os.getuid(), "--regid=%d" % os.getgid(), "--clear-groups", "--"]
if can(["sudo", "-n", "unshare", "-m", "--propagation", "private"] + drop + ["true"]):
    other_namespace = ["sudo", "-n", "unshare", "-m", "--propagation", "private"] + drop
elif can(["unshare", "-U", "-m", "--map-current-user", "true"]):
    other_namespace = ["unshare", "-U", "-m", "--map-current-user"]
`
