package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/wirecert"
)

// THE CLI LAYER HAD NO TEST, AND BOTH ITS BUGS LIVED THERE.
//
// `billet ca issue <node>` could not run at all: the shared flag parser rejects
// positional arguments, which is right for every other command and wrong for
// this one. And once that was fixed, `billet ca issue epyc-1 --config x.yaml`
// silently ignored the config path, because Go's flag package stops parsing at
// the first positional — so the command read the DEFAULT config file and issued
// against whatever deployment that named.
//
// Neither is reachable from the packages underneath, both are on the path an
// operator takes exactly once per node, and the failure of the second is silent.
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

func TestCAIssueWritesAUsableBundle(t *testing.T) {
	t.Parallel()

	state := t.TempDir()
	cfg := writeCAConfig(t, state)
	out := filepath.Join(t.TempDir(), "bundle")

	// The order an operator writes: the subject first, the options after.
	if err := cmdCAIssue([]string{"epyc-1", "--config", cfg, "--out", out}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	bundle, err := wirecert.LoadBundle(
		filepath.Join(out, "node.crt"),
		filepath.Join(out, "node.key"),
		filepath.Join(out, "ca.crt"))
	if err != nil {
		t.Fatalf("the bundle it wrote does not load: %v", err)
	}

	name, err := bundle.NodeName()
	if err != nil {
		t.Fatalf("node name: %v", err)
	}

	if name != "epyc-1" {
		t.Errorf("the bundle names node %q, want epyc-1; the control plane authorises by this",
			name)
	}

	// THE DEPLOYMENT MUST MATCH THE CONTROL PLANE THAT ISSUED IT. This is the
	// silent half of the flag bug: a --config that was ignored would issue from
	// the default config's authority, and the certificate would name a deployment
	// this server has never heard of. The node would be refused on arrival, with
	// nothing on either side pointing at the flag that caused it.
	got, err := bundle.Deployment()
	if err != nil {
		t.Fatalf("deployment: %v", err)
	}

	want, err := os.ReadFile(filepath.Join(state, "deployment-id"))
	if err != nil {
		t.Fatalf("read the server's identity: %v", err)
	}

	if got != strings.TrimSpace(string(want)) {
		t.Errorf("the bundle names deployment %q but this control plane is %q; --config was "+
			"not honoured, so it was issued from another installation's authority",
			got, strings.TrimSpace(string(want)))
	}
}

// Flags before the subject work too, because that is what a script writes.
func TestCAIssueAcceptsFlagsBeforeTheNode(t *testing.T) {
	t.Parallel()

	cfg := writeCAConfig(t, t.TempDir())
	out := filepath.Join(t.TempDir(), "bundle")

	if err := cmdCAIssue([]string{"--config", cfg, "--out", out, "mac-mini-1"}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	if _, err := os.Stat(filepath.Join(out, "node.crt")); err != nil {
		t.Errorf("no certificate was written: %v", err)
	}
}

// A NODE NAME THE SERVER WOULD NEVER ACCEPT IS REFUSED HERE, rather than after
// an operator has copied the bundle and restarted a host to find out.
func TestCAIssueRefusesAnUnusableNodeName(t *testing.T) {
	t.Parallel()

	cfg := writeCAConfig(t, t.TempDir())

	err := cmdCAIssue([]string{"Not A Name", "--config", cfg, "--out", filepath.Join(t.TempDir(), "b")})
	if err == nil {
		t.Fatal("a certificate was issued for a name that is not a legal node name")
	}
}

// Re-issuing over a live node's directory is refused, and the message says what
// to do instead.
func TestCAIssueWillNotOverwriteABundle(t *testing.T) {
	t.Parallel()

	cfg := writeCAConfig(t, t.TempDir())
	out := filepath.Join(t.TempDir(), "bundle")

	if err := cmdCAIssue([]string{"epyc-1", "--config", cfg, "--out", out}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	err := cmdCAIssue([]string{"epyc-1", "--config", cfg, "--out", out})
	if err == nil {
		t.Fatal("a second bundle was written over the first")
	}

	if !strings.Contains(err.Error(), "--out") {
		t.Errorf("the error does not say how to proceed: %v", err)
	}
}

// A HOST WITH NO SERVER SECTION HOLDS NO AUTHORITY, and saying so beats
// minting one in a node's state directory that nothing will ever verify
// against.
func TestCAIssueRefusesOnAHostWithNoControlPlane(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")

	body := `
node:
  name: host-1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ` + t.TempDir() + `
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

	err := cmdCAIssue([]string{"epyc-1", "--config", path, "--out", filepath.Join(dir, "b")})
	if err == nil {
		t.Fatal("a node-only host issued a certificate")
	}

	if !strings.Contains(err.Error(), "control plane") {
		t.Errorf("the error does not say where to run this: %v", err)
	}
}
