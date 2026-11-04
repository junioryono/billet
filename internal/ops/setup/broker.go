package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostauthority"
	"github.com/junioryono/billet/internal/lifeops"
	"github.com/junioryono/billet/internal/retirement"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/tokenbroker"
)

// Broker is `billet broker`: the process that holds App keys for deployments that
// hold none, and the key a control-plane host proves its deployment to it with.
func Broker(ctx context.Context, env cli.Env, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: billet broker serve --config broker.yaml | billet broker client-key [--config PATH] [--show] [--new-deployment]")
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
	newDeployment := fs.Bool("new-deployment", false,
		"this host has no deployment identity and is starting a new deployment, not restoring one: mint it")
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

	path := cfg.Server.TokenBroker.ClientKeyPath

	// AN OBSERVER CREATES NOTHING: no identity, no lock directory, no key.
	if *show {
		deployment, found, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%s holds no deployment identity, so this key proves nothing yet", cfg.Server.IdentityDir)
		}

		key, err := tokenbroker.ReadClientKey(path)
		if err != nil {
			return err
		}

		fingerprint, err := tokenbroker.Fingerprint(key)
		if err != nil {
			return err
		}

		printBrokerStanza(env, cfg, path, deployment, fingerprint)

		return nil
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

	// MINTED ONLY WHEN ASKED. An identity minted here on a replacement host stops the
	// restore that was about to bring the real one back, and names a deployment that
	// does not exist in the stanza this prints.
	if _, found, err := state.PeekDeploymentID(cfg.Server.IdentityDir); err != nil {
		return err
	} else if !found && !*newDeployment {
		return fmt.Errorf("%s holds no deployment identity. If this host is restoring a deployment, run "+
			"`billet local restore` first and this after it. If this is a new deployment, run this again with "+
			"--new-deployment", cfg.Server.IdentityDir)
	}

	// BEFORE THE KEY EXISTS: root on a host with no recorded service account has nobody to
	// hand a mode 0600 key to, and printing a stanza for it would look like success.
	if os.Geteuid() == 0 && acc.Account() == nil {
		return errors.New("this host records no service account, so a key made by root could only be read by " +
			"root. Run this as the account that runs the control plane, or prepare the host first " +
			"(`billet local prepare`)")
	}

	deployment, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return err
	}

	fingerprint, err := tokenbroker.CreateClientKey(path)
	if err != nil {
		return err
	}

	if err := giveToService(path, acc.Account()); err != nil {
		return err
	}

	printBrokerStanza(env, cfg, path, deployment, fingerprint)

	return nil
}

// printBrokerStanza prints what this host's key is, and what to register at the broker.
func printBrokerStanza(env cli.Env, cfg *config.Config, path, deployment, fingerprint string) {
	fmt.Fprintf(env.Stdout, "key          %s\ndeployment   %s\nfingerprint  %s\n\nCompare both with what the "+
		"broker's operator registers, on a channel you trust:\n\ndeployments:\n  - id: %s\n    clients: [%q]\n"+
		"    installations:\n", path, deployment, fingerprint, deployment, fingerprint)

	for _, target := range cfg.GitHubTargets() {
		// actions_read ALWAYS, AS `github-app create` DEFAULTS IT: every deployment reads
		// GitHub's job records for its queue times, and a publishing tier needs them.
		fmt.Fprintf(env.Stdout, "      - {app_id: %d, installation_id: %d, target: %q, actions_read: true}\n",
			target.AppID, target.InstallationID, target.Path())
	}
}

// giveToService hands a key a privileged run just made to the account the service runs
// as: it is the service that reads it, and a mode 0600 file root owns would stop the
// control plane at its first token. Through lifeops' repair, the one re-owning mechanism,
// which follows no symlink and refuses a file with a second name. Nothing happens for
// anyone but root.
func giveToService(path string, account *retirement.ServiceAccount) error {
	if os.Geteuid() != 0 || account == nil {
		return nil
	}

	if _, err := lifeops.NewConverger(lifeops.NewInspector()).RepairPaths(filepath.Dir(path),
		[]lifeops.RepairTarget{{Name: filepath.Base(path)}}, account.UID, account.GID); err != nil {
		return fmt.Errorf("give %s to the service account: %w", path, err)
	}

	return nil
}
