package initconfig

import (
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// The build images billet names for a generation, per environment.
//
// NAMED RATHER THAN BUILT, and rather than left to the project. billet does not
// build CodeBuild images and does not need to: AWS curates these, and a
// generation that named none would leave the project's own configuration to
// decide — which is a different image on every adopted project and no image at
// all on a fresh one. `imageOverride` is sent on every launch for the same reason
// the log group is: what billet's policy names and what the build actually uses
// must not be able to disagree.
//
// MEASURED FOR macOS, not read: ADR-007 records that
// `aws/codebuild/macos-arm-base:14` ships Xcode 26.2, git 2.52.0 and bsdtar on
// arm64 and does NOT ship the Actions runner — which is what billet's generated
// buildspec already assumes, and why internal/runnerrelease pins the osx-arm64
// checksum.
const (
	defaultCodeBuildLinuxImage = "aws/codebuild/amazonlinux-x86_64-standard:5.0"
	defaultCodeBuildARMImage   = "aws/codebuild/amazonlinux-aarch64-standard:3.0"
	defaultCodeBuildMacImage   = "aws/codebuild/macos-arm-base:14"
)

// CodeBuildParams is the CodeBuild placement a generation needs and billet
// cannot detect.
//
// EVERY FIELD HERE IS SOMEBODY'S DECISION. There is no machine to measure: a
// codebuild node calls an API and the build appears in a region, so the ceiling
// is a declared budget and the shapes are a declared catalogue — the node.ec2
// rule, for the node.ec2 reason. What is different is the ACKNOWLEDGEMENT, which
// exists so a sentence is read by a person rather than met as a build that died
// at hour 36.
type CodeBuildParams struct {
	Region  string
	Project string
	// Environment decides this node's guest OS, which billet reports at
	// registration rather than taking a second answer from the config.
	Environment config.CodeBuildEnvironment
	// FleetARN selects reserved capacity. Empty means on-demand compute, which
	// three of the six environments do not offer at all.
	FleetARN string
	// ComputeTypes is the ordered catalogue, most preferred first, each entry
	// declaring what it holds and what it costs.
	ComputeTypes []config.RemoteShape
	// JITParameterPath is where each build's single-use registration is written.
	// It is an IAM boundary rather than a naming preference, so billet does not
	// guess one.
	JITParameterPath string
	JITKMSKeyID      string
	LogGroup         string
	PrivilegedMode   bool
	// AcceptCeiling is the operator having read what CodeBuild's own limits are.
	// Its absence is a refusal; see errCodeBuildNeedsCeiling.
	AcceptCeiling        bool
	BuildTimeoutMinutes  int
	QueuedTimeoutMinutes int
	// FleetCapacity is how many builds this fleet may run at once, and it is
	// REQUIRED for macOS and meaningless elsewhere.
	//
	// A macOS tier with no explicit max_concurrent inherits its host's limit,
	// which defaults to APPLE's two-guests-per-machine allowance — a rule about
	// hardware somebody owns, and not the rule here: on this backend the cap is
	// the fleet capacity, which billet cannot see and AWS defaults to ONE. So the
	// generation asks rather than inventing a number that reads as a licence
	// statement it is not.
	FleetCapacity int
	// NodeName is what this orchestrator is called in the deployment, and it is
	// REQUIRED for macOS for the same reason a tart generation requires one:
	// config validation refuses a macOS tier that names no node, because the
	// per-host guest limit cannot be enforced against a tier that is pinned to
	// nowhere.
	//
	// IT IS NOT THIS MACHINE'S HOSTNAME, and that is the difference from tart. A
	// tart node IS the Mac, so its hostname is at least a candidate; a codebuild
	// node is a small machine somewhere that calls an API, and its hostname says
	// nothing about the fleet the limit is actually about. So it is asked for
	// rather than derived, and a generation that guessed would name a host the
	// operator never chose — which they would meet again the first time
	// `billet ca issue` disagreed.
	NodeName string
}

// errCodeBuildNeedsPolicy is what a codebuild generation with no trusted-pool
// policy gets.
//
// REFUSED RATHER THAN GENERATED AS UNTRUSTED, which is the docker rule for a
// sharper reason. Docker refuses untrusted work because a container shares the
// host kernel; this backend refuses it because AWS documents a reserved-capacity
// instance as staying alive between builds and sharing cached data with other
// projects in the account, BY DESIGN. So an untrusted codebuild config is one
// that loads, starts, and then refuses its first fork pull request — with no
// setting anywhere that would have made it work.
var errCodeBuildNeedsPolicy = errors.New(
	"a codebuild config needs --runner-group and at least one --workflow: untrusted work is " +
		"REFUSED on this backend outright, not gated on a network setting, because AWS " +
		"documents a reserved-capacity instance as staying alive between builds and sharing " +
		"cached data with other projects in the account. Every tier here is therefore " +
		"trusted, and a trusted pool is defined by a non-default GitHub runner group and an " +
		"exact workflow allowlist. Create a runner group in GitHub (Settings -> Actions -> " +
		"Runner groups), restrict it to the workflows below, and pass both. Untrusted tiers " +
		"belong on firecracker or ec2")

// errCodeBuildNeedsCeiling is what a generation that has not acknowledged
// CodeBuild's own limits gets.
//
// IT CHANGES NOTHING ABOUT HOW BILLET BEHAVES, which is exactly why it has to be
// refused at generation time as well as at load: the whole point is that the
// sentence is read by a person BEFORE a tier advertises capacity. A generator
// that wrote `accept_external_build_ceiling: true` on the operator's behalf would
// be acknowledging it for them, which is the one thing the field exists to
// prevent.
var errCodeBuildNeedsCeiling = fmt.Errorf(
	"--accept-external-build-ceiling is required: every job on a codebuild node inherits "+
		"limits billet cannot lift — a build is capped at %d minutes (36 hours) and a build "+
		"still waiting for capacity is FAILED after %d minutes (8 hours). billet adds no "+
		"deadline of its own and no drain or upgrade ever stops a build for taking too long, "+
		"but it will not write a tier whose ceiling nobody has acknowledged. Work that can "+
		"exceed either limit belongs on owned EC2 or Mac capacity; "+
		"see docs/deploying/aws-codebuild.md",
	config.CodeBuildBuildCeilingMinutes, config.CodeBuildQueuedCeilingMinutes)

// normalizeCodeBuild fills in what a generation may default and refuses what it
// may not.
//
// PREPARE RUNS BEFORE VALIDATION, which is config.CodeBuildConfig.Prepare's own
// stated rule and the reason it is exported: it NORMALIZES and DEFAULTS, and the
// defaults are what the range checks then judge. A generator that validated
// first would validate a block with two zeroed timeouts and then write a
// different one.
func normalizeCodeBuild(p *Params) error {
	cb := p.CodeBuild

	if !cb.AcceptCeiling {
		return errCodeBuildNeedsCeiling
	}

	// PREPARED FIRST, AND THE PREPARED VALUES ARE WHAT EVERYTHING BELOW READS.
	//
	// A review round found the ordering the other way round, and the failure it
	// produces is not cosmetic: Prepare TRIMS, so a caller reaching Generate
	// directly with `" MAC_ARM "` had validation see the normalized MAC_ARM while
	// every rendering decision — the default image, the node pin, the host policy,
	// the tier's guest_os — read the raw string and took the LINUX branch. The
	// output validated and described a node that registers as macOS advertising
	// Linux tiers, which are then unplaceable. The CLI trimmed on its way in, so
	// only the exported entry point could reach it, which is precisely the
	// two-entry-points shape refuseForeignBackendInputs exists for.
	prepared := cb.block()
	prepared.Prepare()
	cb.adopt(prepared)

	if p.Image == "" {
		image, err := defaultCodeBuildImage(cb.Environment)
		if err != nil {
			return err
		}

		p.Image = image
	}

	// THE FLEET'S CAPACITY IS ASKED FOR ONLY WHERE IT DECIDES SOMETHING, and
	// refused where it does not — the rule node.ceph follows on a container
	// backend. A Linux tier's concurrency is bounded by node.max_vcpu and
	// node.max_memory, so a per-tier cap there would be a second answer to a
	// question already answered.
	if cb.Environment == config.CodeBuildMacARM {
		if cb.FleetCapacity <= 0 {
			return errors.New("--codebuild-fleet-capacity is required for a MAC_ARM " +
				"generation and must be positive: a macOS tier with no explicit " +
				"max_concurrent inherits Apple's two-guests-per-machine allowance, which is a " +
				"rule about hardware you own and not the rule here — on this backend the cap " +
				"is your reserved fleet's capacity, which billet cannot see and which AWS " +
				"defaults to ONE. `aws codebuild batch-get-fleets --names <fleet>` reports it")
		}

		// A macOS TIER MUST NAME A HOST, and this is config validation's rule
		// rather than the generator's: the per-host guest limit cannot be enforced
		// against a tier pinned to nowhere. billet does not derive the name — a
		// codebuild node is a small machine calling an API, so its hostname says
		// nothing about the fleet the limit is about.
		if cb.NodeName == "" {
			return errors.New("--node-name is required for a MAC_ARM generation: a macOS " +
				"tier has to name the host it is pinned to, because the per-host guest limit " +
				"cannot be enforced against a tier pinned to nowhere. billet will not derive " +
				"one from this machine's hostname — a codebuild node is a small machine " +
				"calling an API, and its hostname says nothing about the fleet the limit is " +
				"about. It is also the name `billet ca issue` mints a certificate for")
		}

		// VALIDATED WHEREVER IT IS SUPPLIED, not only where it is required. A name
		// is written into the file on any codebuild generation that gives one, and
		// it is what the control plane authorises and what `billet ca issue` must
		// later mint a certificate for — so an illegal one has to be refused here
		// rather than at the operator's next command.
	}

	if cb.NodeName != "" {
		if err := config.ValidateNodeName("--node-name", cb.NodeName); err != nil {
			return err
		}
	}

	if cb.Environment != config.CodeBuildMacARM && cb.FleetCapacity != 0 {
		return fmt.Errorf("--codebuild-fleet-capacity is only meaningful for MAC_ARM, and "+
			"this generation is %s: a Linux tier's concurrency is bounded by node.max_vcpu "+
			"and node.max_memory, so a per-tier cap would be a second answer to a question "+
			"the budget already answers", cb.Environment)
	}

	// THE PROVIDER'S OWN RULE, ON THE GENERATOR'S SIDE, and it judges the PREPARED
	// block adopted at the top — never a second one built here, which would be
	// validating a copy the renderer does not use: the exact shape
	// EC2Config.normalize was written to remove. CheckCodeBuild is exported for
	// the alloc.New reason (the provider's constructor cannot assume its config
	// came through config.Load), so both sides call one rule rather than holding
	// two opinions about acceptable CodeBuild blocks.
	if errs := config.CheckCodeBuild(cb.block()); len(errs) > 0 {
		return fmt.Errorf("the codebuild inputs are not usable: %w", errors.Join(errs...))
	}

	return nil
}

// adopt copies a prepared block's normalized and defaulted values back, so every
// decision below — and the file itself — reads what validation judged.
//
// EVERY FIELD Prepare TOUCHES, not the two timeouts an earlier version copied. A
// trimmed value left behind is one the file carries with its padding while
// validation saw it without, which is the one-representation rule
// internal/config has already had to learn three times.
func (c *CodeBuildParams) adopt(prepared config.CodeBuildConfig) {
	c.Region = prepared.Region
	c.Project = prepared.Project
	c.FleetARN = prepared.FleetARN
	c.Environment = prepared.EnvironmentType
	c.JITParameterPath = prepared.JITParameterPath
	c.JITKMSKeyID = prepared.JITKMSKeyID
	c.LogGroup = prepared.LogGroup
	c.BuildTimeoutMinutes = prepared.BuildTimeoutMinutes
	c.QueuedTimeoutMinutes = prepared.QueuedTimeoutMinutes

	// AND THE SHAPE NAMES, which is the half the provider's own constructor was
	// missing before Prepare existed: a padded compute type passed validation,
	// which checks a trimmed copy, and was then sent to AWS with its padding.
	// Here it would also reach a tier LABEL.
	c.ComputeTypes = prepared.ComputeTypes
}

// block renders these params as the config type, which is what validation and
// the provider both read.
func (c *CodeBuildParams) block() config.CodeBuildConfig {
	return config.CodeBuildConfig{
		Region:                     c.Region,
		Project:                    c.Project,
		FleetARN:                   c.FleetARN,
		EnvironmentType:            c.Environment,
		ComputeTypes:               c.ComputeTypes,
		AcceptExternalBuildCeiling: c.AcceptCeiling,
		BuildTimeoutMinutes:        c.BuildTimeoutMinutes,
		QueuedTimeoutMinutes:       c.QueuedTimeoutMinutes,
		JITParameterPath:           c.JITParameterPath,
		JITKMSKeyID:                c.JITKMSKeyID,
		LogGroup:                   c.LogGroup,
		PrivilegedMode:             c.PrivilegedMode,
	}
}

// defaultCodeBuildImage names the curated image for an environment, and refuses
// where there is no single right answer.
//
// THE GPU ENVIRONMENT HAS NO DEFAULT ON PURPOSE. Its images are large, versioned
// against a CUDA release and chosen for a workload; naming one would be billet
// picking somebody's toolchain. Every other environment has one obvious curated
// image for its architecture.
func defaultCodeBuildImage(env config.CodeBuildEnvironment) (string, error) {
	switch env {
	case config.CodeBuildLinuxContainer, config.CodeBuildLinuxEC2:
		return defaultCodeBuildLinuxImage, nil
	case config.CodeBuildARMContainer, config.CodeBuildARMEC2:
		return defaultCodeBuildARMImage, nil
	case config.CodeBuildMacARM:
		return defaultCodeBuildMacImage, nil
	case config.CodeBuildLinuxGPUContainer:
		return "", errors.New("--image is required for LINUX_GPU_CONTAINER: its curated " +
			"images are versioned against a CUDA release and chosen for a workload, so " +
			"naming one would be billet picking your toolchain")
	}

	// Unreachable through the CLI, which validates the environment first, and
	// reachable through Generate, which is exported. CheckCodeBuild would refuse
	// it a moment later; this says which decision could not be made.
	return "", fmt.Errorf("no build image is known for environment %q", env)
}

// codeBuildTiers derives one tier per compute type that fits the budget.
//
// THE SHAPE IS THE TIER, exactly as it is for ec2: placement charges the first
// declared shape that fits rather than the smaller tier request, so a tier whose
// size does not correspond to a shape would be charged for something an operator
// never sees in the file. Ordered as the operator ordered them, because that
// order is what decides which one a job is bought on.
//
// AND THE BUDGET IS SHARED, so the running total is what a candidate is tested
// against rather than the bare ceiling. Every tier is its own scale set and
// every listener escrows one discovery slot BEFORE it advertises, so a
// catalogue whose floor exceeds the ceiling advertises zero everywhere and
// every job queues forever against a control plane reporting itself healthy.
func codeBuildTiers(shapes []config.RemoteShape, ceilVCPU int, ceilMemory config.ByteSize) []tier {
	return remoteTiers(shapes, ceilVCPU, ceilMemory, func(s config.RemoteShape) string {
		return codeBuildTierLabel(s.Type, s.VCPU)
	})
}

// codeBuildTierLabel names a tier after the vCPU it holds, matching every other
// generated catalogue, and disambiguates by compute type where two shapes hold
// the same.
//
// TWO SHAPES CAN DECLARE THE SAME vCPU — a general and a large of one size, or
// one an operator declared twice at different prices — and a duplicate label is
// a config validation error rather than a silent merge, so the label carries the
// shape when the number alone would collide. Lowercased and underscore-free
// because a label has to match config's own labelRe.
func codeBuildTierLabel(computeType string, vcpu int) string {
	suffix := strings.ToLower(computeType)
	suffix = strings.ReplaceAll(suffix, "_", "-")
	suffix = strings.ReplaceAll(suffix, ".", "-")

	return fmt.Sprintf("billet-%dvcpu-%s", vcpu, suffix)
}

// codeBuildComputeTypesYAML renders the ordered catalogue.
func codeBuildComputeTypesYAML(shapes []config.RemoteShape) string {
	var b strings.Builder

	for i := range shapes {
		s := &shapes[i]
		price := s.PriceUSDPerHour

		fmt.Fprintf(&b, "      - type: %s\n        vcpu: %d\n        memory: %s\n"+
			"        price_usd_per_hour: %s\n",
			yamlScalar(s.Type), s.VCPU, s.Memory, price.Decimal())
	}

	return b.String()
}

// codeBuildOptionalYAML renders the node.codebuild keys a generation only writes
// when it was told about them.
//
// ABSENT RATHER THAN EMPTY, because several of these mean something different
// when unset: no fleet_arn is on-demand compute, no log_group is CodeBuild's own
// default for the project, and no jit_kms_key_id is the account's aws/ssm key.
// Writing them blank would put three values in the file that billet then reads
// as decisions nobody made.
func (c *CodeBuildParams) optionalYAML() string {
	var b strings.Builder

	if c.FleetARN != "" {
		b.WriteString("    # RESERVED CAPACITY. A fleet is a 24-hour commitment rather than an\n")
		b.WriteString("    # hourly one — DeleteFleet moves it to PENDING_DELETION and it bills\n")
		b.WriteString("    # until every instance has run for 24 hours — and it still serves\n")
		b.WriteString("    # builds while pending, so the fleet you have is the one to keep using.\n")
		fmt.Fprintf(&b, "    fleet_arn: %s\n", yamlScalar(c.FleetARN))
	}

	if c.JITKMSKeyID != "" {
		fmt.Fprintf(&b, "    jit_kms_key_id: %s\n", yamlScalar(c.JITKMSKeyID))
	}

	if c.LogGroup != "" {
		b.WriteString("    # Where builds log. Unset pins CodeBuild's own default for the\n")
		b.WriteString("    # project, which is what the build role's grant is scoped to.\n")
		fmt.Fprintf(&b, "    log_group: %s\n", yamlScalar(c.LogGroup))
	}

	if c.PrivilegedMode {
		b.WriteString("    # A GitHub Actions job routinely runs `docker build` and service\n")
		b.WriteString("    # containers, and neither works without this. It is a real privilege\n")
		b.WriteString("    # grant, which is why billet does not turn it on for you.\n")
		b.WriteString("    privileged_mode: true\n")
	}

	return b.String()
}

// renderCodeBuildConfig writes a codebuild config as commented text.
//
// It differs from renderEC2Config in what the node block carries and in what the
// comments have to say out loud: two ceilings billet cannot lift, a trust refusal
// that is not a setting, and a concurrency quota that defaults to one and will
// bite before anything else does.
func renderCodeBuildConfig(p Params, trusted bool, appID, installationID int) string {
	scope := scopeLineFor(p.Org, p.Repository)

	paths := p.paths()
	cb := p.CodeBuild

	lockBlock := ""
	if p.Profile == ProfileLocalService {
		lockBlock = p.lockBlock(paths.lockDir)
	}

	runIntro := "# One file, both roles: `billet server` is the control plane and\n" +
		"# `billet node` is a compute host that starts ONE CodeBuild build per job.\n" +
		"# On this machine you run both, as two processes talking over the loopback\n" +
		"# address below; the jobs themselves run in %s."
	if p.Profile == ProfileLocalService {
		runIntro = "# One file, both roles: `billet server` is the control plane and\n" +
			"# `billet node` is a compute host that starts ONE CodeBuild build per job,\n" +
			"# run as " + p.serviceUnits() + " (`billet local up`) over the loopback\n" +
			"# address below; the jobs themselves run in %s."
	}

	// Rendered separately and passed as an argument: splicing it into the
	// template would make any % in an operator value corrupt the argument list.
	intro := fmt.Sprintf(runIntro, cb.Region)

	nameBlock, policyBlock := codeBuildNodeBlocks(cb)

	return fmt.Sprintf(`# billet — written by `+"`billet init --provider codebuild`"+` for an AWS CodeBuild fleet.
#
%s
#
# Add a second orchestrator by running `+"`billet ca issue <name>`"+` here, copying
# the bundle to that host, and giving it a config with a node: section pointing
# at this one. Its node.name comes from the certificate.

server:
  # Loopback only, so billet opens nothing the network can reach.
  listen: %s

  state_dir: %s

  # THE CEILING BILLET ESCROWS AGAINST — for codebuild this is your CLOUD BUDGET,
  # not a measurement, because the builds run in a region rather than on this
  # host. Placement charges the compute type it will buy, so these are hard
  # resource budgets: billet never has more than this much vCPU or memory running
  # at once.
  #
  # SIZE IT AGAINST YOUR QUOTA, NOT ONLY YOUR WALLET. CodeBuild's
  # concurrently-running-builds quota is per environment and compute type and
  # DEFAULTS TO ONE, and the account-wide build QUEUE is capped at 30. Past that
  # StartBuild is refused, billet fails the lease, and GitHub requeues the job at
  # most three times — so a budget that can escrow more builds than the account
  # can queue turns overflow into failed jobs rather than slow ones. Raise the
  # quota in Service Quotas before this advertises capacity it cannot use.
  max_vcpu: %d
  max_memory: %s

github:
%s

  # Filled in by `+"`billet github-app create --config <this file>`"+`.
  app_id: %d
  installation_id: %d
  private_key_path: %s

# This machine, as the codebuild orchestrator. It runs no jobs itself — it calls
# the CodeBuild API and the build appears in the region.
node:
%s  server_addr: %s
  provider: codebuild
  state_dir: %s
%s
  # REQUIRED for codebuild and equal to the ceiling above: there is no host to
  # detect a contribution from, so billet will not guess how much to buy on your
  # account.
  max_vcpu: %d
  max_memory: %s

  codebuild:
    region: %s
    # DEDICATED TO THIS DEPLOYMENT AND THIS NODE. A CodeBuild build cannot be
    # tagged — StartBuild has no field that becomes one — so the project is half
    # of what tells billet's own builds from somebody else's, and List feeds a
    # loop that STOPS builds. Do not point this at a project that carries other
    # work.
    project: %s
    # WHAT THIS NODE'S GUEST OS IS DERIVED FROM. billet reports it at
    # registration rather than taking a second answer from the config.
    environment_type: %s
%s    # ACKNOWLEDGED, BY YOU. It changes nothing about how billet behaves; it
    # exists so this is read before a tier advertises capacity: a build is capped
    # at %s and a build still waiting for capacity is FAILED after %s. Both are
    # CodeBuild's own limits and billet cannot lift either. Work that can exceed
    # them belongs on owned EC2 or Mac capacity.
    accept_external_build_ceiling: true
    build_timeout_minutes: %d
    queued_timeout_minutes: %d
    # WHERE EACH BUILD'S SINGLE-USE RUNNER REGISTRATION IS WRITTEN, as an SSM
    # SecureString. It is an IAM boundary rather than a naming preference: the
    # node's policy grants PutParameter and DeleteParameter on exactly this path,
    # so a value billet guessed would either be unwritable or wider than the grant
    # you reviewed. Only the parameter's NAME travels in the launch request.
    jit_parameter_path: %s
    # THE IAM POLICY THIS NODE NEEDS IS GENERATED FROM THIS FILE. Run
    # `+"`billet init iam --config <this file> --account <id>`"+` for the node's own
    # policy, and again with --build-role for the project's service role. They are
    # deliberately different principals: the build role runs your workflow and may
    # not start builds.
    #
    # The compute types billet may buy, most preferred first, each DECLARING what
    # it holds — billet ships no table of them. Placement charges the first that
    # fits. VERIFY the prices; a stale one only understates the exposure billet
    # reports and never gates a job.
    compute_types:
%s%s
# Tiers are yours to define. The label is what users put in `+"`runs-on`"+`. Each is
# sized to a compute type above and fits the budget, so a job on it can actually
# be placed and bought.
#
%s
tiers:
%s`,
		intro,
		yamlScalar(p.Listen),
		yamlScalar(paths.serverState),
		p.VCPU, p.Memory,
		scope,
		appID, installationID,
		yamlScalar(paths.keyPath),
		nameBlock,
		yamlScalar(p.Listen),
		yamlScalar(paths.nodeState),
		lockBlock,
		p.VCPU, p.Memory,
		yamlScalar(cb.Region),
		yamlScalar(cb.Project),
		cb.Environment,
		cb.optionalYAML(),
		minutesText(config.CodeBuildBuildCeilingMinutes),
		minutesText(config.CodeBuildQueuedCeilingMinutes),
		cb.BuildTimeoutMinutes,
		cb.QueuedTimeoutMinutes,
		yamlScalar(cb.JITParameterPath),
		codeBuildComputeTypesYAML(cb.ComputeTypes),
		policyBlock,
		codeBuildTierIntro(cb.Environment),
		renderCodeBuildTiers(codeBuildTiers(cb.ComputeTypes, p.VCPU, p.Memory), p, trusted),
	)
}

// minutesText renders a minute count the way an operator thinks about it. It is
// a second copy of cmd/billet's helper because internal/initconfig may not import
// cmd/, and the alternative — exporting one from config — would put a rendering
// concern in the leaf package every other package reads.
func minutesText(minutes int) string {
	if minutes%60 == 0 && minutes >= 60 {
		return fmt.Sprintf("%dh", minutes/60)
	}

	return fmt.Sprintf("%dm", minutes)
}

// codeBuildNodeBlocks renders `node.name` and the `nodes:` policy a macOS
// generation needs, and nothing at all otherwise.
//
// THE POLICY IS NOT DECORATION: without it the host's macOS limit defaults to
// APPLE's two-guests-per-machine allowance, and validateMacOSHostLimits sums
// every pinned tier's max_concurrent against that — so a fleet of four would be
// refused at load, with a diagnostic about a licence that has nothing to do with
// a managed fleet. Setting the limit to the fleet's own capacity is what
// docs/deploying/aws-codebuild.md tells an operator to do; the generation does it
// for them,
// because it is the same number they just supplied.
func codeBuildNodeBlocks(c *CodeBuildParams) (string, string) {
	if c.Environment != config.CodeBuildMacARM {
		// A NAME SUPPLIED HERE IS STILL WRITTEN, and an earlier version dropped
		// it. --node-name decides nothing on a Linux generation — with a
		// certificate the name comes from it, and without one it defaults to this
		// machine's hostname — but "decides nothing" is not "may be discarded":
		// the operator asked for a name, and a generation that accepted it and
		// wrote nothing is exactly the silent discard this backend's own flag
		// refusals exist to prevent.
		if c.NodeName == "" {
			return omittedNameComment, ""
		}

		return fmt.Sprintf("  # The name you gave this orchestrator. With a certificate the\n"+
			"  # name comes from it instead, and the two must agree.\n  name: %s\n",
			yamlScalar(c.NodeName)), ""
	}

	nameBlock := fmt.Sprintf("  # NAMED, because a macOS tier has to pin the host its guest\n"+
		"  # limit is enforced against. This is also the name `billet ca issue` mints\n"+
		"  # a certificate for.\n  name: %s\n", yamlScalar(c.NodeName))

	policyBlock := fmt.Sprintf(`
# WHAT THIS HOST MAY RUN, AND HOW MANY macOS GUESTS AT ONCE.
#
# Apple's two-guests-per-machine allowance does not apply to a managed fleet, and
# billet knows that — the per-host default is Apple's only for a backend that runs
# work on the host itself. Here the cap is your reserved fleet's capacity, which
# is the number below; without it the tier above would be judged against Apple's
# licence and refused at load for a reason that has nothing to do with CodeBuild.
nodes:
  - name: %s
    provider: codebuild
    macos_vm_limit: %d
`, yamlScalar(c.NodeName), c.FleetCapacity)

	return nameBlock, policyBlock
}

// codeBuildTierIntro says what these tiers are, and what they cannot be.
func codeBuildTierIntro(env config.CodeBuildEnvironment) string {
	shared := "# Every tier here is `trust: trusted`, and that is not a setting you can\n" +
		"# change: untrusted work is REFUSED on this backend. AWS documents a\n" +
		"# reserved-capacity instance as staying alive between builds and sharing\n" +
		"# cached data with other projects in the account, by design — so a fork's\n" +
		"# pull request would be arbitrary code on a machine that later runs somebody\n" +
		"# else's build, and no security group fixes that. Run untrusted tiers on\n" +
		"# firecracker or ec2."

	if env == config.CodeBuildMacARM {
		return shared + "\n#\n" +
			"# macOS is reserved capacity ONLY, and a fleet that reports ACTIVE may have\n" +
			"# no Mac behind it yet — builds then sit QUEUED until AWS finds capacity.\n" +
			"# Run `billet check` before enabling this tier: it reports a fleet carrying\n" +
			"# a status context. Warm a new fleet with one build first."
	}

	return shared
}

// renderCodeBuildTiers writes the derived catalogue.
//
// NO `command:` IS WRITTEN, deliberately. config.Tier.RunnerCommandFor already
// answers `./run.sh` for this backend — the path billet's own generated buildspec
// leaves the runner at, since a curated CodeBuild image ships none — and writing
// it here would be a second copy of that decision in every generated file, which
// stops being true the moment the buildspec moves it.
func renderCodeBuildTiers(ts []tier, p Params, trusted bool) string {
	var b strings.Builder

	guestOS := p.CodeBuild.Environment.GuestOS()

	for i, t := range ts {
		if i > 0 {
			b.WriteString("\n")
		}

		// THE LABEL IS QUOTED, and that is not symmetry with the image beside it.
		// It is DERIVED from a compute-type name, and a compute type has no
		// character grammar beyond being non-empty — so `BUILD_CUSTOM #production`
		// produces a label whose tail YAML reads as a COMMENT. The file then loads,
		// carrying a tier called `billet-4vcpu-build-custom`, and Generate's own
		// self-validation cannot see the difference because the document it parses
		// is the truncated one. Quoted, the whole label survives the round trip and
		// config's label grammar refuses it by name.
		fmt.Fprintf(&b, "  - label: %s\n    provider: codebuild\n    guest_os: %s\n"+
			"    vcpu: %d\n    memory: %s\n    image: %s\n",
			yamlScalar(t.label), guestOS, t.vcpu, t.memory, yamlScalar(p.Image))

		// THE FLEET'S CAPACITY, WRITTEN ONLY WHERE IT DECIDES SOMETHING. A macOS
		// tier with no explicit cap inherits Apple's per-machine allowance, which
		// is a claim about hardware nobody here owns.
		if guestOS == config.GuestMacOS {
			// PINNED, BECAUSE THE LIMIT IS PER HOST. Config validation refuses a
			// macOS tier that names no node: a limit cannot be enforced against a
			// tier pinned to nowhere.
			fmt.Fprintf(&b, "    node: %s\n", yamlScalar(p.CodeBuild.NodeName))
			fmt.Fprintf(&b, "    max_concurrent: %d\n", p.CodeBuild.FleetCapacity)
		}

		renderTierPolicy(&b, p, trusted)
	}

	return b.String()
}
