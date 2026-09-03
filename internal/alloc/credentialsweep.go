package alloc

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// LeaseClosure is what the ledger knows about whether one lease is over.
//
// THREE ANSWERS, NOT TWO. Known=false is a lease the ledger has never heard of;
// Terminal=false is one still open; Terminal=true is one that released its
// capacity, closed at FinishedAt. The caller that asks is the control plane's
// sweep of staged CodeBuild registrations, and only the third answer, aged past
// the service's own build ceilings, may authorise deleting one. Collapsing the
// first into the third would delete a registration for a lease this ledger cannot
// see — which is what a ledger restored from an older backup looks like.
type LeaseClosure struct {
	Known    bool
	Terminal bool
	// FinishedAt is when the ledger closed the lease, or zero when the history
	// row predates the column. A zero time is never old enough.
	FinishedAt time.Time
}

// LeaseClosure reports whether a lease is over and when the ledger closed it.
//
// A READ ERROR IS RETURNED, NOT FOLDED INTO "UNKNOWN". An unknown lease is a fact
// the caller reports and keeps; a database that could not answer is evidence about
// nothing, and the caller stops rather than acting on any lease it has not asked
// about.
func (a *Allocator) LeaseClosure(ctx context.Context, leaseID string) (LeaseClosure, error) {
	var out LeaseClosure

	err := a.db.View(ctx, func(tx querier) error {
		row, err := state.ReadQueries(tx).ReadLeaseClosure(ctx, leaseID)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read the closure of lease %s: %w", leaseID, err)
		}

		out.Known = true
		out.Terminal = Phase(row.Phase).Terminal()

		if out.Terminal && row.FinishedAt != "" {
			finished, err := time.Parse(timestampFormat, row.FinishedAt)
			if err != nil {
				return fmt.Errorf("alloc: lease %s closed at %q, which is not a timestamp billet "+
					"writes: %w", leaseID, row.FinishedAt, err)
			}

			out.FinishedAt = finished
		}

		return nil
	})

	return out, err
}

// RegistrationPath is one codebuild host and the Parameter Store path it stages
// runner registrations under.
type RegistrationPath struct {
	Node   string
	Region string
	// Path is empty for a host that registered before it could name one, which
	// the caller reports as unswept rather than treating as clean.
	Path string
	// Decommissioned says an operator has taken the host out of the fleet. Its
	// path is still swept: the registrations a dead node left behind are exactly
	// the ones nobody is left to remove.
	Decommissioned bool
}

// CodeBuildRegistrationPaths lists every codebuild host the ledger has ever seen
// and the path each stages registrations under.
func (a *Allocator) CodeBuildRegistrationPaths(ctx context.Context) ([]RegistrationPath, error) {
	var out []RegistrationPath

	err := a.db.View(ctx, func(tx querier) error {
		rows, err := state.ReadQueries(tx).ListCodeBuildRegistrationPaths(ctx,
			string(config.ProviderCodeBuild))
		if err != nil {
			return fmt.Errorf("alloc: list codebuild registration paths: %w", err)
		}

		for _, row := range rows {
			out = append(out, RegistrationPath{
				Node:           row.Name,
				Region:         row.CodebuildRegion,
				Path:           row.CodebuildJitPath,
				Decommissioned: row.DecommissionedAt != "",
			})
		}

		return nil
	})

	return out, err
}
