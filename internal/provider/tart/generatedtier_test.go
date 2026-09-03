package tart

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/provider"
)

// EVERY TIER `billet init --provider tart` WRITES ACTUALLY LAUNCHES.
//
// THIS IS THE TEST THE DOCKER TRIAL FORCED, ONE BACKEND OVER. The docker trial
// shipped generating a config that LOADED, passed `billet check`, and was then
// refused by the provider at the launch boundary — a config-load-only test could
// never see it, because the gap is between "this file is valid" and "a job
// starts on it". internal/e2e proves the docker half against a real container
// runtime; a Mac and an 87GB guest image are not available to CI, so this is the
// strongest thing that can run everywhere.
//
// IT LIVES IN package tart DELIBERATELY, importing the generator rather than the
// other way round. The only thing that can say a generated tier is launchable is
// the backend that would REFUSE it: checkSpec turns away an empty command, a
// sized /dev/shm, cache volumes, an image tart would read as a flag and a name
// no lease could be recovered from — and re-asserting a READING of that list
// from another package is the second-opinion-that-drifts mistake this repo keeps
// finding. depguard's provider rule forbids reaching up into the scheduler, the
// node runtime and cmd/; initconfig is none of those and imports only config.
//
// The Spec is assembled the way internal/node/runner.go assembles one, because a
// hand-tuned Spec would prove the fixture rather than the generation.
func TestEveryGeneratedTartTierLaunchesOnThisBackend(t *testing.T) {
	for _, tc := range []struct {
		name  string
		trust func(*initconfig.Params)
	}{
		{name: "untrusted", trust: func(*initconfig.Params) {}},
		{name: "trusted", trust: func(p *initconfig.Params) {
			p.RunnerGroup = "Billet trusted"
			p.Workflows = []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := generatedTartConfig(t, tc.trust)

			s := newStub(t)
			placeRunnerWherePublishedImagesPutIt(t, s)
			markMeasuredImagesPulled(t, s)

			p := newProvider(t, s)

			// THE NODE BLOCK THE GENERATION WROTE, not one the test chose. This is
			// what cmd/billet does with cfg.Node.Tart, and it is the difference
			// between the two cases: an untrusted tier launches only because the
			// generation named an isolation mechanism, and a generator that stopped
			// writing one would fail here rather than on somebody's Mac.
			var tartCfg config.TartConfig
			if cfg.Node.Tart != nil {
				tartCfg = *cfg.Node.Tart
			}

			WithConfig(tartCfg)(p)

			for i := range cfg.Tiers {
				tier := &cfg.Tiers[i]

				t.Run(tier.Label, func(t *testing.T) {
					spec := specForTier(tier, fmt.Sprintf("billet-lease%d", i+1))

					inst, err := p.Launch(t.Context(), spec)
					if err != nil {
						t.Fatalf("a tier `billet init` generated could not launch: %v", err)
					}

					if !inst.Running {
						t.Error("Launch returned an instance that does not report running")
					}

					// THE IMAGE THE GENERATION NAMED IS THE ONE TART WAS ASKED
					// ABOUT. A moving tag is resolved to its pulled DIGEST before
					// the clone, so the clone's own argument is a digest and the
					// reference appears in the resolution — asserting the reference
					// against the clone line would be asserting against a spelling
					// billet deliberately does not use.
					if argv := s.argv(t); !strings.Contains(argv, "fqn "+spec.Image) {
						t.Errorf("the generated image %q never reached tart:\n%s", spec.Image, argv)
					}

					if _, err := p.Destroy(t.Context(), spec.Name); err != nil {
						t.Errorf("Destroy: %v", err)
					}
				})
			}
		})
	}
}

// generatedTartConfig renders a tart config through the real generator and loads
// it, so what follows is the tiers billet DECIDED rather than a reconstruction.
func generatedTartConfig(t *testing.T, trust func(*initconfig.Params)) *config.Config {
	t.Helper()

	p := initconfig.Params{
		Org:      "acme",
		Provider: config.ProviderTart,
		// The platform this generation is for, and the only profile that can name
		// one: a user-session generation's paths come from the RUNNING process, so
		// on Linux CI it is for Linux whatever GOOS says and Generate refuses it.
		// The service shape's paths are constants per platform, so it can be
		// written from anywhere — which is what lets this assert a launch here
		// rather than that refusal.
		GOOS:    "darwin",
		Profile: initconfig.ProfileLocalService,
		VCPU:    12,
		Memory:  32 * config.GiB,
		Tart: &initconfig.TartParams{
			GuestOS:  []config.GuestOS{config.GuestMacOS, config.GuestLinux},
			NodeName: "mac-mini-1",
		},
	}
	trust(&p)

	body, _, err := initconfig.Generate(p)
	if err != nil {
		t.Fatalf("initconfig.Generate: %v", err)
	}

	// The App identity `github-app create --config` fills in afterwards; without
	// it the config does not load at all, and what is under test is the tiers.
	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)

	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("the generated config does not load: %v\n\n%s", err, body)
	}

	if len(cfg.Tiers) < 2 {
		t.Fatalf("both guest kinds were asked for and %d tier(s) came back", len(cfg.Tiers))
	}

	return cfg
}

// specForTier builds the launch request a node would build for this tier.
//
// IT REACHES THE CATALOGUE DIRECTLY, WHICH IS ONE HOP SHORT OF PRODUCTION, and
// the shortfall is deliberate rather than overlooked. A control plane sends a
// node a nodeapi.TierSpec — that is where ImageFor and RunnerCommandFor are
// actually called — and depguard forbids a provider importing anything under
// internal/node, the wire vocabulary included. A provider genuinely has no
// business knowing it: production goes plane, node, provider, and this backend
// never sees a TierSpec.
//
// So the hop is proved where it can be: internal/integration's
// TestAGeneratedTartTierCrossesTheWireIntact takes a generated tier through the
// real nodeapi.TierSpecOf and asserts the image and command that arrive. What is
// proved HERE is the other half — that the backend accepts what a node would then
// hand it — and size comes from the LEASE in production, equal to the tier's
// request at reserve time.
func specForTier(tier *config.Tier, name string) provider.Spec {
	trust := provider.TrustUntrusted
	if tier.Trust == config.WorkloadTrusted {
		trust = provider.TrustTrusted
	}

	return provider.Spec{
		Name:      name,
		VCPU:      tier.VCPU,
		Memory:    tier.Memory,
		Image:     tier.ImageFor(config.ProviderTart),
		Disk:      tier.Disk,
		SHM:       tier.SHM,
		Trust:     trust,
		Command:   tier.RunnerCommandFor(config.ProviderTart),
		JITConfig: "eyJ0b2tlbiI6IlNVUEVSU0VDUkVUUkVHSVNUUkFUSU9OIn0=",
	}
}

// placeRunnerWherePublishedImagesPutIt models the one fact about the guest that
// decides whether a generated tier can start anything.
//
// The stub's own fake guest keeps its runner at $HOME/run.sh, which is where
// GitHub's GENERIC default points. The images this generator names do not: the
// cirruslabs guests ship the runner in ~/actions-runner, which is why
// config.RunnerCommandFor(tart) defaults there.
//
// SO THE GENERIC ONE IS REMOVED, and that is the whole value of this helper. A
// fixture carrying both paths accepts either command, which makes the assertion
// vacuous for the field most likely to be wrong — the guest boots, the delivery
// reports success, and the first job of the day dies on a missing file, on a
// machine CI does not have. With only the real path present, a generated tier
// pointing at the generic default fails here.
func placeRunnerWherePublishedImagesPutIt(t *testing.T, s *stub) {
	t.Helper()

	generic := filepath.Join(s.guestHome, "run.sh")

	body, err := os.ReadFile(generic)
	if err != nil {
		t.Fatalf("read the stub's guest runner: %v", err)
	}

	dir := filepath.Join(s.guestHome, "actions-runner")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make the guest runner directory: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "run.sh"), body, 0o755); err != nil {
		t.Fatalf("place the guest runner: %v", err)
	}

	if err := os.Remove(generic); err != nil {
		t.Fatalf("remove the generic runner path these images do not have: %v", err)
	}
}

// markMeasuredImagesPulled seeds the fake store with the two LITERAL references
// billet has run a real job in.
//
// LITERALS, NOT initconfig's CONSTANTS, and that is the whole point of this
// helper. Seeding whatever the generator emitted would make the fixture follow
// the generator anywhere: retarget a default to a typo, or to an image that does
// not exist, and the test would mark that pulled and pass. A launch REFUSES an
// image that is not present, so with the store holding only what was measured, a
// generation that names anything else fails here the way it would fail on a Mac.
//
// initconfig has its own test pinning the constants to these same strings; this
// one proves the LAUNCH works with them, which is the half that package cannot
// see.
func markMeasuredImagesPulled(t *testing.T, s *stub) {
	t.Helper()

	measured := []string{
		"ghcr.io/cirruslabs/macos-tahoe-xcode:latest",
		"ghcr.io/cirruslabs/ubuntu-runner-arm64:latest",
	}

	f, err := os.OpenFile(s.pulled, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open the pulled set: %v", err)
	}

	defer func() {
		if err := f.Close(); err != nil {
			t.Errorf("close the pulled set: %v", err)
		}
	}()

	for _, image := range measured {
		if _, err := fmt.Fprintln(f, image); err != nil {
			t.Fatalf("mark %s pulled: %v", image, err)
		}
	}
}
