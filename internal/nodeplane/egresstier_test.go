package nodeplane

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
)

// listedTierForPlane is the arch test tier with an allowlist, and no architecture.
func listedTierForPlane() config.Tier {
	tier := archTierForPlane()
	tier.Arch = ""
	tier.Network = &config.TierNetwork{Egress: &config.TierEgress{Allow: []string{"example.com"}}}

	return tier
}

func listedPlane(t *testing.T, req nodeapi.RegisterRequest) *Plane {
	t.Helper()

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithTierCatalog([]config.Tier{listedTierForPlane()}), WithRegistrar(&recordingRegistrar{}),
		WithCommandTimeout(5*time.Second))
	if _, err := p.Register(t.Context(), req); err != nil {
		t.Fatalf("register: %v", err)
	}

	return p
}

// A TIER WITH A LIST IS NOT SENT TO A NODE THAT WOULD IGNORE IT: an older node drops the
// field and runs the job open, which is the one outcome the list exists to prevent.
func TestAListedTierIsNotLaunchedOnAnOlderNode(t *testing.T) {
	t.Parallel()

	req := archNode("n1-1")
	req.Version = 37

	p := listedPlane(t, req)

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := p.NewRunner().Launch(ctx, testLease(), dispatch.Job{RequestID: 7})
	if !errors.Is(err, ErrNoNode) || !strings.Contains(err.Error(), "egress allowlist") ||
		!strings.Contains(err.Error(), "wire 37") {
		t.Fatalf("launch = %v, want ErrNoNode naming the allowlist and the wire", err)
	}

	if n := p.QueuedForTest("n1"); n != 0 {
		t.Errorf("%d commands were queued for a node the launch was refused", n)
	}
}

func TestACurrentNodeReceivesTheTiersList(t *testing.T) {
	t.Parallel()

	p := listedPlane(t, archNode("n1-1"))

	launched := make(chan error, 1)

	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7})
	}()

	cmd, took, err := p.Poll(t.Context(), "n1", "n1-1")
	if err != nil || !took || cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("poll = %+v, %v, %v", cmd, took, err)
	}

	if cmd.Tier == nil || cmd.Tier.Egress == nil || !slices.Equal(cmd.Tier.Egress.Allow, []string{"example.com"}) {
		t.Fatalf("launched tier = %+v, want its list", cmd.Tier)
	}

	if err := p.Result("n1", "n1-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("result: %v", err)
	}

	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}
}

// AN OPEN TIER GOES TO A NODE OF ANY VERSION, as it always did.
func TestAnOpenTierIsStillLaunchedOnAnOlderNode(t *testing.T) {
	t.Parallel()

	tier := listedTierForPlane()
	tier.Network = nil

	req := archNode("n1-1")
	req.Version = 37

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithTierCatalog([]config.Tier{tier}), WithRegistrar(&recordingRegistrar{}),
		WithCommandTimeout(5*time.Second))
	if _, err := p.Register(t.Context(), req); err != nil {
		t.Fatalf("register: %v", err)
	}

	launched := make(chan error, 1)

	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7})
	}()

	cmd, took, err := p.Poll(t.Context(), "n1", "n1-1")
	if err != nil || !took || cmd.Tier == nil || cmd.Tier.Egress != nil {
		t.Fatalf("poll = %+v, %v, %v", cmd, took, err)
	}

	if err := p.Result("n1", "n1-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("result: %v", err)
	}

	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}
}
