package alloc

import (
	"errors"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

func poolAllocator(t *testing.T, now *time.Time) *Allocator {
	t.Helper()

	return newBareAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 16 * config.GiB},
		[]config.Tier{archTier("intel", config.ArchX64)}, WithClock(func() time.Time { return *now }))
}

func poolOrder(id string) PoolOrder {
	return PoolOrder{ID: id, Source: "aws-metal", Shape: "z1d.metal", Location: "us-west-2a", Arch: config.ArchX64}
}

// AN ORDER IS COMMITTED BEFORE ITS SOURCE IS ASKED, and reads back exactly as written,
// with its first attempt, which bounds a resend.
func TestAPoolOrderReadsBackAsCommitted(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	if err := a.RecordPoolOrder(t.Context(), poolOrder("o-1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	orders, err := a.PoolOrders(t.Context())
	if err != nil || len(orders) != 1 {
		t.Fatalf("orders = %+v, %v", orders, err)
	}
	got := orders[0]
	if got.ID != "o-1" || got.Source != "aws-metal" || got.Shape != "z1d.metal" || got.Location != "us-west-2a" ||
		got.Arch != config.ArchX64 || got.State != PoolOrdering || got.HostID != "" || !got.FirstAttempt.Equal(now) {
		t.Fatalf("order = %+v", got)
	}
	if err := a.RecordPoolOrder(t.Context(), poolOrder("o-1")); err == nil {
		t.Fatal("an order identifier was recorded twice")
	}
}

// EVERY MOVE IS ONE WAY and starts from one state: a move asked of an order anywhere
// else is refused, so two processes cannot both decide what became of one order.
func TestPoolOrderMovesAreOneWay(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	ctx := t.Context()
	for _, id := range []string{"placed", "refused", "uncertain"} {
		if err := a.RecordPoolOrder(ctx, poolOrder(id)); err != nil {
			t.Fatalf("record %s: %v", id, err)
		}
	}
	now = now.Add(time.Minute)
	if err := a.PoolOrderPlaced(ctx, "placed", "i-1"); err != nil {
		t.Fatalf("placed: %v", err)
	}
	if err := a.PoolOrderRefused(ctx, "refused", "no stock"); err != nil {
		t.Fatalf("refused: %v", err)
	}
	if err := a.PoolOrderUncertain(ctx, "uncertain", "the answer was lost"); err != nil {
		t.Fatalf("uncertain: %v", err)
	}
	for name, move := range map[string]error{
		"place a placed order":          a.PoolOrderPlaced(ctx, "placed", "i-2"),
		"refuse a placed order":         a.PoolOrderRefused(ctx, "placed", "late"),
		"place a refused order":         a.PoolOrderPlaced(ctx, "refused", "i-3"),
		"place an uncertain order":      a.PoolOrderPlaced(ctx, "uncertain", "i-4"),
		"abandon a placed order":        a.PoolOrderAbandoned(ctx, "placed", "looked"),
		"place an order never recorded": a.PoolOrderPlaced(ctx, "nobody", "i-5"),
	} {
		if !errors.Is(move, ErrPoolOrderMoved) {
			t.Errorf("%s: %v", name, move)
		}
	}
	if err := a.PoolOrderAbandoned(ctx, "uncertain", "the console shows no such server"); err != nil {
		t.Fatalf("abandon: %v", err)
	}
	orders, err := a.PoolOrders(ctx)
	if err != nil {
		t.Fatalf("orders: %v", err)
	}
	want := map[string]struct{ state, host, reason string }{
		"placed":    {PoolPlaced, "i-1", ""},
		"refused":   {PoolRefused, "", "no stock"},
		"uncertain": {PoolAbandoned, "", "the console shows no such server"},
	}
	for _, o := range orders {
		w := want[o.ID]
		if o.State != w.state || o.HostID != w.host || o.Reason != w.reason || !o.Updated.Equal(now) {
			t.Errorf("%s = %+v, want %+v", o.ID, o, w)
		}
	}
}

func TestAPoolOrderMissingWhatItNeedsIsRefused(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	a := poolAllocator(t, &now)
	for name, o := range map[string]PoolOrder{
		"no id":           {Source: "s", Shape: "x", Arch: config.ArchX64},
		"no source":       {ID: "o", Shape: "x", Arch: config.ArchX64},
		"no shape":        {ID: "o", Source: "s", Arch: config.ArchX64},
		"no architecture": {ID: "o", Source: "s", Shape: "x"},
		"unknown arch":    {ID: "o", Source: "s", Shape: "x", Arch: "sparc"},
	} {
		if err := a.RecordPoolOrder(t.Context(), o); err == nil {
			t.Errorf("%s: recorded", name)
		}
	}
	if err := a.RecordPoolOrder(t.Context(), poolOrder("o-1")); err != nil {
		t.Fatalf("record: %v", err)
	}
	if err := a.PoolOrderPlaced(t.Context(), "o-1", ""); err == nil {
		t.Error("an order was placed without a host")
	}
	if err := a.PoolOrderUncertain(t.Context(), "o-1", "lost"); err != nil {
		t.Fatalf("uncertain: %v", err)
	}
	if err := a.PoolOrderAbandoned(t.Context(), "o-1", ""); err == nil || errors.Is(err, ErrPoolOrderMoved) {
		t.Errorf("an uncertain order was abandoned without a reason, or refused for another: %v", err)
	}
	if orders, err := a.PoolOrders(t.Context()); err != nil || orders[0].State != PoolUncertain {
		t.Errorf("after a refused abandon: %+v, %v", orders, err)
	}
}
