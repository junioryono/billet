package nodeclient

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/junioryono/billet/internal/nodeapi"
)

// WHICH REPORTS ARE SENT AGAIN: a destroy's that was not heard, and nothing
// else. A verdict about this node is the same answer the next time, and every
// other command's report is either custody (a launch) or one the plane does
// not wait on.
func TestOnlyADestroysUnheardReportIsSentAgain(t *testing.T) {
	t.Parallel()

	unheard := fmt.Errorf("nodeclient: POST /v1/nodes/n1/result: %w", context.DeadlineExceeded)
	destroy := nodeapi.Command{Kind: nodeapi.CommandDestroy}

	if !retriesReport(destroy, unheard) {
		t.Error("a destroy's report the plane did not answer is not sent again")
	}

	for _, verdict := range []error{ErrUnregistered, ErrRefused, ErrSuperseded, ErrUnauthenticated} {
		if retriesReport(destroy, fmt.Errorf("%w: from the plane", verdict)) {
			t.Errorf("a destroy's report answered %v is sent again, to hear the same verdict", verdict)
		}
	}

	for _, kind := range []nodeapi.CommandKind{
		nodeapi.CommandLaunch, nodeapi.CommandSweep, nodeapi.CommandTend,
		nodeapi.CommandUpgrade, nodeapi.CommandInventory,
	} {
		if retriesReport(nodeapi.Command{Kind: kind}, unheard) {
			t.Errorf("a %s report is sent again; only a destroy's is", kind)
		}
	}

	if retriesReport(destroy, errors.Join(unheard, ErrRefused)) {
		t.Error("a verdict joined to a transport error is sent again")
	}
}
