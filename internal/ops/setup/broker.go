package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"syscall"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/regularfile"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/tokenbroker"
)

// Broker is `billet broker`: the process that holds App keys for deployments that
// hold none, and the key a control-plane host proves its deployment to it with.
func Broker(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet broker serve --config broker.yaml | billet broker client-key [--config PATH] [--show]")
	}

	switch args[0] {
	case "serve":
		return brokerServe(ctx, env, args[1:])
	case "client-key":
		return brokerClientKey(ctx, env, args[1:])
	}

	return fmt.Errorf("unknown broker command %q; try serve or client-key", args[0])
}

func brokerServe(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet broker serve", env.Stdout)
	path := fs.String("config", "", "the broker's own config file, never a billet.yaml")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	if *path == "" {
		return errors.New("billet broker serve: --config names the broker's file, which holds its App keys' " +
			"paths and who may ask for what")
	}

	cfg, err := config.LoadBroker(*path)
	if err != nil {
		return err
	}

	return app.ServeBroker(ctx, cfg, slog.Default())
}

// brokerClientKey makes this host's key, or shows it, and prints what the broker's
// registry needs: two fingerprints compared by a person, as enrollment does it.
func brokerClientKey(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet broker client-key", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	show := fs.Bool("show", false, "print the existing key's fingerprint rather than making one")
	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil || cfg.Server.TokenBroker == nil {
		return fmt.Errorf("%s has no server.token_broker; this host has no broker to prove itself to", *cfgPath)
	}

	// UNDER THE IDENTITY EXCLUSION, because the identity may be minted here on a host
	// that has not started yet, and the registry entry names it.
	acc, err := hostauthority.Open(ctx, cfg.Server.IdentityDir,
		hostauthority.Intent{Create: true, Wait: hostauthority.Wait})
	if err != nil {
		return err
	}

	defer func() {
		if err := acc.Release(); err != nil {
			fmt.Fprintf(env.Stderr, "billet broker client-key: release the identity exclusion: %v\n", err)
		}
	}()

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return err
	}

	path := cfg.Server.TokenBroker.ClientKeyPath

	var fingerprint string

	if *show {
		key, err := tokenbroker.ReadClientKey(path)
		if err != nil {
			return err
		}

		if fingerprint, err = tokenbroker.Fingerprint(key); err != nil {
			return err
		}
	} else {
		if fingerprint, err = tokenbroker.CreateClientKey(path); err != nil {
			return err
		}

		if err := giveToService(path, cfg.Server.IdentityDir); err != nil {
			return err
		}
	}

	fmt.Fprintf(env.Stdout, "key          %s\ndeployment   %s\nfingerprint  %s\n\nCompare both with what the "+
		"broker's operator registers, on a channel you trust:\n\ndeployments:\n  - id: %s\n    clients: [%q]\n"+
		"    installations:\n", path, deployment, fingerprint, deployment, fingerprint)

	for _, target := range cfg.GitHubTargets() {
		// actions_read ALWAYS, AS `github-app create` DEFAULTS IT: every deployment reads
		// GitHub's job records for its queue times, and a publishing tier needs them.
		fmt.Fprintf(env.Stdout, "      - {app_id: %d, installation_id: %d, target: %q, actions_read: true}\n",
			target.AppID, target.InstallationID, target.Path())
	}

	return nil
}

// giveToService hands a key a privileged run just made to the account the service runs
// as, which owns the identity directory: it is the service that reads it, and a mode 0600
// file root owns would stop the control plane at its first token. Through a descriptor,
// never a pathname. Nothing happens for anyone but root.
func giveToService(path, identityDir string) error {
	if os.Geteuid() != 0 {
		return nil
	}

	var owner syscall.Stat_t
	if err := syscall.Stat(identityDir, &owner); err != nil {
		return fmt.Errorf("read who owns %s: %w", identityDir, err)
	}

	f, _, err := regularfile.Open(path, regularfile.Options{})
	if err != nil {
		return fmt.Errorf("open %s to hand it over: %w", path, err)
	}

	defer func() { _ = f.Close() }()

	if err := f.Chown(int(owner.Uid), int(owner.Gid)); err != nil {
		return fmt.Errorf("give %s to the service account: %w", path, err)
	}

	return nil
}
