// Package config defines billet's on-disk configuration and its validation rules.
//
// A single billet.yaml describes both roles. `billet server` reads the server and
// github sections plus the tier catalog; `billet node` reads the node section and
// learns its tier assignments from the server. Running `billet server --dev` reads
// everything and runs both in one process.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole of billet.yaml.
type Config struct {
	// Server is required for the server and dev roles, ignored by a pure node.
	Server *ServerConfig `yaml:"server,omitempty"`
	// Node is required for the node and dev roles, ignored by a pure server.
	Node *NodeConfig `yaml:"node,omitempty"`
	// GitHub is required for the server and dev roles.
	GitHub *GitHubConfig `yaml:"github,omitempty"`
	// Tiers is the runner catalog. Each tier becomes one GitHub scale set, and
	// its Label is what users put in `runs-on`.
	Tiers []Tier `yaml:"tiers,omitempty"`
	// Nodes describes per-host policy to the server. It is optional and separate
	// from the Node section on purpose: Node is how a host describes ITSELF,
	// while Nodes is how the control plane describes the FLEET. Host policy has
	// to live on the server side, because the limits it expresses are enforced
	// across tiers the individual host never sees.
	//
	// Every field defaults, so a deployment that wants the standard behaviour
	// omits the section entirely.
	Nodes []NodePolicy `yaml:"nodes,omitempty"`
}

// NodePolicy is what one compute host is permitted to run.
//
// It exists because a host's capabilities are not implied by its provider. An
// Apple Silicon machine can serve macOS guests, Linux arm64 guests, or both,
// and which of those an operator wants is a deployment decision rather than a
// property of the hardware — someone may keep a Mac exclusively for macOS so
// the licensed guests never contend with Linux builds, or run it purely as an
// arm64 Linux builder and never boot macOS on it at all.
type NodePolicy struct {
	// Name matches Tier.Node and NodeConfig.Name.
	Name string `yaml:"name"`

	// Provider is the compute backend this host runs, matching NodeConfig.
	// Provider. Optional, and used only to decide whether an unpinned tier could
	// ever land here: a firecracker tier is not a placement candidate for a Tart
	// host, so without it a macOS-only Mac would appear to conflict with every
	// x64 Linux tier in the deployment.
	Provider ProviderKind `yaml:"provider,omitempty"`

	// GuestOS is an allowlist of what may be scheduled here. Empty means
	// unconstrained, which is the default and preserves the behaviour of a
	// config that never mentions the node.
	//
	// Note the shape difference from Tier.GuestOS, which is a single value: a
	// tier boots exactly one guest OS, while a host may permit several.
	GuestOS []GuestOS `yaml:"guest_os,omitempty"`

	// MacOSVMLimit caps concurrent macOS guests on this host, counting warm
	// ones. Nil means DefaultMacOSVMLimit — an unconfigured Apple host is still
	// bound by Apple's licence, so the default is the licence, not "unlimited".
	//
	// Lowering it is the common case: reserve one slot for interactive use, or
	// set 0 to keep a Mac for Linux arm64 work only. Raising it above
	// DefaultMacOSVMLimit is permitted because billet cannot know what licence
	// or hardware agreement a given operator has, but Apple's standard terms
	// allow at most DefaultMacOSVMLimit macOS guests per Apple-branded host —
	// exceeding that is an assertion about YOUR licence, not a tuning knob.
	MacOSVMLimit *int `yaml:"macos_vm_limit,omitempty"`
}

// Clone returns a deep copy, sharing nothing mutable with the receiver.
//
// A shallow struct copy is not enough and the difference is silent: GuestOS is a
// slice and MacOSVMLimit is a POINTER, so a caller holding the original can
// widen a host's allowlist or raise its macOS cap after the allocator has been
// built from it — moving a licence limit out from under leases already counted
// against it, with nothing to indicate the rules changed.
func (p NodePolicy) Clone() NodePolicy {
	p.GuestOS = slices.Clone(p.GuestOS)

	if p.MacOSVMLimit != nil {
		limit := *p.MacOSVMLimit
		p.MacOSVMLimit = &limit
	}

	return p
}

// AllowsGuestOS reports whether this host may run a given guest OS. An empty
// allowlist permits everything.
func (p NodePolicy) AllowsGuestOS(g GuestOS) bool {
	if len(p.GuestOS) == 0 {
		return true
	}

	for _, allowed := range p.GuestOS {
		if allowed == g {
			return true
		}
	}

	return false
}

// MacOSLimit is the effective cap on concurrent macOS guests for this host.
//
// An allowlist that excludes macOS yields 0 whatever MacOSVMLimit says, so the
// two fields cannot disagree about whether macOS runs here. Validation rejects
// the contradictory config that would make this matter; this method makes the
// answer well-defined regardless of how it was constructed.
func (p NodePolicy) MacOSLimit() int {
	if !p.AllowsGuestOS(GuestMacOS) {
		return 0
	}

	if p.MacOSVMLimit == nil {
		return DefaultMacOSVMLimit
	}

	return *p.MacOSVMLimit
}

// ServerConfig configures the control plane.
type ServerConfig struct {
	// Listen is the address nodes dial. Nodes always connect outbound, so on a
	// single-box deployment this stays on loopback and billet needs no open port
	// reachable from anywhere else.
	Listen string `yaml:"listen"`
	// StateDir holds the SQLite database, the process lock, and the mTLS CA. It
	// MUST be on local storage: SQLite's WAL cannot work on a network filesystem,
	// and the state package fails closed if it detects otherwise.
	StateDir string `yaml:"state_dir"`
	// MaxVCPU and MaxMemory bound what the allocator will ever hand out across
	// every tier combined. They are required and must be positive: capacity is
	// escrowed before each scale-set listener advertises to GitHub, and an absent
	// ceiling means concurrent listeners can collectively overcommit the machine
	// with nothing to stop them.
	MaxVCPU   int      `yaml:"max_vcpu"`
	MaxMemory ByteSize `yaml:"max_memory"`
}

// NodeConfig configures a compute host.
type NodeConfig struct {
	// Name identifies this node to the server and in tier pinning. Defaults to
	// the hostname.
	Name string `yaml:"name,omitempty"`
	// ServerAddr is the control plane to dial. Nodes always initiate the
	// connection, so a node needs no inbound reachability of its own.
	ServerAddr string `yaml:"server_addr"`
	// Provider selects the compute backend for this host.
	Provider ProviderKind `yaml:"provider"`
	// StateDir holds node-local data: the generation pointer store (which is
	// authoritative for this node's volumes), image cache, and mTLS identity.
	StateDir string `yaml:"state_dir"`
	// Firecracker is required when Provider is ProviderFirecracker.
	Firecracker *FirecrackerConfig `yaml:"firecracker,omitempty"`
}

// ProviderKind names a compute backend.
type ProviderKind string

const (
	// ProviderFirecracker runs one Firecracker microVM per job on bare metal.
	// Requires /dev/kvm.
	ProviderFirecracker ProviderKind = "firecracker"
	// ProviderTart runs macOS and Linux arm64 guests on Apple Silicon. Requires
	// Tart, which is FSL-licensed and installed separately.
	ProviderTart ProviderKind = "tart"
	// ProviderEC2 launches one spot instance per job. Firecracker is not an
	// option on EC2 outside .metal instances, so here the instance itself is the
	// isolation boundary.
	ProviderEC2 ProviderKind = "ec2"
	// ProviderDocker runs jobs in containers. Isolation is materially weaker than
	// a VM; this exists so `billet init` works on a laptop and it refuses
	// untrusted workloads outright.
	ProviderDocker ProviderKind = "docker"
)

var allProviders = []ProviderKind{ProviderFirecracker, ProviderTart, ProviderEC2, ProviderDocker}

func (p ProviderKind) valid() bool {
	for _, k := range allProviders {
		if p == k {
			return true
		}
	}
	return false
}

// GuestOS classifies what a tier boots. It exists as an explicit field rather
// than being inferred from the label because Apple's licensing limit is enforced
// against it: inferring "this is macOS" from the operator's chosen label means a
// tier named `sonoma-arm64` silently escapes the cap.
type GuestOS string

const (
	GuestLinux   GuestOS = "linux"
	GuestMacOS   GuestOS = "macos"
	GuestWindows GuestOS = "windows"
)

var allGuestOS = []GuestOS{GuestLinux, GuestMacOS, GuestWindows}

func (g GuestOS) valid() bool {
	for _, k := range allGuestOS {
		if g == k {
			return true
		}
	}
	return false
}

// FirecrackerConfig configures the bare-metal microVM backend.
type FirecrackerConfig struct {
	// BinaryPath and JailerPath locate the firecracker and jailer binaries.
	BinaryPath string `yaml:"binary_path,omitempty"`
	JailerPath string `yaml:"jailer_path,omitempty"`
	// KernelImage is the uncompressed guest kernel. It must be built with
	// everything Docker needs; validate with moby's contrib/check-config.sh
	// rather than a hand-maintained list.
	KernelImage string `yaml:"kernel_image"`
	// ZFSPool backs golden images, per-job root clones, and cache volumes.
	ZFSPool string `yaml:"zfs_pool"`
	// Bridge is the host bridge guests attach to.
	Bridge string `yaml:"bridge,omitempty"`
}

// GitHubConfig holds the App identity used to manage runners.
//
// billet requests exactly two permissions: metadata:read and
// organization_self_hosted_runners:read+write. It deliberately does not request
// actions:read, which would expose workflow runs, logs, and artifacts.
type GitHubConfig struct {
	Org            string `yaml:"org"`
	AppID          int64  `yaml:"app_id"`
	InstallationID int64  `yaml:"installation_id"`
	// PrivateKeyPath points at the App private key PEM. This file is the single
	// most sensitive thing in a billet deployment: it lives only on the control
	// plane, and nodes never hold long-lived GitHub credentials.
	PrivateKeyPath string `yaml:"private_key_path"`
}

// Tier is one runner shape. Its Label is what appears in `runs-on`.
type Tier struct {
	Label    string       `yaml:"label"`
	Provider ProviderKind `yaml:"provider"`
	// GuestOS defaults to linux. Set it explicitly for macOS and Windows tiers —
	// licensing and capability checks key off this field, not off the label.
	GuestOS GuestOS `yaml:"guest_os,omitempty"`
	// Node optionally pins this tier to a named node. Required when only one
	// node can serve it — macOS tiers, for example.
	Node string `yaml:"node,omitempty"`

	VCPU   int      `yaml:"vcpu"`
	Memory ByteSize `yaml:"memory"`
	Disk   ByteSize `yaml:"disk,omitempty"`
	// SHM sizes /dev/shm. Chromium and Postgres both misbehave on the default,
	// so this is a tier knob rather than an image constant.
	SHM ByteSize `yaml:"shm,omitempty"`

	Image string `yaml:"image"`

	// WarmPool pre-boots idle VMs to hide cold-start latency. Start at 0-2 and
	// raise it only after measuring what cold start actually costs; a large warm
	// pool burns memory to solve a problem Firecracker may not have. Warm guests
	// count against MaxConcurrent, because a warm macOS guest is still a running
	// macOS guest as far as Apple's licence is concerned.
	WarmPool int `yaml:"warm_pool,omitempty"`

	// Intercept enables the colocated Actions cache for this tier. It defaults
	// to false for a reason: cache interception terminates TLS in front of
	// GitHub's results service, which also carries artifact metadata. Leave it
	// off for tiers that publish release artifacts or hold deployment secrets.
	Intercept bool `yaml:"intercept,omitempty"`

	// MaxConcurrent caps simultaneous instances of this tier, counting warm ones.
	// Zero means "no per-tier cap" and is only legal for non-macOS tiers.
	MaxConcurrent int `yaml:"max_concurrent,omitempty"`
}

// DefaultMacOSVMLimit is Apple's licensing cap on macOS guests per
// Apple-branded host. Linux guests on the same machine are not subject to it.
//
// This is the DEFAULT, not a hard ceiling: a deployment sets its own per-host
// number via NodePolicy.MacOSVMLimit, because billet cannot know what hardware
// or licence agreement an operator has. What the default guarantees is that a
// config which says nothing gets Apple's standard terms rather than "unlimited".
//
// The static check here is a guard, not the enforcement point: the allocator
// additionally holds a single host-wide count of running plus warm macOS guests
// at runtime, because two separately-valid tiers on one node still share one
// physical Mac. Both read the effective limit from the same NodePolicy, so there
// is one number rather than two that can drift apart.
const DefaultMacOSVMLimit = 2

// NodePolicyFor returns the policy for a named host, and whether one was
// declared. The zero NodePolicy is the documented default — unconstrained guest
// OS, Apple's standard macOS limit — so the returned value is usable either way
// and callers only need the boolean when they care about the distinction.
func (c *Config) NodePolicyFor(name string) (NodePolicy, bool) {
	for i := range c.Nodes {
		if c.Nodes[i].Name == name {
			return c.Nodes[i], true
		}
	}

	return NodePolicy{Name: name}, false
}

// MacOSLimitForNode is the effective cap on concurrent macOS guests for a host.
func (c *Config) MacOSLimitForNode(name string) int {
	p, _ := c.NodePolicyFor(name)

	return p.MacOSLimit()
}

// NodePolicies is the declared fleet policy keyed by node name. It is what the
// allocator is built from, so runtime enforcement and this package's load-time
// guard read the same rules rather than two copies that can drift.
//
// Only DECLARED hosts appear. An absent host is unconstrained in guest OS and
// carries Apple's default macOS limit — the same thing the allocator assumes
// for a name it does not recognise.
func (c *Config) NodePolicies() map[string]NodePolicy {
	policies := make(map[string]NodePolicy, len(c.Nodes))

	for i := range c.Nodes {
		policies[c.Nodes[i].Name] = c.Nodes[i].Clone()
	}

	return policies
}

var labelRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	defer f.Close()

	var c Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // typos in a CI config should be loud, not ignored
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	// A second document would otherwise be silently ignored, which for a config
	// that assigns capacity is a quiet way to run something other than intended.
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("config %s contains more than one YAML document", path)
	}

	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() {
	if c.Server != nil {
		if c.Server.Listen == "" {
			c.Server.Listen = "127.0.0.1:7717"
		}
		if c.Server.StateDir == "" {
			c.Server.StateDir = defaultStateDir("server")
		}
	}
	if c.Node != nil {
		if c.Node.Name == "" {
			if h, err := os.Hostname(); err == nil {
				c.Node.Name = h
			}
		}
		if c.Node.StateDir == "" {
			c.Node.StateDir = defaultStateDir("node")
		}
		if c.Node.ServerAddr == "" && c.Server != nil {
			c.Node.ServerAddr = c.Server.Listen
		}
	}
	for i := range c.Tiers {
		t := &c.Tiers[i]
		if t.Provider == "" && c.Node != nil {
			t.Provider = c.Node.Provider
		}
		if t.GuestOS == "" {
			t.GuestOS = GuestLinux
		}
		// A macOS tier with no explicit cap inherits its host's limit rather than
		// "unlimited", so forgetting the field fails safe. Reading it from the
		// node policy means lowering a Mac's limit tightens every macOS tier
		// pinned to it, instead of leaving tiers at a default the host no longer
		// permits.
		// Only from a usable limit. A negative one is a config error reported by
		// validateNodes, and copying it here would turn one bad field into three
		// diagnostics, two of them naming max_concurrent — which the operator
		// never set.
		if t.GuestOS == GuestMacOS && t.MaxConcurrent == 0 {
			if limit := c.MacOSLimitForNode(t.Node); limit > 0 {
				t.MaxConcurrent = limit
			}
		}
	}
}

func defaultStateDir(role string) string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "billet", role)
	}
	return filepath.Join(".", ".billet", role)
}

// Validate reports every problem it finds rather than stopping at the first, so
// a misconfigured deployment takes one round trip to fix instead of five.
func (c *Config) Validate() error {
	var errs []error

	if c.Server == nil && c.Node == nil {
		errs = append(errs, errors.New("config defines neither a server nor a node section"))
	}

	errs = append(errs, c.validateServer()...)
	errs = append(errs, c.validateGitHub()...)
	errs = append(errs, c.validateNode()...)
	errs = append(errs, c.validateNodes()...)
	errs = append(errs, c.validateTiers()...)
	errs = append(errs, c.validateCapacity()...)
	errs = append(errs, c.validateMacOSHostLimits()...)

	return errors.Join(errs...)
}

func (c *Config) validateServer() []error {
	if c.Server == nil {
		return nil
	}
	var errs []error
	if c.Server.StateDir == "" {
		errs = append(errs, errors.New("server.state_dir is required"))
	}
	if err := validateHostPort("server.listen", c.Server.Listen); err != nil {
		errs = append(errs, err)
	}
	// Required, not optional. Without a ceiling the allocator has nothing to
	// escrow against and concurrent tier listeners can overcommit the machine.
	if c.Server.MaxVCPU <= 0 {
		errs = append(errs, errors.New(
			"server.max_vcpu must be positive; it is the ceiling the allocator escrows against"))
	}
	if c.Server.MaxMemory <= 0 {
		errs = append(errs, errors.New(
			"server.max_memory must be positive; it is the ceiling the allocator escrows against"))
	}
	return errs
}

func (c *Config) validateGitHub() []error {
	if c.GitHub == nil {
		if c.Server != nil {
			return []error{errors.New("github section is required for the server role")}
		}
		return nil
	}
	var errs []error
	if strings.TrimSpace(c.GitHub.Org) == "" {
		errs = append(errs, errors.New("github.org is required"))
	}
	if c.GitHub.AppID <= 0 {
		errs = append(errs, errors.New("github.app_id is required; run `billet github-app create`"))
	}
	if c.GitHub.InstallationID <= 0 {
		errs = append(errs, errors.New("github.installation_id is required; creating an App does not install it"))
	}
	if c.GitHub.PrivateKeyPath == "" {
		errs = append(errs, errors.New("github.private_key_path is required"))
	}
	return errs
}

func (c *Config) validateNode() []error {
	if c.Node == nil {
		return nil
	}
	var errs []error
	if !c.Node.Provider.valid() {
		errs = append(errs, fmt.Errorf("node.provider %q is not one of %v", c.Node.Provider, allProviders))
	}
	if strings.TrimSpace(c.Node.Name) == "" {
		errs = append(errs, errors.New("node.name is required and must not be blank"))
	}
	if c.Node.StateDir == "" {
		errs = append(errs, errors.New("node.state_dir is required"))
	}
	if err := validateHostPort("node.server_addr", c.Node.ServerAddr); err != nil {
		errs = append(errs, err)
	}
	if c.Node.Provider == ProviderFirecracker {
		if c.Node.Firecracker == nil {
			errs = append(errs, errors.New("node.firecracker is required when provider is firecracker"))
		} else {
			if c.Node.Firecracker.KernelImage == "" {
				errs = append(errs, errors.New("node.firecracker.kernel_image is required"))
			}
			if c.Node.Firecracker.ZFSPool == "" {
				errs = append(errs, errors.New("node.firecracker.zfs_pool is required"))
			}
		}
	}
	return errs
}

func (c *Config) validateTiers() []error {
	var errs []error
	seen := make(map[string]struct{}, len(c.Tiers))
	for i := range c.Tiers {
		t := &c.Tiers[i]
		where := fmt.Sprintf("tiers[%d]", i)
		if t.Label != "" {
			where = fmt.Sprintf("tier %q", t.Label)
		}
		if !labelRe.MatchString(t.Label) {
			errs = append(errs, fmt.Errorf("%s: label must match %s", where, labelRe))
		}
		if _, dup := seen[t.Label]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate label", where))
		}
		seen[t.Label] = struct{}{}

		if !t.Provider.valid() {
			errs = append(errs, fmt.Errorf("%s: provider %q is not one of %v", where, t.Provider, allProviders))
		}
		if !t.GuestOS.valid() {
			errs = append(errs, fmt.Errorf("%s: guest_os %q is not one of %v", where, t.GuestOS, allGuestOS))
		}
		if t.VCPU <= 0 {
			errs = append(errs, fmt.Errorf("%s: vcpu must be positive", where))
		}
		if t.Memory <= 0 {
			errs = append(errs, fmt.Errorf("%s: memory must be positive", where))
		}
		if t.Disk < 0 {
			errs = append(errs, fmt.Errorf("%s: disk must not be negative", where))
		}
		if t.SHM < 0 {
			errs = append(errs, fmt.Errorf("%s: shm must not be negative", where))
		}
		if t.Image == "" {
			errs = append(errs, fmt.Errorf("%s: image is required", where))
		}
		if t.WarmPool < 0 {
			errs = append(errs, fmt.Errorf("%s: warm_pool must not be negative", where))
		}
		if t.MaxConcurrent < 0 {
			errs = append(errs, fmt.Errorf("%s: max_concurrent must not be negative", where))
		}
		// Warm instances are running instances. A warm pool larger than the cap
		// would sit permanently over the limit with no job ever running.
		if t.MaxConcurrent > 0 && t.WarmPool > t.MaxConcurrent {
			errs = append(errs, fmt.Errorf(
				"%s: warm_pool %d exceeds max_concurrent %d; warm instances count against the cap",
				where, t.WarmPool, t.MaxConcurrent))
		}

		errs = append(errs, c.validateGuestOSRules(where, t)...)
	}
	return errs
}

func (c *Config) validateGuestOSRules(where string, t *Tier) []error {
	var errs []error

	// A host may be restricted to a subset of guest operating systems, and a
	// tier pinned to a host that does not permit its guest OS would queue
	// forever with nothing saying why.
	if t.Node != "" {
		if p, declared := c.NodePolicyFor(t.Node); declared && !p.AllowsGuestOS(t.GuestOS) {
			errs = append(errs, fmt.Errorf(
				"%s: guest_os %s is not in node %q's guest_os allowlist %v",
				where, t.GuestOS, t.Node, p.GuestOS))
		}
	} else {
		// An UNPINNED tier may be placed on any host, so a restrictive allowlist
		// constrains it even though it names no host. Checking only pinned tiers
		// left the allowlist bypassable: a macOS-only Mac could still be handed a
		// Linux guest, because nothing tied the tier to a host for the check to
		// fire on.
		//
		// The predicate is "could this tier actually land here", not guest OS
		// alone. A firecracker tier can never run on a Tart host, so a bare
		// guest-OS comparison would make declaring one macOS-only Mac an error
		// for every ordinary x64 Linux tier in the deployment. Only a host that
		// declares the SAME provider can be a placement candidate, which is why
		// this fires on provider match and stays silent otherwise.
		//
		// Silence is not safety: a node that declares no provider cannot be
		// reasoned about here, and the allocator enforces the allowlist again at
		// Bind, where the host is actually known.
		//
		// Only the first offending host is reported — one unpinned tier against
		// five restrictive hosts is one mistake, not five.
		for i := range c.Nodes {
			p := &c.Nodes[i]
			if p.Provider == t.Provider && len(p.GuestOS) > 0 && !p.AllowsGuestOS(t.GuestOS) {
				errs = append(errs, fmt.Errorf(
					"%s: guest_os %s is unpinned, but node %q runs the same provider and its "+
						"guest_os allowlist %v excludes it; pin this tier to a host that permits "+
						"it, or widen that allowlist",
					where, t.GuestOS, p.Name, p.GuestOS))

				break
			}
		}
	}

	switch t.GuestOS {
	case GuestMacOS:
		// Apple's licence permits macOS guests only on Apple-branded hardware,
		// which for billet means the Tart provider.
		if t.Provider != ProviderTart {
			errs = append(errs, fmt.Errorf(
				"%s: guest_os macos requires the tart provider (Apple hardware)", where))
		}
		if t.Node == "" {
			errs = append(errs, fmt.Errorf(
				"%s: guest_os macos requires an explicit node, so the per-host licence limit can be enforced", where))
			break
		}

		// The bound is the HOST's limit, not the package default, so lowering a
		// Mac's limit actually constrains the tiers pinned to it.
		limit := c.MacOSLimitForNode(t.Node)
		p, declared := c.NodePolicyFor(t.Node)

		switch {
		case limit < 0:
			// The node policy is itself invalid and validateNodes already says
			// so. Deriving a tier bound from a broken number would report the
			// same mistake again, pointing at the wrong field and with
			// arithmetic ("between 1 and -1") that reads as a billet bug.
		case limit == 0 && (!declared || p.AllowsGuestOS(GuestMacOS)):
			// A host explicitly set to zero macOS guests. The allowlist branch
			// above already covers the case where macos is absent from guest_os,
			// so reporting both would name one mistake twice.
			errs = append(errs, fmt.Errorf(
				"%s: node %q sets macos_vm_limit to 0, so it runs no macOS guests", where, t.Node))
		case limit == 0:
			// Reported by the allowlist check above.
		case t.MaxConcurrent <= 0 || t.MaxConcurrent > limit:
			errs = append(errs, fmt.Errorf(
				"%s: max_concurrent must be between 1 and %d, %s",
				where, limit, c.macOSLimitReason(t.Node)))
		}
	case GuestWindows:
		if t.Provider == ProviderTart {
			errs = append(errs, fmt.Errorf("%s: the tart provider cannot run Windows guests", where))
		}
	}
	return errs
}

// macOSLimitReason explains where a host's macOS limit came from, so a
// diagnostic says why the number is what it is. An operator who has not touched
// the setting needs to know the constraint is Apple's licence rather than a
// billet default they can simply raise; an operator who set it themselves needs
// to be pointed at their own field, not at Apple.
func (c *Config) macOSLimitReason(node string) string {
	if p, declared := c.NodePolicyFor(node); declared && p.MacOSVMLimit != nil {
		return fmt.Sprintf("the macos_vm_limit set for node %q", node)
	}

	return fmt.Sprintf(
		"Apple's licence limit of %d macOS guests per Apple-branded host (node %q does not override it)",
		DefaultMacOSVMLimit, node)
}

// validateNodes checks the fleet catalog on its own terms, before any tier
// refers to it.
func (c *Config) validateNodes() []error {
	var errs []error
	seen := make(map[string]struct{}, len(c.Nodes))

	for i := range c.Nodes {
		p := &c.Nodes[i]

		where := fmt.Sprintf("nodes[%d]", i)
		if p.Name != "" {
			where = fmt.Sprintf("node %q", p.Name)
		}

		if !labelRe.MatchString(p.Name) {
			errs = append(errs, fmt.Errorf("%s: name must match %s", where, labelRe))
		}
		if _, dup := seen[p.Name]; dup {
			// Two entries for one host means one of them is silently ignored,
			// and which one wins depends on ordering.
			errs = append(errs, fmt.Errorf("%s: duplicate node name", where))
		}
		seen[p.Name] = struct{}{}

		if p.Provider != "" && !p.Provider.valid() {
			errs = append(errs, fmt.Errorf("%s: provider %q is not one of %v", where, p.Provider, allProviders))
		}

		guests := make(map[GuestOS]struct{}, len(p.GuestOS))
		for _, g := range p.GuestOS {
			if !g.valid() {
				errs = append(errs, fmt.Errorf("%s: guest_os %q is not one of %v", where, g, allGuestOS))
			}
			if _, dup := guests[g]; dup {
				errs = append(errs, fmt.Errorf("%s: duplicate guest_os %q", where, g))
			}
			guests[g] = struct{}{}
		}

		if p.MacOSVMLimit == nil {
			continue
		}

		switch {
		case *p.MacOSVMLimit < 0:
			errs = append(errs, fmt.Errorf("%s: macos_vm_limit must not be negative", where))
		case *p.MacOSVMLimit > 0 && !p.AllowsGuestOS(GuestMacOS):
			// Both fields decide whether macOS runs here. Silently letting the
			// allowlist win would mean a config that reads as "two macOS guests"
			// schedules none, so the contradiction is rejected instead.
			errs = append(errs, fmt.Errorf(
				"%s: macos_vm_limit is %d but guest_os %v excludes macos; "+
					"add macos to guest_os or set macos_vm_limit to 0",
				where, *p.MacOSVMLimit, p.GuestOS))
		}
	}
	return errs
}

// validateMacOSHostLimits catches the case two individually-valid macOS tiers
// pinned to the same Mac collectively exceed its per-host limit. The allocator
// still has to count at runtime; this only stops the obvious mistake at load
// time.
func (c *Config) validateMacOSHostLimits() []error {
	perNode := make(map[string]int)

	// Node order follows the tier catalog rather than map iteration, so a config
	// with two bad hosts reports them the same way every run.
	var order []string

	for i := range c.Tiers {
		t := &c.Tiers[i]
		if t.GuestOS != GuestMacOS || t.Node == "" {
			continue
		}

		if _, seen := perNode[t.Node]; !seen {
			order = append(order, t.Node)
		}

		perNode[t.Node] += t.MaxConcurrent
	}
	var errs []error
	for _, node := range order {
		p, declared := c.NodePolicyFor(node)
		limit := p.MacOSLimit()

		// Both of these are already reported — against the node policy itself,
		// or against each offending tier. Repeating them as an aggregate
		// describes one mistake twice, and does it with false arithmetic: a host
		// whose allowlist excludes macOS has an effective limit of zero, so
		// rendering it through macOSLimitReason claims "1 guest exceeds Apple's
		// limit of 2", which is both wrong and points at the wrong field.
		if limit < 0 || (declared && !p.AllowsGuestOS(GuestMacOS)) {
			continue
		}

		if total := perNode[node]; total > limit {
			errs = append(errs, fmt.Errorf(
				"node %q: macOS tiers allow %d concurrent guests in total, exceeding %s",
				node, total, c.macOSLimitReason(node)))
		}
	}
	return errs
}

// validateCapacity catches the case where a single tier is defined larger than
// the machine will ever allow, which otherwise surfaces as jobs that queue
// forever with no explanation.
func (c *Config) validateCapacity() []error {
	if c.Server == nil {
		return nil
	}
	var errs []error

	for i := range c.Tiers {
		t := &c.Tiers[i]
		if c.Server.MaxVCPU > 0 && t.VCPU > c.Server.MaxVCPU {
			errs = append(errs, fmt.Errorf(
				"tier %q requests %d vCPU but server.max_vcpu is %d; jobs on this tier would never be schedulable",
				t.Label, t.VCPU, c.Server.MaxVCPU))
		}
		if c.Server.MaxMemory > 0 && t.Memory > c.Server.MaxMemory {
			errs = append(errs, fmt.Errorf(
				"tier %q requests %s but server.max_memory is %s; jobs on this tier would never be schedulable",
				t.Label, t.Memory, c.Server.MaxMemory))
		}
	}
	return errs
}

func validateHostPort(field, addr string) error {
	if addr == "" {
		return fmt.Errorf("%s is required", field)
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%s %q must be host:port: %w", field, addr, err)
	}
	if host == "" && !strings.HasPrefix(addr, ":") {
		return fmt.Errorf("%s %q has an empty host", field, addr)
	}
	// Validated directly rather than through net.LookupPort, which would accept a
	// service NAME ("http") and resolve it against /etc/services — a listen
	// address that means different things on different machines.
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s %q has an invalid port %q; expected 1-65535", field, addr, port)
	}
	return nil
}

// TierByLabel returns the tier a `runs-on` label refers to.
func (c *Config) TierByLabel(label string) (*Tier, bool) {
	for i := range c.Tiers {
		if c.Tiers[i].Label == label {
			return &c.Tiers[i], true
		}
	}
	return nil, false
}

// PollInterval is how often a node re-reports capacity to the server when
// nothing has changed. Assignment itself is push-driven; this is only a
// liveness and drift backstop.
const PollInterval = 15 * time.Second
