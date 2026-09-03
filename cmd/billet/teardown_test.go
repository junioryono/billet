package main

import (
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

func teardownTiers() []config.Tier {
	return []config.Tier{
		{Label: "billet-2vcpu", RunnerGroup: "billet"},
		{Label: "billet-4vcpu", RunnerGroup: "trusted"},
	}
}

// The case the whole change exists for: a tier removed from the config still has
// a scale set on GitHub, and nothing else can name it.
func TestATierTheConfigNoLongerDeclaresIsStillDeletable(t *testing.T) {
	got, undeclared, err := teardownTargets(teardownTiers(), "billet-8vcpu", "trusted", false)
	if err != nil {
		t.Fatalf("teardownTargets: %v", err)
	}

	if !undeclared {
		t.Error("a tier absent from the config was not reported as undeclared, so the " +
			"operator is not told they are acting outside it")
	}

	if len(got) != 1 {
		t.Fatalf("wanted one target, got %d: %v", len(got), got)
	}

	if got[0].Label != "billet-8vcpu" {
		t.Errorf("target label is %q", got[0].Label)
	}

	// The group is the part the config was carrying, so it has to come from the
	// operator — defaulting silently would look in a group the tier was never in.
	if got[0].RunnerGroup != "trusted" {
		t.Errorf("target runner group is %q, not the one the operator named", got[0].RunnerGroup)
	}
}

// A declared tier is unchanged, and takes its OWN group.
func TestADeclaredTierUsesItsDeclaredRunnerGroup(t *testing.T) {
	got, undeclared, err := teardownTargets(teardownTiers(), "billet-4vcpu", "", false)
	if err != nil {
		t.Fatalf("teardownTargets: %v", err)
	}

	if undeclared {
		t.Error("a declared tier was reported as undeclared")
	}

	if len(got) != 1 || got[0].RunnerGroup != "trusted" {
		t.Fatalf("wanted the declared group trusted, got %v", got)
	}
}

// A FLAG MUST NOT REDIRECT A DECLARED TIER. Deleting from a group the tier was
// never in finds nothing, reports it, and leaves the real object in place —
// which reads as "already gone".
func TestARunnerGroupFlagCannotOverrideADeclaredTier(t *testing.T) {
	_, _, err := teardownTargets(teardownTiers(), "billet-4vcpu", "somewhere-else", false)
	if err == nil {
		t.Fatal("a --runner-group contradicting the declared tier was accepted")
	}

	for _, want := range []string{"billet-4vcpu", "trusted", "somewhere-else"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// The same group, stated explicitly, is not a contradiction.
func TestARunnerGroupFlagMatchingTheDeclaredTierIsAccepted(t *testing.T) {
	got, undeclared, err := teardownTargets(teardownTiers(), "billet-4vcpu", "trusted", false)
	if err != nil {
		t.Fatalf("teardownTargets: %v", err)
	}

	if undeclared || len(got) != 1 || got[0].Label != "billet-4vcpu" ||
		got[0].RunnerGroup != "trusted" {
		t.Fatalf("undeclared=%v got=%v", undeclared, got)
	}
}

// --all stays scoped to what the config declares.
func TestEveryDeclaredTierIsTheDefaultTarget(t *testing.T) {
	got, undeclared, err := teardownTargets(teardownTiers(), "", "", false)
	if err != nil {
		t.Fatalf("teardownTargets: %v", err)
	}

	if undeclared {
		t.Error("deleting every declared tier is not acting outside the config")
	}

	if len(got) != 2 || got[0].Label != "billet-2vcpu" || got[1].Label != "billet-4vcpu" ||
		got[0].RunnerGroup != "billet" || got[1].RunnerGroup != "trusted" {
		t.Fatalf("wanted both declared tiers with their own groups, got %v", got)
	}
}

// An empty config with no tier named has nothing to act on, and says so rather
// than reporting success over an empty list.
func TestNoTiersAndNoNameIsRefused(t *testing.T) {
	_, _, err := teardownTargets(nil, "", "", false)
	if err == nil {
		t.Fatal("an empty config with no --tier was accepted")
	}

	if !strings.Contains(err.Error(), "no tiers") {
		t.Errorf("the refusal does not say the config declares no tiers: %v", err)
	}
}

// ...but a NAMED tier still works against a config with none, which is exactly
// the state an operator reaches by deleting the last tier they had.
func TestANamedTierWorksAgainstAConfigWithNoTiers(t *testing.T) {
	got, undeclared, err := teardownTargets(nil, "billet-2vcpu", "billet", false)
	if err != nil {
		t.Fatalf("teardownTargets: %v", err)
	}

	if !undeclared || len(got) != 1 || got[0].Label != "billet-2vcpu" ||
		got[0].RunnerGroup != "billet" {
		t.Fatalf("undeclared=%v got=%v", undeclared, got)
	}
}

// THE GROUP IS NOT GUESSABLE for a tier the config no longer describes. Left
// empty it becomes the default group inside the client, so the command would
// delete from a group the operator never named while telling them they had.
func TestAnUndeclaredTierMustNameItsRunnerGroup(t *testing.T) {
	_, _, err := teardownTargets(teardownTiers(), "billet-8vcpu", "", false)
	if err == nil {
		t.Fatal("an undeclared tier with no --runner-group was accepted")
	}

	for _, want := range []string{"billet-8vcpu", "--runner-group"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
}

// --force ON AN UNDECLARED TIER HAS NO EVIDENCE LEFT. The name and the group both
// came from the operator and the expected label is derived from the name, so the
// label check is the only thing tying the object to billet; skipping it there
// deletes whatever was named.
func TestForcingAnUndeclaredTierIsRefused(t *testing.T) {
	_, _, err := teardownTargets(teardownTiers(), "billet-8vcpu", "billet", true)
	if err == nil {
		t.Fatal("--force on a tier the config does not declare was accepted")
	}

	if !strings.Contains(err.Error(), "only evidence") {
		t.Errorf("the refusal does not say the label check is the only evidence: %v", err)
	}
}

// ...but --force on a DECLARED tier is unchanged; it is why the flag exists.
func TestForcingADeclaredTierIsStillAllowed(t *testing.T) {
	got, undeclared, err := teardownTargets(teardownTiers(), "billet-2vcpu", "", true)
	if err != nil {
		t.Fatalf("teardownTargets: %v", err)
	}

	if undeclared || len(got) != 1 || got[0].Label != "billet-2vcpu" {
		t.Fatalf("undeclared=%v got=%v", undeclared, got)
	}
}

// A tier declaring no group uses the default, so naming that default is
// agreement rather than a contradiction.
func TestNamingTheDefaultGroupOfATierThatDeclaresNoneIsAccepted(t *testing.T) {
	tiers := []config.Tier{{Label: "billet-2vcpu"}}

	got, _, err := teardownTargets(tiers, "billet-2vcpu", groupOrDefault(""), false)
	if err != nil {
		t.Fatalf("naming the effective default group was refused: %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("got %v", got)
	}
}

// --runner-group says where to look for ONE tier. Ignored silently under --all,
// an operator believes it took effect.
func TestARunnerGroupWithoutATierIsRefused(t *testing.T) {
	_, _, err := teardownTargets(teardownTiers(), "", "billet", false)
	if err == nil {
		t.Fatal("--runner-group with no --tier was accepted")
	}

	if !strings.Contains(err.Error(), "--all") {
		t.Errorf("the refusal does not mention --all: %v", err)
	}
}
