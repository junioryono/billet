package host

import (
	"slices"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/nodeending"
)

// recordKilled records the stopped invocation in the node's ending record and
// leaves it running, which is what a node killed at its stop timeout leaves.
func recordKilled(t *testing.T, stateDir string) {
	t.Helper()

	if _, err := nodeending.Begin(stateDir, nodeending.Entry{InvocationID: nodeInvocation, PID: inspectPID,
		Role: nodeending.RoleNode}, nil); err != nil {
		t.Fatal(err)
	}
}

// THE MIGRATION'S STOP IS PROVED BY THE STOPPED INVOCATION'S OWN ENDING (#127):
// systemd's clean triple, which reset-failed also leaves after a kill, is not
// enough when the node's record says the process never wrote its ending; the
// node is left stopped, not started on the new endpoint, and the answer names
// the acknowledgement.
func TestTheMigrationsStopIsProvedByTheNodesEnding(t *testing.T) {
	f := newEndpointFixture(t)
	f.installB(t)
	f.afterStop(t, "inactive", "dead", "success")
	f.afterStart(t)
	f.recordAfterStart(t, canonicalB)
	recordKilled(t, f.nodeState)

	o := f.migrate(t, f.rendering(endpointB))
	if o.str("outcome") != outcomeUnknown || o.str("reason") != endpointReasonUnproved ||
		o.str("state") != stateStopped || !strings.Contains(o.str("why"), "killed or crashed") ||
		!strings.Contains(o.str("next"), "acknowledge --invocation "+nodeInvocation) {
		t.Fatalf("outcome %q reason %q state %q (why %q next %q), want unknown/unproved, left stopped, naming "+
			"the acknowledgement", o.str("outcome"), o.str("reason"), o.str("state"), o.str("why"), o.str("next"))
	}

	for _, c := range f.systemctlCalls(t) {
		if strings.HasPrefix(c, "start") {
			t.Fatalf("the node was started after an unproved stop: %q", f.systemctlCalls(t))
		}
	}
}

// THE REPAIR'S STOP IS PROVED THE SAME WAY: a node killed at its stop is not
// restarted on the repaired unit, and the unit is not reloaded.
func TestTheRepairsStopIsProvedByTheNodesEnding(t *testing.T) {
	f := newRepairFixture(t)
	recordKilled(t, f.nodeState)

	o := f.repair(t)
	if o.str("outcome") != outcomeUnknown || o.str("state") != stateStopped ||
		!strings.Contains(o.str("why"), "killed or crashed") {
		t.Fatalf("outcome %q state %q (why %q), want unknown, left stopped", o.str("outcome"), o.str("state"),
			o.str("why"))
	}

	if got := f.acted(t); !slices.Equal(got, []string{"stop"}) {
		t.Errorf("the repair did %q after an unproved stop, want the stop alone", got)
	}
}
