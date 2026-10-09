package config

import "slices"

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
	// an instance — see docs/reference/decisions/adr-007-codebuild-provider.md.
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
	// ProviderSimulated starts no compute: an instance is a record that reports
	// itself running for a modelled duration and stopped afterwards, so a workload
	// can be driven through the real allocator and placer at a scale no real backend
	// can afford. It exists for billet's own test harness. It fabricates completions,
	// so a configuration that names it anywhere is REFUSED at load, and cmd/billet
	// never constructs it; it is in the closed set only so the ledger and the node
	// wire can register a simulated host in a test.
	ProviderSimulated ProviderKind = "simulated"
)

var allProviders = []ProviderKind{
	ProviderFirecracker, ProviderTart, ProviderEC2, ProviderCodeBuild, ProviderDocker,
	ProviderSimulated,
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
//
// `simulated` is listed as host-backed because every consequence of the answer
// is right for it: its capacity is what the node declares rather than a shape it
// buys, placement charges the tier request, and custody reads its inventory as
// causal, which an authoritative in-memory store is.
func (p ProviderKind) RunsOnHost() bool {
	switch p {
	case ProviderDocker, ProviderFirecracker, ProviderTart, ProviderSimulated:
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

// TestOnly reports whether this backend exists for billet's own test harness and
// may not be named in a configuration.
//
// THE ONE READER OF THAT DISTINCTION. Valid says whether billet implements a
// backend, and the simulated one is implemented: the allocator and the node wire
// register it in tests. What it may not do is reach a deployment through a file,
// because a backend that fabricates completions is a fleet that reports every job
// finished and runs none. That refusal lives in Config.Validate rather than in the
// per-tier and per-node rules alloc.New re-applies, so a catalogue built in code
// can still name it.
func (p ProviderKind) TestOnly() bool { return p == ProviderSimulated }

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
