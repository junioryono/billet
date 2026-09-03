package config

import (
	"strings"
	"testing"
)

// sizesConfig is a deployment whose one tier is a ladder. The %s is the tier
// body, so a case can break exactly one thing and the diff between passing and
// failing is visible.
const sizesConfig = `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 64
  max_memory: 256GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/key.pem
tiers:
  - label: web
    provider: docker
    trust: trusted
    runner_group: billet
    workflows: ["acme/repo/.github/workflows/ci.yml@refs/heads/main"]
    image: ghcr.io/actions/actions-runner:latest
%s`

func parseSizes(t *testing.T, body string) (*Config, error) {
	t.Helper()

	return Parse("billet.yaml", []byte(body))
}

// A LADDER WRITES THE TIERS AN OPERATOR WOULD HAVE WRITTEN BY HAND.
//
// This is the single largest source of hand-written YAML in a real deployment:
// each tier is about fifteen lines and several of them differ in two numbers and
// a label. The expansion has to produce exactly what those hand-written tiers
// were — separate tiers, separate labels, separate scale sets — because that is
// what they already are, and a shorthand meaning anything else would be a new
// scheduling concept wearing a convenience's clothes.
func TestSizesExpandsIntoOneTierPerSize(t *testing.T) {
	t.Parallel()

	cfg, err := parseSizes(t, strings.Replace(sizesConfig, "%s", "    sizes: [2, 4, 8]\n", 1))
	if err != nil {
		t.Fatalf("a sizes ladder did not load: %v", err)
	}

	if len(cfg.Tiers) != 3 {
		t.Fatalf("expanded to %d tiers, want 3: %+v", len(cfg.Tiers), cfg.Tiers)
	}

	want := []struct {
		label  string
		vcpu   int
		memory ByteSize
	}{
		{"web-2vcpu", 2, 8 * GiB},
		{"web-4vcpu", 4, 16 * GiB},
		{"web-8vcpu", 8, 32 * GiB},
	}

	for i, w := range want {
		got := cfg.Tiers[i]

		if got.Label != w.label || got.VCPU != w.vcpu || got.Memory != w.memory {
			t.Errorf("tier %d is %s (%d vCPU, %s), want %s (%d vCPU, %s)",
				i, got.Label, got.VCPU, got.Memory, w.label, w.vcpu, w.memory)
		}

		// EVERYTHING ELSE THE ENTRY SAID REACHES EVERY EXPANSION. A ladder that
		// dropped the trust policy would produce three pools GitHub hands work to
		// from repositories the operator never named — which is the whole reason
		// the tiers were written out longhand in the first place.
		if got.Trust != WorkloadTrusted || got.RunnerGroup != "billet" ||
			len(got.Workflows) != 1 || got.Image == "" || got.Provider != ProviderDocker {
			t.Errorf("tier %s lost part of what the entry said: %+v", got.Label, got)
		}
	}
}

// THE ORDER IS THE ORDER WRITTEN, and the expansion happens where the entry was.
//
// Position decides nothing today. What it decides is whether the file an operator
// reads back looks like the file they wrote, and a shorthand that reordered the
// catalogue would make that false for no reason.
func TestALadderExpandsWhereItWasWritten(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 64
  max_memory: 256GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/key.pem
tiers:
  - label: first
    provider: docker
    trust: trusted
    runner_group: billet
    workflows: ["acme/repo/.github/workflows/ci.yml@refs/heads/main"]
    image: ghcr.io/actions/actions-runner:latest
    vcpu: 2
    memory: 4GiB
  - label: web
    provider: docker
    trust: trusted
    runner_group: billet
    workflows: ["acme/repo/.github/workflows/ci.yml@refs/heads/main"]
    image: ghcr.io/actions/actions-runner:latest
    sizes: [2, 4]
  - label: last
    provider: docker
    trust: trusted
    runner_group: billet
    workflows: ["acme/repo/.github/workflows/ci.yml@refs/heads/main"]
    image: ghcr.io/actions/actions-runner:latest
    vcpu: 2
    memory: 4GiB
`

	cfg, err := parseSizes(t, body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	var labels []string
	for i := range cfg.Tiers {
		labels = append(labels, cfg.Tiers[i].Label)
	}

	want := []string{"first", "web-2vcpu", "web-4vcpu", "last"}
	if strings.Join(labels, ",") != strings.Join(want, ",") {
		t.Errorf("the catalogue is %v, want %v", labels, want)
	}
}

// THE PROPORTION IS THE OPERATOR'S WHEN THEY STATE ONE.
func TestALadderTakesTheDeclaredMemoryPerVCPU(t *testing.T) {
	t.Parallel()

	cfg, err := parseSizes(t, strings.Replace(sizesConfig, "%s",
		"    sizes: [2, 4]\n    memory_per_vcpu: 2GiB\n", 1))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if len(cfg.Tiers) != 2 {
		t.Fatalf("expanded to %d tiers, want 2", len(cfg.Tiers))
	}

	if cfg.Tiers[0].Memory != 4*GiB || cfg.Tiers[1].Memory != 8*GiB {
		t.Errorf("the ladder is %s and %s, want 4GiB and 8GiB",
			cfg.Tiers[0].Memory, cfg.Tiers[1].Memory)
	}
}

// EVERY SPELLING THAT MEANS TWO THINGS AT ONCE IS REFUSED.
//
// Two spellings of one value is a mistake internal/config has already made three
// times, and it is silent every time — so a ladder beside an explicit size, and a
// proportion with no ladder to shape, are both refusals rather than a merge or a
// discard.
func TestALadderRefusesEverySpellingThatMeansTwoThings(t *testing.T) {
	t.Parallel()

	for name, tierBody := range map[string]string{
		"a ladder beside an explicit vcpu":   "    sizes: [2, 4]\n    vcpu: 2\n",
		"a ladder beside an explicit memory": "    sizes: [2, 4]\n    memory: 8GiB\n",
		"a proportion with no ladder": "    vcpu: 2\n    memory: 4GiB\n" +
			"    memory_per_vcpu: 2GiB\n",
		"a size of zero":                  "    sizes: [0]\n",
		"a negative size":                 "    sizes: [-2]\n",
		"a repeated size":                 "    sizes: [2, 2]\n",
		"an empty ladder is not a ladder": "    sizes: []\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := parseSizes(t, strings.Replace(sizesConfig, "%s", tierBody, 1))
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
		})
	}
}

// A macOS LADDER IS REFUSED, AND THE REFUSAL NAMES THE SHORTHAND.
//
// A macOS tier with no explicit max_concurrent inherits its HOST's whole guest
// allowance, so three expansions each inherit the whole of it and validation
// refuses the file for exceeding it — with a diagnostic about Apple's licence
// rather than about the shorthand that caused it. Dividing the shares is a
// decision rather than boilerplate, which is the one thing this shorthand is not
// for.
func TestALadderCannotExpandAMacOSTier(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 64
  max_memory: 256GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/key.pem
tiers:
  - label: mac
    provider: tart
    guest_os: macos
    node: mac-1
    trust: trusted
    runner_group: billet
    workflows: ["acme/repo/.github/workflows/ci.yml@refs/heads/main"]
    image: ghcr.io/cirruslabs/macos-tahoe-xcode:latest
    sizes: [4, 8]
`

	_, err := parseSizes(t, body)
	if err == nil {
		t.Fatal("a macOS ladder was expanded, and each tier would have inherited the host's " +
			"whole guest allowance")
	}

	if !strings.Contains(err.Error(), "max_concurrent") {
		t.Errorf("the refusal does not name what the operator has to divide: %v", err)
	}
}

// A COLLISION WITH A HAND-WRITTEN TIER NAMES THE LADDER, not the label.
//
// validateTiers refuses a duplicate label anyway; its message would say the
// label appears twice, which an operator staring at `sizes: [2, 4]` and a tier
// called `web-2vcpu` has to work backwards from.
func TestALadderThatCollidesWithAHandWrittenTierSaysSo(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 64
  max_memory: 256GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/key.pem
tiers:
  - label: web
    provider: docker
    trust: trusted
    runner_group: billet
    workflows: ["acme/repo/.github/workflows/ci.yml@refs/heads/main"]
    image: ghcr.io/actions/actions-runner:latest
    sizes: [2, 4]
  - label: web-2vcpu
    provider: docker
    trust: trusted
    runner_group: billet
    workflows: ["acme/repo/.github/workflows/ci.yml@refs/heads/main"]
    image: ghcr.io/actions/actions-runner:latest
    vcpu: 2
    memory: 4GiB
`

	_, err := parseSizes(t, body)
	if err == nil {
		t.Fatal("a ladder silently collided with a hand-written tier")
	}

	if !strings.Contains(err.Error(), "sizes") {
		t.Errorf("the refusal does not name the ladder that produced the collision: %v", err)
	}
}

// ...AND A COLLISION BETWEEN TWO HAND-WRITTEN TIERS IS NOT BLAMED ON A LADDER.
//
// The first version of the message above reported every duplicate label as a
// ladder collision, so a config with no `sizes` anywhere in it was told about
// one — a sentence true of the input it was written for and false of the next
// one over. That is validateTiers' diagnostic, and it stays validateTiers'.
func TestADuplicateLabelWithNoLadderIsNotBlamedOnOne(t *testing.T) {
	t.Parallel()

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: /var/lib/billet/server
  max_vcpu: 64
  max_memory: 256GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: /etc/billet/key.pem
tiers:
  - label: web
    provider: docker
    trust: trusted
    runner_group: billet
    workflows: ["acme/repo/.github/workflows/ci.yml@refs/heads/main"]
    image: ghcr.io/actions/actions-runner:latest
    vcpu: 2
    memory: 4GiB
  - label: web
    provider: docker
    trust: trusted
    runner_group: billet
    workflows: ["acme/repo/.github/workflows/ci.yml@refs/heads/main"]
    image: ghcr.io/actions/actions-runner:latest
    vcpu: 4
    memory: 8GiB
`

	_, err := parseSizes(t, body)
	if err == nil {
		t.Fatal("two tiers sharing a label were accepted")
	}

	if strings.Contains(err.Error(), "after `sizes` expansion") {
		t.Errorf("a duplicate with no ladder anywhere in the file was blamed on one: %v", err)
	}
}

// AN EXPANSION DOES NOT SHARE ITS SIBLINGS' SLICES.
//
// A Tier holds slices and a map. Copying the struct shares their backing
// storage, so a later edit to one expansion's workflows — or a provider list
// normalized in place — would reach every other tier the same entry produced. The
// symptom would be one tier's allowlist appearing on three scale sets.
func TestExpandedTiersDoNotShareTheirSlices(t *testing.T) {
	t.Parallel()

	tiers, err := ExpandTierSizes([]Tier{{
		Label:     "web",
		Provider:  ProviderDocker,
		Sizes:     []int{2, 4},
		Workflows: []string{"acme/repo/.github/workflows/ci.yml@refs/heads/main"},
		Command:   []string{"./run.sh"},
	}})
	if err != nil {
		t.Fatalf("ExpandTierSizes: %v", err)
	}

	if len(tiers) != 2 {
		t.Fatalf("expanded to %d tiers, want 2", len(tiers))
	}

	tiers[0].Workflows[0] = "somebody/else/.github/workflows/x.yml@refs/heads/main"
	tiers[0].Command[0] = "./somebody-elses.sh"

	if tiers[1].Workflows[0] != "acme/repo/.github/workflows/ci.yml@refs/heads/main" {
		t.Error("editing one expansion's workflow allowlist reached its sibling, so one " +
			"tier's routing policy would appear on every scale set the ladder created")
	}

	if tiers[1].Command[0] != "./run.sh" {
		t.Error("editing one expansion's command reached its sibling")
	}
}

// AND A CATALOGUE THAT NEVER WENT THROUGH Parse IS REFUSED BY THE ALLOCATOR.
//
// Asserted here rather than only in internal/alloc because it is this package's
// rule: alloc.New is exported and cannot prove its catalogue came through Parse,
// and an unexpanded ladder there has vcpu zero — so without a refusal naming the
// ladder, the message would be about headroom dividing by zero and the sizes the
// operator declared would be a field nothing ever read.
//
// The expansion is idempotent, which is what makes the refusal safe to add: a
// catalogue that HAS been through Parse carries no Sizes at all.
func TestExpansionLeavesNoLadderBehind(t *testing.T) {
	t.Parallel()

	tiers, err := ExpandTierSizes([]Tier{{
		Label: "web", Provider: ProviderDocker, Sizes: []int{2, 4},
	}})
	if err != nil {
		t.Fatalf("ExpandTierSizes: %v", err)
	}

	for i := range tiers {
		if len(tiers[i].Sizes) != 0 || tiers[i].MemoryPerVCPU != 0 {
			t.Errorf("tier %s still carries its ladder after expansion: %+v",
				tiers[i].Label, tiers[i])
		}
	}

	// Idempotent: expanding again changes nothing, so a caller that expanded
	// twice — Parse, then a command that re-derived — cannot double it.
	again, err := ExpandTierSizes(tiers)
	if err != nil {
		t.Fatalf("a second expansion failed: %v", err)
	}

	if len(again) != len(tiers) {
		t.Errorf("a second expansion produced %d tiers from %d", len(again), len(tiers))
	}
}
