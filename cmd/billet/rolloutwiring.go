package main

import (
	"context"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/nodeapi"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/rollout"
)

// ledgerFleet is what the rollout coordinator knows about the hosts.
//
// THE LEDGER, NOT THE PLANE'S MEMORY. A host's release and negotiated wire are
// recorded at registration and survive a control-plane restart; the plane's
// in-memory view does not. A successor that had to wait for every host to
// reconnect before it could resume a rollout would stall for as long as the
// quietest node's poll interval, on exactly the restart the rollout caused.
type ledgerFleet struct{ alloc *alloc.Allocator }

func (f ledgerFleet) Hosts(ctx context.Context) ([]rollout.Host, error) {
	fleet, err := f.alloc.NodeWireVersions(ctx)
	if err != nil {
		return nil, err
	}

	out := make([]rollout.Host, 0, len(fleet))
	for i := range fleet {
		out = append(out, rollout.Host{
			Name:    fleet[i].Name,
			Release: fleet[i].Release,
			Digest:  fleet[i].Digest,
			Wire:    fleet[i].Negotiated,
			Live:    fleet[i].Live,
			Epoch:   fleet[i].Epoch,
		})
	}

	return out, nil
}

// planeDispatcher tells one node to replace its own billet, over the node wire.
type planeDispatcher struct{ runner *nodeplane.Runner }

func (d planeDispatcher) Upgrade(ctx context.Context, node, version, manifestSHA256,
	rolloutID string, generation int64,
) error {
	return d.runner.Upgrade(ctx, node, nodeapi.UpgradeSpec{
		Version:        version,
		ManifestSHA256: manifestSHA256,
		RolloutID:      rolloutID,
		Generation:     generation,
	})
}
