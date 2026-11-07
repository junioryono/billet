package main

import (
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// errAcceptanceLive is what `stopped` answers when a service of the run still holds its
// lock.
var errAcceptanceLive = errors.New("a service of this acceptance run still lives")

// cmdAcceptanceStopped answers whether this run's services have ended, from the locks
// they hold for as long as they live: the control plane's on its identity directory and
// the node's on its deployment. It is the proof a caller needs before it gives the host
// what a running node must not hold, such as the teardown's delete of the cache's state
// objects: `acceptance run` may return while the node it started is still draining, past
// its stop's grace, and a lock that run handed down says nothing about that node.
//
// THREE ANSWERS: nil when neither lock is held, errAcceptanceLive when one is, and any
// other error when it could not tell, which is never read as stopped. It probes the
// locks through state.LockHeld, which creates nothing, so asking as another account
// leaves no file a service could not open later.
func cmdAcceptanceStopped(env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet acceptance stopped", env.Stdout)
	workspace := fs.String("workspace", "", "the workspace `billet acceptance up` created")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}
	ws, err := requireAcceptanceWorkspace(*workspace)
	if err != nil {
		return err
	}
	cfg, err := config.Load(ws.ConfigPath)
	if err != nil {
		return fmt.Errorf("could not tell whether this run's services have ended: %w", err)
	}
	if err := acceptanceStopped(cfg, ws.DeploymentID); err != nil {
		return err
	}
	fmt.Fprintf(env.Stdout, "stopped: neither this run's control plane nor its node holds its lock\n")

	return nil
}

func acceptanceStopped(cfg *config.Config, deployment string) error {
	if cfg.Server != nil {
		dir := cfg.Server.IdentityDir
		if dir == "" {
			return errors.New("could not tell whether this run's control plane has ended: its config " +
				"names no identity directory")
		}
		path := state.DirectoryLockPath(dir)
		held, err := state.LockHeld(path)
		switch {
		case err != nil:
			return fmt.Errorf("could not tell whether this run's control plane has ended: %w", err)
		case held:
			return fmt.Errorf("%w: its control plane holds %s", errAcceptanceLive, path)
		}
	}

	if cfg.Node != nil {
		// A NODE ALLOWED TO RUN UNLOCKED proves nothing by a free lock: it starts without
		// one when none could be placed.
		if cfg.Node.AllowUnlockedDeployment {
			return errors.New("could not tell whether this run's node has ended: its config allows it to " +
				"run without the deployment lock (node.allow_unlocked_deployment)")
		}
		if deployment == "" {
			return errors.New("could not tell whether this run's node has ended: the workspace records " +
				"no deployment id, which names the node's lock")
		}
		path, err := state.DeploymentLockPath(cfg.Node.LockDir, deployment)
		if err != nil {
			return fmt.Errorf("could not tell whether this run's node has ended: %w", err)
		}
		held, err := state.LockHeld(path)
		switch {
		case err != nil:
			return fmt.Errorf("could not tell whether this run's node has ended: %w", err)
		case held:
			return fmt.Errorf("%w: its node holds %s", errAcceptanceLive, path)
		}
	}

	return nil
}
