package config

import (
	"strings"
	"testing"
)

// A SITE IS A CACHE AUTHORITY, AND A BUILD ATTACHES TO NONE. The control plane
// already refuses this pairing at registration; that refusal arrives after the file
// loaded cleanly and talks about splitting a site across cache authorities, which is
// true and is not the first thing an operator needs to hear. Measured on the first
// live acceptance run.
func TestACodeBuildNodeCannotDeclareASite(t *testing.T) {
	got := loadErr(t, codeBuildConfig(t, "  name: aws-cb-1\n", "  name: aws-cb-1\n  site: home\n"))

	if !strings.Contains(got, "node.site is set but this node's provider is codebuild") {
		t.Errorf("a site on a codebuild node was not refused: %s", got)
	}

	if !strings.Contains(got, "cache authority") {
		t.Errorf("the refusal does not say what a site is: %s", got)
	}
}

// sitedFallbackTier is a tier listing the given providers, with the per-backend
// images a multi-provider tier must name, and `site: home`.
func sitedFallbackTier(providers string) string {
	return "  - label: billet-4vcpu-ubuntu-2404\n" +
		"    providers: [" + providers + "]\n" +
		"    site: home\n" +
		"    launch:\n" +
		"      firecracker:\n" +
		"        image: ubuntu-2404-x64\n" +
		"      codebuild:\n" +
		"        image: aws/codebuild/amazonlinux-x86_64-standard:5.0\n"
}

// A SITED TIER NEVER PLACES ON A CODEBUILD NODE, AND THE SYMPTOM IS SILENCE.
// Placement confines a sited tier to hosts at that site and a codebuild node has
// none, so the fallback never fires; with codebuild as the only provider the tier
// advertises 0 while `billet check` reports everything healthy. Measured on the
// live acceptance re-run, where the job queued with no line saying why.
func TestASitedTierCannotListCodeBuild(t *testing.T) {
	original := "  - label: billet-4vcpu-ubuntu-2404\n" +
		"    provider: firecracker\n" +
		"    vcpu: 4\n" +
		"    memory: 16GiB\n" +
		"    disk: 80GiB\n" +
		"    image: ubuntu-2404-x64\n"

	replace := func(t *testing.T, tier string) string {
		t.Helper()

		body := withSites("home")
		if !strings.Contains(body, original) {
			t.Fatal("validConfig's first tier has changed, so this case patches nothing")
		}

		return strings.Replace(body, original,
			tier+"    vcpu: 4\n    memory: 16GiB\n    disk: 80GiB\n", 1)
	}

	t.Run("firecracker then codebuild", func(t *testing.T) {
		got := loadErr(t, replace(t, sitedFallbackTier("firecracker, codebuild")))

		if !strings.Contains(got, `tiers[0] (billet-4vcpu-ubuntu-2404): site "home"`) {
			t.Errorf("the refusal does not name the tier and its site: %s", got)
		}

		if !strings.Contains(got, "codebuild could never be placed for this tier") {
			t.Errorf("the refusal does not say why the pairing is refused: %s", got)
		}
	})

	// THE OTHER DIRECTION, or a validator that refuses every sited tier passes.
	// The same tier with the codebuild half removed is the ordinary sited case.
	t.Run("firecracker alone still loads", func(t *testing.T) {
		tier := strings.Replace(sitedFallbackTier("firecracker"),
			"      codebuild:\n        image: aws/codebuild/amazonlinux-x86_64-standard:5.0\n",
			"", 1)

		if _, err := Load(writeConfig(t, replace(t, tier))); err != nil {
			t.Fatalf("a sited firecracker tier no longer loads: %v", err)
		}
	})

	// AND THE SAME TIER WITHOUT THE SITE LOADS, so the refusal is about the pairing
	// and not about a multi-provider tier naming codebuild at all.
	t.Run("the fallback tier loads without a site", func(t *testing.T) {
		tier := strings.Replace(sitedFallbackTier("firecracker, codebuild"),
			"    site: home\n", "", 1)

		if _, err := Load(writeConfig(t, replace(t, tier))); err != nil {
			t.Fatalf("a firecracker-then-codebuild tier no longer loads: %v", err)
		}
	})
}
