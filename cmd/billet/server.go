package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/ops/fleetops"
	"github.com/junioryono/billet/internal/ops/host"
	"github.com/junioryono/billet/internal/state"
)

// runServer starts the control plane and blocks until it is told to stop.
//
// EVERY WAY IT ENDS IS ANSWERED IN ONE PLACE, serverEnded, because a stop can
// land in any step and each step reports it in its own words: answered step by
// step, the next step added would be the one that exits 1 for a stop (#137).
// THE PROBE IS THE EXCEPTION: `--upgrade-probe` exits with its verdict on the
// candidate, so a probe stopped before it proved anything never reads as one
// that passed.
func runServer(
	ctx context.Context, env cli.Env,
	lc *cli.Lifecycle,
	cfg *config.Config,
	dryRun, upgradeProbe, holdProbeFlag bool,
) error {
	fenced, err := runControlPlane(ctx, env, lc, cfg, controlPlaneRun{
		host:      serverHost(env),
		targets:   app.NewScaleSetClients,
		dryRun:    dryRun,
		probe:     upgradeProbe,
		holdProbe: holdProbeFlag,
	})
	if upgradeProbe {
		return err
	}

	// ctx, NOT THE CONTEXT runControlPlane STOPPED ITS LOOPS WITH: that one is
	// cancelled on every path out by its own deferred stop, and by the fence. This
	// is the process's, which only a signal cancels.
	return serverEnded(ctx, env, fenced, err)
}

// controlPlaneRun is how runControlPlane was invoked: what the process and the
// machine give the control plane, how its scale-set clients are built, and the
// command line's flags.
type controlPlaneRun struct {
	host    app.Host
	targets func(context.Context, *config.Config) ([]app.Target, error)

	dryRun, probe, holdProbe bool
}

// runControlPlane opens the control plane and serves it until ctx ends, and
// reports whether a successor fenced this process out of its deployment at any
// point before the ledger closed.
func runControlPlane(
	ctx context.Context, env cli.Env, lc *cli.Lifecycle, cfg *config.Config, run controlPlaneRun,
) (bool, error) {
	// Built by the SHARED constructor, one client per target, so the server and
	// teardown authenticate identically. Two near-identical constructions is how
	// one of them ends up pointed at a different owner than the other.
	targets, err := run.targets(ctx, cfg)
	if err != nil {
		return false, err
	}

	cp, err := app.OpenControlPlane(ctx, cfg, run.host, targets, app.ControlPlaneOptions{Probe: run.probe})
	if err != nil {
		return false, err
	}

	defer cp.Close()

	err = serveControlPlane(ctx, env, lc, cfg, run, cp)

	// THE FENCE IS READ ONCE EVERYTHING ELSE HAS STOPPED: serveControlPlane has
	// stopped the wire and joined the controller's loops by the time it returns,
	// and the ledger closes only after this. A write refused during startup or
	// teardown has latched it by then, and the claim's own write, which a
	// successor can refuse, is read the same way.
	return cp.LeadershipLost(), err
}

// serveControlPlane is the control plane cp after its open: the probe's hold,
// or the claim and the scheduler until ctx ends.
//
// THE ORDER IS THE TYPES': internal/app's control plane hands out a Controller
// only from BecomeController, and the node wire only to a controller that has
// forgotten the fleet and adopted the authority, so a step moved above the claim
// does not compile. What stays here is how the process was invoked: the
// probe's hold and the signals.
func serveControlPlane(
	ctx context.Context, env cli.Env, lc *cli.Lifecycle, cfg *config.Config, run controlPlaneRun,
	cp *app.ControlPlane,
) error {
	if run.probe {
		if err := host.NotifyReady(env); err != nil {
			return fmt.Errorf("server upgrade-probe readiness: %w", err)
		}

		host.HoldProbe(ctx, env, run.holdProbe, host.ServerProbeReadyLine)

		return nil
	}

	// AFTER THE PROBE, which runs beside the service that holds the port, and
	// BEFORE THE CLAIM, so a standby can be scraped while it waits.
	served, err := app.ServeMetrics(ctx, "server", cfg.Server.Metrics, cp.LedgerMetrics())
	if err != nil {
		return err
	}

	defer served.Close(ctx)

	// Ctrl-C and SIGTERM stop the listeners through the context, which is what
	// releases escrowed capacity — see the listener's deferred release. A hard
	// kill skips that and leaves the reaper to expire it.
	//
	// AND IT IS INSTALLED BEFORE THE CLAIM, because a standby may wait there for
	// days and an operator stopping one must not have to kill it.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	// THE CONTROLLER CLAIM, BEFORE ANYTHING POLLS GITHUB OR DISPATCHES A NODE.
	ctl, err := cp.BecomeController(ctx, stop)
	if err != nil {
		return err
	}

	// THE LOOPS ARE JOINED BEFORE THE LEDGER CLOSES: this runs ahead of cp.Close.
	defer ctl.Close()

	adopted, err := ctl.AdoptAuthority(ctx)
	if err != nil {
		return err
	}

	fleet, err := ctl.ForgetFleet(ctx)
	if err != nil {
		return err
	}

	wire, err := ctl.ServeWire(ctx, fleet, adopted)
	if err != nil {
		return err
	}

	defer wire.Stop()

	published, err := ctl.PublishAuthority(ctx, wire)
	if err != nil {
		return err
	}

	scheduler, err := ctl.Schedule(wire, published, app.ScheduleOptions{Hurry: lc.Hurry(), DryRun: run.dryRun})
	if err != nil {
		return err
	}

	err = scheduler.Run(ctx)

	// A FENCED CONTROLLER'S ERROR IS ITS OWN, never a GitHub access problem to
	// explain: Run already says what happened and what a restart does. ASKED OF
	// THE ERROR, not of the ledger again: Run read the fence once, after the
	// plane stopped, and a second read here could see a fence latched since and
	// still return the nil Run answered before it.
	if err == nil || errors.Is(err, state.ErrLeadershipLost) {
		return err
	}

	return host.ExplainGitHubAccess(ctx, cfg, err)
}

// serverHost is what the control plane takes from this process and machine:
// the identity exclusions the host authority provides, the service manager's
// notifications, and stdout for the lines an operator reads.
func serverHost(env cli.Env) app.Host {
	return app.Host{
		ServerAccess:  hostauthority.ServerWireAccess,
		AuthorityLock: fleetops.AuthorityLockAccess,
		Ready:         func() error { return host.NotifyReady(env) },
		Status:        func(text string) error { return host.NotifyStatus(env, text) },
		Out:           env.Stdout,
	}
}
