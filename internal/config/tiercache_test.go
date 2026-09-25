package config

import (
	"strings"
	"testing"
)

// untrustedCacheTier is validConfig's tier with a node cache listener, ready to
// have a cache block inserted after its image.
func untrustedCacheTier(block string) string {
	withCache := strings.Replace(validConfig, "  ceph:\n",
		"  cache:\n    listen: 172.20.0.1:7718\n    guest_endpoint: http://172.20.0.1:7718\n  ceph:\n", 1)

	return strings.Replace(withCache, "    image: ubuntu-2404-x64\n",
		"    image: ubuntu-2404-x64\n"+block, 1)
}

// A TIER THAT SAYS NOTHING GETS EVERY CACHE IT CAN HAVE: a Linux Firecracker
// tier the Git, Bazel and Go caches beside the Docker store and sticky disks,
// with Go test results left off, and a node without a cache listener refuses
// none of it, because a default that cannot run is simply off.
func TestATierWithNoCacheBlockGetsEveryCacheItCanHave(t *testing.T) {
	t.Parallel()

	cfg, err := Load(writeConfig(t, validConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	spec := cfg.Tiers[0].EffectiveCache()
	if !spec.Docker.Enabled || !spec.StickyDisks.Enabled || !spec.Git.Enabled ||
		!spec.Bazel.Enabled || !spec.Go.Enabled || spec.GoTestResults {
		t.Fatalf("effective cache of an unconfigured firecracker tier = %+v", spec)
	}
	// NO REPOSITORY, SO NO DEFAULT-BRANCH NAMESPACE AND NO ACTIONS CACHE: nothing
	// could prove whose default branch a job ran on, or scope its archives.
	if spec.Publish != CachePublishTrustedOnly || spec.Actions.Enabled || cfg.NeedsRunEvidence() {
		t.Fatalf("an unscoped tier = %+v, run evidence %v", spec, cfg.NeedsRunEvidence())
	}

	enabled := true
	goOnly := Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux,
		Cache: &TierCache{Go: &GoCache{Enabled: &enabled}}}.EffectiveCache()
	if !goOnly.Go.Enabled || goOnly.GoTestResults {
		t.Fatalf("a go cache that says nothing of test results = %+v, want them off", goOnly)
	}

	docker := Tier{Provider: ProviderDocker, GuestOS: GuestLinux}.EffectiveCache()
	if docker.Git.Enabled || docker.Go.Enabled || docker.Actions.Enabled {
		t.Fatalf("a docker tier's default = %+v, want the Docker store and sticky disks only", docker)
	}
}

// AN UNTRUSTED TIER WITH A REPOSITORY PUBLISHES FROM ITS DEFAULT BRANCH BY
// DEFAULT, with the Actions cache on; a trusted one publishes what it writes, as
// it always has.
func TestTheDefaultPublicationFollowsTrust(t *testing.T) {
	t.Parallel()

	scope := &CacheScope{Owner: "acme", Repository: "api"}
	untrusted := Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux, Trust: WorkloadUntrusted,
		CacheScope: scope}.EffectiveCache()
	if untrusted.Publish != CachePublishDefaultBranch || !untrusted.Actions.Enabled ||
		untrusted.Owner != "acme" || untrusted.Repository != "api" {
		t.Fatalf("an untrusted tier with a repository = %+v", untrusted)
	}
	trusted := Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux, Trust: WorkloadTrusted,
		CacheScope: scope}.EffectiveCache()
	if trusted.Publish != CachePublishTrustedOnly || trusted.Actions.Enabled {
		t.Fatalf("a trusted tier with no workflow scope = %+v", trusted)
	}
	explicit := Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux, Trust: WorkloadUntrusted,
		CacheScope: scope, Cache: &TierCache{Publish: CachePublishOff}}.EffectiveCache()
	if explicit.Publish != CachePublishOff {
		t.Fatalf("a tier's own publication rule was replaced: %+v", explicit)
	}
}

// AN UNTRUSTED POOL MAY PUBLISH FROM ITS DEFAULT BRANCH, with every cache, once
// it names the repository its namespace belongs to.
func TestAnUntrustedDefaultBranchTierIsAccepted(t *testing.T) {
	t.Parallel()

	body := untrustedCacheTier("    cache_scope:\n      owner: acme\n      repository: api\n" +
		"    cache:\n      publish: default-branch\n      actions: {enabled: true}\n" +
		"      git: {enabled: true}\n      bazel: {enabled: true, max_size: 20GiB}\n" +
		"      go: {enabled: true, test_results: true}\n")
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load refused an untrusted default-branch tier: %v", err)
	}
	spec := cfg.Tiers[0].EffectiveCache()
	if spec.Publish != CachePublishDefaultBranch || spec.Owner != "acme" || spec.Repository != "api" ||
		!spec.Actions.Enabled || !spec.Git.Enabled || spec.Bazel.MaxSize != 20*GiB ||
		!spec.Go.Enabled || !spec.GoTestResults {
		t.Fatalf("effective cache = %+v", spec)
	}
	if !cfg.NeedsRunEvidence() {
		t.Error("a default-branch tier did not ask for run evidence")
	}
}

// EVERY REFUSAL NAMES ITS CAUSE. Asserted on the clause, so the wrong refusal
// cannot pass for the right one.
func TestTheCacheBlockRefusesWhatItCannotHonour(t *testing.T) {
	t.Parallel()

	scope := "    cache_scope:\n      owner: acme\n      repository: api\n"
	for name, tc := range map[string]struct{ block, want string }{
		"an unknown policy": {
			block: "    cache:\n      publish: everyone\n", want: "cache.publish",
		},
		"default-branch with no repository": {
			block: "    cache:\n      publish: default-branch\n", want: "needs a static repository",
		},
		"an untrusted trusted-only Actions cache": {
			block: scope + "    cache:\n      publish: trusted-only\n      actions: {enabled: true}\n",
			want:  "only with cache.publish: default-branch",
		},
		"both spellings of the Actions cache": {
			block: scope + "    intercept: true\n    cache:\n      publish: default-branch\n" +
				"      actions: {enabled: true}\n",
			want: "deprecated spelling",
		},
		"a volume larger than a guest can be given": {
			block: "    cache:\n      docker: {max_size: 200GiB}\n", want: "cache.docker.max_size",
		},
		"an archive larger than GitHub's": {
			block: scope + "    cache:\n      publish: default-branch\n" +
				"      actions: {enabled: true, max_archive: 11GiB}\n",
			want: "cache.actions.max_archive",
		},
		"test results with no Go cache": {
			block: "    cache:\n      go: {test_results: true}\n", want: "needs cache.go.enabled",
		},
		"a scope segment that is a path": {
			block: "    cache_scope:\n      owner: acme\n      repository: a/b\n" +
				"    cache:\n      publish: default-branch\n",
			want: "cache_scope.repository",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(writeConfig(t, untrustedCacheTier(tc.block)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want one naming %q", err, tc.want)
			}
		})
	}
}

// A CACHE THE NODE SERVES NEEDS THE NODE'S LISTENER AND ITS HOST-BACKED STORE,
// whichever cache it is.
func TestEveryNodeServedCacheRequiresAHostBackedCacheNode(t *testing.T) {
	t.Parallel()

	for _, kind := range []string{"git", "bazel", "go"} {
		block := "    cache:\n      " + kind + ": {enabled: true}\n"
		without := strings.Replace(validConfig, "    image: ubuntu-2404-x64\n",
			"    image: ubuntu-2404-x64\n"+block, 1)
		if _, err := Load(writeConfig(t, without)); err == nil ||
			!strings.Contains(err.Error(), "node.cache") {
			t.Errorf("%s without a node cache: %v", kind, err)
		}

		remote := strings.Replace(untrustedCacheTier(block), "    provider: firecracker\n",
			"    provider: ec2\n", 1)
		if _, err := Load(writeConfig(t, remote)); err == nil ||
			!strings.Contains(err.Error(), "only the firecracker provider") {
			t.Errorf("%s on remote compute: %v", kind, err)
		}
	}
}

// A REPOSITORY TARGET IS ITS OWN SCOPE: a default-branch tier under one needs no
// cache_scope, and the repository is written onto the tier at load so it
// travels with the tier to every node.
func TestARepositoryTargetScopesItsDefaultBranchTier(t *testing.T) {
	t.Parallel()

	const tier = "  - label: billet-4vcpu-ubuntu-2404\n"
	body := strings.Replace(repositoryConfig(t), tier, tier+
		"    cache:\n      publish: default-branch\n      actions: {enabled: true}\n", 1)
	body = strings.Replace(body, "  ceph:\n",
		"  cache:\n    listen: 172.20.0.1:7718\n    guest_endpoint: http://172.20.0.1:7718\n  ceph:\n", 1)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load refused a default-branch tier under a repository target: %v", err)
	}
	scope := cfg.Tiers[0].CacheScope
	if scope == nil || scope.Owner != "acme" || scope.Repository != "widgets" {
		t.Fatalf("cache_scope = %+v, want acme/widgets from the target", scope)
	}
	if spec := cfg.Tiers[0].EffectiveCache(); spec.Owner != "acme" || spec.Repository != "widgets" {
		t.Errorf("effective scope = %s/%s", spec.Owner, spec.Repository)
	}
}

// ACTIONS: READ IS ONE TARGET'S NEED, never the deployment's: each target has
// its own App, and one target publishing from a default branch must not make
// another's minimal App fail its permission check.
func TestRunEvidenceIsRequiredPerTarget(t *testing.T) {
	t.Parallel()

	const tier = "  - label: billet-4vcpu-ubuntu-2404\n"
	body := strings.Replace(twoTargetConfig(t, "personal"), tier, tier+
		"    cache:\n      publish: default-branch\n", 1)
	const other = "  - label: billet-8vcpu-ubuntu-2404\n"
	body = strings.Replace(body, other, other+"    target: default\n", 1)
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.TargetNeedsRunEvidence("personal") {
		t.Error("the target with a default-branch tier was not asked for run evidence")
	}
	if cfg.TargetNeedsRunEvidence(DefaultTargetName) {
		t.Error("a target with no default-branch tier was asked for run evidence")
	}
}

// THE DEPRECATED SPELLING READS AS THE NEW ONE, so a trusted interception tier
// written before the cache block existed is unchanged.
func TestInterceptReadsAsTheActionsCache(t *testing.T) {
	t.Parallel()

	tier := Tier{Intercept: true}
	if spec := tier.EffectiveCache(); !spec.Actions.Enabled || tier.NeedsCacheAwareNode() {
		t.Fatalf("intercept: true = %+v, want the Actions cache on and nothing else changed", spec)
	}
}

// A TIER IS KEPT FROM AN OLDER NODE ONLY WHERE THAT NODE WOULD DO MORE THAN
// THE TIER ALLOWS. Every default does less or the same on one, so none of them
// needs a node that reads the cache block.
func TestOnlyATierAnOlderNodeWouldExceedNeedsACacheAwareNode(t *testing.T) {
	t.Parallel()

	off, small := false, 10*GiB
	scope := &CacheScope{Owner: "acme", Repository: "api"}
	firecracker := func(trust WorkloadTrust, cache *TierCache) Tier {
		return Tier{Provider: ProviderFirecracker, GuestOS: GuestLinux, Trust: trust,
			CacheScope: scope, Cache: cache}
	}
	for name, tc := range map[string]struct {
		tier Tier
		want bool
	}{
		"an untrusted tier's defaults":     {firecracker(WorkloadUntrusted, nil), false},
		"a trusted tier's defaults":        {firecracker(WorkloadTrusted, nil), false},
		"an untrusted tier publishing off": {firecracker(WorkloadUntrusted, &TierCache{Publish: CachePublishOff}), false},
		"a trusted tier publishing off":    {firecracker(WorkloadTrusted, &TierCache{Publish: CachePublishOff}), true},
		"a trusted default-branch tier": {firecracker(WorkloadTrusted,
			&TierCache{Publish: CachePublishDefaultBranch}), true},
		"sticky disks turned off": {firecracker(WorkloadUntrusted,
			&TierCache{StickyDisks: &CacheToggle{Enabled: &off}}), true},
		"a smaller docker store": {firecracker(WorkloadUntrusted,
			&TierCache{Docker: &CacheToggle{MaxSize: small}}), true},
		"a smaller actions archive": {firecracker(WorkloadUntrusted,
			&TierCache{Actions: &ActionsCache{MaxArchive: 5 * GiB}}), true},
	} {
		if got := tc.tier.NeedsCacheAwareNode(); got != tc.want {
			t.Errorf("%s: NeedsCacheAwareNode = %v, want %v", name, got, tc.want)
		}
	}
}
