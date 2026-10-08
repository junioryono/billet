package fleetops

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/hostauthority"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/wireshare"
)

// authorityLockAccess is the identity access the authority lock takes, in the
// shape app.WithAuthorityLock wants: the whole exclusion, non-blocking, creating
// nothing, as an operator command wants.
func AuthorityLockAccess(ctx context.Context, dir string) (func() error, error) {
	acc, err := hostauthority.Open(ctx, dir, hostauthority.Intent{})
	if err != nil {
		return nil, err
	}

	return acc.Release, nil
}

// publishRotatedAuthority carries a rotation or a retirement into the store.
//
// A ROTATION NOBODY ELSE HEARS ABOUT IS HALF A ROTATION. On a deployment with a
// second controller the store is how that host learns there is a new authority at
// all — and after a RETIREMENT it is how that host learns the previous pair is
// gone, which it would otherwise keep presenting a certificate from.
//
// REPORTED AND NOT FATAL, because by the time this runs the operation on THIS
// host is complete and irreversible. Returning an error would tell an operator
// their rotation failed when it did not; what they need is the one command that
// finishes the job.
func publishRotatedAuthority(ctx context.Context, env cli.Env, cfg *config.Config, deployment string) {
	store := app.AuthorityStoreFor(cfg)
	if store == nil {
		return
	}

	log := slog.Default()

	err := app.WithAuthorityLock(ctx, cfg, AuthorityLockAccess, log, func(dir string) error {
		return wireshare.Publish(ctx, store, dir, deployment)
	})
	if err == nil {
		fmt.Fprintln(env.Stdout, "Published the new authority to this deployment's identity store.")

		return
	}

	fmt.Fprintf(env.Stderr,
		"\nThis host is done, but publishing to the identity store failed:\n  %v\n\n"+
			"The other controller cannot see this change until it is published. Fix the\n"+
			"problem above and run:\n  billet ca sync --push\n", err)
}
