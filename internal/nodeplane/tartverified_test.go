package nodeplane

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/nodeapi"
)

// A PLANE'S MACOS LIMIT REACHES EACH NODE BY NAME from the version that carries it, and an
// answer below it is the bytes it always was: an older node ignores the field, and a
// newer node at that version records that its control plane does not say.
func TestARegistrationCarriesItsNodesMacOSLimitFromItsVersion(t *testing.T) {
	t.Parallel()

	limits := map[string]int{"n1": 5}
	limit := func(node string) int { return limits[node] }

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute, WithMacOSLimits(limit))

	res := p.wireResponse(39, "n1")
	if res.MacOSVMLimit == nil || *res.MacOSVMLimit != 5 {
		t.Fatalf("at wire 39 the answer carries %v, want 5", res.MacOSVMLimit)
	}

	// A ZERO IS AN ANSWER, which the pointer keeps from reading as silence.
	res = p.wireResponse(39, "n2")
	if res.MacOSVMLimit == nil || *res.MacOSVMLimit != 0 {
		t.Fatalf("for a node of no macOS guests the answer carries %v, want 0", res.MacOSVMLimit)
	}

	// LITERAL, so moving the constant cannot move the boundary a fleet already runs.
	raw, err := json.Marshal(p.wireResponse(38, "n1"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(raw), "macos_vm_limit") {
		t.Fatalf("at wire 38 the answer names the field: %s", raw)
	}
}

// verifiedTartTier is the plane's test tier booting a verified Tart image.
func verifiedTartTier() config.Tier {
	tier := testTier()
	tier.Provider, tier.Providers = "", []config.ProviderKind{config.ProviderTart}
	tier.Image = "ghcr.io/acme/macos:26@verified"

	return tier
}

func verifiedPlane(t *testing.T, tier config.Tier, req nodeapi.RegisterRequest) *Plane {
	t.Helper()

	req.Provider = config.ProviderTart

	p := New(slog.New(slog.DiscardHandler), deployment, time.Minute,
		WithTierCatalog([]config.Tier{tier}), WithRegistrar(&recordingRegistrar{}),
		WithCommandTimeout(5*time.Second))
	if _, err := p.Register(t.Context(), req); err != nil {
		t.Fatalf("register: %v", err)
	}

	return p
}

func nodeAtWire(wire int, incarnation string) nodeapi.RegisterRequest {
	req := releasedNode()
	req.Version, req.MinVersion = wire, nodeapi.MinVersion
	req.Incarnation = incarnation

	return req
}

// AN OLDER NODE IS NEVER SENT A REFERENCE IT CANNOT READ: it hands `@verified` to `tart
// fqn` as a name and fails the launch.
func TestAVerifiedTartTierIsNotLaunchedOnAnOlderNode(t *testing.T) {
	t.Parallel()

	p := verifiedPlane(t, verifiedTartTier(), nodeAtWire(38, "n1-1"))

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	err := p.NewRunner().Launch(ctx, tartLease(), dispatch.Job{RequestID: 7})
	if !errors.Is(err, ErrNoNode) || !strings.Contains(err.Error(), "verified Tart image") {
		t.Fatalf("launch = %v, want ErrNoNode naming the verified image", err)
	}

	if n := p.QueuedForTest("n1"); n != 0 {
		t.Errorf("%d commands were queued for a node the launch was refused", n)
	}
}

// tartLease is the test lease on a Tart node.
func tartLease() *alloc.Lease {
	lease := testLease()
	lease.Providers = []config.ProviderKind{config.ProviderTart}

	return lease
}

// A TIER NAMING NOTHING OF THE KIND STILL RUNS ON AN OLDER NODE.
func TestAnUnverifiedTartTierStillRunsOnAnOlderNode(t *testing.T) {
	t.Parallel()

	tier := verifiedTartTier()
	tier.Image = "ghcr.io/acme/macos:26"

	p := verifiedPlane(t, tier, nodeAtWire(38, "n1-1"))

	ctx, cancel := context.WithCancel(t.Context())
	launched := make(chan error, 1)

	go func() {
		launched <- p.NewRunner().Launch(ctx, tartLease(), dispatch.Job{RequestID: 7})
	}()

	waitForQueued(t, p, "n1")
	cancel()

	if err := <-launched; !errors.Is(err, context.Canceled) {
		t.Fatalf("launch = %v, want it to have been waiting on the node", err)
	}
}

// AND A PROCESS THAT RE-REGISTERS ON AN OLDER WIRE AFTER DISPATCH DOES NOT TAKE IT.
func TestAProcessThatCannotReadAVerifiedLaunchQueuedBeforeItDoesNotTakeIt(t *testing.T) {
	t.Parallel()

	p := verifiedPlane(t, verifiedTartTier(), nodeAtWire(nodeapi.Version, "n1-1"))

	launched := make(chan error, 1)
	go func() {
		launched <- p.NewRunner().Launch(t.Context(), tartLease(), dispatch.Job{RequestID: 7})
	}()

	waitForQueued(t, p, "n1")

	older := nodeAtWire(38, "n1-2")
	older.Provider = config.ProviderTart

	if _, err := p.Register(t.Context(), older); err != nil {
		t.Fatalf("re-register: %v", err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()

	cmd, took, err := p.Poll(ctx, "n1", "n1-2")
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("poll: %v", err)
	}

	if took {
		t.Fatalf("the re-registered process took %s", cmd.Kind)
	}

	if err := <-launched; err == nil || !strings.Contains(err.Error(), "now speaks wire") {
		t.Fatalf("launch = %v, want the delivery refusal", err)
	}
}

// THE SAME SPELLING ON ANOTHER BACKEND is a generation, which every wire reads.
func TestVerifiedOnAnotherBackendIsNotHeldByThisWire(t *testing.T) {
	t.Parallel()

	tier := config.Tier{Image: "ubuntu-2404-x64@verified"}

	if tartTierNamesVerified(tier, config.ProviderFirecracker) {
		t.Fatal("a Firecracker generation was held to Tart's wire")
	}

	if !tartTierNamesVerified(tier, config.ProviderTart) {
		t.Fatal("a Tart tier naming it was not")
	}
}
