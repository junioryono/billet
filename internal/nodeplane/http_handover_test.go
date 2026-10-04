package nodeplane_test

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// OVER THE REAL WIRE, A REGISTRATION TAKES OVER ONLY WHAT IT REPORTS HOLDING
// (#374). The handler adopts the ledger's launched leases as well as the reported
// ones, and only the reported ones may move from a withdrawn process: a host that
// shares the name and reports nothing must not become what a completion's destroy
// reaches, or its no-op answer would free capacity under a guest still running.
func TestARegistrationTakesOverOnlyWhatItReportsHolding(t *testing.T) {
	t.Parallel()

	store := &fakeStore{launched: map[string]bool{"l9": true}}
	p, base := serve(t, store, nodeplane.WithRegistrar(&fakeRegistrar{}))

	register := func(instances ...string) *nodeclient.Client {
		t.Helper()

		c := dial(t, base)
		if err := c.Register(t.Context(), nodeclient.Registration{
			Provider: config.ProviderDocker, Deployment: deployment,
			VCPU: testNodeVCPU, Memory: testNodeMemory,
			InventoryKnown: true, Instances: instances,
		}); err != nil {
			t.Fatalf("register %s: %v", c.Incarnation(), err)
		}

		return c
	}

	first := register("l9")
	if !p.OwnsForTest("l9", "n1", first.Incarnation()) {
		t.Fatal("the process that reported l9 does not own it")
	}
	if err := first.Withdraw(t.Context()); err != nil {
		t.Fatalf("Withdraw: %v", err)
	}

	empty := register()
	if !p.OwnsForTest("l9", "n1", first.Incarnation()) {
		t.Fatalf("a registration reporting nothing took l9 from the withdrawn process "+
			"(now owned by %s?)", empty.Incarnation())
	}

	holder := register("l9")
	if !p.OwnsForTest("l9", "n1", holder.Incarnation()) {
		t.Error("the registration that reported l9 did not take it over from the withdrawn process")
	}
}
