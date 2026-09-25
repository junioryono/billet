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

	current := testRegistration("new-host", config.ProviderFirecracker)
	current.WireMin, current.WireVersion, current.WireMax = 12, CacheAuthorityWireVersion,
		CacheAuthorityWireVersion
	if _, err := a.RegisterNode(t.Context(), current); err != nil {
		t.Fatalf("RegisterNode(new): %v", err)
	}
	leases, err := a.Escrow(t.Context(), "configured", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("Escrow = %v, %v", leases, err)
	}
	if leases[0].TargetNode != "new-host" {
		t.Fatalf("configured lease placed on %q, want new-host", leases[0].TargetNode)
	}
}
