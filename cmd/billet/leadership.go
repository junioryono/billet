package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/junioryono/billet/internal/cli"
)

// stoppedBeforeTheClaim answers what this process should return when taking the
// controller claim ended in an error: the error itself, or nil because the
// process was asked to stop while it was trying.
//
// A STANDBY IS THE ONE CONTROL PLANE THAT CAN BE STOPPED WHILE IT IS STILL
// TRYING, and until 2026-09-19 stopping one exited 1: AwaitController returns
// its context's cancellation, runServer returned it, and systemd was left
// holding `ActiveState=failed` for a unit that did exactly what it was asked.
// The packaged unit is Restart=on-failure, so a stop by hand looked harmless —
// the process came back and stood by again — and the cost only appears where
// something reads the unit's state afterwards. `billet server retire` does:
// AdmitQuietActivation requires the stopped server to be `active` or
// `inactive`, a failed unit is neither, and a retained-node retirement of a
// standby therefore refused at intent with `operation-reactivation:
// billet-server.service ActiveState="failed"`. Measured on a packaged
// active-passive pair under systemd 255 by scripts/retirement-rehearsal.sh
// (2026-09-19, run 35411568410). An active controller stopped after startup
// already exits 0 through plane.Run's own cancellation handling, so this is the
// standby being brought into line with it rather than a new leniency.
//
// WHAT IS LEFT OWNED IS THE TEST, NOT WHAT WAS TOUCHED. A cancellation arriving
// inside ClaimController lands after the exclusion was taken and possibly during
// the promotion migration, so "nothing authoritative has happened yet" would be
// false; what holds is that the claim's own restore gives the exclusion and the
// standby latch back and the migration is one transaction, so a process that
// stops here leaves the deployment as it found it. Below the claim that is no
// longer true, which is why a fenced controller exits non-zero: there a clean
// exit would leave a healed deployment with no controller at all. FENCED IS
// THEREFORE ASKED HERE TOO, because the claim's own write can be refused by a
// successor.
//
// EVERY CONDITION EARNS ITS PLACE. A cancellation is not by itself a shutdown:
// an inner context cancelled while this one is live is a failure whose error
// happens to carry context.Canceled, and reading that as "asked to stop" would
// exit 0 on a real fault. It deliberately does NOT admit context.DeadlineExceeded
// the way internal/server's onlyCancellation does, because the promotion
// migration derives its own startup timeout and a timeout there is a fault.
//
// WHAT IS MEASURED IS THE STANDBY, and the claim is kept to it. A standby has
// sent READY=1 before it waits, so systemd records its clean exit as a stop; a
// first controller cancelled inside ClaimController has not, and what a
// Type=notify unit records for a process that exits 0 without ever sending it is
// not something billet has measured. Exiting 0 is no worse there, and the window
// is a few milliseconds wide. The same is true of every other error runServer
// can return between the claim and plane.Run — see #137, which is that defect
// and is not this one.
//
// AND WHAT IT DISCARDS IT SAYS. The error can carry a second fault joined to the
// cancellation — the claim releases the exclusion through errors.Join, so a
// connection that would not close arrives here attached to a cancellation — and
// an exit status that drops it would be the only record of it.
func stoppedBeforeTheClaim(ctx context.Context, env cli.Env, fenced bool, err error) error {
	if err == nil || fenced || ctx.Err() == nil || !errors.Is(err, context.Canceled) {
		return err
	}

	slog.Default().Info("stopped while taking this deployment's controller claim; "+
		"the deployment is as this host found it", "detail", err.Error())
	fmt.Fprintln(env.Stdout, "billet server: stopped")

	return nil
}
