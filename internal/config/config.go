// Package config defines billet's on-disk configuration and its validation rules.
//
// A single billet.yaml describes both roles. `billet server` reads the server and
// github sections; `billet node` reads the node section. BOTH read the tier
// catalog — the node needs each tier's image, command, disk and shm, which the
// lease riding on a launch command does not carry — so the catalog is duplicated
// on every machine with nothing checking that the copies agree. On a single
// machine the two processes read the same file.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the whole of billet.yaml.
type Config struct {
	// Server is required by `billet server`. A node reads it too when the two
	// share a file on one machine: it is where a certless node learns which
	// deployment it is joining.
	Server *ServerConfig `yaml:"server,omitempty"`
	// Node is required by `billet node`, ignored by a pure server.
	Node *NodeConfig `yaml:"node,omitempty"`
	// GitHub is the target named DefaultTargetName: the one organization or
	// repository a single-target deployment serves. `billet server` needs it or
	// at least one entry under Targets.
	GitHub *GitHubConfig `yaml:"github,omitempty"`
	// Targets are the further organizations and repositories this deployment
	// serves, each with its own App credential. See GitHubTargets.
	Targets []GitHubConfig `yaml:"targets,omitempty"`
	// Tiers is the runner catalog. Each tier becomes one GitHub scale set, named by
	// its runs_on (its label unless it names one), which is what users put in
	// `runs-on`.
	Tiers []Tier `yaml:"tiers,omitempty"`
	// Backup is where archives go when they leave this disk. Optional: absent
	// means `billet local backup --out <dir>` is the whole story and an
	// operator's own tooling carries the directory.
	Backup *BackupConfig `yaml:"backup,omitempty"`
	// hostname is injectable so the node-name default can be tested against a
	// machine whose hostname is not a legal node name. Nil means os.Hostname.
	hostname func() (string, error)

	// nameDefaulted records THAT node.name was defaulted from the hostname;
	// nameFromHostname records what that hostname was.
	//
	// Two fields, because a non-empty value is not the same fact as "billet supplied
	// this": a machine whose hostname is blank defaults to an empty name, which is
	// still the case where the operator needs to be told where it came from.
	nameDefaulted    bool
	nameFromHostname string

	// Nodes describes per-host policy to the server. Separate from the Node section
	// on purpose: Node is how a host describes ITSELF, while Nodes is how the control
	// plane describes the FLEET — and host policy has to live server-side, because
	// the limits it expresses are enforced across tiers the host never sees.
	//
	// Every field defaults, so a deployment wanting standard behaviour omits it.
	Nodes []NodePolicy `yaml:"nodes,omitempty"`

	// Sites are the places this deployment has compute in. Optional: a single-machine
	// deployment never writes one.
	//
	// A SITE IS WHERE COMPUTE AND ITS STORAGE SHARE A FAST NETWORK — the answer to
	// "which storage", which every cache needs one of.
	//
	// DECLARED RATHER THAN INFERRED FROM WHAT NODES SAY, because the failure a free
	// string produces is silent: a node that means "home" and types "hom" would get
	// its own site, with its own empty cache, and every job placed there would run
	// cold while looking perfectly healthy.
	Sites []SiteConfig `yaml:"sites,omitempty"`

	// Images says where published guest images are fetched from. Optional: a
	// deployment that omits it pulls from where billet publishes.
	Images *ImagesConfig `yaml:"images,omitempty"`

	// Release says how this deployment learns about new billet releases.
	//
	// Optional, and its absence is the behaviour every existing install already
	// has: follow the signed stable channel, and update only when an operator
	// asks. See ReleaseConfig — nothing here starts a rollout by itself unless a
	// deployment says so in a sentence.
	Release *ReleaseConfig `yaml:"release,omitempty"`
}

// ImagesConfig points a deployment at a source of published guest images.
//
// CONFIGURABLE FROM THE FIRST RELEASE, AND THAT IS DELIBERATE. Retrofitting a
// second source onto a client that hardcoded one is the specific thing that hurt
// other projects distributing artifacts this way: when the single origin they
// baked in started rate-limiting, every consumer needed a new binary before any
// of them could point elsewhere. A deployment that mirrors internally, or is not
// on the public internet at all, must be able to say so in configuration.
type ImagesConfig struct {
	// Source is the directory the manifest and its assets sit in.
	//
	// Empty means billet's own published images. The default lives in
	// internal/imagesource, next to the one constant naming this project, so a
	// move does not have to be remembered in two places.
	Source string `yaml:"source,omitempty"`

	// SigningIdentity is the certificate SAN pattern a valid signature must carry.
	//
	// REQUIRED FOR A SOURCE THAT IS NOT BILLET'S OWN, because billet's identity
	// cannot vouch for what somebody else's mirror serves — and the alternative to
	// requiring it is silently not verifying, which is the failure this exists to
	// prevent.
	SigningIdentity string `yaml:"signing_identity,omitempty"`

	// SigningIssuer is the OIDC issuer that certificate must come from.
	//
	// A SAN says who a certificate is FOR; the issuer says who vouched for it.
	// Without this, any authority able to mint a certificate carrying that name
	// satisfies the policy.
	SigningIssuer string `yaml:"signing_issuer,omitempty"`
}

var labelRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// Load reads and validates a config file.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	return Parse(path, data)
}

// PeekIdentityBackend answers where a config says its identity material lives,
// WITHOUT validating anything else.
//
// IT EXISTS FOR ONE CALLER AND THE REASON IS AN ORDERING. `billet github-app
// create` runs against a config that is not valid yet — it has no app_id and no
// installation_id, because registering the App is what produces them — so Load
// would refuse it, and the command has to know before the browser flow whether
// the key it is about to receive belongs in a file or in a store. Reading one
// block tolerantly is the smallest thing that answers that.
//
// EVERY FAILURE ANSWERS `file`, which is the compatible direction: a config this
// cannot read is one whose key goes where every config's key has always gone, and
// the ordinary Load that follows will produce the real diagnostic.
func PeekIdentityBackend(data []byte) IdentityBackend {
	var peek struct {
		Server struct {
			Identity struct {
				Backend IdentityBackend `yaml:"backend"`
			} `yaml:"identity"`
		} `yaml:"server"`
	}

	// NOT KnownFields: this is deliberately reading ONE key out of a document
	// whose every other key it does not model.
	if err := yaml.Unmarshal(data, &peek); err != nil {
		return IdentityFile
	}

	if peek.Server.Identity.Backend == "" {
		return IdentityFile
	}

	return peek.Server.Identity.Backend
}

// Parse decodes and validates a config from bytes, naming it in diagnostics.
//
// The bytes half of Load, so a caller that already holds the config — a
// generator validating what it just rendered — can run it through the exact same
// decode, defaulting and validation as a file on disk, rather than a second copy
// of the rules that drifts from this one.
func Parse(name string, data []byte) (*Config, error) {
	c, err := ParseUnvalidated(name, data)
	if err != nil {
		return nil, err
	}

	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", name, err)
	}

	return c, nil
}

// ParseUnvalidated decodes, expands and applies defaults exactly as Parse does
// and stops before Validate, for a reader that judges one section of a
// configuration the loader as a whole would refuse: an endpoint decision's dry
// run over an operator's first emission (whose GitHub App ids are still zero,
// and refused at load by design) needs the node section and nothing else. The
// caller validates what it reads (ValidateNodeSection), and anything that
// installs or starts on the configuration goes through Parse.
func ParseUnvalidated(name string, data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true) // typos in a CI config should be loud, not ignored
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", name, relocatedKeyHint(err))
	}
	// A second document would otherwise be silently ignored, which for a config
	// that assigns capacity is a quiet way to run something other than intended.
	//
	// ONLY io.EOF IS THE END OF THE STREAM. yaml.v3 returns it by identity when
	// the parser reaches stream end; every other value is the parser saying the
	// REST OF THE FILE IS MALFORMED. Reading the two alike let `---` followed by a
	// truncated token reach defaults, validation and a running control plane as
	// though the file had ended cleanly — discarding whatever capacity, placement,
	// identity or security-policy override the operator wrote after it.
	var extra yaml.Node
	switch err := dec.Decode(&extra); {
	case err == nil:
		return nil, fmt.Errorf("config %s contains more than one YAML document", name)
	// The same wrapper the first decode uses, so both parse errors reach the
	// operator through one path rather than two that can drift.
	case !errors.Is(err, io.EOF):
		return nil, fmt.Errorf("parse config %s: %w", name, relocatedKeyHint(err))
	}

	// BEFORE DEFAULTS AND BEFORE VALIDATION, so nothing downstream ever sees an
	// unexpanded tier: applyDefaults fills in per-tier values the expansion has to
	// have produced first — a macOS tier's inherited concurrency cap among them —
	// and validation judges the tiers that will actually exist.
	expanded, err := ExpandTierSizes(c.Tiers)
	if err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", name, err)
	}

	c.Tiers = expanded

	c.applyDefaults()

	return &c, nil
}

// relocatedKeyHint turns "unknown field" into "that key moved, and here is where".
//
// KnownFields cannot tell a typo from a removal. For a typo that is the whole
// story; for a setting that was correct when the operator wrote it and has since
// moved or been replaced, the message names the key they already know about and
// says nothing about what to do.
//
// Pre-1.0 billet may break an existing config. It may NOT break one confusingly,
// and `field zfs_pool not found in type config.FirecrackerConfig` is exactly that:
// it names the key the operator wrote and nothing about the storage that replaced
// it.
func relocatedKeyHint(err error) error {
	msg := err.Error()

	// EVERY MATCH, not the first. KnownFields reports all the unknown fields it
	// found in one error, so a file carrying both `server.lock_dir` and
	// `node.firecracker.zfs_pool` gets both answers — returning inside the loop
	// sent the operator round again for the second one.
	var advice []string

	for _, hint := range keyHints {
		if strings.Contains(msg, hint.needle) {
			advice = append(advice, hint.advice)
		}
	}

	if len(advice) == 0 {
		return err
	}

	return fmt.Errorf("%w\n\n%s", err, strings.Join(advice, "\n\n"))
}

// keyHints maps the exact text KnownFields produces onto what to do about it.
//
// MATCHED ON THE WHOLE "field X not found in type Y" STRING rather than on the
// key alone, because the same key name can legitimately exist in another section
// and the advice for server.lock_dir is wrong for anything else called lock_dir.
var keyHints = []struct{ needle, advice string }{
	{
		needle: "field lock_dir not found in type config.ServerConfig",
		advice: "server.lock_dir has moved to node.lock_dir. The host-wide deployment lock stops " +
			"two processes managing one deployment's containers, and a control plane manages " +
			"none — the node takes it. A server that held it too would keep a node on the same " +
			"machine from ever starting, which is the single-machine deployment: `billet server` " +
			"and `billet node` side by side",
	},
	{
		needle: "field allow_unlocked_deployment not found in type config.ServerConfig",
		advice: "server.allow_unlocked_deployment has moved to node.allow_unlocked_deployment, " +
			"for the same reason server.lock_dir did: the lock belongs to the role that manages " +
			"containers",
	},
	{
		needle: "field zfs_pool not found in type config.FirecrackerConfig",
		advice: "node.firecracker.zfs_pool is gone. billet's storage is a Ceph cluster now, " +
			"configured as node.ceph with image_pool and cache_pool — a sibling of the " +
			"firecracker block, because storage belongs to the site rather than to the compute " +
			"backend. A ZFS clone exists only on the machine that took it, so a cache written to " +
			"one belonged to that host and to no other, which is the storage half of billet " +
			"being a one-machine product. " +
			"docs/reference/decisions/adr-003-ceph-rbd.md is how to build the cluster; " +
			"billet.example.yaml has the block to copy",
	},
}

func (c *Config) applyDefaults() {
	if c.Server != nil {
		if c.Server.Listen == "" {
			c.Server.Listen = "127.0.0.1:7717"
		}

		c.Server.applyStateDefaults()
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

	c.defaultTierTargets()
	c.materializeCacheScope()

	if c.Backup != nil {
		c.Backup.S3.normalize()
	}

	if c.Node != nil {
		c.Node.Name = trimNodeName(c.Node.Name)
		c.Node.EC2.normalize()
		c.Node.CodeBuild.Prepare()
		c.Node.EBSS3.normalize()
		c.Node.Ceph.normalize()
		c.Node.Cache.normalize()
		c.Node.RegistryMirrors.normalize()
		c.Node.Firecracker.Normalize()
		c.Node.Tart.Normalize()

		// THE CERTIFICATE DECIDES WHEN THERE IS ONE. The control plane authorises a
		// node by the name in its certificate, so with a bundle present the config
		// key is a second place to write the same fact — and the hostname default
		// actively fights it, because a machine whose hostname is not its node name
		// gets a name the control plane will refuse. `billet node` fills this in
		// from the bundle; see cmdNode.
		if c.Node.Name == "" && c.Node.TLS == nil {
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
		// Existing configurations become the restrictive pool shape. This is a
		// migration default, not a trust inference: an operator must opt into the
		// privileged shape together with its runner-group workflow boundary.
		if t.Trust == "" {
			t.Trust = WorkloadUntrusted
		}
		// The PINNED host's provider wins over the local node's: on a multi-host
		// deployment the file describing the EPYC box would otherwise stamp `firecracker`
		// onto a tier pinned to a Mac. Only a VALID provider is inherited, or an unknown
		// one produces a second diagnostic blaming a field the operator never supplied.
		if len(t.AcceptableProviders()) == 0 && t.Node != "" {
			if p, declared := c.NodePolicyFor(t.Node); declared && p.Provider.Valid() {
				t.Provider = p.Provider
			}
		}

		// The local provider is inherited only when it is itself valid, for the same
		// reason. Checked against the whole LIST, not the single field: a tier written
		// with `providers:` leaves Provider empty, so testing that field alone would stamp
		// the local backend onto a tier that had already named several — and validation
		// would then refuse the pair as "you set both".
		if len(t.AcceptableProviders()) == 0 && c.Node != nil && c.Node.Provider.Valid() {
			t.Provider = c.Node.Provider
		}
		if t.GuestOS == "" {
			t.GuestOS = GuestLinux
		}
		if t.BuildKitCacheMountLimit == 0 {
			t.BuildKitCacheMountLimit = DefaultBuildKitCacheMountLimit
		}
		// A macOS tier with no explicit cap inherits its host's limit rather than
		// "unlimited", so forgetting the field fails safe, and lowering a Mac's limit
		// tightens every macOS tier pinned to it. Only from a usable limit — copying a
		// negative one would turn one bad field into three diagnostics.
		if t.GuestOS == GuestMacOS && t.MaxConcurrent == 0 {
			limit := c.MacOSLimitForNode(t.Node)
			if t.Node == "" {
				// Unpinned: the hosts its backends declare, between them.
				limit = c.macOSUnpinnedLimit(t)
			}
			if limit > 0 {
				t.MaxConcurrent = limit
			}
		}
	}
}

// Validate reports every problem it finds rather than stopping at the first, so
// a misconfigured deployment takes one round trip to fix instead of five.
func (c *Config) Validate() error {
	var errs []error

	if c.Server == nil && c.Node == nil {
		errs = append(errs, errors.New("config defines neither a server nor a node section"))
	}

	errs = append(errs, c.validateServer()...)
	errs = append(errs, c.validateTargets()...)
	errs = append(errs, c.validateTargetKeyPaths()...)
	errs = append(errs, c.validateTierTargets()...)
	if c.Server != nil {
		errs = append(errs, TargetShareErrors(c.TargetShares(), c.Tiers,
			c.Server.MaxVCPU, c.Server.MaxMemory)...)
	}
	errs = append(errs, c.validateNode()...)
	errs = append(errs, c.validateMetrics()...)
	errs = append(errs, c.validateNoTestOnlyBackend()...)
	errs = append(errs, c.validateNodes()...)
	errs = append(errs, c.validateSites()...)
	errs = append(errs, c.validateCodeBuildTrust()...)
	errs = append(errs, c.validateTiers()...)
	errs = append(errs, c.validateCapacity()...)
	errs = append(errs, ValidateRelease(c.Release)...)
	errs = append(errs, c.validateBackup()...)

	return errors.Join(errs...)
}

// validateNoTestOnlyBackend refuses a file that names a backend that exists only
// for billet's own test harness, at every place a file could name one.
//
// HERE AND NOT IN THE PER-TIER OR PER-NODE RULES, deliberately: alloc.New
// re-applies Tier.ProviderErrors and NodePolicy.Validate to a catalogue built in
// code, and the end-to-end suite builds exactly such a catalogue over the
// simulated backend. What must never happen is a real deployment reaching it
// through a file, and Validate is the one gate every loaded file passes.
func (c *Config) validateNoTestOnlyBackend() []error {
	var errs []error

	refuse := func(where string, p ProviderKind) {
		errs = append(errs, testOnlyProviderError(where, p))
	}

	if err := c.testOnlyNodeProvider(); err != nil {
		errs = append(errs, err)
	}

	for i := range c.Tiers {
		t := &c.Tiers[i]
		where := fmt.Sprintf("tiers[%d]", i)

		for _, p := range t.AcceptableProviders() {
			if p.TestOnly() {
				refuse(where, p)
			}
		}

		// A launch block for the kind is refused on its own, because a tier that
		// does not accept the kind still carries a spelling nothing should read.
		for p := range t.Launch {
			if p.TestOnly() {
				refuse(where+".launch", p)
			}
		}
	}

	for i, n := range c.Nodes {
		if n.Provider.TestOnly() {
			refuse(fmt.Sprintf("nodes[%d]", i), n.Provider)
		}
	}

	return errs
}

// sortedKeys lists a set in a stable order, so a diagnostic naming what IS valid
// reads the same on every run.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}

	slices.Sort(out)

	return out
}

// PollInterval is how often a node re-reports capacity to the server when
// nothing has changed. Assignment itself is push-driven; this is only a
// liveness and drift backstop.
const PollInterval = 15 * time.Second
