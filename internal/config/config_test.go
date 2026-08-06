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
	return "nodes:\n  - name: mac-mini-1\n" + body
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
		"blank name":       {"nodes:\n  - name: \"\"\n", "name must match"},
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

// MacOSLimits is what the allocator is built from, so the runtime cap and this
// package's load-time guard cannot disagree about a host.
func TestMacOSLimitsMapsEveryPinnedHost(t *testing.T) {
	body := validConfig + macOSTier + linuxARMTier + nodesSection("    macos_vm_limit: 1\n")
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	limits := cfg.MacOSLimits()
	if len(limits) != 1 || limits["mac-mini-1"] != 1 {
		t.Errorf("MacOSLimits() = %v, want {mac-mini-1:1}", limits)
	}
}
