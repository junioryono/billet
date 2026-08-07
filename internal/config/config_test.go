package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "billet.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// The client_id GitHub hands back at conversion is OPTIONAL and must stay so.
//
// scaleset's GitHubAppAuth documents ClientID as "the Client ID of the
// application (app id also works)", so every existing config keeps working —
// which is why this cannot become required. It is captured because GitHub's
// newer guidance prefers it as the JWT issuer, and because onboarding already
// fetches it: discarding a value billet was handed is how a config ends up
// needing a second trip through the browser to reconstruct.
func TestClientIDIsOptionalAndRoundTrips(t *testing.T) {
	withID := strings.Replace(validConfig,
		"  app_id: 12345\n", "  app_id: 12345\n  client_id: Iv1.a1b2c3d4e5f6\n", 1)

	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(withID), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("a config carrying client_id was rejected: %v", err)
	}

	if cfg.GitHub.ClientID != "Iv1.a1b2c3d4e5f6" {
		t.Errorf("github.client_id = %q, want it preserved", cfg.GitHub.ClientID)
	}

	// And its absence must stay valid — app_id is still accepted by scaleset, so
	// requiring this would break every config written before it existed.
	plain := &Config{}
	if err := yaml.Unmarshal([]byte(validConfig), plain); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	plain.applyDefaults()

	if err := plain.Validate(); err != nil {
		t.Fatalf("a config without client_id was rejected: %v", err)
	}
}

const validConfig = `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 120
  max_memory: 480GiB
github:
  org: acme
  app_id: 12345
  installation_id: 67890
  private_key_path: /etc/billet/app.pem
node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: firecracker
  state_dir: /var/lib/billet/node
  firecracker:
    kernel_image: /var/lib/billet/vmlinux
    zfs_pool: tank
tiers:
  - label: billet-4vcpu-ubuntu-2404
    provider: firecracker
    vcpu: 4
    memory: 16GiB
    disk: 80GiB
    image: ubuntu-2404-x64
  - label: billet-8vcpu-ubuntu-2404
    provider: firecracker
    vcpu: 8
    memory: 32GiB
    shm: 1GiB
    image: ubuntu-2404-x64
    intercept: true
`

func TestLoadValid(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := len(cfg.Tiers); got != 2 {
		t.Fatalf("tiers = %d, want 2", got)
	}
	tier, ok := cfg.TierByLabel("billet-8vcpu-ubuntu-2404")
	if !ok {
		t.Fatal("TierByLabel did not find the 8vcpu tier")
	}
	if tier.Memory != 32*GiB {
		t.Errorf("memory = %s, want 32GiB", tier.Memory)
	}
	if tier.SHM != GiB {
		t.Errorf("shm = %s, want 1GiB", tier.SHM)
	}
	if !tier.Intercept {
		t.Error("intercept should be true where set")
	}
	if tier.GuestOS != GuestLinux {
		t.Errorf("guest_os = %q, want linux by default", tier.GuestOS)
	}
	// Interception must stay off unless explicitly enabled: it terminates TLS in
	// front of the service that also carries release-artifact metadata.
	first, _ := cfg.TierByLabel("billet-4vcpu-ubuntu-2404")
	if first.Intercept {
		t.Error("intercept defaulted to true; it must default to false")
	}
}

func TestLoadRejectsUnknownFields(t *testing.T) {
	// A typo in a CI config should fail loudly rather than silently doing
	// something other than what the operator wrote.
	body := strings.Replace(validConfig, "max_vcpu: 120", "max_vcpus: 120", 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted an unknown field")
	}
}

func TestLoadRejectsSecondDocument(t *testing.T) {
	body := validConfig + "\n---\nserver:\n  max_vcpu: 999\n"
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load silently ignored a second YAML document")
	}
	if !strings.Contains(err.Error(), "more than one YAML document") {
		t.Errorf("unexpected error: %v", err)
	}
}

const macOSTier = `
  - label: billet-6vcpu-macos-26
    provider: tart
    guest_os: macos
    node: mac-mini-1
    vcpu: 6
    memory: 24GiB
    image: macos-26
`

func TestMacOSTierCappedAtAppleLimit(t *testing.T) {
	body := validConfig + strings.Replace(macOSTier,
		"    image: macos-26\n", "    image: macos-26\n    max_concurrent: 4\n", 1)
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted max_concurrent above Apple's 2-VM licensing limit")
	}
	if !strings.Contains(err.Error(), "Apple's licence") {
		t.Errorf("error should explain the licensing limit, got: %v", err)
	}
}

func TestMacOSTierDefaultsToAppleLimit(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig+macOSTier))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tier, ok := cfg.TierByLabel("billet-6vcpu-macos-26")
	if !ok {
		t.Fatal("macOS tier missing")
	}
	if tier.MaxConcurrent != DefaultMacOSVMLimit {
		t.Errorf("max_concurrent = %d, want %d by default", tier.MaxConcurrent, DefaultMacOSVMLimit)
	}
}

func TestMacOSTierRequiresTart(t *testing.T) {
	body := validConfig + strings.Replace(macOSTier, "provider: tart", "provider: firecracker", 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted a macOS tier on the firecracker provider")
	}
}

// The licence limit must key off guest_os, not off the label. Inferring it from
// a user-chosen name means a tier called "sonoma-arm64" escapes the cap
// entirely — which is precisely the bypass this test exists to prevent.
func TestMacOSLimitIsNotLabelDependent(t *testing.T) {
	body := validConfig + `
  - label: sonoma-arm64
    provider: tart
    guest_os: macos
    node: mac-mini-1
    vcpu: 6
    memory: 24GiB
    image: macos-26
    max_concurrent: 4
`
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("a macOS tier whose label omits 'macos' escaped Apple's limit")
	}
	if !strings.Contains(err.Error(), "Apple's licence") {
		t.Errorf("error should cite the licence limit, got: %v", err)
	}
}

// Conversely, a Linux tier that merely has "macos" in its name must not be
// treated as a macOS guest.
func TestLinuxTierWithMacOSInLabelIsNotCapped(t *testing.T) {
	body := validConfig + `
  - label: builds-macos-artifacts
    provider: firecracker
    vcpu: 4
    memory: 16GiB
    image: ubuntu-2404-x64
    max_concurrent: 16
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load rejected a Linux tier because of its label: %v", err)
	}
	tier, _ := cfg.TierByLabel("builds-macos-artifacts")
	if tier.GuestOS != GuestLinux {
		t.Errorf("guest_os = %q, want linux", tier.GuestOS)
	}
	if tier.MaxConcurrent != 16 {
		t.Errorf("max_concurrent = %d, want 16", tier.MaxConcurrent)
	}
}

// Two individually-legal macOS tiers on one Mac still share one physical host.
func TestMacOSTiersShareTheHostLimit(t *testing.T) {
	body := validConfig + macOSTier + `
  - label: billet-12vcpu-macos-26
    provider: tart
    guest_os: macos
    node: mac-mini-1
    vcpu: 12
    memory: 48GiB
    image: macos-26
`
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("two macOS tiers on one node totalling 4 guests were accepted")
	}
	if !strings.Contains(err.Error(), "per Apple-branded host") {
		t.Errorf("error should cite the per-host limit, got: %v", err)
	}
}

func TestMacOSTierRequiresExplicitNode(t *testing.T) {
	body := validConfig + strings.Replace(macOSTier, "    node: mac-mini-1\n", "", 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("a macOS tier with no node pin was accepted; the per-host limit cannot be enforced")
	}
}

// Warm instances are running instances, so a warm pool above the cap would sit
// permanently over the limit with no job ever running.
func TestWarmPoolCannotExceedMaxConcurrent(t *testing.T) {
	body := validConfig + `
  - label: billet-warm
    provider: firecracker
    vcpu: 4
    memory: 16GiB
    image: ubuntu-2404-x64
    max_concurrent: 2
    warm_pool: 5
`
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("warm_pool above max_concurrent was accepted")
	}
	if !strings.Contains(err.Error(), "count against the cap") {
		t.Errorf("unexpected error: %v", err)
	}
}

// Without a ceiling the allocator has nothing to escrow against, so concurrent
// scale-set listeners can each advertise their own maximum.
func TestServerCapacityCeilingsAreRequired(t *testing.T) {
	for _, replace := range []string{"max_vcpu: 120", "max_memory: 480GiB"} {
		body := strings.Replace(validConfig, replace, strings.Split(replace, ":")[0]+": 0", 1)
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Fatalf("Load accepted %s = 0", strings.Split(replace, ":")[0])
		}
		if !strings.Contains(err.Error(), "escrows against") {
			t.Errorf("error should explain why the ceiling matters, got: %v", err)
		}
	}
}

func TestTierExceedingServerCapacityIsRejected(t *testing.T) {
	body := strings.Replace(validConfig, "max_vcpu: 120", "max_vcpu: 4", 1)
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a tier requesting more vCPU than server.max_vcpu")
	}
	if !strings.Contains(err.Error(), "never be schedulable") {
		t.Errorf("error should say the tier is unschedulable, got: %v", err)
	}
}

func TestInvalidListenAddressRejected(t *testing.T) {
	for _, addr := range []string{"not-a-host-port", "127.0.0.1", "127.0.0.1:notaport", "127.0.0.1:99999"} {
		body := strings.Replace(validConfig, "listen: 127.0.0.1:7717", "listen: "+addr, 1)
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("Load accepted listen address %q", addr)
		}
	}
}

func TestNegativeGitHubIDsRejected(t *testing.T) {
	body := strings.Replace(validConfig, "app_id: 12345", "app_id: -1", 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted a negative app_id")
	}
}

// A config this broken should take one round trip to fix, not eight. Assert the
// specific diagnostics rather than counting lines, so the test still means
// something if the formatting changes.
func TestValidateReportsAllProblemsAtOnce(t *testing.T) {
	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: ""
  app_id: 0
  installation_id: 0
  private_key_path: ""
tiers:
  - label: "bad label with spaces"
    provider: nonsense
    vcpu: 0
    memory: 1GiB
    image: ""
`
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a config with many problems")
	}
	msg := err.Error()
	for _, want := range []string{
		"github.org is required",
		"github.app_id is required",
		"github.installation_id is required",
		"github.private_key_path is required",
		"label must match",
		"provider \"nonsense\"",
		"vcpu must be positive",
		"image is required",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing diagnostic %q in:\n%v", want, err)
		}
	}
}

func TestDuplicateTierLabelRejected(t *testing.T) {
	body := validConfig + `
  - label: billet-4vcpu-ubuntu-2404
    provider: firecracker
    vcpu: 4
    memory: 16GiB
    image: ubuntu-2404-x64
`
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a duplicate tier label")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error should mention the duplicate, got: %v", err)
	}
}

func TestFirecrackerNodeRequiresKernelAndPool(t *testing.T) {
	body := strings.Replace(validConfig,
		"  firecracker:\n    kernel_image: /var/lib/billet/vmlinux\n    zfs_pool: tank\n", "", 1)
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted a firecracker node with no firecracker section")
	}
}

func TestServerRequiresGitHubSection(t *testing.T) {
	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 8
  max_memory: 32GiB
`
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a server with no github section")
	}
	if !strings.Contains(err.Error(), "github section is required") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestMissingInstallationIDIsExplained(t *testing.T) {
	// Creating a GitHub App does not install it, and the resulting failure is
	// otherwise confusing, so the message has to say so.
	body := strings.Replace(validConfig, "installation_id: 67890", "installation_id: 0", 1)
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a config with no installation id")
	}
	if !strings.Contains(err.Error(), "does not install it") {
		t.Errorf("error should explain that creation and installation differ, got: %v", err)
	}
}

// The shipped example is the first file every adopter copies, and nothing else
// proves it is still correct. KnownFields(true) makes this catch the specific
// way it rots: a field renamed in this package leaves the example naming a key
// that no longer exists, and the operator's first run fails on a config they
// did not write.
//
// It cannot go through Load, because app_id and installation_id are deliberately
// zero placeholders that `billet github-app create` fills in. Filling them here
// exercises everything else.
func TestExampleConfigIsValid(t *testing.T) {
	path := filepath.Join("..", "..", "billet.example.yaml")

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open example: %v", err)
	}

	defer f.Close()

	var c Config

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	if err := dec.Decode(&c); err != nil {
		t.Fatalf("billet.example.yaml does not parse against the current schema: %v", err)
	}

	// Load rejects a second YAML document, because a config that assigns capacity
	// must not have half of itself silently ignored. Decoding once here would
	// pass an example that real Load refuses.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		t.Fatal("billet.example.yaml contains more than one YAML document; Load would reject it")
	}

	c.GitHub.AppID, c.GitHub.InstallationID = 1, 1

	c.applyDefaults()

	if err := c.Validate(); err != nil {
		t.Errorf("billet.example.yaml does not validate: %v", err)
	}
}

// --- Per-host node policy -------------------------------------------------
//
// A host's capabilities are not implied by its provider. An Apple Silicon
// machine can serve macOS guests, Linux arm64 guests, or both, and which of
// those an operator wants is a deployment decision rather than a fact about the
// hardware.

// linuxARMTier is a Linux tier on the Mac — the arm64 builder case.
const linuxARMTier = `
  - label: billet-4vcpu-ubuntu-2404-arm
    provider: tart
    guest_os: linux
    node: mac-mini-1
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-arm64
`

func nodesSection(body string) string {
	return nodesSectionFor("mac-mini-1", body)
}

func nodesSectionFor(name, body string) string {
	return "nodes:\n  - name: " + name + "\n" + body
}

// Dedicating a Mac to macOS means Linux tiers pinned to it are a configuration
// error, not something that quietly queues forever.
func TestNodeAllowlistRejectsUnlistedGuestOS(t *testing.T) {
	body := validConfig + macOSTier + linuxARMTier + nodesSection("    guest_os: [macos]\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a linux tier pinned to a macos-only node")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error should name the allowlist, got: %v", err)
	}
}

// The converse: a Mac used purely as an arm64 Linux builder must refuse macOS
// tiers, so "we never boot macOS on this host" is enforceable rather than a
// convention.
func TestNodeAllowlistRejectsMacOSOnALinuxOnlyHost(t *testing.T) {
	body := validConfig + macOSTier + nodesSection("    guest_os: [linux]\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a macos tier pinned to a linux-only node")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error should name the allowlist, got: %v", err)
	}
}

// Listing both is the documented default arrangement and must stay legal.
func TestNodeAllowlistAcceptsBothGuestOS(t *testing.T) {
	body := validConfig + macOSTier + linuxARMTier + nodesSection("    guest_os: [macos, linux]\n")
	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("Load rejected a host serving both guest types: %v", err)
	}
}

// An absent nodes section must behave exactly as it did before the section
// existed.
func TestNoNodePolicyKeepsDefaultBehaviour(t *testing.T) {
	cfg, err := Load(writeConfig(t, validConfig+macOSTier+linuxARMTier))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.MacOSLimitForNode("mac-mini-1"); got != DefaultMacOSVMLimit {
		t.Errorf("undeclared node limit = %d, want the default %d", got, DefaultMacOSVMLimit)
	}
}

// Lowering a host's limit must tighten the tiers pinned to it. If the tier
// default stayed at the package constant, reserving a slot for interactive use
// would be silently ignored.
func TestNodeMacOSLimitLowersTierDefault(t *testing.T) {
	body := validConfig + macOSTier + nodesSection("    macos_vm_limit: 1\n")
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tier, ok := cfg.TierByLabel("billet-6vcpu-macos-26")
	if !ok {
		t.Fatal("macOS tier missing")
	}
	if tier.MaxConcurrent != 1 {
		t.Errorf("max_concurrent = %d, want 1 from the node's limit", tier.MaxConcurrent)
	}
}

func TestNodeMacOSLimitBoundsTierMaxConcurrent(t *testing.T) {
	body := validConfig +
		strings.Replace(macOSTier, "    image: macos-26\n", "    image: macos-26\n    max_concurrent: 2\n", 1) +
		nodesSection("    macos_vm_limit: 1\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a tier exceeding its host's macOS limit")
	}
	if !strings.Contains(err.Error(), `node "mac-mini-1"`) {
		t.Errorf("error should name the host whose limit was exceeded, got: %v", err)
	}
}

func TestNodeMacOSLimitZeroRejectsMacOSTier(t *testing.T) {
	body := validConfig + macOSTier + nodesSection("    macos_vm_limit: 0\n")
	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Fatal("Load accepted a macOS tier on a host whose macOS limit is 0")
	}
}

// Both fields decide whether macOS runs here. Letting the allowlist quietly win
// would mean a config that reads as "two macOS guests" schedules none.
func TestMacOSLimitContradictingAllowlistIsRejected(t *testing.T) {
	body := validConfig + nodesSection("    guest_os: [linux]\n    macos_vm_limit: 2\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted macos_vm_limit > 0 on a host that disallows macos")
	}
	if !strings.Contains(err.Error(), "excludes macos") {
		t.Errorf("error should explain the contradiction, got: %v", err)
	}
}

// The per-host sum is what catches two individually-legal tiers overrunning one
// Mac, and it must use that Mac's limit rather than the package default.
func TestPerHostSumUsesTheNodeLimit(t *testing.T) {
	second := `
  - label: billet-12vcpu-macos-26
    provider: tart
    guest_os: macos
    node: mac-mini-1
    vcpu: 12
    memory: 48GiB
    image: macos-26
    max_concurrent: 1
`
	body := validConfig +
		strings.Replace(macOSTier, "    image: macos-26\n", "    image: macos-26\n    max_concurrent: 1\n", 1) +
		second + nodesSection("    macos_vm_limit: 1\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("two tiers of 1 guest each were accepted on a host limited to 1")
	}
	if !strings.Contains(err.Error(), "macos_vm_limit") {
		t.Errorf("error should point at the operator's own field, got: %v", err)
	}
}

// billet cannot know what licence or hardware agreement an operator has, so
// raising the limit is permitted. The default is what protects the config that
// says nothing.
func TestNodeMacOSLimitMayExceedTheDefault(t *testing.T) {
	body := validConfig +
		strings.Replace(macOSTier, "    image: macos-26\n", "    image: macos-26\n    max_concurrent: 4\n", 1) +
		nodesSection("    macos_vm_limit: 4\n")
	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("Load rejected an explicitly raised host limit: %v", err)
	}
}

func TestNodePolicyRejectsBadInput(t *testing.T) {
	for name, tc := range map[string]struct{ body, want string }{
		"duplicate name":   {"nodes:\n  - name: mac-mini-1\n  - name: mac-mini-1\n", "duplicate node name"},
		"blank name":       {"nodes:\n  - name: \"\"\n", "node name \"\" must match"},
		"unknown guest_os": {nodesSection("    guest_os: [plan9]\n"), "is not one of"},
		"duplicate guest":  {nodesSection("    guest_os: [linux, linux]\n"), "duplicate guest_os"},
		"negative limit":   {nodesSection("    macos_vm_limit: -1\n"), "must not be negative"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeConfig(t, validConfig+tc.body))
			if err == nil {
				t.Fatalf("Load accepted %s", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// An UNPINNED tier can be placed on any host, so a restrictive allowlist
// anywhere in the fleet is a constraint on it. Checking the allowlist only for
// pinned tiers left the door open: a macos-only Mac could still be handed a
// Linux guest, because nothing tied the tier to a host at all.
func TestUnpinnedTierMustBePinnedWhenAHostExcludesIt(t *testing.T) {
	unpinned := `
  - label: billet-4vcpu-ubuntu-2404-arm
    provider: tart
    guest_os: linux
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-arm64
`
	body := validConfig + unpinned + nodesSection("    provider: tart\n    guest_os: [macos]\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("an unpinned linux tier was accepted alongside a macos-only host")
	}
	if !strings.Contains(err.Error(), "mac-mini-1") {
		t.Errorf("error should name the host that excludes it, got: %v", err)
	}
}

// ...but only for hosts that could actually serve it. A firecracker tier can
// never land on a Tart host, so declaring one macOS-only Mac must not turn
// every ordinary x64 Linux tier in the deployment into an error. validConfig's
// two unpinned firecracker tiers are the guard here.
func TestUnpinnedTierIgnoresHostsRunningAnotherProvider(t *testing.T) {
	body := validConfig + nodesSection("    provider: tart\n    guest_os: [macos]\n")
	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("a macos-only Tart host must not conflict with unpinned firecracker tiers: %v", err)
	}
}

// Pinning it elsewhere resolves the problem, so the guard has an exit that is
// not "delete the node policy".
func TestPinningResolvesTheUnpinnedConflict(t *testing.T) {
	pinned := `
  - label: billet-4vcpu-ubuntu-2404-arm
    provider: firecracker
    guest_os: linux
    node: epyc-1
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-x64
`
	body := validConfig + pinned + nodesSection("    guest_os: [macos]\n")
	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Fatalf("pinning the tier elsewhere should resolve it: %v", err)
	}
}

// One bad field must produce one diagnostic. Defaulting a tier's max_concurrent
// from a negative node limit previously cascaded into three, including the
// self-evidently broken "must be between 1 and -1".
func TestNegativeNodeLimitDoesNotCascade(t *testing.T) {
	body := validConfig + macOSTier + nodesSection("    macos_vm_limit: -1\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a negative macos_vm_limit")
	}
	// Asserted whole rather than by exclusion: listing phrases that must NOT
	// appear stays green when a NEW second diagnostic arrives with different
	// wording, which is exactly the cascade this guards against.
	const want = `node "mac-mini-1": macos_vm_limit must not be negative`
	// Exact, not a suffix: a suffix match permits arbitrary diagnostics BEFORE
	// the expected one, which is precisely the cascade this test exists to
	// detect.
	if got := lastLine(err); got != want {
		t.Errorf("want exactly one diagnostic %q, got:\n%v", want, err)
	}
}

// lastLine strips the "invalid config <path>: " prefix errors.Join composes and
// returns what remains, so a test can assert one whole diagnostic.
func lastLine(err error) string {
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 1 {
		return err.Error() // more than one diagnostic; return it all so the failure shows them
	}

	_, msg, found := strings.Cut(lines[0], ".yaml: ")
	if !found {
		return lines[0]
	}

	return msg
}

// Node identifiers are one namespace and must be validated the same way
// everywhere. They were not: nodes[].name matched labelRe, node.name only had
// to be non-blank, and tiers[].node was never checked at all.
//
// A whitespace-only pin is the case that shows why it matters. config treats it
// as pinned — a macOS tier passes the "must name a node" rule and inherits a
// concurrency default — while alloc trims it to empty and rejects the same
// tier. For a Linux tier the trim silently converts a pin into no pin at all.
func TestNodeNamesAreValidatedConsistently(t *testing.T) {
	for name, body := range map[string]string{
		"blank tier pin": validConfig + `
  - label: billet-6vcpu-macos-26
    provider: tart
    guest_os: macos
    node: "   "
    vcpu: 6
    memory: 24GiB
    image: macos-26
`,
		// A LINUX tier, deliberately. The macOS case above would fail anyway on
		// the separate "macOS must pin a node" rule, so it could not tell
		// whether the pin was being validated at all.
		"blank linux tier pin": validConfig + `
  - label: billet-4vcpu-blank-pin
    provider: firecracker
    guest_os: linux
    node: "   "
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-x64
`,
		"blank node name":     strings.Replace(validConfig, "name: epyc-1", `name: "   "`, 1),
		"spaces in node name": strings.Replace(validConfig, "name: epyc-1", `name: "epyc 1"`, 1),
		"spaces in tier pin": validConfig + `
  - label: billet-4vcpu-arm
    provider: tart
    guest_os: linux
    node: "mac mini"
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-arm64
`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Load(writeConfig(t, body)); err == nil {
				t.Errorf("Load accepted %s", name)
			}
		})
	}
}

// node.name defaults to the machine's hostname, and a hostname is not
// guaranteed to be a legal node name — a long FQDN exceeds the length limit and
// some contain characters the pattern rejects. Tightening node.name to labelRe
// means such a machine now fails to load a config where the operator never
// wrote a name at all, so the diagnostic has to say where the name came from.
// "node.name is invalid" sends them looking for a field they never typed.
func TestHostnameDefaultExplainsItself(t *testing.T) {
	body := strings.Replace(validConfig, "  name: epyc-1\n", "", 1)

	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(body), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Simulate a machine whose hostname cannot be a node name.
	cfg.hostname = func() (string, error) { return "an invalid host name!", nil }
	cfg.applyDefaults()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted an unusable hostname-derived node name")
	}
	if !strings.Contains(err.Error(), "hostname") {
		t.Errorf("error should say the name came from the hostname, got: %v", err)
	}
	if !strings.Contains(err.Error(), "an invalid host name!") {
		t.Errorf("error should quote the offending hostname, got: %v", err)
	}
}

// An EMPTY hostname is still a defaulted name, and the operator still never
// typed one.
//
// Tracking provenance in the derived string conflated "billet supplied this"
// with "the result was non-empty", so a machine reporting no hostname at all —
// which happens in containers — fell back to the generic "node.name is invalid".
// That is the exact wording the hostname branch exists to avoid, in the case
// where it is least obvious what happened.
func TestBlankHostnameStillExplainsItself(t *testing.T) {
	body := strings.Replace(validConfig, "  name: epyc-1\n", "", 1)

	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(body), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cfg.hostname = func() (string, error) { return "", nil }
	cfg.applyDefaults()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate accepted an empty hostname-derived node name")
	}

	if !strings.Contains(err.Error(), "hostname") {
		t.Errorf("error should say the name came from the hostname, got: %v", err)
	}
}

// A usable hostname is still adopted silently, which is the whole point of the
// default on a single-box deployment.
func TestUsableHostnameIsAdopted(t *testing.T) {
	body := strings.Replace(validConfig, "  name: epyc-1\n", "", 1)

	cfg := &Config{}
	if err := yaml.Unmarshal([]byte(body), cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	cfg.hostname = func() (string, error) { return "build-box-01", nil }
	cfg.applyDefaults()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Node.Name != "build-box-01" {
		t.Errorf("node.name = %q, want the hostname", cfg.Node.Name)
	}
}

// Surrounding whitespace is normalized rather than rejected, so a pin and the
// fleet entry it names still match.
func TestNodeNamesAreTrimmed(t *testing.T) {
	body := strings.Replace(validConfig, "name: epyc-1", `name: "  epyc-1  "`, 1)

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Node.Name != "epyc-1" {
		t.Errorf("node.name = %q, want it trimmed", cfg.Node.Name)
	}
}

// The local node section and a fleet entry naming the SAME host must not
// disagree. Believing the fleet entry means the unpinned-tier check compares
// against a provider the machine does not run, so it skips a host that is in
// single-box mode the only host there is.
func TestLocalNodeAndFleetEntryMustAgreeOnProvider(t *testing.T) {
	body := validConfig + nodesSectionFor("epyc-1", "    provider: tart\n    guest_os: [macos]\n")

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a fleet entry contradicting the local node's provider")
	}
	if !strings.Contains(err.Error(), "epyc-1") {
		t.Errorf("error should name the host, got: %v", err)
	}
}

// A fleet entry that omits its provider inherits the local node's, so the
// unpinned-tier check has the real provider to compare against.
func TestFleetEntryInheritsTheLocalProvider(t *testing.T) {
	body := validConfig + nodesSectionFor("epyc-1", "    guest_os: [macos]\n")

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("validConfig's unpinned firecracker tiers should conflict with a macos-only local host")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error should name the allowlist, got: %v", err)
	}
}

// An invalid provider on a node policy must not be copied into a tier, or one
// typo produces a second diagnostic blaming a field nobody wrote.
func TestInvalidNodeProviderIsNotInherited(t *testing.T) {
	body := validConfig + `
  - label: billet-4vcpu-inherit
    guest_os: linux
    node: mac-mini-1
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-arm64
` + nodesSection("    provider: nonsense\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted an unknown provider on a node policy")
	}
	if strings.Count(err.Error(), "is not one of") > 1 {
		t.Errorf("one bad provider produced more than one diagnostic:\n%v", err)
	}
}

// A tier with no provider inherits the PINNED host's, not the local node's.
//
// Defaulting from the local node is right for an unpinned tier and wrong for a
// pinned one: on a multi-host deployment the file that describes the EPYC box
// would silently stamp `firecracker` onto a tier pinned to a Mac. Pinning names
// a host, and that host's declared provider is the more specific answer.
func TestPinnedTierInheritsItsHostProvider(t *testing.T) {
	body := validConfig + `
  - label: billet-4vcpu-ubuntu-2404-arm
    guest_os: linux
    node: mac-mini-1
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-arm64
` + nodesSection("    provider: tart\n    guest_os: [linux]\n")
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tier, ok := cfg.TierByLabel("billet-4vcpu-ubuntu-2404-arm")
	if !ok {
		t.Fatal("tier missing")
	}
	// validConfig's node section declares firecracker; the pinned host is tart.
	if tier.Provider != ProviderTart {
		t.Errorf("provider = %q, want %q from the pinned host", tier.Provider, ProviderTart)
	}
}

// An UNPINNED tier still inherits the local node's provider, which is the
// single-box case the default exists for.
func TestUnpinnedTierInheritsTheLocalProvider(t *testing.T) {
	body := validConfig + `
  - label: billet-2vcpu-plain
    vcpu: 2
    memory: 8GiB
    image: ubuntu-2404-x64
`
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	tier, ok := cfg.TierByLabel("billet-2vcpu-plain")
	if !ok {
		t.Fatal("tier missing")
	}
	if tier.Provider != ProviderFirecracker {
		t.Errorf("provider = %q, want %q from the local node", tier.Provider, ProviderFirecracker)
	}
}

// A pinned tier whose provider differs from its host's loads cleanly and can
// never be placed — the host cannot run it. Silent at load time, that is a job
// that queues forever.
func TestPinnedTierMustMatchItsHostProvider(t *testing.T) {
	body := validConfig + `
  - label: billet-4vcpu-mismatched
    provider: firecracker
    guest_os: linux
    node: mac-mini-1
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-x64
` + nodesSection("    provider: tart\n    guest_os: [linux]\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("a firecracker tier pinned to a tart host was accepted")
	}
	if !strings.Contains(err.Error(), "which runs tart") {
		t.Errorf("error should name the host's actual provider, got: %v", err)
	}
}

// One invalid enum must produce one diagnostic. Feeding a typo into the
// relational allowlist check produced a second describing the same mistake in
// terms that send the reader to the wrong field.
func TestInvalidGuestOSDoesNotCascadeIntoAllowlistErrors(t *testing.T) {
	body := validConfig + `
  - label: billet-4vcpu-bad-guest
    provider: tart
    guest_os: plan9
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-arm64
` + nodesSection("    provider: tart\n    guest_os: [linux]\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted an unknown guest_os")
	}
	if !strings.Contains(err.Error(), "is not one of") {
		t.Errorf("error should report the unknown value, got: %v", err)
	}
	if strings.Contains(err.Error(), "allowlist") {
		t.Errorf("an unknown guest_os cascaded into an allowlist diagnostic, got: %v", err)
	}
}

// The same in the other direction: a typo in a NODE's allowlist must not be
// reported again as a tier mismatch.
func TestInvalidNodeGuestOSDoesNotCascadeIntoTierErrors(t *testing.T) {
	body := validConfig + macOSTier + nodesSection("    provider: tart\n    guest_os: [plan9]\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted an unknown guest_os in a node allowlist")
	}
	// Both halves matter: the required diagnostic must be present, AND the
	// cascade must be absent. Asserting only the absence passes when validation
	// reports nothing useful at all.
	if !strings.Contains(err.Error(), "is not one of") {
		t.Errorf("error should report the unknown value, got: %v", err)
	}
	if strings.Contains(err.Error(), "allowlist") {
		t.Errorf("an unknown node guest_os cascaded into a tier diagnostic, got: %v", err)
	}
}

// The same short-circuit must cover an invalid PROVIDER, not just guest_os: a
// pinned tier with a nonsense provider otherwise gets both the primary
// diagnostic and a relational "runs tart" one describing the same typo.
func TestInvalidTierProviderDoesNotCascade(t *testing.T) {
	body := validConfig + `
  - label: billet-4vcpu-bad-provider
    provider: nonsense
    guest_os: linux
    node: mac-mini-1
    vcpu: 4
    memory: 12GiB
    image: ubuntu-2404-arm64
` + nodesSection("    provider: tart\n    guest_os: [linux]\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted an unknown provider")
	}
	if !strings.Contains(err.Error(), "is not one of") {
		t.Errorf("error should report the unknown provider, got: %v", err)
	}
	if strings.Contains(err.Error(), "which runs") {
		t.Errorf("an unknown provider cascaded into a pinning diagnostic, got: %v", err)
	}
}

// A host that excludes macOS has an effective limit of zero, and zero is not
// Apple's number. Reporting "1 concurrent guest exceeds Apple's limit of 2" is
// arithmetically false and sends the operator looking at the wrong field.
func TestExcludedMacOSHostDoesNotCiteAppleArithmetic(t *testing.T) {
	body := validConfig +
		strings.Replace(macOSTier, "    image: macos-26\n", "    image: macos-26\n    max_concurrent: 1\n", 1) +
		nodesSection("    guest_os: [linux]\n")
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a macOS tier on a linux-only host")
	}
	if !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("error should name the allowlist, got: %v", err)
	}
	if strings.Contains(err.Error(), "Apple's licence limit") {
		t.Errorf("a host that excludes macOS must not be described via Apple's limit, got: %v", err)
	}
}

// NodePolicies is what the allocator is built from, so the runtime checks and
// this package's load-time guard cannot disagree about a host.
func TestNodePoliciesCarriesTheDeclaredPolicy(t *testing.T) {
	body := validConfig + macOSTier + linuxARMTier +
		nodesSection("    guest_os: [macos, linux]\n    macos_vm_limit: 1\n")
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	policies := cfg.NodePolicies()
	if len(policies) != 1 {
		t.Fatalf("NodePolicies() = %v, want one entry", policies)
	}
	p := policies["mac-mini-1"]
	if p.MacOSLimit() != 1 {
		t.Errorf("MacOSLimit() = %d, want 1", p.MacOSLimit())
	}
	if !p.AllowsGuestOS(GuestLinux) || !p.AllowsGuestOS(GuestMacOS) {
		t.Errorf("allowlist %v should permit both declared guest types", p.GuestOS)
	}

	// The returned slices must not alias the config, or a caller mutating one
	// changes the rules the allocator was built with.
	p.GuestOS[0] = GuestWindows
	if cfg.Nodes[0].GuestOS[0] == GuestWindows {
		t.Error("NodePolicies() aliased the config's guest_os slice")
	}
}
