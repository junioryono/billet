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

// archNode is a registration of n1 by process incarnation, at the current wire,
// reporting archs.
func archNode(incarnation string, archs ...config.Arch) nodeapi.RegisterRequest {
	req := releasedNode()
	req.Version, req.MinVersion = nodeapi.Version, nodeapi.MinVersion
	req.Incarnation = incarnation
	req.Architectures = archs

	return req
}

// archPlane is a plane whose catalogue is the arm64 test tier, with n1 registered
// as req.
func archPlane(t *testing.T, req nodeapi.RegisterRequest) *Plane {
	t.Helper()

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithTierCatalog([]config.Tier{archTierForPlane()}), WithRegistrar(&recordingRegistrar{}),
		WithCommandTimeout(5*time.Second))
	if _, err := p.Register(t.Context(), req); err != nil {
		t.Fatalf("register: %v", err)
	}

	return p
}

// A TIER NAMING AN ARCHITECTURE IS NOT SENT TO A NODE THAT CANNOT BE HELD TO IT: an
// older node ignores the launch's arch and launches on whatever it has.
func TestAnArchTierIsNotLaunchedOnAnOlderNode(t *testing.T) {
	t.Parallel()

	p := archPlane(t, releasedNode())

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := p.NewRunner().Launch(ctx, testLease(), dispatch.Job{RequestID: 7})
	if !errors.Is(err, ErrNoNode) || !strings.Contains(err.Error(), "cannot be held to") {
		t.Fatalf("launch = %v, want ErrNoNode naming the architecture", err)
	}

	if n := p.QueuedForTest("n1"); n != 0 {
		t.Errorf("%d commands were queued for a node the launch was refused", n)
	}
}

// NOR TO A CURRENT NODE THAT NOW REPORTS ANOTHER: placement chose it from the ledger
// at escrow, and a host that re-registered as another machine since, through a
// report of none (a rollback) that the ledger lets pass, is held here.
func TestAnArchTierIsNotLaunchedOnANodeNowReportingAnother(t *testing.T) {
	t.Parallel()

	p := archPlane(t, archNode("n1-1", config.ArchX64))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := p.NewRunner().Launch(ctx, testLease(), dispatch.Job{RequestID: 7})
	if !errors.Is(err, ErrNoNode) || !strings.Contains(err.Error(), "now reports launching [x64]") {
		t.Fatalf("launch = %v, want ErrNoNode naming what the node reports", err)
	}

	if n := p.QueuedForTest("n1"); n != 0 {
		t.Errorf("%d commands were queued for a node the launch was refused", n)
	}
}

// AND A PROCESS THAT RE-REGISTERS AFTER DISPATCH, ON AN OLDER WIRE OR REPORTING
// ANOTHER ARCHITECTURE, DOES NOT TAKE IT.
func TestAProcessThatCannotRunAnArchLaunchQueuedBeforeItDoesNotTakeIt(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		then nodeapi.RegisterRequest
		want string
	}{
		{name: "an older wire", then: func() nodeapi.RegisterRequest {
			req := releasedNode()
			req.Incarnation = "n1-2"

			return req
		}(), want: "now speaks wire"},
		{name: "another architecture", then: archNode("n1-2", config.ArchX64), want: "now reports launching [x64]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			p := archPlane(t, archNode("n1-1", config.ArchARM64))

			launched := make(chan error, 1)
			go func() {
				launched <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7})
			}()
			waitForQueued(t, p, "n1")

			if _, err := p.Register(t.Context(), tc.then); err != nil {
				t.Fatalf("re-register: %v", err)
			}

			// THE POLL IS WHAT DELIVERY REFUSES IN, and with nothing left to give it
			// long-polls, so its own deadline ends it.
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()

			cmd, took, err := p.Poll(ctx, "n1", "n1-2")
			if err != nil && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("poll: %v", err)
			}

			if took {
				t.Fatalf("the re-registered process took %s", cmd.Kind)
			}

			err = <-launched
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("launch = %v, want the delivery refusal naming %q", err, tc.want)
			}
		})
	}
}

// AND A NODE THAT CAN IS GIVEN THE TIER'S ARCHITECTURE with the launch, which its
// provider holds the launch to.
func TestACurrentNodeReceivesTheTiersArchitecture(t *testing.T) {
	t.Parallel()

	p := archPlane(t, archNode("n1-1", config.ArchARM64))

	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), testLease(), dispatch.Job{RequestID: 7})
	}()

	cmd, took, err := p.Poll(t.Context(), "n1", "n1-1")
	if err != nil || !took || cmd.Kind != nodeapi.CommandLaunch {
		t.Fatalf("poll = %+v, %v, %v", cmd, took, err)
	}

	if cmd.Tier == nil || cmd.Tier.Arch != config.ArchARM64 {
		t.Fatalf("launched tier = %+v, want arch arm64", cmd.Tier)
	}

	if err := p.Result("n1", "n1-1", nodeapi.CommandResult{ID: cmd.ID, OK: true}); err != nil {
		t.Fatalf("result: %v", err)
	}

	if err := <-launched; err != nil {
		t.Fatalf("launch: %v", err)
	}
}
