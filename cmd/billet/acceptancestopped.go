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
// THREE ANSWERS: nil when it took and released both locks, errAcceptanceLive when one is
// held, and any other error when it could not tell, which is never read as stopped.
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
		lock, err := state.LockStateDir(dir)
		switch {
		case errors.Is(err, state.ErrLocked):
			return fmt.Errorf("%w: its control plane holds %s", errAcceptanceLive, state.DirectoryLockPath(dir))
		case err != nil:
			return fmt.Errorf("could not tell whether this run's control plane has ended: %w", err)
		}
		if err := lock.Release(); err != nil {
			return fmt.Errorf("could not release the control plane's lock this took: %w", err)
		}
	}

	if cfg.Node != nil {
		if deployment == "" {
			return errors.New("could not tell whether this run's node has ended: the workspace records " +
				"no deployment id, which names the node's lock")
		}
		lock, err := state.LockDeployment(deployment, state.LockOptions{Dir: cfg.Node.LockDir})
		switch {
		case errors.Is(err, state.ErrDeploymentLocked):
			return fmt.Errorf("%w: its node holds the lock on deployment %s", errAcceptanceLive, deployment)
		case err != nil:
			return fmt.Errorf("could not tell whether this run's node has ended: %w", err)
		case lock.Degraded() != "":
			return fmt.Errorf("could not tell whether this run's node has ended: no lock could be "+
				"placed (%s)", lock.Degraded())
		}
		if err := lock.Release(); err != nil {
			return fmt.Errorf("could not release the node's lock this took: %w", err)
		}
	}

	return nil
}
