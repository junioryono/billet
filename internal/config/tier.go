package config

import (
	"errors"
	"fmt"
	pathpkg "path"
	"slices"
	"strings"
	"unicode"
)

// Tier is one runner shape. Its Label identifies it in the deployment and its
// ScaleSetName is what appears in `runs-on`.
type Tier struct {
	Label string `yaml:"label"`

	// Target names the GitHub target this tier's scale set belongs to. Defaults
	// to the deployment's only target and is required when there are several,
	// because a scale set exists on exactly one organization or repository and
	// the credential that creates it is that target's.
	Target string `yaml:"target,omitempty"`

	// RunsOn is the name of this tier's scale set on its target, the value a
	// workflow puts in `runs-on`. It defaults to Label. It exists so tiers on
	// different targets can answer to one name: Label identifies the tier inside
	// the deployment (its escrow, its leases, its history) and must be unique,
	// while a scale set is unique only within its target, so two targets may each
	// carry a tier with the same RunsOn.
	RunsOn string `yaml:"runs_on,omitempty"`

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
	// Cache configures every cache the tier's jobs get: whose writes publish,
	// which caches are on, and how large each may grow. See TierCache.
	Cache *TierCache `yaml:"cache,omitempty"`

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

// ScaleSetName is the name of this tier's scale set on its target: RunsOn, or
// Label when the tier names none.
func (t Tier) ScaleSetName() string {
	if t.RunsOn != "" {
		return t.RunsOn
	}

	return t.Label
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
	cache := t.EffectiveCache()
	// A TRUSTED-ONLY ACTIONS CACHE IS SCOPED BY A STATIC WORKFLOW, the one the
	// runner group admits; a default-branch one is scoped by the ref GitHub
	// proves for each job, so an untrusted pool may have it too.
	legacyActions := cache.Actions.Enabled && cache.Publish != CachePublishDefaultBranch
	if trust == WorkloadUntrusted && legacyActions {
		errs = append(errs, fmt.Errorf("%s: an untrusted pool can enable the Actions cache "+
			"only with cache.publish: default-branch", where))
	}
	if cache.Actions.Enabled && t.CacheScope == nil {
		errs = append(errs, fmt.Errorf("%s: the Actions cache requires a static cache_scope because JobStarted arrives after launch", where))
	}
	if scope := t.CacheScope; scope != nil {
		if err := CheckCacheScopeSegment(scope.Owner); err != nil {
			errs = append(errs, fmt.Errorf("%s: cache_scope.owner: %w", where, err))
		}
		if err := CheckCacheScopeSegment(scope.Repository); err != nil {
			errs = append(errs, fmt.Errorf("%s: cache_scope.repository: %w", where, err))
		}
		// THE WORKFLOW REF IS OPTIONAL WHERE NOTHING READS IT: a default-branch
		// namespace is the repository, and the job's own ref is proved per job.
		if scope.WorkflowRef != "" || legacyActions {
			if err := checkWorkflowRef(scope.WorkflowRef); err != nil {
				errs = append(errs, fmt.Errorf("%s: cache_scope.workflow_ref %q %w", where, scope.WorkflowRef, err))
			} else {
				workflow := strings.SplitN(strings.SplitN(scope.WorkflowRef, "@", 2)[0], "/", 3)
				if len(workflow) < 2 || workflow[0] != scope.Owner || workflow[1] != scope.Repository {
					errs = append(errs, fmt.Errorf("%s: cache_scope owner/repository must match workflow_ref", where))
				}
			}
		}
		if legacyActions {
			if _, allowed := seenWorkflows[scope.WorkflowRef]; !allowed {
				errs = append(errs, fmt.Errorf("%s: cache_scope.workflow_ref must be one of the trusted workflows", where))
			}
		}
	}
	errs = append(errs, t.cachePolicyErrors(where)...)

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
// against the reference Mac in docs/reference/reference-hardware.md, the same way
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
//
// THE SAME RULE HOLDS FOR EVERY CACHE THE NODE SERVES TO A GUEST IT CONTROLS:
// the Actions cache, the Git proxy and the Bazel and Go caches each mount a
// site-store volume on the host, which only a firecracker node has.
func (t Tier) InterceptionErrors(where string) []error {
	cache := t.EffectiveCache()
	explicit := t.explicitGuestCaches()
	if !cache.needsGuestCacheServices() || len(explicit) == 0 {
		return nil
	}

	var enabled []string
	for _, kind := range explicit {
		enabled = append(enabled, string(kind))
	}
	what := "the " + strings.Join(enabled, ", ") + " cache"

	providers := t.AcceptableProviders()
	if len(providers) != 1 || providers[0] != ProviderFirecracker {
		return []error{fmt.Errorf("%s: %s requires only the firecracker provider; "+
			"remote and fallback providers cannot reach this node's site-local store", where, what)}
	}
	if t.GuestOS != GuestLinux {
		return []error{fmt.Errorf("%s: %s currently requires a Linux guest", where, what)}
	}

	return nil
}

// ReservedVCPU is the vCPU a tier's floor holds back from other tiers.
func (t Tier) ReservedVCPU() int { return t.Reserved * t.VCPU }

// ReservedMemory is the memory a tier's floor holds back from other tiers.
func (t Tier) ReservedMemory() ByteSize { return ByteSize(t.Reserved) * t.Memory }

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
		if t.RunsOn != "" && !labelRe.MatchString(t.RunsOn) {
			errs = append(errs, fmt.Errorf("%s: runs_on must match %s", where, labelRe))
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
// cannot express, and the one docs/reference/records/aws-acceptance.md measures.
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
// the exact queue-and-requeue failure docs/reference/records/aws-acceptance.md
// measures. A macOS
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

// TierByLabel returns the tier with this label, billet's identity for it; that
// is its `runs-on` value only when it names no runs_on.
func (c *Config) TierByLabel(label string) (*Tier, bool) {
	for i := range c.Tiers {
		if c.Tiers[i].Label == label {
			return &c.Tiers[i], true
		}
	}
	return nil, false
}

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
