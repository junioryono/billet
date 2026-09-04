package main

import (
	"context"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/wiring"
)

// resolveAppKey reads the GitHub App private key, from wherever this deployment
// keeps it, through the one resolver wiring registers for the container.
func resolveAppKey(ctx context.Context, cfg *config.Config) (github.AppKey, error) {
	return wiring.ReadAppKey(ctx, cfg, awscreds.Default())
}

// appKeyLocation is what a diagnostic calls the place the key lives.
//
// FOR AN OPERATOR'S BENEFIT AND NOTHING ELSE. `billet check` reports which of the
// two a deployment is using, because "the App key is fine" is a different fact
// depending on where it was read from and an operator debugging a failover needs
// to know which one they are looking at.
func appKeyLocation(cfg *config.Config) string {
	if cfg.Server.IdentityBackendKind() != config.IdentitySSM {
		return cfg.GitHub.PrivateKeyPath
	}

	return "AWS Parameter Store " + wiring.AppKeyPath(cfg.Server.IdentitySSM())
}
