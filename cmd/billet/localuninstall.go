package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/lifeops/launchd"
)

// uninstallOptions is what runLocalUninstall acts on.
type uninstallOptions struct {
	configPath string
	reason     string
	timeout    time.Duration
	dryRun     bool
	force      bool
}

// cmdLocalUninstall takes this machine's billet services away, and leaves the
// deployment's data where it is.
//
// IT IS `down` PLUS FORGETTING, in that order, and the order is the design. The
// services are drained and stopped by the same path `billet local down` uses —
// admission sealed, the quiescence barrier waited on, the node stopped before
// the server — and only then is anything removed. Uninstalling first would be
// `rm` with extra steps, and removing a running node's definition leaves a
// process with nothing that describes it.
//
// WHAT IT WILL NOT REMOVE is the point of it. The config, the App private key,
// the deployment identity, the node-wire CA and the ledger are what make this
// deployment ITSELF rather than a fresh one — GitHub issues an App key exactly
// once, and a new CA drops every node in the fleet. Uninstall removes binaries
// and services and preserves operator data by default; this names every path
// it left, so an operator who does want it gone can act on a list rather than
// on a memory.
func cmdLocalUninstall(ctx context.Context, args []string) error {
	fs := newFlagSet("billet local uninstall")
	cfgPath := addServiceConfigFlag(fs)
	reason := fs.String("reason", "",
		"why this host is being uninstalled, recorded on the seal for whoever finds it sealed")
	timeout := fs.Duration("timeout", 0,
		"give up waiting for running work after this long (default: wait for as long as the "+
			"jobs take)")
	dryRun := fs.Bool("dry-run", false,
		"report what would be removed and change nothing")
	force := fs.Bool("force", false,
		"stop services running a different billet build than this one")

	if err := parse(fs, args); err != nil {
		return err
	}

	if *timeout < 0 {
		return fmt.Errorf("--timeout %s is negative; use a positive duration, or omit it to "+
			"wait for as long as the jobs take", *timeout)
	}

	return runLocalUninstall(ctx, uninstallOptions{
		configPath: *cfgPath, reason: *reason, timeout: *timeout,
		dryRun: *dryRun, force: *force,
	})
}

func runLocalUninstall(ctx context.Context, o uninstallOptions) error {
	if hostOS != "darwin" {
		return fmt.Errorf("billet local uninstall removes the launch agents billet installs on "+
			"macOS, and this host is %s. On Linux the services come from the package: remove it "+
			"with your package manager, which leaves /var/lib/billet alone", hostOS)
	}

	cfg, err := config.Load(o.configPath)
	if err != nil {
		return err
	}

	if o.dryRun {
		printUninstallPlan(cfg, o)

		fmt.Println("\nNothing was changed (--dry-run).")

		return nil
	}

	// THE WHOLE UNINSTALL IS ONE EXCLUSION, taken here rather than inside the
	// `down` it performs. `down` releases the lock when it returns, so removing
	// the agents afterwards would happen unlocked — and a concurrent `up` in that
	// window takes the lock, bootstraps the node, and then has its plist removed
	// and its override cleared out from under a live process.
	lock, err := lifecycleLock()
	if err != nil {
		return err
	}

	defer func() {
		if err := lock.release(); err != nil {
			fmt.Printf("warn     could not release the lifecycle lock: %v\n", err)
		}
	}()

	// THE SERVICES GO DOWN FIRST, through the same drain `down` performs. This
	// is not a convenience: removing the definition of a running node leaves a
	// process holding guests with nothing on the machine that describes it, and
	// stopping the server before the node has settled tears down leases whose
	// compute is still running.
	fmt.Println("Taking this host down before removing anything.")
	fmt.Println()

	if err := runLocalDown(ctx, downOptions{
		configPath: o.configPath, reason: o.reason, timeout: o.timeout, force: o.force,
		locked: true,
	}); err != nil {
		return fmt.Errorf("this host was NOT uninstalled, because it could not be taken down "+
			"first: %w", err)
	}

	fmt.Println()

	return removeAgents(ctx, cfg, o)
}

// removeAgents takes away the launch agents and leaves everything else.
func removeAgents(ctx context.Context, cfg *config.Config, o uninstallOptions) error {
	c := launchd.New()
	server, node := c.Services()
	upgrade, images := c.Scheduled()

	// THE SCHEDULED AGENTS FIRST, AND BOOTED OUT BEFORE THEY ARE REMOVED. A
	// oneshot that fires between the services going down and its plist going is
	// an upgrade transaction starting on a host being uninstalled; booting it
	// out waits for one that is already running, which is the right answer for
	// a transaction midway through replacing the binary.
	for _, label := range []string{upgrade, images} {
		if _, err := c.StopAndProve(ctx, label); err != nil {
			return fmt.Errorf("this host is down and PARTLY uninstalled: %w", err)
		}
	}

	// THE NODE FIRST, matching the order everything else here uses.
	for _, s := range []struct {
		label string
		want  bool
	}{{upgrade, true}, {images, true}, {node, cfg.Node != nil}, {server, cfg.Server != nil}} {
		if !s.want {
			continue
		}

		if err := c.Uninstall(ctx, s.label); err != nil {
			return fmt.Errorf("this host is down and PARTLY uninstalled: %w", err)
		}

		fmt.Printf("remove   %s is gone, and its disabled override with it\n", s.label)
	}

	printPreserved(cfg, o.configPath)

	return nil
}

// printPreserved names every path uninstall deliberately left.
//
// NAMED RATHER THAN SUMMARISED. "Your data is preserved" is a sentence an
// operator cannot act on: the point of this list is that somebody who DOES want
// the deployment gone can delete it deliberately, and somebody who does not can
// see that the irreplaceable parts are still there. The App key is the sharpest
// of them — GitHub issues it exactly once and will not reissue it.
func printPreserved(cfg *config.Config, cfgPath string) {
	fmt.Println()
	fmt.Println("Left alone, because this is what makes it THIS deployment rather than a new one:")
	fmt.Printf("         config    %s\n", cfgPath)

	for _, keyPath := range appKeyFilePaths(cfg) {
		fmt.Printf("         app key   %s  (GitHub issues this ONCE and will not reissue it)\n",
			keyPath)
	}

	if cfg.Server != nil && cfg.Server.IdentityDir != "" {
		fmt.Printf("         ledger    %s  (also the deployment identity and the node-wire CA)\n",
			cfg.Server.IdentityDir)
	}

	if cfg.Node != nil && cfg.Node.StateDir != "" {
		fmt.Printf("         node      %s\n", cfg.Node.StateDir)
	}

	// THE BINARY NEEDS ROOT, WHICH THIS COMMAND HAS REFUSED. A launch agent
	// lives in a logged-in user's domain, so uninstall runs as that user and
	// cannot remove something from /usr/local/bin. Naming the command is honest;
	// asking for a password to finish a job that is already done is not.
	if self, err := os.Executable(); err == nil {
		fmt.Println()
		fmt.Printf("The binary is still installed. It needs root, which this command does not "+
			"take:\n\n  sudo rm %s\n", self)
	}
}

// printUninstallPlan reports what would happen.
func printUninstallPlan(cfg *config.Config, o uninstallOptions) {
	fmt.Printf("plan     what `billet local uninstall` would do on this host:\n")
	fmt.Printf("         1. take it down (seal admission, wait for running work, stop node then " +
		"server)\n")

	if o.timeout > 0 {
		fmt.Printf("            giving up on the wait after %s\n", o.timeout)
	}

	fmt.Printf("         2. remove each launch agent, and clear its disabled override\n")
	fmt.Printf("         3. leave the config, App key, ledger, identity and CA where they are\n")

	printPreserved(cfg, o.configPath)
}
