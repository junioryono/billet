package config

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// threeTargetConfig is validConfig serving three targets, the github block's
// organization, a repository and a second organization, with declaration added
// to its tiers.
func threeTargetConfig(t *testing.T, declaration string) string {
	t.Helper()

	body := validConfig
	for _, label := range []string{"billet-4vcpu-ubuntu-2404", "billet-8vcpu-ubuntu-2404"} {
		line := "  - label: " + label + "\n"
		if !strings.Contains(body, line) {
			t.Fatalf("the fixture no longer has tier %s, so this case patches nothing", label)
		}

		body = strings.Replace(body, line, line+"    target: default\n", 1)
	}

	return body + declaration + `
targets:
  - name: personal
    repository: someone/widgets
    app_id: 777
    installation_id: 888
    private_key_path: /etc/billet/app-personal.pem
  - name: beta
    org: beta-co
    app_id: 999
    installation_id: 1000
    private_key_path: /etc/billet/app-beta.pem
`
}

const everyTarget = `  - label: ${target}-1vcpu
    runs_on: billet-1vcpu-ubuntu-2404
    targets: all
    provider: firecracker
    vcpu: 1
    memory: 4GiB
    image: ubuntu-2404-x64
    command: [/usr/local/bin/run]
`

// A TIER WRITTEN ONCE FOR EVERY TARGET BECOMES ONE PER TARGET, each answering to
// the same runs-on name, in the order the targets are declared.
func TestATierForEveryTargetBecomesOnePerTarget(t *testing.T) {
	cfg, err := Load(writeConfig(t, threeTargetConfig(t, everyTarget)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var made []Tier

	for _, tier := range cfg.Tiers {
		if tier.Declaration != "" {
			made = append(made, tier)
		}
	}

	want := []struct{ label, target string }{
		{"default-1vcpu", DefaultTargetName}, {"personal-1vcpu", "personal"}, {"beta-1vcpu", "beta"},
	}
	if len(made) != len(want) {
		t.Fatalf("the declaration made %d tiers, want %d: %+v", len(made), len(want), made)
	}

	for i, w := range want {
		got := made[i]
		if got.Label != w.label || got.Target != w.target {
			t.Errorf("tier %d is %q on %q, want %q on %q", i, got.Label, got.Target, w.label, w.target)
		}

		if got.ScaleSetName() != "billet-1vcpu-ubuntu-2404" {
			t.Errorf("tier %q answers to %q, want the declaration's runs_on", got.Label, got.ScaleSetName())
		}

		if got.Declaration != "${target}-1vcpu" || got.WritesTargets() {
			t.Errorf("tier %q has declaration %q and targets %+v, want the label as written and none",
				got.Label, got.Declaration, got.Targets)
		}

		if got.VCPU != 1 || got.Memory != 4*GiB || got.Trust != WorkloadUntrusted {
			t.Errorf("tier %q has %d vCPU, %s, trust %q", got.Label, got.VCPU, got.Memory, got.Trust)
		}
	}

	// THE REPOSITORY TARGET'S CACHE SCOPE IS DERIVED, from its own repository,
	// and the organizations' are not.
	if s := made[1].CacheScope; s == nil || s.Owner != "someone" || s.Repository != "widgets" {
		t.Errorf("the repository target's tier has cache scope %+v, want someone/widgets", s)
	}

	if made[0].CacheScope != nil || made[2].CacheScope != nil {
		t.Errorf("an organization target's tier was given a cache scope: %+v, %+v",
			made[0].CacheScope, made[2].CacheScope)
	}

	// NO TWO SHARE A SLICE: one target's tier is changed, and the others are not.
	made[0].Command[0] = "changed"
	if made[1].Command[0] != "/usr/local/bin/run" || made[2].Command[0] != "/usr/local/bin/run" {
		t.Errorf("the expanded tiers share their command: %v, %v", made[1].Command, made[2].Command)
	}

	// The hand-written tiers are untouched.
	for _, label := range []string{"billet-4vcpu-ubuntu-2404", "billet-8vcpu-ubuntu-2404"} {
		if tier, ok := cfg.TierByLabel(label); !ok || tier.Declaration != "" {
			t.Errorf("hand-written tier %q is %+v, %v", label, tier, ok)
		}
	}
}

// A DECLARATION THAT NAMES ITS TARGETS SERVES ONLY THOSE, in the order it names
// them.
func TestATierForNamedTargetsServesOnlyThose(t *testing.T) {
	named := strings.Replace(everyTarget, "    targets: all\n", "    targets: [beta, personal]\n", 1)

	cfg, err := Load(writeConfig(t, threeTargetConfig(t, named)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var labels []string

	for _, tier := range cfg.Tiers {
		if tier.Declaration != "" {
			labels = append(labels, tier.Label)
		}
	}

	if want := []string{"beta-1vcpu", "personal-1vcpu"}; !slices.Equal(labels, want) {
		t.Errorf("the declaration made %v, want %v", labels, want)
	}
}

// `sizes` AND `targets` TOGETHER make every size on every target.
func TestALadderForEveryTargetMakesEverySizeOnEach(t *testing.T) {
	ladder := `  - label: ${target}-web
    runs_on: billet-web
    targets: [personal]
    provider: firecracker
    sizes: [1, 2]
    image: ubuntu-2404-x64
`

	cfg, err := Load(writeConfig(t, threeTargetConfig(t, ladder)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var got []string

	for _, tier := range cfg.Tiers {
		if tier.Declaration != "" {
			got = append(got, tier.Label+"="+tier.ScaleSetName())
		}
	}

	if want := []string{"personal-web-1vcpu=billet-web-1vcpu", "personal-web-2vcpu=billet-web-2vcpu"}; !slices.Equal(got, want) {
		t.Errorf("the ladder made %v, want %v", got, want)
	}
}

// A NODE'S FILE DECLARES NO TARGETS, and keeps one tier per target a
// declaration names, or one for the default target under `all`: the node never
// launches from the catalogue, but its image reaper protects what the tiers pin
// and its preflight sizes their disks, so a declaration dropped there would
// leave a pinned generation unprotected.
func TestANodesFileKeepsADeclarationsShapes(t *testing.T) {
	const server = "server:\n  listen: 127.0.0.1:7717\n  state_dir: /var/lib/billet/server\n" +
		"  max_vcpu: 120\n  max_memory: 480GiB\n"
	const github = "github:\n  org: acme\n  app_id: 12345\n  installation_id: 67890\n" +
		"  private_key_path: /etc/billet/app.pem\n"

	named := strings.Replace(strings.Replace(everyTarget, "${target}-1vcpu", "${target}-named", 1),
		"    targets: all\n", "    targets: [default, elsewhere]\n", 1)
	nodeOnly := validConfig + everyTarget + named
	for _, block := range []string{server, github} {
		if !strings.Contains(nodeOnly, block) {
			t.Fatalf("the fixture no longer contains %q, so this case removes nothing", block)
		}

		nodeOnly = strings.Replace(nodeOnly, block, "", 1)
	}

	cfg, err := Load(writeConfig(t, nodeOnly))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if len(cfg.GitHubTargets()) != 0 {
		t.Fatalf("the node's file still declares targets: %+v", cfg.GitHubTargets())
	}

	var got []string

	for _, tier := range cfg.Tiers {
		if strings.Contains(tier.Label, TargetPlaceholder) {
			t.Errorf("a node's file kept an unexpanded label: %+v", tier)
		}

		if tier.Declaration != "" {
			got = append(got, tier.Label+"="+tier.Image)
		}
	}

	want := []string{"default-1vcpu=ubuntu-2404-x64", "default-named=ubuntu-2404-x64",
		"elsewhere-named=ubuntu-2404-x64"}
	if !slices.Equal(got, want) {
		t.Errorf("the node's file kept %v from its declarations, want %v", got, want)
	}

	if len(cfg.Tiers) != 2+len(want) {
		t.Errorf("the node's file has %d tiers, want its two hand-written ones and %d declared",
			len(cfg.Tiers), len(want))
	}
}

// TWO LADDERS FOR DISJOINT TARGETS share a label only before targets expand it,
// and load; each tier says the declaration as written, not the ladder's suffix.
func TestLaddersForDisjointTargetsLoad(t *testing.T) {
	ladder := func(target string) string {
		return `  - label: ${target}-web
    runs_on: billet-web
    targets: [` + target + `]
    provider: firecracker
    sizes: [2]
    image: ubuntu-2404-x64
`
	}

	cfg, err := Load(writeConfig(t, threeTargetConfig(t, ladder("personal")+ladder("beta"))))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var got []string

	for _, tier := range cfg.Tiers {
		if tier.Declaration != "" {
			got = append(got, tier.Label+" from "+tier.Declaration)
		}
	}

	want := []string{"personal-web-2vcpu from ${target}-web", "beta-web-2vcpu from ${target}-web"}
	if !slices.Equal(got, want) {
		t.Errorf("the ladders made %v, want %v", got, want)
	}
}

// EVERY DECLARATION THAT CANNOT MEAN ONE THING IS REFUSED BY NAME.
func TestADeclarationThatCannotMeanOneThingIsRefused(t *testing.T) {
	cases := []struct {
		name, from, to, want string
	}{
		{"target and targets", "    targets: all\n", "    targets: all\n    target: beta\n",
			"target and targets are both written"},
		{"no placeholder", "  - label: ${target}-1vcpu\n", "  - label: one-1vcpu\n",
			"write ${target} in the label"},
		{"no runs_on", "    runs_on: billet-1vcpu-ubuntu-2404\n", "", "runs_on is required"},
		{"a cache scope", "    targets: all\n",
			"    targets: all\n    cache_scope:\n      owner: acme\n      repository: app\n",
			"cache_scope names one owner"},
		{"an undeclared target", "    targets: all\n", "    targets: [beta, gamma]\n",
			`targets names "gamma", which is not one this config declares`},
		{"a target twice", "    targets: all\n", "    targets: [beta, beta]\n", `targets lists "beta" twice`},
		{"an empty list", "    targets: all\n", "    targets: []\n", "targets is an empty list"},
		{"a scalar other than all", "    targets: all\n", "    targets: beta\n",
			"targets is `all` or a list of target names"},
		{"a mapping", "    targets: all\n", "    targets: {beta: true}\n",
			"targets is `all` or a list of target names"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(everyTarget, tc.from) {
				t.Fatalf("the fixture does not contain %q, so this case changes nothing", tc.from)
			}

			body := threeTargetConfig(t, strings.Replace(everyTarget, tc.from, tc.to, 1))

			_, err := Load(writeConfig(t, body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load = %v, want an error saying %q", err, tc.want)
			}
		})
	}
}

// THE PLACEHOLDER MEANS NOTHING OUTSIDE A DECLARATION, and a tier carrying it
// without `targets` is refused rather than given a label with a dollar in it.
func TestThePlaceholderWithoutTargetsIsRefused(t *testing.T) {
	for _, field := range []string{"label", "runs_on"} {
		t.Run(field, func(t *testing.T) {
			body := validConfig
			if field == "label" {
				body = strings.Replace(body, "  - label: billet-4vcpu-ubuntu-2404\n",
					"  - label: ${target}-4vcpu\n", 1)
			} else {
				body = strings.Replace(body, "  - label: billet-4vcpu-ubuntu-2404\n",
					"  - label: billet-4vcpu-ubuntu-2404\n    runs_on: ${target}-4vcpu\n", 1)
			}

			if body == validConfig {
				t.Fatal("the fixture's first tier has changed, so this case patches nothing")
			}

			_, err := Load(writeConfig(t, body))
			if err == nil || !strings.Contains(err.Error(), "only in a tier that writes `targets`") {
				t.Errorf("Load = %v, want the placeholder refused outside a declaration", err)
			}
		})
	}
}

// A HAND-WRITTEN TIER WITH AN EXPANDED TIER'S LABEL is refused naming the
// declaration, since the operator wrote that label nowhere.
func TestAHandWrittenTierThatCollidesWithADeclarationSaysSo(t *testing.T) {
	handWritten := `  - label: beta-1vcpu
    target: beta
    provider: firecracker
    vcpu: 1
    memory: 4GiB
    image: ubuntu-2404-x64
`

	_, err := Load(writeConfig(t, threeTargetConfig(t, handWritten+everyTarget)))
	if err == nil || !strings.Contains(err.Error(), `the declaration "${target}-1vcpu" makes it for target "beta"`) {
		t.Errorf("Load = %v, want the collision blamed on the declaration", err)
	}
}

// A CONFIG WITHOUT `targets` LOADS TO EXACTLY THE TIERS IT DID: no tier carries a
// declaration, and the shipped example and template keep their labels.
func TestAConfigWithoutTargetsIsUnchanged(t *testing.T) {
	for _, path := range []string{"../../billet.example.yaml", "../../deploy/billet.yaml"} {
		t.Run(path, func(t *testing.T) {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v", path, err)
			}

			cfg, err := ParseUnvalidated(path, body)
			if err != nil {
				t.Fatalf("ParseUnvalidated: %v", err)
			}

			if len(cfg.Tiers) == 0 {
				t.Fatal("the file has no tiers, so this checks nothing")
			}

			for _, tier := range cfg.Tiers {
				if tier.Declaration != "" || tier.WritesTargets() ||
					strings.Contains(tier.Label, TargetPlaceholder) {
					t.Errorf("tier %+v came from a declaration in a file that writes none", tier)
				}
			}
		})
	}
}

// A DECLARATION WRITES BACK THE WAY IT READS, so a tool that renders tiers does
// not turn `all` into a list or a list into `all`.
func TestTierTargetsWritesBackAsWritten(t *testing.T) {
	for _, s := range []TierTargets{{All: true, written: true}, {Names: []string{"a", "b"}, written: true}} {
		v, err := s.MarshalYAML()
		if err != nil {
			t.Fatalf("MarshalYAML: %v", err)
		}

		if s.All && v != "all" {
			t.Errorf("all wrote %v", v)
		}

		if names, ok := v.([]string); !s.All && (!ok || !slices.Equal(names, s.Names)) {
			t.Errorf("%v wrote %v", s.Names, v)
		}
	}
}
