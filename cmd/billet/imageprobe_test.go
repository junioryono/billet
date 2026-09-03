package main

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/deploymentid"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/wirecert"
)

// nodeDeployment is a minted identity for a machine that is running jobs.
const nodeDeployment = "638617ef8137d24429c7afb64d07736c"

// A VERIFICATION'S MICROVM IS NOT THE NODE'S TO SWEEP.
//
// The probe's lease is invented rather than allocated, so the node daemon's sweep —
// which lists every instance this deployment owns and destroys any whose lease it
// cannot account for — is entirely correct to call it an orphan and kill it. The node
// sweeps on its own five-minute loop and the control plane broadcasts one after every
// successful reap, on a thirty-second tick, so on a healthy registered node a
// boot-to-report window of about a minute normally spans one and the weekly gate
// reports a perfectly good image as broken.
//
// Ownership is what keeps them apart, and it is invisible at the call site: the
// identity is one string argument that looks incidental.
func TestAProbeIsNotOwnedByTheNodeThatWouldSweepIt(t *testing.T) {
	t.Parallel()

	got := probeDeployment(nodeDeployment)

	if got == nodeDeployment {
		t.Fatal("a verification's microVM carries the node's own identity, so the node's sweep " +
			"will find a lease it cannot account for and destroy the probe mid-verification — " +
			"reporting a healthy image as broken")
	}

	// THE SAME ANSWER EVERY TIME, which is what makes the pre-launch cleanup exact.
	// It destroys ONE name rather than enumerating, because enumeration cannot be
	// made safe here — so a probe identity that varied between runs would leave
	// residue nothing could ever name again.
	if again := probeDeployment(nodeDeployment); again != got {
		t.Errorf("probeDeployment answered %q and then %q; a leftover probe can only be "+
			"cleaned up by a run that derives the same identity", got, again)
	}

	// AND THE DEPLOYMENT IS IN THE HASH. This does not prove uniqueness — a truncated
	// hash cannot — and it is not what the test is for: it catches the deployment
	// being left OUT of the derivation, which makes two billets on one machine collide
	// with certainty rather than on a 128-bit event. They may share a Ceph pool, so
	// that is one deployment's cleanup aimed at the other's live disk, and it is the
	// same reason probeLeaseID covers the deployment too.
	const other = "638617ef8137d24429c7afb64d07736d"

	if probeDeployment(other) == got {
		t.Errorf("two deployments derive one probe identity %q, so the derivation does "+
			"not depend on which deployment asked", got)
	}
}

// AND THE IDENTITY IS ONE THE PROVIDER WILL ACTUALLY ACCEPT.
//
// This is the whole defect in one call. The probe deliberately owns its microVM
// under an identity that is not the node's, and the provider validates the owner it
// is handed against the grammar its jail markers are written and read under. Both
// rules are right and for a release they disagreed: `deployment + "-imageverify"` is
// 44 characters, the provider requires 32, and every verification on the only
// backend that boots a guest image failed before it launched — which is also the
// gate the documented binary upgrade runs.
//
// DRIVEN THROUGH THE PRODUCTION CONSTRUCTOR rather than through the grammar it
// applies, because a test that re-states the rule agrees with a constructor that
// later applies a different one.
func TestTheProbeIdentityIsOneTheFirecrackerProviderAccepts(t *testing.T) {
	t.Parallel()

	_, cfg := probeFixture(t)

	if _, err := firecracker.New(probeDeployment(nodeDeployment), cfg, noRootDisk{}); err != nil {
		t.Fatalf("the provider refuses the identity every verification runs under, so "+
			"`billet images pull --verify` cannot succeed on this backend at all: %v", err)
	}
}

// AND A PROBE'S JAIL IS INVISIBLE TO THE NODE WITHOUT BREAKING ITS INVENTORY.
//
// The two halves are one property and only the first is obvious. The node must not
// REPORT the probe, because everything it reports and cannot account for it
// destroys. It must also not FAIL on it: List returns an error rather than a shorter
// list — deliberately, since a silently dropped row is capacity resold under a
// running guest — so an owner marker the node cannot parse takes the whole
// inventory down for as long as the directory is there, and nothing else reaps a
// probe by design.
//
// A jail on disk rather than one launched, because the property is entirely about
// what the marker says and who reads it.
func TestANodeSweepIgnoresAProbeJailWithoutFailingItsInventory(t *testing.T) {
	t.Parallel()

	base, cfg := probeFixture(t)

	lease, err := probeLeaseID(nodeDeployment)
	if err != nil {
		t.Fatalf("derive the probe lease: %v", err)
	}

	name := provider.InstanceName(lease)
	stageJail(t, base, name, probeDeployment(nodeDeployment))

	node, err := firecracker.New(nodeDeployment, cfg, noRootDisk{})
	if err != nil {
		t.Fatalf("construct the node's own provider: %v", err)
	}

	listed, err := node.List(t.Context())
	if err != nil {
		t.Fatalf("the node's inventory failed over a probe's jail, which frees no capacity "+
			"and reconciles nothing until an operator removes the directory: %v", err)
	}

	if len(listed) != 0 {
		t.Fatalf("the node reports the probe as its own, so its sweep destroys it "+
			"mid-verification: %+v", listed)
	}

	// AND THE VERIFICATION CAN STILL FIND ITS OWN, which is what makes the leftover
	// cleanup possible at all: nothing else will ever reap one.
	probe, err := firecracker.New(probeDeployment(nodeDeployment), cfg, noRootDisk{})
	if err != nil {
		t.Fatalf("construct the probe's provider: %v", err)
	}

	mine, err := probe.List(t.Context())
	if err != nil {
		t.Fatalf("the verification cannot list its own probe: %v", err)
	}

	if len(mine) != 1 || mine[0].Name != name {
		t.Fatalf("the verification does not see the probe it owns, so a leftover would "+
			"never be cleaned up: %+v", mine)
	}
}

// AND THE NODE CANNOT DESTROY IT EITHER, WHICH IS THE HALF THAT PROTECTS ANYTHING.
//
// Not reporting the probe is what keeps the sweep from reaching for it. This is the
// same ownership rule one layer down, at the point that ACTS rather than the one
// that reports — and it is the one that has to hold, because the sweep is not the
// only caller of Destroy and a probe destroyed mid-verification is a healthy image
// reported as broken.
//
// The opposite direction — that the verification can destroy its OWN leftover —
// cannot be asserted from here, and the reason is measured rather than assumed: on
// this platform Destroy gets past the ownership check and then fails needing `ip`
// and /proc/mounts, whose injection seams are private to the provider's package. A
// test for it would skip on every machine `make check` runs on, which is exactly
// the drift that left "billet-selftest" refused for a release. The firecracker
// package owns that direction.
func TestTheNodeCannotDestroyTheProbeItMustNotSweep(t *testing.T) {
	t.Parallel()

	base, cfg := probeFixture(t)

	lease, err := probeLeaseID(nodeDeployment)
	if err != nil {
		t.Fatalf("derive the probe lease: %v", err)
	}

	name := provider.InstanceName(lease)
	stageJail(t, base, name, probeDeployment(nodeDeployment))

	node, err := firecracker.New(nodeDeployment, cfg, noRootDisk{})
	if err != nil {
		t.Fatalf("construct the node's own provider: %v", err)
	}

	if _, err := node.Destroy(t.Context(), name); err == nil {
		t.Fatal("the node destroyed a jail belonging to the verification probe, which is a " +
			"microVM torn down mid-boot and a healthy image reported as broken")
	} else if !strings.Contains(err.Error(), "belongs to deployment") {
		t.Errorf("the refusal is not about ownership, so it may be incidental: %v", err)
	}

	// AND THE JAIL IS STILL THERE. A refusal is the cheapest thing Destroy produces
	// and says nothing about what it did on the way to producing one.
	if _, err := os.Stat(filepath.Join(base, "firecracker", name)); err != nil {
		t.Errorf("the probe's jail did not survive the refusal: %v", err)
	}
}

// probeFixture is a chroot base and the configuration a provider is built over it
// with, returning both.
//
// NOT t.TempDir(), AND FOR A REAL CONSTRAINT RATHER THAN A PREFERENCE. A jail's API
// socket is a fixed-size address — 103 usable bytes on darwin — and the provider
// refuses a layout whose socket it could not dial, at construction. The tail below
// the base is 80 bytes here: a `/`, the resolved binary's name, a `/`, `billet-`
// plus a 32-character lease, and `/root/run/firecracker.socket`. That leaves 23 for
// the base, while Go's temp directory on this machine starts from a 49-character
// TMPDIR and embeds the test's name after it.
//
// AND os.Mkdir RATHER THAN MkdirAll, WHICH IS WHAT MAKES THE CLEANUP BELOW SAFE.
// Mkdir refuses a name that already exists, so a directory this call returns is one
// it created — where MkdirAll tolerates residue from a run that was killed, and the
// cleanup would then remove a path this test never made. The name has to be
// hand-rolled rather than MkdirTemp's for the budget above.
//
// The stub binary is called `firecracker` rather than a versioned name for the same
// budget: `firecracker-v1.16.1` is eight bytes longer and puts the address over.
func probeFixture(t *testing.T) (string, config.FirecrackerConfig) {
	t.Helper()

	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("name a temp dir: %v", err)
	}

	root := "/tmp/b" + hex.EncodeToString(suffix[:])
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatalf("make a short temp dir: %v", err)
	}

	// AFTER THE CREATE, because only a create that succeeded proves this directory is
	// this test's to remove.
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove %s: %v", root, err)
		}
	})

	base := filepath.Join(root, "j")
	if err := os.Mkdir(base, 0o700); err != nil {
		t.Fatalf("make the chroot base: %v", err)
	}

	// THE JAILER NAMES ITS CHROOT AFTER THE RESOLVED BINARY and refuses one whose
	// name does not contain `firecracker`, so this stands in for both: the provider
	// resolves it at construction, and List enumerates the directory named after it.
	binary := filepath.Join(root, "firecracker")
	if err := os.WriteFile(binary, []byte("#!/bin/true\n"), 0o600); err != nil {
		t.Fatalf("write the stub binary: %v", err)
	}

	cfg := config.FirecrackerConfig{
		BinaryPath:  binary,
		JailerPath:  binary,
		KernelImage: binary,
		ChrootBase:  base,
		Bridge:      "br0",
	}
	cfg.Normalize()

	return base, cfg
}

// stageJail writes the one thing a jail needs to be attributable: a directory named
// after the instance, holding a marker naming the deployment that owns it.
func stageJail(t *testing.T, base, name, owner string) {
	t.Helper()

	dir := filepath.Join(base, "firecracker", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("stage a jail at %s: %v", dir, err)
	}

	// ONE NEWLINE-TERMINATED IDENTITY, which is the whole of the marker's format.
	if err := os.WriteFile(filepath.Join(dir, "billet-owner"), []byte(owner+"\n"), 0o600); err != nil {
		t.Fatalf("write the owner marker: %v", err)
	}
}

// AN IDENTITY BILLET DID NOT MINT IS REFUSED BEFORE ANYTHING DERIVES FROM IT.
//
// The two sources do not agree about parsing: a state directory's identity is
// validated when it is read, and a bundle's is a certificate's organization field
// that nothing between the CA and here looks at — createCA writes whatever string it
// is handed. Everything this command does is DERIVED from that value, and the
// derivations are hashes, so they come out well-formed from a malformed input and
// the first thing that would object is a filename.
func TestAnIdentityBilletDidNotMintIsRefusedBeforeAnythingDerivesFromIt(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), "not-a-minted-identity")
	if err != nil {
		t.Fatalf("mint an authority naming something billet would not have: %v", err)
	}

	bundle, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue: %v", err)
	}

	dir := t.TempDir()
	if err := bundle.Write(dir); err != nil {
		t.Fatalf("write the bundle: %v", err)
	}

	cfg := nodeConfigFor(t, "epyc-1", t.TempDir(), dir)

	// THE BUNDLE PATH IS THE ONE UNDER TEST, and this says so BEFORE the assertion
	// rather than instead of it. verifyDeploymentID swallows a bundle it cannot load
	// and falls back to the state directory, which mints a valid identity — so a
	// broken fixture would not produce a false pass (the refusal below would simply
	// not arrive), but it would report the guard as missing when what is missing is
	// the certificate. This separates the two.
	loaded, err := nodeBundle(cfg)
	if err != nil || loaded == nil {
		t.Fatalf("the bundle this test turns on did not load: bundle=%v err=%v", loaded, err)
	}

	if _, err := verifyDeploymentID(cfg); err == nil {
		t.Fatal("a certificate naming a deployment billet could not have minted was accepted, " +
			"so the probe's owner, its lease and its lock are all derived from a value " +
			"nothing parsed")
	} else if !strings.Contains(err.Error(), "not one billet mints") {
		t.Errorf("the refusal does not name the identity as the reason: %v", err)
	}
}

// AND THE ORDINARY PATH STILL ANSWERS WITH THE IDENTITY THIS HOST ALREADY HAS.
//
// A node with no certificate falls back to its own state directory, and that is not
// an exotic configuration — it is every single-machine deployment. A parse in front
// of a fallback that returns the wrong variable would refuse every one of them with
// a complaint about an identity zero characters long, and the bundle test above
// would not notice, because it never reaches the fallback.
//
// THE EXPECTED VALUE IS WRITTEN BEFORE THE CALL, not read back afterwards. Against a
// file the call itself created, the comparison only proves the function agrees with
// its own side effect — so a fallback that DISCARDED the existing identity and
// minted a replacement would pass, and that is not a hypothetical shape: a node
// inventing an identity rather than joining one is the failure that made a freshly
// enrolled host unable to register at all, forever, with nothing an operator could
// copy to fix it.
func TestANodeWithNoCertificateStillAnswersFromItsOwnDirectory(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()

	// The identity this machine is already running under, and the containers it has
	// already labelled with it.
	const existing = "0123456789abcdef0123456789abcdef"

	identityPath := filepath.Join(stateDir, "deployment-id")
	if err := os.WriteFile(identityPath, []byte(existing+"\n"), 0o600); err != nil {
		t.Fatalf("stage the identity this host already has: %v", err)
	}

	path := filepath.Join(t.TempDir(), "billet.yaml")

	body := `
node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ` + stateDir + `
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
		t.Fatalf("load: %v", err)
	}

	// THE FIXTURE TAKES THE PATH THIS TEST IS ABOUT. Either of these being present
	// answers the question before the fallback is reached, so the test would pass
	// while proving something else entirely.
	if cfg.Node.TLS != nil || cfg.Server != nil {
		t.Fatalf("this config does not describe a node without a certificate: tls=%v server=%v",
			cfg.Node.TLS, cfg.Server)
	}

	deployment, err := verifyDeploymentID(cfg)
	if err != nil {
		t.Fatalf("a node with no certificate cannot say which microVMs are its own: %v", err)
	}

	if deployment != existing {
		t.Errorf("verifyDeploymentID answered %q, and this host is running as %q — its "+
			"compute is labelled with the second", deployment, existing)
	}

	// AND THE IDENTITY ON DISK IS STILL THE SAME BYTES. Answering correctly while
	// replacing what is on disk strands every container the original identity
	// labelled, one restart later.
	//
	// COMPARED EXACTLY rather than trimmed, because a rewrite that normalises to the
	// same identity is still a replacement of the recorded one. This says nothing
	// about whether the file was OPENED — a truncate-and-rewrite of identical bytes
	// passes — and proving that would need a write seam this command has no other use
	// for.
	onDisk, err := os.ReadFile(identityPath)
	if err != nil {
		t.Fatalf("read the identity back: %v", err)
	}

	if string(onDisk) != existing+"\n" {
		t.Errorf("the state directory now holds %q, not the %q it had before",
			string(onDisk), existing+"\n")
	}
}

// AND THE DERIVED IDENTITY IS ONE BILLET COULD HAVE MINTED.
//
// The provider's check is this grammar, and so is the marker parser's on every read,
// so stating it directly is what names the rule the two tests above exercise
// through their callers.
func TestTheProbeIdentityIsAValueBilletCouldHaveMinted(t *testing.T) {
	t.Parallel()

	if err := deploymentid.Validate(probeDeployment(nodeDeployment)); err != nil {
		t.Fatalf("the probe identity is outside the grammar every owner marker is read "+
			"back through: %v", err)
	}
}
