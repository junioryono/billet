package nodeapi

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// THE PLACEMENT AND CACHE FACTS RIDE THE LEASE ACROSS THE WIRE, in a launch
// command, and a node decodes them leniently: a field the node does not know
// is dropped, one it does is kept.
func TestALeaseCarriesItsPlacementAndCacheFactsAcrossTheWire(t *testing.T) {
	t.Parallel()

	lease := &alloc.Lease{
		ID: "l1", Tier: "cloud", VCPU: 8, Memory: 16 * config.GiB,
		InstanceType: "eight", Site: "us-east", PriceUSDPerHour: 340_000,
		ImageCache: alloc.ImageCacheWarm, CacheGeneration: "gen-7",
		ActionsCache: alloc.ActionsCacheServed,
		Epoch:        3,
	}

	raw, err := json.Marshal(Command{ID: "c1", Kind: CommandLaunch, Lease: lease})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got Command
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.Lease == nil || !reflect.DeepEqual(got.Lease, lease) {
		t.Fatalf("lease after the round trip = %+v, want %+v", got.Lease, lease)
	}
}
