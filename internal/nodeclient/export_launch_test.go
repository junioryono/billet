package nodeclient

import (
	"context"
	"time"

	"github.com/junioryono/billet/internal/nodeapi"
)

// ExecutePolledForTest runs one command across the shutdown boundary with the
// poll's context and the launch context given apart, which a real serve only
// separates in the moment a stop arrives.
func ExecutePolledForTest(ctx, launchCtx context.Context, compute Compute, cmd nodeapi.Command) nodeapi.CommandResult {
	return executePolled(ctx, launchCtx, nil, compute, cmd, false, time.Second, nodeapi.Version)
}
