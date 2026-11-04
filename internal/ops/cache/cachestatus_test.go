package cache

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// WHAT THE CACHES DID IS RENDERED PER TIER AND CACHE, a cache nothing observed
// is left out, and a job whose outcome was not observed is counted as that.
func TestCacheStatusRendersWhatTheCachesDid(t *testing.T) {
	t.Parallel()

	var out strings.Builder
	printCacheOutcomes(&out, map[string]map[string]map[string]int{
		"linux-8": {
			"git":   {"warm": 3, "cold": 1, "": 1},
			"bazel": {"": 5},
		},
	}, 5, 24*time.Hour)
	text := out.String()
	for _, want := range []string{"5 job(s) assigned in the last", "linux-8", "git",
		"not observed 1, cold 1, warm 3"} {
		if !strings.Contains(text, want) {
			t.Errorf("the report lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "bazel") {
		t.Errorf("a cache nothing observed was reported:\n%s", text)
	}
}

// A BUILD CACHE THAT KEEPS GOING COLD AND NEVER WARMS IS NAMED, and only that: one
// warm job, too few cold ones, or a cache the warning is not about says nothing.
func TestABuildCacheThatNeverWarmsIsNamed(t *testing.T) {
	t.Parallel()

	got := neverWarmed(map[string]map[string]map[string]int{
		"always-cold":  {"go": {"cold": neverWarmAfter}},
		"bazel-cold":   {"bazel": {"cold": neverWarmAfter + 1, "unused": 3}, "go": {"unused": 20}},
		"warmed-once":  {"go": {"cold": 100, "warm": 1}},
		"just-started": {"go": {"cold": neverWarmAfter - 1}},
		"git-cold":     {"git": {"cold": 100}},
	}, 24*time.Hour)

	if len(got) != 2 || !strings.Contains(got[0], "always-cold's go cache was cold in 10 job(s)") ||
		!strings.Contains(got[1], "bazel-cold's bazel cache was cold in 11 job(s)") {
		t.Fatalf("neverWarmed = %q", got)
	}
}

// THE KILL SWITCH'S KINDS ARE THE CACHES CONFIG KNOWS, plus all, and nothing
// else reaches the ledger.
func TestTheKillSwitchKindIsACacheOrAll(t *testing.T) {
	t.Parallel()

	for flag, want := range map[string]string{"all": state.AllCaches, "docker": "docker", "go": "go", "npm": "npm"} {
		got, _, err := cachePolicyKind(flag)
		if err != nil || got != want {
			t.Errorf("--kind %s = %q, %v, want %q", flag, got, err, want)
		}
	}
	for _, flag := range []string{"", "*", "Docker", "pip"} {
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
