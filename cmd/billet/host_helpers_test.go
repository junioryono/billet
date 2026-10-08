package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
)

// Helpers cmd/billet's tests share with internal/ops/host's, copied rather
// than imported: a test helper is not part of any package's API.

// writeCAConfig writes a control-plane config with one docker tier, the shape
// the CA and status commands are run against.
func writeCAConfig(t *testing.T, stateDir string) string {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /tmp/key.pem
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	return path
}

// nodeConfigWithoutName is nodeConfigFor with the name left to the certificate.
func nodeConfigWithoutName(t *testing.T, stateDir, bundleDir string) *config.Config {
	t.Helper()

	path := filepath.Join(t.TempDir(), "billet.yaml")

	body := `
node:
  server_addr: 10.0.0.4:7717
  provider: docker
  state_dir: ` + stateDir + `
  tls:
    cert: ` + filepath.Join(bundleDir, "node.crt") + `
    key: ` + filepath.Join(bundleDir, "node.key") + `
    ca: ` + filepath.Join(bundleDir, "ca.crt") + `
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("a node config that leaves its name to the certificate was refused: %v", err)
	}

	return cfg
}

// notifySocket is a datagram socket standing in for systemd's, at a path short
// enough for a Unix address.
func notifySocket(t *testing.T) (string, *net.UnixConn) {
	t.Helper()

	dir := t.TempDir()
	digest := sha256.Sum256([]byte(dir))
	shortDir := "/tmp/bn-" + hex.EncodeToString(digest[:6])
	if err := os.Symlink(dir, shortDir); err != nil {
		t.Fatalf("create short readiness path: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(shortDir) })
	path := filepath.Join(shortDir, "notify.sock")
	listener, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatalf("listen for readiness: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	return path, listener
}

// readNotification is the one message the socket received.
func readNotification(t *testing.T, listener *net.UnixConn) string {
	t.Helper()

	if err := listener.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set readiness deadline: %v", err)
	}

	message := make([]byte, 64)
	n, _, err := listener.ReadFromUnix(message)
	if err != nil {
		t.Fatalf("read readiness: %v", err)
	}

	return string(message[:n])
}

// capture redirects stdout for the duration of fn and returns what was written.
//
// `billet check` REPORTS to an operator, so what it prints is the whole product
// and asserting only its error return would leave the interesting half untested.
func capture(t *testing.T, fn func()) string {
	t.Helper()

	saved := os.Stdout

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}

	os.Stdout = w

	// RESTORED BY CLEANUP, not only by the happy path below. A t.Fatal inside fn
	// unwinds past the restore, leaving every later test in the package writing
	// into a pipe nobody reads — which surfaces as an unrelated test hanging or
	// losing its output, a long way from the test that actually failed.
	t.Cleanup(func() { os.Stdout = saved })

	done := make(chan string, 1)

	go func() {
		var b strings.Builder

		_, _ = io.Copy(&b, r) //nolint:errcheck // the write end is closed below, ending the copy

		done <- b.String()
	}()

	fn()

	os.Stdout = saved

	if err := w.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}

	return <-done
}

// stubGitHubUnverifiable points cmdCheck's App verification at a fake that
// always answers 502, which classifies as UNVERIFIABLE — the advisory band —
// so a test about some other subsystem proceeds past the github line without
// ever reaching the real GitHub. A unit test must never depend on api.github.com.
func stubGitHubUnverifiable(t *testing.T) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)

	prev := app.GitHubAPIBase
	app.GitHubAPIBase = srv.URL
	t.Cleanup(func() { app.GitHubAPIBase = prev })
}
