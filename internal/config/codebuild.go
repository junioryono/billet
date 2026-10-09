package config

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

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

// codeBuildProjectRe is what AWS documents a project name may contain: letters,
// digits, hyphen and underscore, 2 to 150 characters.
//
// PINNED TO THE DOCUMENTED RULE rather than to a guess about URL safety, which is
// the mistake the runner-group validator made in both directions. The name is
// interpolated into no URL here — it travels in a JSON body — so the only reason to
// constrain it is that a name AWS will refuse should be refused at load rather than
// on the first launch.
var codeBuildProjectRe = regexp.MustCompile(`^[A-Za-z0-9_-]{2,150}$`)

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
			"docs/reference/decisions/adr-002-cloud-compute-backend.md "+
			"disqualified Lambda outright",
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
				"on owned EC2 or Mac capacity; see docs/deploying/aws-codebuild.md",
			CodeBuildBuildCeilingMinutes, CodeBuildQueuedCeilingMinutes))
	}

	return errs
}
