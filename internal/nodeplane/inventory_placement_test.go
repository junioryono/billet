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
	lease  *alloc.Lease
	err    error
	lookup func(context.Context, string) (*alloc.Lease, error)
}

func (r placementRegistrar) Lease(ctx context.Context, id string) (*alloc.Lease, error) {
	if r.lookup != nil {
		return r.lookup(ctx, id)
	}
	return r.lease, r.err
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

	for _, replace := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "superseded"}[replace], func(t *testing.T) {
			t.Parallel()

			entered, proceed := make(chan struct{}), make(chan struct{})
			p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
				WithRegistrar(placementRegistrar{lookup: func(ctx context.Context, _ string) (*alloc.Lease, error) {
					close(entered)
					select {
					case <-proceed:
						return &alloc.Lease{ID: "l1", Node: "n1"}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}}))
			_, err := p.Register(t.Context(), nodeapi.RegisterRequest{
				Version: nodeapi.Version, Node: "n1", Incarnation: "first",
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
				_, reconcileErr := p.ReconcileInventory(ctx, "n1", "first", []string{"l1"})
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
			if replace {
				p.nodes["n1"].incarnation = "replacement"
				p.nodes["n1"].ledgerEpoch++
			}
			p.mu.Unlock()
			close(proceed)
			err = <-done
			_, owned := p.OwnerOfLease("l1")
			if replace {
				if !errors.Is(err, ErrSuperseded) || owned {
					t.Fatalf("superseded inventory err=%v owned=%v", err, owned)
				}
			} else if err != nil || !owned {
				t.Fatalf("current inventory err=%v owned=%v", err, owned)
			}
		})
	}
}
