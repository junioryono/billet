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
	lease *alloc.Lease
	err   error
}

func (r placementRegistrar) Lease(context.Context, string) (*alloc.Lease, error) {
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
