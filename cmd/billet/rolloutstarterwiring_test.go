package main

import (
	"go/ast"
	"testing"
)

// THE CONTROL PLANE IS GIVEN THE STARTER, NOT ONLY THE COORDINATOR.
//
// `release.automatic` is true by default, and the whole of what makes that true
// is one option passed in runServer. The starter works in its own tests against
// a fake resolver; what nothing there can see is the control plane being
// assembled without it — which is a deployment that documents automatic updates
// and never starts one, and every surface reads healthy.
func TestTheControlPlaneIsWiredWithTheRolloutStarter(t *testing.T) {
	fn := findFunc(t, "runServer")

	var starter, coordinator, built bool

	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}

		switch calleeName(call) {
		case "WithRolloutStarter":
			starter = true
		case "WithRolloutCoordinator":
			coordinator = true
		case "newRolloutStarter":
			built = true
		}

		return true
	})

	if !coordinator {
		t.Fatal("runServer no longer passes WithRolloutCoordinator; this test's premise moved")
	}

	if !built || !starter {
		t.Fatalf("runServer builds the starter (%v) and passes WithRolloutStarter (%v); both "+
			"must be true or the channel advances and nothing ever starts a rollout",
			built, starter)
	}
}
