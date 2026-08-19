package guestassets

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGuestActionsProxyFallsBackBeforeAcceptingTheClientTunnel(t *testing.T) {
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
import sys

spec = importlib.util.spec_from_file_location("billet_actions_proxy", sys.argv[1])
proxy = importlib.util.module_from_spec(spec)
spec.loader.exec_module(proxy)

def unavailable(_upstream, _authority):
    raise OSError("node proxy is down")

calls = []
proxy.intercepted_connection = unavailable
proxy.direct_connection = lambda host, port: (calls.append((host, port)) or "direct", b"")
connection, buffered = proxy.connect("http://node.invalid:7718", proxy.RESULTS_HOST, 443, proxy.RESULTS_HOST + ":443")
assert connection == "direct"
assert buffered == b""
assert calls == [(proxy.RESULTS_HOST, 443)]

calls.clear()
connection, buffered = proxy.connect("http://node.invalid:7718", "github.com", 443, "github.com:443")
assert connection == "direct"
assert calls == [("github.com", 443)]
`
	run := exec.CommandContext(t.Context(), python, "-c", probe, path)
	if output, err := run.CombinedOutput(); err != nil {
		t.Fatalf("guest proxy fail-open probe: %v\n%s", err, output)
	}
}
