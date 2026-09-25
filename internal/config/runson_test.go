package config

import (
	"strings"
	"testing"
)

// withRunsOn gives the fixture's first tier a runs_on and a target.
func withRunsOn(t *testing.T, body, target, runsOn string) string {
	t.Helper()

	const tier = "  - label: billet-4vcpu-ubuntu-2404\n"
	if !strings.Contains(body, tier) {
		t.Fatal("the fixture's first tier has changed, so this case patches nothing")
	}

	return strings.Replace(body, tier,
		tier+"    target: "+target+"\n    runs_on: "+runsOn+"\n", 1)
}

// twoTargets is the fixture with a second target and its second tier on the
// default one.
func twoTargets(t *testing.T) string {
	t.Helper()

	body := twoTargetConfig(t, "")

	const second = "  - label: billet-8vcpu-ubuntu-2404\n"
	if !strings.Contains(body, second) {
		t.Fatal("the fixture's second tier has changed, so this case patches nothing")
	}

	return strings.Replace(body, second, second+"    target: default\n", 1)
}

func TestTiersOnDifferentTargetsMayShareAScaleSetName(t *testing.T) {
	body := withRunsOn(t, twoTargets(t), "personal", "billet-8vcpu-ubuntu-2404")

	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load refused one scale-set name on two targets: %v", err)
	}

	if got := cfg.Tiers[0].ScaleSetName(); got != "billet-8vcpu-ubuntu-2404" {
		t.Errorf("the tier with runs_on answers to %q", got)
	}

	if got := cfg.Tiers[1].ScaleSetName(); got != "billet-8vcpu-ubuntu-2404" {
		t.Errorf("a tier without runs_on answers to %q, not its label", got)
	}
}

func TestTiersOnOneTargetMayNotShareAScaleSetName(t *testing.T) {
	body := withRunsOn(t, twoTargets(t), "default", "billet-8vcpu-ubuntu-2404")

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted two tiers answering to one name on one target")
	}

	for _, want := range []string{
		`tier "billet-8vcpu-ubuntu-2404"`, "billet-4vcpu-ubuntu-2404",
		"unique within its target",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not carry %q: %v", want, err)
		}
	}
}

func TestRunsOnIsALabel(t *testing.T) {
	body := withRunsOn(t, validConfig, "default", "'has space'")

	_, err := Load(writeConfig(t, body))
	if err == nil {
		t.Fatal("Load accepted a runs_on that is not a label")
	}

	if !strings.Contains(err.Error(), "runs_on must match") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestSizesExpandRunsOnAsTheyExpandTheLabel(t *testing.T) {
	got, err := ExpandTierSizes([]Tier{{Label: "taksa", RunsOn: "shared", Sizes: []int{2, 4}}})
	if err != nil {
		t.Fatalf("ExpandTierSizes: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("expanded to %d tiers, want 2", len(got))
	}

	if got[0].RunsOn != "shared-2vcpu" || got[1].RunsOn != "shared-4vcpu" {
		t.Errorf("expanded runs_on as %q and %q", got[0].RunsOn, got[1].RunsOn)
	}

	plain, err := ExpandTierSizes([]Tier{{Label: "plain", Sizes: []int{2}}})
	if err != nil {
		t.Fatalf("ExpandTierSizes: %v", err)
	}

	if len(plain) != 1 {
		t.Fatalf("expanded to %d tiers, want 1", len(plain))
	}

	if plain[0].RunsOn != "" || plain[0].ScaleSetName() != "plain-2vcpu" {
		t.Errorf("a ladder without runs_on answers to %q", plain[0].ScaleSetName())
	}
}

// Uniqueness is per GitHub owner, not per config name: a second target on the
// same organization, or a repository inside it, cannot answer to a name the
// organization's tiers already do.
func TestAScaleSetNameIsUniquePerOwnerNotPerTargetName(t *testing.T) {
	for name, entry := range map[string]string{
		"the same organization":        "    org: acme\n",
		"a repository inside that org": "    repository: acme/widgets\n",
	} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(twoTargets(t), "    repository: someone/widgets\n", entry, 1)
			if !strings.Contains(body, entry) {
				t.Fatal("the fixture's second target has changed, so this case patches nothing")
			}

			body = withRunsOn(t, body, "personal", "billet-8vcpu-ubuntu-2404")

			_, err := Load(writeConfig(t, body))
			if err == nil {
				t.Fatal("Load accepted one scale-set name twice on one owner")
			}

			if !strings.Contains(err.Error(), "billet-8vcpu-ubuntu-2404") {
				t.Errorf("the refusal does not name the scale set: %v", err)
			}
		})
	}
}
