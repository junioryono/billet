package server

import (
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// resizeOrder is the contending control plane, with the Server a resize asks.
func resizeOrder(t *testing.T, order config.AdmissionOrder) (*Server, []*Listener) {
	t.Helper()

	tiers := contenders()

	a, err := alloc.New(openState(t), alloc.Limits{MaxVCPU: 8, MaxMemory: 64 * config.GiB}, tiers)
	if err != nil {
		t.Fatal(err)
	}

	registerHost(t, a)

	s := New(a, nil, tiers, "resize-test", nil, WithNodeRunner(&launchLog{}), WithAdmissionOrder(order))

	listeners := make([]*Listener, 0, len(tiers))
	for i := range tiers {
		listeners = append(listeners, NewListener(a, tiers[i].Label, &fakeSession{}, s.listenerOpts(nil)...))
	}

	return s, listeners
}

// A RESIZE IS JUDGED AS A PURCHASE OUT OF TURN (#172). The small tier fills the
// ceiling and the large tier waits for it; a fallback growing one of the small
// tier's leases is asked to leave the large tier room for one lease, as a pool
// purchase ahead of its turn is, and under fill nothing is protected.
func TestAResizeLeavesTheWaitersAheadTheirRoom(t *testing.T) {
	t.Parallel()

	s, listeners := resizeOrder(t, config.AdmissionFair)
	small, large := listeners[0], listeners[1]

	if err := small.reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile: %v", err)
	}

	if err := large.reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile: %v", err)
	}

	if protect := s.ResizeAdmission(t.Context(), "a-small"); !slices.Equal(protect, []string{"b-large"}) {
		t.Errorf("a resize for the small tier is asked to protect %v, want the waiting large tier", protect)
	}

	// The waiter itself is not held behind anyone.
	if protect := s.ResizeAdmission(t.Context(), "b-large"); len(protect) != 0 {
		t.Errorf("the waiter's own resize is asked to protect %v, want nothing", protect)
	}

	fill, fillListeners := resizeOrder(t, config.AdmissionFill)

	if err := fillListeners[0].reconcilePool(t.Context(), 2); err != nil {
		t.Fatalf("small tier reconcile under fill: %v", err)
	}

	if err := fillListeners[1].reconcilePool(t.Context(), 1); err != nil {
		t.Fatalf("large tier reconcile under fill: %v", err)
	}

	if protect := fill.ResizeAdmission(t.Context(), "a-small"); len(protect) != 0 {
		t.Errorf("under fill a resize is asked to protect %v, want nothing", protect)
	}
}
