package server

import (
	"errors"
	"testing"

	"github.com/junioryono/billet/internal/dispatch"
)

// THE OLD NAMES ARE THE NEW VALUES, NOT COPIES OF THEM. The node returns
// dispatch.ErrCustody and the plane tests for server.ErrCustody, so a sentinel
// declared afresh here would make errors.Is answer no across the two, and a
// lease the runner holds would be released. The type aliases need no test: a
// re-declared type would stop the node's runner satisfying server.Runner, which
// the compiler reports.
func TestTheDispatchSentinelsAreTheSameValues(t *testing.T) {
	t.Parallel()

	for name, pair := range map[string][2]error{
		"ErrCustody":           {ErrCustody, dispatch.ErrCustody},
		"ErrHolderUnavailable": {ErrHolderUnavailable, dispatch.ErrHolderUnavailable},
	} {
		if pair[0] != pair[1] || !errors.Is(pair[0], pair[1]) { //nolint:errorlint // identity is the property under test
			t.Errorf("server.%s is not dispatch.%s itself", name, name)
		}
	}
}
