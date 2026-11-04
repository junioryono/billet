package setup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/hostauthority"
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
	} else if fingerprint, err = tokenbroker.CreateClientKey(path); err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "key          %s\ndeployment   %s\nfingerprint  %s\n\nCompare both with what the "+
		"broker's operator registers, on a channel you trust:\n\ndeployments:\n  - id: %s\n    clients: [%q]\n"+
		"    installations:\n", path, deployment, fingerprint, deployment, fingerprint)

	for _, target := range cfg.GitHubTargets() {
		fmt.Fprintf(env.Stdout, "      - {app_id: %d, installation_id: %d, target: %q", target.AppID,
			target.InstallationID, target.Path())

		if cfg.TargetNeedsRunEvidence(target.Name) {
			fmt.Fprint(env.Stdout, ", actions_read: true")
		}

		fmt.Fprintln(env.Stdout, "}")
	}

	return nil
}
