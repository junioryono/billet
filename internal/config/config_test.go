package config

import (
	"fmt"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// A runner group name billet cannot look up safely is refused at load, not left
// for GitHub to answer confusingly.
//
// The scale-set client interpolates the name into a query string unescaped, so
// "Platform & Security" — an entirely ordinary group name — is parsed as two
// parameters and returns "group not found". That reads as a permissions problem
// and sends the operator to the wrong page.
func TestRunnerGroupMustBeQueryable(t *testing.T) {
	for name, group := range map[string]string{
		"ampersand": "Platform & Security",
		"hash":      "team#1",
		"semicolon": "build;test",
		"percent":   "fifty%",
		"plus":      "a+b",
		"newline":   "team\nname",
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkRunnerGroup(group); err == nil {
				t.Errorf("accepted runner_group %q, which the client cannot query for", group)
			}
		})
	}

	// Accepted, and every one of these was rejected by the ASCII allowlist this
	// replaced. = and ? survive the client's parse-and-re-encode untouched, and
	// non-ASCII names are entirely ordinary outside English-speaking orgs.
	for name, group := range map[string]string{
		"space":       "Build Farm",
		"equals":      "team=platform",
		"question":    "who?",
		"slash":       "eng/platform",
		"colon":       "eng:platform",
		"accented":    "Grupo-Ñ",
		"non-latin":   "研发",
		"parenthesis": "Build (x64)",
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkRunnerGroup(group); err != nil {
				t.Errorf("rejected runner_group %q, which the client handles correctly: %v", group, err)
			}
		})
	}

	// And it is wired into Load, not merely unit-tested in isolation.
	body := strings.Replace(validConfig,
		"    provider: firecracker\n", "    provider: firecracker\n    runner_group: A & B\n", 1)

	if _, err := Load(writeConfig(t, body)); err == nil {
		t.Error("Load accepted a runner_group containing &")
	}
}

// The rule above is only correct if it matches what the client actually does, so
// this asserts the PROPERTY rather than the list: every name checkRunnerGroup
// accepts must arrive at GitHub unchanged.
//
// It reproduces actions/scaleset v0.4.0's handling — client.go:351 interpolates
// the name unescaped into a path, and newActionsServiceRequest then parses that
// path, copies its query, and re-encodes it. The first version of this rule was
// derived by reasoning about which characters are "URL-safe" and was wrong in
// both directions: it rejected = ? and every non-ASCII name, and it would have
// missed ; entirely.
func TestAcceptedRunnerGroupsSurviveTheClientsURLHandling(t *testing.T) {
	roundTrip := func(t *testing.T, group string) string {
		t.Helper()

		parsedPath, err := url.Parse(fmt.Sprintf("/_apis/runtime/runnergroups/?groupName=%s", group))
		if err != nil {
			t.Fatalf("the client's url.Parse of the interpolated path failed: %v", err)
		}

		u, err := url.Parse("https://example.com/_apis/runtime/runnergroups/")
		if err != nil {
			t.Fatalf("url.Parse: %v", err)
		}

		q := u.Query()
		maps.Copy(q, parsedPath.Query())
		q.Set("api-version", "6.0-preview")
		u.RawQuery = q.Encode()

		// What the Actions service decodes on the other end.
		final, err := url.Parse(u.String())
		if err != nil {
			t.Fatalf("url.Parse of the encoded request URL: %v", err)
		}

		return final.Query().Get("groupName")
	}

	for _, group := range []string{
		"Build Farm", "team=platform", "who?", "eng/platform", "eng:platform",
		"Grupo-Ñ", "研发", "Build (x64)", "plain", "a,b", "a'b", `a"b`,
	} {
		t.Run(group, func(t *testing.T) {
			if err := checkRunnerGroup(group); err != nil {
				t.Fatalf("checkRunnerGroup rejected %q: %v", group, err)
			}

			if got := roundTrip(t, group); got != group {
				t.Errorf("the client turns %q into %q on the wire; checkRunnerGroup should "+
					"have rejected it", group, got)
			}
		})
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

// node.max_custody is optional, and its absence means NO bound.
//
// The default matters more than the parsing. Elapsed time is not evidence that a
// job stopped making progress — billet imposes no job limit and self-hosted
// runners run past GitHub's six-hour default — so a bound billet picked would
// kill legitimate long jobs for no reason visible in the logs.
func TestMaxCustodyDefaultsToNoBound(t *testing.T) {
	t.Parallel()

	var n *NodeConfig

	if d, err := n.MaxCustodyDuration(); err != nil || d != 0 {
		t.Errorf("a nil node section gave (%v, %v), want (0, nil)", d, err)
	}

	empty := &NodeConfig{}

	if d, err := empty.MaxCustodyDuration(); err != nil || d != 0 {
		t.Errorf("an unset max_custody gave (%v, %v), want (0, nil)", d, err)
	}

	blank := &NodeConfig{MaxCustody: "   "}

	if d, err := blank.MaxCustodyDuration(); err != nil || d != 0 {
		t.Errorf("a whitespace max_custody gave (%v, %v), want (0, nil)", d, err)
	}
}

func TestMaxCustodyParsesADuration(t *testing.T) {
	t.Parallel()

	n := &NodeConfig{MaxCustody: " 36h "}

	d, err := n.MaxCustodyDuration()
	if err != nil {
		t.Fatalf("MaxCustodyDuration: %v", err)
	}

	if d != 36*time.Hour {
		t.Errorf("parsed %v, want 36h", d)
	}
}

// A value billet cannot read is refused when the FILE is read, not hours later
// when a wedged container finally needed reclaiming.
func TestMaxCustodyRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()

	for name, value := range map[string]string{
		"not a duration": "tomorrow",
		"bare number":    "24",
		"negative":       "-1h",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			n := &NodeConfig{MaxCustody: value}

			if _, err := n.MaxCustodyDuration(); err == nil {
				t.Errorf("accepted max_custody %q", value)
			}
		})
	}
}

// A bad max_custody is refused by LOADING the file, not only by the accessor.
//
// The accessor tests prove the parsing; this proves validation actually calls
// it. Without this, removing the validation hook leaves every focused test green
// while billet accepts a config it cannot act on — and the operator finds out
// when a wedged container finally needs reclaiming.
func TestLoadRejectsAnUnreadableMaxCustody(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "billet.yaml")

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + dir + `
  max_vcpu: 8
  max_memory: 16GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + filepath.Join(dir, "key.pem") + `
node:
  name: test-host
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: ` + dir + `
  max_custody: tomorrow
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 4GiB
    image: busybox:latest
`

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("loaded a config whose max_custody billet cannot read")
	}

	if !strings.Contains(err.Error(), "max_custody") {
		t.Errorf("the error does not name the offending field: %v", err)
	}
}

// A tier may name one backend or an ordered list, and both read the same way.
func TestATierReportsItsAcceptableProviders(t *testing.T) {
	t.Parallel()

	single := Tier{Provider: ProviderDocker}

	if got := single.AcceptableProviders(); len(got) != 1 || got[0] != ProviderDocker {
		t.Errorf("a single provider read back as %v", got)
	}

	list := Tier{Providers: []ProviderKind{ProviderFirecracker, ProviderEC2}}

	if got := list.AcceptableProviders(); len(got) != 2 || got[0] != ProviderFirecracker {
		t.Errorf("a provider list read back as %v, and its ORDER is its meaning", got)
	}

	if !list.AcceptsProvider(ProviderEC2) {
		t.Error("a listed fallback was not accepted")
	}

	if list.AcceptsProvider(ProviderDocker) {
		t.Error("accepted a backend the tier never named")
	}
}

// BOTH SPELLINGS IS AN ERROR, not a merge.
//
// They are two ways of writing the same field, and guessing which one an
// operator meant is not a kindness when the answer decides where untrusted code
// runs.
func TestATierCannotSetBothProviderAndProviders(t *testing.T) {
	t.Parallel()

	tier := Tier{
		Label:     "billet-8vcpu",
		Provider:  ProviderFirecracker,
		Providers: []ProviderKind{ProviderFirecracker, ProviderEC2},
		VCPU:      8,
		Memory:    32 * GiB,
		Image:     "ubuntu-2404",
		GuestOS:   GuestLinux,
	}

	errs := tier.ProviderErrors("tiers[0]")
	if len(errs) == 0 {
		t.Fatal("accepted a tier that sets provider and providers")
	}

	if !strings.Contains(errs[0].Error(), "not both") {
		t.Errorf("the error does not say what to do: %v", errs[0])
	}
}

// A repeated provider is a typo, not a stronger preference.
//
// There is no second chance at the same backend, so collapsing it silently would
// hide the mistake inside a list whose entire meaning is its order.
func TestADuplicateProviderIsRefused(t *testing.T) {
	t.Parallel()

	tier := Tier{
		Label:     "billet-8vcpu",
		Providers: []ProviderKind{ProviderEC2, ProviderEC2},
	}

	if errs := tier.ProviderErrors("tiers[0]"); len(errs) == 0 {
		t.Fatal("accepted a tier listing the same provider twice")
	}
}

func TestATierWithNoProviderIsRefused(t *testing.T) {
	t.Parallel()

	if errs := (Tier{Label: "billet-8vcpu"}).ProviderErrors("tiers[0]"); len(errs) == 0 {
		t.Fatal("accepted a tier that names no backend; its leases could never be placed")
	}
}

// A macOS tier may not carry a non-Apple FALLBACK.
//
// Apple's licence permits macOS guests only on Apple hardware, and a fallback is
// exactly the case nobody is watching when it fires — so every listed provider
// must be Tart, not merely the first.
func TestAMacOSTierCannotFallBackOffAppleHardware(t *testing.T) {
	t.Parallel()

	c := &Config{
		Tiers: []Tier{{
			Label:     "billet-6vcpu-macos",
			Providers: []ProviderKind{ProviderTart, ProviderEC2},
			Node:      "mac-mini-1",
			VCPU:      6,
			Memory:    16 * GiB,
			Image:     "macos-26",
			GuestOS:   GuestMacOS,
		}},
	}

	var found bool

	for _, err := range c.validateGuestOSRules("tiers[0]", &c.Tiers[0]) {
		if strings.Contains(err.Error(), "tart") {
			found = true
		}
	}

	if !found {
		t.Fatal("a macOS tier was allowed to fall back to a non-Apple backend")
	}
}

// A tier that names providers is not given the local one as well.
//
// Defaulting tested the single `provider` field, which a tier written with
// `providers:` leaves empty — so billet stamped the local backend onto a tier
// that had already named several, and then refused the config as "you set both",
// blaming the operator for a field it had just filled in itself. Found by
// running `billet check` against a multi-provider tier, which is exactly the
// thing the feature is for.
func TestATierWithProvidersDoesNotInheritTheLocalOne(t *testing.T) {
	t.Parallel()

	c := &Config{
		Server: &ServerConfig{Listen: "127.0.0.1:7717", StateDir: t.TempDir(),
			MaxVCPU: 8, MaxMemory: 16 * GiB},
		Node: &NodeConfig{
			Name: "test-host", ServerAddr: "127.0.0.1:7717",
			Provider: ProviderDocker, StateDir: t.TempDir(),
		},
		Tiers: []Tier{{
			Label:     "billet-8vcpu-ubuntu-2404",
			Providers: []ProviderKind{ProviderFirecracker, ProviderEC2},
			VCPU:      8,
			Memory:    32 * GiB,
			Image:     "ubuntu-2404-x64",
		}},
	}

	c.applyDefaults()

	if c.Tiers[0].Provider != "" {
		t.Fatalf("a tier that named providers was also given provider %q, which validation "+
			"then refuses as setting both", c.Tiers[0].Provider)
	}

	accepted := c.Tiers[0].AcceptableProviders()
	if len(accepted) != 2 || accepted[0] != ProviderFirecracker {
		t.Errorf("the tier's own list was altered by defaulting: %v", accepted)
	}
}

// A PLURAL tier pinned to a host it can never bind to is refused at load.
//
// The check compared the singular `provider` field, which a tier written with
// `providers:` leaves empty — so this config loaded perfectly cleanly and the
// failure surfaced only at runtime, after capacity had been escrowed and a job
// offered. The whole reason for a load-time guard is that the runtime symptom is
// a job that queues forever.
func TestAPluralTierPinnedToAWrongHostIsRefused(t *testing.T) {
	t.Parallel()

	c := &Config{
		Nodes: []NodePolicy{{Name: "mac-mini-1", Provider: ProviderTart}},
		Tiers: []Tier{{
			Label:     "billet-8vcpu-ubuntu-2404",
			Providers: []ProviderKind{ProviderFirecracker, ProviderEC2},
			Node:      "mac-mini-1",
			VCPU:      8,
			Memory:    32 * GiB,
			Image:     "ubuntu-2404-x64",
			GuestOS:   GuestLinux,
		}},
	}

	var found bool

	for _, err := range c.validateGuestOSRules("tiers[0]", &c.Tiers[0]) {
		if strings.Contains(err.Error(), "pinned to node") {
			found = true
		}
	}

	if !found {
		t.Fatal("a tier that can never bind to the host it is pinned to loaded cleanly; " +
			"its jobs would queue forever with no diagnostic")
	}
}

// An UNPINNED plural tier is constrained by a matching host's allowlist.
//
// The unpinned branch compared the singular field, so a tier written with
// `providers:` was never checked against any host's guest_os allowlist — the
// list could contain tart and a macOS-only Mac would still not object. Every
// existing test for this branch used `provider:`, so reverting it to equality
// left them all green.
func TestAnUnpinnedPluralTierIsCheckedAgainstAllowlists(t *testing.T) {
	t.Parallel()

	c := &Config{
		// A Mac that will only run macOS guests.
		Nodes: []NodePolicy{{
			Name: "mac-mini-1", Provider: ProviderTart, GuestOS: []GuestOS{GuestMacOS},
		}},
		Tiers: []Tier{{
			Label: "billet-4vcpu-ubuntu-arm",
			// Unpinned, and tart is one of the backends it accepts — so this Mac is
			// a placement candidate and its allowlist applies.
			Providers: []ProviderKind{ProviderFirecracker, ProviderTart},
			VCPU:      4,
			Memory:    12 * GiB,
			Image:     "ubuntu-2404-arm64",
			GuestOS:   GuestLinux,
		}},
	}

	var found bool

	for _, err := range c.validateGuestOSRules("tiers[0]", &c.Tiers[0]) {
		if strings.Contains(err.Error(), "allowlist") {
			found = true
		}
	}

	if !found {
		t.Fatal("an unpinned plural tier was not checked against a candidate host's " +
			"guest_os allowlist; a linux guest could be scheduled onto a macOS-only Mac")
	}
}

// `billet check` catches a bad reservation, rather than the server failing
// later.
//
// These validations lived only in alloc.New, and check runs config validation
// alone — so it reported a broken configuration as valid and the failure
// surfaced at startup while constructing the allocator. That is the opposite of
// what a check command is for.
func TestValidateRejectsBadReservations(t *testing.T) {
	t.Parallel()

	base := func(tiers []Tier) *Config {
		return &Config{
			Server: &ServerConfig{
				Listen: "127.0.0.1:7717", StateDir: t.TempDir(),
				MaxVCPU: 32, MaxMemory: 64 * GiB,
			},
			GitHub: &GitHubConfig{
				Org: "acme", AppID: 1, InstallationID: 2,
				PrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"),
			},
			Node: &NodeConfig{
				Name: "test-host", ServerAddr: "127.0.0.1:7717",
				Provider: ProviderDocker, StateDir: t.TempDir(),
			},
			Tiers: tiers,
		}
	}

	tier := func(label string, vcpu int, mem ByteSize, reserved, maxConc int) Tier {
		return Tier{
			Label: label, Provider: ProviderDocker, GuestOS: GuestLinux,
			VCPU: vcpu, Memory: mem, Image: "ubuntu-2404",
			Reserved: reserved, MaxConcurrent: maxConc,
		}
	}

	for name, tc := range map[string]struct {
		tiers []Tier
		want  string
	}{
		"negative": {
			tiers: []Tier{tier("billet-2vcpu", 2, 4*GiB, -1, 0)},
			want:  "negative",
		},
		"above its own ceiling": {
			tiers: []Tier{tier("billet-2vcpu", 2, 4*GiB, 4, 2)},
			want:  "max_concurrent",
		},
		"more than the machine has": {
			tiers: []Tier{
				tier("billet-2vcpu", 2, 4*GiB, 8, 0),  // 16 vCPU
				tier("billet-8vcpu", 8, 16*GiB, 4, 0), // 32 vCPU -> 48 > 32
			},
			want: "vCPU left",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			c := base(tc.tiers)
			c.applyDefaults()

			// Validate joins its errors, so the whole message is searched rather
			// than a slice walked.
			err := c.Validate()
			if err == nil {
				t.Fatal("validation accepted a reservation the allocator will refuse; " +
					"`billet check` would report this config as fine")
			}

			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error does not mention %q: %v", tc.want, err)
			}
		})
	}
}

// An invalid BUDGET does not produce fabricated floor errors.
//
// max_vcpu: 0 is already reported against the field that holds it. Running the
// floor check anyway added "reserved 2 needs more than the 0 vCPU left" for
// every reservation — sending the reader to fix tiers that are not the problem,
// which is the worst thing a diagnostic can do.
func TestAnInvalidBudgetDoesNotBlameTheTiers(t *testing.T) {
	t.Parallel()

	c := &Config{
		Server: &ServerConfig{
			Listen: "127.0.0.1:7717", StateDir: t.TempDir(),
			MaxVCPU: 0, MaxMemory: 64 * GiB,
		},
		GitHub: &GitHubConfig{
			Org: "acme", AppID: 1, InstallationID: 2,
			PrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"),
		},
		Node: &NodeConfig{
			Name: "test-host", ServerAddr: "127.0.0.1:7717",
			Provider: ProviderDocker, StateDir: t.TempDir(),
		},
		Tiers: []Tier{{
			Label: "billet-2vcpu", Provider: ProviderDocker, GuestOS: GuestLinux,
			VCPU: 2, Memory: 4 * GiB, Image: "ubuntu-2404", Reserved: 2,
		}},
	}

	c.applyDefaults()

	err := c.Validate()
	if err == nil {
		t.Fatal("accepted a zero vCPU budget")
	}

	if strings.Contains(err.Error(), "vCPU left") {
		t.Errorf("the zero budget produced a fabricated per-tier floor error, which points "+
			"the reader at the wrong field: %v", err)
	}
}

// A reservation that arrives after the budget is EXACTLY exhausted is still an
// error.
//
// The case a per-iteration "remaining > 0" guard silently accepts. The remaining
// budget legitimately reaches zero once earlier tiers have taken it all, and at
// that point a further reservation must be reported — skipping the check there
// waves through a catalogue that cannot be honoured. Found by mutating the guard
// I had just added as belt-and-braces.
func TestAReservationAfterTheBudgetIsExhaustedIsRefused(t *testing.T) {
	t.Parallel()

	c := &Config{
		Server: &ServerConfig{
			Listen: "127.0.0.1:7717", StateDir: t.TempDir(),
			MaxVCPU: 8, MaxMemory: 64 * GiB,
		},
		GitHub: &GitHubConfig{
			Org: "acme", AppID: 1, InstallationID: 2,
			PrivateKeyPath: filepath.Join(t.TempDir(), "key.pem"),
		},
		Node: &NodeConfig{
			Name: "test-host", ServerAddr: "127.0.0.1:7717",
			Provider: ProviderDocker, StateDir: t.TempDir(),
		},
		Tiers: []Tier{
			// Takes the entire 8 vCPU budget, exactly.
			{
				Label: "billet-first", Provider: ProviderDocker, GuestOS: GuestLinux,
				VCPU: 2, Memory: 1 * GiB, Image: "ubuntu-2404", Reserved: 4,
			},
			// Then asks for more of a budget that is now exactly zero.
			{
				Label: "billet-second", Provider: ProviderDocker, GuestOS: GuestLinux,
				VCPU: 2, Memory: 1 * GiB, Image: "ubuntu-2404", Reserved: 1,
			},
		},
	}

	c.applyDefaults()

	err := c.Validate()
	if err == nil {
		t.Fatal("accepted a reservation against an exhausted budget; every tier would " +
			"compute zero headroom and billet would advertise nothing")
	}

	if !strings.Contains(err.Error(), "billet-second") {
		t.Errorf("the error does not name the tier that does not fit: %v", err)
	}
}

// A RELATIVE server.lock_dir IS CAUGHT AT LOAD, not only when the lock is taken.
//
// Tested at this layer on its own, because the state package has its own check
// and would keep passing if this one were deleted — so without this the config
// gate could disappear and nothing would notice. It matters that the config
// catches it: `billet check` is how an operator validates a file before it ever
// reaches a host, and the failure it prevents is invisible at runtime. A
// relative path resolves against each process working directory, so one config
// puts a unit started in / and an operator started in /srv/billet into different
// collision domains while both log the same string.
func TestValidateRejectsARelativeLockDir(t *testing.T) {
	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  lock_dir: locks
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
	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a relative server.lock_dir")
	}

	if !strings.Contains(err.Error(), "lock_dir") {
		t.Errorf("the error does not name the key: %v", err)
	}
}

// A NODE THAT DIALS THE NETWORK NEEDS A CERTIFICATE, and saying so here is the
// difference between one clear error and a handshake failure on another machine.
//
// The control plane identifies a node by the name in its certificate and refuses
// a connection without one, so a node configured to reach a remote control plane
// with no bundle can never work. It would fail at the TLS layer, on the node, in
// a message that does not mention the config key that caused it.
func TestARemoteNodeNeedsACertificate(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  lock_dir: /run/billet/locks
  max_vcpu: 8
  max_memory: 32GiB
node:
  name: host-1
  server_addr: 10.0.0.4:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
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

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("a node pointed at a remote control plane with no node.tls was accepted")
	}

	if !strings.Contains(err.Error(), "node.tls") {
		t.Errorf("the error does not name the key an operator has to add: %v", err)
	}
}

// Loopback is the exception, because the control plane is inside this machine
// and there is nothing between the two to authenticate against.
func TestALoopbackNodeNeedsNoCertificate(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  lock_dir: /run/billet/locks
  max_vcpu: 8
  max_memory: 32GiB
node:
  name: host-1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
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

	if _, err := Load(writeConfig(t, body)); err != nil {
		t.Errorf("a node dialling a control plane in its own machine was refused for having "+
			"no certificate: %v", err)
	}
}

// The complete shape loads, which is what keeps the three tests above from all
// passing against a rule that refuses everything.
func TestACompleteNodeTLSBlockIsAccepted(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  lock_dir: /run/billet/locks
  max_vcpu: 8
  max_memory: 32GiB
node:
  name: host-1
  server_addr: 10.0.0.4:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
  tls:
    cert: /etc/billet/tls/node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt
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

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("a complete node.tls block was refused: %v", err)
	}

	if cfg.Node.TLS == nil || cfg.Node.TLS.CertPath != "/etc/billet/tls/node.crt" {
		t.Errorf("the certificate path did not survive loading: %+v", cfg.Node.TLS)
	}
}

// HALF A BUNDLE IS NOT A BUNDLE. Each file has a distinct job — the identity,
// the key that proves it, and the authority the control plane is checked against
// — so a missing one is not something to default.
func TestAPartialNodeTLSBlockIsRefused(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  lock_dir: /run/billet/locks
  max_vcpu: 8
  max_memory: 32GiB
node:
  name: host-1
  server_addr: 10.0.0.4:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
  tls:
    cert: /etc/billet/tls/node.crt
    ca: /etc/billet/tls/ca.crt
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

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("a node.tls block with no key was accepted")
	}

	if !strings.Contains(err.Error(), "node.tls.key") {
		t.Errorf("the error does not name the missing file: %v", err)
	}
}

// A RELATIVE CERTIFICATE PATH RESOLVES AGAINST WHATEVER STARTED THE SERVICE,
// which is one directory under systemd and another in the shell where it was
// tested.
func TestARelativeNodeTLSPathIsRefused(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  lock_dir: /run/billet/locks
  max_vcpu: 8
  max_memory: 32GiB
node:
  name: host-1
  server_addr: 10.0.0.4:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
  tls:
    cert: node.crt
    key: /etc/billet/tls/node.key
    ca: /etc/billet/tls/ca.crt
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

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("a relative node.tls.cert was accepted")
	}

	if !strings.Contains(err.Error(), "node.tls.cert") {
		t.Errorf("the error does not name the offending key: %v", err)
	}
}

// TWO ROLES MAY NAME DIFFERENT LOCK DIRECTORIES, and one config file is why.
//
// A single billet.yaml deployed to every machine carries the controller's
// lock_dir alongside each node's. A node-only host never reads the server
// section, so refusing to load over it would take that host down for a setting
// with no bearing on it. Whether the two agree is asked by `server --dev`, which
// is the one process that runs both roles at once.
func TestOneFileMayConfigureBothRolesDifferently(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  lock_dir: /run/billet/server-locks
  max_vcpu: 8
  max_memory: 32GiB
node:
  name: host-1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/node-locks
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

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("a shared config file naming a lock_dir per role was refused: %v", err)
	}

	if cfg.LockDirsAgree() {
		t.Error("two different lock directories were reported as agreeing, so `server --dev` " +
			"would take two locks for one deployment identity")
	}
}

// The same question, answered the other way.
func TestMatchingLockDirsAgree(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  lock_dir: /run/billet/locks
  max_vcpu: 8
  max_memory: 32GiB
node:
  name: host-1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: /run/billet/locks
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

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if !cfg.LockDirsAgree() {
		t.Error("identical lock directories were reported as disagreeing, so `server --dev` " +
			"would refuse a correct configuration")
	}
}

// A relative node.lock_dir is refused for the same reason the server's is.
func TestARelativeNodeLockDirIsRefused(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  lock_dir: locks
  max_vcpu: 8
  max_memory: 32GiB
node:
  name: host-1
  server_addr: 127.0.0.1:7717
  provider: docker
  state_dir: /var/lib/billet/node
  lock_dir: locks
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

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("a relative node.lock_dir was accepted")
	}
}
