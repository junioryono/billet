package actions_test

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// THE STICKY DISK TRUSTS WHAT THE GUEST'S SYSTEM STORE TRUSTS. An EC2 image
// installs the cache endpoint's issuer in the distribution's bundle, and Node
// reads only the roots it was built with, so every sticky disk on an EC2 node
// with an https endpoint ran cold (2026-10-25). The library is driven by real
// Node against a TLS endpoint whose issuer is only in a stand-in system bundle;
// without the bundle the same call must fail, or the success proves nothing.
func TestTheStickyDiskTrustsTheGuestSystemBundle(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed on this build host")
	}

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if _, err := w.Write([]byte(`{"slot":1,"device":"/dev/test","cold":true}`)); err != nil {
			t.Errorf("answer the cache call: %v", err)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	bundle := filepath.Join(dir, "ca-certificates.crt")
	issuer := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(bundle, issuer, 0o600); err != nil {
		t.Fatalf("write the stand-in system bundle: %v", err)
	}
	lib, err := filepath.Abs(filepath.Join("stickydisk", "lib.js"))
	if err != nil {
		t.Fatalf("resolve lib.js: %v", err)
	}
	script := `
const lib = require(process.argv[1]);
lib.systemCABundles.splice(0, lib.systemCABundles.length, process.argv[2]);
lib.cacheCall(process.argv[3], "token", "/v1/volumes", { key: "k" }).then(
  (r) => console.log("ok slot=" + r.slot),
  (e) => console.log("failed " + e.message));
`

	call := func(bundlePath string) string {
		t.Helper()

		out, err := exec.CommandContext(t.Context(), node, "-e", script, lib, bundlePath, server.URL).CombinedOutput()
		if err != nil {
			t.Fatalf("node: %v\n%s", err, out)
		}

		return strings.TrimSpace(string(out))
	}

	if got := call(bundle); got != "ok slot=1" {
		t.Fatalf("with the issuer in the system bundle the cache call said %q, want ok slot=1", got)
	}
	if got := call(filepath.Join(dir, "absent.crt")); !strings.HasPrefix(got, "failed ") {
		t.Fatalf("with no system bundle the cache call said %q; Node's own roots cannot know this issuer, "+
			"so the first case proves nothing", got)
	}
}
