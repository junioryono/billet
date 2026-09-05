package rollout

import (
	"log/slog"
	"strings"
	"testing"
)

// A CONTROL PLANE THAT NAMES ITSELF IN THE BARE FORM IS ON THE TARGET. Releases
// through v0.9.1 report "0.4.0" while the rollout recorded "v0.4.0", and a string
// inequality read every such control plane as never on its target, so no node
// was ever told to move and the rollout waited forever for a controller that was
// already there.
func TestAControlPlaneReportingTheBareFormIsOnTheTarget(t *testing.T) {
	_, s := open(t)

	fleet := &fakeFleet{}
	fleet.set("epyc-1", "v0.3.26", 14, true)

	dispatch := &fakeDispatcher{}

	r, err := s.Start(t.Context(), StartRequest{
		TargetVersion: targetVersion, TargetDigest: targetDigest,
		PriorVersion: "v0.3.26", Policy: DefaultPolicy(), CreatedBy: "ops", Nodes: []string{"epyc-1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	c := NewCoordinator(s, fleet, dispatch, strings.TrimPrefix(targetVersion, "v"), 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, c) // the controller observes itself on the target
	tick(t, c) // and tells the node

	if len(dispatch.told) != 1 {
		t.Fatalf("a control plane reporting %q was not read as on the target %s; it told %v",
			strings.TrimPrefix(targetVersion, "v"), targetVersion, dispatch.told)
	}

	// AND A HOST THAT COMES BACK IN THE BARE FORM HAS CONVERGED.
	fleet.set("epyc-1", strings.TrimPrefix(targetVersion, "v"), 14, true)

	tick(t, c)

	if got := phaseOf(t, s, r); got != PhaseCommitted {
		t.Fatalf("a host reporting the bare %q is %s, want committed",
			strings.TrimPrefix(targetVersion, "v"), got)
	}
}
