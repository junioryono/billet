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
	"unicode"

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
	// hostname is injectable so the node-name default can be tested against a
	// machine whose hostname is not a legal node name. Nil means os.Hostname.
	hostname func() (string, error)

	// nameDefaulted records THAT node.name was defaulted from the hostname;
	// nameFromHostname records what that hostname was.
	//
	// Two fields, because a non-empty value is not the same fact as "billet
	// supplied this". A machine whose hostname is blank or all whitespace
	// defaults to an empty name — still defaulted, still the case where the
	// operator needs to be told where it came from — and a single string field
	// reported that one with the generic wording, which sends someone who never
	// typed a name looking for a field that is not in their file.
	nameDefaulted    bool
	nameFromHostname string

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

// policyEnumsValid reports whether every enum this policy carries is a known
// value. Relational checks consult it so one typo produces one diagnostic
// against the field that holds it, rather than a second one phrased as an
// allowlist mismatch.
func (p NodePolicy) policyEnumsValid() bool {
	if p.Provider != "" && !p.Provider.Valid() {
		return false
	}

	for _, g := range p.GuestOS {
		if !g.Valid() {
			return false
		}
	}

	return true
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

	// MaxCustody bounds how long billet holds capacity for compute it cannot
	// account for — a container adopted from a crashed run, or one an ambiguous
	// launch may have left behind — before destroying it and reclaiming the
	// capacity.
	//
	// EMPTY MEANS NO BOUND, and that is the right default rather than a cautious
	// one. Elapsed time is not evidence that a job stopped making progress:
	// billet imposes no job limit, and self-hosted runners are routinely
	// configured past GitHub's six-hour default, so a bound picked by billet
	// would kill legitimate long jobs for no reason visible in the logs. Killing
	// live work should be authorised by a completion, an observed exit, or by an
	// operator who knows their own longest job — which is what this is.
	//
	// Billet warns hourly about held capacity regardless, which is the signal
	// that actually helps; this is a backstop for a runner that has wedged.
	//
	// A Go duration string: "24h", "90m".
	MaxCustody string `yaml:"max_custody,omitempty"`
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

// Valid reports whether this is a known provider. Exported because alloc.New
// must reject a catalog it cannot prove came through Load.
func (p ProviderKind) Valid() bool {
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

// Valid reports whether this is a known guest OS.
func (g GuestOS) Valid() bool {
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
	Org   string `yaml:"org"`
	AppID int64  `yaml:"app_id"`
	// ClientID is the App's OAuth client identifier, and it is OPTIONAL.
	//
	// GitHub's newer guidance prefers it over the numeric app id as the JWT
	// issuer, and the scale-set client accepts either — its GitHubAppAuth
	// documents ClientID as "the Client ID of the application (app id also
	// works)". So this must never become required: every config written before
	// the field existed keeps working.
	//
	// It is recorded because the manifest conversion already returns it, and
	// throwing away a value GitHub handed over means a second trip through the
	// browser to get it back. It is an identifier, not a secret — App.Forget
	// deliberately keeps it while blanking the client SECRET beside it.
	ClientID       string `yaml:"client_id,omitempty"`
	InstallationID int64  `yaml:"installation_id"`
	// PrivateKeyPath points at the App private key PEM. This file is the single
	// most sensitive thing in a billet deployment: it lives only on the control
	// plane, and nodes never hold long-lived GitHub credentials.
	PrivateKeyPath string `yaml:"private_key_path"`
}

// Tier is one runner shape. Its Label is what appears in `runs-on`.
type Tier struct {
	Label string `yaml:"label"`

	// Provider is the single backend this tier runs on. Kept because it is what
	// almost every deployment wants and what every existing config says; it is
	// normalized into Providers, which is what the rest of billet reads.
	Provider ProviderKind `yaml:"provider,omitempty"`

	// Providers is an ORDERED preference list, most preferred first.
	//
	// The reason one `runs-on` label can span a machine at home and a cloud: a
	// tier that lists `[firecracker, ec2]` may be placed on either, and losing
	// the bare-metal host does not take the label down with it. That is the
	// difference between self-hosted CI you can rely on and self-hosted CI you
	// can rely on until the power goes out.
	//
	// Setting both this and Provider is an error rather than a merge. They are
	// two spellings of the same field, and guessing which one an operator meant —
	// when the answer decides where untrusted code runs — is not a kindness.
	//
	// THE ORDER IS RECORDED BUT NOT YET ACTED ON. Nothing chooses among nodes
	// today: a node binds itself, so placement can only accept or refuse the node
	// that asked. Preference starts to bite when the node runs in its own process
	// and the control plane picks. Listing several providers already removes the
	// hard pin, which is what a fallback needs.
	Providers []ProviderKind `yaml:"providers,omitempty"`
	// GuestOS defaults to linux. Set it explicitly for macOS and Windows tiers —
	// licensing and capability checks key off this field, not off the label.
	GuestOS GuestOS `yaml:"guest_os,omitempty"`
	// Node optionally pins this tier to a named node. Required when only one
	// node can serve it — macOS tiers, for example.
	Node string `yaml:"node,omitempty"`
	// RunnerGroup is the GitHub runner group this tier's scale set belongs to.
	// Empty means GitHub's "default" group.
	//
	// It matters for access control rather than for scheduling: a runner group is
	// how an organization decides which repositories may use these runners, and
	// putting every tier in the default group hands them to every repository in
	// the org. billet does not enforce that policy — GitHub does — but it must be
	// expressible, or an operator has to go and move the scale set by hand after
	// every reconcile.
	RunnerGroup string `yaml:"runner_group,omitempty"`

	// NOTE on what is accepted: the scale-set client interpolates this name into
	// a query string WITHOUT escaping it, so a perfectly ordinary group name like
	// "Platform & Security" is parsed as two parameters and comes back as "group
	// not found" — a confusing first-contact failure that looks like a
	// permissions problem. Validation therefore rejects what the client cannot
	// carry, rather than letting GitHub answer confusingly. See validateTier.

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

// ProviderErrors reports everything wrong with a tier's backend declaration.
//
// One function, because `provider:` and `providers:` are two spellings of the
// same field and validating them separately is how they drift.
//
// EXPORTED because alloc.New is documented as unable to assume its catalogue
// came through Load — a caller can construct tiers directly — and it was
// therefore accepting tiers this package refuses, including a macOS tier with a
// non-Apple fallback. A rule that only one entry point enforces is a rule with a
// second entry point that does not.
func (t Tier) ProviderErrors(where string) []error {
	var errs []error

	// BOTH SPELLINGS IS AN ERROR, not a merge. Guessing which one an operator
	// meant is not a kindness when the answer decides where untrusted code runs.
	if t.Provider != "" && len(t.Providers) > 0 {
		errs = append(errs, fmt.Errorf(
			"%s: set either provider or providers, not both; they are two spellings of "+
				"the same field and billet will not guess which one you meant", where))
	}

	accepted := t.AcceptableProviders()

	if len(accepted) == 0 {
		errs = append(errs, fmt.Errorf("%s: no provider; set provider, or providers for a "+
			"tier that may fall back to another backend", where))

		return errs
	}

	seen := make(map[ProviderKind]struct{}, len(accepted))

	for _, p := range accepted {
		if !p.Valid() {
			errs = append(errs, fmt.Errorf("%s: provider %q is not one of %v", where, p, allProviders))

			continue
		}

		// A repeat is a typo, not a stronger preference — there is no second
		// chance at the same backend, so silently collapsing it would hide the
		// mistake in a list whose whole meaning is its order.
		if _, dup := seen[p]; dup {
			errs = append(errs, fmt.Errorf("%s: provider %q is listed twice", where, p))
		}

		seen[p] = struct{}{}
	}

	return errs
}

// GuestOSProviderErrors reports backends that cannot host a tier's guest OS.
//
// Split out from the fuller relational validation so alloc can apply the part
// that is a SAFETY invariant rather than a configuration convenience. Apple's
// licence permits macOS guests only on Apple hardware, and a catalogue built in
// code was able to declare a macOS tier that falls back to EC2 — runtime
// placement only tests list membership, so that lease would have bound to an EC2
// node quite happily.
func (t Tier) GuestOSProviderErrors(where string) []error {
	var errs []error

	for _, p := range t.AcceptableProviders() {
		switch {
		case t.GuestOS == GuestMacOS && p != ProviderTart:
			errs = append(errs, fmt.Errorf(
				"%s: guest_os macos requires the tart provider (Apple hardware), but this "+
					"tier also accepts %q", where, p))

		case t.GuestOS == GuestWindows && p == ProviderTart:
			errs = append(errs, fmt.Errorf("%s: the tart provider cannot run Windows guests", where))
		}
	}

	return errs
}

// AcceptableProviders reports the backends this tier may run on, most preferred
// first.
//
// The single reader for that question, so `provider:` and `providers:` cannot
// drift apart. Callers must not consult Tier.Provider directly.
// CLONED, because callers keep what this returns. The allocator copies the list
// onto every lease it reserves, and handing out the tier's own backing array
// meant a caller mutating one element afterwards changed what future leases
// authorize — with no revalidation, and directly against the snapshot rule that
// the rest of this design rests on. Following NodePolicy.Clone's precedent.
func (t Tier) AcceptableProviders() []ProviderKind {
	if len(t.Providers) > 0 {
		return slices.Clone(t.Providers)
	}

	if t.Provider != "" {
		return []ProviderKind{t.Provider}
	}

	return nil
}

// AcceptsProvider reports whether a tier may run on a backend.
func (t Tier) AcceptsProvider(p ProviderKind) bool {
	return slices.Contains(t.AcceptableProviders(), p)
}

// MaxCustodyDuration parses Node.MaxCustody, reporting zero when unset.
//
// Parsed on demand rather than at load time because the config type stays a
// plain data shape — but validation calls it too, so a typo is reported when the
// file is read rather than hours later when a container needs reclaiming.
func (n *NodeConfig) MaxCustodyDuration() (time.Duration, error) {
	if n == nil || strings.TrimSpace(n.MaxCustody) == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(strings.TrimSpace(n.MaxCustody))
	if err != nil {
		return 0, fmt.Errorf("node.max_custody: %q is not a duration like \"24h\": %w",
			n.MaxCustody, err)
	}

	if d < 0 {
		return 0, fmt.Errorf("node.max_custody: %q is negative; leave it unset for no bound",
			n.MaxCustody)
	}

	return d, nil
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

// runnerGroupUnsafe are the characters that do not survive the scale-set
// client's handling of a group name.
//
// This started as an allowlist and the allowlist was wrong in both directions.
// The client interpolates the name unescaped into a path (client.go:351), then
// url.Parse's it, reads Query(), and re-Encode's it — so the question is not
// "is this character legal in a URL" but "does this character survive one
// parse-and-re-encode round trip". Measured against v0.4.0, rather than
// reasoned about:
//
//	&   splits the value into another parameter    "Platform & Security" -> "Platform "
//	#   starts a fragment and truncates it         "a#b"                 -> "a"
//	;   ParseQuery has rejected it as a separator
//	    since Go 1.17 and drops the whole pair     "a;b"                 -> ""
//	%   is decoded and re-encoded, so a literal
//	    one is either destroyed or transformed     "100%" -> ""   "%41" -> "A"
//	+   is decoded to a space                      "a+b"                 -> "a b"
//
// Everything else survives, INCLUDING = and ? and / and : and quotes — and
// including non-ASCII, so the old rule rejected "Grupo-Ñ" and "研发" for no
// reason. Control characters are excluded separately: url.Parse refuses them
// outright, which is a clean failure but a much later and stranger one.
const runnerGroupUnsafe = "&#;%+"

// maxRunnerGroupLen is BILLET's sanity bound, not GitHub's rule.
//
// The 100 it replaces was carried over from the regex this check replaced, where
// it was equally invented — and stating an invented number as though it were
// GitHub's is the same mistake as the allowlist: a rule about someone else's API
// that is not pinned to that API's behaviour. GitHub does not document a runner
// group name length, so billet does not claim to know one.
//
// What is genuinely billet's concern is that a runaway config value cannot build
// a URL the Actions service rejects wholesale, and 512 bytes is far above any
// plausible group name while still catching a field that was pasted into by
// accident. It counts BYTES because the thing being bounded is a URL, and it is
// generous enough that the bytes-versus-runes distinction cannot reject a real
// name: 512 bytes is 170 characters even in the worst case for CJK text.
const maxRunnerGroupLen = 512

// checkRunnerGroup reports why a runner group name cannot be looked up, or nil.
//
// An empty name is fine and means GitHub's default group.
func checkRunnerGroup(group string) error {
	if group == "" {
		return nil
	}

	if strings.TrimSpace(group) == "" {
		return fmt.Errorf("runner_group %q is only whitespace; leave it unset to use GitHub's "+
			"default group", group)
	}

	if len(group) > maxRunnerGroupLen {
		return fmt.Errorf("runner_group is %d bytes, over billet's %d byte sanity limit; this is "+
			"not GitHub's rule, it is a guard against a config value that ran away",
			len(group), maxRunnerGroupLen)
	}

	for _, r := range group {
		switch {
		case unicode.IsControl(r):
			return fmt.Errorf("runner_group %q contains a control character (%U); the scale-set "+
				"client builds a URL from this name and url.Parse refuses control characters",
				group, r)
		case strings.ContainsRune(runnerGroupUnsafe, r):
			return fmt.Errorf("runner_group %q contains %q, which does not survive the scale-set "+
				"client's URL handling — the name that reaches GitHub would not be the one you "+
				"wrote, and the lookup comes back as \"group not found\". Rename the group, or "+
				"leave this unset to use GitHub's default group", group, r)
		}
	}

	return nil
}

// validateNodeName is the ONE rule for a node identifier, wherever it appears:
// node.name, nodes[].name, and tiers[].node all name hosts in a single
// namespace, so validating them differently lets the same string be a legal
// host here and an illegal one there.
//
// A whitespace-only pin is what made that concrete. This package treated it as
// "pinned" — a macOS tier satisfied the must-name-a-node rule and inherited a
// concurrency default from it — while the allocator trimmed it to empty and
// rejected the same tier. On a Linux tier the trim silently turned a pin into
// no pin at all, which is a placement decision changed by whitespace.
func ValidateNodeName(where, name string) error {
	if !labelRe.MatchString(name) {
		return fmt.Errorf("%s: node name %q must match %s", where, name, labelRe)
	}

	return nil
}

// trimNodeName strips surrounding whitespace ONLY when something is left.
//
// Trimming unconditionally destroyed the difference between "absent" and
// "present but unusable", and both directions were wrong. A `node.name` of
// "   " became empty and was replaced by the machine's hostname, silently
// adopting a different identity than the one written. A tier's `node: "   "`
// became unpinned, and the `if t.Node != ""` guard then skipped validation
// entirely — removing the operator's placement constraint, which is precisely
// what validating names was meant to prevent.
//
// Leaving a whitespace-only value intact lets it reach the pattern check and be
// rejected, which is the honest outcome.
func trimNodeName(name string) string {
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		return trimmed
	}

	return name
}

// Validate reports every way this policy is malformed on its own terms.
//
// Exported because internal/alloc must apply the SAME rules: its constructor
// accepts a catalog it cannot prove came through Load, and a second
// hand-written copy of these checks is how the two drift into disagreeing about
// which hosts are legal.
func (p NodePolicy) Validate(where string) []error {
	var errs []error

	if err := ValidateNodeName(where, p.Name); err != nil {
		errs = append(errs, err)
	}

	if p.Provider != "" && !p.Provider.Valid() {
		errs = append(errs, fmt.Errorf("%s: provider %q is not one of %v", where, p.Provider, allProviders))
	}

	seen := make(map[GuestOS]struct{}, len(p.GuestOS))

	for _, g := range p.GuestOS {
		if !g.Valid() {
			errs = append(errs, fmt.Errorf("%s: guest_os %q is not one of %v", where, g, allGuestOS))
		}

		if _, dup := seen[g]; dup {
			errs = append(errs, fmt.Errorf("%s: duplicate guest_os %q", where, g))
		}

		seen[g] = struct{}{}
	}

	if p.MacOSVMLimit == nil {
		return errs
	}

	switch {
	case *p.MacOSVMLimit < 0:
		errs = append(errs, fmt.Errorf("%s: macos_vm_limit must not be negative", where))
	case *p.MacOSVMLimit > 0 && !p.AllowsGuestOS(GuestMacOS):
		// Both fields decide whether macOS runs here. Silently letting the
		// allowlist win would mean a config that reads as "two macOS guests"
		// schedules none.
		errs = append(errs, fmt.Errorf(
			"%s: macos_vm_limit is %d but guest_os %v excludes macos; "+
				"add macos to guest_os or set macos_vm_limit to 0",
			where, *p.MacOSVMLimit, p.GuestOS))
	}

	return errs
}

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
	// Node names are normalized FIRST, before anything looks one up. A pin that
	// differs from its fleet entry only by surrounding whitespace would
	// otherwise fail to match it, and the tier would silently inherit fleet-wide
	// defaults instead of that host's policy.
	for i := range c.Nodes {
		c.Nodes[i].Name = trimNodeName(c.Nodes[i].Name)
	}

	for i := range c.Tiers {
		c.Tiers[i].Node = trimNodeName(c.Tiers[i].Node)
	}

	if c.Node != nil {
		c.Node.Name = trimNodeName(c.Node.Name)

		if c.Node.Name == "" {
			lookup := c.hostname
			if lookup == nil {
				lookup = os.Hostname
			}

			if h, err := lookup(); err == nil {
				c.Node.Name = strings.TrimSpace(h)
				c.nameDefaulted = true
				c.nameFromHostname = h
			}
		}
		if c.Node.StateDir == "" {
			c.Node.StateDir = defaultStateDir("node")
		}
		if c.Node.ServerAddr == "" && c.Server != nil {
			c.Node.ServerAddr = c.Server.Listen
		}

		// A fleet entry for THIS host that omits its provider takes the local
		// one. Without it the unpinned-tier check compares against an empty
		// provider and skips the host entirely — and in single-box mode that is
		// the only host there is.
		for i := range c.Nodes {
			if p := &c.Nodes[i]; p.Name == c.Node.Name && p.Provider == "" {
				p.Provider = c.Node.Provider
			}
		}
	}
	for i := range c.Tiers {
		t := &c.Tiers[i]
		// The PINNED host's provider wins over the local node's. Defaulting from
		// the local node is right for an unpinned tier and wrong for a pinned
		// one: on a multi-host deployment, the file describing the EPYC box would
		// otherwise stamp `firecracker` onto a tier pinned to a Mac, producing a
		// contradiction the operator never wrote. Pinning names a host, and that
		// host's declared provider is the more specific answer.
		// Only a VALID provider is inherited. Copying an unknown one produces a
		// second diagnostic blaming a field the operator never supplied, for a
		// typo they made once somewhere else.
		if len(t.AcceptableProviders()) == 0 && t.Node != "" {
			if p, declared := c.NodePolicyFor(t.Node); declared && p.Provider.Valid() {
				t.Provider = p.Provider
			}
		}

		// The local provider is inherited only when it is itself valid, for the
		// same reason: an invalid one copied here becomes a second diagnostic
		// against a field the operator never wrote.
		//
		// Checked against the whole LIST, not the single field. A tier written
		// with `providers:` leaves Provider empty, so testing that field alone
		// stamped the local backend onto a tier that had already named several —
		// and validation then refused the pair as "you set both", blaming the
		// operator for a field billet had just filled in itself.
		if len(t.AcceptableProviders()) == 0 && c.Node != nil && c.Node.Provider.Valid() {
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
	if !c.Node.Provider.Valid() {
		errs = append(errs, fmt.Errorf("node.provider %q is not one of %v", c.Node.Provider, allProviders))
	}

	// Parsed here so a typo is reported when the file is READ, rather than hours
	// later when a wedged container needed reclaiming and the bound turned out to
	// be unparseable.
	if _, err := c.Node.MaxCustodyDuration(); err != nil {
		errs = append(errs, err)
	}
	if err := ValidateNodeName("node.name", c.Node.Name); err != nil {
		// Say where the name came from when billet supplied it. A hostname is not
		// guaranteed to be a legal node name — a long FQDN exceeds the length
		// limit — and "node.name is invalid" sends an operator who never typed
		// one looking for a field that is not in their file.
		if c.nameDefaulted {
			err = fmt.Errorf(
				"node.name defaulted to this machine's hostname %q, which is not a usable node name "+
					"(must match %s); set node.name explicitly",
				c.nameFromHostname, labelRe)
		}

		errs = append(errs, err)
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

		// Rejected HERE rather than left for GitHub to answer confusingly. The
		// scale-set client interpolates this name into a query string without
		// escaping it, so a group name containing & — "Platform & Security" is a
		// realistic one — is parsed as several parameters and comes back as
		// "group not found". That reads as a permissions problem and sends an
		// operator to the wrong page entirely.
		if err := checkRunnerGroup(t.RunnerGroup); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		seen[t.Label] = struct{}{}

		errs = append(errs, t.ProviderErrors(where)...)
		if !t.GuestOS.Valid() {
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

		// A pin names a host, so it obeys the same rule as any other node name.
		if t.Node != "" {
			if err := ValidateNodeName(where, t.Node); err != nil {
				errs = append(errs, err)
			}
		}

		errs = append(errs, c.validateGuestOSRules(where, t)...)
	}
	return errs
}

func (c *Config) validateGuestOSRules(where string, t *Tier) []error {
	var errs []error

	// Relational checks are skipped when either side carries an invalid enum
	// value: the value itself is already reported, and comparing a typo against
	// an allowlist produces a second diagnostic describing the same mistake in
	// terms that send the reader to the wrong field.
	if !t.GuestOS.Valid() || len(t.ProviderErrors("")) > 0 {
		return errs
	}

	// A host may be restricted to a subset of guest operating systems, and a
	// tier pinned to a host that does not permit its guest OS would queue
	// forever with nothing saying why.
	if t.Node != "" {
		p, declared := c.NodePolicyFor(t.Node)

		switch {
		case !declared:
		case !p.policyEnumsValid():
			// Reported against the node itself.
		case !p.AllowsGuestOS(t.GuestOS):
			errs = append(errs, fmt.Errorf(
				"%s: guest_os %s is not in node %q's guest_os allowlist %v",
				where, t.GuestOS, t.Node, p.GuestOS))
		// ACCEPTS, not equals. A tier written with `providers:` leaves the singular
		// field empty, so comparing it let a plural tier pinned to a host it can
		// never bind to load perfectly cleanly — the whole point of this check
		// being that the failure would otherwise surface as a job that queues
		// forever.
		case p.Provider != "" && len(t.AcceptableProviders()) > 0 && !t.AcceptsProvider(p.Provider):
			// A tier pinned to a host running a different backend loads cleanly
			// and can never be placed: the host cannot run it. Silent at load
			// time, this is a job that queues forever.
			errs = append(errs, fmt.Errorf(
				"%s: this tier accepts %v and is pinned to node %q, which runs %s",
				where, t.AcceptableProviders(), t.Node, p.Provider))
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
			if !p.policyEnumsValid() {
				continue
			}

			// Again ACCEPTS rather than equals: a plural tier that could land on
			// this host is constrained by its allowlist, and comparing the empty
			// singular field meant a list containing tart was never checked
			// against a macOS-only Mac.
			if t.AcceptsProvider(p.Provider) && len(p.GuestOS) > 0 && !p.AllowsGuestOS(t.GuestOS) {
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
		// EVERY listed provider must be Tart, not merely one of them. A macOS tier
		// that could fall back to EC2 is a tier that would run macOS somewhere
		// Apple's licence does not permit — and the fallback is exactly the case
		// nobody is watching when it happens.
		errs = append(errs, t.GuestOSProviderErrors(where)...)
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
		errs = append(errs, t.GuestOSProviderErrors(where)...)
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

		// Each entry on its own terms, through the shared rules the allocator
		// also applies.
		errs = append(errs, p.Validate(where)...)

		if _, dup := seen[p.Name]; dup {
			// Two entries for one host means one of them is silently ignored,
			// and which one wins depends on ordering.
			errs = append(errs, fmt.Errorf("%s: duplicate node name", where))
		}
		seen[p.Name] = struct{}{}

		// The local node section and a fleet entry for the SAME host describe one
		// machine. Letting them disagree means the unpinned-tier check compares
		// against a provider the machine does not run and skips it — and in
		// single-box mode that is the only host there is, so every tier looks
		// placeable and none is.
		if c.Node != nil && p.Name == c.Node.Name &&
			p.Provider != "" && c.Node.Provider != "" && p.Provider != c.Node.Provider {
			errs = append(errs, fmt.Errorf(
				"%s: provider %s contradicts node.provider %s for the same host",
				where, p.Provider, c.Node.Provider))
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
