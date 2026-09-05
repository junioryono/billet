package initconfig

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

const (
	trialGroup    = "billet-trial"
	trialWorkflow = "acme/repo/.github/workflows/ci.yml@refs/heads/main"
)

func dockerParams() Params {
	return Params{
		Org:         "acme",
		Provider:    config.ProviderDocker,
		Image:       DefaultRunnerImage,
		VCPU:        8,
		Memory:      16 * config.GiB,
		RunnerGroup: trialGroup,
		Workflows:   []string{trialWorkflow},
	}
}

// A GENERATED DOCKER CONFIG IS TRUSTED AND BOUND TO A POLICY, or it does not run.
func TestGenerateDockerIsTrustedAndAllowlisted(t *testing.T) {
	body, _, err := Generate(dockerParams())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// It validates through the config package's own path once the App exists.
	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)
	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("the generated config does not load: %v\n\n%s", err, body)
	}

	if len(cfg.Tiers) == 0 {
		t.Fatal("no tiers")
	}
	for i := range cfg.Tiers {
		tier := &cfg.Tiers[i]
		if tier.Trust != config.WorkloadTrusted {
			t.Errorf("tier %q trust %q, want trusted", tier.Label, tier.Trust)
		}
		if tier.RunnerGroup != trialGroup {
			t.Errorf("tier %q runner group %q, want %q", tier.Label, tier.RunnerGroup, trialGroup)
		}
		if len(tier.Workflows) != 1 || tier.Workflows[0] != trialWorkflow {
			t.Errorf("tier %q workflows %v", tier.Label, tier.Workflows)
		}
	}
}

// A DOCKER TRIAL WITH NO POLICY IS REFUSED before anything is written.
func TestGenerateDockerRefusesNoPolicy(t *testing.T) {
	p := dockerParams()
	p.RunnerGroup = ""
	p.Workflows = nil

	if _, _, err := Generate(p); err == nil {
		t.Fatal("Generate accepted a docker trial with no policy")
	} else if !strings.Contains(err.Error(), "--runner-group") || !strings.Contains(err.Error(), "--workflow") {
		t.Errorf("the refusal does not name the flags: %v", err)
	}
}

// THE POLICY IS VALIDATED BY THE FLAG, so a bad ref is caught here.
func TestGenerateDockerRefusesAMalformedWorkflow(t *testing.T) {
	p := dockerParams()
	p.Workflows = []string{"not-a-workflow-ref"}

	if _, _, err := Generate(p); err == nil {
		t.Fatal("Generate accepted a malformed workflow ref")
	} else if !strings.Contains(err.Error(), "--workflow") {
		t.Errorf("the refusal does not name --workflow: %v", err)
	}
}

// A BLANK-BUT-PRESENT RUNNER GROUP IS REFUSED, not read as absent.
//
// A group of only whitespace trims to empty, and if that were treated as "no
// policy" a firecracker host would silently emit untrusted tiers while a docker
// host would be told --runner-group was missing when it was supplied blank. It
// is caught by the flag, for both providers.
func TestGenerateRefusesABlankRunnerGroup(t *testing.T) {
	// Asserted on "whitespace", not merely "--runner-group": a blank group with a
	// workflow is ALSO caught by the partial-policy check, whose message likewise
	// names --runner-group, and a blank group with no workflow on firecracker
	// would otherwise generate untrusted tiers with NO error at all. Only the
	// dedicated guard produces this diagnostic, so only this assertion has teeth.
	for _, provider := range []config.ProviderKind{config.ProviderDocker, config.ProviderFirecracker} {
		// with and without a workflow: without one, a firecracker blank group is
		// silently untrusted rather than a partial policy, so the guard is the only
		// thing that refuses it.
		for _, workflows := range [][]string{{trialWorkflow}, nil} {
			p := firecrackerParams()
			p.Provider = provider
			p.RunnerGroup = "   "
			p.Workflows = workflows

			if _, _, err := Generate(p); err == nil {
				t.Errorf("%s: Generate accepted a blank runner group (workflows=%v)", provider, workflows)
			} else if !strings.Contains(err.Error(), "whitespace") {
				t.Errorf("%s: the refusal does not name the blank group (workflows=%v): %v",
					provider, workflows, err)
			}
		}
	}
}

// AN IMAGE WITH ANY WHITESPACE OR CONTROL CHARACTER IS REFUSED. config.Parse
// checks only that the image is non-empty, so a padded, interior-spaced, tabbed,
// or non-breaking-spaced reference would validate and then fail to launch as an
// unresolvable reference. Neither a docker reference nor a firecracker generation
// name contains whitespace, so the whole class is caught — not just outer
// padding. Exact empty is left alone; it is the per-provider default signal,
// proven by the defaulting tests above.
func TestGenerateRefusesAnImageWithWhitespace(t *testing.T) {
	images := map[string]string{
		"trailing padding": "ghcr.io/actions/actions-runner:latest ",
		"leading padding":  " ghcr.io/actions/actions-runner:latest",
		"interior space":   "ghcr.io/actions /actions-runner:latest",
		"interior tab":     "ghcr.io/actions	/actions-runner:latest",
		"non-breaking":     "ghcr.io/actions /actions-runner:latest",
		"control char":     "ghcr.io/actions\a/actions-runner:latest",
	}
	for _, provider := range []config.ProviderKind{config.ProviderDocker, config.ProviderFirecracker} {
		for name, img := range images {
			t.Run(string(provider)+"/"+name, func(t *testing.T) {
				p := dockerParams()
				p.Provider = provider
				p.Image = img

				if _, _, err := Generate(p); err == nil {
					t.Fatalf("Generate accepted image %q for %s", img, provider)
				} else if !strings.Contains(err.Error(), "--image") {
					t.Errorf("the refusal does not name --image: %v", err)
				}
			})
		}
	}
}

// GENERATE PROVES THE RENDERED CONFIG VALIDATES, and returns ITS error.
//
// A parameter that passes every flag guard but renders a config config.Parse
// rejects — here a zero-memory ceiling — must be caught by Generate's own
// config.Parse round trip, not left for the operator's next `billet check`. If
// Generate returned the body without validating, this would surface only later.
func TestGenerateSelfValidatesTheRenderedConfig(t *testing.T) {
	p := dockerParams()
	p.Memory = 0 // renders max_memory: 0, which config rejects

	if _, _, err := Generate(p); err == nil {
		t.Fatal("Generate returned a config it never validated")
	} else if !strings.Contains(err.Error(), "not valid") {
		t.Errorf("the error is not the self-validation error: %v", err)
	}
}

// AN OPERATOR SCALAR SHAPED LIKE app_id: 0 DOES NOT STEER THE PLACEHOLDERS.
//
// Validation renders a SEPARATE config with non-zero App ids rather than
// substring-replacing the returned body. The org is rendered before the App id
// keys, so an org that literally contains `app_id: 0` would, under the old
// substring approach, capture the replacement — leaving the real app_id at zero
// and failing self-validation. Generation must instead succeed with both
// placeholders intact.
func TestGenerateSurvivesAnAppIdShapedOrg(t *testing.T) {
	p := dockerParams()
	p.Org = "app_id: 0"

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate refused an app_id-shaped org: %v", err)
	}
	for _, want := range []string{"app_id: 0", "installation_id: 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("the returned config lost the placeholder %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "app_id: 1") {
		t.Errorf("a validation-only app id leaked into the returned config:\n%s", body)
	}
}

// USER-SUPPLIED SCALARS SURVIVE A YAML ROUND TRIP.
//
// A runner group name with a colon renders raw as a mapping key and breaks the
// file; a workflow ref is safe but is quoted here anyway by the same library
// that reads it. The proof is that the config loads and the values come back
// unchanged.
func TestGenerateQuotesUnsafeScalars(t *testing.T) {
	p := dockerParams()
	// A legal-to-billet runner group whose ':' YAML would otherwise read as a
	// key/value separator. checkRunnerGroup permits ':' (only &#;%+ are unsafe).
	p.RunnerGroup = "team: platform"

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)
	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("a config with a colon in the runner group does not load: %v\n\n%s", err, body)
	}

	if cfg.Tiers[0].RunnerGroup != "team: platform" {
		t.Errorf("the runner group came back as %q, not %q", cfg.Tiers[0].RunnerGroup, "team: platform")
	}
}

// THE ORG SURVIVES A YAML ROUND TRIP, unquoted values notwithstanding.
//
// An org rendered raw is at the mercy of YAML's indicator characters: `@` cannot
// start a plain scalar at all, so `org: @acme` is a parse error rather than a
// value. It goes through yamlScalar, so it comes back whole.
//
// The `#` case this test used to carry — `org: acme # prod` parsing to `acme`, a
// different organization — is now refused by config.CheckOrg before any of this,
// and TestGenerateRefusesAnOrgThatNamesSomethingElse is where that lives.
func TestGenerateQuotesTheOrg(t *testing.T) {
	p := dockerParams()
	p.Org = "@acme"

	cfg := parse(t, mustGenerate(t, p))

	if cfg.GitHub.Org != "@acme" {
		t.Errorf("org came back as %q, not the whole value", cfg.GitHub.Org)
	}
}

// AN ORG THAT WOULD NAME A DIFFERENT ORGANIZATION IS REFUSED BY ITS OWN FLAG.
//
// billet builds the scale-set client's config URL as "https://github.com/" plus
// this value, unescaped, so `acme # prod` resolves to the organization `acme `
// and `acme/x` resolves to a REPOSITORY — neither reports anything. Refusing at
// the flag is what keeps the answer next to the thing the operator typed;
// letting it through would surface as a load error blaming a file billet wrote.
func TestGenerateRefusesAnOrgThatNamesSomethingElse(t *testing.T) {
	t.Parallel()

	for name, org := range map[string]string{
		"a comment":     "acme # prod",
		"a repository":  "acme/api",
		"a query":       "acme?x",
		"an escape":     "acme%41",
		"padding":       " acme ",
		"only spaces":   "   ",
		"a control run": "acme\nprod",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			p := dockerParams()
			p.Org = org

			_, _, err := Generate(p)
			if err == nil {
				t.Fatalf("Generate accepted --org %q", org)
			}
			if !strings.Contains(err.Error(), "--org") {
				t.Errorf("the refusal does not name the flag that carried it: %v", err)
			}
		})
	}
}

// A PADDED RUNNER GROUP IS NORMALIZED, so the value validated is the value
// written. Trailing whitespace would otherwise pass the trimmed flag check and
// then be rendered raw, producing a config that names a group GitHub does not
// have.
func TestGenerateNormalizesRunnerGroupPadding(t *testing.T) {
	p := dockerParams()
	p.RunnerGroup = "  billet-trial  "

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)
	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if cfg.Tiers[0].RunnerGroup != "billet-trial" {
		t.Errorf("runner group is %q, want the trimmed %q", cfg.Tiers[0].RunnerGroup, "billet-trial")
	}
}

func firecrackerParams() Params {
	// Image is omitted on purpose, so the test exercises the per-provider
	// defaulting rather than asserting a value it supplied.
	return Params{
		Org:      "acme",
		Provider: config.ProviderFirecracker,
		VCPU:     8,
		Memory:   16 * config.GiB,
	}
}

func parse(t *testing.T, body string) *config.Config {
	t.Helper()

	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)
	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("the generated config does not load: %v\n\n%s", err, body)
	}

	return cfg
}

// A GENERATED FIRECRACKER CONFIG IS UNTRUSTED, NAMES ITS HOST PREP, AND LOADS.
//
// firecracker gives every tier its own kernel, so untrusted is its safe default
// and no policy is required — the opposite of docker. The host prep billet
// cannot do (a bridge, an untrusted bridge, two Ceph pools) must be named in the
// file so `billet check` and the operator can act on it.
func TestGenerateFirecrackerIsUntrusted(t *testing.T) {
	cfg := parse(t, mustGenerate(t, firecrackerParams()))

	if cfg.Node == nil || cfg.Node.Firecracker == nil || cfg.Node.Ceph == nil {
		t.Fatalf("the firecracker/ceph host blocks are missing: %+v", cfg.Node)
	}
	if cfg.Node.Firecracker.Bridge == "" || cfg.Node.Firecracker.UntrustedBridge == "" {
		t.Errorf("the bridges are not both named: %+v", cfg.Node.Firecracker)
	}
	if cfg.Node.Ceph.ImagePool == cfg.Node.Ceph.CachePool {
		t.Errorf("image and cache pools must differ: %q", cfg.Node.Ceph.ImagePool)
	}

	if len(cfg.Tiers) == 0 {
		t.Fatal("no tiers")
	}
	for i := range cfg.Tiers {
		tier := &cfg.Tiers[i]
		if tier.Provider != config.ProviderFirecracker {
			t.Errorf("tier %q provider %q, want firecracker", tier.Label, tier.Provider)
		}
		// The image defaulted, because firecrackerParams did not set one: a golden
		// generation, never the docker runner container.
		if tier.Image != DefaultFirecrackerImage {
			t.Errorf("tier %q image %q, want the defaulted %q", tier.Label, tier.Image, DefaultFirecrackerImage)
		}
		if tier.Trust != config.WorkloadUntrusted {
			t.Errorf("tier %q trust %q, want untrusted", tier.Label, tier.Trust)
		}
		if tier.RunnerGroup != "" || len(tier.Workflows) != 0 {
			t.Errorf("tier %q carries a policy it was not given: %+v", tier.Label, tier)
		}
	}
}

// FIRECRACKER ACCEPTS A TRUSTED POLICY WHEN GIVEN ONE, unlike docker it does not
// require it. Given a runner group and workflow allowlist, its tiers come out
// trusted and bound to that exact policy — the whole catalogue, not an extra
// tier beside an untrusted default.
func TestGenerateFirecrackerAcceptsAPolicy(t *testing.T) {
	p := firecrackerParams()
	p.RunnerGroup = trialGroup
	p.Workflows = []string{trialWorkflow}

	cfg := parse(t, mustGenerate(t, p))

	for i := range cfg.Tiers {
		tier := &cfg.Tiers[i]
		if tier.Trust != config.WorkloadTrusted {
			t.Errorf("tier %q trust %q, want trusted", tier.Label, tier.Trust)
		}
		if tier.RunnerGroup != trialGroup {
			t.Errorf("tier %q runner group %q, want %q", tier.Label, tier.RunnerGroup, trialGroup)
		}
		// The exact allowlist, not merely a non-empty one: a regression that
		// substituted another valid ref would otherwise pass.
		if !slices.Equal(tier.Workflows, []string{trialWorkflow}) {
			t.Errorf("tier %q workflows %v, want [%q]", tier.Label, tier.Workflows, trialWorkflow)
		}
	}
}

// THE RETURNED FILE KEEPS THE ZERO APP-ID PLACEHOLDERS.
//
// Generate validates a SEPARATE render carrying non-zero App ids, so the file it
// returns must still leave app_id/installation_id at zero for `billet github-app
// create` to fill. A regression that returned the validation render — or rendered
// the real body with non-zero ids — would otherwise load fine and only surface
// when the operator's App never took effect.
func TestGenerateLeavesTheAppIdsAtZero(t *testing.T) {
	body := mustGenerate(t, dockerParams())

	for _, want := range []string{"app_id: 0", "installation_id: 0"} {
		if !strings.Contains(body, want) {
			t.Errorf("the returned config does not carry the placeholder %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "app_id: 1") || strings.Contains(body, "installation_id: 1") {
		t.Errorf("the returned config carries a validation-only non-zero App id:\n%s", body)
	}
}

// THE CEILING LEAVES HEADROOM, AND vCPU NEVER GOES BELOW ONE FOR A REAL READING.
//
// Two qualifications, both of which an earlier wording got wrong. Memory can and
// does reach zero: a host too small to spend a whole GiB after its reservation is
// refused by config.Parse rather than handed a budget it cannot place a tier
// under. And the vCPU floor holds only for a POSITIVE detection — zero or fewer
// vCPU is not a small machine, it is a reading that never happened, and it
// returns zero rather than being floored up to more than was detected.
func TestCeilings(t *testing.T) {
	if got := CeilingVCPU(1); got != 1 {
		t.Errorf("CeilingVCPU(1) = %d, want 1 (never zero)", got)
	}
	if got := CeilingVCPU(8); got != 8-HeadroomVCPU {
		t.Errorf("CeilingVCPU(8) = %d, want %d", got, 8-HeadroomVCPU)
	}
	// The reservation is CAPPED at half the machine, so a host smaller than the
	// flat floor still gets a budget rather than the whole amount — which would
	// withhold nothing from the kernel — or nothing at all, which would break
	// init's commitment to write a config on any size of machine.
	if got := CeilingMemory(2 * config.GiB); got != config.GiB {
		t.Errorf("CeilingMemory below the floor = %s, want half the machine", got)
	}
	if got := CeilingMemory(64 * config.GiB); got != 64*config.GiB-HeadroomMemory {
		t.Errorf("CeilingMemory(64GiB) = %s", got)
	}
}

func mustGenerate(t *testing.T, p Params) string {
	t.Helper()

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	return body
}

func ec2Shapes() []config.EC2InstanceType {
	return []config.EC2InstanceType{
		{Type: "c7i.xlarge", VCPU: 4, Memory: 8 * config.GiB, PriceUSDPerHour: 170000},
		{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB, PriceUSDPerHour: 340000},
	}
}

func ec2Params() Params {
	return Params{
		Org:      "acme",
		Provider: config.ProviderEC2,
		VCPU:     16,
		Memory:   32 * config.GiB,
		EC2: &EC2Params{
			Region:                  "us-west-2",
			SubnetID:                "subnet-0abc",
			SecurityGroups:          []string{"sg-trusted"},
			UntrustedSecurityGroups: []string{"sg-untrusted"},
			Shapes:                  ec2Shapes(),
		},
	}
}

func loadGenerated(t *testing.T, body string) *config.Config {
	t.Helper()

	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)
	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("the generated config does not load: %v\n\n%s", err, body)
	}

	return cfg
}

// A TARGET BILLET SHIPS NO SERVICES FOR IS REFUSED, NOT RENDERED AS LINUX.
//
// Everything downstream reads GOOS as a binary — paths, serviceUnits and
// lockBlock all ask "is it darwin" and treat every other value as Linux — so
// `GOOS: "windows"` produced /etc/billet, /var/lib/billet and systemd prose for a
// platform with neither. The file validates and describes a machine that cannot
// run it, which is the shape this generator exists to remove.
//
// Unreachable through the CLI, which refuses the same thing by flag before it
// gets here; this is the half an exported caller can reach.
func TestGenerateRefusesATargetBilletShipsNoServicesFor(t *testing.T) {
	t.Parallel()

	p := dockerParams()
	p.Profile = ProfileLocalService
	p.GOOS = "windows"

	body, _, err := Generate(p)
	if err == nil {
		t.Fatalf("a service-shape config was generated for windows\n\n%s", body)
	}

	for _, want := range []string{"windows", "systemd", "launch agents"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Generate = %v, want it to mention %q", err, want)
		}
	}

	// AND BOTH PLATFORMS BILLET DOES SHIP FOR STILL GENERATE, which is what keeps
	// this a rule about the two shapes rather than a ban on naming a target.
	for _, goos := range []string{"linux", "darwin"} {
		p.GOOS = goos
		if _, _, err := Generate(p); err != nil {
			t.Errorf("a %s service generation was refused: %v", goos, err)
		}
	}
}

// AN EC2 CONFIG WITH NO POLICY IS UNTRUSTED AND LOADS, carrying the ec2 block,
// the shapes it may buy, and a tier per shape on the untrusted security group.
func TestGenerateEC2UntrustedLoads(t *testing.T) {
	body, trusted, err := Generate(ec2Params())
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if trusted {
		t.Fatal("an ec2 config with no policy should be untrusted")
	}

	cfg := loadGenerated(t, body)

	if cfg.Node == nil || cfg.Node.Provider != config.ProviderEC2 || cfg.Node.EC2 == nil {
		t.Fatalf("node is not an ec2 backend: %+v", cfg.Node)
	}
	if cfg.Node.MaxVCPU != 16 || cfg.Node.MaxMemory != 32*config.GiB {
		t.Errorf("node budget = %d vCPU, %s", cfg.Node.MaxVCPU, cfg.Node.MaxMemory)
	}
	if cfg.Server.MaxVCPU != 16 || cfg.Server.MaxMemory != 32*config.GiB {
		t.Errorf("server ceiling = %d vCPU, %s", cfg.Server.MaxVCPU, cfg.Server.MaxMemory)
	}
	if len(cfg.Node.EC2.InstanceTypes) != 2 {
		t.Fatalf("shapes = %+v", cfg.Node.EC2.InstanceTypes)
	}
	if got := cfg.Node.EC2.InstanceTypes[0]; got.Type != "c7i.xlarge" || got.VCPU != 4 ||
		got.Memory != 8*config.GiB || got.PriceUSDPerHour != 170000 {
		t.Errorf("shape[0] = %+v", got)
	}
	if len(cfg.Node.EC2.UntrustedSecurityGroupIDs) != 1 {
		t.Errorf("untrusted groups = %v", cfg.Node.EC2.UntrustedSecurityGroupIDs)
	}

	// One tier per shape, sized to the shape, in order.
	want := []struct {
		label  string
		vcpu   int
		memory config.ByteSize
	}{
		{"billet-ec2-4vcpu", 4, 8 * config.GiB},
		{"billet-ec2-8vcpu", 8, 16 * config.GiB},
	}
	if len(cfg.Tiers) != len(want) {
		t.Fatalf("got %d tiers, want %d: %+v", len(cfg.Tiers), len(want), cfg.Tiers)
	}
	for i, w := range want {
		tr := &cfg.Tiers[i]
		if tr.Label != w.label || tr.VCPU != w.vcpu || tr.Memory != w.memory {
			t.Errorf("tier[%d] = {%s %d %s}, want {%s %d %s}",
				i, tr.Label, tr.VCPU, tr.Memory, w.label, w.vcpu, w.memory)
		}
		if tr.Trust != config.WorkloadUntrusted {
			t.Errorf("tier %q trust %q, want untrusted", tr.Label, tr.Trust)
		}
		if tr.Image != PlaceholderAMI {
			t.Errorf("tier %q image %q, want the placeholder AMI", tr.Label, tr.Image)
		}
	}
}

// AN EC2 CONFIG WITH A POLICY IS TRUSTED and needs no untrusted group.
func TestGenerateEC2TrustedLoads(t *testing.T) {
	p := ec2Params()
	p.RunnerGroup = trialGroup
	p.Workflows = []string{trialWorkflow}
	p.EC2.UntrustedSecurityGroups = nil

	body, trusted, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if !trusted {
		t.Fatal("a policy should make the ec2 pool trusted")
	}

	cfg := loadGenerated(t, body)
	if len(cfg.Tiers) != 2 {
		t.Fatalf("got %d tiers, want 2: %+v", len(cfg.Tiers), cfg.Tiers)
	}
	for i := range cfg.Tiers {
		tr := &cfg.Tiers[i]
		if tr.Trust != config.WorkloadTrusted || tr.RunnerGroup != trialGroup ||
			len(tr.Workflows) != 1 || tr.Workflows[0] != trialWorkflow {
			t.Errorf("tier %q not trusted+bound: %+v", tr.Label, tr)
		}
	}
}

// AN UNTRUSTED EC2 TRIAL WITH NO UNTRUSTED GROUP IS REFUSED, by the flag.
func TestGenerateEC2UntrustedNeedsAGroup(t *testing.T) {
	p := ec2Params()
	p.EC2.UntrustedSecurityGroups = nil

	if _, _, err := Generate(p); err == nil {
		t.Fatal("Generate accepted an untrusted ec2 trial with no untrusted group")
	} else if !strings.Contains(err.Error(), "--untrusted-security-group") {
		t.Errorf("refusal does not name the flag: %v", err)
	}
}

// A BUDGET SMALLER THAN EVERY SHAPE places nothing and is refused.
func TestGenerateEC2RefusesABudgetNoShapeFits(t *testing.T) {
	p := ec2Params()
	p.VCPU = 2
	p.Memory = 4 * config.GiB // smaller than the 4-vCPU/8GiB smallest shape

	if _, _, err := Generate(p); err == nil {
		t.Fatal("Generate accepted a budget no shape fits")
	} else if !strings.Contains(err.Error(), "no declared shape fits") {
		t.Errorf("wrong refusal: %v", err)
	}
}

// A SHAPE LARGER THAN THE BUDGET IS DROPPED from the tier catalogue rather than
// rendered into a tier validation would refuse.
func TestGenerateEC2DropsOversizeShapes(t *testing.T) {
	p := ec2Params()
	p.VCPU = 4
	p.Memory = 8 * config.GiB // only the 4-vCPU shape fits

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := loadGenerated(t, body)
	if len(cfg.Tiers) != 1 || cfg.Tiers[0].Label != "billet-ec2-4vcpu" || cfg.Tiers[0].VCPU != 4 {
		t.Fatalf("tiers = %+v, want one 4-vCPU tier", cfg.Tiers)
	}
	// Both shapes are still declared — the budget bounds the fleet, not what the
	// operator may later buy by adding a bigger tier.
	names := []string{}
	for _, s := range cfg.Node.EC2.InstanceTypes {
		names = append(names, s.Type)
	}
	if len(names) != 2 || names[0] != "c7i.xlarge" || names[1] != "c7i.2xlarge" {
		t.Errorf("declared shapes = %v, want both retained by name", names)
	}
}

// A SHAPE THAT FITS THE VCPU BUDGET BUT NOT THE MEMORY BUDGET IS ALSO DROPPED —
// the memory leg of the filter, which a vcpu-only case would leave untested.
func TestGenerateEC2DropsAShapeOverTheMemoryBudget(t *testing.T) {
	p := ec2Params()
	p.VCPU = 8
	p.Memory = 8 * config.GiB // fits 8 vCPU, but the 8-vCPU shape needs 16GiB
	p.EC2.Shapes = []config.EC2InstanceType{
		{Type: "c7i.xlarge", VCPU: 4, Memory: 8 * config.GiB, PriceUSDPerHour: 170000},
		{Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB, PriceUSDPerHour: 340000},
	}

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := loadGenerated(t, body)
	if len(cfg.Tiers) != 1 || cfg.Tiers[0].Label != "billet-ec2-4vcpu" {
		t.Fatalf("memory-oversize shape was not dropped: %+v", cfg.Tiers)
	}
}

// SHAPES SHARING A VCPU COLLAPSE TO ONE TIER, because a tier label must be unique;
// both shapes stay declared so placement can still pick the smaller fitting one.
func TestGenerateEC2CollapsesEqualVCPUShapes(t *testing.T) {
	p := ec2Params()
	p.EC2.Shapes = []config.EC2InstanceType{
		{Type: "c7i.xlarge", VCPU: 4, Memory: 8 * config.GiB, PriceUSDPerHour: 170000},
		{Type: "r7i.xlarge", VCPU: 4, Memory: 32 * config.GiB, PriceUSDPerHour: 264600},
	}

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := loadGenerated(t, body)
	if len(cfg.Tiers) != 1 || cfg.Tiers[0].Label != "billet-ec2-4vcpu" {
		t.Fatalf("equal-vCPU shapes did not collapse to one tier: %+v", cfg.Tiers)
	}
	if len(cfg.Node.EC2.InstanceTypes) != 2 {
		t.Errorf("a shape was dropped from the declaration: %+v", cfg.Node.EC2.InstanceTypes)
	}
}

// A FRACTIONAL-GiB SHAPE AND A SIX-DECIMAL PRICE ROUND-TRIP THROUGH THE RENDERER:
// the generated YAML must re-parse to the exact memory and price, or a copied
// number would silently drift.
func TestGenerateEC2FractionalMemoryAndPriceRoundTrip(t *testing.T) {
	p := ec2Params()
	p.VCPU = 2
	p.Memory = 4 * config.GiB
	p.EC2.Shapes = []config.EC2InstanceType{
		{Type: "t3.nano", VCPU: 2, Memory: 512 * config.MiB, PriceUSDPerHour: 170001}, // 0.170001
	}

	body, _, err := Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	cfg := loadGenerated(t, body)
	if len(cfg.Node.EC2.InstanceTypes) != 1 {
		t.Fatalf("shapes = %+v", cfg.Node.EC2.InstanceTypes)
	}
	got := cfg.Node.EC2.InstanceTypes[0]
	if got.Memory != 512*config.MiB {
		t.Errorf("memory round-trip = %s, want 512MiB", got.Memory)
	}
	if got.PriceUSDPerHour != 170001 {
		t.Errorf("price round-trip = %v, want 170001 (0.170001)", got.PriceUSDPerHour)
	}
	if len(cfg.Tiers) != 1 || cfg.Tiers[0].Memory != 512*config.MiB {
		t.Errorf("tier memory = %+v, want a 512MiB tier", cfg.Tiers)
	}
}

// THE UNTRUSTED_BRIDGE COMMENT MUST DESCRIBE THE TIERS THIS RUN WROTE. It is
// advice about whether the bridge can be removed, and it was fixed text: a
// trusted-only generation claimed removing it "refuses every job this file
// schedules", the reverse of true, since nothing in that file attaches to the
// bridge at all — sending an operator to build a second bridge for nothing.
//
// Asserted on the generated TEXT, not the parsed config, because a comment is
// the only part of a config that survives nowhere else.
func TestTheUntrustedBridgeCommentMatchesTheTiersItShipsWith(t *testing.T) {
	untrusted := mustGenerate(t, firecrackerParams())

	trustedParams := firecrackerParams()
	trustedParams.RunnerGroup = trialGroup
	trustedParams.Workflows = []string{trialWorkflow}
	trusted := mustGenerate(t, trustedParams)

	const (
		refusesEveryJob = "refuses every job\n    # this file schedules"
		nothingAttaches = "nothing here attaches to it yet"
	)

	if !strings.Contains(untrusted, refusesEveryJob) {
		t.Error("an untrusted generation does not say that removing the bridge refuses " +
			"every job it schedules, which for untrusted tiers is exactly what happens")
	}

	if strings.Contains(untrusted, nothingAttaches) {
		t.Error("an untrusted generation claims nothing attaches to the untrusted bridge")
	}

	if !strings.Contains(trusted, nothingAttaches) {
		t.Error("a trusted generation does not say that nothing attaches to the untrusted " +
			"bridge, so an operator is not told the bridge is optional for this file")
	}

	if strings.Contains(trusted, refusesEveryJob) {
		t.Error("a trusted generation still claims removing the untrusted bridge refuses " +
			"every job it schedules; no tier in it uses that bridge")
	}
}

// A REPOSITORY TARGET CANNOT CARRY A TRUSTED POOL, so the policy flags are
// refused by name rather than rendered into a config that fails to load; and
// docker, which admits only trusted work, cannot serve one at all, refused
// before the generic "docker needs a policy" answer can send the operator to
// create a runner group GitHub has nowhere to put.
func TestGenerateRefusesARepositoryWithAPoolOrOnDocker(t *testing.T) {
	t.Parallel()

	t.Run("policy flags name a pool a repository cannot have", func(t *testing.T) {
		t.Parallel()

		p := firecrackerParams()
		p.Org = ""
		p.Repository = "acme/widgets"
		p.RunnerGroup = trialGroup
		p.Workflows = []string{trialWorkflow}

		_, _, err := Generate(p)
		if !errors.Is(err, errRepositoryHasNoPool) {
			t.Fatalf("Generate on --repository with a pool: %v, want %v", err, errRepositoryHasNoPool)
		}
	})

	t.Run("docker cannot serve a repository", func(t *testing.T) {
		t.Parallel()

		p := dockerParams()
		p.Org = ""
		p.Repository = "acme/widgets"
		p.RunnerGroup = ""
		p.Workflows = nil

		_, _, err := Generate(p)
		if !errors.Is(err, errDockerCannotServeRepository) {
			t.Fatalf("Generate on --repository --provider docker: %v, want %v", err,
				errDockerCannotServeRepository)
		}

		if strings.Contains(err.Error(), "Create a runner group") {
			t.Errorf("the refusal sends the operator to create a runner group a repository cannot use: %v", err)
		}
	})

	t.Run("firecracker serves a repository untrusted", func(t *testing.T) {
		t.Parallel()

		p := firecrackerParams()
		p.Org = ""
		p.Repository = "acme/widgets"
		p.RunnerGroup = ""
		p.Workflows = nil

		body, _, err := Generate(p)
		if err != nil {
			t.Fatalf("Generate on --repository --provider firecracker: %v", err)
		}

		if !strings.Contains(body, "repository: acme/widgets") {
			t.Errorf("the generated config does not name the repository:\n%s", body)
		}

		if strings.Contains(body, "trust: trusted") {
			t.Errorf("a repository target's tier was generated trusted:\n%s", body)
		}
	})
}
