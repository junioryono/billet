package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// becomeController takes this deployment's controller claim, waiting for it if
// this host is one of an active/passive pair.
//
// ONE FUNCTION FOR BOTH LAYOUTS, because the difference between them is a single
// question — is a held claim a MISTAKE or the thing this process is here for —
// and everything downstream is identical. A standby is not a second kind of
// control plane; it is the same one, stopped at this line until it can go on.
//
// READY=1 IS SENT BEFORE THE WAIT, AND IT HAS TO BE. The packaged unit is
// Type=notify with TimeoutStartSec=120, so a standby that withheld readiness
// until promotion would be killed at two minutes and restarted forever — the
// same restart-loop argument runServer already settles for a tier waiting on
// GitHub to expire a message session. A waiting standby IS doing its job, and
// the STATUS line is what says which job that is.
//
// IT IS SENT AGAIN AFTER PROMOTION by runServer's ordinary readiness call, which
// is not redundant: sd_notify READY=1 is idempotent, and the second one carries
// the point at which the listeners are actually up.
func becomeController(
	ctx context.Context,
	cfg *config.Config,
	db *state.DB,
	deployment string,
	standby bool,
) error {
	log := slog.Default()

	if !standby {
		claim, err := db.ClaimController(ctx, controllerName(cfg), deployment)
		if err != nil {
			return fmt.Errorf("controller claim: %w", err)
		}

		log.Info("claimed this deployment's controller",
			"holder", claim.Holder, "epoch", claim.Epoch)

		return nil
	}

	if err := notifyReady(); err != nil {
		return fmt.Errorf("server standby readiness: %w", err)
	}

	fmt.Println("billet server: standing by; this host takes over when the controller's " +
		"database session ends")

	// RATE-LIMITED IN THE LOG AND NOT IN THE STATUS. A standby may wait for days,
	// so a line per poll is a journal nobody can read — but `systemctl status`
	// shows only the latest STATUS, so refreshing that costs nothing and is the
	// one place an operator looks.
	var (
		lastLogged time.Time
		waits      int
	)

	claim, err := db.AwaitController(ctx, controllerName(cfg), deployment,
		func(held state.ControllerClaim) {
			waits++

			describe := "the ledger records no holder"
			if held.Holder != "" {
				describe = fmt.Sprintf("%s holds it at epoch %d", held.Holder, held.Epoch)
			}

			//nolint:errcheck // a status line is a diagnostic; failing to send one is not a reason to stop.
			_ = notifyStatus("standby: waiting for the controller claim (" + describe + ")")

			if waits == 1 || time.Since(lastLogged) > standbyLogInterval {
				lastLogged = time.Now()

				log.Info("standing by for this deployment's controller",
					"holder", held.Holder, "epoch", held.Epoch)
			}
		})
	if err != nil {
		return fmt.Errorf("controller claim: %w", err)
	}

	//nolint:errcheck // as above.
	_ = notifyStatus("controller")

	log.Info("promoted to this deployment's controller",
		"holder", claim.Holder, "epoch", claim.Epoch)

	return nil
}

// standbyLogInterval paces what a waiting standby writes to the journal.
//
// A STANDBY MAY WAIT FOR DAYS, which is the ordinary state of a healthy pair, so
// the log has to be quiet enough to read afterwards and frequent enough that
// "this host is standing by" is visible without asking. The systemd STATUS line
// beside it is refreshed on every poll, because that one is a replacement rather
// than an append.
const standbyLogInterval = 5 * time.Minute

// stopWhenReplaced ends the control plane the moment its ledger refuses a write
// because a successor has claimed the deployment.
//
// REFUSING THE WRITE IS NOT STOPPING THE PROCESS, and that gap is the whole
// reason this exists. Every background writer in the control plane is
// deliberately patient with an error it cannot classify — a heartbeat keeps its
// lease rather than dropping it, the reaper logs and tries again, a cleanup
// retry backs off — because the alternative is a database blip failing builds.
// All of that is right for a blip and wrong for a lost claim: it leaves a
// replaced controller polling GitHub, holding its message session, and running
// the cleanup loop that calls Runner.Destroy, which never touches the ledger and
// is therefore fenced by nothing.
//
// A SIGNAL RATHER THAN A CHECK ON SOME PATH, because every path that could do
// the checking is one this process may sit inside for a whole long poll.
//
// IT RETURNS ON THE CONTEXT TOO, so an ordinary shutdown does not leave it
// blocked on a channel that will never close.
func stopWhenReplaced(
	ctx context.Context, replaced <-chan struct{}, stop func(), log *slog.Logger,
) {
	select {
	case <-ctx.Done():
	case <-replaced:
		log.Error("this process is no longer this deployment's controller; stopping. " +
			"Nothing running here is destroyed and no capacity is handed back — the " +
			"controller that replaced this one adopts both")
		stop()
	}
}
