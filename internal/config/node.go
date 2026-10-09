package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// NodePolicy is what one compute host is permitted to run.
//
// A host's capabilities are not implied by its provider: an Apple Silicon machine
// can serve macOS guests, Linux arm64 guests, or both, and which of those an
// operator wants is a deployment decision rather than a property of the hardware.
type NodePolicy struct {
	// Name matches Tier.Node and NodeConfig.Name.
	Name string `yaml:"name"`

	// Provider is the compute backend this host runs, matching NodeConfig.Provider.
	// Optional, and used only to decide whether an unpinned tier could ever land
	// here: without it a macOS-only Mac would appear to conflict with every x64 Linux
	// tier in the deployment.
	Provider ProviderKind `yaml:"provider,omitempty"`

	// GuestOS is an allowlist of what may be scheduled here. Empty means
	// unconstrained, which is the default and preserves the behaviour of a
	// config that never mentions the node.
	//
	// Note the shape difference from Tier.GuestOS, which is a single value: a
	// tier boots exactly one guest OS, while a host may permit several.
	GuestOS []GuestOS `yaml:"guest_os,omitempty"`

	// MacOSVMLimit caps concurrent macOS guests on this host, counting warm ones. Nil
	// means DefaultMacOSVMLimit — an unconfigured Apple host is still bound by
	// Apple's licence, so the default is the licence, not "unlimited".
	//
	// Raising it above DefaultMacOSVMLimit is permitted because billet cannot know
	// what licence an operator has, but Apple's standard terms allow at most
	// DefaultMacOSVMLimit macOS guests per Apple-branded host — exceeding that is an
	// assertion about YOUR licence, not a tuning knob.
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
// A shallow struct copy is not enough, and the difference is silent: GuestOS is a
// slice and MacOSVMLimit is a POINTER, so a caller holding the original could
// widen a host's allowlist or raise its macOS cap after the allocator was built
// from it — moving a licence limit out from under leases already counted.
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
// An allowlist that excludes macOS yields 0 whatever MacOSVMLimit says, so the two
// fields cannot disagree about whether macOS runs here.
func (p NodePolicy) MacOSLimit() int {
	if !p.AllowsGuestOS(GuestMacOS) {
		return 0
	}

	if p.MacOSVMLimit == nil {
		return DefaultMacOSVMLimit
	}

	return *p.MacOSVMLimit
}

// NodeTLS points at the three files `billet ca issue` produced.
//
// Paths rather than inline PEM: a private key pasted into a config file ends up in
// a backup, a paste buffer, and eventually a support thread.
type NodeTLS struct {
	// CertPath is this node's certificate. Its common name is the node name the
	// control plane will act on.
	CertPath string `yaml:"cert"`
	// KeyPath is the matching private key. A secret.
	KeyPath string `yaml:"key"`
	// CAPath is the deployment authority this node verifies the control plane
	// against.
	CAPath string `yaml:"ca"`
}

// NodeConfig configures a compute host.
type NodeConfig struct {
	// Name identifies this node to the server and in tier pinning. Defaults to
	// the hostname.
	Name string `yaml:"name,omitempty"`
	// ServerAddr is the control plane to dial. Nodes always initiate the
	// connection, so a node needs no inbound reachability of its own.
	ServerAddr string `yaml:"server_addr"`
	// BootstrapAddr is where `billet node --enroll` asks to join, when the control
	// plane serves enrollment on an address of its own (server.bootstrap_listen).
	//
	// USED ONCE AND NEVER AGAIN. A running node never touches those routes, so
	// this decides nothing after the certificate is written. Unset falls back to
	// server_addr, which is right for a control plane that has no separate
	// enrollment address; against one that does, the node wire refuses a
	// connection with no certificate and the fallback cannot work.
	BootstrapAddr string `yaml:"bootstrap_addr,omitempty"`
	// Provider selects the compute backend for this host.
	Provider ProviderKind `yaml:"provider"`

	// Site is where this machine physically is, naming one of the control
	// plane's declared sites. Optional, and only meaningful once a deployment
	// has more than one place.
	Site string `yaml:"site,omitempty"`

	// MaxVCPU and MaxMemory are what this host CONTRIBUTES, which is not what it has.
	// Unset means "everything I can detect".
	//
	// DECLARED ON THE MACHINE rather than in the control plane's config, because the
	// person running this host knows what else it does — the same reason the provider
	// is declared here.
	//
	// Setting them ABOVE what the machine has is allowed and warned about;
	// overcommitting is a decision an operator is entitled to make.
	MaxVCPU   int      `yaml:"max_vcpu,omitempty"`
	MaxMemory ByteSize `yaml:"max_memory,omitempty"`

	// TLS is the certificate bundle this node presents, issued by the control
	// plane's `billet ca issue`.
	//
	// REQUIRED TO DIAL ANYTHING BUT LOOPBACK. The wire's whole authorisation model
	// is the name in this certificate, so a node without one can only talk to a
	// control plane in its own machine.
	TLS *NodeTLS `yaml:"tls,omitempty"`
	// LockDir is where this node places the host-wide deployment lock.
	//
	// THE LOCK BELONGS TO THE NODE ROLE, because the node is what manages containers
	// and a control plane manages none. It is exclusive per identity, so a server that
	// took it would keep a node on the same machine from ever starting.
	//
	// THE LOCK'S SCOPE HAS TO MATCH THE DAEMON'S, and billet cannot derive that: every
	// process reaching the same container runtime must meet at the same directory. The
	// per-user default is wrong for a system service sharing /var/run/docker.sock, and
	// for containers sharing a socket with private filesystems.
	//
	// It must NOT be world-writable, or any local user could hold the file and keep
	// billet from starting. A directory shared between two accounts must be SETGID;
	// 2770 works everywhere, while 2730 works only where a directory can be opened for
	// search without reading it (Linux O_PATH, darwin and FreeBSD O_SEARCH).
	LockDir string `yaml:"lock_dir,omitempty"`
	// AllowUnlockedDeployment starts this node even when the host-wide lock cannot be
	// placed.
	//
	// AN OPT-IN, BECAUSE AUTHORIZATION MUST NOT BE DERIVED FROM AN I/O FAILURE.
	// Downgrading automatically would let a symlink loop, a permissions change,
	// ENOLCK, descriptor exhaustion or a service manager with no HOME each silently
	// switch off the protection, with a log line as the only evidence.
	AllowUnlockedDeployment bool `yaml:"allow_unlocked_deployment,omitempty"`
	// StateDir holds node-local data: the generation pointer store (which is
	// authoritative for this node's volumes), image cache, and mTLS identity.
	StateDir string `yaml:"state_dir"`
	// Firecracker is required when Provider is ProviderFirecracker.
	Firecracker *FirecrackerConfig `yaml:"firecracker,omitempty"`
	// Tart configures the Apple Silicon backend. Optional: a node running only
	// trusted tiers needs none of it.
	Tart *TartConfig `yaml:"tart,omitempty"`
	// EC2 is required when Provider is ProviderEC2.
	EC2 *EC2Config `yaml:"ec2,omitempty"`
	// CodeBuild is required when Provider is ProviderCodeBuild, and refused for
	// every other backend — the same rule as node.ec2 and node.firecracker, for
	// the same reason: nothing else reads it, so on another provider it is a
	// project, a fleet and a parameter path that look configured and are consulted
	// by nothing.
	CodeBuild *CodeBuildConfig `yaml:"codebuild,omitempty"`
	// EBSS3 is the cache store local to an EC2 site's instances. EBS carries
	// block generations and S3 carries the fenced per-key state.
	EBSS3 *EBSS3Config `yaml:"ebs_s3,omitempty"`
	// Ceph is the site's storage, required when Provider is ProviderFirecracker
	// and refused for every other backend.
	//
	// REFUSED RATHER THAN IGNORED, because nothing reads it on a host that cannot
	// attach a block device: a container has nowhere to put one, and an ec2 node
	// orchestrates compute in a region that cannot reach this cluster at all. A
	// block of settings that looks configured and is inert is the failure billet
	// refuses elsewhere — it reads as a working cache right up to the first job
	// that expected one.
	Ceph *CephConfig `yaml:"ceph,omitempty"`
	// Cache exposes the per-job sticky-volume API on a Firecracker guest bridge or
	// over TLS to EC2 guests. Optional: a node without it offers no dynamic cache
	// volumes to workflows.
	//
	// OPTIONAL IS NOT FREE, and `billet check` says so. A node that names a store
	// and no endpoint has a cache nothing can reach: an EBSS3 node is REFUSED
	// there, because that block backs nothing else, and a Ceph node is reported,
	// because image_pool still boots every guest. The refusal is not here because
	// `billet decommission` and `billet init iam` both read a store block on a
	// config that has already lost its listener.
	Cache *NodeCacheConfig `yaml:"cache,omitempty"`
	// Monitoring measures each job from the host. Absent means off; see
	// NodeMonitoringConfig.
	Monitoring *NodeMonitoringConfig `yaml:"monitoring,omitempty"`
	// RegistryMirrors are three site-local Distribution pull-through caches. One
	// instance per upstream is required because proxy mode has one remote URL.
	RegistryMirrors *RegistryMirrors `yaml:"registry_mirrors,omitempty"`

	// MaxCustody bounds how long billet holds capacity for compute it cannot account
	// for — a container adopted from a crashed run, or one an ambiguous launch may
	// have left behind — before destroying it. A Go duration string.
	//
	// EMPTY MEANS NO BOUND, deliberately. Elapsed time is not evidence that a job
	// stopped making progress: billet imposes no job limit and self-hosted runners are
	// routinely configured past GitHub's six-hour default, so a bound picked by billet
	// would kill legitimate long jobs for no reason visible in the logs. Billet warns
	// hourly about held capacity regardless.
	MaxCustody string `yaml:"max_custody,omitempty"`
	// DrainTimeout bounds how long a stopping node waits for the compute it is still
	// holding before letting the teardown destroy it. A Go duration string.
	//
	// Separate from the control plane's key: the two are restarted for different
	// reasons and need not wait the same amount of time.
	DrainTimeout string `yaml:"drain_timeout,omitempty"`
	// Metrics serves the node's Prometheus metrics. Absent, nothing is served:
	// there is no default address.
	Metrics *MetricsConfig `yaml:"metrics,omitempty"`
	// Stop is what a SIGTERM does while this node holds compute: "drain" (the
	// default) waits for it, "handoff" leaves it running for the next node process
	// to adopt. See NodeConfig.HandsOverOnStop.
	Stop string `yaml:"stop,omitempty"`
}

// NodeCacheConfig exposes storage to one guest through short-lived credentials.
type NodeCacheConfig struct {
	// Listen is one literal, non-loopback address guests can reach. Wildcards are
	// refused because they can expose the bearer-token API on another interface.
	Listen string `yaml:"listen"`
	// GuestEndpoint is the HTTP origin placed in guest metadata. It must name the
	// same address as Listen; the per-instance bearer token authorizes every call.
	GuestEndpoint string `yaml:"guest_endpoint"`
	// TLSCert and TLSKey terminate the HTTPS endpoint an EC2 guest reaches across
	// the VPC. Firecracker's isolated bridge uses HTTP and refuses these fields.
	TLSCert string `yaml:"tls_cert,omitempty"`
	TLSKey  string `yaml:"tls_key,omitempty"`
}

// RegistryMirrors names the independent public-registry caches visible at a site.
type RegistryMirrors struct {
	DockerIO string `yaml:"docker.io" json:"docker.io"`
	GHCRIO   string `yaml:"ghcr.io" json:"ghcr.io"`
	QuayIO   string `yaml:"quay.io" json:"quay.io"`
}

// registryMirrorOriginRe is the strict syntax gate, capturing the host and the
// port so the origin can be canonicalized from what it accepted.
//
// A TERMINAL DNS ROOT DOT IS REFUSED RATHER THAN CANONICALIZED, and that is a
// decision rather than an accident of the pattern: the host must end
// alphanumeric, so `https://cache.home.example.` is not an accepted spelling and
// there is no second form to fold into the first.
var registryMirrorOriginRe = regexp.MustCompile(
	`^https://([A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?)(?::([1-9][0-9]{0,4}))?$`)

// Empty reports whether no mirror was configured.
func (r *RegistryMirrors) Empty() bool {
	return r.DockerIO == "" && r.GHCRIO == "" && r.QuayIO == ""
}

func (r *RegistryMirrors) normalize() {
	if r == nil {
		return
	}

	for _, value := range []*string{&r.DockerIO, &r.GHCRIO, &r.QuayIO} {
		*value = strings.TrimSpace(*value)
		*value = strings.TrimSuffix(*value, "/")
	}
}

// registryMirrorOrigin is the Distribution instance an accepted endpoint
// addresses, for COMPARISON ONLY — the operator's own spelling stays in the
// config and is what reaches the guest.
//
// TWO SPELLINGS ARE ONE SERVICE, and comparing the raw strings missed both:
// DNS host names are case-insensitive, and 443 is https's default port, so
// https://CACHE.example, https://cache.example and https://cache.example:443 all
// name one listener. Three tiers pointed at "three separate instances" that way
// would silently share one proxy — and one Distribution process accepts ONE
// upstream, which is the entire reason three are required.
//
// WHAT THIS PROVES IS NARROWER THAN "three instances", and the difference is
// worth stating rather than implying: two DIFFERENT origins can still be one
// process behind two DNS names or two forwarded ports, and no amount of URL
// syntax can see that. Distinct origins are the most a config file can be held
// to; the rest is the operator's to get right.
func registryMirrorOrigin(host, port string) string {
	origin := "https://" + strings.ToLower(host)
	if port != "" && port != "443" {
		origin += ":" + port
	}

	return origin
}

// CheckRegistryMirrors refuses a set that cannot be three separate HTTPS origins.
func CheckRegistryMirrors(r RegistryMirrors) []error {
	var errs []error
	seen := make(map[string]struct{ upstream, endpoint string }, 3)

	for _, mirror := range []struct{ upstream, endpoint string }{
		{"docker.io", r.DockerIO},
		{"ghcr.io", r.GHCRIO},
		{"quay.io", r.QuayIO},
	} {
		where := "node.registry_mirrors." + mirror.upstream
		match := registryMirrorOriginRe.FindStringSubmatch(mirror.endpoint)
		if match == nil {
			errs = append(errs, fmt.Errorf("%s must be an HTTPS origin", where))

			continue
		}
		if match[2] != "" {
			port, err := strconv.Atoi(match[2])
			if err != nil || port > 65535 {
				errs = append(errs, fmt.Errorf("%s has an invalid port", where))

				// One diagnostic per bad field: an endpoint already refused has
				// no business also being reported as somebody's duplicate.
				continue
			}
		}

		origin := registryMirrorOrigin(match[1], match[2])
		if previous, exists := seen[origin]; exists {
			// BOTH ORIGINAL SPELLINGS, because that is what the operator has to
			// find in their file — naming only the canonical form would print an
			// origin that appears in it nowhere.
			errs = append(errs, fmt.Errorf("%s %q and node.registry_mirrors.%s %q are the same "+
				"origin %s; Distribution proxy mode permits one upstream per instance, so these "+
				"two upstreams would share one cache", where, mirror.endpoint, previous.upstream,
				previous.endpoint, origin))
		} else {
			seen[origin] = struct{ upstream, endpoint string }{mirror.upstream, mirror.endpoint}
		}
	}

	return errs
}

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

// MaxCustodyDuration parses Node.MaxCustody, reporting zero when unset.
//
// Parsed on demand so the config type stays a plain data shape — but validation
// calls it too, so a typo is reported when the file is read rather than hours
// later when a container needs reclaiming.
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

// NodePolicies is the declared fleet policy keyed by node name. The allocator is
// built from it, so runtime enforcement and this package's load-time guard read
// the same rules rather than two copies that can drift.
//
// Only DECLARED hosts appear; an absent host is unconstrained in guest OS and
// carries Apple's default macOS limit.
func (c *Config) NodePolicies() map[string]NodePolicy {
	policies := make(map[string]NodePolicy, len(c.Nodes))

	for i := range c.Nodes {
		policies[c.Nodes[i].Name] = c.Nodes[i].Clone()
	}

	return policies
}

// ValidateNodeName is the ONE rule for a node identifier, wherever it appears:
// node.name, nodes[].name and tiers[].node all name hosts in a single namespace,
// so validating them differently lets the same string be a legal host here and an
// illegal one there.
//
// A whitespace-only pin is the concrete case: treated as "pinned" here it
// satisfies the must-name-a-node rule, while a consumer that trims it sees no pin
// at all — a placement decision changed by whitespace.
func ValidateNodeName(where, name string) error {
	if !labelRe.MatchString(name) {
		return fmt.Errorf("%s: node name %q must match %s", where, name, labelRe)
	}

	return nil
}

// trimNodeName strips surrounding whitespace ONLY when something is left.
//
// Trimming unconditionally destroys the difference between "absent" and "present
// but unusable", and both directions are wrong: a node.name of "   " would become
// empty and silently adopt the machine's hostname, and a tier's `node: "   "`
// would become unpinned and skip validation entirely.
//
// Leaving a whitespace-only value intact lets it reach the pattern check and be
// rejected.
func trimNodeName(name string) string {
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		return trimmed
	}

	return name
}

// Validate reports every way this policy is malformed on its own terms.
//
// Exported because internal/alloc must apply the SAME rules: its constructor
// accepts a catalog it cannot prove came through Load, and a second hand-written
// copy is how the two drift into disagreeing about which hosts are legal.
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

// ValidateNodeSection judges the node section alone, as Validate judges it
// inside the whole: the provider (a test-only one refused as the whole
// validation refuses it), the addresses, the TLS material and the node's own
// limits. A configuration with no node section validates.
func (c *Config) ValidateNodeSection() error {
	if c.Node == nil {
		return nil
	}

	errs := c.validateNode()
	if err := c.testOnlyNodeProvider(); err != nil {
		errs = append(errs, err)
	}

	return errors.Join(errs...)
}

// testOnlyNodeProvider is the node half of validateNoTestOnlyBackend, shared
// with ValidateNodeSection so the node-only judgement cannot admit a provider
// the whole one refuses.
func (c *Config) testOnlyNodeProvider() error {
	if c.Node != nil && c.Node.Provider.TestOnly() {
		return testOnlyProviderError("node.provider", c.Node.Provider)
	}

	return nil
}

func testOnlyProviderError(where string, p ProviderKind) error {
	return fmt.Errorf("%s: provider %q starts no compute and fabricates completions; it exists for billet's own test "+
		"harness and cannot be named in a configuration", where, p)
}

// validateNodeTLS refuses a node that would dial the network unauthenticated.
//
// THE NAME IN THE CERTIFICATE IS THE ONLY THING THAT AUTHORISES A NODE, so a
// host without one can only reach a control plane inside its own machine. The
// alternative — letting it try, and failing at the handshake — reports the
// problem on the wrong machine at the wrong time, in a TLS error that does not
// mention the config key that caused it.
func (c *Config) validateNodeTLS() []error {
	var errs []error

	if c.Node.TLS == nil {
		if c.Node.ServerAddr != "" && !isLoopbackHostPort(c.Node.ServerAddr) {
			errs = append(errs, fmt.Errorf(
				"node.server_addr is %q, which is not on this machine, but node.tls is not set: "+
					"the control plane identifies a node by the name in its certificate and will "+
					"refuse a connection without one. Run `billet ca issue %s` on the control "+
					"plane and copy the bundle here",
				c.Node.ServerAddr, c.Node.Name))
		}

		return errs
	}

	// THE SERVER'S LISTEN ADDRESS DECIDES THE TRANSPORT, and the node's destination
	// cannot be used to infer it: a control plane listening on 0.0.0.0 serves mTLS on
	// EVERY interface, loopback included, so a node colocated with it dials 127.0.0.1
	// AND needs its certificate.
	//
	// So the question is only answerable when this file describes both roles. A
	// standalone node's file says nothing about how the server bound its listener.
	if c.Server != nil && LoopbackAddr(c.Server.Listen) {
		errs = append(errs, fmt.Errorf(
			"node.tls is set, but server.listen is %q — a control plane that accepts only on "+
				"loopback serves plain HTTP, because there is nothing between the two "+
				"processes to authenticate. This node would dial https and never connect. "+
				"Remove node.tls, or bind the server to the address it publishes to its fleet",
			c.Server.Listen))

		return errs
	}

	for key, path := range map[string]string{
		"node.tls.cert": c.Node.TLS.CertPath,
		"node.tls.key":  c.Node.TLS.KeyPath,
		"node.tls.ca":   c.Node.TLS.CAPath,
	} {
		if path == "" {
			errs = append(errs, fmt.Errorf("%s is required when node.tls is set", key))

			continue
		}

		// Absolute, because a node is a long-lived service whose working directory is
		// whatever started it: a relative certificate path resolves differently under
		// systemd than in the shell where it was tested.
		if !filepath.IsAbs(path) {
			errs = append(errs, fmt.Errorf(
				"%s must be an absolute path, got %q: a relative one resolves against whatever "+
					"working directory the service happens to start in", key, path))
		}
	}

	// SORTED, because the map above iterates in a random order and an error list
	// that reshuffles between runs is one an operator cannot diff.
	slices.SortFunc(errs, func(a, b error) int { return strings.Compare(a.Error(), b.Error()) })

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

	// Absolute, because a relative path resolves against each process's working
	// directory: one config could put the server and the node into different
	// collision domains on one host.
	if c.Node.LockDir != "" && !filepath.IsAbs(c.Node.LockDir) {
		errs = append(errs, fmt.Errorf(
			"node.lock_dir must be an absolute path, got %q: a relative one resolves against "+
				"each process's working directory, so the node and the server could lock "+
				"different files for one deployment identity", c.Node.LockDir))
	}

	// ZERO IS "I DID NOT SAY" AND NEGATIVE IS NOT A SMALLER OFFER: a negative
	// contribution is a ceiling every comparison passes, which switches the capacity
	// check off rather than offering a little. The memory side is refused when the
	// size is parsed; this one is a plain int.
	if c.Node.MaxVCPU < 0 {
		errs = append(errs, fmt.Errorf("node.max_vcpu is %d; leave it unset to contribute "+
			"everything this machine has", c.Node.MaxVCPU))
	}

	// Parsed here so a typo is reported when the file is READ, rather than hours
	// later when a wedged container needed reclaiming and the bound turned out to
	// be unparseable.
	if _, err := c.Node.DrainTimeoutDuration(); err != nil {
		errs = append(errs, err)
	}

	if handOver, err := c.Node.HandsOverOnStop(); err != nil {
		errs = append(errs, err)
	} else if handOver && c.Node.Provider != ProviderFirecracker {
		// ONLY WHERE THE GUESTS OUTLIVE THE SERVICE, AND ARE ADOPTED. A
		// Firecracker VMM runs in a cgroup of its own and the next node process
		// adopts it; no other backend has been shown to do both (#374).
		errs = append(errs, fmt.Errorf("node.stop: handoff needs node.provider: firecracker, "+
			"whose guests outlive the node's service and are adopted by its next process; "+
			"%s nodes drain", c.Node.Provider))
	}

	if _, err := c.Node.MaxCustodyDuration(); err != nil {
		errs = append(errs, err)
	}
	// AN EMPTY NAME WITH A BUNDLE IS NOT A MISSING NAME. The control plane
	// authorises a node by the name in its certificate, so the bundle carries it
	// and `billet node` fills this in once it is read.
	if c.Node.Name != "" || c.Node.TLS == nil {
		if err := ValidateNodeName("node.name", c.Node.Name); err != nil {
			// Say where the name came from when billet supplied it: a hostname is
			// not guaranteed to be a legal node name, and "node.name is invalid"
			// sends an operator who never typed one looking for a field not in
			// their file.
			if c.nameDefaulted {
				err = fmt.Errorf(
					"node.name defaulted to this machine's hostname %q, which is not a usable "+
						"node name (must match %s); set node.name explicitly",
					c.nameFromHostname, labelRe)
			}

			errs = append(errs, err)
		}
	}

	// SYNTAX IS LOCAL, MEMBERSHIP IS THE CONTROL PLANE'S. Whether this site was
	// declared cannot be answered here — see the note in validateSites — but
	// whether the name billet will present at registration is the one the
	// operator wrote can be, and this is the only machine that can say so before
	// the registration is refused on another one.
	if err := checkIdentityPadding("node.site", c.Node.Site); err != nil {
		errs = append(errs, err)
	}

	if c.Node.StateDir == "" {
		errs = append(errs, errors.New("node.state_dir is required"))
	}
	if err := validateHostPort("node.server_addr", c.Node.ServerAddr); err != nil {
		errs = append(errs, err)
	}

	// CHECKED HERE OR NOWHERE. It is used once, by `billet node --enroll`, which
	// prefixes it with https:// and dials — so a value that is a URL already, or
	// carries a path, or is a bare host, loads cleanly and fails months later at
	// the one moment an operator is standing at a new machine with a join token.
	if c.Node.BootstrapAddr != "" {
		if err := validateHostPort("node.bootstrap_addr", c.Node.BootstrapAddr); err != nil {
			errs = append(errs, err)
		}
	}

	errs = append(errs, c.validateNodeTLS()...)

	switch {
	case c.Node.Provider == ProviderFirecracker && c.Node.Firecracker == nil:
		errs = append(errs, errors.New("node.firecracker is required when provider is firecracker"))

	case c.Node.Provider == ProviderFirecracker:
		errs = append(errs, CheckFirecracker(*c.Node.Firecracker)...)

	// REFUSED RATHER THAN IGNORED, exactly as node.ceph is a few lines below. Only
	// firecracker reads this block, so on any other provider it is a jail directory,
	// a uid range and a bridge that look configured and are consulted by nothing —
	// and the shape that produces it is somebody switching a node's provider and
	// leaving the old block behind, which is precisely when a silent acceptance is
	// most expensive.
	case c.Node.Firecracker != nil:
		errs = append(errs, fmt.Errorf("node.firecracker is set but this node's provider is %s, "+
			"and only firecracker reads it, so this host would carry a jail directory, a uid "+
			"range and a bridge that nothing consults", c.Node.Provider))
	}

	errs = append(errs, c.validateTartNode()...)
	errs = append(errs, c.validateCephNode()...)
	errs = append(errs, c.validateEBSS3Node()...)
	errs = append(errs, c.validateCacheNode()...)
	errs = append(errs, c.validateMonitoringNode()...)
	if c.Node.Cache == nil {
		for i := range c.Tiers {
			if len(c.Tiers[i].explicitGuestCaches()) > 0 &&
				c.Tiers[i].AcceptsProvider(c.Node.Provider) {
				errs = append(errs, fmt.Errorf("tier %q enables a cache the node serves, but "+
					"node.cache is not configured on this %s node", c.Tiers[i].Label, c.Node.Provider))

				break
			}
		}
	}
	if c.Node.RegistryMirrors != nil {
		if c.Node.Provider != ProviderFirecracker {
			errs = append(errs, fmt.Errorf("node.registry_mirrors is set but this node's provider is %s, and only the managed Firecracker guest consumes it", c.Node.Provider))
		} else {
			errs = append(errs, CheckRegistryMirrors(*c.Node.RegistryMirrors)...)
		}
	}

	if c.Node.Provider == ProviderEC2 {
		errs = append(errs, c.validateEC2Node()...)
	}

	errs = append(errs, c.validateCodeBuildNode()...)

	return errs
}

// validateCacheNode keeps the bearer-token service on an isolated Firecracker
// bridge or an authenticated TLS connection from an EC2 guest.
func (c *Config) validateCacheNode() []error {
	if c.Node.Cache == nil {
		return nil
	}

	if c.Node.Provider != ProviderFirecracker && c.Node.Provider != ProviderEC2 {
		return []error{fmt.Errorf("node.cache is set but this node's provider is %s, and only "+
			"firecracker and ec2 can hot-attach their block volumes", c.Node.Provider)}
	}

	cache := c.Node.Cache
	var errs []error
	if err := validateHostPort("node.cache.listen", cache.Listen); err != nil {
		errs = append(errs, err)
	} else {
		host, _, splitErr := net.SplitHostPort(cache.Listen)
		if splitErr != nil {
			errs = append(errs, fmt.Errorf("node.cache.listen: %w", splitErr))

			return errs
		}
		ip := net.ParseIP(host)
		if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
			errs = append(errs, errors.New("node.cache.listen must use one literal, non-loopback "+
				"address guests can reach; wildcards and hostnames could expose the bearer-token "+
				"API on another interface"))
		}
	}

	u, err := url.Parse(cache.GuestEndpoint)
	if err != nil || u.Opaque != "" || u.Host == "" {
		errs = append(errs, errors.New("node.cache.guest_endpoint must be an HTTP origin the guest can reach"))

		return errs
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		errs = append(errs, errors.New("node.cache.guest_endpoint must be an origin without "+
			"credentials, a path, a query, or a fragment"))
	}

	listenHost, listenPort, listenErr := net.SplitHostPort(cache.Listen)
	if c.Node.Provider == ProviderFirecracker {
		if u.Scheme != "http" {
			errs = append(errs, errors.New("node.cache.guest_endpoint must use http on the isolated Firecracker bridge"))
		}
		if cache.TLSCert != "" || cache.TLSKey != "" {
			errs = append(errs, errors.New("node.cache.tls_cert and tls_key are only used by an EC2 HTTPS listener"))
		}
		endpointIP := net.ParseIP(u.Hostname())
		if listenErr == nil && (endpointIP == nil || !endpointIP.Equal(net.ParseIP(listenHost)) ||
			u.Port() != listenPort) {
			errs = append(errs, errors.New("node.cache.guest_endpoint must name exactly the address in "+
				"node.cache.listen, so metadata cannot direct a guest to a different host"))
		}
	} else {
		if c.Node.EBSS3 == nil {
			errs = append(errs, errors.New("node.cache on an EC2 node needs node.ebs_s3"))
		}
		if u.Scheme != "https" {
			errs = append(errs, errors.New("node.cache.guest_endpoint must use https for an EC2 guest; its bearer token crosses the VPC"))
		}
		if cache.TLSCert == "" || cache.TLSKey == "" || !filepath.IsAbs(cache.TLSCert) ||
			!filepath.IsAbs(cache.TLSKey) {
			errs = append(errs, errors.New("node.cache.tls_cert and tls_key must be absolute paths for an EC2 HTTPS listener"))
		}
		if listenErr == nil && u.Port() != listenPort {
			errs = append(errs, errors.New("node.cache.guest_endpoint must use the port in node.cache.listen"))
		}
	}

	return errs
}

func (c *NodeCacheConfig) normalize() {
	if c == nil {
		return
	}

	c.Listen = strings.TrimSpace(c.Listen)
	c.GuestEndpoint = strings.TrimSpace(c.GuestEndpoint)
	c.TLSCert = strings.TrimSpace(c.TLSCert)
	c.TLSKey = strings.TrimSpace(c.TLSKey)
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
