package main

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/state"
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

	serverState := t.TempDir()
	cfg := writeCAConfig(t, serverState)
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

	want, err := os.ReadFile(filepath.Join(serverState, "deployment-id"))
	if err != nil {
		t.Fatalf("read the server's identity: %v", err)
	}

	if got != strings.TrimSpace(string(want)) {
		t.Errorf("the bundle names deployment %q but this control plane is %q; --config was "+
			"not honoured, so it was issued from another installation's authority",
			got, strings.TrimSpace(string(want)))
	}
}

// A BUNDLE ISSUED DURING A ROTATION MUST BE ABLE TO REACH THE CONTROL PLANE
// THAT ISSUED IT, which is not what the issuing authority alone gives it.
//
// A rotation is an overlap: the NEW authority issues node certificates and the
// OLD one signs what the control plane PRESENTS, because a node that has not
// renewed trusts only the old one. So a bundle whose ca.crt carries only the
// issuer cannot verify the server it was just enrolled against — the one machine
// a rotation is supposed not to strand, stranded by the command an operator runs
// to add it. The wire's enroll and renew responses have always carried the whole
// trust bundle; this is the out-of-band path, and it did not.
//
// DRIVEN THROUGH cmdCAIssue rather than through wirecert, deliberately. The
// property that broke is which authority the COMMAND hands out, and every piece
// underneath it was already correct on its own.
func TestCAIssueDuringARotationWritesABundleThatCanVerifyTheServer(t *testing.T) {
	t.Parallel()

	serverState := t.TempDir()
	cfg := writeCAConfig(t, serverState)

	deployment, err := state.DeploymentID(serverState)
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}

	if _, err := wirecert.LoadOrCreateCA(serverState, deployment); err != nil {
		t.Fatalf("create the authority: %v", err)
	}

	if _, err := wirecert.Rotate(serverState, deployment); err != nil {
		t.Fatalf("rotate: %v", err)
	}

	out := filepath.Join(t.TempDir(), "bundle")
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

	// The node's own side of the wire, built exactly as `billet node` builds it:
	// it verifies the leaf against the authority beside it, so this already
	// proves the bundle is internally coherent.
	if _, err := wirecert.ClientTLS(bundle); err != nil {
		t.Fatalf("the bundle does not hold together: %v", err)
	}

	// AND AGAINST WHAT THE SERVER WOULD ACTUALLY PRESENT. That is the assertion
	// the bug survives: the certificate and its authority agreed with each other
	// and with nothing this control plane serves.
	authority, err := wirecert.LoadServing(serverState, deployment)
	if err != nil {
		t.Fatalf("load serving: %v", err)
	}

	serving, err := authority.Presents.IssueServer([]string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("issue the serving certificate: %v", err)
	}

	leaf, err := wirecert.LeafOf(serving)
	if err != nil {
		t.Fatalf("parse the serving certificate: %v", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle.CAPEM) {
		t.Fatal("the bundle's ca.crt could not be parsed")
	}

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: pool, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("a node enrolled during the overlap cannot verify the control plane that "+
			"issued its bundle, so it drops off the wire it would need to recover: %v", err)
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

// THE WIRE'S AUTHORITY BELONGS TO THE DEPLOYMENT, NOT TO THE HOSTNAME.
//
// serveNodeWire's third parameter is the deployment id, and every mTLS
// deployment turns on it: the CA is minted with the deployment in its
// Organization, a node reads its own deployment out of the certificate it is
// given, and Plane.Register refuses a node whose deployment is not this one's.
//
// Handing it the hostname instead breaks both boot orders and neither says why.
// Server first: the CA carries the hostname, so every node certificate does too,
// the enrolled node cannot even parse it into a deployment id — the format is 32
// hex characters and a hostname is not — and if it could, the plane would refuse
// it forever as foreign. CLI first: `billet ca issue` mints against the real id,
// and then the control plane will not start at all, because parseCA refuses an
// authority issued for something else.
//
// Nothing else catches it. A loopback listener skips the whole block, which is
// every local run and, until this test, every test.
func TestTheNodeWireMintsItsAuthorityForTheDeployment(t *testing.T) {
	stateDir := t.TempDir()

	deploymentID, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}

	// ":0" is the wildcard — every interface, which is exactly the case that
	// requires certificates. A loopback address would serve plain HTTP and mint
	// nothing.
	cfg := &config.Config{Server: &config.ServerConfig{
		Listen: ":0", IdentityDir: stateDir, NodeTLSHosts: []string{"billet.example"},
	}}

	stop, err := serveNodeWire(t.Context(), cfg,
		nodeplane.New(slog.New(slog.DiscardHandler), deploymentID, time.Minute),
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("serving the node wire on a network address: %v", err)
	}

	t.Cleanup(stop.stop)

	// The authority on disk has to be one this deployment can load. It is the
	// same call `billet ca issue` and `billet nodes approve` make, so an
	// authority that fails here is one no node can ever be admitted against.
	if _, err := wirecert.LoadOrCreateCA(stateDir, deploymentID); err != nil {
		t.Fatalf("the wire minted an authority this deployment cannot use: %v", err)
	}
}

// AND IT ACCEPTS THE ONE THE CLI ALREADY MINTED, which is the other boot order:
// an operator runs `billet ca issue` for the first node before ever starting the
// control plane on an address nodes can reach.
func TestTheNodeWireAcceptsTheAuthorityTheCLIMinted(t *testing.T) {
	stateDir := t.TempDir()

	deploymentID, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}

	// What `billet ca issue` does before the server has ever run.
	if _, err := wirecert.LoadOrCreateCA(stateDir, deploymentID); err != nil {
		t.Fatalf("minting the authority: %v", err)
	}

	cfg := &config.Config{Server: &config.ServerConfig{
		Listen: ":0", IdentityDir: stateDir, NodeTLSHosts: []string{"billet.example"},
	}}

	stop, err := serveNodeWire(t.Context(), cfg,
		nodeplane.New(slog.New(slog.DiscardHandler), deploymentID, time.Minute),
		nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatalf("the control plane refused the authority its own CLI minted: %v", err)
	}

	t.Cleanup(stop.stop)
}

// AN INTERRUPTED ENROLLMENT COMES BACK AS THE SAME REQUEST.
//
// The name is claimed by the first key to ask, so a machine that loses its key
// while waiting for a human and generates another is a SECOND key asking for a
// name that is already taken — and the control plane is right to refuse it. The
// key therefore has to outlive the process that made it.
func TestAnInterruptedEnrollmentReusesItsStagedKey(t *testing.T) {
	dir := t.TempDir()
	tls := &config.NodeTLS{
		CertPath: filepath.Join(dir, "node.crt"),
		KeyPath:  filepath.Join(dir, "node.key"),
		CAPath:   filepath.Join(dir, "ca.crt"),
	}

	firstCSR, firstKey, err := pendingIdentity(tls, "epyc-1")
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	// The process dies here, and the operator starts it again.
	secondCSR, secondKey, err := pendingIdentity(tls, "epyc-1")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}

	if !bytes.Equal(firstKey, secondKey) {
		t.Error("a retry generated a new private key, so it is asking for a name the first " +
			"attempt already claimed and will be refused forever")
	}

	if !bytes.Equal(firstCSR, secondCSR) {
		t.Error("a retry presents a different request, so the fingerprint the operator was " +
			"shown describes nothing")
	}

	// The staged key is a secret.
	info, err := os.Stat(filepath.Join(dir, "pending.key"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("the staged private key is mode %04o", got)
	}

	// And once it becomes a bundle, the spare copy goes.
	clearPendingIdentity(tls)

	if _, err := os.Stat(filepath.Join(dir, "pending.key")); !os.IsNotExist(err) {
		t.Error("the staged key outlived the enrollment, leaving a second copy of a private key")
	}
}

// A STAGED KEY BELONGS TO ONE NODE NAME.
//
// The control plane derives a certificate's subject from the REQUEST, not from
// the CSR — deliberately, so a node cannot rename itself by editing what it
// asks for. The consequence is that reusing a staged key after node.name changed
// would let ONE key collect certificates for two names, and revoking one of them
// would leave the other working.
func TestAStagedEnrollmentRefusesADifferentNodeName(t *testing.T) {
	dir := t.TempDir()
	tls := &config.NodeTLS{
		CertPath: filepath.Join(dir, "node.crt"),
		KeyPath:  filepath.Join(dir, "node.key"),
		CAPath:   filepath.Join(dir, "ca.crt"),
	}

	if _, _, err := pendingIdentity(tls, "epyc-1"); err != nil {
		t.Fatalf("stage: %v", err)
	}

	_, _, err := pendingIdentity(tls, "epyc-2")
	if err == nil {
		t.Fatal("a key staged for epyc-1 was reused to ask for epyc-2; one compromised key " +
			"would hold two identities and revoking one would not reach the other")
	}

	if !strings.Contains(err.Error(), "epyc-1") {
		t.Errorf("the refusal does not name the enrollment in the way: %v", err)
	}
}

// AND A STAGED KEY ANYONE CAN READ IS NOT USED.
//
// It is the identity this machine is about to be approved for. A backup that
// restored it 0644, or a directory somebody else can write, means the future
// node identity is already somebody else's — and quietly continuing with it is
// how that becomes their runner.
func TestAStagedEnrollmentRefusesAWorldReadableKey(t *testing.T) {
	dir := t.TempDir()
	tls := &config.NodeTLS{
		CertPath: filepath.Join(dir, "node.crt"),
		KeyPath:  filepath.Join(dir, "node.key"),
		CAPath:   filepath.Join(dir, "ca.crt"),
	}

	if _, _, err := pendingIdentity(tls, "epyc-1"); err != nil {
		t.Fatalf("stage: %v", err)
	}

	if err := os.Chmod(filepath.Join(dir, "pending.key"), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if _, _, err := pendingIdentity(tls, "epyc-1"); err == nil {
		t.Fatal("a world-readable staged key was used to enroll; whoever can read it can be " +
			"this node once it is approved")
	}
}

// A NODE ADMITTED BEFORE BILLET TRACKED SERIALS CAN STILL BE REVOKED.
//
// Revocation reaches the serials billet wrote down, and issued_certs did not
// exist until this release — so every node admitted by an older control plane
// holds a working certificate whose serial was never recorded. `billet nodes
// revoke` would report that the machine holds nothing and change nothing, which
// is the worst possible answer to a compromise.
//
// The admission trail is what makes it recoverable: both ways in stored the
// certificate they handed over, so the serial can be read back out of it.
func TestRevocationReachesACertificateAdmittedBeforeSerialsWereTracked(t *testing.T) {
	stateDir := t.TempDir()
	cfg := writeCAConfig(t, stateDir)

	deploymentID, err := state.DeploymentID(stateDir)
	if err != nil {
		t.Fatalf("deployment id: %v", err)
	}

	ca, err := wirecert.LoadOrCreateCA(stateDir, deploymentID)
	if err != nil {
		t.Fatalf("authority: %v", err)
	}

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	leaf, err := wirecert.LeafOf(bundle)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	a, closeDB, err := controlPlaneAllocator(t.Context(), cfg)
	if err != nil {
		t.Fatalf("allocator: %v", err)
	}

	// WHAT AN OLDER BILLET LEFT BEHIND: the admission is recorded, the serial is
	// not.
	if _, err := a.RecordIssued(t.Context(), "epyc-1",
		wirecert.FingerprintOfCert(leaf), string(bundle.CertPEM)); err != nil {
		t.Fatalf("record the admission: %v", err)
	}

	live, err := a.LiveCertsFor(t.Context(), "epyc-1")
	if err != nil {
		t.Fatalf("LiveCertsFor: %v", err)
	}

	if len(live) != 0 {
		t.Fatalf("this test does not stage the pre-upgrade state: %d serial(s) already recorded",
			len(live))
	}

	// THROUGH THE COMMAND, not the helper it calls. Driving backfillIssuedCerts
	// directly would leave this test green with the production call deleted,
	// which is the wiring it exists to protect.
	closeDB()

	if err := cmdNodesRevoke(t.Context(), []string{"epyc-1", "--config", cfg}); err != nil {
		t.Fatalf("billet nodes revoke: %v", err)
	}

	a, closeDB, err = controlPlaneAllocator(t.Context(), cfg)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	defer closeDB()

	gone, err := a.CertRevoked(t.Context(), wirecert.Serial(leaf))
	if err != nil {
		t.Fatalf("CertRevoked: %v", err)
	}

	if !gone {
		t.Error("the certificate the machine presents is still accepted after `billet nodes " +
			"revoke`; an upgraded deployment cannot take back what it admitted")
	}
}

// A HALF-PRESENT STAGE IS NOT A MISSING ONE.
//
// The key is what the control plane has recorded a pending request against. If
// its companions are lost but it survives, generating a fresh identity replaces
// the key that request belongs to — and the name is then held by a fingerprint
// this machine can no longer present, which no retry can fix and only an
// operator can free. That is the exact deadlock the staging exists to avoid,
// reached by the staging itself.
func TestAPartlyMissingStageDoesNotReplaceTheKey(t *testing.T) {
	for _, lost := range []string{"pending.csr", "pending.node"} {
		t.Run("lost "+lost, func(t *testing.T) {
			dir := t.TempDir()
			tls := &config.NodeTLS{
				CertPath: filepath.Join(dir, "node.crt"),
				KeyPath:  filepath.Join(dir, "node.key"),
				CAPath:   filepath.Join(dir, "ca.crt"),
			}

			if _, _, err := pendingIdentity(tls, "epyc-1"); err != nil {
				t.Fatalf("stage: %v", err)
			}

			before, err := os.ReadFile(filepath.Join(dir, "pending.key"))
			if err != nil {
				t.Fatalf("read the staged key: %v", err)
			}

			if err := os.Remove(filepath.Join(dir, lost)); err != nil {
				t.Fatalf("remove %s: %v", lost, err)
			}

			if _, _, err := pendingIdentity(tls, "epyc-1"); err == nil {
				t.Error("a partly missing stage was treated as no stage at all")
			}

			after, err := os.ReadFile(filepath.Join(dir, "pending.key"))
			if err != nil {
				t.Fatalf("read the staged key back: %v", err)
			}

			if !bytes.Equal(before, after) {
				t.Error("the staged private key was replaced, so the pending request on the " +
					"control plane now names a key this machine cannot present")
			}
		})
	}
}

// --reissue IS THE DELIBERATE PATH OVER AN EXISTING BUNDLE: the old one moves
// aside rather than vanishing (the node still holds that key until restarted,
// and the old certificate stays VALID until revoked — the output says both),
// and the new bundle differs from the old.
func TestCAIssueReissueReplacesDeliberately(t *testing.T) {
	// NOT parallel: capture swaps the process-global stdout, and a parallel
	// sibling printing through the real one races the swap.
	cfg := writeCAConfig(t, t.TempDir())
	out := filepath.Join(t.TempDir(), "bundle")

	if err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", cfg, "--out", out}); err != nil {
		t.Fatalf("ca issue: %v", err)
	}
	oldKey, err := os.ReadFile(filepath.Join(out, "node.key"))
	if err != nil {
		t.Fatalf("read old key: %v", err)
	}

	output := capture(t, func() {
		if err := cmdCAIssue(t.Context(),
			[]string{"epyc-1", "--config", cfg, "--out", out, "--reissue"}); err != nil {
			t.Fatalf("ca issue --reissue: %v", err)
		}
	})

	newKey, err := os.ReadFile(filepath.Join(out, "node.key"))
	if err != nil {
		t.Fatalf("read new key: %v", err)
	}
	if bytes.Equal(newKey, oldKey) {
		t.Error("the reissued bundle carries the old key")
	}

	aside, err := os.ReadFile(filepath.Join(out+".replaced", "node.key"))
	if err != nil {
		t.Fatalf("the old bundle was not moved aside: %v", err)
	}
	if !bytes.Equal(aside, oldKey) {
		t.Error("the moved-aside bundle is not the old one")
	}

	for _, must := range []string{"stays valid until you revoke", "ca revoke"} {
		if !strings.Contains(output, must) {
			t.Errorf("the reissue output does not say %q:\n%s", must, output)
		}
	}

	// A THIRD ISSUE MUST NOT DESTROY THE ARCHIVE: .replaced holds the only
	// copy of the certificate the revoke command reads, so a further --reissue
	// refuses while it exists rather than silently deleting an unrevoked
	// credential's only handle.
	err = cmdCAIssue(t.Context(), []string{"epyc-1", "--config", cfg, "--out", out, "--reissue"})
	if err == nil {
		t.Fatal("a second --reissue destroyed the archived bundle")
	}
	if !strings.Contains(err.Error(), ".replaced") || !strings.Contains(err.Error(), "revoke") {
		t.Errorf("the refusal does not name the archive and the remedy: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out+".replaced", "node.key")); statErr != nil {
		t.Errorf("the refusal did not preserve the archive: %v", statErr)
	}
}

// A SHORT LEAF IS THE ONLY WAY TO REHEARSE A ROTATION, because a node renews once
// less than a third of its certificate's life remains and a year-long leaf
// renews in eight months. The flag is bounded on both sides, and the bounds are
// asserted by the messages that name them, because a refusal that says the wrong
// thing sends an operator to the wrong fix.
func TestCAIssueHonoursALifetime(t *testing.T) {
	t.Parallel()

	serverState := t.TempDir()
	cfg := writeCAConfig(t, serverState)
	out := filepath.Join(t.TempDir(), "bundle")

	before := time.Now()

	if err := cmdCAIssue(t.Context(), []string{"epyc-1", "--config", cfg, "--out", out,
		"--lifetime", "15m"}); err != nil {
		t.Fatalf("ca issue --lifetime 15m: %v", err)
	}

	bundle, err := wirecert.LoadBundle(
		filepath.Join(out, "node.crt"),
		filepath.Join(out, "node.key"),
		filepath.Join(out, "ca.crt"))
	if err != nil {
		t.Fatalf("the bundle it wrote does not load: %v", err)
	}

	block, _ := pem.Decode(bundle.CertPEM)
	if block == nil {
		t.Fatal("the bundle's certificate is not PEM")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse the issued certificate: %v", err)
	}

	// Issued at `now` with ClockSkew of backdating, good for the lifetime asked.
	wantNotAfter := before.Add(15 * time.Minute)
	if cert.NotAfter.Before(wantNotAfter.Add(-30*time.Second)) ||
		cert.NotAfter.After(wantNotAfter.Add(30*time.Second)) {
		t.Errorf("the certificate expires at %s, want about %s (15m after issue)",
			cert.NotAfter, wantNotAfter)
	}

	if life := cert.NotAfter.Sub(cert.NotBefore); life > 15*time.Minute+wirecert.ClockSkew+time.Minute {
		t.Errorf("the certificate's life is %s; --lifetime 15m was not honoured", life)
	}

	for _, tc := range []struct{ lifetime, want string }{
		{"5m", "outside"},
		{"9000h", "outside"},
		{"0s", "outside"},
	} {
		err := cmdCAIssue(t.Context(), []string{"epyc-2", "--config", cfg,
			"--out", filepath.Join(t.TempDir(), "b"), "--lifetime", tc.lifetime})
		if err == nil {
			t.Fatalf("--lifetime %s was accepted", tc.lifetime)
		}

		if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "10m") {
			t.Errorf("--lifetime %s: refusal %q does not name the bound", tc.lifetime, err)
		}
	}
}
