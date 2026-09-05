package nodeplane_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A CACHE OBSERVATION CROSSES THE WIRE WITH ITS EPOCH, so the ledger can fence
// it the way it fences every other write from the process holding the lease.
func TestACacheObservationCrossesTheNodeWire(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	want := alloc.CacheObservation{
		ImageCache: alloc.ImageCacheWarm, CacheGeneration: "gen-7", ActionsCache: alloc.ActionsCacheServed,
	}
	if err := c.RecordCacheObservation(t.Context(), "l1", 7, want); err != nil {
		t.Fatalf("RecordCacheObservation: %v", err)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.observations) != 1 || store.observations[0].lease != "l1" ||
		store.observations[0].epoch != 7 || store.observations[0].obs != want {
		t.Fatalf("the ledger was asked to record %+v, want %+v for l1 at epoch 7",
			store.observations, want)
	}
}

// A TOKEN THIS CONTROL PLANE DOES NOT RECORD IS REFUSED AT THE BOUNDARY, as a
// phase is, and never reaches the ledger. A newer node's vocabulary is a
// permanent refusal rather than a transient error the node would retry.
func TestAnUnknownCacheTokenIsRefusedAtTheWire(t *testing.T) {
	t.Parallel()

	store := &fakeStore{}
	_, base := serve(t, store)
	c := dial(t, base)

	body := `{"epoch":7,"image_cache":"tepid"}`

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		base+"/v1/nodes/n1/leases/l1/cache", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	req.Header.Set(nodeapi.HeaderIncarnation, c.Incarnation())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()

	var refusal nodeapi.ErrorResponse
	if err := json.NewDecoder(resp.Body).Decode(&refusal); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}

	if resp.StatusCode != http.StatusBadRequest || refusal.Code != nodeapi.CodeRefused {
		t.Fatalf("an unknown token was answered %d %q, want 400 %q", resp.StatusCode, refusal.Code,
			nodeapi.CodeRefused)
	}

	store.mu.Lock()
	defer store.mu.Unlock()

	if len(store.observations) != 0 {
		t.Fatalf("the ledger was asked to record %+v from a refused request", store.observations)
	}
}
