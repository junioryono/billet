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
	"net"
	"net/netip"
	"net/url"
	"os"
	pathpkg "path"
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
	// Server is required by `billet server`. A node reads it too when the two
	// share a file on one machine: it is where a certless node learns which
	// deployment it is joining.
	Server *ServerConfig `yaml:"server,omitempty"`
	// Node is required by `billet node`, ignored by a pure server.
	Node *NodeConfig `yaml:"node,omitempty"`
	// GitHub is required by `billet server`.
	GitHub *GitHubConfig `yaml:"github,omitempty"`
	// Tiers is the runner catalog. Each tier becomes one GitHub scale set, and
	// its Label is what users put in `runs-on`.
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

// SiteConfig declares one place compute runs.
//
// A STRUCT RATHER THAN A STRING, because a site declares both placement identity
// and its intended storage backend — Ceph at a bare-metal site, EBS and S3 in a
// cloud region. The control plane validates both parts when a remote node
// registers, so split configs cannot create two storage authorities for one
// logical site.
type SiteConfig struct {
	// Name is what a node and a tier refer to this site by.
	Name string `yaml:"name"`
	// Store is the storage local to this site. Ceph serves host-backed compute;
	// EBS snapshots and S3 serve AWS without exposing the home cluster over a WAN.
	Store SiteStoreKind `yaml:"store"`
}

// SiteStoreKind selects the storage implementation local to one site.
type SiteStoreKind string

const (
	// SiteStoreCeph is an RBD cluster on the site's own storage network.
	SiteStoreCeph SiteStoreKind = "ceph"
	// SiteStoreEBSS3 stores block generations in EBS and fenced state in S3.
	SiteStoreEBSS3 SiteStoreKind = "ebs-s3"
)

// Valid reports whether this is a recognized storage backend name.
func (s SiteStoreKind) Valid() bool {
	return s == SiteStoreCeph || s == SiteStoreEBSS3
}

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

// StateBackend names the engine the control-plane ledger lives in.
type StateBackend string

const (
	// StateSQLite is the default and the recommended shape for a laptop, one
	// owned server, or the small controller ADR-001 describes. It is explicitly
	// SINGLE CONTROLLER: the exclusive lock on the state directory is what stops
	// a second one, and there is no shared-storage story, because SQLite's WAL
	// cannot work on a network filesystem.
	StateSQLite StateBackend = "sqlite"

	// StatePostgres puts the ledger in a database billet does not operate, which
	// is what makes the controller replaceable — the scheduling state outlives
	// the machine, so recovery is a managed backup rather than a directory.
	//
	// IT IS NOT HIGH AVAILABILITY ON ITS OWN. Exactly one controller may make
	// scheduling decisions either way, and a database's ability to serialize
	// writes is not proof that only one process is polling GitHub.
	StatePostgres StateBackend = "postgres"
)

// IdentityBackend names where a deployment's identity material lives.
type IdentityBackend string

const (
	// IdentityFile is the default and what every deployment has today: the
	// node-wire authority and the GitHub App private key are files in
	// identity_dir. It is the right answer for one controller, and it is the only
	// answer for a deployment with no AWS account.
	IdentityFile IdentityBackend = "file"

	// IdentitySSM puts them in AWS Systems Manager Parameter Store as
	// SecureStrings, so an active/passive pair shares one authority instead of
	// two copies somebody has to keep in step.
	//
	// PARAMETER STORE RATHER THAN SECRETS MANAGER, and the deciding fact is a
	// deletion: DeleteParameter is immediate where DeleteSecret imposes a
	// seven-day recovery window unless forced. billet already speaks this service,
	// with signing vectors in the tree, so it is one client rather than two.
	IdentitySSM IdentityBackend = "aws-ssm"
)

// IdentityConfig is where the deployment's identity material lives.
//
// SEPARATE FROM `state:` BECAUSE THE TWO ARE NOT INTERCHANGEABLE, which is the
// same sentence identity_dir already carries. A ledger is rows and can move into
// a database; a private key cannot follow it there, which is why the pairing is
// a refusal.
type IdentityConfig struct {
	Backend IdentityBackend `yaml:"backend"`

	// AWSSSM configures the store when Backend selects it, and is REFUSED
	// otherwise rather than ignored — the same rule the `state:` block follows,
	// and for the same reason: silently ignoring a block produces a deployment
	// that believes it configured something.
	AWSSSM *IdentitySSMConfig `yaml:"aws_ssm,omitempty"`
}

// IdentitySSMConfig names the Parameter Store path this deployment's identity
// lives under.
type IdentitySSMConfig struct {
	// Region is the SIGNING region, and it also selects the endpoint: there is no
	// override, because an override is a way to send a deployment's private key to
	// a host of somebody's choosing.
	Region string `yaml:"region"`

	// Prefix isolates one deployment inside an account. Everything billet stores
	// lands under it, so IAM can be scoped by path and two deployments cannot read
	// each other's authority.
	Prefix string `yaml:"prefix"`

	// KMSKeyID names the key that encrypts the SecureStrings. Empty uses the
	// account's default SSM key, which is what a deployment that has not chosen
	// one gets — and which is a real choice rather than an omission, because that
	// key's policy is what decides who else in the account can read them.
	KMSKeyID string `yaml:"kms_key_id,omitempty"`
}

// ControllerMode says how many control planes this deployment runs, and
// therefore what a controller does when it finds the claim already held.
type ControllerMode string

const (
	// ControllersSingle is the default and what every deployment has today: one
	// control plane, and a second one is a MISTAKE that says so loudly. It exits
	// non-zero naming the machine that holds the claim, and `Restart=on-failure`
	// repeats that refusal every RestartSec until somebody fixes it.
	ControllersSingle ControllerMode = "single"

	// ControllersActivePassive says this deployment runs more than one control
	// plane on purpose. Whichever takes the claim first is the controller; the
	// others WAIT, and one of them takes over when the incumbent's database
	// session ends.
	//
	// BOTH HOSTS WRITE THE SAME VALUE, and that symmetry is the reason it is a
	// property of the deployment rather than a flag on one process. After a
	// failover the standby IS the controller, so a per-process spelling would
	// leave a file describing a role its host no longer has.
	//
	// IT IS NOT AUTOMATIC, and the diagnostic is why. If waiting were what every
	// refused controller did, two machines misconfigured as active would stop
	// being a loud restart loop and become a deployment that looks healthy and
	// has quietly halved itself.
	ControllersActivePassive ControllerMode = "active-passive"
)

// StateConfig is the versioned form of "where does the ledger live".
type StateConfig struct {
	Backend StateBackend `yaml:"backend"`

	// Postgres configures the ledger when Backend selects it, and is REFUSED
	// otherwise rather than ignored — the same way a `node.ceph` block on a
	// non-firecracker backend is, and for the same reason: silently ignoring it
	// produces a deployment that believes it configured something.
	//
	// THERE IS NO `sqlite:` BLOCK, and its absence is deliberate. The only thing
	// it could carry is a path, the SQLite ledger is always billet.db inside
	// identity_dir, and every part of billet that reads a ledger FILE — the
	// restore planner, the writer barrier, the archive — derives it that way. A
	// key that billet accepted and then did not use would be a deployment
	// started against a freshly created empty ledger, which is not a failure
	// anybody would see until the fleet came back empty.
	Postgres *PostgresStateConfig `yaml:"postgres,omitempty"`
}

// PostgresStateConfig is the ledger in PostgreSQL.
type PostgresStateConfig struct {
	// DSNEnv names the ENVIRONMENT VARIABLE holding the connection string, and
	// the indirection is the point: a DSN carries a password, and a secret
	// written into YAML ends up in a backup, a paste buffer, and eventually a
	// support thread. It is the same rule the GitHub App private key follows.
	DSNEnv string `yaml:"dsn_env"`
}

// ServerConfig configures the control plane.
type ServerConfig struct {
	// Listen is the address nodes dial. Nodes always connect outbound, so on a
	// single-box deployment this stays on loopback and billet needs no open port
	// reachable from anywhere else.
	Listen string `yaml:"listen"`
	// BootstrapListen is a SECOND address serving only the two routes a machine
	// that has never enrolled needs: reading this deployment's authority, and
	// asking to join.
	//
	// ITS ABSENCE IS A REFUSAL, not a default. Without it this control plane does
	// not enroll over the network at all, and admission happens out of band —
	// `billet ca issue <node>` on the server, the bundle copied to the host.
	//
	// It exists because those two routes cannot require a certificate, and a
	// listener that admits callers who need not prove anything cannot share a
	// connection budget with the fleet: an anonymous caller that completes a
	// handshake and idles holds a slot, and once the budget is full a healthy
	// node's connection is never accepted. So `listen` demands a certificate in
	// the handshake and serves nothing else, and this address carries the rest.
	//
	// Both listeners present the same certificate, so whatever name a node dials
	// this one by has to be covered by node_tls_hosts (or by a concrete host in
	// one of the two listen addresses, which billet derives them from).
	//
	// Refused against a loopback `listen`: there are no certificates on a loopback
	// wire, so there is nothing to enroll into.
	BootstrapListen string `yaml:"bootstrap_listen,omitempty"`
	// StateDir is the SHORTHAND, and it is what most deployments write: one
	// directory holding the SQLite ledger, the process lock, the maintenance
	// fence and the mTLS CA. It MUST be on local storage, because SQLite's WAL
	// cannot work on a network filesystem and the state package fails closed if
	// it detects otherwise.
	//
	// It means exactly `identity_dir: <dir>` plus `state: {backend: sqlite}`, and
	// it stays supported. Writing it TOGETHER with `state:` is refused rather
	// than merged: two spellings of one value is a mistake internal/config has
	// already made three times, and it is silent every time.
	StateDir string `yaml:"state_dir,omitempty"`

	// IdentityDir holds what is NOT rows: the deployment identity, the node-wire
	// CA and its rotation state, the process lock, and the maintenance fence.
	//
	// SEPARATE FROM THE LEDGER BECAUSE THE TWO ARE NOT INTERCHANGEABLE. A ledger
	// can move into a database billet does not operate; a private key cannot
	// follow it there, and local process coordination has nothing to do with SQL
	// rows. The pairing is a refusal for the same reason: a subset of a
	// deployment is one that starts, looks healthy, and is not.
	//
	// Required when `state:` is written out; `state_dir` supplies it otherwise.
	IdentityDir string `yaml:"identity_dir,omitempty"`

	// State selects the backend the ledger lives in. Absent means the shorthand
	// above.
	State *StateConfig `yaml:"state,omitempty"`

	// Controllers says whether this deployment runs one control plane or an
	// active/passive pair. Absent means `single`, which is what every deployment
	// before this key had.
	//
	// REFUSED ON A SQLITE LEDGER. There is nothing to elect over: the ledger is a
	// file on local storage that a second machine cannot open at all, so a standby
	// would be a second process on one host waiting for a lock its own service
	// manager already restarts it to take.
	Controllers ControllerMode `yaml:"controllers,omitempty"`

	// Identity says where the deployment's identity material lives. Absent means
	// `file`, which is what every deployment before this key had.
	Identity *IdentityConfig `yaml:"identity,omitempty"`
	// MaxVCPU and MaxMemory bound what the allocator will ever hand out across every
	// tier combined. Required and positive: capacity is escrowed before each listener
	// advertises, so an absent ceiling lets concurrent listeners collectively
	// overcommit the machine.
	MaxVCPU   int      `yaml:"max_vcpu"`
	MaxMemory ByteSize `yaml:"max_memory"`

	// Placement decides which of several suitable machines a job is sent to.
	// Empty means pack. Only meaningful once a deployment has more than one.
	Placement PlacementPolicy `yaml:"placement,omitempty"`
	// NodeTLSHosts are the names and addresses nodes will dial this control plane by.
	// They become the subject names of the certificate it serves.
	//
	// REQUIRED WHEN listen IS A WILDCARD, which says which interfaces to accept on and
	// nothing about what a node types: a certificate minted for "0.0.0.0" matches
	// nothing, and the failure arrives as a handshake error on the node. A concrete
	// listen address supplies itself.
	NodeTLSHosts []string `yaml:"node_tls_hosts,omitempty"`
	// DrainTimeout bounds how long a stopping control plane waits for the jobs it is
	// already running before it destroys them. A Go duration string: "6h", "90m".
	//
	// A service manager's stop timeout must exceed this plus the teardown, or its own
	// expiry arrives first as a SIGKILL — skipping the teardown and stranding exactly
	// the compute the drain was protecting.
	DrainTimeout string `yaml:"drain_timeout,omitempty"`
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
	Cache *NodeCacheConfig `yaml:"cache,omitempty"`
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

// ProviderKind names a compute backend.
type ProviderKind string

const (
	// ProviderFirecracker runs one Firecracker microVM per job on bare metal.
	// Requires /dev/kvm.
	ProviderFirecracker ProviderKind = "firecracker"
	// ProviderTart runs macOS and Linux arm64 guests on Apple Silicon. Requires
	// Tart, which is FSL-licensed and installed separately.
	ProviderTart ProviderKind = "tart"
	// ProviderEC2 launches one instance per job — on demand unless node.ec2.spot
	// says otherwise, because a reclaimed spot instance is a failed build that
	// GitHub will not requeue. Firecracker is not an option on EC2 outside .metal
	// instances, so here the instance itself is the isolation boundary, which is
	// also why this backend may run untrusted work at all.
	ProviderEC2 ProviderKind = "ec2"
	// ProviderCodeBuild runs one AWS CodeBuild build per job, started through the
	// API after billet has already escrowed the job.
	//
	// IT DOES NOT USE CODEBUILD'S OWN GITHUB ACTIONS RUNNER INTEGRATION, which is
	// webhook-only and would take over job detection, runner registration and
	// scheduling. billet starts an ordinary NO_SOURCE project and runs GitHub's
	// runner from its own JIT configuration, exactly as the ec2 backend does inside
	// an instance — see docs/adr-007-codebuild-provider.md.
	//
	// It is how billet reaches AWS-MANAGED APPLE SILICON, through a reserved-capacity
	// MAC_ARM fleet, without an operator allocating an EC2 Mac Dedicated Host. It is
	// also the one backend carrying an EXTERNAL job ceiling: CodeBuild caps a build
	// at 36 hours and fails a queued one after at most 8, neither of which billet can
	// lift, which is why accept_external_build_ceiling has no default. And it refuses
	// untrusted work outright rather than gating it on a network, because a
	// reserved-capacity instance survives between builds and shares cached state
	// across projects in the account.
	ProviderCodeBuild ProviderKind = "codebuild"
	// ProviderDocker runs jobs in containers. Isolation is materially weaker than
	// a VM; this exists so `billet init` works on a laptop and it refuses
	// untrusted workloads outright.
	ProviderDocker ProviderKind = "docker"
)

var allProviders = []ProviderKind{
	ProviderFirecracker, ProviderTart, ProviderEC2, ProviderCodeBuild, ProviderDocker,
}

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

// RunsOnHost reports whether this backend runs jobs on the machine billet is
// running on, so that machine's cores and memory are what it can offer.
//
// Every backend but ec2 does. An ec2 node is an ORCHESTRATOR: it holds
// credentials and calls an API, and the compute appears somewhere else entirely,
// so what the box it runs on happens to have says nothing about what it can
// contribute. Reading the two alike makes a t4g.nano offer two vCPU to a fleet it
// could buy a hundred of, and makes an honest `max_vcpu: 512` look like a typo
// worth warning about on every boot.
//
// AN ALLOWLIST RATHER THAN `!= ec2`, so a second remote backend that nobody
// remembers to add here is treated as remote — which loses a warning, where the
// other direction would invent a contribution out of the wrong machine's
// hardware. `codebuild` is that second remote backend, and it arrived without
// this function needing a line: the allowlist already answered correctly for a
// name it had never heard of, which is what the shape was chosen for.
func (p ProviderKind) RunsOnHost() bool {
	switch p {
	case ProviderDocker, ProviderFirecracker, ProviderTart:
		return true
	case ProviderEC2, ProviderCodeBuild:
		return false
	default:
		return false
	}
}

// ShapeField names the configuration key holding a remote backend's ordered
// purchasable shapes, so a diagnostic about one points at the field the operator
// actually wrote.
//
// ONE PLACE DECIDES THE SPELLING, because the shape validator is shared: it is
// called from config loading, where the key is known, and from the allocator,
// where a node's REGISTERED provider is the only thing that says which key its
// shapes came out of. Two copies of that mapping is a diagnostic naming
// `node.ec2.instance_types` at an operator whose file says
// `node.codebuild.compute_types`.
// RemoteProviders is every backend whose compute runs somewhere other than the
// node's own machine.
//
// DERIVED FROM RunsOnHost RATHER THAN LISTED, so a third remote backend is included
// by the same allowlist that already decides how it is charged. A second hand-written
// list is how `billet status` came to report the cost exposure of an ec2 fleet and
// nothing at all for a codebuild one, which reads as a fleet that costs nothing.
func RemoteProviders() []ProviderKind {
	out := make([]ProviderKind, 0, len(allProviders))

	for _, p := range allProviders {
		if !p.RunsOnHost() {
			out = append(out, p)
		}
	}

	return out
}

func (p ProviderKind) ShapeField() string {
	switch p {
	case ProviderCodeBuild:
		return "node.codebuild.compute_types"
	case ProviderEC2:
		return "node.ec2.instance_types"
	default:
		return string(p) + " shapes"
	}
}

// shapeNoun is how a shape validator's prose refers to what it is validating —
// "EC2 instance types", "CodeBuild compute types" — so the sentence explaining
// why the list has to be declared reads correctly for either backend.
func (p ProviderKind) shapeNoun() string {
	switch p {
	case ProviderCodeBuild:
		return "CodeBuild compute types"
	case ProviderEC2:
		return "EC2 instance types"
	default:
		return string(p) + " shapes"
	}
}

// GuestOS classifies what a tier boots.
//
// An explicit field rather than inferred from the label, because Apple's licensing
// limit is enforced against it: inferring "this is macOS" from the operator's
// chosen label lets a tier named `sonoma-arm64` silently escape the cap.
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
	//
	// BinaryPath IS NOT ONLY AN EXECUTABLE. The jailer names its chroot after this
	// file AFTER RESOLVING SYMLINKS, so it also decides the directory billet
	// enumerates to find out what is running here — see the firecracker provider's
	// jail type, where getting it wrong reads as an empty inventory.
	BinaryPath string `yaml:"binary_path,omitempty"`
	JailerPath string `yaml:"jailer_path,omitempty"`
	// KernelImage is the uncompressed guest kernel. It must be built with
	// everything Docker needs; validate with moby's contrib/check-config.sh
	// rather than a hand-maintained list.
	KernelImage string `yaml:"kernel_image"`

	// KernelDir is where `billet images pull` keeps the kernels it fetches.
	//
	// SEPARATE FROM KernelImage BECAUSE THEY ANSWER DIFFERENT QUESTIONS. KernelImage
	// is the fallback for a generation that records no kernel of its own — a
	// hand-built image, whose builder installs none. This is where a PAIRED kernel is
	// looked up, and a generation that names one boots that one instead: the two are
	// published together and a mismatch fails inside somebody's job rather than at
	// launch.
	//
	// Empty means the default, which is where a pull puts them.
	KernelDir string `yaml:"kernel_dir,omitempty"`
	// ChrootBase is where the jailer builds each microVM's chroot. It defaults to
	// the jailer's own default, and it must be on local storage with room for a
	// hard link to the guest kernel per running job.
	ChrootBase string `yaml:"chroot_base,omitempty"`
	// JailUIDMin and JailUIDCount are the range of uids microVMs run as, ONE PER
	// GUEST.
	//
	// NOT ONE ACCOUNT FOR ALL OF THEM. The jailer drops each VMM to a uid, and a
	// shared one means every VMM on the host is the same user to the kernel — so a
	// VMM that escapes its chroot can reach every other jail's files, signal every
	// other VMM, and open every other guest's root disk. The chroot is what
	// separates them while it holds; a uid of its own is what separates them when
	// it does not.
	//
	// THEY ARE NUMBERS, NOT ACCOUNTS, and deliberately: an account a person could
	// log in as is a bigger thing than an integer the kernel uses to keep two
	// processes apart, and creating one per job is a deployment step per job. The
	// default range is far above anything a distribution allocates.
	JailUIDMin   int `yaml:"jail_uid_min,omitempty"`
	JailUIDCount int `yaml:"jail_uid_count,omitempty"`
	// Bridge is the host bridge trusted guests attach to.
	Bridge string `yaml:"bridge"`
	// UntrustedBridge is the bridge fork pull-request guests attach to, and its
	// ABSENCE is what refuses them.
	//
	// A microVM is a real isolation boundary, which is why this backend can run
	// code billet cannot vouch for at all — but that boundary is the KERNEL, not
	// the network. A guest on the ordinary bridge reaches whatever that bridge
	// reaches, which on a machine that also holds the Ceph cluster and the
	// control-plane database is everything that matters. So untrusted work runs
	// only once its network has been described separately, rather than defaulting
	// onto the trusted bridge because nobody said otherwise. This is the same rule,
	// and the same reasoning, as node.ec2.untrusted_security_group_ids.
	UntrustedBridge string `yaml:"untrusted_bridge,omitempty"`
	// ImageVerifyPort is the host port a verification guest reports to. It is
	// fixed so host policy can admit exactly this service instead of opening an
	// arbitrary high port to every guest. Only one verification runs per host,
	// enforced by the verification lock.
	ImageVerifyPort int `yaml:"image_verify_port,omitempty"`
}

// TartConfig is the Apple Silicon backend's node settings.
type TartConfig struct {
	// UntrustedIsolation names the mechanism that confines a fork pull
	// request's guest, and its ABSENCE is what refuses one.
	//
	// The same rule, and the same reasoning, as
	// node.firecracker.untrusted_bridge: a tart VM is a real kernel boundary,
	// and the NETWORK is not one. Tart's default is shared NAT, where a guest
	// reaches the host and can ARP-spoof the vmnet bridge to read another
	// guest's traffic — so untrusted work runs only once its confinement has
	// been described, rather than landing on the default because nobody said
	// otherwise.
	//
	// STATED BY THE OPERATOR RATHER THAN DETECTED, because billet cannot prove
	// this from the host. What it can see of softnet is two metadata bits — a
	// setuid bit and an owner of root — which say the helper could start, not
	// what its policy then permits. Naming it here is the operator asserting
	// the mechanism is the one they want, exactly as naming a bridge is.
	UntrustedIsolation TartIsolation `yaml:"untrusted_isolation,omitempty"`

	// UntrustedDNS are the resolvers an isolated guest is given, and billet
	// gives them because billet is what took the working one away.
	//
	// MEASURED: under softnet a guest's DHCP-assigned resolver is the vmnet
	// gateway, which sits in the private address space softnet blocks. Egress
	// to public addresses keeps working and TCP/443 keeps working, so nothing
	// looks wrong — every job simply fails to resolve github.com. Public
	// resolvers are reachable under exactly the policy that broke the gateway
	// one.
	UntrustedDNS []string `yaml:"untrusted_dns,omitempty"`
}

// Normalize trims the block and fills the resolver default.
//
// EXPORTED FOR THE SAME REASON CheckFirecracker AND CheckTart ARE: the
// provider's constructor cannot assume its configuration came through Load, and
// a value trimmed for the CHECK while the caller launches with the raw one is
// the defect the ec2 and ceph blocks each shipped with once.
func (t *TartConfig) Normalize() {
	if t == nil {
		return
	}

	t.UntrustedIsolation = TartIsolation(strings.TrimSpace(string(t.UntrustedIsolation)))

	for i := range t.UntrustedDNS {
		t.UntrustedDNS[i] = strings.TrimSpace(t.UntrustedDNS[i])
	}

	// ONLY WHERE IT IS READ. Defaulting resolvers on a node that never isolates
	// a guest would put two addresses in an operator's rendered config that
	// nothing consults, which is the "looks configured, is inert" shape billet
	// refuses elsewhere.
	if t.UntrustedIsolation != "" && len(t.UntrustedDNS) == 0 {
		t.UntrustedDNS = DefaultUntrustedDNS()
	}
}

// TartIsolation is a confinement mechanism for untrusted guests.
type TartIsolation string

// IsolationSoftnet is tart's own userspace packet filter, which restricts a
// guest to public destinations and isolates guests from each other on the
// bridge. It is the only mechanism billet drives today; the type exists so a
// second one is a new value rather than a new meaning for a boolean.
const IsolationSoftnet TartIsolation = "softnet"

// DefaultUntrustedDNS is what an isolated guest resolves through when the
// operator names no resolver: Cloudflare and Google, both public, both
// reachable under the policy that blocks the gateway resolver.
//
// Two of them, from different operators, because a runner that cannot resolve
// fails every job on the host and the second one costs nothing.
func DefaultUntrustedDNS() []string { return []string{"1.1.1.1", "8.8.8.8"} }

// DefaultFirecrackerBinary and friends are where the reference host installs
// these, which is also where each project's own instructions put them.
const (
	DefaultFirecrackerBinary = "/usr/local/bin/firecracker"
	DefaultJailerBinary      = "/usr/local/bin/jailer"
	// DefaultKernelDir is where managed kernels are installed and where a
	// generation's recorded kernel name is resolved at launch.
	DefaultKernelDir = "/var/lib/billet/kernels"
	// DefaultChrootBase is the jailer's own default, so billet and a hand-run
	// jailer agree about where a microVM lives.
	DefaultChrootBase = "/srv/jailer"
	// DefaultJailUIDMin is where the per-microVM uid range starts.
	//
	// ABOVE EVERYTHING A DISTRIBUTION USES. Regular accounts stop at 60000 on
	// Debian and Ubuntu, systemd's DynamicUser range is 61184-65519, and the
	// subordinate-uid ranges `useradd` hands out for user namespaces start at
	// 100000 and are allocated 65536 at a time. Starting at 900000 sits clear of
	// all of it, and clear of the 65534 `nobody` that a truncated or overflowed
	// value would otherwise land on.
	DefaultJailUIDMin = 900000
	// DefaultJailUIDCount is how many microVMs one host may run at once, as far as
	// uids go. Far above what any machine can hold, so it is a guard rather than a
	// capacity limit — the allocator's budget is the real one.
	DefaultJailUIDCount = 1024
	// DefaultImageVerifyPort is adjacent to the control plane and guest cache
	// ports, and is reserved for the short-lived `billet images verify` listener.
	DefaultImageVerifyPort = 7719
	// MinJailUID is the lowest start billet will accept. Below this a range starts
	// overlapping accounts that belong to somebody, and a microVM would run as a
	// user with a home directory and a login shell.
	MinJailUID = 100000
)

// Normalize fills in the defaults and trims what billet later passes verbatim.
//
// EXPORTED FOR THE SAME REASON CheckFirecracker IS. The provider's constructor is
// exported and cannot assume its configuration came through Load, and a value that
// was trimmed for a CHECK while the caller used the raw one is the exact defect the
// ec2 and ceph blocks each shipped with once.
func (f *FirecrackerConfig) Normalize() {
	if f == nil {
		return
	}

	f.BinaryPath = strings.TrimSpace(f.BinaryPath)
	f.JailerPath = strings.TrimSpace(f.JailerPath)
	f.KernelImage = strings.TrimSpace(f.KernelImage)
	f.KernelDir = strings.TrimSpace(f.KernelDir)
	f.ChrootBase = strings.TrimSpace(f.ChrootBase)
	f.Bridge = strings.TrimSpace(f.Bridge)
	f.UntrustedBridge = strings.TrimSpace(f.UntrustedBridge)

	for _, d := range []struct {
		into  *string
		value string
	}{
		{&f.BinaryPath, DefaultFirecrackerBinary},
		{&f.JailerPath, DefaultJailerBinary},
		{&f.KernelDir, DefaultKernelDir},
		{&f.ChrootBase, DefaultChrootBase},
	} {
		if *d.into == "" {
			*d.into = d.value
		}
	}

	if f.JailUIDMin == 0 {
		f.JailUIDMin = DefaultJailUIDMin
	}

	if f.JailUIDCount == 0 {
		f.JailUIDCount = DefaultJailUIDCount
	}

	if f.ImageVerifyPort == 0 {
		f.ImageVerifyPort = DefaultImageVerifyPort
	}
}

// CheckFirecracker refuses a microVM block billet cannot safely act on.
//
// EXPORTED AND CALLED FROM BOTH SIDES, like CheckCeph and CheckEC2Endpoint: the
// provider's constructor is exported, so a rule enforced only in Load has a second
// entry point that does not enforce it.
//
// It takes a VALUE, so a caller cannot hand it a nil pointer and read the empty
// result as approval.
func CheckFirecracker(f FirecrackerConfig) []error {
	var errs []error

	// EVERY PATH IS ABSOLUTE, because a node is a service whose working directory
	// is whatever started it — the same rule node.ceph.conf_path follows. A relative
	// chroot base resolves against `/` under systemd, so a jail an operator built by
	// hand while testing is not the one billet enumerates in production.
	for _, f := range []struct{ field, value string }{
		{"binary_path", f.BinaryPath},
		{"jailer_path", f.JailerPath},
		{"kernel_image", f.KernelImage},
		{"kernel_dir", f.KernelDir},
		{"chroot_base", f.ChrootBase},
	} {
		switch {
		case f.value == "":
			errs = append(errs, fmt.Errorf("node.firecracker.%s is required", f.field))
		case f.value != strings.TrimSpace(f.value):
			errs = append(errs, fmt.Errorf("node.firecracker.%s %q has leading or trailing "+
				"whitespace; billet passes it to the jailer exactly as written", f.field, f.value))
		case !filepath.IsAbs(f.value):
			errs = append(errs, fmt.Errorf("node.firecracker.%s %q is relative; a node runs as a "+
				"service, whose working directory is not where you wrote this file",
				f.field, f.value))
		}
	}

	errs = append(errs, checkBridge("bridge", f.Bridge, true)...)
	errs = append(errs, checkBridge("untrusted_bridge", f.UntrustedBridge, false)...)

	// THE TWO BRIDGES MUST DIFFER, or the setting that admits untrusted work admits
	// it onto the trusted network — which is the one outcome
	// node.firecracker.untrusted_bridge exists to prevent, reached by a
	// configuration that looks like it took the precaution.
	if f.Bridge != "" && f.Bridge == f.UntrustedBridge {
		errs = append(errs, fmt.Errorf("node.firecracker.bridge and "+
			"node.firecracker.untrusted_bridge are both %q: a fork's pull request would run on "+
			"the same network as everything else, which is what naming a separate bridge is for",
			f.Bridge))
	}

	errs = append(errs, checkJailUIDs(f.JailUIDMin, f.JailUIDCount)...)

	if f.ImageVerifyPort < 1 || f.ImageVerifyPort > 65535 {
		errs = append(errs, fmt.Errorf("node.firecracker.image_verify_port is %d; expected 1-65535",
			f.ImageVerifyPort))
	}

	return errs
}

// checkJailUIDs refuses a uid range billet will not run microVMs as.
//
// THE FLOOR IS THE POINT. A range that starts low overlaps accounts that belong to
// somebody — root at 0, the distribution's system accounts, the operator's own login
// — and the jailer drops a VMM to whatever number it is given without asking whether
// a person owns it. Running a guest's VMM as an existing user is strictly worse than
// running it as root would be honest about.
func checkJailUIDs(minUID, count int) []error {
	var errs []error

	if minUID < MinJailUID {
		errs = append(errs, fmt.Errorf("node.firecracker.jail_uid_min is %d, and billet will not "+
			"run a microVM as a uid below %d: the jailer drops each vmm to one of these numbers, "+
			"and below that they belong to root, to the distribution's own accounts, or to a "+
			"person", minUID, MinJailUID))
	}

	if count <= 0 {
		errs = append(errs, fmt.Errorf("node.firecracker.jail_uid_count is %d, so no microVM "+
			"could ever be given a uid", count))
	}

	// A RANGE THAT WRAPS IS NOT A RANGE. These are added together to find the end,
	// and a large enough pair overflows to a negative one — which every "is this uid
	// in range" comparison then passes, including for uid 0.
	if count > 0 && minUID > 0 && minUID+count < minUID {
		errs = append(errs, fmt.Errorf("node.firecracker.jail_uid_min %d plus jail_uid_count %d "+
			"overflows", minUID, count))
	}

	// LINUX UIDS STOP AT 2^32-2, and the kernel refuses (uid_t)-1 outright. Staying
	// inside a signed 32-bit range keeps every number here representable everywhere
	// billet passes it — an argv value to the jailer, an argument to `ip tuntap`,
	// and a Go int on a 32-bit host.
	if end := minUID + count - 1; count > 0 && minUID >= MinJailUID && end > maxJailUID {
		errs = append(errs, fmt.Errorf("node.firecracker.jail_uid_min %d plus jail_uid_count %d "+
			"ends at %d, past the highest uid billet will use (%d)", minUID, count, end, maxJailUID))
	}

	return errs
}

// maxJailUID is the highest uid billet will hand a microVM. Well inside the
// kernel's own limit, and inside a signed 32-bit integer on every host billet
// builds for.
const maxJailUID = 2_000_000

// maxBridgeName is the kernel's IFNAMSIZ limit, less the terminator.
const maxBridgeName = 15

// bridgeName is what the kernel accepts as a network device name.
//
// A SHAPE RATHER THAN A LIST, and narrow because billet builds an `ip link set dev
// <tap> master <bridge>` argv from it. There is no shell, so this is not about
// quoting; it is that a leading dash is read by `ip` as an option, and a name over
// IFNAMSIZ is refused by the kernel with an error that names neither the field nor
// the limit.
var bridgeName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

// checkBridge refuses a bridge name that would not reach the kernel intact.
func checkBridge(field, name string, required bool) []error {
	if name == "" {
		if required {
			return []error{fmt.Errorf("node.firecracker.%s is required: every guest gets one "+
				"network device, and billet attaches it to this bridge", field)}
		}

		return nil
	}

	if name != strings.TrimSpace(name) {
		return []error{fmt.Errorf("node.firecracker.%s %q has leading or trailing whitespace; "+
			"billet passes it to `ip` exactly as written", field, name)}
	}

	if len(name) > maxBridgeName {
		return []error{fmt.Errorf("node.firecracker.%s %q is %d characters; the kernel's limit "+
			"for a network device name is %d", field, name, len(name), maxBridgeName)}
	}

	if !bridgeName.MatchString(name) {
		return []error{fmt.Errorf("node.firecracker.%s %q is not a network device name (expected "+
			"something like br0); billet builds an `ip link` argument from it, where a leading "+
			"dash is read as an option", field, name)}
	}

	// NOT A NAME BILLET ALLOCATES FOR ITSELF. Guest devices are named bt-0, bt-1 and
	// so on, taken lowest-first, so a bridge called bt-3 is a name a launch will
	// eventually try to create — and `ip tuntap add` refuses it because the kernel
	// already has it. That launch fails, hands the name back, and the next one draws
	// the same name and fails identically: the host stops being able to start a
	// microVM, for a reason that names a device rather than this setting.
	if strings.HasPrefix(name, TapPrefix) {
		return []error{fmt.Errorf("node.firecracker.%s %q starts with %q, which is how billet "+
			"names the device it creates for each guest; a launch would eventually try to create "+
			"this exact name and be refused, and every launch after it the same way. Rename the "+
			"bridge", field, name, TapPrefix)}
	}

	return nil
}

// TapPrefix is how the firecracker backend names the network device it creates for
// each guest.
//
// DECLARED HERE AND USED THERE, rather than the other way round, because config may
// not import a provider — and a second copy in this file would be a constant that can
// drift from the thing it is validating against, which is the whole failure this
// check exists to prevent.
const TapPrefix = "bt-"

// CephConfig points this host at the Ceph cluster its site keeps.
//
// STORAGE BELONGS TO THE SITE, NOT TO THE COMPUTE BACKEND, which is why this is a
// sibling of the firecracker block rather than a field inside it. Golden images,
// per-job root clones and every cache in the plane are RBD images in one cluster,
// and the code that mounts a cache is not the code that boots a microVM. What
// lives here is the half a HOST holds: where the monitors are, who this machine
// authenticates as, and which pools that identity may use.
//
// It replaced a `zfs_pool` key. A ZFS clone exists only on the machine that took
// it, so a cache written to one pinned every repository to the host that first
// built it — the storage half of billet being a one-machine product. RBD presents
// the same NVMe as a pool any node at the site can map.
type CephConfig struct {
	// ConfPath is the ceph.conf naming this cluster's monitors. Empty means Ceph's
	// own search path, which finds /etc/ceph/ceph.conf.
	ConfPath string `yaml:"conf_path,omitempty"`

	// User is the RADOS identity billet authenticates as, WITHOUT the `client.`
	// prefix.
	//
	// DEFAULTS TO billet RATHER THAN admin, which is what the rbd command picks on
	// its own. An admin key can delete a pool, so defaulting to it would make a
	// compromised node able to destroy the cluster it caches in rather than the
	// images it was handed — and it would do so silently, because everything works.
	User string `yaml:"user,omitempty"`

	// KeyringPath holds that identity's secret. Empty means Ceph's own search path,
	// which finds /etc/ceph/ceph.<user>.keyring.
	KeyringPath string `yaml:"keyring_path,omitempty"`

	// ImagePool holds golden images and the per-job root clones taken from them.
	ImagePool string `yaml:"image_pool"`

	// CachePool holds cache volumes: sticky disks, build state, the layer cache.
	//
	// SEPARATE FROM ImagePool, and validation refuses one name for both. Not for
	// tuning — RBD's object size is a per-IMAGE property, so the reason ZFS wanted
	// two datasets does not survive the move. It is about blast radius: a cache is
	// disposable and a golden image is not, so "throw the cache away" has to be
	// something an operator can do to a whole pool without taking the images with
	// it, and a garbage collector walking one pool must not be able to reach the
	// other.
	CachePool string `yaml:"cache_pool"`
}

// EBSS3Config points a cloud node at its site's EBS and S3 cache storage.
type EBSS3Config struct {
	// Region is the signing region for both services and must match node.ec2.
	Region string `yaml:"region"`
	// AvailabilityZone is where cache volumes are created. EBS volumes and the
	// instances consuming them must be in the same zone.
	AvailabilityZone string `yaml:"availability_zone"`
	// Bucket holds the atomic pointer, lease and fencing state objects.
	Bucket string `yaml:"bucket"`
	// Prefix isolates one deployment and site inside a bucket.
	Prefix string `yaml:"prefix,omitempty"`
	// KMSKeyID optionally selects one customer-managed key for EBS volumes and
	// snapshots. Empty uses the account's EBS encryption default key.
	//
	// A PER-DEPLOYMENT key is what closes the cross-deployment READ boundary:
	// the IAM tag conditions cannot stop another deployment cloning a snapshot
	// and reading the cache (ec2:CreateVolume does not authorize the parent
	// snapshot; the value-scoped conditions give destructive integrity only).
	// With each deployment's snapshots encrypted under its own key — whose key
	// policy delegates to IAM and admits no foreign role — and its role's KMS
	// grants scoped to exactly that key, as `billet init iam` and the terraform
	// module both do, the foreign clone fails at the KMS grant instead
	// (measured with iam:SimulateCustomPolicy: every KMS action on another
	// deployment's key is implicitly denied). Sharing one key between
	// deployments silently reopens that boundary — as does a key whose own
	// policy or grants admit other roles, which no identity policy can see.
	// Leaving this EMPTY encrypts under the ACCOUNT's default EBS key — the
	// AWS-managed aws/ebs unless the account configured another — and aws/ebs
	// authorizes any principal in the account through EC2, so an opted-out
	// deployment's snapshots stay readable no matter what keys its neighbours
	// set. A key protects only snapshots created after it was set; evict or
	// re-snapshot older generations to bring them under it.
	KMSKeyID string `yaml:"kms_key_id,omitempty"`
}

func (e *EBSS3Config) normalize() {
	if e == nil {
		return
	}

	e.Region = strings.TrimSpace(e.Region)
	e.AvailabilityZone = strings.TrimSpace(e.AvailabilityZone)
	e.Bucket = strings.TrimSpace(e.Bucket)
	e.Prefix = strings.TrimSpace(e.Prefix)
	e.KMSKeyID = strings.TrimSpace(e.KMSKeyID)
	if e.Prefix == "" {
		e.Prefix = "billet-cache"
	}
}

// EC2Config configures the cloud backend: one instance per job, in one subnet.
//
// EVERY FIELD HERE IS A PLACEMENT DECISION SOMEBODY HAS TO MAKE, and none of the
// load-bearing ones is defaulted. billet cannot pick a subnet, and a wrong guess
// is either a job that cannot reach GitHub or one that can reach a production
// database.
type EC2Config struct {
	// Region is which AWS region to launch in. It also selects the API endpoint,
	// so an ordinary install configures no host of its own.
	Region string `yaml:"region"`
	// Endpoint overrides the API endpoint billet derives from Region — for a VPC
	// interface endpoint, a non-commercial partition, or a test.
	Endpoint string `yaml:"endpoint,omitempty"`

	// SubnetID is where instances are launched. Its route to GitHub is the
	// operator's to arrange: a private subnet needs a NAT gateway, a public one
	// needs AssignPublicIP.
	SubnetID string `yaml:"subnet_id"`
	// SecurityGroupIDs apply to trusted work.
	SecurityGroupIDs []string `yaml:"security_group_ids"`
	// UntrustedSecurityGroupIDs apply to fork pull-request work, and their
	// ABSENCE is what refuses it.
	//
	// A whole instance is a real isolation boundary, which is why this backend can
	// run code billet cannot vouch for at all — but that boundary is the KERNEL,
	// not the network. A fork's job in the same security group as everything else
	// reaches whatever that group reaches, which on a subnet somebody already had
	// is usually more than they are picturing. So untrusted work runs only once
	// its network has been described separately, rather than defaulting onto the
	// trusted group because nobody said otherwise.
	UntrustedSecurityGroupIDs []string `yaml:"untrusted_security_group_ids,omitempty"`
	// AssignPublicIP gives instances a public address, for a subnet with no NAT
	// gateway. A runner that cannot reach GitHub registers and then does nothing.
	AssignPublicIP bool `yaml:"assign_public_ip,omitempty"`

	// InstanceProfile is the IAM role TRUSTED instances receive. OPTIONAL, and
	// empty is the right answer unless a job genuinely needs AWS credentials: an
	// instance profile is readable from inside the guest, so it is a credential
	// handed to whatever the job runs.
	//
	// UNTRUSTED WORK NEVER GETS IT, whatever this says. A fork's pull request runs
	// its steps directly on the instance, so it could read the role's temporary
	// credentials out of the metadata service — past the isolation that lets this
	// backend run untrusted work at all.
	InstanceProfile string `yaml:"instance_profile,omitempty"`

	// InstanceTypes are the shapes billet may buy, each DECLARING what it holds,
	// because billet ships no table of EC2 instance types.
	//
	// A table would be out of date within a quarter — AWS adds types continuously
	// — and being out of date here means launching a machine that does not fit a
	// lease the allocator has already escrowed. Declaring them keeps the fleet's
	// cost surface in the operator's own file, which is where a spending decision
	// belongs anyway.
	InstanceTypes []EC2InstanceType `yaml:"instance_types"`

	// Spot buys interruptible capacity.
	//
	// DEFAULTS OFF, which reverses the assumption this backend was filed under.
	// It exists so one `runs-on` label survives the bare-metal host going away,
	// and GitHub does not requeue a job whose runner vanished mid-execution — so a
	// spot reclaim is a FAILED BUILD rather than a retry. Defaulting to spot would
	// make the failover path the unreliable one, which is the opposite of what a
	// failover is for. An operator who would rather have a cheap build that
	// sometimes dies says so here.
	Spot bool `yaml:"spot,omitempty"`
	// InterruptionQueueURL receives EventBridge's EC2 Spot interruption warnings.
	// Required with Spot: without it a reclaim is an unexplained failed build.
	InterruptionQueueURL string `yaml:"interruption_queue_url,omitempty"`
	// NodeName is filled from the effective node identity before the provider is
	// constructed. It is not a second operator-configured identity.
	NodeName string `yaml:"-"`
}

// RemoteShape is what a remote backend's ordered shape list holds, whatever that
// backend calls its shapes — an EC2 instance type, a CodeBuild compute type.
//
// AN ALIAS RATHER THAN A DEFINED TYPE, and that is the whole point: a defined type
// would inherit none of the methods and none of the validation, so there would be
// two shape catalogues to keep in step and two validators to keep in agreement.
// The ledger column, the wire field and the validator are all one thing; this name
// exists so a codebuild config field does not have to be spelled `EC2InstanceType`
// and read as a copy-paste mistake.
type RemoteShape = EC2InstanceType

// EC2InstanceType is one shape billet may buy, and what it holds.
//
// The vCPU and memory are DECLARED rather than looked up, because the allocator
// has already escrowed a size against this node before any of this is consulted:
// a shape that turns out smaller than the lease it was chosen for over-commits a
// machine nobody can see.
//
// It carries the ORIGINAL name because the ledger column, the registration field
// and every existing config say `ec2`, and renaming a shipped on-disk spelling to
// tidy a Go identifier is a flag day for nothing. `RemoteShape` above is the alias
// a second remote backend uses.
type EC2InstanceType struct {
	Type   string   `yaml:"type" json:"type"`
	VCPU   int      `yaml:"vcpu" json:"vcpu"`
	Memory ByteSize `yaml:"memory" json:"memory"`
	// PriceUSDPerHour is the operator-audited compute rate used to report the
	// maximum configured exposure. It is required because the answer has to be in
	// the config, not fetched from a mutable service when a job arrives. It is not
	// an admission gate and cannot make an already-accepted job wait because a
	// copied price went stale.
	PriceUSDPerHour USDPerHour `yaml:"price_usd_per_hour" json:"price_usd_per_hour"`
}

// CodeBuildEnvironment is a CodeBuild environment type billet is willing to run a
// job in.
//
// A CLOSED SET DRAWN FROM WHAT StartBuild ACCEPTS, minus the ones that cannot run
// a GitHub Actions job. It is not the full enum on purpose — see
// checkCodeBuildEnvironment for what each exclusion costs.
type CodeBuildEnvironment string

const (
	// CodeBuildLinuxContainer is the ordinary x86-64 container environment.
	// Docker inside the job needs privileged_mode.
	CodeBuildLinuxContainer CodeBuildEnvironment = "LINUX_CONTAINER"
	// CodeBuildARMContainer is the arm64 container environment.
	CodeBuildARMContainer CodeBuildEnvironment = "ARM_CONTAINER"
	// CodeBuildLinuxGPUContainer is the GPU container environment. Its default
	// account quota is ZERO, so a tier on it advertises capacity nothing can run
	// until the quota is raised.
	CodeBuildLinuxGPUContainer CodeBuildEnvironment = "LINUX_GPU_CONTAINER"
	// CodeBuildLinuxEC2 runs directly on an EC2 instance rather than in a
	// container, so Docker works without privileged_mode. Reserved capacity only.
	CodeBuildLinuxEC2 CodeBuildEnvironment = "LINUX_EC2"
	// CodeBuildARMEC2 is the arm64 form of the same.
	CodeBuildARMEC2 CodeBuildEnvironment = "ARM_EC2"
	// CodeBuildMacARM is AWS-managed Apple silicon, and the whole reason this
	// backend reaches macOS at all. RESERVED CAPACITY ONLY — on-demand fleets do
	// not offer macOS — so it requires fleet_arn.
	CodeBuildMacARM CodeBuildEnvironment = "MAC_ARM"
)

var codeBuildEnvironments = []CodeBuildEnvironment{
	CodeBuildLinuxContainer, CodeBuildARMContainer, CodeBuildLinuxGPUContainer,
	CodeBuildLinuxEC2, CodeBuildARMEC2, CodeBuildMacARM,
}

// Valid reports whether this is an environment type billet runs jobs in.
func (e CodeBuildEnvironment) Valid() bool {
	return slices.Contains(codeBuildEnvironments, e)
}

// GuestOS is what a build in this environment boots.
//
// DERIVED RATHER THAN CONFIGURED, because the two would then be two authorities
// for one fact: an operator who wrote `environment_type: MAC_ARM` beside
// `guest_os: [linux]` would have a node that advertises Linux and starts macOS
// builds. What a node REPORTS at registration comes from here, and placement's
// durable check (alloc.Bind) is what a node cannot route around.
func (e CodeBuildEnvironment) GuestOS() GuestOS {
	if e == CodeBuildMacARM {
		return GuestMacOS
	}

	return GuestLinux
}

// Container reports whether this environment runs the job inside a container, in
// which case Docker inside the job needs privileged mode. An EC2 or macOS
// environment IS the machine, so there is nothing to privilege.
func (e CodeBuildEnvironment) Container() bool {
	switch e {
	case CodeBuildLinuxContainer, CodeBuildARMContainer, CodeBuildLinuxGPUContainer:
		return true
	case CodeBuildLinuxEC2, CodeBuildARMEC2, CodeBuildMacARM:
		return false
	default:
		return false
	}
}

// ReservedOnly reports whether this environment exists only on a reserved-capacity
// fleet, so a config naming it without a fleet describes builds AWS will refuse.
func (e CodeBuildEnvironment) ReservedOnly() bool {
	switch e {
	case CodeBuildMacARM, CodeBuildLinuxEC2, CodeBuildARMEC2:
		return true
	case CodeBuildLinuxContainer, CodeBuildARMContainer, CodeBuildLinuxGPUContainer:
		return false
	default:
		return false
	}
}

// CodeBuildBuildCeilingMinutes is CodeBuild's own maximum build timeout, and it is
// not billet's to raise. Every CodeBuild-backed job inherits it.
const CodeBuildBuildCeilingMinutes = 2160

// CodeBuildBuildFloorMinutes is CodeBuild's minimum build timeout.
const CodeBuildBuildFloorMinutes = 5

// CodeBuildQueuedCeilingMinutes is how long CodeBuild will hold a QUEUED build
// before failing it. A SECOND external ceiling, and the one an operator does not
// expect: on a fleet at capacity it is what kills a job that never got a machine.
const CodeBuildQueuedCeilingMinutes = 480

// CodeBuildQueuedFloorMinutes is CodeBuild's minimum queued timeout.
const CodeBuildQueuedFloorMinutes = 5

// CodeBuildAccountQueuedBuilds is how many builds CodeBuild will hold in the queue
// for a WHOLE ACCOUNT before refusing StartBuild outright. A THIRD external ceiling,
// measured on 2026-09-02 rather than read: the thirty-first queued build is refused
// with `AccountLimitExceededException: Cannot have more than 30 builds in queue for
// the account`, and Service Quotas lists no quota to raise it. It is shared by every
// project in the account, so it bounds what a deployment may escrow against CodeBuild:
// a burst beyond the concurrency quota plus this many is refused at launch, which
// billet reports as a conclusive failure and GitHub requeues at most three times.
const CodeBuildAccountQueuedBuilds = 30

// CodeBuildConfig configures the AWS CodeBuild backend: one build per job, in one
// project.
//
// LIKE node.ec2, EVERY LOAD-BEARING FIELD IS A DECISION SOMEBODY HAS TO MAKE and
// none of them is defaulted. billet cannot pick a project, and it will not pick a
// compute type on somebody's account.
//
// UNLIKE node.ec2, ONE FIELD EXISTS PURELY SO A LIMIT CANNOT BE A SURPRISE.
// CodeBuild caps a build at 36 hours and fails a queued build after at most 8, and
// billet can lift neither — so accept_external_build_ceiling has no default and its
// absence is a refusal. The alternative is an operator meeting the ceiling for the
// first time as a 36-hour build that died, on a backend documented as the fallback
// that keeps CI working.
type CodeBuildConfig struct {
	// Region is which AWS region to build in. It also selects the API endpoint,
	// so an ordinary install configures no host of its own.
	Region string `yaml:"region"`
	// Endpoint overrides the API endpoint billet derives from Region — for a VPC
	// interface endpoint, a non-commercial partition, or a test.
	Endpoint string `yaml:"endpoint,omitempty"`

	// Project is the CodeBuild project billet starts builds in, and it must be
	// DEDICATED to this deployment and this node.
	//
	// THE PROJECT IS HALF THE OWNERSHIP BOUNDARY, because a CodeBuild build cannot
	// be tagged: `StartBuild` has no field that becomes one, so the per-instance
	// `sh.billet.owner` tag the ec2 backend filters `List` on does not exist here.
	// What replaces it is this project plus per-build markers read back through
	// BatchGetBuilds — and `List` feeds a loop that STOPS builds, so a project
	// shared with an ordinary CodeBuild workload is a way for billet to stop
	// somebody else's build.
	Project string `yaml:"project"`

	// FleetARN selects a reserved-capacity fleet. Empty means on-demand compute.
	//
	// ITS PRESENCE IS ALSO A STATEMENT ABOUT ISOLATION, which is why the provider
	// reads it rather than only passing it along: AWS documents a reserved
	// instance as remaining alive between builds and as sharing cached data with
	// other projects in the account, by design. macOS is reserved-only, so every
	// macOS build inherits that.
	//
	// A fleetOverride ALSO discards the project's VPC configuration — the fleet's
	// own network governs — so a network reviewed on the project proves nothing
	// about a build that named a fleet.
	FleetARN string `yaml:"fleet_arn,omitempty"`

	// EnvironmentType is the CodeBuild environment builds run in, and it is what
	// this node's guest OS is DERIVED from.
	EnvironmentType CodeBuildEnvironment `yaml:"environment_type"`

	// ComputeTypes are the compute types billet may buy, each DECLARING what it
	// holds, for the same reason node.ec2.instance_types are declared: billet
	// ships no table of them, and being out of date here means starting a build
	// smaller than the lease the allocator already escrowed.
	//
	// ORDERED, most preferred first, and placement charges the first entry that
	// fits rather than the smaller tier request.
	ComputeTypes []RemoteShape `yaml:"compute_types"`

	// AcceptExternalBuildCeiling acknowledges that every job on this node inherits
	// CodeBuild's ceilings, which billet cannot lift.
	//
	// NO DEFAULT, AND ITS ABSENCE IS THE REFUSAL — the same shape as
	// node.firecracker.untrusted_bridge and node.ec2.untrusted_security_group_ids.
	// It is not a feature flag: nothing changes when it is set. It exists so that
	// the sentence "this tier cannot run a job longer than 36 hours" is read by a
	// person before a tier advertises capacity, rather than discovered from a build
	// that died at hour 36 with GitHub reporting a failed job.
	AcceptExternalBuildCeiling bool `yaml:"accept_external_build_ceiling"`

	// BuildTimeoutMinutes is the ceiling billet asks CodeBuild for, 5 to 2160.
	//
	// THE OPERATOR'S NUMBER, NOT BILLET'S. billet adds no deadline of its own and
	// no drain or upgrade ever stops a build for taking too long; this is passed
	// through as timeoutInMinutesOverride. It defaults to the maximum, so a config
	// that says nothing gets the longest job CodeBuild permits.
	//
	// It also SIZES THE INVENTORY WINDOW, which is the one non-obvious consequence.
	// CodeBuild cannot list only active builds, so `List` walks recent history and
	// stops once every build it sees is older than this plus the queued ceiling —
	// at which point CodeBuild has necessarily ended them. Declaring a tighter
	// ceiling therefore makes this node's inventory cheaper; declaring the maximum
	// is supported and costs a longer walk.
	BuildTimeoutMinutes int `yaml:"build_timeout_minutes,omitempty"`

	// QueuedTimeoutMinutes is how long a build may wait for capacity, 5 to 480,
	// after which CodeBuild FAILS it. Defaults to the maximum.
	QueuedTimeoutMinutes int `yaml:"queued_timeout_minutes,omitempty"`

	// JITParameterPath is the SSM Parameter Store path prefix billet writes each
	// build's single-use runner registration under.
	//
	// REQUIRED, because it is an IAM boundary rather than a naming preference: the
	// node's policy grants ssm:PutParameter and ssm:DeleteParameter on exactly this
	// path, so a value billet guessed would either be unwritable or — worse — wider
	// than the grant an operator reviewed.
	//
	// THE REGISTRATION GOES HERE RATHER THAN INTO THE LAUNCH REQUEST because every
	// StartBuild field is rendered in the console and in CloudTrail. What travels in
	// the request is the parameter's NAME; the value is resolved into the build by
	// CodeBuild itself, under the BUILD's service role, which is a different
	// principal from this node's.
	JITParameterPath string `yaml:"jit_parameter_path"`

	// JITKMSKeyID selects the customer-managed key SecureString parameters are
	// encrypted with. Empty uses the account's aws/ssm key.
	JITKMSKeyID string `yaml:"jit_kms_key_id,omitempty"`

	// LogGroup pins where a build's logs go. Empty leaves the project's own
	// configuration alone.
	LogGroup string `yaml:"log_group,omitempty"`

	// PrivilegedMode grants the build the privilege Docker needs, and is only
	// meaningful for a CONTAINER environment — an EC2 or macOS environment is the
	// machine, so there is nothing to privilege and setting it is refused rather
	// than ignored.
	//
	// A GitHub Actions job routinely runs `docker build` and service containers, so
	// a container-environment tier that leaves this off produces jobs that fail on
	// their first Docker step. It is not defaulted on, because it is a real
	// privilege grant and billet does not hand one out on somebody's behalf.
	PrivilegedMode bool `yaml:"privileged_mode,omitempty"`

	// UntrustedVPCID, UntrustedSubnetIDs and UntrustedSecurityGroupIDs name the
	// isolated network a fork pull-request build runs in, and THEIR ABSENCE IS THE
	// REFUSAL — the same shape as node.ec2.untrusted_security_group_ids and
	// node.firecracker.untrusted_bridge. A build container isolates the kernel, not
	// the network, and a subnet somebody already had usually reaches more than they
	// are picturing.
	//
	// UNLIKE ec2, THE NETWORK LIVES ON THE PROJECT, because StartBuild has no VPC
	// override — CodeBuild sets a build's network from the project's vpcConfig (and a
	// fleetOverride discards even that). So these three fields are what the project
	// billet launches into was created with, and the provider VERIFIES the project
	// carries exactly this network before it starts an untrusted build. Declaring
	// them is only meaningful on an on-demand container node: reserved capacity is
	// shared between builds and macOS is reserved-only, so an untrusted build is
	// refused there whatever the network says, and declaring a network beside
	// fleet_arn or a reserved environment_type is refused as dead config.
	UntrustedVPCID            string   `yaml:"untrusted_vpc_id,omitempty"`
	UntrustedSubnetIDs        []string `yaml:"untrusted_subnets,omitempty"`
	UntrustedSecurityGroupIDs []string `yaml:"untrusted_security_group_ids,omitempty"`
}

// HasUntrustedNetwork reports whether all three untrusted-network fields are set,
// which is the only shape that admits untrusted work. A PARTIAL set is not "half
// configured" — it is a config error CheckCodeBuild refuses, the same rule as
// ec2's use_vpc needing both a subnet and a group.
func (b *CodeBuildConfig) HasUntrustedNetwork() bool {
	return b.UntrustedVPCID != "" && len(b.UntrustedSubnetIDs) > 0 &&
		len(b.UntrustedSecurityGroupIDs) > 0
}

// Prepare normalizes and defaults a CodeBuild block, and it is the ONE place that
// does either.
//
// EXPORTED AND CALLED FROM BOTH SIDES, which is the rule CheckCodeBuild already
// follows and for a sharper reason. Load prepares before it validates; the
// provider's exported constructor cannot assume its configuration came through
// Load, and the first version reproduced only part of this by hand — it trimmed
// the scalars and missed the compute-type NAMES, which validation checks trimmed
// and the launch sends raw, and the timeout DEFAULTS, without which a caller that
// omitted them passed validation (zero means "not stated") and sent a zero
// override AWS refuses. Two of the shapes this repository keeps finding, in one
// function: a check that examines a copy the consumer does not use, and a default
// applied on one of two entry points.
//
// IT MUST RUN BEFORE VALIDATION on both paths, because the defaults it fills in
// are what the range checks then judge.
func (b *CodeBuildConfig) Prepare() {
	b.normalize()
	b.applyDefaults()
}

// normalize trims the values billet later uses verbatim, for the reason
// EC2Config.normalize gives: validation used to trim a copy while the consumer
// used the raw string, so a padded region passed the shape check and was then
// signed with its padding.
func (b *CodeBuildConfig) normalize() {
	if b == nil {
		return
	}

	b.Region = strings.TrimSpace(b.Region)
	b.Endpoint = strings.TrimSpace(b.Endpoint)
	b.Project = strings.TrimSpace(b.Project)
	b.FleetARN = strings.TrimSpace(b.FleetARN)
	b.JITParameterPath = strings.TrimSpace(b.JITParameterPath)
	b.JITKMSKeyID = strings.TrimSpace(b.JITKMSKeyID)
	b.LogGroup = strings.TrimSpace(b.LogGroup)
	b.EnvironmentType = CodeBuildEnvironment(strings.TrimSpace(string(b.EnvironmentType)))

	// THE SHAPE NAMES TOO, and this is the half the provider's constructor was
	// missing: a padded compute type passed validation, which checks a trimmed
	// copy, and was then sent to AWS with its padding — refused as an unknown
	// compute type, naming nothing.
	for i := range b.ComputeTypes {
		b.ComputeTypes[i].Type = strings.TrimSpace(b.ComputeTypes[i].Type)
	}

	// THE UNTRUSTED NETWORK IDS ARE VERIFIED AGAINST THE PROJECT'S OWN vpcConfig at
	// launch, so a padded one would compare unequal to a value AWS reports trimmed
	// and refuse every untrusted launch — the same reason the region and the shape
	// names are trimmed here.
	b.UntrustedVPCID = strings.TrimSpace(b.UntrustedVPCID)
	for i := range b.UntrustedSubnetIDs {
		b.UntrustedSubnetIDs[i] = strings.TrimSpace(b.UntrustedSubnetIDs[i])
	}
	for i := range b.UntrustedSecurityGroupIDs {
		b.UntrustedSecurityGroupIDs[i] = strings.TrimSpace(b.UntrustedSecurityGroupIDs[i])
	}
}

// applyDefaults fills in the two ceilings, which default to CodeBuild's own
// maxima so a config that says nothing gets the longest job the service permits
// rather than a shorter one billet chose.
func (b *CodeBuildConfig) applyDefaults() {
	if b == nil {
		return
	}

	if b.BuildTimeoutMinutes == 0 {
		b.BuildTimeoutMinutes = CodeBuildBuildCeilingMinutes
	}

	if b.QueuedTimeoutMinutes == 0 {
		b.QueuedTimeoutMinutes = CodeBuildQueuedCeilingMinutes
	}
}

// LogGroupName is the CloudWatch group this node's builds write to.
//
// DERIVED WHEN NOTHING NAMES ONE, and there is exactly one right answer to derive:
// CodeBuild's own default group for a project is /aws/codebuild/<project>. Both the
// launch path and the IAM renderer need this, and they must not disagree — the build
// role's grant is scoped to a group ARN, so a policy naming one group while the build
// writes to another is a role that cannot write its own logs.
//
// EXPORTED FROM config FOR THE SAME REASON CheckCodeBuild IS: two derivations of one
// value is one derivation that is wrong, and config is the leaf both sides already read.
func (b *CodeBuildConfig) LogGroupName() string {
	if b == nil {
		return ""
	}

	if b.LogGroup != "" {
		return b.LogGroup
	}

	return "/aws/codebuild/" + b.Project
}

// InventoryWindowMinutes is how far back `List` and `Find` must look before an
// absence is conclusive.
//
// DERIVED FROM THE DECLARED CEILINGS RATHER THAN CHOSEN, because it is the only
// bound available: CodeBuild offers no way to list active builds and retains a
// year of history, so what makes the walk finite is that the service itself ends
// a build once these two elapse. A build older than their sum cannot still be
// running.
//
// The slack is deliberate and generous. It covers the gap between billet asking
// and CodeBuild acting, and getting it wrong in the short direction means reading
// a RUNNING build as absent — which frees capacity for compute that is still
// executing somebody's job. Getting it wrong long costs one extra page.
func (b *CodeBuildConfig) InventoryWindowMinutes() int {
	if b == nil {
		return CodeBuildBuildCeilingMinutes + CodeBuildQueuedCeilingMinutes + codeBuildWindowSlackMinutes
	}

	build, queued := b.BuildTimeoutMinutes, b.QueuedTimeoutMinutes
	if build <= 0 {
		build = CodeBuildBuildCeilingMinutes
	}

	if queued <= 0 {
		queued = CodeBuildQueuedCeilingMinutes
	}

	return build + queued + codeBuildWindowSlackMinutes
}

// codeBuildWindowSlackMinutes is the margin added to the declared ceilings. An
// hour, because the cost of being too generous is one more page of history and the
// cost of being too tight is a running build read as gone.
const codeBuildWindowSlackMinutes = 60

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

// BackupConfig is where this deployment's archives go when they leave the disk
// they protect.
//
// OPTIONAL, AND THE DIRECTORY REMAINS THE CONTRACT. `billet local backup --out
// <dir>` writes a manifest of digests and sizes precisely so that somebody
// else's tooling can carry it; an operator who already has restic, rclone or a
// NAS needs nothing here. What this adds is the half that matters on the day the
// machine is new: `billet local restore --from s3://…` fetches, verifies and
// restores in one command, where an upload-only answer would have them
// installing another tool mid-outage.
//
// WHAT BILLET DELIBERATELY DOES NOT BECOME is a backup tool. There is no dedupe,
// no incremental, no catalogue and no retention: the bucket does retention
// (versioning and a lifecycle rule), the manifest does verification, and billet
// never issues a delete — so the credential sitting on the one host that holds
// the App key cannot destroy the history it just wrote.
type BackupConfig struct {
	// S3 is an S3-compatible bucket. Nil means billet uploads nothing and the
	// archive directory is the seam.
	S3 *BackupS3Config `yaml:"s3,omitempty"`
}

// BackupS3Config names the bucket and how to reach it.
type BackupS3Config struct {
	// Bucket receives one object per archive entry.
	Bucket string `yaml:"bucket"`
	// Region is the SIGNING region. With no Endpoint it also selects the AWS
	// endpoint, so a typo there is a request signed for somewhere else.
	Region string `yaml:"region"`
	// Prefix isolates one deployment inside a shared bucket. Archives land under
	// <prefix>/<deployment-id>/<created-at>/, so IAM can be scoped by prefix and
	// two deployments cannot read each other's credentials.
	Prefix string `yaml:"prefix,omitempty"`
	// Endpoint overrides the AWS endpoint billet derives from Region, for an
	// S3-compatible store: Ceph RGW — which billet's own reference hardware
	// already runs — MinIO, or R2. Addressing is PATH style against it
	// (<endpoint>/<bucket>/<key>), because virtual-host style is what MinIO does
	// not do by default.
	Endpoint string `yaml:"endpoint,omitempty"`
	// KMSKeyID selects a customer-managed key for server-side encryption. Empty
	// uses SSE-S3 (AES256), which every S3-compatible store supports.
	//
	// AN ARCHIVE IS TWO PRIVATE KEYS AND A LEDGER, so what encrypts it at rest is
	// a real decision rather than a detail — and the one thing billet can enforce
	// from here is that it asks for encryption at all.
	KMSKeyID string `yaml:"kms_key_id,omitempty"`
}

func (b *BackupS3Config) normalize() {
	if b == nil {
		return
	}

	b.Bucket = strings.TrimSpace(b.Bucket)
	b.Region = strings.TrimSpace(b.Region)
	b.Prefix = strings.TrimSpace(b.Prefix)
	b.Endpoint = strings.TrimSpace(b.Endpoint)
	b.KMSKeyID = strings.TrimSpace(b.KMSKeyID)

	if b.Prefix == "" {
		b.Prefix = "billet-backups"
	}
}

// Tier is one runner shape. Its Label is what appears in `runs-on`.
type Tier struct {
	Label string `yaml:"label"`

	// Trust is the authority every member of this runner pool receives before
	// GitHub assigns it a job. It is explicit because scale-set JIT runners are
	// pool members, not registrations bound to the assignment that caused Billet
	// to create them.
	Trust WorkloadTrust `yaml:"trust"`
	// Workflows is the exact GitHub runner-group workflow allowlist a trusted
	// pool expects. An untrusted pool needs no routing claim for its safety.
	Workflows []string `yaml:"workflows,omitempty"`
	// CacheScope is the one immutable Actions cache identity placed in a guest at
	// launch. It is required for interception because JobStarted arrives after
	// the guest already has its launch-time credentials.
	CacheScope *CacheScope `yaml:"cache_scope,omitempty"`

	// Provider is the single backend this tier runs on. Kept because it is what
	// almost every deployment wants and what every existing config says; it is
	// normalized into Providers, which is what the rest of billet reads.
	Provider ProviderKind `yaml:"provider,omitempty"`

	// Providers is an ORDERED preference list, most preferred first.
	//
	// The reason one `runs-on` label can span a machine at home and a cloud: a tier
	// listing `[firecracker, ec2]` may be placed on either, so losing the bare-metal
	// host does not take the label down with it.
	//
	// THE ORDER DECIDES, AT ESCROW — the allocator walks it most-preferred-first over
	// the hosts that can serve the tier and have room, so a job reaches the cloud only
	// when home is full, rather than when a cloud node polls first. server.placement
	// decides only between hosts this order cannot separate.
	//
	// Setting both this and Provider is an error rather than a merge: guessing which
	// spelling an operator meant, when the answer decides where untrusted code runs,
	// is not a kindness. Every backend in allProviders is built and may appear here —
	// stated without a count, because both sides of this merge had independently
	// corrected the previous wording and one of them said "four" for a list of five.
	Providers []ProviderKind `yaml:"providers,omitempty"`
	// GuestOS defaults to linux. Set it explicitly for macOS and Windows tiers —
	// licensing and capability checks key off this field, not off the label.
	GuestOS GuestOS `yaml:"guest_os,omitempty"`
	// Node optionally pins this tier to a named node. Required when only one
	// node can serve it — macOS tiers, for example.
	Node string `yaml:"node,omitempty"`

	// Site optionally confines this tier to one place, the way Node confines it to one
	// machine — but a site holds several machines, so it constrains without giving up
	// the fallback that having several of them buys.
	//
	// The reason to reach for it is data rather than hardware: a job that must not
	// leave a location, or one whose cache exists in only one place.
	Site string `yaml:"site,omitempty"`
	// RunnerGroup is the GitHub runner group this tier's scale set belongs to. Empty
	// means GitHub's "default" group.
	//
	// Access control rather than scheduling: a runner group is how an organization
	// decides which repositories may use these runners, and putting every tier in the
	// default group hands them to every repository in the org.
	RunnerGroup string `yaml:"runner_group,omitempty"`

	// Command starts the runner inside this tier's image. Empty uses the provider's
	// packaged runner service.
	//
	// Expressible because a container image's default command is a shell: a backend
	// that launches one gets a container that exits immediately while every signal
	// reports success, so the command cannot be left to the image.
	Command []string `yaml:"command,omitempty"`

	// Launch holds backend-specific boot details for a tier that accepts more than
	// one provider. A Firecracker generation and an EC2 AMI are both images, but
	// neither backend can interpret the other's name; commands can differ for the
	// same reason. Single-provider tiers keep the simpler image and command fields.
	Launch map[ProviderKind]TierLaunch `yaml:"launch,omitempty"`

	// NOTE: the scale-set client interpolates this name into a query string WITHOUT
	// escaping it, so an ordinary group name like "Platform & Security" is parsed as
	// two parameters and comes back as "group not found". Validation rejects what the
	// client cannot carry rather than letting GitHub answer confusingly.

	VCPU   int      `yaml:"vcpu"`
	Memory ByteSize `yaml:"memory"`

	// Sizes expands this entry into one tier per vCPU count, most operators'
	// single largest source of hand-written YAML.
	//
	// A real deployment wants several sizes of the same thing and each one is
	// fifteen lines that differ in two numbers and a label — so `sizes: [2, 4, 8]`
	// against everything else this entry says writes the other thirteen. The
	// expansion happens in Parse, before defaults and before validation, so
	// nothing downstream ever sees an unexpanded tier.
	//
	// IT IS A TEMPLATE, NOT A RANGE. Each size becomes a real, separate tier with
	// its own label, its own scale set and its own escrow — because that is what
	// it already was when it was written out by hand, and a shorthand that meant
	// anything else would be a new scheduling concept wearing a convenience's
	// clothes.
	//
	// Refused beside an explicit vcpu or memory: two spellings of one value is a
	// mistake internal/config has already made three times, and it is silent
	// every time.
	Sizes []int `yaml:"sizes,omitempty"`
	// MemoryPerVCPU is the proportion `sizes` shapes each tier to. It is
	// meaningless without Sizes and refused there.
	//
	// The default is DefaultMemoryPerVCPU, which is the same 4GiB every generated
	// catalogue already uses — so a config that writes `sizes` and nothing else
	// gets exactly the ladder `billet init` would have written for it.
	MemoryPerVCPU ByteSize `yaml:"memory_per_vcpu,omitempty"`
	Disk          ByteSize `yaml:"disk,omitempty"`
	// SHM sizes /dev/shm. Chromium and Postgres both misbehave on the default,
	// so this is a tier knob rather than an image constant.
	SHM ByteSize `yaml:"shm,omitempty"`
	// BuildKitCacheMountLimit bounds each persistent RUN --mount=type=cache
	// record. BuildKit's ordinary GC bounds the whole worker; this catches one
	// active mount that never becomes old enough for that policy to trim.
	BuildKitCacheMountLimit ByteSize `yaml:"buildkit_cache_mount_limit,omitempty"`

	Image string `yaml:"image,omitempty"`

	// WarmPool is reserved for pre-booted idle VMs. Validation refuses a non-zero
	// value until a provider implements it, because accepting an inert cost setting
	// would tell an operator cold-start capacity exists when it does not.
	WarmPool int `yaml:"warm_pool,omitempty"`

	// Intercept routes the Actions results origin through the node-local cache proxy.
	// False is the safe default because the same origin carries artifact metadata.
	Intercept bool `yaml:"intercept,omitempty"`

	// MaxConcurrent caps simultaneous instances of this tier, counting warm ones.
	// Zero means "no per-tier cap" and is only legal for non-macOS tiers.
	MaxConcurrent int `yaml:"max_concurrent,omitempty"`

	// Reserved is how many simultaneous instances of this tier are always available to
	// it, no matter how busy every other tier is.
	//
	// A FLOOR, where MaxConcurrent is a ceiling. Billet shares one budget across every
	// tier and headroom is whatever is left, so a tier with steady demand can hold all
	// of it while the others advertise zero and their jobs queue at GitHub. A
	// reservation is deducted from what OTHER tiers may take, only while it is unmet.
	//
	// THE COST IS CAPACITY OTHER TIERS CANNOT USE. An idle listener keeps one discovery
	// slot rather than claiming its whole floor, but the allocator still holds every
	// unmet reserved slot away from competing tiers. Reserve for tiers that need a hard
	// guarantee under contention. Zero is the default.
	Reserved int `yaml:"reserved,omitempty"`
}

// WorkloadTrust is the launch authority shared by a tier's runner pool.
type WorkloadTrust string

const (
	WorkloadUntrusted WorkloadTrust = "untrusted"
	WorkloadTrusted   WorkloadTrust = "trusted"
)

// Valid reports whether the pool trust is one Billet understands.
func (t WorkloadTrust) Valid() bool {
	return t == WorkloadUntrusted || t == WorkloadTrusted
}

// Effective returns the restrictive migration default for an omitted trust.
func (t WorkloadTrust) Effective() WorkloadTrust {
	if t == "" {
		return WorkloadUntrusted
	}
	return t
}

// PoolPolicyErrors reports unsafe or contradictory authority for a pooled
// scale-set tier. Exported because alloc.New cannot assume its catalogue came
// through Config.Load and must enforce the same boundary.
func (t Tier) PoolPolicyErrors(where string) []error {
	var errs []error
	trust := t.Trust.Effective()
	if !trust.Valid() {
		errs = append(errs, fmt.Errorf("%s: trust %q is not one of [untrusted trusted]; "+
			"scale-set runners are pooled, so launch authority must be explicit", where, t.Trust))
	}
	if trust == WorkloadTrusted {
		if t.RunnerGroup == "" || strings.EqualFold(t.RunnerGroup, "default") {
			errs = append(errs, fmt.Errorf("%s: trusted pools require a non-default runner_group", where))
		}
		if len(t.Workflows) == 0 {
			errs = append(errs, fmt.Errorf("%s: trusted pools require an exact workflows allowlist", where))
		}
	} else if len(t.Workflows) != 0 {
		errs = append(errs, fmt.Errorf("%s: workflows applies only to a trusted pool", where))
	}
	seenWorkflows := make(map[string]struct{}, len(t.Workflows))
	for j, workflow := range t.Workflows {
		if err := checkWorkflowRef(workflow); err != nil {
			errs = append(errs, fmt.Errorf("%s: workflows[%d] %q %w", where, j, workflow, err))
		}
		if _, duplicate := seenWorkflows[workflow]; duplicate {
			errs = append(errs, fmt.Errorf("%s: workflow %q is listed twice", where, workflow))
		}
		seenWorkflows[workflow] = struct{}{}
	}
	if trust == WorkloadUntrusted && t.Intercept {
		errs = append(errs, fmt.Errorf("%s: an untrusted pool cannot enable Actions cache interception", where))
	}
	if t.Intercept && t.CacheScope == nil {
		errs = append(errs, fmt.Errorf("%s: intercept requires a static cache_scope because JobStarted arrives after launch", where))
	}
	if scope := t.CacheScope; scope != nil {
		if strings.TrimSpace(scope.Owner) == "" || strings.TrimSpace(scope.Owner) != scope.Owner ||
			strings.Contains(scope.Owner, "/") || len(scope.Owner) > 100 {
			errs = append(errs, fmt.Errorf("%s: cache_scope.owner must be one trimmed path component no longer than 100 bytes", where))
		}
		if strings.TrimSpace(scope.Repository) == "" ||
			strings.TrimSpace(scope.Repository) != scope.Repository ||
			strings.Contains(scope.Repository, "/") || len(scope.Repository) > 100 {
			errs = append(errs, fmt.Errorf("%s: cache_scope.repository must be one trimmed path component no longer than 100 bytes", where))
		}
		if err := checkWorkflowRef(scope.WorkflowRef); err != nil {
			errs = append(errs, fmt.Errorf("%s: cache_scope.workflow_ref %q %w", where, scope.WorkflowRef, err))
		} else {
			workflow := strings.SplitN(strings.SplitN(scope.WorkflowRef, "@", 2)[0], "/", 3)
			if len(workflow) < 2 || workflow[0] != scope.Owner || workflow[1] != scope.Repository {
				errs = append(errs, fmt.Errorf("%s: cache_scope owner/repository must match workflow_ref", where))
			}
		}
		if t.Intercept {
			if _, allowed := seenWorkflows[scope.WorkflowRef]; !allowed {
				errs = append(errs, fmt.Errorf("%s: cache_scope.workflow_ref must be one of the trusted workflows", where))
			}
		}
	}

	return errs
}

// CheckWorkflowRef reports why a workflow ref is not a usable GitHub allowlist
// entry, or nil. Exported for the same reason CheckEC2Region and CheckCeph are:
// `billet init` validates a `--workflow` flag against the one rule tier
// validation applies, so a bad flag is refused by its own name rather than
// surfacing later as a config-load error blaming the generated tier.
func CheckWorkflowRef(value string) error { return checkWorkflowRef(value) }

// CheckRunnerGroup reports why a runner group name cannot be looked up by the
// scale-set client, or nil for an empty name (GitHub's default group). Exported
// alongside CheckWorkflowRef so `billet init` can validate a `--runner-group`
// flag against the transport-safety rule before writing a config.
func CheckRunnerGroup(group string) error { return checkRunnerGroup(group) }

func checkWorkflowRef(value string) error {
	if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value || len(value) > 2048 ||
		strings.ContainsAny(value, "\x00\r\n") {
		return errors.New("must be a non-empty, trimmed GitHub workflow ref no longer than 2048 bytes")
	}
	workflow, ref, ok := strings.Cut(value, "@")
	if !ok || workflow == "" || ref == "" || strings.Contains(ref, "@") {
		return errors.New("must have owner/repository/.github/workflows/file.yml@ref form")
	}
	parts := strings.Split(workflow, "/")
	if len(parts) != 5 || parts[0] == "" || parts[1] == "" || parts[2] != ".github" ||
		parts[3] != "workflows" || (pathpkg.Ext(parts[4]) != ".yml" && pathpkg.Ext(parts[4]) != ".yaml") {
		return errors.New("must name owner/repository/.github/workflows/file.yml")
	}
	return nil
}

// CacheScope is the authenticated identity an intercepted Actions cache uses.
type CacheScope struct {
	Owner       string `yaml:"owner"`
	Repository  string `yaml:"repository"`
	WorkflowRef string `yaml:"workflow_ref"`
}

// TierLaunch is the part of a tier whose spelling belongs to one backend.
type TierLaunch struct {
	Image   string   `yaml:"image"`
	Command []string `yaml:"command,omitempty"`
}

// DefaultMacOSVMLimit is Apple's licensing cap on macOS guests per Apple-branded
// host. Linux guests on the same machine are not subject to it.
//
// A DEFAULT, not a hard ceiling: a deployment sets its own per-host number via
// NodePolicy.MacOSVMLimit. What the default guarantees is that a config which says
// nothing gets Apple's standard terms rather than "unlimited".
//
// The static check is a guard, not the enforcement point — the allocator also
// holds a host-wide count of running plus warm macOS guests at runtime, because
// two separately-valid tiers on one node share one physical Mac.
const DefaultMacOSVMLimit = 2

// MinMacOSGuestMemory is the smallest macOS guest Apple's hypervisor will
// start, and it is a MEASUREMENT rather than a recommendation.
//
// Virtualization.framework refuses anything below it outright —
// "LessThanMinimalResourcesError: VM should have 4294967296 bytes of memory at
// minimum" — so a smaller tier is a config error, not a small guest. Recorded
// against the reference Mac in docs/reference-hardware.md, the same way
// DefaultMacOSVMLimit is pinned to the refusal a third concurrent guest gets.
//
// ENFORCED HERE AND NOWHERE ELSE, and that is forced rather than chosen:
// provider.Spec carries no guest OS, so the tart backend's checkSpec cannot ask
// this question at the launch boundary. Config validation is the only layer that
// knows a tier is macOS, so do not add a second half-copy under the provider —
// it could only guess.
const MinMacOSGuestMemory ByteSize = 4 * GiB

const (
	// DefaultBuildKitCacheMountLimit bounds one BuildKit cache mount when a tier
	// does not choose a tighter policy.
	DefaultBuildKitCacheMountLimit ByteSize = 20 * GiB
	// MaxBuildKitCacheMountLimit is the largest volume the sticky-disk API can
	// create, so a larger per-mount number could never constrain anything.
	MaxBuildKitCacheMountLimit ByteSize = 100 * GiB
)

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
// One function, because `provider:` and `providers:` are two spellings of the same
// field and validating them separately is how they drift.
//
// EXPORTED because alloc.New cannot assume its catalogue came through Load — a
// caller can construct tiers directly — and a rule only one entry point enforces
// is a rule with a second entry point that does not.
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

// ReservationErrors reports everything wrong with a tier's floor, on its own.
//
// The cross-tier sum is checked separately, because it needs the whole
// catalogue and the budget.
func (t Tier) ReservationErrors(where string) []error {
	var errs []error

	if t.Reserved < 0 {
		errs = append(errs, fmt.Errorf("%s: reserved %d is negative", where, t.Reserved))
	}

	if t.MaxConcurrent > 0 && t.Reserved > t.MaxConcurrent {
		errs = append(errs, fmt.Errorf(
			"%s: reserved %d exceeds max_concurrent %d; the reservation could never be "+
				"filled and would hold that capacity back from every other tier forever",
			where, t.Reserved, t.MaxConcurrent))
	}

	return errs
}

// InterceptionErrors reports tiers that could reach a backend without the local
// storage and guest control the transparent Actions cache requires.
//
// Exported because alloc.New cannot assume its catalogue came through Load.
func (t Tier) InterceptionErrors(where string) []error {
	if !t.Intercept {
		return nil
	}

	providers := t.AcceptableProviders()
	if len(providers) != 1 || providers[0] != ProviderFirecracker {
		return []error{fmt.Errorf("%s: intercept requires only the firecracker provider; "+
			"remote and fallback providers cannot reach this node's site-local archive store", where)}
	}
	if t.GuestOS != GuestLinux {
		return []error{fmt.Errorf("%s: intercept currently requires a Linux guest", where)}
	}

	return nil
}

// ReservedVCPU is the vCPU a tier's floor holds back from other tiers.
func (t Tier) ReservedVCPU() int { return t.Reserved * t.VCPU }

// ReservedMemory is the memory a tier's floor holds back from other tiers.
func (t Tier) ReservedMemory() ByteSize { return ByteSize(t.Reserved) * t.Memory }

// macOSProviders are the backends that can serve a macOS guest.
//
// AN ALLOWLIST, because the consequence of a wrong answer is asymmetric: a backend
// missing from here refuses a tier loudly, while one wrongly included lets a macOS
// tier bind to a Linux host, where placement only tests list membership and the
// failure surfaces inside somebody's job.
//
// `tart` serves macOS on Apple hardware the operator owns, under the operator's own
// licence. `codebuild` reaches AWS-managed Apple silicon through a reserved-capacity
// MAC_ARM fleet, under AWS's agreement rather than the operator's — which is the
// reason Apple's per-host allowance is not billet's default there; see
// validateGuestOSRules.
var macOSProviders = []ProviderKind{ProviderTart, ProviderCodeBuild}

// ServesMacOS reports whether this backend can run a macOS guest at all.
//
// THE ONE READER OF macOSProviders OUTSIDE VALIDATION. `billet check` used to ask
// `!= tart` and printed "macOS n/a (codebuild cannot run macOS guests)" beside a
// node whose fleet had just run an Xcode job — a second copy of the allowlist,
// written before the second member existed, and nothing tied it to this one.
func (p ProviderKind) ServesMacOS() bool { return slices.Contains(macOSProviders, p) }

// GuestOSProviderErrors reports backends that cannot host a tier's guest OS.
//
// Split out from the fuller relational validation so alloc can apply the part that
// is a SAFETY invariant rather than a configuration convenience: only some backends
// can serve macOS at all, and runtime placement only tests list membership, so a
// macOS tier that fell back to a Linux backend would bind there happily.
func (t Tier) GuestOSProviderErrors(where string) []error {
	var errs []error

	for _, p := range t.AcceptableProviders() {
		switch {
		case t.GuestOS == GuestMacOS && !slices.Contains(macOSProviders, p):
			errs = append(errs, fmt.Errorf(
				"%s: guest_os macos requires one of %v — tart on Apple hardware you own, or "+
					"codebuild on an AWS-managed MAC_ARM fleet — but this tier also accepts %q",
				where, macOSProviders, p))

		case t.GuestOS == GuestWindows && p == ProviderTart:
			errs = append(errs, fmt.Errorf("%s: the tart provider cannot run Windows guests", where))

		// BILLET SHIPS NO WINDOWS RUNNER, and CodeBuild does offer Windows
		// environments — so without this a Windows tier on codebuild passes every
		// check here and is then refused by node.codebuild.environment_type on the
		// one machine that could serve it, which reports the problem in the wrong
		// file. Named separately from tart's clause because the reasons differ:
		// tart cannot run Windows at all, and billet cannot start a runner in it.
		case t.GuestOS == GuestWindows && p == ProviderCodeBuild:
			errs = append(errs, fmt.Errorf("%s: billet ships no Windows runner image or runner "+
				"entrypoint, so a codebuild Windows tier would start a build that registers "+
				"nothing", where))
		}
	}

	return errs
}

// AcceptableProviders reports the backends this tier may run on, most preferred
// first. The single reader for that question, so `provider:` and `providers:`
// cannot drift apart — callers must not consult Tier.Provider directly.
//
// CLONED, because callers keep what this returns: the allocator copies the list
// onto every lease it reserves, and handing out the tier's own backing array would
// let a caller change what future leases authorize.
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

// LaunchErrors reports incomplete or ambiguous backend-specific boot details.
func (t Tier) LaunchErrors(where string) []error {
	accepted := t.AcceptableProviders()
	var errs []error

	if len(t.Launch) == 0 {
		if len(accepted) > 1 {
			errs = append(errs, fmt.Errorf("%s: a tier with multiple providers must set launch "+
				"for each provider; image names and runner commands belong to their backend", where))

			return errs
		}

		if t.Image == "" {
			errs = append(errs, fmt.Errorf("%s: image is required", where))
		}

		return errs
	}

	if t.Image != "" {
		errs = append(errs, fmt.Errorf("%s: set either image or launch, not both; one image "+
			"cannot name artifacts for several backends", where))
	}

	if len(t.Command) > 0 {
		errs = append(errs, fmt.Errorf("%s: set commands inside launch when launch is used; "+
			"billet will not guess whether a top-level command applies to every backend", where))
	}

	acceptedSet := make(map[ProviderKind]struct{}, len(accepted))
	for _, provider := range accepted {
		acceptedSet[provider] = struct{}{}

		launch, ok := t.Launch[provider]
		if !ok {
			errs = append(errs, fmt.Errorf("%s: launch.%s is required because the tier accepts %s",
				where, provider, provider))
		} else if launch.Image == "" {
			errs = append(errs, fmt.Errorf("%s: launch.%s.image is required", where, provider))
		}
	}

	extra := make([]string, 0, len(t.Launch))
	for provider := range t.Launch {
		if _, ok := acceptedSet[provider]; !ok {
			extra = append(extra, string(provider))
		}
	}
	slices.Sort(extra)

	for _, provider := range extra {
		errs = append(errs, fmt.Errorf("%s: launch.%s is set, but the tier does not accept %s",
			where, provider, provider))
	}

	return errs
}

// ImageFor returns the image name understood by a selected provider.
func (t Tier) ImageFor(provider ProviderKind) string {
	if launch, ok := t.Launch[provider]; ok {
		return launch.Image
	}

	return t.Image
}

// RunnerCommandFor returns the argv understood by a selected provider.
func (t Tier) RunnerCommandFor(provider ProviderKind) []string {
	if launch, ok := t.Launch[provider]; ok && len(launch.Command) > 0 {
		return slices.Clone(launch.Command)
	}
	if len(t.Command) > 0 {
		return slices.Clone(t.Command)
	}
	if provider == ProviderFirecracker {
		return []string{"./billet-runner-service"}
	}
	if provider == ProviderEC2 {
		return []string{"/usr/local/bin/billet-runner"}
	}

	if provider == ProviderCodeBuild {
		// WHERE BILLET'S OWN BUILDSPEC PUTS THE RUNNER, which is a path inside the
		// build rather than in the image: a CodeBuild curated image ships no Actions
		// runner (CodeBuild's own runner feature installs one during
		// DOWNLOAD_SOURCE, which billet does not use), so the generated buildspec
		// either finds a preinstalled runner or fetches the pinned release and
		// leaves it here. A tier whose image ships its own overrides this like any
		// other command.
		return []string{"./run.sh"}
	}

	if provider == ProviderTart {
		// WHERE THE PUBLISHED IMAGES PUT IT. The macOS images billet documents
		// (ghcr.io/cirruslabs/macos-*) ship the Actions runner in
		// ~/actions-runner, and the tart backend delivers from the guest's home
		// directory — so the bare ./run.sh below resolves to nothing and the
		// first job of the day fails on a missing file.
		//
		// Safe as a default because run.sh re-homes itself: it resolves its own
		// directory from $0 before doing anything, so invoking it by a relative
		// path from $HOME works. Measured in the image rather than assumed. A
		// tier that builds its own image overrides this like any other command.
		return []string{"./actions-runner/run.sh"}
	}

	return t.RunnerCommand()
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

var labelRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,63}$`)

// awsRegionRe matches the SHAPE of a region rather than a list of them.
//
// An allowlist is a rule about somebody else's product, and it goes stale the
// next time AWS opens a region — at which point billet refuses a config that is
// perfectly correct. The shape catches the mistake people actually make, which is
// dropping the hyphens, and still admits partitions billet has never run in:
// us-gov-west-1, cn-north-1, ap-southeast-4.
var awsRegionRe = regexp.MustCompile(`^[a-z]{2,}(-[a-z]+)+-\d+$`)

// runnerGroupUnsafe are the characters that do not survive the scale-set client's
// handling of a group name.
//
// The client interpolates the name unescaped into a path, then url.Parse's it,
// reads Query(), and re-Encode's it — so the question is not "is this legal in a
// URL" but "does it survive one parse-and-re-encode round trip". Measured against
// v0.4.0 rather than reasoned about:
//
//	&   splits the value into another parameter    "Platform & Security" -> "Platform "
//	#   starts a fragment and truncates it         "a#b"                 -> "a"
//	;   ParseQuery has rejected it as a separator
//	    since Go 1.17 and drops the whole pair     "a;b"                 -> ""
const runnerGroupUnsafe = "&#;%+"

// maxRunnerGroupLen is BILLET's sanity bound, not GitHub's rule.
//
// GitHub does not document a runner group name length, so billet does not claim to
// know one. What is genuinely billet's concern is that a runaway config value
// cannot build a URL the Actions service rejects wholesale. It counts BYTES
// because the thing being bounded is a URL, and 512 is 170 characters even in the
// worst case for CJK text.
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

	// A PADDED NAME SURVIVES THE URL HANDLING INTACT, which is exactly the
	// problem: billet would ask GitHub about a group whose name really does begin
	// with a space, the lookup comes back "group not found", and that reads as a
	// permissions problem. `billet init` already trims this flag before writing a
	// config, so the two agree.
	if err := checkIdentityPadding("runner_group", group); err != nil {
		return err
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
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", name, err)
	}
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
			"being a one-machine product. docs/adr-003-ceph-rbd.md is how to build the cluster; " +
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

// DefaultServerStateDir is where a config that omits server.state_dir actually
// keeps its state — the value applyDefaults fills in. Exported for `billet
// init`'s identity refusal, which must treat an ABSENT key as this directory
// rather than as "no deployment to protect".
func DefaultServerStateDir() string { return defaultStateDir("server") }

// applyStateDefaults resolves the shorthand into the explicit form, ONCE, before
// anything validates or consumes it.
//
// THE SAME RULE THE REST OF THIS PACKAGE FOLLOWS: normalize in one place and
// write the result back, so validation and every consumer read the same value.
// Validation examining a shape the consumer does not use has now been found
// three times here, and it is silent every time.
//
// IT DOES NOT INVENT A BACKEND FOR A `state:` BLOCK THAT NAMES NONE. An absent
// backend is a config that did not say, and defaulting it to sqlite would make
// `state: {postgres: {...}}` a SQLite deployment carrying a PostgreSQL block
// nothing reads. Validation refuses it instead.
func (s *ServerConfig) applyStateDefaults() {
	if s.State == nil {
		if s.StateDir == "" {
			s.StateDir = defaultStateDir("server")
		}

		if s.IdentityDir == "" {
			s.IdentityDir = s.StateDir
		}

		return
	}

	if s.IdentityDir == "" && s.StateDir != "" {
		// Refused by validation as two spellings of one value; defaulted here
		// anyway so the diagnostic is about that rather than about a missing
		// identity_dir it would have had.
		s.IdentityDir = s.StateDir
	}
}

// LedgerBackend is the engine this config selects, resolved.
func (s *ServerConfig) LedgerBackend() StateBackend {
	if s.State == nil {
		return StateSQLite
	}

	return s.State.Backend
}

// LedgerPath is the SQLite ledger file, and is empty for any other backend.
//
// DERIVED, NEVER CONFIGURED. It is billet.db inside identity_dir, which is what
// state_dir has always meant and what every reader of a ledger file already
// assumes — the restore planner, the writer barrier and the archive among them.
func (s *ServerConfig) LedgerPath() string {
	if s.LedgerBackend() != StateSQLite {
		return ""
	}

	return filepath.Join(s.IdentityDir, "billet.db")
}

// LedgerDSNEnv names the environment variable holding the PostgreSQL DSN, and is
// empty for any other backend.
func (s *ServerConfig) LedgerDSNEnv() string {
	if s.State == nil || s.State.Postgres == nil {
		return ""
	}

	return s.State.Postgres.DSNEnv
}

// envVarName is what a legal environment variable name looks like.
//
// REFUSED RATHER THAN ACCEPTED-AND-EMPTY, because os.Getenv answers "" for a
// name no shell could ever have exported, and an empty DSN is a control plane
// that fails to start with a message about the DSN rather than about the name.
var envVarName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func defaultStateDir(role string) string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "billet", role)
	}
	return filepath.Join(".", ".billet", role)
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

// LoopbackAddr reports whether an address accepts only from this machine.
//
// SHARED WITH THE SERVER'S OWN DECISION: the wire is served without TLS on exactly
// the addresses this returns true for, so config validation and the listener must
// not answer differently. A wildcard is NOT loopback.
func LoopbackAddr(addr string) bool {
	return isLoopbackHostPort(addr)
}

// isLoopbackHostPort reports whether an address is on this machine.
func isLoopbackHostPort(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}

	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
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
	errs = append(errs, c.validateSites()...)
	errs = append(errs, c.validateTiers()...)
	errs = append(errs, c.validateCapacity()...)
	errs = append(errs, c.validateMacOSHostLimits()...)
	errs = append(errs, ValidateRelease(c.Release)...)
	errs = append(errs, c.validateBackup()...)

	return errors.Join(errs...)
}

// validateBackup checks the archive store, and refuses a block that names none.
//
// AN EMPTY `backup: {}` IS A MISTAKE, NOT A DEFAULT. It reads as "billet is
// looking after this" and does nothing at all, which is the one failure mode a
// backup must not have — the operator finds out on the day they need the archive
// that was never uploaded. Leaving the section out entirely is the supported way
// to say the directory is the seam.
//
// A BARE `backup:` WITH NOTHING UNDER IT CANNOT BE CAUGHT HERE, and that is
// MEASURED: yaml.v3 does not call UnmarshalYAML for a null value on a pointer
// field or a value one, so the field is left untouched and is indistinguishable
// from the key being absent. Catching it would mean decoding the document into
// a yaml.Node and walking it. What covers it instead is `billet local backup`
// itself, which says on EVERY run when an archive is still on the disk it
// protects.
func (c *Config) validateBackup() []error {
	if c.Backup == nil {
		return nil
	}

	if c.Backup.S3 == nil {
		return []error{errors.New("backup: is set but names no destination. Remove the section " +
			"if `billet local backup --out <dir>` and your own tooling are the plan — an empty " +
			"one reads as a backup billet is looking after and uploads nothing")}
	}

	// A BACKUP IS OF A CONTROL PLANE, so a node-only host declaring one has
	// written down somewhere nothing will ever put anything.
	if c.Server == nil {
		return []error{errors.New("backup: is set but this config declares no server, and a " +
			"backup is of a control plane: the ledger, the deployment identity and the " +
			"node-wire authority all live in server.state_dir")}
	}

	return CheckBackupS3(*c.Backup.S3)
}

// validateState refuses every way of naming the ledger that would leave a
// deployment believing it configured something it did not.
//
// Each rule below has a failure behind it, and the shape they share is that the
// alternative is SILENT: a config that loads, a control plane that starts, and a
// disagreement discovered later by an operator who has no reason to suspect this
// file.
// validateControllers refuses a controller layout this deployment cannot have.
//
// NORMALIZED RATHER THAN DEFAULTED AT EVERY READER, so `single` is written back
// and no caller has to remember what an empty value means. That is the rule this
// file already follows for a value nothing outside the process has an opinion
// about; it is not an identity, so there is nothing to compare it against
// elsewhere.
func (s *ServerConfig) validateControllers() []error {
	if s.Controllers == "" {
		s.Controllers = ControllersSingle

		return nil
	}

	switch s.Controllers {
	case ControllersSingle:
		return nil
	case ControllersActivePassive:
		// REFUSED ON A SQLITE LEDGER RATHER THAN IGNORED. There is nothing for a
		// second control plane to elect over — the ledger is a file on local
		// storage, and the state package refuses to put one anywhere a second
		// machine could reach — so a standby here would wait forever for a lock
		// that only its own host's other process could hold. Accepting the key and
		// doing nothing with it is the failure this file has already made three
		// times: a deployment that believes it configured something.
		if s.LedgerBackend() != StatePostgres {
			return []error{fmt.Errorf(
				"server.controllers: %s needs a ledger a second machine can reach, and this "+
					"deployment's is %s. A SQLite ledger is a file on local storage — billet "+
					"refuses to put one on a network filesystem at all — so there is nothing for "+
					"a second control plane to take over. Move the ledger to PostgreSQL "+
					"(server.state.backend: %s) or leave this as %s",
				ControllersActivePassive, s.LedgerBackend(), StatePostgres, ControllersSingle)}
		}

		return nil
	default:
		return []error{fmt.Errorf(
			"server.controllers: %q is not a controller layout billet knows; write %s (the "+
				"default: one control plane, and a second one is refused) or %s (a pair, where "+
				"whichever takes the claim first is the controller and the other waits)",
			s.Controllers, ControllersSingle, ControllersActivePassive)}
	}
}

// IdentityBackendKind is where this deployment's identity material lives,
// resolved. Absent means the file backend.
func (s *ServerConfig) IdentityBackendKind() IdentityBackend {
	if s == nil || s.Identity == nil || s.Identity.Backend == "" {
		return IdentityFile
	}

	return s.Identity.Backend
}

// IdentitySSM is the Parameter Store configuration, or nil.
func (s *ServerConfig) IdentitySSM() *IdentitySSMConfig {
	if s == nil || s.Identity == nil {
		return nil
	}

	return s.Identity.AWSSSM
}

// validateIdentity refuses an identity store this deployment cannot have.
func (c *Config) validateIdentity() []error {
	s := c.Server
	if s == nil || s.Identity == nil {
		return nil
	}

	switch s.Identity.Backend {
	case IdentityFile:
		if s.Identity.AWSSSM != nil {
			return []error{fmt.Errorf(
				"server.identity.aws_ssm is written but the backend is %s, so nothing would "+
					"read it. Set backend: %s, or remove the block",
				IdentityFile, IdentitySSM)}
		}

		return nil
	case IdentitySSM:
		return c.validateSSMIdentity()
	case "":
		return []error{fmt.Errorf(
			"server.identity.backend is required when server.identity is written out; "+
				"write %s (the default: the authority and the App key are files in "+
				"identity_dir) or %s", IdentityFile, IdentitySSM)}
	default:
		return []error{fmt.Errorf(
			"server.identity.backend: %q is not an identity store billet knows; write %s or %s",
			s.Identity.Backend, IdentityFile, IdentitySSM)}
	}
}

// validateSSMIdentity checks the Parameter Store block and refuses the second
// spelling of the App key.
func (c *Config) validateSSMIdentity() []error {
	var errs []error

	ssm := c.Server.Identity.AWSSSM
	if ssm == nil {
		return []error{fmt.Errorf(
			"server.identity.aws_ssm is required when the backend is %s; it names the region "+
				"and the parameter path this deployment's identity lives under", IdentitySSM)}
	}

	if strings.TrimSpace(ssm.Region) == "" {
		errs = append(errs, errors.New(
			"server.identity.aws_ssm.region is required; it is the SIGNING region and it "+
				"selects the endpoint, so there is no value billet could guess"))
	}

	switch {
	case strings.TrimSpace(ssm.Prefix) == "":
		errs = append(errs, errors.New(
			"server.identity.aws_ssm.prefix is required; everything billet stores lands under "+
				"it, so IAM can be scoped by path and two deployments cannot read each "+
				"other's authority"))
	case !strings.HasPrefix(ssm.Prefix, "/"):
		// A PARAMETER STORE PATH IS ABSOLUTE. A relative name is a legal parameter
		// and a DIFFERENT one, so accepting it would silently give a deployment a
		// store nobody's IAM policy covers.
		errs = append(errs, fmt.Errorf(
			"server.identity.aws_ssm.prefix must start with '/' (got %q); Parameter Store "+
				"paths are absolute, and a relative name is a different parameter rather "+
				"than the same one written informally", ssm.Prefix))
	}

	// TWO SPELLINGS OF ONE VALUE, WHICH THIS FILE HAS ALREADY GOT WRONG THREE
	// TIMES. With the App key in Parameter Store there is no path to read, and a
	// config carrying both would leave an operator unable to tell which one the
	// deployment is actually using — or worse, updating the one nothing reads.
	if c.GitHub != nil && c.GitHub.PrivateKeyPath != "" {
		errs = append(errs, fmt.Errorf(
			"github.private_key_path is written and server.identity.backend is %s, which are "+
				"two spellings of where the App key lives. With this backend the key is a "+
				"SecureString under %s, so remove the path; `billet github-app create` writes "+
				"it there", IdentitySSM, ssm.Prefix))
	}

	return errs
}

func (s *ServerConfig) validateState() []error {
	var errs []error

	// TWO SPELLINGS OF ONE VALUE. Merging them means guessing which the operator
	// meant when they disagree, and the guess is invisible.
	if s.StateDir != "" && s.State != nil {
		errs = append(errs, errors.New(
			"server.state_dir and server.state are two spellings of the same thing and only "+
				"one may be written. state_dir means `identity_dir: <dir>` plus "+
				"`state: {backend: sqlite}`; write that out instead if you need to name a "+
				"backend"))
	}

	if s.IdentityDir == "" {
		errs = append(errs, errors.New(
			"server.identity_dir is required when server.state is written out; it holds the "+
				"deployment identity, the node-wire CA, the process lock and the maintenance "+
				"fence, none of which move into a database"))
	}

	// AN IDENTITY IS REFUSED RATHER THAN TRIMMED, and a path is normalized. This
	// one is a path, so its padding is not an identity mistake — but it is also
	// the directory a CA lives in, and silently changing which directory a
	// config names is the failure this whole rule family exists for.
	if s.IdentityDir != "" && strings.TrimSpace(s.IdentityDir) != s.IdentityDir {
		errs = append(errs, fmt.Errorf(
			"server.identity_dir is %q, which begins or ends with whitespace; it names a "+
				"directory holding this deployment's private key, so billet will not guess "+
				"which one you meant", s.IdentityDir))
	}

	if s.State == nil {
		return errs
	}

	switch s.State.Backend {
	case StateSQLite:
		if s.State.Postgres != nil {
			errs = append(errs, errors.New(
				"server.state.postgres is set with backend: sqlite, so nothing would read it; "+
					"remove it or select the backend it configures"))
		}
	case StatePostgres:
		errs = append(errs, s.validatePostgresState()...)
	case "":
		errs = append(errs, errors.New(
			"server.state.backend is required; it is "+string(StateSQLite)+" or "+
				string(StatePostgres)))
	default:
		errs = append(errs, fmt.Errorf(
			"server.state.backend is %q; it is %s or %s",
			s.State.Backend, StateSQLite, StatePostgres))
	}

	return errs
}

func (s *ServerConfig) validatePostgresState() []error {
	var errs []error

	pg := s.State.Postgres
	if pg == nil || pg.DSNEnv == "" {
		return append(errs, errors.New(
			"server.state.postgres.dsn_env is required; it names the ENVIRONMENT VARIABLE "+
				"holding the connection string, because a DSN carries a password and a secret "+
				"written into this file ends up in a backup"))
	}

	if err := CheckDSNEnv(pg.DSNEnv); err != nil {
		errs = append(errs, fmt.Errorf("server.state.postgres.dsn_env: %w", err))
	}

	return errs
}

// CheckDSNEnv is the one rule for what may name the PostgreSQL connection
// string's environment variable.
//
// EXPORTED BECAUSE THERE ARE TWO ENTRY POINTS. Config validation reaches it for
// a file on disk, and `billet init` reaches it for a flag — and a rule enforced
// at only one of two entry points is an entry point that does not enforce it.
// Without this, `--state-dsn-env 9-lives` was accepted by the generator, written
// into the file, and then refused by Parse with a message blaming a generated
// block the operator never typed.
func CheckDSNEnv(name string) error {
	// THE MOST LIKELY MISTAKE IS PUTTING THE DSN ITSELF HERE, and it is worth its
	// own sentence: os.Getenv would answer "" for it, and the deployment would
	// fail to start complaining about an empty data source rather than about the
	// value that is wrong. Checked first, so the more specific diagnostic wins.
	if strings.ContainsAny(name, ":/@ ") || strings.HasPrefix(name, "postgres") {
		return fmt.Errorf(
			"%q looks like a connection string rather than the NAME of an environment "+
				"variable holding one. Write the name, and keep the DSN out of the config", name)
	}

	if !envVarName.MatchString(name) {
		return fmt.Errorf(
			"%q is not a legal environment variable name; nothing could export it, so the "+
				"DSN would always read as empty", name)
	}

	return nil
}

func (c *Config) validateServer() []error {
	if c.Server == nil {
		return nil
	}
	var errs []error

	errs = append(errs, c.Server.validateState()...)
	// AFTER validateState, because what a controller layout is allowed to be
	// depends on which backend the ledger is on and LedgerBackend answers from
	// the block that one has just checked.
	errs = append(errs, c.Server.validateControllers()...)
	errs = append(errs, c.validateIdentity()...)
	if err := validateHostPort("server.listen", c.Server.Listen); err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, c.validateBootstrapListen()...)

	if err := c.Server.Placement.Validate(); err != nil {
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
	// Parsed here so a typo is reported when the file is READ, rather than at the
	// shutdown that needed it — by which point the operator is watching a restart
	// that will not finish and has no reason to suspect the config.
	if _, err := c.Server.DrainTimeoutDuration(); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// validateBootstrapListen checks the enrollment listener against the wire it
// exists to keep anonymous traffic off.
func (c *Config) validateBootstrapListen() []error {
	addr := strings.TrimSpace(c.Server.BootstrapListen)
	if addr == "" {
		return nil
	}

	// A LOOPBACK WIRE HAS NO CERTIFICATES AT ALL, so it has nothing to enroll a
	// machine into — and nothing off this host could reach the operational wire
	// anyway. The same rule refuses node.tls against a loopback server.
	if LoopbackAddr(c.Server.Listen) {
		return []error{fmt.Errorf(
			"server.bootstrap_listen is set, but server.listen is %q — a control plane that "+
				"accepts only on loopback serves plain HTTP and issues no certificates, so there "+
				"is nothing for a node to enroll into. Remove server.bootstrap_listen, or bind "+
				"server.listen to the address this deployment publishes to its fleet",
			c.Server.Listen)}
	}

	var errs []error

	if err := validateHostPort("server.bootstrap_listen", addr); err != nil {
		errs = append(errs, err)
	}

	// THE POINT IS THAT THEY ARE NOT THE SAME LISTENER, and equality of the two
	// strings is not the question — "0.0.0.0:7717" and "127.0.0.1:7717" are
	// different strings and the same socket. Leaving that to bind time was a
	// worse answer than it looked: whichever listener binds second reports the
	// collision, so the operator is sent to the setting that is fine.
	if addressesOverlap(addr, strings.TrimSpace(c.Server.Listen)) {
		errs = append(errs, fmt.Errorf(
			"server.bootstrap_listen is %q and server.listen is %q, which are the same socket. "+
				"Separating them is the whole point: the node wire demands a client certificate "+
				"in the handshake, and enrollment cannot, so one address cannot serve both",
			addr, strings.TrimSpace(c.Server.Listen)))
	}

	return errs
}

// addressesOverlap reports whether two listen addresses would contend for one
// socket.
//
// A WILDCARD OVERLAPS EVERYTHING ON ITS PORT, which is the case a string
// comparison misses: ":7717", "0.0.0.0:7717" and "[::]:7717" all accept on the
// address any concrete host on this machine names. Both wildcard forms count,
// because a dual-stack "::" listener takes the v4 address too.
//
// It answers NO for anything it cannot parse. Whether an address is well formed
// is validateHostPort's question, and reporting one malformed address as two
// problems helps nobody.
func addressesOverlap(a, b string) bool {
	aHost, aPort, aErr := net.SplitHostPort(a)
	bHost, bPort, bErr := net.SplitHostPort(b)

	if aErr != nil || bErr != nil || aPort != bPort {
		return false
	}

	if isWildcardHost(aHost) || isWildcardHost(bHost) {
		return true
	}

	return aHost == bHost
}

// isWildcardHost reports whether a listen host accepts on every interface.
func isWildcardHost(host string) bool {
	return host == "" || host == "0.0.0.0" || host == "::"
}

func (c *Config) validateGitHub() []error {
	if c.GitHub == nil {
		if c.Server != nil {
			return []error{errors.New("github section is required for the server role")}
		}
		return nil
	}
	var errs []error
	// CheckOrg rather than a non-empty test: this name is concatenated into the
	// scale-set client's organization URL, so a value that validates here and
	// names a different organization there is the whole failure.
	if err := CheckOrg(c.GitHub.Org); err != nil {
		errs = append(errs, err)
	}
	if c.GitHub.AppID <= 0 {
		errs = append(errs, errors.New("github.app_id is required; run `billet github-app create`"))
	}
	if c.GitHub.InstallationID <= 0 {
		errs = append(errs, errors.New("github.installation_id is required; creating an App does not install it"))
	}
	// REQUIRED ONLY WHILE THE KEY IS A FILE. With the identity store selected the
	// App key is a SecureString and there is no path to name, so requiring one
	// would make a correct deployment unloadable — and the two-spellings refusal
	// beside it means a config cannot carry both.
	if c.GitHub.PrivateKeyPath == "" && c.Server.IdentityBackendKind() == IdentityFile {
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
	if c.Node.Cache == nil {
		for i := range c.Tiers {
			if c.Tiers[i].Intercept && c.Tiers[i].AcceptsProvider(c.Node.Provider) {
				errs = append(errs, fmt.Errorf("tier %q enables intercept, but node.cache is not "+
					"configured on this %s node", c.Tiers[i].Label, c.Node.Provider))

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

// validateTartNode checks the Apple Silicon block, and refuses it on any other
// backend for the reason node.firecracker and node.ceph are refused: nothing
// else reads it, so it would describe an isolation policy no launch consults.
func (c *Config) validateTartNode() []error {
	if c.Node.Tart == nil {
		return nil
	}

	if c.Node.Provider != ProviderTart {
		return []error{fmt.Errorf("node.tart is set but this node's provider is %s, and only "+
			"tart reads it, so this host would describe an isolation policy that no launch "+
			"consults", c.Node.Provider)}
	}

	return CheckTart(*c.Node.Tart)
}

// CheckTart validates the Apple Silicon block.
//
// EXPORTED for the reason alloc.New re-applies config's rules: a caller that
// built a TartConfig in code never passed through Load, and a rule enforced in
// only one of the two entry points is not enforced.
func CheckTart(t TartConfig) []error {
	var errs []error

	// A MISSPELLING MUST NOT READ AS "NO ISOLATION". Both are absent as far as
	// a struct field is concerned, and the two mean opposite things: one is an
	// operator who decided not to run untrusted work, the other is an operator
	// who thought they had configured it. Only the empty string is the former.
	switch t.UntrustedIsolation {
	case "", IsolationSoftnet:
	default:
		errs = append(errs, fmt.Errorf("node.tart.untrusted_isolation is %q, and the only "+
			"mechanism billet drives is %q (tart's own userspace packet filter, which confines a "+
			"guest to public destinations and isolates guests from each other)",
			t.UntrustedIsolation, IsolationSoftnet))
	}

	if len(t.UntrustedDNS) > 0 && t.UntrustedIsolation == "" {
		errs = append(errs, errors.New("node.tart.untrusted_dns is set but "+
			"node.tart.untrusted_isolation is not, so no guest is ever isolated and nothing "+
			"reads these resolvers"))
	}

	for _, resolver := range t.UntrustedDNS {
		// AN ADDRESS, NOT A NAME. This is what a guest resolves THROUGH, so a
		// hostname here could only be resolved by the resolver it is meant to
		// configure. netip.ParseAddr refuses a port, a CIDR and a name alike.
		addr, err := netip.ParseAddr(resolver)
		if err != nil {
			errs = append(errs, fmt.Errorf("node.tart.untrusted_dns %q is not an IP address: "+
				"a guest with no working resolver cannot look one up, so this must be literal",
				resolver))

			continue
		}

		// AND NOT A ZONE, which is the part that made "it parsed as an address"
		// a weaker statement than it looks. ParseAddr accepts an IPv6 zone and
		// places NO restriction on its contents, so
		// `2001:4860:4860::8888%x;touch${IFS}/tmp/pwn` parses cleanly — and this
		// value is written into a shell script that runs inside the guest. A
		// zone is a link-local scope name for the host it is used on; it is
		// meaningless as a resolver a guest was handed, so refusing it costs
		// nothing and removes the class.
		if addr.Zone() != "" {
			errs = append(errs, fmt.Errorf("node.tart.untrusted_dns %q carries an IPv6 zone "+
				"(%q); a zone names a link on the machine that uses it, so it cannot describe "+
				"a resolver for a guest, and billet refuses it rather than passing it to one",
				resolver, addr.Zone()))
		}
	}

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

var (
	s3BucketName       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]{1,61}[a-z0-9])$`)
	availabilityZoneID = regexp.MustCompile(`^[a-z0-9-]+$`)
)

func (c *Config) validateEBSS3Node() []error {
	if c.Node.EBSS3 == nil {
		return nil
	}
	if c.Node.Provider != ProviderEC2 {
		return []error{fmt.Errorf("node.ebs_s3 is set but this node's provider is %s; EBS volumes can attach only to EC2 instances in their availability zone", c.Node.Provider)}
	}

	e := c.Node.EBSS3
	var errs []error
	if c.Node.Site == "" {
		errs = append(errs, errors.New("node.site is required with node.ebs_s3 because cache keys are scoped by site"))
	}
	errs = append(errs, CheckEBSS3(*e)...)
	if c.Node.EC2 != nil && e.Region != c.Node.EC2.Region {
		errs = append(errs, fmt.Errorf("node.ebs_s3.region %q differs from node.ec2.region %q; a cache volume and the instance using it must be in one region", e.Region, c.Node.EC2.Region))
	}
	if c.Node.Site != "" {
		for _, site := range c.Sites {
			if site.Name == c.Node.Site && site.Store != SiteStoreEBSS3 {
				errs = append(errs, fmt.Errorf("node.site %q selects %s storage but node.ebs_s3 is configured", site.Name, site.Store))
			}
		}
	}

	return errs
}

// CheckEBSS3 applies the safety rules needed by both config loading and the
// exported cloud-store constructor.
// CheckBackupS3 reports everything wrong with the archive store.
//
// EXPORTED AND CALLED FROM BOTH SIDES, like CheckEBSS3 and CheckCeph: the store
// constructor is exported too, so a caller whose configuration did not come
// through config.Load must not be able to build one that points somewhere else.
//
// THE REGION IS TWO DIFFERENT FACTS depending on the endpoint, and conflating
// them refuses correct deployments. With no endpoint it selects the AWS host
// billet dials, so it has to look like an AWS region or every request goes
// somewhere that does not exist. With one, it is only the SIGNING region — the
// server on the far side decides what it accepts, and Ceph RGW and MinIO
// deployments legitimately use names AWS never issued.
func CheckBackupS3(b BackupS3Config) []error {
	var errs []error

	switch {
	case b.Endpoint == "":
		if err := CheckEC2Region(b.Region); err != nil {
			errs = append(errs, errors.New("backup.s3.region is required and must look like an "+
				"aws region (something like us-west-2): with no backup.s3.endpoint it selects "+
				"the host billet dials, and it is signed into every request"))
		}
	case b.Region == "" || strings.ContainsAny(b.Region, " \t\r\n\x00"):
		errs = append(errs, errors.New("backup.s3.region is required even with an endpoint: "+
			"SigV4 signs a region into every request, and the store on the far side compares "+
			"it. Use the one that store expects, conventionally us-east-1"))
	}

	if err := CheckBackupEndpoint(b.Endpoint); err != nil {
		errs = append(errs, err)
	}

	// THE SAME BUCKET RULE THE CACHE STORE USES. A dot in the name breaks TLS
	// against the virtual-host address, which is the one billet builds when no
	// endpoint overrides it.
	if !s3BucketName.MatchString(b.Bucket) || strings.Contains(b.Bucket, ".") ||
		net.ParseIP(b.Bucket) != nil || strings.Contains(b.Bucket, "..") ||
		strings.Contains(b.Bucket, ".-") || strings.Contains(b.Bucket, "-.") {
		errs = append(errs, fmt.Errorf(
			"backup.s3.bucket %q is not a TLS-compatible S3 bucket name without dots", b.Bucket))
	}

	// A LITERAL PREFIX, because it is what an IAM policy is scoped to. A wildcard
	// there widens the grant to every sibling prefix in the bucket — which is
	// every other deployment's archives, each of which is two private keys.
	if strings.HasPrefix(b.Prefix, "/") || strings.HasSuffix(b.Prefix, "/") ||
		strings.ContainsRune(b.Prefix, 0) || strings.ContainsAny(b.Prefix, "*?") {
		errs = append(errs, errors.New("backup.s3.prefix must be a relative object prefix with "+
			"no leading or trailing slash, no NUL and no wildcard"))
	}

	for segment := range strings.SplitSeq(b.Prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			errs = append(errs,
				errors.New("backup.s3.prefix contains an empty, dot, or dot-dot segment"))

			break
		}
	}

	return errs
}

func CheckEBSS3(e EBSS3Config) []error {
	var errs []error
	if err := CheckEC2Region(e.Region); err != nil {
		errs = append(errs, fmt.Errorf("node.ebs_s3.region: %w", err))
	}
	if e.AvailabilityZone == e.Region || !strings.HasPrefix(e.AvailabilityZone, e.Region) ||
		!availabilityZoneID.MatchString(e.AvailabilityZone) {
		errs = append(errs, fmt.Errorf("node.ebs_s3.availability_zone %q is not a zone in region %q", e.AvailabilityZone, e.Region))
	}
	if !s3BucketName.MatchString(e.Bucket) || strings.Contains(e.Bucket, ".") || net.ParseIP(e.Bucket) != nil ||
		strings.Contains(e.Bucket, "..") || strings.Contains(e.Bucket, ".-") ||
		strings.Contains(e.Bucket, "-.") {
		errs = append(errs, fmt.Errorf("node.ebs_s3.bucket %q is not a TLS-compatible S3 bucket name without dots", e.Bucket))
	}
	if strings.HasPrefix(e.Prefix, "/") || strings.HasSuffix(e.Prefix, "/") ||
		strings.ContainsRune(e.Prefix, 0) {
		errs = append(errs, errors.New("node.ebs_s3.prefix must be a relative object prefix without a trailing slash or NUL"))
	}
	for _, segment := range strings.Split(e.Prefix, "/") {
		if segment == "" || segment == "." || segment == ".." {
			errs = append(errs, errors.New("node.ebs_s3.prefix contains an empty, dot, or dot-dot segment"))

			break
		}
	}

	return errs
}

// validateCephNode reports everything wrong with this host's storage.
//
// TWO DIRECTIONS, because both mistakes are silent. A firecracker node with no
// ceph block has nowhere to put a golden image or a cache, and would fail on the
// first launch with a message about a missing rootfs rather than about the
// storage that was never configured. A node of any other backend WITH one has
// written down a cluster nothing will ever read.
func (c *Config) validateCephNode() []error {
	// A PROVIDER THAT IS NOT A PROVIDER GETS ONE DIAGNOSTIC, not two. validateNode
	// already refuses an unknown backend by name, and a second error telling the
	// operator that their typo "cannot attach a block device" is billet asserting
	// something about a string it does not recognise. validateGuestOSRules skips
	// its relational checks on an invalid enum for the same reason.
	if !c.Node.Provider.Valid() {
		return nil
	}

	if c.Node.Provider != ProviderFirecracker {
		if c.Node.Ceph != nil {
			// NAMED, NOT CHARACTERISED. Firecracker is the only backend that reads
			// this block, so anything else would carry a cluster nothing consults —
			// and the reasons differ per backend, so the message gives the one that
			// applies rather than a sentence about containers that is untrue of tart.
			return []error{fmt.Errorf("node.ceph is set but this node's provider is %s, and only "+
				"firecracker reads it (%s), so this host would carry a cluster nothing "+
				"consults", c.Node.Provider, cephRefusalReason(c.Node.Provider))}
		}

		return nil
	}

	if c.Node.Ceph == nil {
		return []error{errors.New("node.ceph is required when provider is firecracker: golden " +
			"images, per-job root clones and every cache are RBD images, so a firecracker host " +
			"with no cluster has nothing to boot a guest from")}
	}

	return CheckCeph(*c.Node.Ceph)
}

// cephRefusalReason says why THIS backend has no use for a storage block.
//
// One clause per backend rather than one sentence covering all of them: a
// container really does have nowhere to attach a block device, an ec2 node's
// compute really is in a region that cannot reach the cluster, and neither is
// true of tart, which simply does not read this yet.
func cephRefusalReason(p ProviderKind) string {
	switch p {
	case ProviderDocker:
		return "a container has nowhere to attach a block device"
	case ProviderEC2:
		return "an ec2 node's compute runs in a region that cannot reach this cluster"
	case ProviderCodeBuild:
		return "a codebuild build has nowhere to attach a block device and runs in a region " +
			"that cannot reach this cluster"
	case ProviderTart, ProviderFirecracker:
		return "nothing in that backend reads it"
	default:
		return "nothing in that backend reads it"
	}
}

// normalize fills in the identity billet authenticates as and trims what it later
// passes to the rbd client verbatim.
//
// THE SAME REASON EC2Config IS NORMALIZED. Validation trimmed to check and the
// caller used the raw string, so `image_pool: "  tank  "` passed the shape check
// and was then handed to Ceph with its padding. YAML strips whitespace from a
// plain scalar but keeps it inside quotes, so an ordinary-looking file reaches it.
func (p *CephConfig) normalize() {
	if p == nil {
		return
	}

	p.ConfPath = strings.TrimSpace(p.ConfPath)
	p.User = strings.TrimSpace(p.User)
	p.KeyringPath = strings.TrimSpace(p.KeyringPath)
	p.ImagePool = strings.TrimSpace(p.ImagePool)
	p.CachePool = strings.TrimSpace(p.CachePool)

	if p.User == "" {
		p.User = DefaultCephUser
	}
}

// DefaultCephUser is the RADOS identity billet authenticates as when the config
// names none. Deliberately not `admin` — see CephConfig.User.
const DefaultCephUser = "billet"

// DefaultCephConf is where Ceph's own search path finds a cluster's monitors, and
// therefore where billet is talking to when node.ceph.conf_path says nothing.
const DefaultCephConf = "/etc/ceph/ceph.conf"

// ConfPathOrDefault names the file this configuration reaches the cluster
// through, so an operator reading `billet check` can see WHICH cluster answered.
//
// An empty conf_path is not "no file" — it is Ceph's search path, which finds
// DefaultCephConf. Printing the empty string there tells an operator with two
// clusters nothing at all.
func (p *CephConfig) ConfPathOrDefault() string {
	if p == nil || p.ConfPath == "" {
		return DefaultCephConf
	}

	return p.ConfPath
}

// CheckCeph refuses a storage block billet cannot safely act on.
//
// EXPORTED AND CALLED FROM BOTH SIDES, like CheckEC2Endpoint and for the same
// reason: the client's constructor is exported and cannot assume its
// configuration came through Load. A rule enforced in only one of the two has a
// second entry point that does not enforce it.
//
// It takes a VALUE, so a caller cannot hand it a nil pointer and read the empty
// result as approval.
func CheckCeph(p CephConfig) []error {
	var errs []error

	// PADDING IS REFUSED HERE RATHER THAN TRIMMED, because this function decides
	// and the CALLER executes. Load has already normalized, so a padded value
	// reaching this point came from a caller that did not — and trimming only for
	// the decision is the exact defect the ec2 block shipped with: `region: "  x  "`
	// passed the shape check and was then signed with its padding. Every field is
	// listed, so adding one to the struct without adding it here is visible.
	for _, f := range []struct{ field, value string }{
		{"user", p.User},
		{"image_pool", p.ImagePool},
		{"cache_pool", p.CachePool},
		{"conf_path", p.ConfPath},
		{"keyring_path", p.KeyringPath},
	} {
		if f.value != strings.TrimSpace(f.value) {
			errs = append(errs, fmt.Errorf("node.ceph.%s %q has leading or trailing whitespace; "+
				"billet passes it to the rbd client exactly as written", f.field, f.value))
		}

		// A NUL CANNOT BE CARRIED BY AN ARGV AT ALL. YAML can encode one, and it
		// passes every shape check here — including filepath.IsAbs — so without this
		// billet accepts a configuration that exec refuses before rbd ever starts,
		// with an error naming neither the field nor the byte.
		if strings.IndexByte(f.value, 0) >= 0 {
			errs = append(errs, fmt.Errorf("node.ceph.%s contains a NUL byte, which cannot be "+
				"passed to a command at all", f.field))
		}
	}

	// admin IS THE DEFAULT THE rbd COMMAND WOULD PICK, which is what makes naming
	// it here worth an error rather than a comment. An admin key can delete a pool;
	// a node holding one turns "this host was compromised" into "the site's storage
	// was deleted", and nothing about the deployment looks different until then.
	//
	// An EMPTY user is refused for the same reason rather than defaulted here: a
	// config loaded through Load has already been given DefaultCephUser, so an empty
	// one reaching this point came from a caller that skipped normalization — and
	// handing rbd no identity makes it pick client.admin.
	if user := strings.TrimSpace(p.User); user == "" || user == "admin" {
		errs = append(errs, fmt.Errorf("node.ceph.user is %q: billet will not authenticate to a "+
			"cluster as an administrator, because that key can delete the pools this node only "+
			"needs to read and clone; create a scoped identity with `ceph auth get-or-create "+
			"client.%s mon 'profile rbd' osd 'profile rbd pool=<images>, profile rbd "+
			"pool=<cache>'`", p.User, DefaultCephUser))
	} else {
		errs = append(errs, checkCephIdentity(user)...)
	}

	errs = append(errs, checkCephPool("image_pool", p.ImagePool)...)
	errs = append(errs, checkCephPool("cache_pool", p.CachePool)...)

	// ONE NAME FOR BOTH is refused rather than merged. A cache is disposable and a
	// golden image is not: eviction has to be able to walk the cache pool, and
	// "delete everything in it" has to stay a thing an operator can do, neither of
	// which survives the images living there too.
	if p.ImagePool != "" && p.ImagePool == p.CachePool {
		errs = append(errs, fmt.Errorf("node.ceph.image_pool and node.ceph.cache_pool are both "+
			"%q: golden images are rebuilt from a recipe and caches are thrown away, so they "+
			"cannot share a pool that eviction is allowed to walk", p.ImagePool))
	}

	errs = append(errs, checkCephPath("conf_path", p.ConfPath)...)
	errs = append(errs, checkCephPath("keyring_path", p.KeyringPath)...)

	return errs
}

// checkCephIdentity refuses an identity that would authenticate as something else.
//
// ONE RULE, AND IT IS THE ONLY ONE THAT SURVIVED BEING MEASURED. rbd prefixes the
// value of --id with `client.` itself, so `--id client.billet` asks the cluster
// about `client.client.billet`: run against a working deployment it answers
// `(13) Permission denied` while the plain form lists the pool, and the error names
// an entity the operator never created.
//
// The shapes that were refused alongside it are not refused any more, because the
// reasons given for them were reasoning rather than measurement — the mistake this
// repo has already made once, with the runner-group validator. `rbd --id -weird`
// does NOT read the value as an option: --id requires a value and program_options
// consumes the next token whatever it starts with, so a leading dash is addressable.
// Whitespace and slashes are addressable too — `ceph auth get-or-create` accepts
// `client.a/b` and `client.a b`, and each is a single argv element. A pool name is
// different, and checkCephPool says why.
func checkCephIdentity(user string) []error {
	if strings.HasPrefix(user, "client.") {
		return []error{fmt.Errorf("node.ceph.user %q carries the `client.` prefix, which rbd adds "+
			"itself: billet would authenticate as client.%s, which is not an entity your cluster "+
			"has", user, user)}
	}

	return nil
}

// checkCephPool refuses a pool name billet cannot address.
//
// EVERY RULE HERE IS PINNED TO MEASURED BEHAVIOUR, because Ceph is more permissive
// than it looks: it creates pools named `a/b`, `a@b`, `a b` and `a\tb` without
// complaint. So the question is never "is this a legal pool name", it is "does
// billet address it correctly", and the answers came from running rbd rather than
// from reading it.
//
//   - `/` and `@` — a pool name is only half of what billet builds. Images are
//     addressed as `pool/image` and snapshots as `pool/image@snap`, so either
//     character makes the spec parse somewhere else.
//   - a leading `-` — and NOT because of `-p`, which consumes the next token
//     whatever it starts with. Those specs are POSITIONAL arguments:
//     `rbd info -weirdpool/nothing` answers `unrecognised option
//     '-weirdpool/nothing'` where the same call on an ordinary pool reaches the
//     image.
//   - a leading `.` — refused by Ceph itself ("pool names beginning with . are not
//     allowed"), so this is billet saying so at load rather than at first launch.
//
// Interior whitespace was refused too and is not any more: it is addressable at
// every layer measured, and a rule whose stated reason is untrue is worse than no
// rule. Padding is a different matter and CheckCeph refuses it.
func checkCephPool(field, pool string) []error {
	if pool == "" {
		return []error{fmt.Errorf("node.ceph.%s is required", field)}
	}

	for _, bad := range []struct {
		what   string
		reason string
	}{
		{"/", "billet addresses an image as pool/image, so a slash points at a different pool"},
		{"@", "billet addresses a snapshot as pool/image@snap, so an @ starts a snapshot name"},
	} {
		if strings.Contains(pool, bad.what) {
			return []error{fmt.Errorf("node.ceph.%s %q contains %q: %s", field, pool, bad.what, bad.reason)}
		}
	}

	if strings.HasPrefix(pool, ".") {
		return []error{fmt.Errorf("node.ceph.%s %q begins with a dot, which ceph reserves for its "+
			"own pools such as .mgr and refuses to create", field, pool)}
	}

	if strings.HasPrefix(pool, "-") {
		return []error{fmt.Errorf("node.ceph.%s %q begins with a dash: billet addresses images as "+
			"positional pool/image arguments, and rbd reads one starting with a dash as an "+
			"option it does not recognise", field, pool)}
	}

	return nil
}

// checkCephPath refuses a relative override.
//
// A NODE IS A SERVICE, and a service's working directory is not the one the
// operator was standing in when they wrote the config. A relative ceph.conf
// resolves against `/` under systemd, so it is found while testing by hand and
// missing in production — which surfaces as a cluster that cannot be reached
// rather than as a path that was wrong.
func checkCephPath(field, path string) []error {
	if path == "" {
		return nil
	}

	if !filepath.IsAbs(path) {
		return []error{fmt.Errorf("node.ceph.%s %q is relative; a node runs as a service, whose "+
			"working directory is not where you wrote this file", field, path)}
	}

	return nil
}

// validateEC2Node reports everything wrong with a cloud node.
func (c *Config) validateEC2Node() []error {
	var errs []error

	// WHAT IT WILL BUY, BECAUSE THERE IS NOTHING TO MEASURE.
	//
	// Every other backend runs jobs on this machine, so an unset contribution
	// means "everything I can detect" and detection answers correctly. An ec2 node
	// calls an API and the compute appears in a region, so detection would report
	// whatever small instance holds this process — and billet would advertise that
	// to GitHub as the capacity of an entire cloud, placing one job and then
	// looking full. That is the failover this backend exists for, silently not
	// working.
	//
	// Required rather than defaulted: billet has no standing to choose how much to
	// buy on somebody's account. Placement charges the declared shape that will be
	// purchased, including a fallback, so these are hard resource budgets rather
	// than estimates made from the smaller tier request.
	if c.Node.MaxVCPU <= 0 {
		errs = append(errs, errors.New(
			"node.max_vcpu is required when provider is ec2: there is no machine to detect it "+
				"from, because the compute this node launches runs in a region rather than on "+
				"this host, and billet will not choose how much to buy on your behalf"))
	}

	if c.Node.MaxMemory <= 0 {
		errs = append(errs, errors.New(
			"node.max_memory is required when provider is ec2: there is no machine to detect it "+
				"from, because the compute this node launches runs in a region rather than on "+
				"this host, and billet will not choose how much to buy on your behalf"))
	}

	if c.Node.EC2 == nil {
		errs = append(errs, errors.New("node.ec2 is required when provider is ec2"))

		return errs
	}

	e := c.Node.EC2

	if err := CheckEC2Region(e.Region); err != nil {
		errs = append(errs, err)
	}

	if strings.TrimSpace(e.SubnetID) == "" {
		errs = append(errs, errors.New("node.ec2.subnet_id is required; billet cannot choose "+
			"which network a runner should be able to reach"))
	}

	if err := CheckEC2Endpoint(e.Endpoint); err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, CheckEC2SecurityGroups("security_group_ids", e.SecurityGroupIDs, true)...)
	errs = append(errs, CheckEC2SecurityGroups(
		"untrusted_security_group_ids", e.UntrustedSecurityGroupIDs, false)...)

	errs = append(errs, e.instanceTypeErrors()...)

	if e.Spot && e.InterruptionQueueURL == "" {
		errs = append(errs, errors.New("node.ec2.interruption_queue_url is required when spot is "+
			"enabled, so a reclaim is recorded before the instance disappears"))
	}
	if !e.Spot && e.InterruptionQueueURL != "" {
		errs = append(errs, errors.New("node.ec2.interruption_queue_url is set while spot is off; "+
			"the queue would be consumed for compute this node never buys"))
	}
	if err := CheckSQSQueueURL(e.InterruptionQueueURL, e.Region); err != nil {
		errs = append(errs, err)
	}
	if c.Node.Name != "" {
		e.NodeName = c.Node.Name
	}
	if e.NodeName != "" {
		if err := CheckSQSQueueNode(e.InterruptionQueueURL, e.NodeName); err != nil {
			errs = append(errs, err)
		}
	}

	return errs
}

// normalize trims the values billet later uses verbatim.
//
// THE SAME REASON NODE NAMES ARE NORMALIZED FIRST. Validation trimmed these to
// CHECK them and everything else used the raw string, so `region: "  us-west-2  "`
// passed the shape check and was then signed with its padding — a 403 naming
// nothing. YAML strips whitespace from a plain scalar but keeps it inside quotes,
// so this is reachable from an ordinary-looking file.
func (e *EC2Config) normalize() {
	if e == nil {
		return
	}

	e.Region = strings.TrimSpace(e.Region)
	e.Endpoint = strings.TrimSpace(e.Endpoint)
	e.SubnetID = strings.TrimSpace(e.SubnetID)
	e.InstanceProfile = strings.TrimSpace(e.InstanceProfile)
	e.InterruptionQueueURL = strings.TrimSpace(e.InterruptionQueueURL)
	e.NodeName = trimNodeName(e.NodeName)

	for i := range e.SecurityGroupIDs {
		e.SecurityGroupIDs[i] = strings.TrimSpace(e.SecurityGroupIDs[i])
	}

	for i := range e.UntrustedSecurityGroupIDs {
		e.UntrustedSecurityGroupIDs[i] = strings.TrimSpace(e.UntrustedSecurityGroupIDs[i])
	}

	for i := range e.InstanceTypes {
		e.InstanceTypes[i].Type = strings.TrimSpace(e.InstanceTypes[i].Type)
	}
}

// CheckSQSQueueURL refuses a warning queue that cannot be signed safely.
func CheckSQSQueueURL(raw, region string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" {
		return errors.New("node.ec2.interruption_queue_url is not a url billet can dial")
	}
	if u.User != nil {
		return errors.New("node.ec2.interruption_queue_url must not carry a username or password")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("node.ec2.interruption_queue_url must not carry a query or fragment")
	}
	if u.Hostname() == "" || u.EscapedPath() == "" || u.EscapedPath() == "/" {
		return errors.New("node.ec2.interruption_queue_url must name a queue path")
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !isLoopbackHost(u.Hostname())) {
		return errors.New("node.ec2.interruption_queue_url must use https; only loopback may use http")
	}
	if isLoopbackHost(u.Hostname()) {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	standard := host == "sqs."+region+".amazonaws.com" ||
		host == "sqs."+region+".amazonaws.com.cn" ||
		host == region+".queue.amazonaws.com"
	private := strings.HasSuffix(host, ".sqs."+region+".vpce.amazonaws.com") ||
		strings.HasSuffix(host, ".sqs."+region+".vpce.amazonaws.com.cn")
	if !standard && !private {
		return fmt.Errorf("node.ec2.interruption_queue_url must name an SQS endpoint in node.ec2.region %q", region)
	}

	return nil
}

// CheckSQSQueueNode makes the one-queue-per-node topology enforceable from
// independently deployed node configs: distinct node names imply distinct queue
// URLs rather than relying on an operator remembering not to share one.
func CheckSQSQueueNode(raw, node string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || pathpkg.Base(u.Path) != node {
		return fmt.Errorf("node.ec2.interruption_queue_url must name a queue whose name is exactly the effective node name %q", node)
	}

	return nil
}

// CheckEC2SecurityGroups refuses a list billet cannot safely launch against.
//
// EXPORTED AND CALLED FROM BOTH SIDES, like CheckEC2Endpoint and for the same
// reason: the provider's constructor is exported and cannot assume its
// configuration came through Load.
//
// BOTH HALVES OF THE RULE LIVE HERE. An earlier version exported only the
// blank-entry half, so the constructor accepted a config with NO trusted group at
// all — and RunInstances without a group lets EC2 pick the VPC's default, which in
// a VPC somebody already had usually permits a good deal more than they are
// picturing. A rule split across two places has an entry point that does not
// enforce it, which is the thing exporting it was meant to fix.
//
// `required` is false for the untrusted list, where EMPTY IS MEANINGFUL: its
// absence is what refuses fork pull-request work.
func CheckEC2SecurityGroups(field string, groups []string, required bool) []error {
	var errs []error

	if required && len(groups) == 0 {
		errs = append(errs, fmt.Errorf("node.ec2.%s needs at least one group; it is what decides "+
			"what a running job can reach, so billet will not fall back to the VPC's default",
			field))
	}

	// EVERY BLANK, not the first: an operator fixing one and re-running to find
	// the next is the failure mode Validate exists to avoid.
	for i, g := range groups {
		if strings.TrimSpace(g) == "" {
			errs = append(errs, fmt.Errorf(
				"node.ec2.%s[%d] is empty; an empty string is not a security group, and on the "+
					"untrusted list a non-empty list is what admits fork pull-request work",
				field, i))
		}
	}

	return errs
}

// CheckEC2Region refuses a region that is not one.
//
// EXPORTED, because a region is not only an address: it is interpolated into the
// DEFAULT ENDPOINT HOST, and it is part of the scope every request is signed with.
// The first of those is why the provider's constructor re-applies it — measured,
// a region of `x@attacker.example/?` produces a default endpoint whose host is
// `attacker.example`, and the signed request and its session token go there.
//
// A SHAPE RATHER THAN A LIST. An allowlist goes stale the next time AWS opens a
// region, and being stale means refusing a config that is correct. The shape
// catches the mistake people make, which is dropping the hyphens, and still
// admits partitions billet has never run in.
func CheckEC2Region(region string) error {
	region = strings.TrimSpace(region)

	if region == "" {
		return errors.New("node.ec2.region is required")
	}

	if !awsRegionRe.MatchString(region) {
		return fmt.Errorf(
			"node.ec2.region %q does not look like an aws region (expected something like "+
				"us-west-2); it is signed into every request and interpolated into the default "+
				"endpoint, so an endpoint override cannot compensate for a typo here", region)
	}

	return nil
}

// CheckEC2Endpoint refuses an endpoint that would carry a credential in the clear
// or send a signed request somewhere billet did not mean.
//
// NOTHING HERE RENDERS THE ENDPOINT, and that is the rule rather than an
// oversight. Every attempt to render it safely was wrong in a new way:
// interpolating it printed a password; wrapping url.Parse's error printed one too,
// because *url.Error embeds the whole URL; and url.Redacted masks only a
// HIERARCHICAL url's password, so it leaves an opaque one
// (`http:alice:secret@host`) and any `?token=` query completely intact. Both
// measured. Naming the field and the failed component tells an operator
// everything they can act on and cannot leak anything.
//
// LOOPBACK IS THE EXCEPTION to https, and it is billet's existing rule rather than
// a new one: a loopback wire has no certificates at all, because there the trust
// boundary is the machine itself.
func CheckEC2Endpoint(endpoint string) error {
	return checkSignedEndpoint("node.ec2.endpoint", endpoint)
}

// CheckBackupEndpoint applies the same rule to the archive store's endpoint.
//
// THE SAME RULE, NOT A SECOND READING OF IT. Both endpoints receive requests
// billet signs and sends a session token with, so both refuse plaintext outside
// loopback for one reason — and a second implementation is a second security
// boundary, which is the argument internal/awssig already makes about having one
// signer rather than two.
func CheckBackupEndpoint(endpoint string) error {
	return checkSignedEndpoint("backup.s3.endpoint", endpoint)
}

func checkSignedEndpoint(field, endpoint string) error {
	if strings.TrimSpace(endpoint) == "" {
		return nil
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("%s is not a url", field)
	}

	if u.Opaque != "" {
		return fmt.Errorf("%s is not a url billet can dial: it has no // and therefore no host, so "+
			"nothing says which machine to sign a request for", field)
	}

	if u.User != nil {
		return fmt.Errorf("%s must not carry a username or password: billet authenticates with a "+
			"request signature, so one would be a credential in a string that gets logged and "+
			"nothing else", field)
	}

	if u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("%s must not carry a query string or fragment: billet builds every request "+
			"itself, so anything there is either ignored or a secret in a value that gets "+
			"logged", field)
	}

	if u.Scheme != "https" && u.Scheme != "http" {
		return endpointNeedsHTTPS(field)
	}

	if u.Hostname() == "" {
		return fmt.Errorf("%s names no host", field)
	}

	// THE SERVICE LIVES AT THE ROOT, for both callers. A path here would be
	// signed and sent, so `https://vpce.example/v1` addresses every call
	// somewhere that is not the service — and no AWS regional, VPC-interface or
	// non-commercial-partition endpoint needs one, nor does an ordinary Ceph RGW
	// or MinIO. Absent and "/" are both the root.
	if path := u.EscapedPath(); path != "" && path != "/" {
		return fmt.Errorf("%s must name a host with no path: billet builds the request path "+
			"itself and signs whatever it is given", field)
	}

	if u.Scheme == "https" || isLoopbackHost(u.Hostname()) {
		return nil
	}

	return endpointNeedsHTTPS(field)
}

// endpointNeedsHTTPS names the rule without naming the value.
func endpointNeedsHTTPS(field string) error {
	return fmt.Errorf("%s must use https: billet signs each request and sends a session token "+
		"with it, so plaintext hands an on-path observer a replayable request. Only a loopback "+
		"address may use http, where the trust boundary is the machine itself", field)
}

// isLoopbackHost reports whether a host names this machine.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// instanceTypeErrors reports everything wrong with the shapes a cloud node may
// buy.
//
// A ZERO IS A FORGOTTEN FIELD, NOT AN UNKNOWN TO BE TOLERATED. These numbers are
// what billet matches an already-escrowed lease against, so a shape that
// understates itself launches a machine smaller than the work reserved for it and
// over-commits a host nobody can inspect.
func (e *EC2Config) instanceTypeErrors() []error {
	errs := CheckRemoteShapes(ProviderEC2, e.InstanceTypes)
	for i := range e.InstanceTypes {
		if e.InstanceTypes[i].PriceUSDPerHour <= 0 {
			errs = append(errs, fmt.Errorf("node.ec2.instance_types[%d]: "+
				"price_usd_per_hour must be more than zero", i))
		}
	}

	return errs
}

// CheckRemoteShapes validates one remote backend's ordered shape catalogue.
//
// Exported because the allocator receives the same catalogue over node
// registration and cannot assume it came through Config.Load.
//
// THE PROVIDER RATHER THAN A FIELD NAME, so the messages name the key the operator
// actually wrote. It used to be `CheckEC2InstanceTypes`, whose prose was hard-coded
// to `node.ec2.instance_types` — which was fine while there was one remote backend
// and became a diagnostic pointing at a field that is not in a codebuild
// operator's file the moment there were two. `ProviderKind.ShapeField` is the one
// place that mapping lives, and the allocator has the registered provider to hand.
func CheckRemoteShapes(p ProviderKind, types []RemoteShape) []error {
	var errs []error
	field := p.ShapeField()

	if len(types) == 0 {
		errs = append(errs, fmt.Errorf("%s needs at least one shape; billet ships no table of "+
			"%s, so a shape it may buy has to be declared along with what it holds",
			field, p.shapeNoun()))

		return errs
	}

	seen := make(map[string]struct{}, len(types))

	for i := range types {
		it := &types[i]
		where := fmt.Sprintf("%s[%d]", field, i)

		if name := strings.TrimSpace(it.Type); name == "" {
			errs = append(errs, fmt.Errorf("%s: type is required", where))
		} else {
			// A repeat is a typo rather than a stronger preference, exactly as it
			// is in a tier's provider list: billet picks one shape per launch, so
			// collapsing a duplicate silently would hide the mistake.
			if _, dup := seen[name]; dup {
				errs = append(errs, fmt.Errorf("%s: type %q is listed twice", where, name))
			}

			seen[name] = struct{}{}
		}

		if it.VCPU <= 0 {
			errs = append(errs, fmt.Errorf("%s: vcpu must be more than zero; it is what billet "+
				"matches an already-reserved lease against", where))
		}

		if it.Memory <= 0 {
			errs = append(errs, fmt.Errorf("%s: memory must be more than zero; it is what "+
				"billet matches an already-reserved lease against", where))
		}
	}

	return errs
}

// codeBuildProjectRe is what AWS documents a project name may contain: letters,
// digits, hyphen and underscore, 2 to 150 characters.
//
// PINNED TO THE DOCUMENTED RULE rather than to a guess about URL safety, which is
// the mistake the runner-group validator made in both directions. The name is
// interpolated into no URL here — it travels in a JSON body — so the only reason to
// constrain it is that a name AWS will refuse should be refused at load rather than
// on the first launch.
var codeBuildProjectRe = regexp.MustCompile(`^[A-Za-z0-9_-]{2,150}$`)

// ssmParameterPathRe is a Parameter Store hierarchy prefix: a leading slash, then
// segments of letters, digits, underscore, dot and hyphen.
//
// NO WILDCARD AND NO TRAILING SLASH, and the wildcard is the one that matters: this
// prefix lands in an IAM Resource ARN, so a `*` or `?` in it widens the node's
// ssm:PutParameter grant to sibling paths — which on a shared account is another
// deployment's runner registrations. The same rule awspolicy states for a cache
// prefix, applied where the value is written.
// reservedSSMNamespace reports whether a parameter path lands in a namespace
// Parameter Store keeps for itself.
//
// CASE-INSENSITIVE, AND MEASURED. The path grammar admits uppercase, so a
// case-sensitive check let `/AWS/billet` through — and AWS refuses it. Asked in
// us-west-2 on 2026-08-31, PutParameter answered:
//
//	/AWS/billet/…  AccessDeniedException: No access to reserved parameter name
//	/Aws/billet/…  AccessDeniedException: No access to reserved parameter name
//	/SSM/billet/…  ValidationException: can't be prefixed with "ssm" (case-insensitive)
//	/aws/billet/…  AccessDeniedException: No access to reserved parameter name
//
// AWS's own message says case-insensitive. Refusing at load is the point: the
// alternative is a config that validates and then fails on every registration, which
// is a tier advertising capacity it cannot serve.
func reservedSSMNamespace(path string) bool {
	lower := strings.ToLower(path)

	return strings.HasPrefix(lower, "/aws") || strings.HasPrefix(lower, "/ssm")
}

var ssmParameterPathRe = regexp.MustCompile(`^(/[A-Za-z0-9_.-]+)+$`)

// CheckSSMParameterPath refuses a Parameter Store prefix that would widen an IAM
// grant or land in a namespace AWS keeps for itself.
//
// EXPORTED BECAUSE THE PATH HAS TWO READERS AND ONE RULE. A codebuild node writes
// registrations under it and its config is checked here at load; the node then
// REPORTS the same path at registration, and the control plane sweeps under it,
// so alloc.RegisterNode re-applies this to a value that arrived over the wire
// rather than through Load (the alloc.New rule). Two spellings of the rule would
// be two spellings that drift.
//
// The message starts with the quoted value so a caller can prefix the config key
// it is checking.
func CheckSSMParameterPath(path string) error {
	switch {
	case path == "":
		return errors.New("must name a Parameter Store path")

	case !ssmParameterPathRe.MatchString(path):
		// THE WILDCARD IS THE DANGEROUS ONE and the message says so, because a `*`
		// here reads as a harmless glob and is a widened IAM Resource: on a shared
		// account the sibling paths it admits are other deployments' runner
		// registrations.
		return fmt.Errorf("%q must be an absolute Parameter Store path with no trailing slash "+
			"and no wildcard (letters, digits, _ . - and / only); it lands in an IAM Resource "+
			"arn, so a * or ? widens the node's parameter grant to paths it should not reach",
			path)

	case reservedSSMNamespace(path):
		return fmt.Errorf("%q starts with a namespace AWS reserves (/aws, /ssm, in any case), "+
			"so PutParameter would refuse every registration", path)
	}

	return nil
}

// codeBuildFleetARNRe is the shape of a reserved-capacity fleet ARN, captured so
// the region can be compared against the one billet signs with.
var codeBuildFleetARNRe = regexp.MustCompile(
	`^arn:[a-z0-9-]+:codebuild:([a-z0-9-]+):[0-9]{12}:fleet/[A-Za-z0-9_-]+(:[0-9a-fA-F-]+)?$`)

// CheckCodeBuildRegion refuses a region that cannot be signed with.
//
// The same rule as CheckEC2Region and for the same reason: the region is signed
// into every request AND interpolated into the default endpoint, so it decides
// which host a signed request reaches. Measured on the ec2 side —
// `x@attacker.example/?` yields a url whose host is `attacker.example`.
func CheckCodeBuildRegion(region string) error {
	region = strings.TrimSpace(region)

	if region == "" {
		return errors.New("node.codebuild.region is required")
	}

	if !awsRegionRe.MatchString(region) {
		return fmt.Errorf(
			"node.codebuild.region %q does not look like an aws region (expected something like "+
				"us-west-2); it is signed into every request and interpolated into the default "+
				"endpoint, so an endpoint override cannot compensate for a typo here", region)
	}

	return nil
}

// CheckCodeBuildEndpoint applies billet's one endpoint rule to this backend's API
// host. See CheckEC2Endpoint for why nothing here renders the value.
func CheckCodeBuildEndpoint(endpoint string) error {
	return checkSignedEndpoint("node.codebuild.endpoint", endpoint)
}

// CheckCodeBuild reports everything wrong with the CodeBuild block.
//
// EXPORTED AND CALLED FROM BOTH SIDES, the alloc.New rule: the provider's
// constructor is exported too, so a caller whose configuration did not come through
// config.Load must not be able to build one that signs requests for somewhere else,
// runs untrusted work, or writes a runner registration outside the path its IAM
// policy was scoped to.
//
// IT DOES NOT CHECK THE CEILING ACKNOWLEDGEMENT, which is deliberately the node
// validator's job: this function is also what `billet check` and the provider
// constructor call, and refusing there would make a diagnostic unusable on a config
// somebody is in the middle of writing. The acknowledgement gates a node that
// SERVES work, which is where validateCodeBuildNode applies it.
func CheckCodeBuild(b CodeBuildConfig) []error {
	var errs []error

	if err := CheckCodeBuildRegion(b.Region); err != nil {
		errs = append(errs, err)
	}

	if err := CheckCodeBuildEndpoint(b.Endpoint); err != nil {
		errs = append(errs, err)
	}

	switch {
	case b.Project == "":
		errs = append(errs, errors.New("node.codebuild.project is required; billet cannot choose "+
			"which project to start builds in, and the project is half of what tells its own "+
			"builds from somebody else's because a CodeBuild build cannot be tagged"))

	case !codeBuildProjectRe.MatchString(b.Project):
		errs = append(errs, fmt.Errorf("node.codebuild.project %q is not a project name AWS "+
			"accepts (letters, digits, - and _, 2 to 150 characters)", b.Project))
	}

	errs = append(errs, checkCodeBuildEnvironment(b)...)
	errs = append(errs, checkCodeBuildFleet(b)...)
	errs = append(errs, checkCodeBuildTimeouts(b)...)
	errs = append(errs, checkCodeBuildJITPath(b)...)
	errs = append(errs, checkCodeBuildComputeTypes(b)...)
	errs = append(errs, checkCodeBuildUntrusted(b)...)

	return errs
}

// checkCodeBuildUntrusted validates the isolated network a fork pull-request build
// runs in.
//
// THE CHECKS HERE ARE STATIC ONLY — internal consistency of the three fields and
// the environments they can never be used on. WHETHER A GIVEN TIER MAY RUN
// UNTRUSTED WORK is decided by the provider's untrustedNetwork, which Accepts and
// the launch path share, because a node config does not know a tier's trust class.
// This layer refuses config that is dead on its face: a network that could never
// admit untrusted work no matter what tier names the node.
func checkCodeBuildUntrusted(b CodeBuildConfig) []error {
	var errs []error

	anySet := b.UntrustedVPCID != "" || len(b.UntrustedSubnetIDs) > 0 ||
		len(b.UntrustedSecurityGroupIDs) > 0
	if !anySet {
		return nil
	}

	// A PARTIAL SET IS THE ec2 use_vpc TRAP: a vpc id with no subnet, or subnets
	// with no group, is a network that cannot be built and would refuse every
	// untrusted launch while looking configured. All three or none.
	if !b.HasUntrustedNetwork() {
		errs = append(errs, errors.New("node.codebuild.untrusted_vpc_id, untrusted_subnets and "+
			"untrusted_security_group_ids must be set together: a fork build's network needs a "+
			"vpc, at least one subnet in it, and at least one security group, and a partial set "+
			"describes an isolation that cannot be built"))
	}

	// A BLANK ENTRY IS NOT AN ENTRY. normalize trims each id, so `[" "]` arrives here
	// as `[""]` — non-empty by length and naming nothing. It passed HasUntrustedNetwork
	// and Accepts, and failed only on the first fork job when the project comparison
	// found no such subnet: a config that loads and cannot work, which is the shape
	// Validate exists to refuse.
	for i, s := range b.UntrustedSubnetIDs {
		if s == "" {
			errs = append(errs, fmt.Errorf("node.codebuild.untrusted_subnets[%d] is blank", i))
		}
	}

	for i, g := range b.UntrustedSecurityGroupIDs {
		if g == "" {
			errs = append(errs, fmt.Errorf("node.codebuild.untrusted_security_group_ids[%d] is blank", i))
		}
	}

	// AN UNTRUSTED NETWORK ON A NODE THAT CAN NEVER RUN UNTRUSTED WORK IS DEAD
	// CONFIG, and the two ways to get there are the two the provider refuses at
	// launch — stated here too so the refusal lands at load rather than on the
	// first fork job, which is the ADR-005 direction.
	if b.FleetARN != "" {
		errs = append(errs, errors.New("node.codebuild names an untrusted network beside "+
			"fleet_arn, but untrusted work is refused on a reserved-capacity fleet: a "+
			"fleetOverride discards the project's vpc, and a reserved instance is shared "+
			"between builds, so no network isolates a fork's code from the next build. Drop "+
			"the untrusted_* fields or drop fleet_arn"))
	}

	if b.EnvironmentType.Valid() && !b.EnvironmentType.Container() {
		errs = append(errs, fmt.Errorf("node.codebuild names an untrusted network but "+
			"environment_type %s runs directly on a reserved-capacity machine rather than in a "+
			"per-build container, so untrusted work is refused there whatever the network says; "+
			"untrusted CodeBuild work needs an on-demand container environment "+
			"(LINUX_CONTAINER or ARM_CONTAINER)", b.EnvironmentType))
	}

	return errs
}

// checkCodeBuildEnvironment refuses an environment billet cannot run a GitHub
// Actions job in, and says why for each excluded one rather than listing the
// accepted set and leaving an operator to guess which of theirs is missing.
func checkCodeBuildEnvironment(b CodeBuildConfig) []error {
	if b.EnvironmentType == "" {
		return []error{errors.New("node.codebuild.environment_type is required; it decides " +
			"whether this node runs Linux or macOS builds, and billet reports that guest OS at " +
			"registration rather than taking a second answer from the config")}
	}

	if b.EnvironmentType.Valid() {
		// PRIVILEGE IS REFUSED WHERE IT MEANS NOTHING rather than ignored, the same
		// rule as node.ceph on a container backend: a build running directly on an
		// EC2 instance or a Mac IS the machine, so there is nothing to privilege,
		// and a setting that reads as "Docker will work" and does nothing is worse
		// than its absence.
		if b.PrivilegedMode && !b.EnvironmentType.Container() {
			return []error{fmt.Errorf("node.codebuild.privileged_mode is set but environment_type "+
				"%s runs the job directly on the machine rather than in a container, so there is "+
				"no container privilege to grant", b.EnvironmentType)}
		}

		return nil
	}

	// NAMED EXCLUSIONS, because both are things an operator would reasonably try.
	upper := strings.ToUpper(string(b.EnvironmentType))

	switch {
	case strings.Contains(upper, "LAMBDA"):
		return []error{fmt.Errorf("node.codebuild.environment_type %s cannot run a GitHub Actions "+
			"job: Lambda compute offers no container privilege, so `docker build`, service "+
			"containers and `docker compose` all fail — which is the same reason "+
			"docs/adr-002-cloud-compute-backend.md disqualified Lambda outright",
			b.EnvironmentType)}

	case strings.Contains(upper, "WINDOWS"):
		return []error{fmt.Errorf("node.codebuild.environment_type %s names a Windows environment, "+
			"and billet ships no Windows runner image or runner entrypoint, so a build would "+
			"start and register nothing", b.EnvironmentType)}

	default:
		return []error{fmt.Errorf("node.codebuild.environment_type %q is not one of %v",
			b.EnvironmentType, codeBuildEnvironments)}
	}
}

// checkCodeBuildFleet refuses a fleet that cannot serve this node's builds, and an
// environment that has no on-demand form asking for one.
func checkCodeBuildFleet(b CodeBuildConfig) []error {
	var errs []error

	if b.FleetARN == "" {
		// MEASURED AGAINST AWS'S OWN DOCUMENTATION rather than assumed: on-demand
		// fleets do not offer macOS at all, and LINUX_EC2/ARM_EC2 ("instance running
		// mode") exist only on reserved capacity. Without this the launch is refused
		// by CodeBuild per job, which reads as a transient failure rather than as a
		// config that can never work.
		if b.EnvironmentType.Valid() && b.EnvironmentType.ReservedOnly() {
			errs = append(errs, fmt.Errorf("node.codebuild.fleet_arn is required with "+
				"environment_type %s: that environment exists only on reserved capacity, and "+
				"on-demand CodeBuild does not offer it", b.EnvironmentType))
		}

		return errs
	}

	match := codeBuildFleetARNRe.FindStringSubmatch(b.FleetARN)
	if match == nil {
		errs = append(errs, fmt.Errorf("node.codebuild.fleet_arn %q is not a codebuild fleet arn "+
			"(expected arn:<partition>:codebuild:<region>:<account>:fleet/<name>)", b.FleetARN))

		return errs
	}

	// A FLEET IN ANOTHER REGION CANNOT SERVE THESE BUILDS, and the failure without
	// this check is a per-job refusal from an API call billet signed for a different
	// region than the fleet lives in — which names neither field.
	if region := strings.TrimSpace(b.Region); region != "" && match[1] != region {
		errs = append(errs, fmt.Errorf("node.codebuild.fleet_arn names region %q but "+
			"node.codebuild.region is %q; a fleet serves builds only in its own region",
			match[1], region))
	}

	return errs
}

// checkCodeBuildTimeouts refuses a ceiling CodeBuild would reject, and says which
// of the two limits is external.
func checkCodeBuildTimeouts(b CodeBuildConfig) []error {
	var errs []error

	if b.BuildTimeoutMinutes != 0 &&
		(b.BuildTimeoutMinutes < CodeBuildBuildFloorMinutes ||
			b.BuildTimeoutMinutes > CodeBuildBuildCeilingMinutes) {
		errs = append(errs, fmt.Errorf("node.codebuild.build_timeout_minutes is %d; CodeBuild "+
			"accepts %d to %d (36 hours), and that ceiling is the service's rather than billet's "+
			"— a job that needs longer belongs on owned EC2 or Mac capacity",
			b.BuildTimeoutMinutes, CodeBuildBuildFloorMinutes, CodeBuildBuildCeilingMinutes))
	}

	if b.QueuedTimeoutMinutes != 0 &&
		(b.QueuedTimeoutMinutes < CodeBuildQueuedFloorMinutes ||
			b.QueuedTimeoutMinutes > CodeBuildQueuedCeilingMinutes) {
		errs = append(errs, fmt.Errorf("node.codebuild.queued_timeout_minutes is %d; CodeBuild "+
			"accepts %d to %d (8 hours), after which it FAILS a build that never got a machine",
			b.QueuedTimeoutMinutes, CodeBuildQueuedFloorMinutes, CodeBuildQueuedCeilingMinutes))
	}

	return errs
}

// checkCodeBuildJITPath refuses a parameter prefix that would widen an IAM grant or
// collide with a reserved namespace.
func checkCodeBuildJITPath(b CodeBuildConfig) []error {
	var errs []error

	switch b.JITParameterPath {
	case "":
		errs = append(errs, errors.New("node.codebuild.jit_parameter_path is required: each "+
			"build's single-use runner registration is written to Parameter Store under it, and "+
			"the node's IAM policy is scoped to exactly that path — so billet guessing one "+
			"would either be unwritable or wider than the grant you reviewed"))

	default:
		if err := CheckSSMParameterPath(b.JITParameterPath); err != nil {
			errs = append(errs, fmt.Errorf("node.codebuild.jit_parameter_path %w", err))
		}
	}

	// A key may be named by id, arn or alias, and all three are legitimate — what is
	// refused is a wildcard, for the reason above: it reaches an IAM Resource.
	if b.JITKMSKeyID != "" &&
		(strings.ContainsAny(b.JITKMSKeyID, "*?") || strings.ContainsAny(b.JITKMSKeyID, " \t\n")) {
		errs = append(errs, fmt.Errorf("node.codebuild.jit_kms_key_id %q must be one exact key "+
			"id, arn or alias/<name>; a wildcard would widen the node's KMS grant to every key "+
			"it matches", b.JITKMSKeyID))
	}

	return errs
}

// checkCodeBuildComputeTypes validates the ordered shape catalogue and its prices.
func checkCodeBuildComputeTypes(b CodeBuildConfig) []error {
	errs := CheckRemoteShapes(ProviderCodeBuild, b.ComputeTypes)

	for i := range b.ComputeTypes {
		if b.ComputeTypes[i].PriceUSDPerHour <= 0 {
			errs = append(errs, fmt.Errorf("node.codebuild.compute_types[%d]: "+
				"price_usd_per_hour must be more than zero", i))
		}

		// A COMPUTE TYPE THAT CANNOT RUN DOCKER IS NOT A CHEAPER OPTION, and this is
		// the compute-type half of the environment-type refusal above: the two are
		// separate fields and a Lambda compute type on a container environment is
		// just as unable to run a job.
		if strings.Contains(strings.ToUpper(b.ComputeTypes[i].Type), "LAMBDA") {
			errs = append(errs, fmt.Errorf("node.codebuild.compute_types[%d]: %q is Lambda "+
				"compute, which offers no container privilege, so `docker build` and service "+
				"containers fail — a tier on it would be admitted and then fail every job",
				i, b.ComputeTypes[i].Type))
		}
	}

	return errs
}

// validateCodeBuildNode applies the rules that depend on the node as a whole:
// whether this block belongs here at all, whether the host declared what it will
// buy, and whether somebody has acknowledged the ceilings their jobs inherit.
func (c *Config) validateCodeBuildNode() []error {
	// One diagnostic for a provider that is not a provider. validateNode already
	// refuses an unknown backend by name, and a second error asserting something
	// about a string billet does not recognise is noise — the same reason
	// validateCephNode returns early.
	if !c.Node.Provider.Valid() {
		return nil
	}

	if c.Node.Provider != ProviderCodeBuild {
		if c.Node.CodeBuild != nil {
			return []error{fmt.Errorf("node.codebuild is set but this node's provider is %s, and "+
				"only codebuild reads it, so this host would carry a project, a fleet and a "+
				"parameter path that nothing consults", c.Node.Provider)}
		}

		return nil
	}

	var errs []error

	// WHAT IT WILL BUY, BECAUSE THERE IS NOTHING TO MEASURE — the ec2 rule, for the
	// ec2 reason. A codebuild node calls an API and the build appears in a region,
	// so detection would report whatever small machine holds this process and billet
	// would advertise that as the capacity of a managed fleet.
	if c.Node.MaxVCPU <= 0 {
		errs = append(errs, errors.New(
			"node.max_vcpu is required when provider is codebuild: there is no machine to detect "+
				"it from, because the builds this node starts run in a region rather than on this "+
				"host, and billet will not choose how much to buy on your behalf"))
	}

	if c.Node.MaxMemory <= 0 {
		errs = append(errs, errors.New(
			"node.max_memory is required when provider is codebuild: there is no machine to "+
				"detect it from, because the builds this node starts run in a region rather than "+
				"on this host, and billet will not choose how much to buy on your behalf"))
	}

	// A SITE IS A CACHE AUTHORITY, AND A BUILD ATTACHES TO NONE. Every declared
	// site names a store (ceph or ebs-s3), and the control plane refuses a node
	// whose provider cannot use that store — so a codebuild node naming one is
	// refused at REGISTRATION, with a message about splitting a site across cache
	// authorities. That is true and it is not the first thing an operator needs to
	// hear, and it arrives after the config loaded cleanly. Said here instead, where
	// the file is being written. The registration check stays: this file may be
	// the node's alone, and the control plane is the authority on what a site is.
	if c.Node.Site != "" {
		errs = append(errs, fmt.Errorf("node.site is set but this node's provider is codebuild; "+
			"a site is a cache authority (ceph or ebs-s3) and a CodeBuild build attaches to "+
			"neither, so the control plane would refuse this registration — remove node.site"))
	}

	if c.Node.CodeBuild == nil {
		errs = append(errs, errors.New("node.codebuild is required when provider is codebuild"))

		return errs
	}

	errs = append(errs, CheckCodeBuild(*c.Node.CodeBuild)...)

	// THE ACKNOWLEDGEMENT IS CHECKED HERE AND NOWHERE ELSE, because it gates a node
	// that will SERVE work rather than a configuration that can be inspected. It
	// changes nothing about how billet behaves; it exists so the sentence is read by
	// a person before a tier advertises capacity.
	if !c.Node.CodeBuild.AcceptExternalBuildCeiling {
		errs = append(errs, fmt.Errorf(
			"node.codebuild.accept_external_build_ceiling must be set to true: every job on this "+
				"node inherits CodeBuild's own limits, which billet cannot lift — a build is "+
				"capped at %d minutes (36 hours) and a build still waiting for capacity is FAILED "+
				"after %d minutes (8 hours). billet adds no deadline of its own and no drain or "+
				"upgrade ever stops a build for taking too long, but it will not advertise a tier "+
				"whose ceiling nobody has acknowledged. Work that can exceed either limit belongs "+
				"on owned EC2 or Mac capacity; see docs/codebuild.md",
			CodeBuildBuildCeilingMinutes, CodeBuildQueuedCeilingMinutes))
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

		// Rejected HERE rather than left for GitHub to answer confusingly: an unescaped
		// "Platform & Security" comes back as "group not found", which reads as a
		// permissions problem.
		if err := checkRunnerGroup(t.RunnerGroup); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", where, err))
		}
		seen[t.Label] = struct{}{}

		errs = append(errs, t.PoolPolicyErrors(where)...)

		errs = append(errs, t.ProviderErrors(where)...)
		errs = append(errs, t.InterceptionErrors(where)...)
		errs = append(errs, t.ReservationErrors(where)...)
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
		if t.BuildKitCacheMountLimit <= 0 ||
			t.BuildKitCacheMountLimit > MaxBuildKitCacheMountLimit {
			errs = append(errs, fmt.Errorf("%s: buildkit_cache_mount_limit must be more than zero "+
				"and no larger than 100GiB", where))
		}
		errs = append(errs, t.LaunchErrors(where)...)
		if t.WarmPool < 0 {
			errs = append(errs, fmt.Errorf("%s: warm_pool must not be negative", where))
		} else if t.WarmPool > 0 {
			errs = append(errs, fmt.Errorf("%s: warm_pool is not implemented; accepting it would "+
				"report pre-booted capacity while every job still pays a cold launch", where))
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
	// THE FLOORS MUST FIT TOGETHER, and this must live here as well as in the
	// allocator: `billet check` only runs config validation, so with the check in
	// alloc.New alone it would report a broken configuration as valid.
	if c.Server != nil {
		errs = append(errs, c.floorFitErrors()...)
	}

	return errs
}

// floorFitErrors reports reservations that cannot all be honoured at once.
//
// The failure is invisible where it happens: every tier deducts every other tier's
// unmet floor, so floors exceeding the budget make EVERY tier compute zero
// headroom and the whole deployment quietly advertise nothing.
//
// Checked by division rather than multiplication: `reserved * vcpu` is unchecked
// arithmetic on a config-supplied number, and a large enough one wraps negative,
// at which point a "does it fit" test passes comfortably.
func (c *Config) floorFitErrors() []error {
	// A non-positive budget is already reported against the field that holds it, and
	// checking floors against it produces a fabricated diagnostic per reservation.
	//
	// GUARDED ONCE, HERE, and not again inside the loop: the remaining budget
	// legitimately reaches zero once earlier tiers have taken it all, and a further
	// reservation MUST be reported at that point.
	if c.Server.MaxVCPU <= 0 || c.Server.MaxMemory <= 0 {
		return nil
	}

	remainingVCPU := c.Server.MaxVCPU
	remainingMemory := c.Server.MaxMemory

	var errs []error

	for i := range c.Tiers {
		t := &c.Tiers[i]

		// Skip anything already reported: a tier with a bad size or a negative
		// reservation would otherwise produce a second, stranger diagnostic.
		if t.Reserved <= 0 || t.VCPU <= 0 || t.Memory <= 0 {
			continue
		}

		if t.Reserved > remainingVCPU/t.VCPU {
			errs = append(errs, fmt.Errorf(
				"tiers[%d] (%s): reserved %d needs more than the %d vCPU left after the other "+
					"tiers' reservations; every tier would then compute zero headroom and "+
					"billet would advertise no capacity at all",
				i, t.Label, t.Reserved, remainingVCPU))

			continue
		}

		if ByteSize(t.Reserved) > remainingMemory/t.Memory {
			errs = append(errs, fmt.Errorf(
				"tiers[%d] (%s): reserved %d needs more than the %s left after the other "+
					"tiers' reservations; every tier would then compute zero headroom and "+
					"billet would advertise no capacity at all",
				i, t.Label, t.Reserved, remainingMemory))

			continue
		}

		remainingVCPU -= t.Reserved * t.VCPU
		remainingMemory -= ByteSize(t.Reserved) * t.Memory
	}

	return errs
}

func (c *Config) validateGuestOSRules(where string, t *Tier) []error {
	var errs []error

	// Relational checks are skipped when either side carries an invalid enum value:
	// the value is already reported, and comparing a typo against an allowlist
	// produces a second diagnostic pointing at the wrong field.
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
		// ACCEPTS, not equals: a tier written with `providers:` leaves the singular field
		// empty, so comparing it would let a plural tier pinned to a host it can never
		// bind to load cleanly — and the failure would surface as a job that queues
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
		// constrains it even though it names no host. Checking only pinned tiers would
		// leave the allowlist bypassable.
		//
		// The predicate is "could this tier actually land here", not guest OS alone: a
		// firecracker tier can never run on a Tart host, so a bare guest-OS comparison
		// would make declaring one macOS-only Mac an error for every x64 Linux tier.
		// Silence is not safety — a node declaring no provider cannot be reasoned about
		// here, and the allocator enforces the allowlist again at Bind.
		//
		// Only the first offending host is reported: one unpinned tier against five
		// restrictive hosts is one mistake, not five.
		for i := range c.Nodes {
			p := &c.Nodes[i]
			if !p.policyEnumsValid() {
				continue
			}

			// Again ACCEPTS rather than equals: comparing the empty singular field would mean
			// a list containing tart was never checked against a macOS-only Mac.
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
		// EVERY listed provider must be one that can serve macOS, not merely one of
		// them: a macOS tier that could fall back to a Linux backend is the case
		// nobody is watching when it happens.
		errs = append(errs, t.GuestOSProviderErrors(where)...)

		// THE HYPERVISOR'S OWN FLOOR, refused at load rather than at launch.
		// Under it Virtualization.framework answers LessThanMinimalResourcesError
		// and the guest never boots — so without this the tier validates, the
		// node registers, capacity is advertised, and every job on that label
		// fails at the point where the least is known about why.
		if t.Memory > 0 && t.Memory < MinMacOSGuestMemory {
			errs = append(errs, fmt.Errorf(
				"%s: guest_os macos needs at least %s of memory, and this asks for %s; "+
					"Apple's hypervisor refuses a smaller macOS guest outright, so the tier "+
					"would advertise capacity no job could ever start on",
				where, MinMacOSGuestMemory, t.Memory))
		}

		if t.Node == "" {
			errs = append(errs, c.validateUnpinnedMacOSTier(where, t)...)

			break
		}

		// A MANAGED FLEET'S CAP IS NOT APPLE'S PER-HOST ALLOWANCE, and billet cannot
		// derive it. `tart` runs macOS on hardware somebody owns, so an unset limit
		// meaning "Apple's two per host" is right there and is what MacOSLimit
		// answers. A remote backend reaches macOS through a fleet its provider
		// operates under its OWN agreement, with a capacity billet has no way to
		// see — so silence there would cap a five-instance fleet at two and blame
		// Apple in the diagnostic, which sends an operator to argue with the wrong
		// party. The number is asked for instead of invented.
		if p, declared := c.NodePolicyFor(t.Node); p.MacOSVMLimit == nil {
			// A NODE THE TIERS DESCRIBE TWO WAYS. One pinned tier could run on a
			// Mac somebody owns and another on an AWS fleet, the policy names no
			// provider, and no node block says: the limit's meaning depends on
			// which, so the provider is asked for rather than guessed.
			if _, ambiguous := c.macOSNodeProvider(t.Node, declared, p); ambiguous {
				errs = append(errs, fmt.Errorf(
					"%s: node %q is pinned by macOS tiers on both a host-backed and a remote "+
						"backend and declares no provider, so billet cannot tell whether an "+
						"unset macos_vm_limit means Apple's per-host allowance or a fleet's "+
						"capacity; set nodes[].provider for it", where, t.Node))

				break
			}

			if remote := c.macOSHostProvider(t.Node, declared, p); remote != "" {
				errs = append(errs, fmt.Errorf(
					"%s: node %q runs %s, which reaches macOS through a managed fleet rather than "+
						"Apple hardware you own, so set macos_vm_limit for it to that fleet's "+
						"capacity; billet will not assume Apple's per-host allowance of %d applies "+
						"to a fleet %s operates", where, t.Node, remote, DefaultMacOSVMLimit, remote))

				break
			}
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

// validateUnpinnedMacOSTier is the one shape a macOS tier may take without a
// node: SEVERAL backends behind one label, so that a Mac somebody owns fills
// first and a managed fleet takes the overflow — the topology a single pin
// cannot express, and the one docs/aws-acceptance.md measures.
//
// The pin was never the enforcement point. Placement counts macOS guests per
// host and refuses a host past its limit whether or not the tier named it; what
// the pin gave the load-time guard was ONE host to check the limit and the tier's
// concurrency against. Without it, the hosts are the declared node policies for
// the tier's backends, and the guard holds each of them to the pinned tier's own
// rules: a remote backend must declare its fleet's capacity rather than inherit
// Apple's number, and the tier's max_concurrent is bounded by what those hosts
// permit between them. A single-backend macOS tier keeps the pin, because there
// nothing is gained by leaving the host unnamed and the count is easier to read.
//
// WHAT AN UNDECLARED HOST GETS is Apple's default, which the allocator applies to
// any node it has no policy for. For a Mac somebody owns that is the licence;
// for a fleet it is a cap below the truth, which under-uses the fleet and never
// overcommits it. And a reservation stays pinned, because a floor is held on ONE
// host's licence and there is no host to hold it on.
func (c *Config) validateUnpinnedMacOSTier(where string, t *Tier) []error {
	providers := t.AcceptableProviders()
	if len(providers) < 2 {
		return []error{fmt.Errorf(
			"%s: guest_os macos requires an explicit node, so the per-host licence limit can be "+
				"enforced; only a tier listing several providers may leave it unpinned", where)}
	}

	var errs []error
	if t.Reserved > 0 {
		errs = append(errs, fmt.Errorf(
			"%s: an unpinned macOS tier cannot reserve guests; a reservation is held against one "+
				"host's licence, so pin the tier to reserve on it", where))
	}

	total := 0
	for _, provider := range providers {
		hosts, limitless := c.macOSHostsFor(provider)
		for _, name := range limitless {
			errs = append(errs, fmt.Errorf(
				"%s: node %q runs %s, which reaches macOS through a managed fleet rather than "+
					"Apple hardware you own, so set macos_vm_limit for it to that fleet's "+
					"capacity; billet will not assume Apple's per-host allowance of %d applies "+
					"to a fleet %s operates", where, name, provider, DefaultMacOSVMLimit, provider))
		}
		if len(hosts) == 0 && len(limitless) == 0 {
			errs = append(errs, fmt.Errorf(
				"%s: no nodes[] entry declares a %s host with macOS capacity above zero, and an "+
					"unpinned macOS tier is counted against the hosts its backends declare; add "+
					"one (with macos_vm_limit, for a managed fleet)", where, provider))
		}
		for _, limit := range hosts {
			total += limit
		}
	}

	if total > 0 && (t.MaxConcurrent <= 0 || t.MaxConcurrent > total) {
		errs = append(errs, fmt.Errorf(
			"%s: max_concurrent must be between 1 and %d, the macOS guests the declared %v "+
				"hosts permit between them", where, total, providers))
	}

	return errs
}

// macOSHostsFor lists the declared hosts running one backend that may serve
// macOS, by their effective limit, and separately the remote ones that declare
// no limit — which get no number here, because Apple's default is not theirs.
func (c *Config) macOSHostsFor(provider ProviderKind) (map[string]int, []string) {
	hosts := map[string]int{}
	var limitless []string
	for i := range c.Nodes {
		p := &c.Nodes[i]
		if !p.policyEnumsValid() || p.Provider != provider || !p.AllowsGuestOS(GuestMacOS) {
			continue
		}
		if p.MacOSVMLimit == nil && !provider.RunsOnHost() {
			limitless = append(limitless, p.Name)

			continue
		}
		if limit := p.MacOSLimit(); limit > 0 {
			hosts[p.Name] = limit
		}
	}

	return hosts, limitless
}

// macOSUnpinnedLimit is what an unpinned macOS tier's max_concurrent defaults
// to: the guests its backends' declared hosts permit between them.
func (c *Config) macOSUnpinnedLimit(t *Tier) int {
	total := 0
	for _, provider := range t.AcceptableProviders() {
		hosts, _ := c.macOSHostsFor(provider)
		for _, limit := range hosts {
			total += limit
		}
	}

	return total
}

// macOSLimitReason explains where a host's macOS limit came from, so a
// diagnostic says why the number is what it is. An operator who has not touched
// the setting needs to know the constraint is Apple's licence rather than a
// billet default they can simply raise; an operator who set it themselves needs
// to be pointed at their own field, not at Apple.
//
// AND APPLE IS NAMED ONLY WHEN APPLE IS THE PARTY. A managed fleet's capacity is
// its provider's, not Apple's per-host allowance, so naming Apple for a remote
// backend sends the operator to argue with a licence that does not bind them. The
// remote case should be unreachable — validateGuestOSRules requires an explicit
// limit there — but this function is also called from an aggregate check, and a
// diagnostic whose stated reason is untrue is worse than no reason.
func (c *Config) macOSLimitReason(node string) string {
	p, declared := c.NodePolicyFor(node)
	if declared && p.MacOSVMLimit != nil {
		return fmt.Sprintf("the macos_vm_limit set for node %q", node)
	}

	if remote := c.macOSHostProvider(node, declared, p); remote != "" {
		return fmt.Sprintf(
			"billet's default of %d, which is Apple's per-host allowance rather than anything %s "+
				"imposes; set macos_vm_limit for node %q to that fleet's capacity",
			DefaultMacOSVMLimit, remote, node)
	}

	return fmt.Sprintf(
		"Apple's licence limit of %d macOS guests per Apple-branded host (node %q does not override it)",
		DefaultMacOSVMLimit, node)
}

// macOSHostProvider reports the REMOTE backend a macOS host runs, or empty when
// macOS there is Apple hardware somebody owns (or when nothing says).
//
// TWO PLACES CAN ANSWER AND THEY ARE NOT THE SAME QUESTION. A fleet entry's own
// `provider:` is the deployment describing that host, and it is the only answer
// available for a machine this config is not itself the node section of. Falling
// back to the LOCAL node's provider is right on a single-box deployment — where
// applyDefaults has already stamped it onto the matching entry — and is checked by
// name so a multi-host file cannot attribute the EPYC box's backend to a Mac.
//
// Returning empty for "nothing says" is the safe direction: it keeps Apple's
// allowance as the default, which is a refusal rather than a licence billet
// invented.
func (c *Config) macOSHostProvider(node string, declared bool, p NodePolicy) ProviderKind {
	kind, _ := c.macOSNodeProvider(node, declared, p)
	if kind.Valid() && !kind.RunsOnHost() {
		return kind
	}

	return ""
}

// macOSNodeProvider is the backend a macOS-serving node runs, from the three
// places that can say: the node's policy, the local node block, and failing both
// the macOS tiers pinned to it. The second result is true when the tiers were
// consulted and DISAGREE — some host-backed, some remote — so nothing can say
// which agreement the limit falls under.
//
// THE TIERS ARE A VALID SOURCE, and the first version did not ask them. A
// server-only config carries no node block, and nodes[].provider is optional; a
// policy written as `{name: cb}` under a tier `provider: codebuild, node: cb`
// therefore resolved to no provider, was read as a Mac somebody owns, and
// inherited Apple's two — advertising two jobs against a one-Mac fleet, which is
// the exact queue-and-requeue failure docs/aws-acceptance.md measures. A macOS
// tier's providers are all in macOSProviders, and a tier pinned to a node can
// only ever be placed on a node running one of them, so a tier that accepts only
// remote backends has said what the node is.
func (c *Config) macOSNodeProvider(node string, declared bool, p NodePolicy) (ProviderKind, bool) {
	kind := p.Provider
	if !declared || kind == "" {
		if c.Node != nil && c.Node.Name == node {
			kind = c.Node.Provider
		}
	}

	if kind != "" {
		return kind, false
	}

	var remote, host ProviderKind

	for i := range c.Tiers {
		t := &c.Tiers[i]
		if t.GuestOS != GuestMacOS || t.Node != node {
			continue
		}

		for _, kind := range t.AcceptableProviders() {
			if kind.RunsOnHost() {
				host = kind
			} else {
				remote = kind
			}
		}
	}

	switch {
	case remote != "" && host != "":
		return "", true
	case remote != "":
		return remote, false
	default:
		return host, false
	}
}

// MacOSFleetProvider names the remote backend a node reaches macOS through, or
// "" for a Mac somebody owns (or a node nothing describes).
//
// Exported for `billet check`, whose policy line has to say which agreement a
// missing macos_vm_limit falls under rather than attribute Apple's number to a
// fleet AWS operates.
func (c *Config) MacOSFleetProvider(node string) ProviderKind {
	p, declared := c.NodePolicyFor(node)

	return c.macOSHostProvider(node, declared, p)
}

// validateSites checks the declared places, and everything that refers to one.
//
// FAIL CLOSED ON A NAME THAT WAS NEVER DECLARED, which is the whole reason a
// site is a declared block. A free string cannot tell a typo from a new place,
// so "hom" would become a site of its own with an empty cache, and every job
// there would run cold while the deployment looked healthy. There is no signal
// after startup that says which of those two an operator meant, so this is the
// only moment it can be caught.
func (c *Config) validateSites() []error {
	var errs []error

	declared := make(map[string]bool, len(c.Sites))

	// refused holds the name an operator MEANT by a declaration billet turned
	// down for its padding, so a reference to it is not reported a second time
	// as undeclared. Without it, one stray space produced a diagnostic saying
	// "this config declares no sites" about a config that declares one, which
	// sends the operator to add a block they already wrote.
	//
	// The name still does not enter declared: nothing may validate against a
	// spelling no node can ever present.
	refused := make(map[string]bool)

	for i, s := range c.Sites {
		// THE NAME IS TAKEN EXACTLY AS WRITTEN, and this loop used to trim it.
		// That trim was the bug: nothing wrote the trimmed value back, so
		// validation authorised `home` while nodeplane.WithSites keyed its
		// authority map — and alloc's placement compared — the padded original.
		// A site declared as " home " therefore accepted `tiers[].site: home` at
		// load and then refused every node reporting `home`, permanently.
		name := s.Name

		if strings.TrimSpace(name) == "" {
			errs = append(errs, fmt.Errorf("sites[%d]: a site must have a name; nothing can "+
				"refer to one without it", i))

			continue
		}

		switch err := checkIdentityPadding(fmt.Sprintf("sites[%d]: site name", i), name); {
		case err != nil:
			errs = append(errs, err)
			refused[strings.TrimSpace(name)] = true
		case declared[name]:
			errs = append(errs, fmt.Errorf("sites[%d]: site %q is declared twice; a site is "+
				"where a cache lives, so two answers to that is one too many", i, name))

			continue
		default:
			declared[name] = true
		}

		// THE STORE IS ITS OWN FIELD AND ITS OWN MISTAKE. A padded name does not
		// make `store: magic-disk` any less wrong, and reporting them one load
		// at a time costs an edit cycle for a file whose problems were both
		// visible at once — which is the thing Validate exists to avoid.
		if !s.Store.Valid() {
			errs = append(errs, fmt.Errorf("sites[%d] (%s): store %q is not one of ceph or "+
				"ebs-s3; storage is selected per site and cannot be inferred from whichever "+
				"node registered first", i, name, s.Store))
		}
	}

	// NODE.SITE IS NOT CHECKED HERE, and that is not an omission.
	//
	// This file may be the NODE's, on another machine, where a sites block has no
	// reason to exist — sites are the control plane's to declare. Checking it
	// against a local block would refuse exactly the deployment this feature is
	// for: a node that correctly names one of the server's places, in a config
	// that has never heard of them.
	//
	// The claim is checked where the answer lives, when the node registers. See
	// nodeplane.WithSites.
	for i := range c.Tiers {
		where := fmt.Sprintf("tiers[%d] (%s): site", i, c.Tiers[i].Label)

		// A SITED TIER CAN NEVER REACH A CODEBUILD NODE, AND THE SYMPTOM IS SILENCE.
		// Placement confines a sited tier to hosts AT that site, and a codebuild
		// node declares none (see validateCodeBuildNode) — so a tier listing
		// codebuild beside a site has a provider that is never eligible for it.
		// Nothing refuses that at runtime: the fallback simply never fires, and a
		// tier whose only provider is codebuild advertises 0 while `billet check`
		// reports everything healthy. Measured on the first live acceptance run,
		// where the job queued with no line saying why.
		if c.Tiers[i].Site != "" &&
			slices.Contains(c.Tiers[i].AcceptableProviders(), ProviderCodeBuild) {
			errs = append(errs, fmt.Errorf("%s %q confines this tier to nodes at that site, "+
				"but its providers include codebuild, and a codebuild node cannot declare a "+
				"site (a build attaches to no cache authority) — so codebuild could never be "+
				"placed for this tier and the fallback would silently never fire; drop the "+
				"site or drop codebuild from providers", where, c.Tiers[i].Site))
		}

		// Padding first, and instead of the membership check: a padded reference
		// is not a declared site either, and reporting both sends the operator
		// looking for a missing sites entry when the answer is two spaces.
		if err := checkIdentityPadding(where, c.Tiers[i].Site); err != nil {
			errs = append(errs, err)

			continue
		}

		// The same rule from the declaration's side: this tier names the site the
		// operator meant, and the reason it does not exist is already reported
		// against the declaration that has the space in it.
		if refused[c.Tiers[i].Site] {
			continue
		}

		errs = append(errs, siteRefError(where, c.Tiers[i].Site, declared, len(c.Sites) > 0)...)
	}

	return errs
}

// siteRefError checks one reference to a site.
//
// AN EMPTY AUTHORITY SET IS TWO DIFFERENT SITUATIONS, and telling an operator
// the wrong one sends them to the wrong edit. `declared` holds the names that
// came through validation intact, so it is empty both when the file has no
// sites block at all and when it has one whose every entry was refused — for a
// blank name or for surrounding whitespace. Using its length as a proxy for the
// first answered "this config declares no sites; add a sites block" about a
// config that has one, which is advice to write something already written.
//
// declaredAny is therefore passed separately: it says whether the file HAS a
// block, which is the question the first branch is actually asking.
func siteRefError(where, site string, declared map[string]bool, declaredAny bool) []error {
	if site == "" {
		return nil
	}

	switch {
	case !declaredAny:
		return []error{fmt.Errorf("%s is %q, but this config declares no sites; add a sites "+
			"block naming it", where, site)}

	// The block exists and nothing usable came out of it, so every entry has
	// already produced its own diagnostic. Pointing at those is the actionable
	// answer: this reference may well be right once they are fixed.
	case len(declared) == 0:
		return []error{fmt.Errorf("%s is %q, and this config's sites block declares no usable "+
			"name — every entry in it was refused above. Fix those; this reference may already "+
			"be correct", where, site)}

	case !declared[site]:
		return []error{fmt.Errorf("%s is %q, which is not a declared site (have %s)",
			where, site, strings.Join(sortedKeys(declared), ", "))}
	}

	return nil
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
	errs := c.validateRemoteShapes()

	if c.Server == nil {
		return errs
	}

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

// localRemoteShapes reports this node's own ordered shape catalogue, and which
// backend it belongs to.
//
// ONE READER FOR "WHAT MAY THIS HOST BUY", so the tier-fits check below does not
// have to know how many remote backends exist. A host-backed provider has no shape
// list at all — its capacity is the machine — which is why the answer is empty
// rather than a zero-length list with a provider attached.
func (c *Config) localRemoteShapes() (ProviderKind, []RemoteShape) {
	if c.Node == nil {
		return "", nil
	}

	switch c.Node.Provider {
	case ProviderEC2:
		if c.Node.EC2 != nil {
			return ProviderEC2, c.Node.EC2.InstanceTypes
		}

	case ProviderCodeBuild:
		if c.Node.CodeBuild != nil {
			return ProviderCodeBuild, c.Node.CodeBuild.ComputeTypes
		}

	case ProviderDocker, ProviderFirecracker, ProviderTart:
		return "", nil
	}

	return "", nil
}

// validateRemoteShapes refuses a tier this node could be given and could not buy.
//
// A TIER LARGER THAN EVERY DECLARED SHAPE QUEUES FOREVER WITH NOTHING SAYING WHY.
// The allocator escrows it happily, because the node's budget covers it — the
// failure appears only after GitHub has assigned the job, as a launch error on
// one host. billet already refuses a tier pinned to a host that cannot run its
// guest OS at load time, for exactly this reason.
//
// Only checkable when one file holds both the node and the tiers, which is the
// single-machine shape. In a fleet the node's file has no tiers, and the launch
// path's error is what remains — it names the size asked for and every shape
// declared, so it is actionable wherever it is read.
func (c *Config) validateRemoteShapes() []error {
	kind, shapes := c.localRemoteShapes()
	if kind == "" {
		return nil
	}

	if len(shapes) == 0 {
		// Already reported as a missing field; saying it twice helps nobody.
		return nil
	}

	var errs []error

	for i := range c.Tiers {
		t := &c.Tiers[i]

		if !t.AcceptsProvider(kind) {
			continue
		}

		// A TIER PINNED ELSEWHERE IS NOT THIS NODE'S PROBLEM. It names a different
		// machine, so the shapes this one may buy say nothing about whether it can
		// run.
		if t.Node != "" && t.Node != c.Node.Name {
			continue
		}

		// NOR IS A TIER THIS NODE COULD NEVER BE GIVEN. Every node in a fleet reads
		// the same tier catalogue, so a small cloud node sees the tiers meant for a
		// large one — and refusing them would make one deployment's config
		// unloadable on half its machines. The allocator will never place work here
		// that exceeds this node's own contribution, so a shape that cannot hold it
		// is not a contradiction.
		//
		// What remains refused is the case that really is broken: a tier this node
		// IS eligible for, and no declared shape can buy.
		// A PINNED TIER IS NOT LET THROUGH BY THAT, and the distinction is the
		// whole point: pinned means this node or nowhere, so oversize is not
		// "another machine's job" — it is a tier that can never run at all.
		if t.Node == "" && ((c.Node.MaxVCPU > 0 && t.VCPU > c.Node.MaxVCPU) ||
			(c.Node.MaxMemory > 0 && t.Memory > c.Node.MaxMemory)) {
			continue
		}

		fits := false

		for _, shape := range shapes {
			if shape.VCPU >= t.VCPU && shape.Memory >= t.Memory {
				fits = true

				break
			}
		}

		if fits {
			continue
		}

		declared := make([]string, 0, len(shapes))
		for _, shape := range shapes {
			declared = append(declared,
				fmt.Sprintf("%s (%d vCPU, %s)", shape.Type, shape.VCPU, shape.Memory))
		}

		errs = append(errs, fmt.Errorf(
			"tier %q requests %d vCPU and %s, which no shape in %s can hold (%s); a job on this "+
				"tier would be admitted and then fail to launch",
			t.Label, t.VCPU, t.Memory, kind.ShapeField(), strings.Join(declared, ", ")))
	}

	return errs
}

// CheckHostPort validates a host:port the way config validation will, naming
// the field. Exported for callers that must refuse a bad value by the flag
// that carried it BEFORE it is rendered into a config — the same
// validate-in-both-consumers rule as CheckRunnerGroup.
func CheckHostPort(field, addr string) error { return validateHostPort(field, addr) }

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

// RunnerCommand is what starts the runner inside this tier's image.
//
// Defaulted here rather than in each backend so every provider agrees, and so
// the default is stated once in a place an operator reads. The wrapper is
// relative because the stock image's working directory is the runner's home.
//
// The generic default remains GitHub's `run.sh`, which is what the Docker runner
// image contains. RunnerCommandFor selects `./billet-runner-service` for the
// Firecracker image billet builds, and the full `/usr/local/bin/billet-runner`
// entrypoint for EC2 because that script prepares and completes the cache around
// the inner result-preserving wrapper.
//
// A self-hosted runner updates itself by EXITING: the listener returns "updating"
// and the wrapper notices and re-execs it with the same arguments — including the JIT
// registration, which is what lets the restarted runner go on to take one job from
// its pool. Exec the listener directly and there is no loop: on a backend where
// each job gets its own machine, the listener exits, the machine is destroyed as
// though the work were finished, the job is redelivered, and the next machine does
// the same thing.
//
// MEASURED, AND NOT CURRENTLY REACHABLE: a JIT configuration minted by GitHub's REST
// API carries `DisableUpdate = True` (and `Ephemeral = True`), so the service never
// sends these runners an update in the first place. The loop above is therefore
// insurance rather than a live requirement.
//
// It is worth keeping as the default anyway. It costs nothing, it is what GitHub
// documents as the way to start a runner, and the setting it depends on is theirs to
// change — while the failure it prevents is silent and spends a guest per attempt.
//
// THE REAL CONSEQUENCE OF THAT MEASUREMENT IS ELSEWHERE, and it is larger: because
// these runners never self-update, GitHub's 30-day rule is a HARD EXPIRY for billet.
// A runner more than 30 days behind a release is refused work outright, and nothing
// on the guest can rescue it — only republishing the image can. See
// internal/runnerrelease.
func (t Tier) RunnerCommand() []string {
	if len(t.Command) > 0 {
		return t.Command
	}

	return []string{"./run.sh"}
}
