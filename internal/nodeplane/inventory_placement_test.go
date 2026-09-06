package nodeplane

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeapi"
)

type placementRegistrar struct {
	countingRegistrar
	lease   *alloc.Lease
	err     error
	lookup  func(context.Context, string) (*alloc.Lease, error)
	resolve func(context.Context, string, []string, int64) (int, error)
}

func (r placementRegistrar) Lease(ctx context.Context, id string) (*alloc.Lease, error) {
	if r.lookup != nil {
		return r.lookup(ctx, id)
	}
	return r.lease, r.err
}

func (r placementRegistrar) ResolveQuarantineFor(ctx context.Context, node string, ids []string, epoch int64) (int, error) {
	if r.resolve != nil {
		return r.resolve(ctx, node, ids, epoch)
	}
	return 0, nil
}

func TestInProcessRegistrationChecksInventoryPlacement(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		lease *alloc.Lease
		err   error
		want  bool
	}{
		{name: "own", lease: &alloc.Lease{ID: "l1", Node: "n1"}, want: true},
		{name: "foreign", lease: &alloc.Lease{ID: "l1", Node: "n2"}},
		{name: "unreadable", err: errors.New("ledger unavailable")},
		{name: "ended", err: alloc.ErrLeaseNotFound, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
				WithRegistrar(placementRegistrar{lease: tc.lease, err: tc.err}))
			_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
				Version: nodeapi.Version, Node: "n1", Incarnation: "first",
				Provider: config.ProviderDocker, Deployment: deployment,
				VCPU: 8, Memory: 32 * config.GiB, InventoryKnown: true, Instances: []string{"l1"},
			})
			_, owned := p.OwnerOfLease("l1")
			if (err == nil) != tc.want || owned != tc.want {
				t.Fatalf("inventory err=%v owned=%v, want admitted=%v", err, owned, tc.want)
			}
			if tc.name == "foreign" && !errors.Is(err, ErrRefused) {
				t.Fatalf("foreign inventory did not receive a placement refusal: %v", err)
			}
			if tc.err != nil && !tc.want && !errors.Is(err, tc.err) {
				t.Fatalf("inventory lost its failed ledger read: %v", err)
			}
		})
	}
}

func TestInventoryReadsEachDistinctPlacementOnlyOnce(t *testing.T) {
	t.Parallel()

	calls := 0
	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithRegistrar(placementRegistrar{lookup: func(context.Context, string) (*alloc.Lease, error) {
			calls++
			return nil, alloc.ErrLeaseNotFound
		}}))
	ids := make([]string, 1000)
	for i := range ids {
		ids[i] = "orphan"
	}
	_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
		Version: nodeapi.Version, Node: "n1", Incarnation: "first",
		Provider: config.ProviderDocker, Deployment: deployment,
		VCPU: 8, Memory: 32 * config.GiB, InventoryKnown: true, Instances: ids,
	})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("duplicate inventory caused %d ledger reads, want one", calls)
	}
}

func TestInventoryPlacementReadsLeaveThePlaneAvailableAndRecheckIncarnation(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name        string
		incarnation string
		replace     bool
	}{
		{name: "current", incarnation: "first"},
		{name: "superseded", incarnation: "first", replace: true},
		{name: "legacy-current"},
		{name: "legacy-superseded", replace: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			entered, proceed := make(chan struct{}), make(chan struct{})
			resolutions := 0
			p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
				WithRegistrar(placementRegistrar{lookup: func(ctx context.Context, _ string) (*alloc.Lease, error) {
					close(entered)
					select {
					case <-proceed:
						return &alloc.Lease{ID: "l1", Node: "n1"}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}, resolve: func(context.Context, string, []string, int64) (int, error) {
					resolutions++
					return 0, nil
				}}))
			_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
				Version: nodeapi.Version, Node: "n1", Incarnation: tc.incarnation,
				Provider: config.ProviderDocker, Deployment: deployment,
				VCPU: 8, Memory: 32 * config.GiB,
			})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, reconcileErr := p.ReconcileInventory(ctx, "n1", tc.incarnation, []string{"l1"})
				done <- reconcileErr
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				cancel()
				<-done
				t.Fatal("reconciliation never reached the placement read")
			}
			if !p.mu.TryLock() {
				close(proceed)
				<-done
				t.Fatal("a blocked placement read holds the shared plane mutex")
			}
			if tc.replace {
				p.nodes["n1"].incarnation = "replacement"
				p.nodes["n1"].ledgerEpoch++
			}
			p.mu.Unlock()
			close(proceed)
			err = <-done
			_, owned := p.OwnerOfLease("l1")
			if tc.replace {
				if !errors.Is(err, ErrSuperseded) || owned || resolutions != 0 {
					t.Fatalf("superseded inventory err=%v owned=%v resolutions=%d", err, owned, resolutions)
				}
			} else if err != nil || owned != (tc.incarnation != "") || resolutions != 1 {
				t.Fatalf("current inventory err=%v owned=%v resolutions=%d", err, owned, resolutions)
			}
		})
	}
}
