package nodeplane

import (
	"context"
	"fmt"

	"github.com/junioryono/billet/internal/nodeapi"
)

// Upgrade tells one node to replace its own billet, and reports whether the
// updater started.
//
// ONE NODE, NOT A BROADCAST, and that is the whole difference from Sweep and
// Tend. Those are whole-fleet housekeeping where doing it everywhere at once is
// the point; an upgrade drains a host and takes its capacity out of the
// deployment, so a rollout does them in a cohort it chose. Telling every node at
// once is the shape that empties a fleet.
//
// IT RETURNS WHEN THE UPDATER HAS STARTED, not when the upgrade is done. The node
// execs a detached transaction that outlives the process this command was
// delivered to — so there is nothing here to wait for, and waiting would hold the
// node's single command slot for the length of a drain.
//
// THE WIRE GATE IS THE CALLER'S, and deliberately not here. Whether a node
// negotiated a version that has this command is recorded on its LEDGER ROW, which
// the plane does not read — the rollout coordinator does, because it is already
// reading every host's release to decide who needs upgrading at all. Checking it
// here would mean a second source for the same fact.
func (r *Runner) Upgrade(ctx context.Context, node string, spec nodeapi.UpgradeSpec) error {
	target := r.plane.liveNode(node)
	if target == nil {
		return fmt.Errorf("%w: %s is not connected, so it cannot be told to upgrade",
			ErrNoNode, node)
	}

	id, err := commandID()
	if err != nil {
		return err
	}

	pend := &pending{
		cmd: nodeapi.Command{
			ID:      id,
			Kind:    nodeapi.CommandUpgrade,
			Upgrade: &spec,
		},
		done: make(chan nodeapi.CommandResult, 1),
	}

	res, err := r.plane.dispatch(ctx, target, pend)
	if err != nil {
		return fmt.Errorf("telling %s to install %s: %w", node, spec.Version, err)
	}

	if !res.OK {
		return fmt.Errorf("%s refused to install %s: %s", node, spec.Version, res.Error)
	}

	return nil
}
