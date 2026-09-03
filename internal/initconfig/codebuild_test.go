package initconfig_test

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
)

// codeBuildShapes is an ordered catalogue in the shape an operator declares one:
// most preferred first, each entry carrying what it holds and what it costs.
//
// The sizes are CodeBuild's own published ones for the general compute types, so
// a reader comparing this to the console sees the same numbers.
func codeBuildShapes() []config.RemoteShape {
	return []config.RemoteShape{
		{Type: "BUILD_GENERAL1_SMALL", VCPU: 2, Memory: 3 * config.GiB, PriceUSDPerHour: 300000},
		{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 4, Memory: 7 * config.GiB, PriceUSDPerHour: 600000},
	}
}

// codeBuildParams is a generation that should succeed, so a case can break
// exactly one thing and the diff between passing and failing is visible.
func codeBuildParams() initconfig.Params {
	return initconfig.Params{
		Org:         "acme",
		Provider:    config.ProviderCodeBuild,
		RunnerGroup: "billet",
		Workflows:   []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"},
		VCPU:        16,
		Memory:      32 * config.GiB,
		GOOS:        "linux",
		CodeBuild: &initconfig.CodeBuildParams{
			Region:           "us-west-2",
			Project:          "billet-runners",
			Environment:      config.CodeBuildLinuxContainer,
			ComputeTypes:     codeBuildShapes(),
			JITParameterPath: "/billet/jit",
			AcceptCeiling:    true,
		},
	}
}

func generate(t *testing.T, p initconfig.Params) string {
	t.Helper()

	body, trusted, err := initconfig.Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	if !trusted {
		t.Fatal("a codebuild generation reported itself untrusted, which this backend refuses")
	}

	return body
}

// A GENERATED CODEBUILD CONFIG LOADS, AND CARRIES WHAT ONLY THE OPERATOR COULD
// HAVE SAID.
//
// Generate already proves the file parses — it round-trips every generation
// through config.Parse before returning it — so what this adds is that the
// values reached the file rather than being defaulted away, which is the failure
// refuseForeignBackendInputs exists for one layer up.
func TestAGeneratedCodeBuildConfigCarriesItsInputs(t *testing.T) {
	t.Parallel()

	cfg := parseGenerated(t, generate(t, codeBuildParams()))

	if cfg.Node == nil || cfg.Node.CodeBuild == nil {
		t.Fatal("the generated config has no node.codebuild block")
	}

	cb := cfg.Node.CodeBuild

	switch {
	case cb.Region != "us-west-2":
		t.Errorf("region is %q", cb.Region)
	case cb.Project != "billet-runners":
		t.Errorf("project is %q", cb.Project)
	case cb.EnvironmentType != config.CodeBuildLinuxContainer:
		t.Errorf("environment_type is %q", cb.EnvironmentType)
	case cb.JITParameterPath != "/billet/jit":
		t.Errorf("jit_parameter_path is %q", cb.JITParameterPath)
	case !cb.AcceptExternalBuildCeiling:
		t.Error("the ceiling acknowledgement did not reach the file")
	}

	// THE ORDER IS THE DECISION. Placement charges the first declared shape that
	// fits, so a catalogue rendered in a different order buys a different machine.
	if len(cb.ComputeTypes) != 2 ||
		cb.ComputeTypes[0].Type != "BUILD_GENERAL1_SMALL" ||
		cb.ComputeTypes[1].Type != "BUILD_GENERAL1_MEDIUM" {
		t.Errorf("the compute types lost their order: %+v", cb.ComputeTypes)
	}

	// AND WHAT THEY HOLD, because billet ships no table of them: a shape whose
	// declared size was dropped would be charged as zero and overcommit a budget
	// nobody can see.
	if cb.ComputeTypes[1].VCPU != 4 || cb.ComputeTypes[1].Memory != 7*config.GiB {
		t.Errorf("a compute type lost its declared size: %+v", cb.ComputeTypes[1])
	}

	if cb.ComputeTypes[1].PriceUSDPerHour == 0 {
		t.Error("a compute type lost its price, so `billet status` would report this fleet " +
			"as costing nothing")
	}

	// THE CEILING IS THE DECLARED BUDGET, not a measurement: there is no machine
	// under this node to withhold headroom from.
	if cfg.Server.MaxVCPU != 16 || cfg.Server.MaxMemory != 32*config.GiB {
		t.Errorf("the ceiling is %d vCPU / %s, want the declared budget unreduced",
			cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}

	if cfg.Node.MaxVCPU != 16 || cfg.Node.MaxMemory != 32*config.GiB {
		t.Errorf("node contribution is %d vCPU / %s, want the declared budget",
			cfg.Node.MaxVCPU, cfg.Node.MaxMemory)
	}
}

// EVERY GENERATED TIER IS TRUSTED, AND CARRIES ITS WHOLE POOL POLICY.
//
// A trusted tier that lost its runner group or an allowlist entry is a pool
// GitHub hands work to from repositories the operator never named — which is
// what renderTierPolicy exists to keep from happening once per renderer.
func TestEveryGeneratedCodeBuildTierIsTrustedAndBound(t *testing.T) {
	t.Parallel()

	cfg := parseGenerated(t, generate(t, codeBuildParams()))

	if len(cfg.Tiers) == 0 {
		t.Fatal("the generation wrote no tiers, so this config schedules nothing")
	}

	for i := range cfg.Tiers {
		tier := &cfg.Tiers[i]

		switch {
		case tier.Trust != config.WorkloadTrusted:
			t.Errorf("tier %s is %s; untrusted work is refused on this backend",
				tier.Label, tier.Trust)
		case tier.RunnerGroup != "billet":
			t.Errorf("tier %s lost its runner group", tier.Label)
		case len(tier.Workflows) != 1:
			t.Errorf("tier %s carries %d workflows, want the one supplied",
				tier.Label, len(tier.Workflows))
		case tier.GuestOS != config.GuestLinux:
			t.Errorf("tier %s has guest_os %s", tier.Label, tier.GuestOS)
		case tier.Image == "":
			t.Errorf("tier %s names no build image, so the project's own would decide",
				tier.Label)
		}
	}
}

// THE CATALOGUE FITS THE BUDGET ALL AT ONCE, NOT EACH TIER ON ITS OWN.
//
// Every tier is its own scale set and every listener escrows one discovery slot
// BEFORE it advertises, so a catalogue whose floor exceeds the ceiling advertises
// zero everywhere and every job queues forever against a control plane reporting
// itself healthy. Checking each candidate against the bare ceiling is what
// produced that, and it passed every test at the time.
func TestTheGeneratedCodeBuildCatalogueFitsItsBudgetTogether(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	// Room for the small shape and not for both.
	p.VCPU, p.Memory = 3, 6*config.GiB

	cfg := parseGenerated(t, generate(t, p))

	var vcpu int

	var memory config.ByteSize

	for i := range cfg.Tiers {
		vcpu += cfg.Tiers[i].VCPU
		memory += cfg.Tiers[i].Memory
	}

	if vcpu > p.VCPU || memory > p.Memory {
		t.Fatalf("the catalogue needs %d vCPU and %s against a budget of %d and %s; every "+
			"tier escrows a discovery slot before it advertises, so this deployment would "+
			"advertise zero on all of them", vcpu, memory, p.VCPU, p.Memory)
	}

	if len(cfg.Tiers) != 1 {
		t.Fatalf("wrote %d tiers, want only the one that fits", len(cfg.Tiers))
	}
}

// AN UNTRUSTED CODEBUILD GENERATION IS REFUSED, and by the flags that would fix
// it.
//
// Not gated on a network setting the way ec2's untrusted refusal is: there is no
// setting that would make this work, because AWS documents a reserved instance as
// staying alive between builds and sharing cached data across projects.
func TestAnUntrustedCodeBuildGenerationIsRefused(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.RunnerGroup, p.Workflows = "", nil

	_, _, err := initconfig.Generate(p)
	if err == nil {
		t.Fatal("a codebuild config with no trusted-pool policy was generated")
	}

	for _, want := range []string{"--runner-group", "--workflow", "REFUSED"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// THE CEILING ACKNOWLEDGEMENT IS REQUIRED, AND BILLET WILL NOT MAKE IT FOR YOU.
//
// It changes nothing about how billet behaves, which is exactly why a generator
// that wrote it on the operator's behalf would defeat the field entirely: the
// whole point is that the sentence is read by a person before a tier advertises
// capacity.
func TestAGenerationWithoutTheCeilingAcknowledgementIsRefused(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.AcceptCeiling = false

	_, _, err := initconfig.Generate(p)
	if err == nil {
		t.Fatal("a codebuild config was generated without acknowledging CodeBuild's ceilings")
	}

	// THE NUMBERS, not just the flag: an operator who has to look them up has not
	// been told anything the field exists to tell them.
	for _, want := range []string{"--accept-external-build-ceiling", "2160", "480"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

// A macOS GENERATION ASKS FOR THE FLEET'S CAPACITY RATHER THAN INHERITING
// APPLE'S ALLOWANCE.
//
// A macOS tier with no explicit max_concurrent inherits its host's limit, which
// defaults to Apple's two-guests-per-machine licence — a rule about hardware
// somebody owns, and not the rule on a managed fleet, where the cap is the fleet's
// capacity and AWS defaults it to ONE. Writing 2 would be billet making a licence
// statement on the operator's behalf that is also wrong.
func TestAMacOSCodeBuildGenerationNeedsItsFleetCapacity(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.Environment = config.CodeBuildMacARM
	p.CodeBuild.FleetARN = "arn:aws:codebuild:us-west-2:123456789012:fleet/macs"
	p.CodeBuild.ComputeTypes = []config.RemoteShape{
		{Type: "BUILD_GENERAL1_MEDIUM", VCPU: 8, Memory: 24 * config.GiB, PriceUSDPerHour: 1200000},
	}

	_, _, err := initconfig.Generate(p)
	if err == nil {
		t.Fatal("a macOS generation was written with no fleet capacity, so its tier would " +
			"have inherited Apple's per-machine allowance")
	}

	if !strings.Contains(err.Error(), "--codebuild-fleet-capacity") {
		t.Errorf("the refusal does not name the flag that fixes it: %v", err)
	}

	// AND IT ALSO NEEDS THE HOST'S NAME, which is config validation's rule rather
	// than the generator's: a macOS tier that names no node cannot have a
	// per-host limit enforced against it. billet does not derive one — a
	// codebuild node is a small machine calling an API, so its hostname says
	// nothing about the fleet the limit is about.
	p.CodeBuild.FleetCapacity = 4

	_, _, err = initconfig.Generate(p)
	if err == nil {
		t.Fatal("a macOS generation was written with no node name, so its tier would have " +
			"been pinned to nowhere")
	}

	if !strings.Contains(err.Error(), "--node-name") {
		t.Errorf("the refusal does not name the flag that fixes it: %v", err)
	}

	// ...and with both, the cap is the operator's number.
	p.CodeBuild.NodeName = "cb-macs-1"

	cfg := parseGenerated(t, generate(t, p))

	if len(cfg.Tiers) != 1 {
		t.Fatalf("wrote %d tiers, want one", len(cfg.Tiers))
	}

	if cfg.Tiers[0].GuestOS != config.GuestMacOS {
		t.Errorf("a MAC_ARM generation wrote guest_os %s", cfg.Tiers[0].GuestOS)
	}

	if cfg.Tiers[0].MaxConcurrent != 4 {
		t.Errorf("the macOS tier caps at %d, want the declared fleet capacity of 4",
			cfg.Tiers[0].MaxConcurrent)
	}

	if cfg.Tiers[0].Node != "cb-macs-1" {
		t.Errorf("the macOS tier is pinned to %q, want the named host", cfg.Tiers[0].Node)
	}

	// AND THE HOST POLICY RAISES THE LIMIT TO THE FLEET'S CAPACITY. Without it
	// the default is APPLE's two-guests-per-machine allowance, and a fleet of four
	// is refused at load with a diagnostic about a licence that has nothing to do
	// with a managed fleet. That this generation LOADED at four is the proof; the
	// policy is asserted too so a regression that dropped it fails here rather
	// than only for an operator whose fleet happens to be larger than two.
	if len(cfg.Nodes) != 1 || cfg.Nodes[0].Name != "cb-macs-1" {
		t.Fatalf("the generation wrote no host policy for the pinned node: %+v", cfg.Nodes)
	}

	if limit := cfg.Nodes[0].MacOSLimit(); limit != 4 {
		t.Errorf("the host's macOS limit is %d, want the declared fleet capacity of 4 — "+
			"Apple's per-machine allowance is not the rule on a managed fleet", limit)
	}
}

// AND IT IS REFUSED WHERE IT DECIDES NOTHING, rather than accepted and ignored.
//
// A Linux tier's concurrency is bounded by node.max_vcpu and node.max_memory, so
// a per-tier cap there would be a second answer to a question the budget already
// answers — and a flag accepted and silently discarded leaves somebody believing
// they asked for something.
func TestFleetCapacityIsRefusedOnANonMacGeneration(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.FleetCapacity = 4

	_, _, err := initconfig.Generate(p)
	if err == nil {
		t.Fatal("a fleet capacity was accepted on a Linux generation, where it decides nothing")
	}

	if !strings.Contains(err.Error(), "MAC_ARM") {
		t.Errorf("the refusal does not say where the flag IS meaningful: %v", err)
	}
}

// CODEBUILD INPUTS ON ANOTHER BACKEND ARE REFUSED, NOT DISCARDED.
//
// Generate is exported and internal/e2e reaches it directly, so a caller passing
// the wrong backend's block used to get a config with every one of those values
// thrown away and no error, while the CLI refused the same combination by flag
// name. Two entry points enforcing different contracts is the shape the
// billet-config skill names.
func TestCodeBuildInputsOnAnotherBackendAreRefused(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.Provider = config.ProviderDocker
	p.Image = "ghcr.io/actions/actions-runner:latest"

	_, _, err := initconfig.Generate(p)
	if err == nil {
		t.Fatal("codebuild inputs were silently discarded on a docker generation")
	}

	if !strings.Contains(err.Error(), "codebuild inputs are set") {
		t.Errorf("the refusal does not name what would have been discarded: %v", err)
	}
}

// THE GPU ENVIRONMENT HAS NO DEFAULT IMAGE, and says so rather than picking one.
//
// Its curated images are versioned against a CUDA release and chosen for a
// workload. Every other environment has one obvious curated image for its
// architecture, which is why only this one asks.
func TestTheGPUEnvironmentRequiresAnExplicitImage(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.Environment = config.CodeBuildLinuxGPUContainer

	_, _, err := initconfig.Generate(p)
	if err == nil {
		t.Fatal("billet picked a GPU image on the operator's behalf")
	}

	if !strings.Contains(err.Error(), "--image") {
		t.Errorf("the refusal does not name the flag that supplies one: %v", err)
	}

	p.Image = "aws/codebuild/linux-gpu-standard:1.0"

	if cfg := parseGenerated(t, generate(t, p)); cfg.Tiers[0].Image != p.Image {
		t.Errorf("the supplied GPU image did not reach the tier: %q", cfg.Tiers[0].Image)
	}
}

// AN ENVIRONMENT WITH NO ON-DEMAND FORM IS REFUSED BY CONFIG'S OWN RULE.
//
// LINUX_EC2, ARM_EC2 and MAC_ARM exist only on reserved capacity, so a generation
// naming one without a fleet cannot ever run a build. Asserted here because
// Generate calls CheckCodeBuild rather than carrying a second opinion about
// acceptable CodeBuild blocks — a mutation removing that call lands on this test.
func TestAReservedOnlyEnvironmentWithoutAFleetIsRefused(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.Environment = config.CodeBuildARMEC2

	_, _, err := initconfig.Generate(p)
	if err == nil {
		t.Fatal("a reserved-only environment was generated with no fleet, so every launch " +
			"would be refused by CodeBuild per job")
	}

	if !strings.Contains(err.Error(), "fleet_arn") {
		t.Errorf("the refusal does not name the field that is missing: %v", err)
	}
}

// PREPARE RUNS BEFORE EVERY DECISION, NOT JUST BEFORE VALIDATION.
//
// Prepare TRIMS, so an untrimmed environment reaching Generate directly had
// validation see the normalized MAC_ARM while every rendering decision — the
// default image, the node pin, the host policy, the tier's guest_os — read the
// raw string and took the LINUX branch. The output validated and described a node
// that registers as macOS advertising Linux tiers, which are then unplaceable.
//
// Reachable only through the exported entry point, because the CLI trims on its
// way in — which is exactly the two-entry-points shape refuseForeignBackendInputs
// exists for, and the reason this test drives Generate rather than the CLI.
func TestAnUntrimmedEnvironmentIsNormalizedBeforeAnythingReadsIt(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.Environment = "  MAC_ARM  "
	p.CodeBuild.FleetARN = "arn:aws:codebuild:us-west-2:123456789012:fleet/macs"
	p.CodeBuild.FleetCapacity = 2
	p.CodeBuild.NodeName = "cb-macs-1"
	p.CodeBuild.ComputeTypes = []config.RemoteShape{{
		Type: "BUILD_GENERAL1_MEDIUM", VCPU: 8, Memory: 24 * config.GiB, PriceUSDPerHour: 1200000,
	}}

	cfg := parseGenerated(t, generate(t, p))

	if cfg.Node.CodeBuild.EnvironmentType != config.CodeBuildMacARM {
		t.Fatalf("the environment reached the file as %q", cfg.Node.CodeBuild.EnvironmentType)
	}

	// THE RENDERING DECISIONS, which are what read the raw value before.
	if cfg.Tiers[0].GuestOS != config.GuestMacOS {
		t.Errorf("the tier's guest_os is %s: a node registering as macOS would advertise a "+
			"Linux tier nothing can place", cfg.Tiers[0].GuestOS)
	}

	if cfg.Tiers[0].Node == "" || cfg.Tiers[0].MaxConcurrent != 2 {
		t.Errorf("the macOS tier was rendered through the Linux branch: node=%q concurrent=%d",
			cfg.Tiers[0].Node, cfg.Tiers[0].MaxConcurrent)
	}

	if len(cfg.Nodes) != 1 {
		t.Errorf("the host policy was not written, so this tier is judged against Apple's "+
			"licence: %+v", cfg.Nodes)
	}

	if cfg.Tiers[0].Image != "aws/codebuild/macos-arm-base:14" {
		t.Errorf("the default image is %q, which is the Linux branch's", cfg.Tiers[0].Image)
	}
}

// A PADDED COMPUTE TYPE DOES NOT REACH A TIER LABEL.
//
// Prepare trims shape names too, and that is the half the provider's own
// constructor was missing before it existed: validation checks a trimmed copy
// while the launch sends the raw string. Here it would also become part of a
// label.
func TestAPaddedComputeTypeIsNormalizedBeforeItBecomesALabel(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.ComputeTypes = []config.RemoteShape{{
		Type: "  BUILD_GENERAL1_MEDIUM  ", VCPU: 4, Memory: 7 * config.GiB,
		PriceUSDPerHour: 600000,
	}}

	cfg := parseGenerated(t, generate(t, p))

	if got := cfg.Node.CodeBuild.ComputeTypes[0].Type; got != "BUILD_GENERAL1_MEDIUM" {
		t.Errorf("the compute type reached the file as %q", got)
	}

	if got := cfg.Tiers[0].Label; got != "billet-4vcpu-build-general1-medium" {
		t.Errorf("the tier label is %q, which carries the padding", got)
	}
}

// A COMPUTE-TYPE NAME CANNOT COMMENT OUT THE REST OF ITS OWN LABEL.
//
// A compute type has no character grammar beyond being non-empty, and the label
// is DERIVED from it — so an unquoted `BUILD_CUSTOM #production` produces a line
// whose tail YAML reads as a comment. The file then loads carrying a shorter
// label, and Generate's own self-validation cannot see the difference, because
// the document it parses is the truncated one. Quoted, the whole label survives
// the round trip and config's label grammar refuses it by name.
func TestAComputeTypeCannotCommentOutItsOwnTierLabel(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.ComputeTypes = []config.RemoteShape{{
		Type: "BUILD_CUSTOM #production", VCPU: 4, Memory: 7 * config.GiB,
		PriceUSDPerHour: 600000,
	}}

	_, _, err := initconfig.Generate(p)
	if err == nil {
		t.Fatal("a compute type containing a YAML comment marker produced a config that " +
			"loaded, carrying a tier label that is not the one the file appears to name")
	}
}

// A NODE NAME IS WRITTEN WHEREVER IT IS GIVEN, not only where it is required.
//
// It decides nothing on a Linux generation — with a certificate the name comes
// from it, and without one it defaults to this machine's hostname — but "decides
// nothing" is not "may be discarded". An earlier version accepted the name and
// wrote nothing, which is the silent discard this backend's own flag refusals
// exist to prevent.
func TestANodeNameOnALinuxGenerationIsWrittenRatherThanDropped(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.NodeName = "cb-linux-1"

	cfg := parseGenerated(t, generate(t, p))

	if cfg.Node.Name != "cb-linux-1" {
		t.Fatalf("node.name is %q; the name the operator supplied was discarded", cfg.Node.Name)
	}

	// ...and it is still VALIDATED, because it is what the control plane
	// authorises and what `billet ca issue` mints a certificate for.
	p.CodeBuild.NodeName = "Not A Legal Node Name"

	if _, _, err := initconfig.Generate(p); err == nil {
		t.Error("an illegal node name was written into the file for a later command to meet")
	}
}

// THE CATALOGUE'S FLOOR IS WHAT PLACEMENT WILL ACTUALLY CHARGE.
//
// Placement walks the operator's ordered catalogue and charges the FIRST entry
// that fits the tier — that is what the order is for — so a tier derived from a
// small shape listed after a large one is charged the large one. A floor summed
// from the tier REQUESTS therefore understates what the deployment holds, and the
// tier it lets through has no node that can afford it: it advertises, and nothing
// can ever be placed on it.
func TestTheCatalogueIsCostedAtTheShapePlacementWouldCharge(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	// The preferred shape is too big for the budget; the second fits it on its
	// own, and would be CHARGED the first, which does not.
	p.CodeBuild.ComputeTypes = []config.RemoteShape{
		{Type: "BUILD_GENERAL1_LARGE", VCPU: 8, Memory: 15 * config.GiB, PriceUSDPerHour: 2000000},
		{Type: "BUILD_GENERAL1_SMALL", VCPU: 2, Memory: 3 * config.GiB, PriceUSDPerHour: 500000},
	}
	p.VCPU, p.Memory = 4, 6*config.GiB

	cfg := parseGenerated(t, generate(t, p))

	if len(cfg.Tiers) != 0 {
		t.Fatalf("wrote %d tier(s) against a budget that cannot hold what placement would "+
			"charge for any of them: %+v", len(cfg.Tiers), cfg.Tiers)
	}
}

// ...AND A CATALOGUE THE BUDGET CAN AFFORD STILL PRODUCES TIERS, which is the
// other direction and keeps the rule above from being a refusal of everything.
func TestAnAffordableCatalogueStillProducesTiers(t *testing.T) {
	t.Parallel()

	p := codeBuildParams()
	p.CodeBuild.ComputeTypes = []config.RemoteShape{
		{Type: "BUILD_GENERAL1_SMALL", VCPU: 2, Memory: 3 * config.GiB, PriceUSDPerHour: 500000},
		{Type: "BUILD_GENERAL1_LARGE", VCPU: 8, Memory: 15 * config.GiB, PriceUSDPerHour: 2000000},
	}
	p.VCPU, p.Memory = 16, 32*config.GiB

	cfg := parseGenerated(t, generate(t, p))

	if len(cfg.Tiers) != 2 {
		t.Fatalf("wrote %d tiers, want both: %+v", len(cfg.Tiers), cfg.Tiers)
	}
}

// parseGenerated loads a generation the way a file on disk is loaded.
//
// THROUGH config.Parse, because that is the single validation path and the only
// thing that proves the generated text means what the test thinks it says.
func parseGenerated(t *testing.T, body string) *config.Config {
	t.Helper()

	// The App ids are zero in a real generation — `github-app create` fills them —
	// and config validation requires them, so they are supplied here exactly as
	// Generate's own self-validation supplies them.
	body = strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	body = strings.Replace(body, "installation_id: 0", "installation_id: 1", 1)

	cfg, err := config.Parse("generated", []byte(body))
	if err != nil {
		t.Fatalf("the generated config does not load: %v\n\n%s", err, body)
	}

	return cfg
}
