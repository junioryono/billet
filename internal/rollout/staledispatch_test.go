package rollout

import (
	"log/slog"
	"testing"
)

// A CONTROL PLANE THAT HAS MOVED PAST A ROLLOUT'S TARGET TELLS NO NODE TO MOVE,
// WHATEVER THE ROLLOUT REMEMBERS ABOUT ITS CONTROLLER. The controller phase says
// a control plane converged on the target once; the coordinator dispatching nodes
// runs in the binary the host runs now. Moved forward by hand and restarted, the
// old coordinator would, in its first tick, send every pending node to a release
// the control plane had left, before the starter's first pass superseded the
// rollout. Here the second coordinator is that restarted control plane.
func TestARestartedControlPlaneAheadOfTheTargetDispatchesNothing(t *testing.T) {
	s, fleet, dispatch, c, r := coordinated(t, "epyc-1")

	tick(t, c) // the controller, on the target, records its phase as converged

	if !mustOpen(t, s).ControllerPhase.Converged() {
		t.Fatal("the controller phase was not recorded as converged")
	}

	// THE SAME LEDGER, A NEWER CONTROL PLANE: the host was moved past the target
	// and the coordinator started again.
	ahead := NewCoordinator(s, fleet, dispatch, newerVersion, 14,
		WithCoordinatorLogger(slog.New(slog.DiscardHandler)))

	tick(t, ahead)

	if len(dispatch.told) != 0 {
		t.Fatalf("a control plane on %s told %v to move to %s, the target it has left",
			newerVersion, dispatch.told, r.TargetVersion)
	}

	// AND THE ORIGINAL, STILL ON THE TARGET, DOES TELL IT: the gate is about the
	// binary asking, not about the rollout.
	tick(t, c)

	if len(dispatch.told) != 1 {
		t.Fatalf("the control plane on the target told %v; want the one node", dispatch.told)
	}
}

func mustOpen(t *testing.T, s *Store) *Rollout {
	t.Helper()

	r, err := s.Open(t.Context())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return r
}
