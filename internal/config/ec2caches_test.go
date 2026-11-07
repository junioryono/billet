package config

import (
	"strings"
	"testing"
)

const ec2CacheNode = `  provider: ec2
  site: aws-us-west-2
  ebs_s3:
    region: us-west-2
    availability_zone: us-west-2a
    bucket: billet-cache-example
  cache:
    listen: 10.0.2.10:7718
    guest_endpoint: https://cache.aws.example:7718
    tls_cert: /etc/billet/cache.crt
    tls_key: /etc/billet/cache.key
`

// ec2CacheTiers is a config whose EC2 node serves its caches, and whose first tier
// runs there, with the given lines added to that tier.
func ec2CacheTiers(t *testing.T, lines string) string {
	t.Helper()
	body := cloudConfig(t, "  provider: ec2\n", ec2CacheNode)
	onEC2 := strings.Replace(body, "    provider: firecracker\n", "    provider: ec2\n", 1)
	if onEC2 == body {
		t.Fatal("the fixture has drifted: no tier to move to ec2")
	}
	withLines := strings.Replace(onEC2, "    image: ubuntu-2404-x64\n", "    image: ami-0abc\n"+lines, 1)
	if withLines == onEC2 {
		t.Fatal("the fixture has drifted: no tier image to patch")
	}

	return withLines
}

// EVERY CACHE THE NODE SERVES MAY BE TURNED ON ON A LINUX EC2 TIER (ADR-024), with the
// reverse held in the refusals' own test: a backend ADR-024 refuses stays refused.
func TestAnEC2TierMayNameEveryCacheTheNodeServes(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"git", "bazel", "go", "npm"} {
		block := "    cache:\n      " + kind + ": {enabled: true}\n"
		if _, err := Load(writeConfig(t, ec2CacheTiers(t, block))); err != nil {
			t.Errorf("%s on an EC2 tier: %v", kind, err)
		}
	}
}

func TestAnEC2TierGetsTheCachesAFirecrackerTierGets(t *testing.T) {
	t.Parallel()
	ec2 := Tier{Provider: ProviderEC2, GuestOS: GuestLinux, Trust: WorkloadTrusted}.EffectiveCache()
	firecracker := Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux, Trust: WorkloadTrusted}.EffectiveCache()
	if ec2 != firecracker {
		t.Fatalf("an EC2 tier's defaults = %+v, a Firecracker tier's = %+v", ec2, firecracker)
	}
	if !ec2.Git.Enabled || !ec2.Bazel.Enabled || !ec2.Go.Enabled || !ec2.Npm {
		t.Fatalf("a trusted EC2 tier's defaults = %+v", ec2)
	}
}

// A MAC'S GUEST IS NOT SERVED, whatever a default would say, because no store exists.
func TestAnEC2MacTierGetsNoGuestCaches(t *testing.T) {
	t.Parallel()
	for name, tier := range map[string]Tier{
		"a macOS guest":      {Provider: ProviderEC2, GuestOS: GuestMacOS, Trust: WorkloadTrusted},
		"a fallback tier":    {Providers: []ProviderKind{ProviderFirecracker, ProviderEC2}, GuestOS: GuestLinux, Trust: WorkloadTrusted},
		"the docker backend": {Provider: ProviderDocker, GuestOS: GuestLinux, Trust: WorkloadTrusted},
		"codebuild":          {Provider: ProviderCodeBuild, GuestOS: GuestLinux, Trust: WorkloadTrusted},
	} {
		if spec := tier.EffectiveCache(); spec.NeedsGuestCacheServices() {
			t.Errorf("%s has guest caches by default: %+v", name, spec)
		}
	}
}

// A TIER THAT ASKS FOR A CACHE ITS NODE HAS NO LISTENER FOR IS REFUSED, on EC2 as on
// Firecracker: without it the guest would be told about nothing.
func TestAnEC2TierNamingACacheNeedsTheNodesListener(t *testing.T) {
	t.Parallel()
	body := strings.Replace(validConfig, firecrackerNode, ec2Node, 1)
	body = strings.Replace(body, "    provider: firecracker\n", "    provider: ec2\n", 1)
	body = strings.Replace(body, "    image: ubuntu-2404-x64\n",
		"    image: ami-0abc\n    cache:\n      git: {enabled: true}\n", 1)
	if _, err := Load(writeConfig(t, body)); err == nil || !strings.Contains(err.Error(), "node.cache") {
		t.Fatalf("Load = %v, want it to name node.cache", err)
	}
}

func TestWithoutGuestCachesLeavesWhatAnOlderNodeServes(t *testing.T) {
	t.Parallel()
	on := true
	spec := Tier{Provider: ProviderEC2, GuestOS: GuestLinux, Trust: WorkloadTrusted, Cache: &TierCache{
		Actions: &ActionsCache{Enabled: &on},
		Go:      &GoCache{Enabled: &on, TestResults: true},
		Npm:     &NpmCache{Enabled: &on},
	}}.EffectiveCache()
	less := spec.WithoutGuestCaches()
	if less.NeedsGuestCacheServices() || less.GoTestResults {
		t.Fatalf("guest caches survived: %+v", less)
	}
	if !less.Docker.Enabled || !less.StickyDisks.Enabled || less.Publish != spec.Publish {
		t.Fatalf("what an older node serves went with them: %+v", less)
	}
}
