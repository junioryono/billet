package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/state"
)

// A TIER NO REACHABLE HOST CAN HONOUR IS SAID TO WAIT, by name, until one host
// this deployment can reach speaks the version that reads its cache block; a
// tier any host can run is never named.
func TestStatusNamesATierWaitingForACacheAwareHost(t *testing.T) {
	t.Parallel()

	off := false
	restrictive := config.Tier{Label: "strict", Provider: config.ProviderFirecracker, GuestOS: config.GuestLinux,
		Cache: &config.TierCache{StickyDisks: &config.CacheToggle{Enabled: &off}}}
	plain := config.Tier{Label: "plain", Provider: config.ProviderFirecracker, GuestOS: config.GuestLinux}
	tiers := []config.Tier{restrictive, plain}
	old := alloc.NodeWire{Name: "old", Live: true, Negotiated: nodeapi.VersionCacheAuthority - 1}
	gone := alloc.NodeWire{Name: "gone", Negotiated: nodeapi.VersionCacheAuthority}
	current := alloc.NodeWire{Name: "new", Live: true, Negotiated: nodeapi.VersionCacheAuthority}

	lines := cacheAwareWaits(tiers, []alloc.NodeWire{old, gone})
	if len(lines) != 1 || !strings.Contains(lines[0], "tier strict WAITS") {
		t.Fatalf("with no reachable host on the version = %q, want the strict tier named", lines)
	}
	if lines := cacheAwareWaits(tiers, []alloc.NodeWire{old, current}); len(lines) != 0 {
		t.Fatalf("with a reachable host on the version = %q, want nothing", lines)
	}
}

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
