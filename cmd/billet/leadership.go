package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/state"
)

// serverEnded answers what `billet server` returns for how its control plane
// ended: nil for the stop it was asked for, wherever in its life that landed,
// and the error otherwise.
//
// A STOP IS NOT A FAILURE, AND EXITING 1 FOR ONE COSTS A RETIREMENT. Until
// 2026-09-19 a standby stopped while it waited for the claim exited 1: systemd
// held `ActiveState=failed` for a unit that did exactly what it was asked,
// `Restart=on-failure` hid it from an operator stopping one by hand, and
// `billet server retire`, whose AdmitQuietActivation requires a stopped server
// to be `active` or `inactive`, refused at intent with `operation-reactivation:
// billet-server.service ActiveState="failed"`. Measured on a packaged
// active-passive pair under systemd 255 by scripts/retirement-rehearsal.sh (run
// 35411568410). That fix answered for the claim alone, and every step after it,
// the scheduler's own startup included, had the same shape (#137); the answer is
// now asked once, of whatever the run returned.
//
// WHAT IS A STOP IS state.StoppedBy: the process was asked to stop, and every
// cause at the bottom of the error is that context ending. So a fault joined to
// the stop is a fault (the claim gives its exclusion back through errors.Join,
// and a connection that would not close arrives attached to a cancellation), a
// deadline under a stop is some inner budget running out, and a schema the run
// migrated before it gave up is a state.SchemaMoved joined beside the stop,
// which is no stop either: the ledger moved one way and the run did not finish
// what that was for, so it exits non-zero and says from which schema to which
// (#148). A process nobody asked to stop is never stopped, whatever its error
// wraps.
//
// AND A FENCED HOST EXITS NON-ZERO WHATEVER ELSE IS TRUE. A successor has the
// deployment; a clean exit would leave a healed partition with no controller at
// all, while Restart=on-failure starts this one again to take it back or be
// refused naming the holder.
//
// WHAT A STOP DURING STARTUP LEAVES IN SYSTEMD IS MEASURED, not assumed. A
// Type=notify unit stopped while still `activating`, whose process exits 0
// before it ever sent READY=1, ends `inactive (dead)` with Result=success; exit
// 1 ends `failed`. And a process that exits 0 on its own before READY=1 fails
// its start job, so Restart=on-failure starts it again: an exit 0 before
// readiness cannot leave a deployment silently down. Measured under systemd 255
// (255.4-1ubuntu8.18) on 2026-10-16.
func serverEnded(ctx context.Context, env cli.Env, fenced bool, err error) error {
	switch {
	case fenced && errors.Is(err, state.ErrLeadershipLost):
		return err
	case fenced && err == nil:
		return fmt.Errorf("%w: a successor claimed the deployment while this process was "+
			"stopping; restarting is how this host takes it back, or is refused naming the holder",
			state.ErrLeadershipLost)
	case fenced:
		return fmt.Errorf("%w: a successor claimed the deployment while this process held it, "+
			"and restarting is how this host takes it back, or is refused naming the holder: %w",
			state.ErrLeadershipLost, err)
	case err == nil:
	case state.StoppedBy(ctx, err):
		slog.Default().Info("stopped before this control plane was serving", "detail", err.Error())
	default:
		return err
	}

	fmt.Fprintln(env.Stdout, "billet server: stopped")

	return nil
}
