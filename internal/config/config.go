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
	"errors"
	"fmt"
	"net"
	"net/url"
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
}

// SiteConfig declares one place compute runs.
//
// A STRUCT RATHER THAN A STRING, because a site is where the storage backend will
// be configured — Ceph at a bare-metal site, EBS and S3 in a cloud region
// (#23/#25). Today it carries identity, which is the half placement needs.
type SiteConfig struct {
	// Name is what a node and a tier refer to this site by.
	Name string `yaml:"name"`
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
	// EC2 is required when Provider is ProviderEC2.
	EC2 *EC2Config `yaml:"ec2,omitempty"`
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
// hardware.
func (p ProviderKind) RunsOnHost() bool {
	switch p {
	case ProviderDocker, ProviderFirecracker, ProviderTart:
		return true
	case ProviderEC2:
		return false
	default:
		return false
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
	BinaryPath string `yaml:"binary_path,omitempty"`
	JailerPath string `yaml:"jailer_path,omitempty"`
	// KernelImage is the uncompressed guest kernel. It must be built with
	// everything Docker needs; validate with moby's contrib/check-config.sh
	// rather than a hand-maintained list.
	KernelImage string `yaml:"kernel_image"`
	// Bridge is the host bridge guests attach to.
	Bridge string `yaml:"bridge,omitempty"`
}

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
}

// EC2InstanceType is one shape billet may buy, and what it holds.
//
// The vCPU and memory are DECLARED rather than looked up, because the allocator
// has already escrowed a size against this node before any of this is consulted:
// a shape that turns out smaller than the lease it was chosen for over-commits a
// machine nobody can see.
type EC2InstanceType struct {
	Type   string   `yaml:"type"`
	VCPU   int      `yaml:"vcpu"`
	Memory ByteSize `yaml:"memory"`
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
	// is not a kindness. docker and ec2 are built; firecracker and tart are not.
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

	// Command starts the runner inside this tier's image. Empty means the stock runner
	// image's `./run.sh`.
	//
	// Expressible because a container image's default command is a shell: a backend
	// that launches one gets a container that exits immediately while every signal
	// reports success, so the command cannot be left to the image.
	Command []string `yaml:"command,omitempty"`

	// NOTE: the scale-set client interpolates this name into a query string WITHOUT
	// escaping it, so an ordinary group name like "Platform & Security" is parsed as
	// two parameters and comes back as "group not found". Validation rejects what the
	// client cannot carry rather than letting GitHub answer confusingly.

	VCPU   int      `yaml:"vcpu"`
	Memory ByteSize `yaml:"memory"`
	Disk   ByteSize `yaml:"disk,omitempty"`
	// SHM sizes /dev/shm. Chromium and Postgres both misbehave on the default,
	// so this is a tier knob rather than an image constant.
	SHM ByteSize `yaml:"shm,omitempty"`

	Image string `yaml:"image"`

	// WarmPool pre-boots idle VMs to hide cold-start latency. Warm guests count
	// against MaxConcurrent, because a warm macOS guest is still a running macOS guest
	// as far as Apple's licence is concerned.
	WarmPool int `yaml:"warm_pool,omitempty"`

	// Intercept enables the colocated Actions cache for this tier. Off by default:
	// cache interception terminates TLS in front of GitHub's results service, which
	// also carries artifact metadata. Leave it off for tiers that publish release
	// artifacts or hold deployment secrets.
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
	// THE COST IS IDLE CAPACITY. Listeners refill escrow eagerly and without regard to
	// demand, so a reserved tier CLAIMS its floor almost immediately and holds it for
	// as long as it is healthy: reserve 2 slots of an 8 vCPU tier nothing uses and the
	// machine is permanently 16 vCPU smaller, with no error and no log line. Reserve
	// for tiers that have demand and are being crowded out. Zero is the default.
	Reserved int `yaml:"reserved,omitempty"`
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

// ReservedVCPU is the vCPU a tier's floor holds back from other tiers.
func (t Tier) ReservedVCPU() int { return t.Reserved * t.VCPU }

// ReservedMemory is the memory a tier's floor holds back from other tiers.
func (t Tier) ReservedMemory() ByteSize { return ByteSize(t.Reserved) * t.Memory }

// GuestOSProviderErrors reports backends that cannot host a tier's guest OS.
//
// Split out from the fuller relational validation so alloc can apply the part that
// is a SAFETY invariant rather than a configuration convenience: Apple's licence
// permits macOS guests only on Apple hardware, and runtime placement only tests
// list membership, so a macOS tier that fell back to EC2 would bind there happily.
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
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	defer f.Close()

	var c Config
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true) // typos in a CI config should be loud, not ignored
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, relocatedKeyHint(err))
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

// relocatedKeyHint turns "unknown field" into "that key moved, and here is where".
//
// KnownFields cannot tell a typo from a removal. For a typo that is the whole
// story; for a setting that was correct when the operator wrote it and has since
// moved roles, the message names the key they already know about and says nothing
// about what to do.
func relocatedKeyHint(err error) error {
	moved := map[string]string{
		"lock_dir":                  "node.lock_dir",
		"allow_unlocked_deployment": "node.allow_unlocked_deployment",
	}

	msg := err.Error()

	for key, home := range moved {
		if !strings.Contains(msg, "field "+key+" not found in type config.ServerConfig") {
			continue
		}

		return fmt.Errorf("%w\n\nserver.%s has moved to %s. The host-wide deployment lock "+
			"stops two processes managing one deployment's containers, and a control plane "+
			"manages none — the node takes it. A server that held it too would keep a node on "+
			"the same machine from ever starting, which is the single-machine deployment: "+
			"`billet server` and `billet node` side by side", err, key, home)
	}

	return err
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
		c.Node.EC2.normalize()
		c.Node.Ceph.normalize()

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
		// A macOS tier with no explicit cap inherits its host's limit rather than
		// "unlimited", so forgetting the field fails safe, and lowering a Mac's limit
		// tightens every macOS tier pinned to it. Only from a usable limit — copying a
		// negative one would turn one bad field into three diagnostics.
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

	if c.Node.StateDir == "" {
		errs = append(errs, errors.New("node.state_dir is required"))
	}
	if err := validateHostPort("node.server_addr", c.Node.ServerAddr); err != nil {
		errs = append(errs, err)
	}

	errs = append(errs, c.validateNodeTLS()...)
	if c.Node.Provider == ProviderFirecracker {
		if c.Node.Firecracker == nil {
			errs = append(errs, errors.New("node.firecracker is required when provider is firecracker"))
		} else if c.Node.Firecracker.KernelImage == "" {
			errs = append(errs, errors.New("node.firecracker.kernel_image is required"))
		}
	}

	errs = append(errs, c.validateCephNode()...)

	if c.Node.Provider == ProviderEC2 {
		errs = append(errs, c.validateEC2Node()...)
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
	if c.Node.Provider != ProviderFirecracker {
		if c.Node.Ceph != nil {
			return []error{fmt.Errorf("node.ceph is set but this node's provider is %s, which "+
				"cannot attach a block device: a container has nowhere to put one and an ec2 "+
				"node's compute runs in a region that cannot reach this cluster, so nothing "+
				"would ever read these settings", c.Node.Provider)}
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

// checkCephPool refuses a pool name billet cannot address.
//
// PINNED TO WHAT THE rbd CLIENT DOES, not to a reading of Ceph's naming rules.
// billet addresses an image as `pool/image`, so the characters that matter are
// the ones that change how that string parses — a `/` splits it somewhere else
// and a `@` starts a snapshot. Ceph reserves a leading `.` for its own pools
// (`.mgr`), and `rbd` reads a leading `-` as a flag. Everything else is Ceph's to
// accept or refuse, and it says so plainly when it refuses.
func checkCephPool(field, pool string) []error {
	if pool == "" {
		return []error{fmt.Errorf("node.ceph.%s is required", field)}
	}

	for _, bad := range []struct {
		what   string
		reason string
	}{
		{"/", "billet addresses an image as pool/image, so a slash points at a different pool"},
		{"@", "rbd reads @ as the start of a snapshot name"},
		{" ", "a space is not part of a pool name and will not round-trip through the client"},
	} {
		if strings.Contains(pool, bad.what) {
			return []error{fmt.Errorf("node.ceph.%s %q contains %q: %s", field, pool, bad.what, bad.reason)}
		}
	}

	if strings.HasPrefix(pool, ".") {
		return []error{fmt.Errorf("node.ceph.%s %q begins with a dot, which ceph reserves for its "+
			"own pools such as .mgr", field, pool)}
	}

	if strings.HasPrefix(pool, "-") {
		return []error{fmt.Errorf("node.ceph.%s %q begins with a dash, which the rbd client reads "+
			"as a flag rather than as a pool", field, pool)}
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
	// buy on somebody's account. It is NOT a spending limit — see #47, and the
	// package comment on internal/provider/ec2.
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
	if strings.TrimSpace(endpoint) == "" {
		return nil
	}

	u, err := url.Parse(endpoint)
	if err != nil {
		return errors.New("node.ec2.endpoint is not a url")
	}

	if u.Opaque != "" {
		return errors.New("node.ec2.endpoint is not a url billet can dial: it has no // and " +
			"therefore no host, so nothing says which machine to sign a request for")
	}

	if u.User != nil {
		return errors.New("node.ec2.endpoint must not carry a username or password: billet " +
			"authenticates with a request signature, so one would be a credential in a string " +
			"that gets logged and nothing else")
	}

	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("node.ec2.endpoint must not carry a query string or fragment: billet " +
			"builds every request itself, so anything there is either ignored or a secret in a " +
			"value that gets logged")
	}

	if u.Scheme != "https" && u.Scheme != "http" {
		return errEndpointNeedsHTTPS
	}

	if u.Hostname() == "" {
		return errors.New("node.ec2.endpoint names no host")
	}

	// THE QUERY API LIVES AT THE ROOT. A path here would be signed and posted to,
	// so `https://vpce.example/v1` sends every call somewhere that is not the
	// service — and no AWS regional, VPC-interface or non-commercial-partition
	// endpoint needs one. Absent and "/" are both the root.
	if path := u.EscapedPath(); path != "" && path != "/" {
		return errors.New("node.ec2.endpoint must name a host with no path: billet posts the " +
			"ec2 query api at the root, and signs whatever path it is given")
	}

	if u.Scheme == "https" || isLoopbackHost(u.Hostname()) {
		return nil
	}

	return errEndpointNeedsHTTPS
}

// errEndpointNeedsHTTPS names the rule without naming the value.
var errEndpointNeedsHTTPS = errors.New(
	"node.ec2.endpoint must use https: billet signs each request and sends a session token with " +
		"it, so plaintext hands an on-path observer a replayable request. Only a loopback " +
		"address may use http, where the trust boundary is the machine itself")

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
	var errs []error

	if len(e.InstanceTypes) == 0 {
		errs = append(errs, errors.New("node.ec2.instance_types needs at least one shape; "+
			"billet ships no table of EC2 instance types, so a shape it may buy has to be "+
			"declared along with what it holds"))

		return errs
	}

	seen := make(map[string]struct{}, len(e.InstanceTypes))

	for i := range e.InstanceTypes {
		it := &e.InstanceTypes[i]
		where := fmt.Sprintf("node.ec2.instance_types[%d]", i)

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

		errs = append(errs, t.ProviderErrors(where)...)
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
		// EVERY listed provider must be Tart, not merely one of them: Apple's licence
		// permits macOS guests only on Apple hardware, and a macOS tier that could fall
		// back to EC2 is the case nobody is watching when it happens.
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

// SiteNames lists the places this deployment declares.
//
// For the node wire, which is where a REMOTE node's claim to be somewhere is
// checked — the node's own config cannot answer that question.
func (c *Config) SiteNames() []string {
	out := make([]string, 0, len(c.Sites))
	for _, s := range c.Sites {
		out = append(out, s.Name)
	}

	return out
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

	for i, s := range c.Sites {
		name := strings.TrimSpace(s.Name)

		if name == "" {
			errs = append(errs, fmt.Errorf("sites[%d]: a site must have a name; nothing can "+
				"refer to one without it", i))

			continue
		}

		if declared[name] {
			errs = append(errs, fmt.Errorf("sites[%d]: site %q is declared twice; a site is "+
				"where a cache lives, so two answers to that is one too many", i, name))

			continue
		}

		declared[name] = true
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
		errs = append(errs, siteRefError(
			fmt.Sprintf("tiers[%d] (%s): site", i, c.Tiers[i].Label),
			c.Tiers[i].Site, declared)...)
	}

	return errs
}

// siteRefError checks one reference to a site.
func siteRefError(where, site string, declared map[string]bool) []error {
	if site == "" {
		return nil
	}

	if len(declared) == 0 {
		return []error{fmt.Errorf("%s is %q, but this config declares no sites; add a sites "+
			"block naming it", where, site)}
	}

	if !declared[site] {
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
	errs := c.validateEC2Shapes()

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

// validateEC2Shapes refuses a tier this node could be given and could not buy.
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
func (c *Config) validateEC2Shapes() []error {
	if c.Node == nil || c.Node.Provider != ProviderEC2 || c.Node.EC2 == nil {
		return nil
	}

	shapes := c.Node.EC2.InstanceTypes
	if len(shapes) == 0 {
		// Already reported as a missing field; saying it twice helps nobody.
		return nil
	}

	var errs []error

	for i := range c.Tiers {
		t := &c.Tiers[i]

		if !t.AcceptsProvider(ProviderEC2) {
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
			"tier %q requests %d vCPU and %s, which no shape in node.ec2.instance_types can "+
				"hold (%s); a job on this tier would be admitted and then fail to launch",
			t.Label, t.VCPU, t.Memory, strings.Join(declared, ", ")))
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

// RunnerCommand is what starts the runner inside this tier's image.
//
// Defaulted here rather than in each backend so every provider agrees, and so
// the default is stated once in a place an operator reads. `./run.sh` is
// relative because the stock image's working directory is the runner's home.
func (t Tier) RunnerCommand() []string {
	if len(t.Command) > 0 {
		return t.Command
	}

	return []string{"./run.sh"}
}
