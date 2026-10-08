package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/cli"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/state"
)

// printCredentialSweeps is `billet status`'s account of the sweep: what it has
// removed, what it is waiting on, and which hosts it cannot sweep at all.
//
// IT NEVER FAILS THE COMMAND, for the reason printRollout does not: status is what
// somebody runs when something is already wrong.
func printCredentialSweeps(ctx context.Context, env cli.Env, a *alloc.Allocator, db *state.DB) {
	sweeps, err := db.CredentialSweeps(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "staged    unavailable: %v\n", err)

		return
	}

	paths, err := a.CodeBuildRegistrationPaths(ctx)
	if err != nil {
		fmt.Fprintf(env.Stdout, "staged    unavailable: %v\n", err)

		return
	}

	for _, sw := range sweeps {
		fmt.Fprintf(env.Stdout, "staged    %s (%s): %d registration(s) removed in total, %d last pass; %d waiting "+
			"on their leases, %d naming leases this ledger has never seen, %d not billet's; last "+
			"swept %s\n",
			sw.Path, sw.Region, sw.RemovedTotal, sw.Removed, sw.Kept, sw.Unaccounted,
			sw.ForeignNames, formatSweptAt(sw.SweptAt))

		if sw.Error != "" {
			fmt.Fprintf(env.Stdout, "          the last pass stopped: %s\n", sw.Error)
		}
	}

	// A CODEBUILD HOST THAT NAMED NO PATH IS SAID OUT LOUD. Silence here would read
	// as "nothing to sweep", and the leak this exists for is one nobody sees.
	for _, p := range paths {
		if p.Path != "" && p.Region != "" {
			continue
		}

		fmt.Fprintf(env.Stdout, "staged    node %s registered without its registration path, so nothing "+
			"sweeps the registrations it stages; upgrade it so its registration names "+
			"node.codebuild.jit_parameter_path\n", p.Node)
	}
}

// formatSweptAt renders a pass's time, or says the record could not.
func formatSweptAt(t time.Time) string {
	if t.IsZero() {
		return "at an unrecorded time"
	}

	return strings.TrimSpace(t.UTC().Format(time.RFC3339))
}
