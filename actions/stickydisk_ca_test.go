package actions_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// THE STICKY DISK TRUSTS WHAT THE GUEST'S SYSTEM STORE TRUSTS. An EC2 image
// installs the cache endpoint's issuer in the distribution's bundle, and Node
// reads only the roots it was built with, so every sticky disk on an EC2 node
// with an https endpoint ran cold (2026-10-25). The library is driven by real
// Node against a TLS endpoint whose issuer is only in a stand-in system bundle;
// without the bundle the same call must fail, or the success proves nothing.
// Adding the bundle keeps what Node already trusted: an issuer named only by
// NODE_EXTRA_CA_CERTS is still trusted beside a readable bundle that lacks it,
// because an explicit `ca` replaces Node's whole default set.
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
	unrelated := filepath.Join(dir, "unrelated.crt")
	if err := os.WriteFile(unrelated, unrelatedCertificate(t), 0o600); err != nil {
		t.Fatalf("write the unrelated system bundle: %v", err)
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

	call := func(bundlePath string, env ...string) string {
		t.Helper()

		cmd := exec.CommandContext(t.Context(), node, "-e", script, lib, bundlePath, server.URL)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
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
	if got := call(unrelated, "NODE_EXTRA_CA_CERTS="+bundle); got != "ok slot=1" {
		t.Fatalf("with the issuer only in NODE_EXTRA_CA_CERTS and an unrelated system bundle the cache call "+
			"said %q, want ok slot=1: adding the bundle dropped what Node already trusted", got)
	}
	if got := call(unrelated); !strings.HasPrefix(got, "failed ") {
		t.Fatalf("with only an unrelated system bundle the cache call said %q; the case above proves nothing", got)
	}
}

// unrelatedCertificate is a self-signed CA that issued nothing the test serves.
func unrelatedCertificate(t *testing.T) []byte {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unrelated test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("self-sign: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}
