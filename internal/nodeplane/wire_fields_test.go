package nodeplane_test

import (
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/nodeplane"
)

// WHAT THE NODE SAID HAS TO REACH THE LEDGER, and nothing was proving it did.
//
// The registration path is node config → HTTP body → plane → registrar, and the
// only thing asserted across it was the node's NAME. A serialiser that dropped
// the capacity, a plane that forgot to copy a field into the registration, or a
// json tag that never matched would all have passed — and the failure is silent
// downstream: a host recorded as contributing nothing joins the fleet, is never
// chosen, and produces no error anywhere.
func TestWhatTheNodeReportsArrivesAtTheLedger(t *testing.T) {
	t.Parallel()

	const (
		wantVCPU   = 96
		wantMemory = 384 * config.GiB
		wantSite   = "home"
	)

	reg := &fakeRegistrar{}
	wantShapes := []config.EC2InstanceType{{
		Type: "c7i.2xlarge", VCPU: 8, Memory: 16 * config.GiB,
		PriceUSDPerHour: 340_000,
	}}

	_, base := serve(t, &fakeStore{}, nodeplane.WithRegistrar(reg),
		nodeplane.WithSites([]config.SiteConfig{{Name: wantSite, Store: config.SiteStoreEBSS3}}))

	c := dial(t, base)

	if err := c.Register(t.Context(), nodeclient.Registration{
		Provider:   config.ProviderEC2,
		Deployment: deployment,
		Site:       wantSite,
		VCPU:       wantVCPU,
		Memory:     wantMemory,
		EC2Shapes:  wantShapes,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}

	got := reg.lastRegistration()

	if got.VCPU != wantVCPU {
		t.Errorf("vcpu reached the ledger as %d, want %d", got.VCPU, wantVCPU)
	}

	if got.Memory != wantMemory {
		t.Errorf("memory reached the ledger as %s, want %s", got.Memory, wantMemory)
	}

	if got.Site != wantSite {
		t.Errorf("site reached the ledger as %q, want %q", got.Site, wantSite)
	}

	if got.Provider != config.ProviderEC2 {
		t.Errorf("provider reached the ledger as %q, want %q", got.Provider, config.ProviderEC2)
	}

	if len(got.EC2Shapes) != 1 || got.EC2Shapes[0] != wantShapes[0] {
		t.Errorf("EC2 shapes reached the ledger as %+v, want %+v", got.EC2Shapes, wantShapes)
	}
}
