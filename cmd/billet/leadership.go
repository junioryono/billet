package main

import (
	"context"
	"errors"
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
func stoppedBeforeTheClaim(ctx context.Context, fenced bool, err error) error {
	if err == nil || fenced || ctx.Err() == nil || !errors.Is(err, context.Canceled) {
		return err
	}

	slog.Default().Info("stopped while taking this deployment's controller claim; "+
		"the deployment is as this host found it", "detail", err.Error())
	fmt.Println("billet server: stopped")

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
