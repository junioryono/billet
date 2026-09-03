package codebuild

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/provider"
)

// specFor assembles the launch the node runtime would assemble for one tier.
//
// THROUGH config RATHER THAN THROUGH nodeapi.TierSpecOf, and that is depguard's
// rule rather than a preference: a provider is a leaf and must not import the
// node runtime that drives it. What TierSpecOf contributes for these fields is
// exactly `t.Image` and `t.RunnerCommandFor(provider)`, both of which live in
// config — so this is the same derivation rather than a second opinion about it,
// and in particular the COMMAND still comes from the one function that answers
// `./run.sh` for this backend.
//
// The size comes from the tier because that is what the lease is escrowed
// against; TierSpec carries neither.
func specFor(t config.Tier, name string) provider.Spec {
	return provider.Spec{
		Name:      name,
		Image:     t.Image,
		VCPU:      t.VCPU,
		Memory:    t.Memory,
		Command:   t.RunnerCommandFor(config.ProviderCodeBuild),
		Trust:     provider.TrustTrusted,
		JITConfig: "generated-tier-proof",
	}
}

// EVERY TIER `billet init --provider codebuild` WRITES ACTUALLY LAUNCHES.
//
// THIS IS THE TEST THE DOCKER TRIAL FORCED, ONE BACKEND OVER. The docker trial
// shipped generating a config that LOADED, passed `billet check`, and was then
// refused by the provider at the launch boundary — a config-load-only test could
// never see it, because the gap is between "this file is valid" and "a job
// starts on it". internal/initconfig proves the file parses; nothing there can
// prove the backend accepts what it wrote.
//
// IT LIVES IN package codebuild DELIBERATELY, importing the generator rather
// than the other way round. The only thing that can say a generated tier is
// launchable is the backend that would REFUSE it — checkSpec turns away an empty
// command, and the buildspec builder turns away an argv it cannot quote — and
// re-asserting a READING of those rules from another package is the
// second-opinion-that-drifts mistake this repository keeps finding. depguard's
// provider rule forbids reaching up into the scheduler, the node runtime and
// cmd/; initconfig is none of those and imports only config.
//
// THE SPEC IS ASSEMBLED THE WAY internal/node ASSEMBLES ONE, through
// nodeapi.TierSpecOf, because a hand-tuned Spec would prove the fixture rather
// than the generation. That is also what supplies the tier's COMMAND: the
// generator deliberately writes none, because config.Tier.RunnerCommandFor
// already answers `./run.sh` for this backend — so if that default ever stopped
// being written, this test is what fails rather than somebody's first job.
func TestEveryGeneratedCodeBuildTierLaunchesOnThisBackend(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*initconfig.Params)
		wantOS  config.GuestOS
		wantImg string
	}{{
		name:    "linux on demand",
		mutate:  func(*initconfig.Params) {},
		wantOS:  config.GuestLinux,
		wantImg: "aws/codebuild/amazonlinux-x86_64-standard:5.0",
	}, {
		name: "arm64 linux on demand",
		mutate: func(p *initconfig.Params) {
			p.CodeBuild.Environment = config.CodeBuildARMContainer
		},
		wantOS:  config.GuestLinux,
		wantImg: "aws/codebuild/amazonlinux-aarch64-standard:3.0",
	}, {
		// RESERVED CAPACITY AND macOS, which is the case with the most that can
		// go wrong in the generation: it is the only one that pins a node, writes
		// a host policy and caps concurrency, and the only one whose guest OS is
		// not linux.
		name: "macos on a reserved fleet",
		mutate: func(p *initconfig.Params) {
			p.CodeBuild.Environment = config.CodeBuildMacARM
			p.CodeBuild.FleetARN = "arn:aws:codebuild:us-west-2:123456789012:fleet/macs"
			p.CodeBuild.FleetCapacity = 1
			p.CodeBuild.NodeName = "cb-macs-1"
			p.CodeBuild.ComputeTypes = []config.RemoteShape{{
				Type: "BUILD_GENERAL1_MEDIUM", VCPU: 8, Memory: 24 * config.GiB,
				PriceUSDPerHour: 1200000,
			}}
		},
		wantOS:  config.GuestMacOS,
		wantImg: "aws/codebuild/macos-arm-base:14",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := generatedCodeBuildConfig(t, tc.mutate)

			f := newFakeAWS(t)

			// THE NODE BLOCK THE GENERATION WROTE, not one the test chose. This is
			// what cmd/billet does with cfg.Node.CodeBuild, and it is what makes the
			// macOS case meaningful: that launch only reaches AWS because the
			// generation named a fleet, and a generator that stopped writing one
			// would fail here rather than on somebody's account.
			p := newTestProvider(t, f, func(dst *config.CodeBuildConfig) {
				// EVERYTHING BUT THE ENDPOINT, which the harness has already
				// pointed at the fake. A generation names no endpoint — an
				// ordinary install derives one from the region — so copying the
				// block wholesale would send these launches at the real API.
				endpoint := dst.Endpoint
				*dst = *cfg.Node.CodeBuild
				dst.Endpoint = endpoint
			})

			if len(cfg.Tiers) == 0 {
				t.Fatal("the generation wrote no tiers, so this proves nothing")
			}

			for i := range cfg.Tiers {
				tier := &cfg.Tiers[i]

				if tier.GuestOS != tc.wantOS {
					t.Errorf("tier %s has guest_os %s, want %s", tier.Label, tier.GuestOS, tc.wantOS)
				}

				if tier.Image != tc.wantImg {
					t.Errorf("tier %s names image %q, want %q", tier.Label, tier.Image, tc.wantImg)
				}

				inst, err := p.Launch(t.Context(), specFor(*tier,
					provider.InstanceName("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa0"+itoa(i))))
				if err != nil {
					t.Fatalf("tier %s does not launch on this backend: %v", tier.Label, err)
				}

				if inst.ID == "" {
					t.Fatalf("tier %s launched with no build id, so nothing could tear it down",
						tier.Label)
				}
			}
		})
	}
}

// THE GENERATION'S COMMAND IS THE ONE THE BUILDSPEC LOOKS FOR, and neither side
// writes it down twice.
//
// The generator deliberately emits no `command:` — RunnerCommandFor already
// answers `./run.sh` for this backend, which is where billet's own generated
// buildspec leaves the runner, since a curated CodeBuild image ships none. A
// second copy in every generated file would stop being true the moment the
// buildspec moved it, and the symptom would be every job on a fresh deployment
// failing on a missing file.
func TestAGeneratedCodeBuildTierTakesTheBackendsOwnRunnerCommand(t *testing.T) {
	cfg := generatedCodeBuildConfig(t, func(*initconfig.Params) {})

	tier := cfg.Tiers[0]

	if len(tier.Command) != 0 {
		t.Errorf("the generation wrote a command (%v); the backend's own default is what "+
			"the buildspec resolves against, and a copy here would drift from it", tier.Command)
	}

	if got := tier.RunnerCommandFor(config.ProviderCodeBuild); len(got) != 1 ||
		got[0] != "./run.sh" {
		t.Fatalf("the dispatched command is %v, want [./run.sh] — the path billet's own "+
			"buildspec leaves the runner at", got)
	}
}

// generatedCodeBuildConfig runs the REAL generator and loads what it wrote.
func generatedCodeBuildConfig(t *testing.T, mutate func(*initconfig.Params)) *config.Config {
	t.Helper()

	p := initconfig.Params{
		Org:         "acme",
		Provider:    config.ProviderCodeBuild,
		RunnerGroup: "billet",
		Workflows:   []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"},
		VCPU:        32,
		Memory:      64 * config.GiB,
		GOOS:        "linux",
		CodeBuild: &initconfig.CodeBuildParams{
			Region:      "us-west-2",
			Project:     "billet-runners",
			Environment: config.CodeBuildLinuxContainer,
			ComputeTypes: []config.RemoteShape{
				{Type: "BUILD_GENERAL1_SMALL", VCPU: 2, Memory: 3 * config.GiB, PriceUSDPerHour: 300000},
			},
			JITParameterPath: "/billet/jit",
			AcceptCeiling:    true,
		},
	}

	mutate(&p)

	body, _, err := initconfig.Generate(p)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// The App ids are zero in a real generation — `github-app create` fills them
	// — and config validation requires them, exactly as Generate's own
	// self-validation supplies them.
	body = strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	body = strings.Replace(body, "installation_id: 0", "installation_id: 1", 1)

	cfg, err := config.Parse("generated", []byte(body))
	if err != nil {
		t.Fatalf("the generated config does not load: %v\n\n%s", err, body)
	}

	return cfg
}

// itoa renders a single digit for a lease name, which must stay 32 hex
// characters for provider.LeaseOf to recover it.
func itoa(i int) string { return string(rune('0' + i)) }
