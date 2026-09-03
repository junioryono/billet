package integration_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/initconfig"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A GENERATED TART TIER CROSSES THE WIRE INTACT.
//
// THE HOP NEITHER END CAN PROVE. `billet init` decides a tier's image and leaves
// its command to the backend's default; a node launches from what the control
// plane SENT it, which is a nodeapi.TierSpec — and TierSpecOf is where ImageFor
// and RunnerCommandFor are actually called. internal/initconfig cannot see that
// mapping, and internal/provider/tart may not import it (depguard forbids a
// provider reaching anything under internal/node, the wire vocabulary included,
// and rightly: production goes plane, node, provider, and the backend never sees
// a TierSpec).
//
// So both ends can be green while the thing between them drops a field. The
// command is the one that costs a day: the guest boots, the delivery reports
// success, and the first job of the day dies on a missing file.
func TestAGeneratedTartTierCrossesTheWireIntact(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		trust func(*initconfig.Params)
	}{
		{name: "untrusted", trust: func(*initconfig.Params) {}},
		// THE TRUSTED CASE IS NOT A DUPLICATE: runner_group and the workflow
		// allowlist only exist on a trusted tier, so an untrusted-only fixture
		// cannot notice either being dropped from the mapping — and a pool that
		// arrives without its allowlist is one GitHub hands work to from
		// repositories the operator never named.
		{name: "trusted", trust: func(p *initconfig.Params) {
			p.RunnerGroup = wantRunnerGroup
			p.Workflows = []string{wantWorkflow}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			crossesTheWireIntact(t, tc.name == "trusted", tc.trust)
		})
	}
}

// The policy the trusted case ASKS FOR, so the assertions can compare against
// what was requested rather than against what came back.
const (
	wantRunnerGroup = "Billet trusted"
	wantWorkflow    = "acme/repo/.github/workflows/ci.yml@refs/heads/main"
)

func crossesTheWireIntact(t *testing.T, trusted bool, trust func(*initconfig.Params)) {
	t.Helper()

	p := initconfig.Params{
		Org:      "acme",
		Provider: config.ProviderTart,
		// The platform this generation is FOR, and the only profile a
		// cross-platform one can honour: a tart config renders a Mac's shape and
		// Generate refuses any other, while the user-session paths come from the
		// RUNNING process rather than the target.
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
	// it the config does not load, and the tiers are what is under test.
	filled := strings.Replace(body, "app_id: 0", "app_id: 1", 1)
	filled = strings.Replace(filled, "installation_id: 0", "installation_id: 1", 1)

	cfg, err := config.Parse("generated", []byte(filled))
	if err != nil {
		t.Fatalf("the generated config does not load: %v\n\n%s", err, body)
	}

	if len(cfg.Tiers) < 2 {
		t.Fatalf("both guest kinds were asked for and %d tier(s) came back", len(cfg.Tiers))
	}

	for i := range cfg.Tiers {
		tier := cfg.Tiers[i]

		t.Run(tier.Label, func(t *testing.T) {
			t.Parallel()

			// THROUGH JSON, because that is what "crosses the wire" means: the
			// plane encodes this shape and the node decodes it, so a field
			// TierSpecOf fills and the struct tags omit never arrives.
			wire := roundTrip(t, nodeapi.TierSpecOf(tier, config.ProviderTart))

			// THE IMAGE THE GENERATION CHOSE, not merely a non-empty one: the two
			// guest kinds name different artifacts and a mapping that sent either
			// tier the other's would launch the wrong guest.
			if want := tier.ImageFor(config.ProviderTart); wire.Image != want {
				t.Errorf("the wire carries image %q, and the catalogue says %q", wire.Image, want)
			}

			// AND THE COMMAND, which the generation deliberately does NOT write:
			// the published images ship the runner in ~/actions-runner, so the
			// tier leans on the backend's default and this is the only place that
			// default is resolved before a launch.
			if len(wire.Command) != 1 || wire.Command[0] != "./actions-runner/run.sh" {
				t.Errorf("the wire carries command %q, and the published images keep the "+
					"runner in ~/actions-runner", wire.Command)
			}

			if wire.Trust != tier.Trust.Effective() {
				t.Errorf("the wire carries trust %q, and the tier is %q",
					wire.Trust, tier.Trust.Effective())
			}

			// THE POLICY A TRUSTED POOL IS DEFINED BY, compared against what was
			// REQUESTED rather than against the tier. Comparing the two sides of the
			// pipeline agrees whenever both are damaged: delete the generator's
			// trusted rendering and the tier arrives untrusted with an empty policy,
			// which the wire then faithfully carries. A pool that reaches GitHub
			// without its allowlist takes work from repositories nobody named.
			if trusted {
				if wire.Trust != config.WorkloadTrusted {
					t.Errorf("a trusted generation put %q on the wire", wire.Trust)
				}
				if wire.RunnerGroup != wantRunnerGroup {
					t.Errorf("the wire carries runner_group %q, and %q was asked for",
						wire.RunnerGroup, wantRunnerGroup)
				}
				if !slices.Equal(wire.Workflows, []string{wantWorkflow}) {
					t.Errorf("the wire carries workflows %q, and %q was asked for",
						wire.Workflows, wantWorkflow)
				}
			} else if wire.RunnerGroup != "" || len(wire.Workflows) > 0 {
				t.Errorf("an untrusted generation carried a pool policy: %q and %q",
					wire.RunnerGroup, wire.Workflows)
			}

			// The two fields the tart backend REFUSES outright. A generation that
			// grew either would be a config whose first job fails at the launch
			// boundary, and this is where they become the node's problem.
			if wire.SHM != 0 {
				t.Errorf("the wire sizes /dev/shm (%s), which tart cannot configure", wire.SHM)
			}
			if wire.Intercept {
				t.Error("the wire enables the results proxy, which this backend cannot wire in")
			}
		})
	}
}

// roundTrip encodes and decodes a TierSpec the way the node wire does.
func roundTrip(t *testing.T, spec *nodeapi.TierSpec) *nodeapi.TierSpec {
	t.Helper()

	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatalf("encode the tier spec: %v", err)
	}

	var decoded nodeapi.TierSpec
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode the tier spec: %v", err)
	}

	return &decoded
}
