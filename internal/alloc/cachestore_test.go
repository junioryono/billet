package alloc

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
)

func aStoreReport(state CacheStoreState) CacheStoreReport {
	return CacheStoreReport{Store: "ceph", State: state, Since: time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)}
}

func TestAStoreReportIsKeptUnderTheRegistrationInForce(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)
	ctx := t.Context()
	register := func() {
		t.Helper()
		if _, err := a.RegisterNode(ctx, NodeRegistration{
			Name: "n1", Provider: config.ProviderDocker, VCPU: 2, Memory: 8 * config.GiB,
		}); err != nil {
			t.Fatalf("RegisterNode: %v", err)
		}
	}

	// A HOST THIS DEPLOYMENT DOES NOT KNOW writes nothing, and says so.
	if err := a.RecordCacheStoreState(ctx, "stranger", "", aStoreReport(CacheStoreClosed)); !errors.Is(err, ErrCacheStoreStale) {
		t.Fatalf("a report for no node: %v", err)
	}

	register()

	// NOR DOES A PROCESS THE LEDGER DOES NOT HAVE REGISTERED: its word would be read as
	// the registered one's.
	if err := a.RecordCacheStoreState(ctx, "n1", "superseded", aStoreReport(CacheStoreOpen)); !errors.Is(err, ErrCacheStoreStale) {
		t.Fatalf("a report from another process: %v", err)
	}
	if rows, err := a.NodeCacheStores(ctx); err != nil || len(rows) != 0 {
		t.Fatalf("after a fenced report: %+v, %v", rows, err)
	}
	stores, err := a.NodeCacheStores(ctx)
	if err != nil {
		t.Fatalf("NodeCacheStores: %v", err)
	}
	if len(stores) != 0 {
		t.Fatalf("a host that said nothing reads as %+v", stores)
	}

	open := aStoreReport(CacheStoreOpen)
	open.Failures, open.Reason = 3, CacheStoreTimeout
	if err := a.RecordCacheStoreState(ctx, "n1", "", open); err != nil {
		t.Fatalf("RecordCacheStoreState: %v", err)
	}
	stores, err = a.NodeCacheStores(ctx)
	if err != nil {
		t.Fatalf("NodeCacheStores: %v", err)
	}
	if len(stores) != 1 {
		t.Fatalf("stores = %+v", stores)
	}
	got := stores[0]
	if got.Node != "n1" || !got.Live || got.Store != "ceph" || got.State != CacheStoreOpen ||
		got.Failures != 3 || got.Reason != CacheStoreTimeout || !got.Since.Equal(open.Since) ||
		got.ReceivedAt.IsZero() {
		t.Fatalf("store = %+v", got)
	}

	// A LATER WORD REPLACES IT.
	if err := a.RecordCacheStoreState(ctx, "n1", "", aStoreReport(CacheStoreClosed)); err != nil {
		t.Fatalf("RecordCacheStoreState: %v", err)
	}
	stores, err = a.NodeCacheStores(ctx)
	if err != nil {
		t.Fatalf("NodeCacheStores: %v", err)
	}
	if len(stores) != 1 || stores[0].State != CacheStoreClosed || stores[0].Failures != 0 || stores[0].Reason != "" {
		t.Fatalf("after a later word: %+v", stores)
	}

	// A HOST THAT RECONNECTED HAS NOT SPOKEN YET: its old word is no longer its current one.
	if err := a.RecordCacheStoreState(ctx, "n1", "", open); err != nil {
		t.Fatalf("RecordCacheStoreState: %v", err)
	}
	register()
	stores, err = a.NodeCacheStores(ctx)
	if err != nil {
		t.Fatalf("NodeCacheStores: %v", err)
	}
	if len(stores) != 0 {
		t.Fatalf("a report from before the host reconnected still reads: %+v", stores)
	}

	// AND THE NEXT IT SAYS IS.
	if err := a.RecordCacheStoreState(ctx, "n1", "", open); err != nil {
		t.Fatalf("RecordCacheStoreState after a registration: %v", err)
	}
	stores, err = a.NodeCacheStores(ctx)
	if err != nil || len(stores) != 1 || stores[0].State != CacheStoreOpen {
		t.Fatalf("after a registration and a word: %+v, %v", stores, err)
	}
}

func TestAStoreReportBilletWouldNotWriteIsRefused(t *testing.T) {
	t.Parallel()

	a := newAllocator(t, Limits{MaxVCPU: 8, MaxMemory: 32 * config.GiB}, nil)
	for name, report := range map[string]CacheStoreReport{
		"an unknown store":  {Store: "zfs", State: CacheStoreClosed, Since: time.Now()},
		"an unknown state":  {Store: "ceph", State: "half", Since: time.Now()},
		"an unknown reason": {Store: "ceph", State: CacheStoreOpen, Since: time.Now(), Reason: "bad luck"},
		"no time":           {Store: "ceph", State: CacheStoreClosed},
		"negative failures": {Store: "ceph", State: CacheStoreOpen, Since: time.Now(), Failures: -1},
	} {
		if err := a.RecordCacheStoreState(t.Context(), "n1", "", report); err == nil ||
			strings.Contains(err.Error(), "no such node") {
			t.Errorf("%s: %v", name, err)
		}
	}
}
