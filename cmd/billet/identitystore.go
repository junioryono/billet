package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/wireshare"
	"github.com/junioryono/billet/internal/wiring"
)

// authorityStoreFor is the deployment's identity store, or nil when it keeps its
// authority as files.
func authorityStoreFor(cfg *config.Config) wireshare.Store {
	return wiring.AuthorityStoreFor(cfg, awscreds.Default())
}

// adoptSharedAuthority gives this host the authority the deployment already
// uses, before anything reads one.
//
// BEFORE serveNodeWire AND AFTER THE CLAIM, and both are load-bearing. The node
// wire's single authority read goes through LoadOrCreateCA, which CREATES one
// when the directory is empty, so a promoted standby that reached it first would
// mint a rival authority and drop the entire fleet. And a host that adopted
// before holding the claim would be writing identity material on the strength
// of nothing.
func adoptSharedAuthority(
	ctx context.Context, cfg *config.Config, deployment string, log *slog.Logger,
) error {
	return wiring.AdoptAuthority(ctx, authorityStoreFor(cfg), cfg.Server.IdentityDir,
		wiring.Deployment(deployment), log)
}

// publishSharedAuthority puts what this host holds into the store.
//
// AFTER serveNodeWire, because that is what creates an authority on a first
// controller and there is nothing to publish before it.
func publishSharedAuthority(
	ctx context.Context, cfg *config.Config, deployment string, log *slog.Logger,
) {
	wiring.PublishAuthority(ctx, authorityStoreFor(cfg), cfg.Server.IdentityDir,
		wiring.Deployment(deployment), log)
}

// publishRotatedAuthority carries a rotation or a retirement into the store.
//
// A ROTATION NOBODY ELSE HEARS ABOUT IS HALF A ROTATION. On a deployment with a
// second controller the store is how that host learns there is a new authority at
// all, and after a RETIREMENT it is how that host learns the previous pair is
// gone, which it would otherwise keep presenting a certificate from.
//
// REPORTED AND NOT FATAL, because by the time this runs the operation on THIS
// host is complete and irreversible. Returning an error would tell an operator
// their rotation failed when it did not; what they need is the one command that
// finishes the job.
func publishRotatedAuthority(ctx context.Context, cfg *config.Config, deployment string) {
	published, err := wiring.PublishRotatedAuthority(ctx, authorityStoreFor(cfg),
		cfg.Server.IdentityDir, wiring.Deployment(deployment), slog.Default())
	if err == nil {
		if published {
			fmt.Println("Published the new authority to this deployment's identity store.")
		}

		return
	}

	fmt.Fprintf(os.Stderr,
		"\nThis host is done, but publishing to the identity store failed:\n  %v\n\n"+
			"The other controller cannot see this change until it is published. Fix the\n"+
			"problem above and run:\n  billet ca sync --push\n", err)
}

// withAuthorityLock runs fn while this host holds the authority lock.
func withAuthorityLock(cfg *config.Config, log *slog.Logger, fn func(dir string) error) error {
	return wiring.WithAuthorityLock(cfg.Server.IdentityDir, log, fn)
}
