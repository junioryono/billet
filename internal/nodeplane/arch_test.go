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

// A REGISTRATION'S ARCHITECTURES REACH THE LEDGER FROM THE WIRE THAT CARRIES THEM,
// and below it the node is recorded as reporting none, which places it only for
// tiers that name none (ADR-022).
func TestArchitecturesAreRecordedOnlyFromTheWireThatCarriesThem(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		wire int
		want []config.Arch
	}{
		{wire: nodeapi.VersionArchitectures, want: []config.Arch{config.ArchARM64}},
		{wire: nodeapi.VersionArchitectures - 1, want: nil},
	} {
		reg := &recordingRegistrar{}
		p := testPlane(t, WithRegistrar(reg))

		req := registerAt("")
		req.Version, req.MinVersion = tc.wire, tc.wire
		req.Architectures = []config.Arch{config.ArchARM64}

		if _, err := p.Register(t.Context(), req); err != nil {
			t.Fatalf("register at wire %d: %v", tc.wire, err)
		}

		if got := reg.recorded().Architectures; reg.callCount() != 1 || !slices.Equal(got, tc.want) {
			t.Errorf("at wire %d the ledger was given architectures %v (%d calls), want %v",
				tc.wire, got, reg.callCount(), tc.want)
		}
	}
}

// AND A REPORT PLACEMENT COULD BE MISLED BY IS REFUSED PERMANENTLY, before the
// ledger, whose errors the node would retry as an outage forever.
func TestAnArchitectureReportPlacementCouldBeMisledByIsRefusedBeforeTheLedger(t *testing.T) {
	t.Parallel()

	for _, archs := range [][]config.Arch{
		{config.ArchX64, config.ArchARM64},
		{"aarch64"},
	} {
		reg := &recordingRegistrar{}
		p := testPlane(t, WithRegistrar(reg))

		req := registerAt("")
		req.Architectures = archs

		_, err := p.Register(t.Context(), req)
		if !errors.Is(err, ErrRefused) {
			t.Errorf("a %s node reporting %v was answered %v, want ErrRefused", req.Provider, archs, err)
		}

		if reg.callCount() != 0 {
			t.Errorf("a refused report reached the ledger: %+v", reg.recorded())
		}
	}
}

// archTierForPlane is the plane's test tier, naming arm64.
func archTierForPlane() config.Tier {
	tier := testTier()
	tier.Arch = config.ArchARM64

	return tier
}

// A TIER NAMING AN ARCHITECTURE IS NOT SENT TO A NODE THAT CANNOT BE HELD TO IT: an
// older node ignores the launch's arch and launches on whatever it has.
func TestAnArchTierIsNotLaunchedOnAnOlderNode(t *testing.T) {
	t.Parallel()

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithTierCatalog([]config.Tier{archTierForPlane()}), WithRegistrar(&recordingRegistrar{}))
	if _, err := p.Register(t.Context(), releasedNode()); err != nil {
		t.Fatalf("register: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := p.NewRunner().Launch(ctx, testLease(), dispatch.Job{RequestID: 7})
	if !errors.Is(err, ErrNoNode) || !strings.Contains(err.Error(), "cannot be held to") {
		t.Fatalf("launch = %v, want ErrNoNode naming the architecture", err)
	}
}

// AND A PROCESS THAT RE-REGISTERS ON AN OLDER WIRE AFTER DISPATCH DOES NOT TAKE IT.
func TestAnOlderProcessCannotTakeAnArchLaunchQueuedForANewerOne(t *testing.T) {
	t.Parallel()

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithTierCatalog([]config.Tier{archTierForPlane()}), WithRegistrar(&recordingRegistrar{}),
		WithCommandTimeout(5*time.Second))
	current := releasedNode()
	current.Version, current.MinVersion = nodeapi.Version, nodeapi.MinVersion
	current.Incarnation = "new"

	if _, err := p.Register(t.Context(), current); err != nil {
		t.Fatalf("register the current process: %v", err)
	}

	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7})
	}()
	waitForQueued(t, p, "n1")

	older := releasedNode()
	older.Incarnation = "old"

	if _, err := p.Register(t.Context(), older); err != nil {
		t.Fatalf("register the older process: %v", err)
	}

	if cmd, took, err := p.Poll(t.Context(), "n1", "old"); err == nil && took {
		t.Fatalf("the older process took %s", cmd.Kind)
	}

	if err := <-launched; err == nil {
		t.Fatal("the launch reported success though nothing took it")
	}
}

// AND A NODE THAT CAN IS GIVEN THE TIER'S ARCHITECTURE with the launch, which its
// provider holds the launch to.
func TestACurrentNodeReceivesTheTiersArchitecture(t *testing.T) {
	t.Parallel()

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithTierCatalog([]config.Tier{archTierForPlane()}), WithCommandTimeout(5*time.Second))
	register(t, p, "n1", config.ProviderDocker)

	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7})
	}()

	cmd, took, err := p.Poll(t.Context(), "n1", "")
	if err != nil || !took || cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("poll = %+v, %v, %v", cmd, took, err)
	}

	if cmd.Tier == nil || cmd.Tier.Arch != config.ArchARM64 {
		t.Fatalf("launched tier = %+v, want arch arm64", cmd.Tier)
	}

	if err := p.Result("n1", "", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("result: %v", err)
	}

	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}
}
