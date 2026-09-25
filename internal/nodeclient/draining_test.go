package nodeclient

import (
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
)

// A LAUNCH A DRAINING NODE REFUSES SAYS SO IN THE FIELD THE PLANE READS, and
// only on a wire that has the field: the plane decodes a result strictly, so an
// older one would refuse the whole report over it.
func TestADrainingRefusalIsFlaggedOnlyWhereTheWireHasTheField(t *testing.T) {
	t.Parallel()

	cmd := nodeapi.Command{ID: "c1", Kind: nodeapi.CommandLaunch}

	current := execute(t.Context(), nil, cmd, true, nodeapi.VersionNodeDraining)
	if current.OK || current.Custody || !current.Draining {
		t.Errorf("a draining refusal on wire %d = %+v, want failed, no custody, draining",
			nodeapi.VersionNodeDraining, current)
	}

	older := execute(t.Context(), nil, cmd, true, nodeapi.VersionNodeDraining-1)
	if older.OK || older.Draining || older.Error == "" {
		t.Errorf("a draining refusal on wire %d = %+v, want a failure without the field",
			nodeapi.VersionNodeDraining-1, older)
	}
}
