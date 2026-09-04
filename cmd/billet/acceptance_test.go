package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"

	"gopkg.in/yaml.v3"
)

// A base config with everything the derivation has to rewrite, so a test of one
// edit cannot pass because the key was absent.
const acceptanceBaseConfig = `# an operator's own comment, which the derivation must not eat
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 32GiB

github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/app-private-key.pem

node:
  name: box-one
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks

nodes:
  - name: box-one
    provider: docker

backup:
  s3:
    bucket: production-billet-backups
    region: us-west-2
    prefix: billet-backups

tiers:
  - label: linux-2vcpu
    provider: docker
    node: box-one
    vcpu: 2
    memory: 8GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: trusted
    runner_group: billet
    workflows:
      - acme/repo/.github/workflows/ci.yml@refs/heads/main
`

func writeAcceptanceBase(t *testing.T, body string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "base.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write the base config: %v", err)
	}

	return path
}

func deriveForTest(t *testing.T, base, workspace string) acceptanceWorkspace {
	t.Helper()

	ws, err := deriveAcceptance(t.Context(), acceptanceInputs{
		base:      base,
		workspace: workspace,
		prefix:    defaultLabelPrefix,
	})
	if err != nil {
		t.Fatalf("deriveAcceptance: %v", err)
	}

	return ws
}

// EVERY SHARED THING IS MADE ITS OWN, and the test asserts on the LOADED config
// rather than on the file's text: a key written at the wrong nesting depth
// satisfies every substring assertion and configures nothing.
func TestAnAcceptanceRunSharesNoStateWithTheDeploymentItCameFrom(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	dir := t.TempDir()

	ws := deriveForTest(t, base, dir)

	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		t.Fatalf("the derived config does not load: %v", err)
	}

	for _, tc := range []struct {
		what string
		got  string
	}{
		{"server.state_dir", cfg.Server.IdentityDir},
		{"node.state_dir", cfg.Node.StateDir},
		{"node.lock_dir", cfg.Node.LockDir},
	} {
		if !strings.HasPrefix(tc.got, dir) {
			t.Errorf("%s is %q, which is not inside the workspace %q — this run would share "+
				"it with the deployment it was derived from", tc.what, tc.got, dir)
		}
	}

	// THE LISTEN ADDRESS IS LOOPBACK AND NOT THE BASE'S. A derived deployment
	// binding the real one's address either fails to start or, worse, takes the
	// port first and leaves the real control plane unable to.
	if !strings.HasPrefix(cfg.Server.Listen, "127.0.0.1:") {
		t.Errorf("server.listen is %q, which is not loopback", cfg.Server.Listen)
	}

	if cfg.Server.Listen == "127.0.0.1:7717" {
		t.Error("server.listen is still the base config's port, so this run and the " +
			"deployment it came from would race for it")
	}

	// AND THE NODE DIALS WHAT THE SERVER BOUND. Two values, one fact: a node
	// pointed at the base's address would register with the REAL control plane
	// and offer this run's capacity to it.
	if cfg.Node.ServerAddr != cfg.Server.Listen {
		t.Errorf("node.server_addr %q does not match server.listen %q, so the derived node "+
			"dials something other than the derived server",
			cfg.Node.ServerAddr, cfg.Server.Listen)
	}
}

// AN ACCEPTANCE RUN NEVER MOVES ITSELF. The binary under test reports no release,
// and the automatic starter reads a fleet reporting no release as not on the
// channel's target: measured 2026-09-04, a snapshot-built rehearsal deployment
// with no release block opened a rollout to v0.6.0 a minute after boot. A base
// that says nothing about releases is on automatic updates by default, so the
// derivation has to say no explicitly, and a base that follows a channel keeps
// everything about it except that switch.
func TestAnAcceptanceRunIsNeverOnAutomaticUpdates(t *testing.T) {
	t.Parallel()

	// AN ALIAS IS NOT A SCALAR. Rewriting the value in place leaves an AliasNode
	// pointing at the anchor, which renders as the anchor's value or not at all;
	// the derivation has to replace the node. The anchor sits on a boolean the
	// base legitimately carries, because the base must load before it is derived.
	aliased := strings.Replace(acceptanceBaseConfig,
		"  lock_dir: /run/billet/locks\n",
		"  lock_dir: /run/billet/locks\n  allow_unlocked_deployment: &on true\n", 1) +
		"\nrelease:\n  automatic: *on\n  channel: candidate\n"

	if !strings.Contains(aliased, "&on true") {
		t.Fatal("the alias fixture did not take, so this test proves nothing about aliases")
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"a base that says nothing about releases", acceptanceBaseConfig},
		{"a base that follows the candidate channel", acceptanceBaseConfig + "\nrelease:\n  channel: candidate\n"},
		{"a base that says yes explicitly", acceptanceBaseConfig + "\nrelease:\n  automatic: true\n"},
		{"a base that says yes through an alias", aliased},
		{"a base with an empty release block", acceptanceBaseConfig + "\nrelease: null # left for the operator\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			base := writeAcceptanceBase(t, tc.body)
			ws := deriveForTest(t, base, t.TempDir())

			cfg, err := config.Load(ws.ConfigPath)
			if err != nil {
				t.Fatalf("the derived config does not load: %v", err)
			}

			if cfg.Release.AutomaticUpdates() {
				t.Fatal("the derived deployment is on automatic updates, so its control plane " +
					"would open a rollout to the channel and drain the node the acceptance job needs")
			}

			if strings.Contains(tc.body, "candidate") && cfg.Release.Channel != config.ChannelCandidate {
				t.Errorf("the base's channel %q was not kept: got %q",
					config.ChannelCandidate, cfg.Release.Channel)
			}

			// THE OPERATOR'S COMMENT SURVIVES the value it hung on being replaced.
			if strings.Contains(tc.body, "left for the operator") {
				derived, err := os.ReadFile(ws.ConfigPath)
				if err != nil {
					t.Fatalf("read the derived config: %v", err)
				}

				if !strings.Contains(string(derived), "left for the operator") {
					t.Error("the comment on `release: null` was eaten by the derivation")
				}
			}
		})
	}
}

// THE NODE THAT DEFINES AN ANCHOR IS REFUSED, NOT REPLACED. Dropping it would leave
// every alias dangling and the derived file unparseable; carrying the anchor onto
// the new `false` would silently flip every other use. The base itself loads,
// which is what makes this the derivation's refusal and not the loader's.
func TestAnAnchoredAutomaticIsRefusedRatherThanRewritten(t *testing.T) {
	t.Parallel()

	body := strings.Replace(acceptanceBaseConfig,
		"  lock_dir: /run/billet/locks\n",
		"  lock_dir: /run/billet/locks\n  allow_unlocked_deployment: *enabled\n", 1)
	body = "release:\n  automatic: &enabled true\n" + body

	if !strings.Contains(body, "*enabled") || !strings.HasPrefix(body, "release:") {
		t.Fatal("the fixture did not take, so this test proves nothing about anchors")
	}

	if _, err := config.Parse("the fixture", []byte(body)); err != nil {
		t.Fatalf("the fixture must load before the derivation can be the one refusing: %v", err)
	}

	base := writeAcceptanceBase(t, body)

	_, err := deriveAcceptance(t.Context(), acceptanceInputs{
		base:      base,
		workspace: t.TempDir(),
		prefix:    defaultLabelPrefix,
	})
	if err == nil {
		t.Fatal("a base whose release.automatic defines an anchor was derived; the aliases of that " +
			"anchor are dangling in the derived file, or were silently flipped")
	}

	if !strings.Contains(err.Error(), "&enabled") {
		t.Errorf("the refusal does not name the anchor: %v", err)
	}
}

// THE TIER LABELS ARE THE SCALE SETS, so this is the assertion the teardown's
// safety rests on: `billet acceptance down` runs `teardown --all` against the
// derived config, which deletes the scale set of every tier in it.
func TestAnAcceptanceRunOwnsNoScaleSetTheBaseConfigNames(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	ws := deriveForTest(t, base, t.TempDir())

	original, err := config.Load(base)
	if err != nil {
		t.Fatalf("load the base config: %v", err)
	}

	derived, err := config.Load(ws.ConfigPath)
	if err != nil {
		t.Fatalf("load the derived config: %v", err)
	}

	for _, d := range derived.Tiers {
		for _, o := range original.Tiers {
			if d.Label == o.Label {
				t.Errorf("the derived tier %q is the same scale set as the base config's; "+
					"`billet acceptance down` would delete it", d.Label)

				break
			}
		}
	}

	if len(derived.Tiers) != len(original.Tiers) {
		t.Errorf("the derivation produced %d tier(s) from %d; it renames tiers, it does not "+
			"add or drop them", len(derived.Tiers), len(original.Tiers))
	}

	// AND THE RECORD NAMES THEM, because `down` and the evidence report what this
	// run owns without re-deriving the config — and a record that disagreed with
	// the file would describe a teardown nobody performed.
	var recorded []string
	for _, d := range derived.Tiers {
		recorded = append(recorded, d.Label)
	}

	if strings.Join(ws.Tiers, ",") != strings.Join(recorded, ",") {
		t.Errorf("the workspace records tiers %v and the derived config has %v",
			ws.Tiers, recorded)
	}
}

// A TIER PINNED TO A HOST MUST NAME THE DERIVED HOST. The node's own name is
// prefixed, so a tier still naming the base one pins to a machine that never
// registers — and a pinned tier whose host never appears advertises nothing,
// forever, with nothing saying why.
func TestAnAcceptanceTierIsPinnedToTheHostThisRunActuallyHas(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	ws := deriveForTest(t, base, t.TempDir())

	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		t.Fatalf("load the derived config: %v", err)
	}

	if cfg.Node.Name != "accept-box-one" {
		t.Errorf("node.name is %q, want the prefixed one", cfg.Node.Name)
	}

	for _, tier := range cfg.Tiers {
		if tier.Node != "" && tier.Node != cfg.Node.Name {
			t.Errorf("tier %q pins host %q and this run's node is %q, so it can never be "+
				"placed", tier.Label, tier.Node, cfg.Node.Name)
		}
	}

	for _, policy := range cfg.Nodes {
		if policy.Name != cfg.Node.Name {
			t.Errorf("nodes[] describes %q and this run's node is %q, so the policy applies "+
				"to nothing", policy.Name, cfg.Node.Name)
		}
	}
}

// THE BACKUP DESTINATION IS DROPPED, so an acceptance run's archives never land
// in the real deployment's bucket, under a prefix its retention rule governs.
func TestAnAcceptanceRunUploadsNothingToTheRealBackupBucket(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	ws := deriveForTest(t, base, t.TempDir())

	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		t.Fatalf("load the derived config: %v", err)
	}

	if cfg.Backup != nil {
		t.Errorf("the derived config still names a backup destination (%+v)", cfg.Backup)
	}

	// AND THE FIXTURE HAD ONE, or this test passes against a base config that
	// never carried the thing it is about.
	original, err := config.Load(base)
	if err != nil {
		t.Fatalf("load the base config: %v", err)
	}

	if original.Backup == nil {
		t.Fatal("the fixture has no backup section, so this test proves nothing")
	}
}

// THE OPERATOR'S OWN COMMENTS SURVIVE. This is not a nicety: what a derived
// config is FOR is running the operator's real configuration, and a file
// rewritten in billet's own shape with every default filled in turns "what did
// this run actually use" into a question nobody can answer by looking.
func TestADerivedConfigIsStillRecognisablyTheOperatorsOwn(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	ws := deriveForTest(t, base, t.TempDir())

	body, err := os.ReadFile(ws.ConfigPath)
	if err != nil {
		t.Fatalf("read the derived config: %v", err)
	}

	if !strings.Contains(string(body), "an operator's own comment") {
		t.Errorf("the derivation dropped the operator's comments:\n%s", body)
	}
}

// THE IDENTITY IS THIS RUN'S, AND IT IS NOT THE BASE DEPLOYMENT'S.
//
// Every destroy billet performs is scoped by it, so this is what makes "an
// acceptance run cannot delete another deployment's compute" true by
// construction rather than by care.
func TestAnAcceptanceRunMintsItsOwnDeploymentIdentity(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	first := deriveForTest(t, base, t.TempDir())
	second := deriveForTest(t, base, t.TempDir())

	if first.DeploymentID == "" {
		t.Fatal("the run recorded no deployment identity, so nothing it creates can be scoped")
	}

	if first.DeploymentID == second.DeploymentID {
		t.Error("two acceptance workspaces minted the same identity, so a teardown of one " +
			"would be scoped to the other's compute")
	}

	// AND THE RECORD AGREES WITH THE DIRECTORY. requireAcceptanceWorkspace refuses
	// a disagreement, and this is what proves `up` never creates one.
	held, ok, err := state.PeekDeploymentID(filepath.Join(filepath.Dir(first.ConfigPath), "server"))
	if err != nil || !ok {
		t.Fatalf("read the workspace's identity: %v (found=%v)", err, ok)
	}

	if held != first.DeploymentID {
		t.Errorf("the record says %s and the state directory holds %s", first.DeploymentID, held)
	}
}

// AND A SECOND RUN IN THE SAME DIRECTORY IS REFUSED RATHER THAN RESUMED.
//
// The workspace is resumable — `up` twice is an operator re-driving a failed run
// — but a workspace whose RECORD and DIRECTORY name different deployments is one
// two runs have used, and every later command is scoped to one of the two.
func TestAWorkspaceHoldingTwoDeploymentsIsRefused(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	dir := t.TempDir()
	ws := deriveForTest(t, base, dir)

	// A resumed run is fine, and asserted here so the refusal below is about the
	// disagreement rather than about running `up` twice.
	again := deriveForTest(t, base, dir)
	if again.DeploymentID != ws.DeploymentID {
		t.Fatalf("re-running up changed the identity from %s to %s",
			ws.DeploymentID, again.DeploymentID)
	}

	record := filepath.Join(dir, acceptanceRecord)

	body, err := os.ReadFile(record)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}

	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse the record: %v", err)
	}

	doc["deployment_id"] = "0000000000000000deadbeefdeadbeef"

	rewritten, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("render the record: %v", err)
	}

	if err := os.WriteFile(record, rewritten, 0o600); err != nil {
		t.Fatalf("write the record: %v", err)
	}

	if _, err := requireAcceptanceWorkspace(dir); err == nil {
		t.Fatal("a workspace whose record and state directory disagree was accepted")
	}
}

// A WORKSPACE INSIDE THE BASE DEPLOYMENT'S STATE DIRECTORY IS REFUSED, because
// an acceptance run mints an identity there and then destroys everything
// carrying it — and in that directory the identity, and the teardown, would be
// the real deployment's.
func TestAnAcceptanceWorkspaceCannotBeInsideTheRealDeployment(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	serverDir := filepath.Join(root, "server")
	nodeDir := filepath.Join(root, "node")

	body := strings.ReplaceAll(acceptanceBaseConfig, "/var/lib/billet/server", serverDir)
	body = strings.ReplaceAll(body, "/var/lib/billet/node", nodeDir)
	base := writeAcceptanceBase(t, body)

	for _, inside := range []string{serverDir, filepath.Join(serverDir, "acc"), nodeDir} {
		_, err := deriveAcceptance(t.Context(), acceptanceInputs{
			base:      base,
			workspace: inside,
			prefix:    defaultLabelPrefix,
		})
		if err == nil {
			t.Errorf("a workspace at %s was accepted, and it is inside the base deployment's "+
				"own state", inside)

			continue
		}

		if !strings.Contains(err.Error(), "state directory") {
			t.Errorf("the refusal for %s does not say why: %v", inside, err)
		}
	}
}

// AN EMPTY LABEL PREFIX IS THE DANGEROUS ONE, because the derived tiers would
// then BE the base config's scale sets — and `billet acceptance down` deletes
// every scale set in the config it is given.
func TestAnAcceptanceRunRefusesAPrefixThatSeparatesNothing(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)

	for _, prefix := range []string{"", "   ", "with space", "a,b"} {
		_, err := deriveAcceptance(t.Context(), acceptanceInputs{
			base:      base,
			workspace: t.TempDir(),
			prefix:    prefix,
		})
		if err == nil {
			t.Errorf("--label-prefix %q was accepted", prefix)

			continue
		}

		if !strings.Contains(err.Error(), "--label-prefix") {
			t.Errorf("the refusal for %q does not name the flag: %v", prefix, err)
		}
	}
}

// A CONFIG SPELLING ITS DIRECTORY `identity_dir` KEEPS THAT SPELLING.
//
// The two are MUTUALLY EXCLUSIVE at load, so a derivation that wrote the
// shorthand into such a config would produce a file billet refuses — with the
// refusal naming a key the operator never typed — and one that rewrote neither
// would leave the run's identity in the real deployment's directory.
//
// THE POSTGRESQL VARIANT OF THIS TEST WAS DELETED, AND THAT IS THE POINT. It
// asserted exactly this against a config whose ledger was external, and passed —
// while the run it blessed would have shared the production ledger and sealed it
// at teardown. An assertion about the SPELLING is silent about which database the
// file names. `identity_dir` without a `state:` block is an ordinary SQLite
// deployment, which is what this exercises;
// TestAnAcceptanceRunRefusesToShareALedgerItCannotIsolate covers the other.
func TestADerivedIdentityDirKeepsItsOwnSpelling(t *testing.T) {
	t.Parallel()

	body := strings.Replace(acceptanceBaseConfig,
		"  state_dir: /var/lib/billet/server\n",
		"  identity_dir: /var/lib/billet/server\n",
		1)

	base := writeAcceptanceBase(t, body)
	dir := t.TempDir()
	ws := deriveForTest(t, base, dir)

	raw, err := os.ReadFile(ws.ConfigPath)
	if err != nil {
		t.Fatalf("read the derived config: %v", err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse the derived config: %v", err)
	}

	server := mappingValue(documentRoot(&doc), "server")
	if mappingValue(server, "state_dir") != nil {
		t.Errorf("the derivation added state_dir beside identity_dir, which billet refuses "+
			"at load:\n%s", raw)
	}

	identity := mappingValue(server, "identity_dir")
	if identity == nil {
		t.Fatalf("the derivation removed identity_dir:\n%s", raw)

		// staticcheck reads t.Fatalf as a call that may return, so without this
		// the dereference below is flagged SA5011.
		return
	}

	if !strings.HasPrefix(identity.Value, dir) {
		t.Errorf("identity_dir is %q, which is not inside the workspace %q — the run would "+
			"keep its identity in the real deployment's directory",
			identity.Value, dir)
	}
}

// A DERIVED CONFIG THAT DOES NOT LOAD IS CAUGHT HERE, not by a service that
// refuses to start several steps later.
func TestADerivedConfigIsProvedBeforeItIsWritten(t *testing.T) {
	t.Parallel()

	// A base with no tiers: the derivation refuses it, because an acceptance run
	// derived from one has nothing to run a job on.
	body, _, ok := strings.Cut(acceptanceBaseConfig, "tiers:")
	if !ok {
		t.Fatal("the fixture has no tiers section, so this test cannot remove one")
	}

	_, err := deriveAcceptance(t.Context(), acceptanceInputs{
		base:      writeAcceptanceBase(t, body),
		workspace: t.TempDir(),
		prefix:    defaultLabelPrefix,
	})
	if err == nil {
		t.Fatal("a base config with no tiers was accepted")
	}
}

// THE ACCOUNT ASSERTION REFUSES BOTH WAYS IT CAN FAIL, and they are different
// answers: the credential is in the wrong account, or billet could not find out.
// Reporting the second as the first would send an operator looking for a
// credential problem they do not have; reporting it as success would run the
// thing this check stands in front of.
func TestTheAccountAssertionSeparatesWrongFromUnknown(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{}

	t.Run("no region to ask in", func(t *testing.T) {
		t.Parallel()

		_, err := assertAWSAccount(t.Context(), acceptanceInputs{
			account: "123456789012",
		}, cfg)
		if err == nil {
			t.Fatal("an account was asserted with nowhere to ask")
		}

		if !strings.Contains(err.Error(), "--region") {
			t.Errorf("the refusal does not say what would fix it: %v", err)
		}
	})
}

// THE KEYS A LOOPBACK DEPLOYMENT CANNOT CARRY ARE REMOVED, and this is asserted
// against the REWRITER rather than through config.Load because the base config it
// is about — a node dialling a control plane on another machine, over mTLS — is
// one that needs a certificate on disk to load at all. What matters is that the
// rewrite drops them: a derived config that kept `node.tls` beside a loopback
// `server_addr` is refused at load, and one that kept `bootstrap_listen` would
// open an enrollment port beside the real deployment's.
func TestTheDerivationDropsWhatALoopbackRunCannotCarry(t *testing.T) {
	t.Parallel()

	const remote = `
server:
  listen: 10.0.0.5:7717
  state_dir: /var/lib/billet/server
  bootstrap_listen: 10.0.0.5:7718
  node_tls_hosts:
    - billet.internal
node:
  name: box-one
  server_addr: 10.0.0.5:7717
  provider: docker
  state_dir: /var/lib/billet/node
  tls:
    cert_file: /etc/billet/node.crt
    key_file: /etc/billet/node.key
    ca_file: /etc/billet/ca.crt
tiers:
  - label: linux-2vcpu
    provider: docker
`

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(remote), &doc); err != nil {
		t.Fatalf("parse the fixture: %v", err)
	}

	root := documentRoot(&doc)

	// THE FIXTURE CARRIES THEM FIRST, or the assertions below hold against a
	// document that never had the keys they are about.
	for _, present := range []struct {
		section *yaml.Node
		key     string
	}{
		{mappingValue(root, "server"), "bootstrap_listen"},
		{mappingValue(root, "server"), "node_tls_hosts"},
		{mappingValue(root, "node"), "tls"},
	} {
		if mappingValue(present.section, present.key) == nil {
			t.Fatalf("the fixture has no %s, so this test proves nothing about removing it",
				present.key)
		}
	}

	if _, err := rewriteForAcceptance(root, t.TempDir(), "accept", "127.0.0.1:1234"); err != nil {
		t.Fatalf("rewriteForAcceptance: %v", err)
	}

	server := mappingValue(root, "server")
	node := mappingValue(root, "node")

	for _, gone := range []struct {
		section *yaml.Node
		key     string
		why     string
	}{
		{server, "bootstrap_listen",
			"an enrollment port beside the real deployment's, which serves callers that " +
				"have presented nothing"},
		{server, "node_tls_hosts",
			"a certificate minted for names this loopback run never serves"},
		{node, "tls",
			"mTLS against a loopback listener, which billet refuses at load"},
	} {
		if mappingValue(gone.section, gone.key) != nil {
			t.Errorf("the derivation kept %s: %s", gone.key, gone.why)
		}
	}

	if v := mappingValue(server, "listen"); v == nil || v.Value != "127.0.0.1:1234" {
		t.Errorf("server.listen was not rewritten to the picked loopback address: %+v", v)
	}
}

// THE SETTLE CHECK MUST SEE A SERVICE THAT REFUSED ITS CONFIG, and it must not
// see one that is fine as having exited.
//
// THIS IS THE TEST THAT WAS MISSING, and its absence hid a defect that would have
// made the command completely non-functional on its first real use. The first
// version asked `Process.Signal(nil)` — MEASURED to return "os: unsupported
// signal type" for a LIVE process as readily as a dead one, so every service was
// reported as having exited three seconds after starting. Nothing that READS the
// code catches that; starting a real child does, and this test does — the
// mutation back to Signal(nil) fails it on the service that stayed up.
//
// WHAT THIS TEST DOES NOT PROVE, said rather than implied: the other candidate
// repair, `Signal(syscall.Signal(0))`, SURVIVES here. It is wrong in principle —
// measured, kill(pid, 0) succeeds against a dead-but-unreaped child, so it cannot
// see the case the check exists for — but with the Wait goroutine reaping within
// milliseconds it happens to answer correctly, so a mutation to it passes. That is
// the argument for reading the Wait's own result rather than signalling: not that
// a signal is observably wrong here, but that it would be right only by racing the
// reaper, and this test cannot tell those apart.
func TestAServiceThatExitsImmediatelyIsCaughtAndOneThatDoesNotIsNot(t *testing.T) {
	t.Parallel()

	// A STAND-IN FOR `billet <role>`, because what is under test is the supervision
	// rather than billet: startAcceptanceService runs `<self> <role> --config
	// <path>`, so a shell that inspects its own first argument plays both parts.
	// `sleep 30` outlives the settle window; `exit 1` does not.
	dir := t.TempDir()
	self := filepath.Join(dir, "fake-billet")

	script := "#!/bin/sh\ncase \"$1\" in\n  server) exec sleep 30 ;;\n  node) exit 1 ;;\nesac\n"
	if err := os.WriteFile(self, []byte(script), 0o755); err != nil {
		t.Fatalf("write the stand-in: %v", err)
	}

	// A SERVICE THAT STAYS UP IS NOT REPORTED AS GONE. This is the half the
	// Signal(nil) defect broke, and it is the half that makes the command usable at
	// all.
	alive, err := startAcceptanceService(t.Context(), self, "server", "billet.yaml", dir)
	if err != nil {
		t.Fatalf("a service that stayed up was reported as having exited: %v", err)
	}

	t.Cleanup(func() { stopAcceptanceService(alive) })

	// AND ONE THAT DIES IS. This is the half Signal(0) would have broken: the child
	// is dead but unreaped when the settle window ends, and a kill(pid, 0) against a
	// zombie succeeds.
	dead, err := startAcceptanceService(t.Context(), self, "node", "billet.yaml", dir)
	if err == nil {
		stopAcceptanceService(dead)
		t.Fatal("a service that exited immediately was reported as started")
	}

	if !strings.Contains(err.Error(), "exited within") {
		t.Errorf("the failure does not say what happened: %v", err)
	}

	// AND IT NAMES THE LOG, because the reason the service refused is in it and
	// nowhere else — the child's output goes to the file, not to this process.
	if !strings.Contains(err.Error(), filepath.Join(dir, "node.log")) {
		t.Errorf("the failure does not name the log that holds the reason: %v", err)
	}
}

// STOPPING A SERVICE TWICE IS NOT AN ERROR, AND STOPPING A DEAD ONE HANGS
// NOTHING. `runAcceptance` stops both children on every path, and a teardown that
// blocked forever on a child that had already gone would be a command that never
// destroys anything.
func TestStoppingAServiceIsSafeHoweverManyTimesItHappens(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	self := filepath.Join(dir, "fake-billet")

	if err := os.WriteFile(self, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatalf("write the stand-in: %v", err)
	}

	svc, err := startAcceptanceService(t.Context(), self, "server", "billet.yaml", dir)
	if err != nil {
		t.Fatalf("startAcceptanceService: %v", err)
	}

	done := make(chan struct{})

	go func() {
		stopAcceptanceService(svc)
		stopAcceptanceService(svc)
		stopAcceptanceService(nil)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("stopping a service that had already stopped blocked; a teardown that hangs " +
			"destroys nothing")
	}
}

// THE SWEEP SEPARATES "SOMETHING IS STILL THERE" FROM "I COULD NOT LOOK".
//
// A caller acts on them differently: the first is a bill and a fleet to clean up
// by hand, the second is a run that establishes nothing either way. Reporting the
// second as the first sends an operator hunting compute that may not exist;
// reporting it as SUCCESS is the failure the whole command is built around, and
// it is the one a workflow step reading an exit status would never see.
func TestTheSweepSeparatesResidueFromNotHavingLooked(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		err         error
		residue     bool
		couldNotAsk bool
	}{
		"nothing left": {err: nil},
		"no cloud backend at all": {
			err: errors.New("billet decommission: decommission tears down the ec2 backend's " +
				"out-of-Terraform resources; this config has no ec2 node, so there is nothing " +
				"for it to remove"),
		},
		"instances still running": {
			err:     errors.New("billet decommission: 2 instance(s) are still live — stop the node"),
			residue: true,
		},
		"AWS could not be reached": {
			err:         errors.New("billet decommission: node.ec2: list instances: dial tcp: timeout"),
			couldNotAsk: true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			gotResidue := isLiveCompute(tc.err)
			if gotResidue != tc.residue {
				t.Errorf("isLiveCompute = %v, want %v, for: %v", gotResidue, tc.residue, tc.err)
			}

			gotNothing := tc.err == nil || isNoCloudBackend(tc.err)
			wantNothing := !tc.residue && !tc.couldNotAsk

			if gotNothing != wantNothing {
				t.Errorf("read as \"nothing remains\" = %v, want %v, for: %v",
					gotNothing, wantNothing, tc.err)
			}

			// AND A FAILURE THAT COULD NOT LOOK IS NEVER SILENT. It is neither
			// residue nor nothing, so it falls to the third branch — which is a
			// non-zero sweep saying it established nothing.
			if tc.couldNotAsk && (gotResidue || gotNothing) {
				t.Errorf("a sweep that could not ask was classified as an answer: %v", tc.err)
			}
		})
	}
}

// AND A RESIDUE FAILURE IS MATCHABLE, so a caller can tell the two apart without
// reading the sentence — which is what a sentinel is for.
func TestASweepThatFoundSomethingIsMatchable(t *testing.T) {
	t.Parallel()

	wrapped := fmt.Errorf("%w: %w", errAcceptanceResidue,
		errors.New("2 instance(s) are still live"))

	if !errors.Is(wrapped, errAcceptanceResidue) {
		t.Error("a sweep that found resources does not match errAcceptanceResidue")
	}
}

// THE ONE FRAGILE MATCH IS FRAGILE IN THE SAFE DIRECTION, and this is what says
// so. `decommission` returns an unwrapped error, so the live-compute answer is
// recognised by its sentence — and a REWORDED refusal stops being recognised.
// What matters is what happens then: it falls through to "could not establish",
// which is still a red sweep. A rewording that turned the sweep GREEN would be
// the defect, and this is the assertion that would catch it.
func TestARewordedRefusalStillFailsTheSweep(t *testing.T) {
	t.Parallel()

	reworded := errors.New("billet decommission: 2 instances are STILL RUNNING for this deployment")

	if isLiveCompute(reworded) {
		t.Skip("the phrase still matches, so there is nothing to prove here")
	}

	if isNoCloudBackend(reworded) {
		t.Fatal("a reworded live-compute refusal was read as \"there is no cloud backend\", " +
			"which turns a run that left instances behind into a green sweep")
	}
}

// A POSTGRESQL BASE CONFIG IS REFUSED, because an isolated identity is not an
// isolated LEDGER.
//
// `dsn_env` names an environment VARIABLE, so the derived deployment inherits the
// same one and connects to the same database. The fresh deployment identity does
// not isolate a single SQL row — and `billet acceptance down` opens that ledger
// and SEALS it. The acceptance run's teardown would seal the production
// deployment.
//
// THE OLD TEST BLESSED THIS. It asserted only the spelling and location of
// `identity_dir`, which the derivation gets right while doing the unsafe thing.
func TestAnAcceptanceRunRefusesToShareALedgerItCannotIsolate(t *testing.T) {
	t.Parallel()

	body := strings.Replace(acceptanceBaseConfig,
		"  state_dir: /var/lib/billet/server\n",
		"  identity_dir: /var/lib/billet/server\n"+
			"  state:\n    backend: postgres\n    postgres:\n      dsn_env: BILLET_STATE_DSN\n",
		1)

	_, err := deriveAcceptance(t.Context(), acceptanceInputs{
		base:      writeAcceptanceBase(t, body),
		workspace: t.TempDir(),
		prefix:    defaultLabelPrefix,
	})
	if err == nil {
		t.Fatal("a PostgreSQL base config was accepted; the derived run would have shared " +
			"the production ledger, and its teardown would have sealed it")
	}

	if !errors.Is(err, errSharedLedger) {
		t.Errorf("the refusal is not the shared-ledger one: %v", err)
	}
}

// AND PREFIXING IS NOT A PROOF OF DISJOINTNESS.
//
// With base tiers `linux` and `accept-linux`, the default prefix derives
// `accept-linux` from the first — which IS the second, an existing production
// scale set. `down` runs `teardown --all`, so the run would delete it and every
// runner registration in it. A single-tier fixture cannot see this: the collision
// is ACROSS tiers.
func TestADerivedLabelThatIsAlreadyARealTierIsRefused(t *testing.T) {
	t.Parallel()

	body := acceptanceBaseConfig + `
  - label: accept-linux-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ghcr.io/actions/actions-runner:latest
    trust: trusted
    runner_group: billet
    workflows:
      - acme/repo/.github/workflows/ci.yml@refs/heads/main
`

	_, err := deriveAcceptance(t.Context(), acceptanceInputs{
		base:      writeAcceptanceBase(t, body),
		workspace: t.TempDir(),
		prefix:    defaultLabelPrefix,
	})
	if err == nil {
		t.Fatal("a derived label equal to an existing tier was accepted; the teardown would " +
			"have deleted that deployment's scale set")
	}

	if !strings.Contains(err.Error(), "accept-linux-2vcpu") {
		t.Errorf("the refusal does not name the colliding label: %v", err)
	}
}

// THE DERIVED CONFIG'S CONTENT IS PART OF WHAT IS PROVED.
//
// Proving the PATH is not proving the FILE: replace <workspace>/billet.yaml with
// the production config and the record is untouched and the state directory still
// holds the recorded identity — after which `teardown --all` deletes the
// production scale sets and `decommission` scopes cloud deletion from that
// config's identity rather than the one just proved.
func TestASwappedConfigIsRefusedEvenThoughTheIdentityStillMatches(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	dir := t.TempDir()
	ws := deriveForTest(t, base, dir)

	// The identity and the record are untouched; only the file changes.
	if err := os.WriteFile(ws.ConfigPath, []byte(acceptanceBaseConfig), 0o600); err != nil {
		t.Fatalf("swap the config: %v", err)
	}

	if _, err := requireAcceptanceWorkspace(dir); err == nil {
		t.Fatal("a swapped derived config was accepted; every destructive step would have " +
			"acted on it")
	}
}

// A WORKSPACE BILLET DID NOT CREATE IS NEVER REMOVED, and a non-empty one is
// never adopted. `down` ends in os.RemoveAll, so "there is no acceptance.json
// here" is not consent to delete somebody's files.
func TestAnAcceptanceRunNeitherAdoptsNorDeletesADirectoryItDidNotCreate(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	dir := t.TempDir()

	keep := filepath.Join(dir, "somebody-elses-file")
	if err := os.WriteFile(keep, []byte("mine"), 0o600); err != nil {
		t.Fatalf("write the bystander: %v", err)
	}

	_, err := deriveAcceptance(t.Context(), acceptanceInputs{
		base:      base,
		workspace: dir,
		prefix:    defaultLabelPrefix,
	})
	if err == nil {
		t.Fatal("a workspace holding unrelated files was adopted; a successful teardown " +
			"would have removed them")
	}

	if _, statErr := os.Stat(keep); statErr != nil {
		t.Errorf("the bystander file is gone: %v", statErr)
	}
}

// AND A RESUME MUST BE THE SAME RUN. Re-running `up` with a different base or
// prefix keeps the identity and replaces the record, after which `down` knows
// only the NEW labels — and whatever the first attempt created is unreachable by
// anything on this machine.
func TestResumingAWorkspaceWithADifferentRunIsRefused(t *testing.T) {
	// NOT PARALLEL, PARENT OR SUBTEST: every case here derives against ONE
	// workspace on purpose, because what is under test is a second derivation
	// against a directory the first already owns.
	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	dir := t.TempDir()

	first := deriveForTest(t, base, dir)

	// The same run again is fine, and asserted so the refusals below are about
	// the CHANGE rather than about running `up` twice.
	if again := deriveForTest(t, base, dir); again.DeploymentID != first.DeploymentID {
		t.Fatalf("a plain re-run changed the identity from %s to %s",
			first.DeploymentID, again.DeploymentID)
	}

	for name, in := range map[string]acceptanceInputs{
		"a different prefix": {base: base, workspace: dir, prefix: "other"},
		"a different base":   {base: writeAcceptanceBase(t, acceptanceBaseConfig), workspace: dir, prefix: defaultLabelPrefix},
	} {
		t.Run(name, func(t *testing.T) {
			// NOT PARALLEL: these subtests share one workspace deliberately, because
			// what is under test is a SECOND derivation against a directory the first
			// already owns.
			if _, err := deriveAcceptance(t.Context(), in); err == nil {
				t.Error("the workspace was re-derived as a different run, orphaning whatever " +
					"the first attempt created")
			}
		})
	}
}

// A RESUMED RUN MUST NOT PASS ON JOBS THAT WERE ALREADY THERE.
//
// The wait counts terminal rows, and a workspace is resumable — so without a
// baseline a second `run --jobs 1` returned success on its first poll, having
// proved nothing and with no workflow ever reaching the deployment. An acceptance
// run that passes without a job is worse than one that fails.
func TestOnlyJobsThisRunWaitedForCountTowardsIt(t *testing.T) {
	t.Parallel()

	before := []acceptanceJob{
		{LeaseID: "old-1", Billet: "succeeded"},
		{LeaseID: "old-2", Billet: "failed"},
	}

	baseline := map[string]bool{"old-1": true, "old-2": true}

	if got := newTerminalJobs(before, baseline); len(got) != 0 {
		t.Errorf("jobs that finished before the wait began counted towards it: %+v", got)
	}

	after := append(append([]acceptanceJob(nil), before...),
		acceptanceJob{LeaseID: "new-1", Billet: "succeeded"},
		// STILL RUNNING, so it does not count either: a job billet has not
		// concluded is one whose compute may still be there.
		acceptanceJob{LeaseID: "new-2"},
	)

	got := newTerminalJobs(after, baseline)
	if len(got) != 1 || got[0].LeaseID != "new-1" {
		t.Errorf("the wait counted %+v; only the new, finished job should count", got)
	}
}

// THE SWEEP HAS THREE ANSWERS AND A BACKEND IT CANNOT ASK IS THE THIRD.
//
// `billet decommission` knows only the ec2 backend, so on docker, firecracker,
// tart or codebuild it refuses WITHOUT LOOKING — and an earlier version read that
// refusal as a clean sweep. That is the could-not-tell/no collapse, and here it
// would have printed "nothing remains" about a host whose node crashed after
// launching a container.
func TestABackendTheSweepCannotAskIsNotReportedAsClean(t *testing.T) {
	t.Parallel()

	base := writeAcceptanceBase(t, acceptanceBaseConfig)
	dir := t.TempDir()
	ws := deriveForTest(t, base, dir)

	// The fixture is a DOCKER deployment, which is exactly the case: decommission
	// refuses it, and that refusal is not evidence of anything.
	err := sweepAcceptance(t.Context(), ws, false)
	if err == nil {
		t.Fatal("a backend the sweep cannot ask was reported as clean")
	}

	if !errors.Is(err, errAcceptanceUnswept) {
		t.Errorf("the failure is not the unswept one: %v", err)
	}

	// AND THE BARRIER IS WHAT MAKES IT CLEAN. alloc.ComputeClear asks each HOST
	// through its own provider, which covers every backend — so a run that got a
	// clearance while its control plane was up has been swept by something
	// stronger than the ec2 inventory.
	if err := sweepAcceptance(t.Context(), ws, true); err != nil {
		t.Errorf("a run whose compute barrier proved every host clear was still reported "+
			"unswept: %v", err)
	}
}
