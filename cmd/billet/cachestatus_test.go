package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// THE KILL SWITCH'S KINDS ARE THE CACHES CONFIG KNOWS, plus all, and nothing
// else reaches the ledger.
func TestTheKillSwitchKindIsACacheOrAll(t *testing.T) {
	t.Parallel()

	for flag, want := range map[string]string{"all": state.AllCaches, "docker": "docker", "go": "go"} {
		got, _, err := cachePolicyKind(flag)
		if err != nil || got != want {
			t.Errorf("--kind %s = %q, %v, want %q", flag, got, err, want)
		}
	}
	for _, flag := range []string{"", "*", "Docker", "npm"} {
		if _, _, err := cachePolicyKind(flag); err == nil {
			t.Errorf("--kind %q was accepted", flag)
		}
	}
}

// STATUS SAYS WHAT EACH TIER GETS with every default applied, and names every
// block, so what an operator reads is what the node enforces.
func TestCacheStatusRendersEffectiveCachesAndBlocks(t *testing.T) {
	t.Parallel()

	enabled := true
	tiers := []config.Tier{
		{Label: "legacy"},
		{Label: "pr", CacheScope: &config.CacheScope{Owner: "acme", Repository: "api"},
			Cache: &config.TierCache{Publish: config.CachePublishDefaultBranch,
				Go: &config.GoCache{Enabled: &enabled, TestResults: true}}},
	}
	var out bytes.Buffer
	printTierCaches(&out, tiers)
	printCacheBlocks(&out, []state.CacheBlock{
		{Kind: state.AllCaches, Owner: "acme"},
		{Kind: "docker", Owner: "acme", Repository: "web"},
	})
	text := out.String()
	for _, want := range []string{
		"legacy  trusted-only", "docker(100GiB) sticky(100GiB)",
		"pr      default-branch  acme/api", "go(20GiB)+tests",
		"org acme", "all", "acme/web", "docker",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status does not say %q:\n%s", want, text)
		}
	}
}
