package nodeplane_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
)

// oldBinaryEnv names a REAL released billet whose node speaks the released wire.
//
// The bridge is the whole point of the negotiated range, and every other test of
// it constructs the old side by hand — a RegisterRequest shaped the way a v12
// build would send one. That proves the server accepts that SHAPE. It does not
// prove a released binary produces it, which is the actual claim an operator
// relies on when they upgrade their control plane first.
//
// Set it to an extracted `billet` from a release at or before the last one that
// spoke nodeapi.MinVersion:
//
//	BILLET_OLD_BINARY=/path/to/billet_0.3.26/billet go test ./internal/nodeplane/
const oldBinaryEnv = "BILLET_OLD_BINARY"

// A RELEASED NODE REGISTERS WITH A CONTROL PLANE BUILT FROM THIS TREE.
//
// SKIPPED WITHOUT THE BINARY, deliberately, and that is a real limitation rather
// than a convenience: CI does not run this, so the bridge is proved on a
// developer's machine or not at all until the release pipeline carries an old
// binary. What it buys when it does run is the one thing no amount of
// hand-written RegisterRequests can buy — evidence that the bytes a shipped
// build puts on the wire are accepted, and recorded as the released protocol.
func TestAReleasedNodeBinaryRegistersOverTheBridge(t *testing.T) {
	binary := strings.TrimSpace(os.Getenv(oldBinaryEnv))
	if binary == "" {
		t.Skipf("set %s to a released billet to prove the bridge against a real old build",
			oldBinaryEnv)
	}

	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("%s=%s: %v", oldBinaryEnv, binary, err)
	}

	reg := &fakeRegistrar{}

	log := slog.New(slog.DiscardHandler)
	p := nodeplane.New(log, deployment, time.Minute, nodeplane.WithRegistrar(reg))

	srv := httptest.NewServer(nodeplane.Handler(log, p, &fakeStore{}, nil))
	t.Cleanup(srv.Close)

	// LOOPBACK AND NO TLS, which is billet's own single-machine shape: the trust
	// boundary is the machine, and the wire refuses to bind anywhere else without
	// a certificate. It also keeps this test free of a CA the old binary would
	// have to be given out of band.
	addr := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	// THE IDENTITY IS SEEDED RATHER THAN MINTED, because a node with no
	// certificate takes its deployment from the `server:` section of its own
	// config — and an unseeded state directory mints a random one, which this
	// control plane then correctly refuses as a foreign installation. That
	// refusal is a real check working; it is simply not the one under test, and
	// it happens AFTER the version negotiation this test exists for.
	serverState := filepath.Join(dir, "server")
	if err := os.MkdirAll(serverState, 0o700); err != nil {
		t.Fatalf("create the server state directory: %v", err)
	}

	if err := os.WriteFile(filepath.Join(serverState, "deployment-id"),
		[]byte(deployment+"\n"), 0o600); err != nil {
		t.Fatalf("seed the deployment identity: %v", err)
	}

	// The old binary parses this, so it is written in the schema THAT build
	// understands rather than whatever this tree's config package accepts now.
	body := fmt.Sprintf(`
server:
  listen: %s
  state_dir: %s
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: %s
node:
  name: old-node
  server_addr: %s
  provider: docker
  state_dir: %s
  lock_dir: %s
  max_vcpu: 4
  max_memory: 16GiB
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`, addr, serverState, filepath.Join(dir, "key.pem"), addr,
		filepath.Join(dir, "node"), filepath.Join(dir, "locks"))

	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the old binary's config: %v", err)
	}

	// A key the old binary can parse if it looks. It never reaches GitHub here —
	// the node half does not authenticate to the App — but config validation may
	// insist the path exists and is a real key.
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})

	if err := os.WriteFile(filepath.Join(dir, "key.pem"), pemBytes, 0o600); err != nil {
		t.Fatalf("write a private key: %v", err)
	}

	cmd := exec.CommandContext(t.Context(), binary, "node", "--config", cfgPath)

	var out strings.Builder

	cmd.Stdout, cmd.Stderr = &out, &out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start the old node: %v", err)
	}

	t.Cleanup(func() {
		if cmd.Process != nil {
			//nolint:errcheck // Killing a process that has already exited on its own
			// -- which is the ordinary outcome when it was refused -- fails, and
			// there is nothing to do about it either way.
			_ = cmd.Process.Kill()
		}

		//nolint:errcheck // The node is being killed, so a non-zero wait is the
		// expected result rather than a problem to report.
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(60 * time.Second)

	for reg.lastRegistration().Name == "" {
		if time.Now().After(deadline) {
			t.Fatalf("a released node never registered with this control plane.\n"+
				"Its output was:\n%s", out.String())
		}

		time.Sleep(100 * time.Millisecond)
	}

	got := reg.lastRegistration()

	// THE RELEASED PROTOCOL, NOT THIS BUILD'S. That is the bridge: an old node is
	// served at the version it speaks, and the fleet keeps working while the
	// control plane runs ahead of it.
	if got.WireVersion != nodeapi.MinVersion {
		t.Errorf("a released node negotiated protocol %d, want %d — the bridge is not "+
			"serving the version that build speaks", got.WireVersion, nodeapi.MinVersion)
	}

	// AND ITS RANGE IS RECORDED AS EXACTLY ONE VERSION. A released build declares
	// no floor, and reading that absence as zero is the defect DeclaredRange
	// exists to prevent — this is the only test where the absent field comes from
	// a real build rather than a struct literal.
	if got.WireMin != nodeapi.MinVersion || got.WireMax != nodeapi.MinVersion {
		t.Errorf("a released node's range was recorded as %d-%d, want %d-%d",
			got.WireMin, got.WireMax, nodeapi.MinVersion, nodeapi.MinVersion)
	}

	if got.Release != "" {
		t.Errorf("a released node predating the release field reported %q", got.Release)
	}

	if got.Provider != config.ProviderDocker {
		t.Errorf("provider reached the ledger as %q", got.Provider)
	}
}
