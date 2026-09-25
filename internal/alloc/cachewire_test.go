package alloc

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A TIER THAT CONFIGURES ITS CACHES IS NOT PLACED ON A HOST TOO OLD TO HONOUR
// IT, so placement does not keep choosing a host the plane then refuses to
// launch on while a newer one sits idle. A legacy tier still uses every host.
func TestACacheConfiguredTierIsPlacedOnlyWhereItCanBeHonoured(t *testing.T) {
	t.Parallel()

	legacy := tier("legacy", 2, 4*config.GiB)
	configured := tier("configured", 2, 4*config.GiB)
	off := false
	configured.Cache = &config.TierCache{StickyDisks: &config.CacheToggle{Enabled: &off}}

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{legacy, configured})

	old := testRegistration("old-host", config.ProviderFirecracker)
	old.WireMin, old.WireVersion, old.WireMax = 12, CacheAuthorityWireVersion-1, CacheAuthorityWireVersion-1
	if _, err := a.RegisterNode(t.Context(), old); err != nil {
		t.Fatalf("RegisterNode(old): %v", err)
	}

	if got := headroom(t, a, "configured"); got != 0 {
		t.Fatalf("configured headroom on an old host = %d, want 0", got)
	}
	if got := headroom(t, a, "legacy"); got == 0 {
		t.Fatal("a legacy tier could not use an old host")
	}
	// THE WAIT IS REPORTED BY THE TIER'S OWN HOSTS: the configured tier waits
	// on the old host; the legacy tier, which the old host serves, does not.
	for _, tc := range []struct {
		tier config.Tier
		want bool
	}{{configured, true}, {legacy, false}} {
		if waits, err := a.WaitsForCacheAwareHost(t.Context(), tc.tier); err != nil || waits != tc.want {
			t.Fatalf("tier %s waits = %v, %v; want %v", tc.tier.Label, waits, err, tc.want)
		}
	}

	current := testRegistration("new-host", config.ProviderFirecracker)
	current.WireMin, current.WireVersion, current.WireMax = 12, CacheAuthorityWireVersion,
		CacheAuthorityWireVersion
	if _, err := a.RegisterNode(t.Context(), current); err != nil {
		t.Fatalf("RegisterNode(new): %v", err)
	}
	if waits, err := a.WaitsForCacheAwareHost(t.Context(), configured); err != nil || waits {
		t.Fatalf("with a host on the version, the configured tier waits = %v, %v", waits, err)
	}
	leases, err := a.Escrow(t.Context(), "configured", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("Escrow = %v, %v", leases, err)
	}
	if leases[0].TargetNode != "new-host" {
		t.Fatalf("configured lease placed on %q, want new-host", leases[0].TargetNode)
	}
}

// A HOST TOO SMALL FOR ONE RUNNER DOES NOT END THE WAIT: a tier whose only
// host on the version cannot hold it is still waiting for one that can.
func TestAnUndersizedCurrentHostDoesNotEndTheWait(t *testing.T) {
	t.Parallel()

	configured := tier("configured", 32, 64*config.GiB)
	off := false
	configured.Cache = &config.TierCache{StickyDisks: &config.CacheToggle{Enabled: &off}}
	a := newBareAllocator(t, Limits{MaxVCPU: 1024, MaxMemory: 2048 * config.GiB}, []config.Tier{configured})

	old := testRegistration("old-host", config.ProviderFirecracker)
	old.WireMin, old.WireVersion, old.WireMax = 12, CacheAuthorityWireVersion-1, CacheAuthorityWireVersion-1
	small := testRegistration("small-host", config.ProviderFirecracker)
	small.VCPU, small.Memory = 4, 8*config.GiB
	for _, reg := range []NodeRegistration{old, small} {
		if _, err := a.RegisterNode(t.Context(), reg); err != nil {
			t.Fatalf("RegisterNode(%s): %v", reg.Name, err)
		}
	}
	if waits, err := a.WaitsForCacheAwareHost(t.Context(), configured); err != nil || !waits {
		t.Fatalf("with only an undersized host on the version, waits = %v, %v; want true", waits, err)
	}
}
