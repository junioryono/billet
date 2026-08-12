package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
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
	if err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", cfg, "--out", out}); err != nil {
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

	if err := cmdCAIssue(t.Context(), []string{"--config", cfg, "--out", out, "mac-mini-1"}); err != nil {
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

	err := cmdCAIssue(t.Context(), []string{"Not A Name", "--config", cfg, "--out", filepath.Join(t.TempDir(), "b")})
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

	if err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", cfg, "--out", out}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", cfg, "--out", out})
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

	err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", path, "--out", filepath.Join(dir, "b")})
	if err == nil {
		t.Fatal("a node-only host issued a certificate")
	}

	if !strings.Contains(err.Error(), "control plane") {
		t.Errorf("the error does not say where to run this: %v", err)
	}
}

// nodeConfigFor writes the config a freshly enrolled host would have.
func nodeConfigFor(t *testing.T, name, stateDir, bundleDir string) *config.Config {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")

	body := `
node:
  name: ` + name + `
  server_addr: 10.0.0.4:7717
  provider: docker
  state_dir: ` + stateDir + `
  tls:
    cert: ` + filepath.Join(bundleDir, "node.crt") + `
    key: ` + filepath.Join(bundleDir, "node.key") + `
    ca: ` + filepath.Join(bundleDir, "ca.crt") + `
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

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("load node config: %v", err)
	}

	return cfg
}

// THE WHOLE ENROLLMENT, END TO END: issue on the control plane, configure the
// node against the copied files, and check the node arrives as the right host in
// the right deployment.
//
// This is the path the P1 broke — a node minted a random deployment identity
// that the control plane refused forever, and nothing an operator could copy
// would have fixed it.
func TestAnEnrolledNodeTakesItsIdentityFromItsBundle(t *testing.T) {
	t.Parallel()

	serverState := t.TempDir()
	serverCfg := writeCAConfig(t, serverState)
	out := filepath.Join(t.TempDir(), "bundle")

	if err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", serverCfg, "--out", out}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	nodeState := t.TempDir()
	cfg := nodeConfigFor(t, "epyc-1", nodeState, out)

	bundle, err := nodeBundle(cfg)
	if err != nil {
		t.Fatalf("the node could not load the bundle it was given: %v", err)
	}

	if bundle == nil {
		t.Fatal("a node configured with node.tls loaded no bundle")
	}

	deployment, _, err := claimNodeDeployment(cfg, bundle)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}

	want, err := os.ReadFile(filepath.Join(serverState, "deployment-id"))
	if err != nil {
		t.Fatalf("read the server's identity: %v", err)
	}

	if deployment != strings.TrimSpace(string(want)) {
		t.Errorf("the node joined deployment %q but the control plane that issued its "+
			"certificate is %q; it would be refused on every registration forever",
			deployment, strings.TrimSpace(string(want)))
	}

	// And it wrote it down, so the containers it labels stay attributable across a
	// restart.
	onDisk, err := os.ReadFile(filepath.Join(nodeState, "deployment-id"))
	if err != nil {
		t.Fatalf("the node did not record its identity: %v", err)
	}

	if strings.TrimSpace(string(onDisk)) != deployment {
		t.Errorf("the node recorded %q but is running as %q",
			strings.TrimSpace(string(onDisk)), deployment)
	}
}

// A BUNDLE FOR A DIFFERENT NODE IS CAUGHT HERE, where the file that holds it can
// be named.
//
// The control plane refuses the mismatch too, but all it can say is "you are not
// who you claim". This can say which file on this host holds the wrong
// certificate, which is the sentence an operator can act on.
func TestANodeRefusesABundleIssuedForSomebodyElse(t *testing.T) {
	t.Parallel()

	serverCfg := writeCAConfig(t, t.TempDir())
	out := filepath.Join(t.TempDir(), "bundle")

	if err := cmdCAIssue(t.Context(), []string{"mac-mini-1", "--config", serverCfg, "--out", out}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	cfg := nodeConfigFor(t, "epyc-1", t.TempDir(), out)

	_, err := nodeBundle(cfg)
	if err == nil {
		t.Fatal("a node called epyc-1 accepted a bundle issued for mac-mini-1")
	}

	if !strings.Contains(err.Error(), "node.crt") {
		t.Errorf("the error does not name the file holding the wrong certificate: %v", err)
	}
}

// A BUNDLE THAT CANNOT BE USED MUST NOT LEAVE AN IDENTITY BEHIND.
//
// The deployment a certificate names is recorded permanently, and a node refuses
// to adopt a different one afterwards — correctly, because its compute carries
// the old label. So a malformed or mixed bundle that wrote its identity and THEN
// failed validation would leave the host unable to accept the right bundle
// without an operator clearing state by hand, for an enrollment that never
// succeeded.
func TestAnUnusableBundleLeavesNoIdentityBehind(t *testing.T) {
	t.Parallel()

	serverCfg := writeCAConfig(t, t.TempDir())
	out := filepath.Join(t.TempDir(), "bundle")

	if err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", serverCfg, "--out", out}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	// A key that belongs to a different certificate, which is what half a copy
	// looks like.
	other := filepath.Join(t.TempDir(), "other")
	if err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", writeCAConfig(t, t.TempDir()), "--out", other}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(other, "node.key"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if err := os.WriteFile(filepath.Join(out, "node.key"), body, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	nodeState := t.TempDir()
	cfg := nodeConfigFor(t, "epyc-1", nodeState, out)

	if _, err := nodeBundle(cfg); err == nil {
		t.Fatal("a certificate and an unrelated key were accepted as a bundle")
	}

	if _, err := os.Stat(filepath.Join(nodeState, "deployment-id")); err == nil {
		t.Error("the failed enrollment still recorded a deployment identity, so the correct " +
			"bundle would now be refused as a conflict")
	}
}

// A NODE WITH A CERTIFICATE NEED NOT BE TOLD ITS OWN NAME.
//
// The control plane authorises a node by the name in its certificate, so
// node.name is a second place to write a fact the bundle already carries — and
// the two can disagree. Worse, the default made them disagree by accident:
// with node.name absent, config.Load filled it from the HOSTNAME, so a machine
// whose hostname is not its node name (`ip-10-0-0-5` for a node enrolled as
// `epyc-1`) got a name the control plane refuses, chosen by nobody.
//
// So a config with a bundle and no name loads, and the name comes from the
// certificate.
func TestANodeWithABundleNeedsNoName(t *testing.T) {
	t.Parallel()

	serverCfg := writeCAConfig(t, t.TempDir())
	out := filepath.Join(t.TempDir(), "bundle")

	if err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", serverCfg, "--out", out}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	cfg := nodeConfigWithoutName(t, t.TempDir(), out)

	if cfg.Node.Name != "" {
		t.Fatalf("config.Load supplied the name %q; with a bundle present the certificate "+
			"is the authority and the hostname default only ever fights it", cfg.Node.Name)
	}

	if _, err := nodeBundle(cfg); err != nil {
		t.Fatalf("loading the bundle: %v", err)
	}

	if cfg.Node.Name != "epyc-1" {
		t.Errorf("the node is called %q; the certificate says %q, and that is the name the "+
			"control plane will authorise", cfg.Node.Name, "epyc-1")
	}
}

// AND A NAME THAT CONTRADICTS THE CERTIFICATE IS STILL REFUSED. Deriving the
// name is not the same as ignoring one: an operator who wrote a name that the
// bundle disagrees with has made a mistake worth naming, and the message says
// which way out they have.
func TestANameThatContradictsTheCertificateIsRefused(t *testing.T) {
	t.Parallel()

	serverCfg := writeCAConfig(t, t.TempDir())
	out := filepath.Join(t.TempDir(), "bundle")

	if err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", serverCfg, "--out", out}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}

	cfg := nodeConfigFor(t, "mac-mini-1", t.TempDir(), out)

	_, err := nodeBundle(cfg)
	if err == nil {
		t.Fatal("a node claimed a name its certificate does not carry")
	}

	if !strings.Contains(err.Error(), "Remove node.name") {
		t.Errorf("the refusal does not say how to resolve it: %v", err)
	}
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
