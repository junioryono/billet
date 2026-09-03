package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/provider/codebuild"
	"github.com/junioryono/billet/internal/state"
)

// controllerCredentialSweep is the control plane's sweep of staged CodeBuild
// registrations a dead node never reaped, over every path the fleet has ever
// registered.
//
// THE LEDGER AUTHORISES AND THE PROVIDER ACTS, and this type is the seam between
// them: it hands codebuild.RegistrationSweeper the allocator's LeaseClosure as the
// only thing that may permit a delete, and records what each pass did so `billet
// status` — another process — can report it. It decides nothing about a lease
// itself.
type controllerCredentialSweep struct {
	alloc *alloc.Allocator
	db    *state.DB
	creds awscreds.Source
	log   *slog.Logger
	now   func() time.Time

	// newSweeper builds a sweeper for one region and path. Replaceable so a test
	// can point one at a fake Parameter Store.
	newSweeper func(region, path string) (*codebuild.RegistrationSweeper, error)

	mu       sync.Mutex
	sweepers map[sweepKey]*codebuild.RegistrationSweeper
}

type sweepKey struct{ region, path string }

// newControllerCredentialSweep wires the sweep over the ledger and the one AWS
// credential chain billet has.
func newControllerCredentialSweep(
	a *alloc.Allocator, db *state.DB, creds awscreds.Source, log *slog.Logger,
) *controllerCredentialSweep {
	s := &controllerCredentialSweep{
		alloc:    a,
		db:       db,
		creds:    creds,
		log:      log,
		now:      time.Now,
		sweepers: map[sweepKey]*codebuild.RegistrationSweeper{},
	}

	s.newSweeper = func(region, path string) (*codebuild.RegistrationSweeper, error) {
		return codebuild.NewRegistrationSweeper(region, path, s.creds,
			codebuild.SweepWithLogger(s.log))
	}

	return s
}

// SweepStagedCredentials runs one pass over every registered path.
//
// ONE PATH FAILING DOES NOT SKIP THE OTHERS, and every pass is recorded whether or
// not it finished: a record that says "stopped: the ledger could not answer" is
// the whole reason the record exists. What IS returned is the join of the
// failures, so the control plane's log names them once per tick.
func (s *controllerCredentialSweep) SweepStagedCredentials(ctx context.Context) error {
	paths, err := s.alloc.CodeBuildRegistrationPaths(ctx)
	if err != nil {
		return err
	}

	var errs []error

	seen := map[sweepKey]bool{}

	for _, p := range paths {
		// A HOST THAT NAMED NO PATH IS NOT SWEPT AND NOT AN ERROR HERE; `billet
		// status` names it. Two hosts on one path are one sweep.
		if p.Path == "" || p.Region == "" {
			continue
		}

		key := sweepKey{region: p.Region, path: p.Path}
		if seen[key] {
			continue
		}

		seen[key] = true

		err := s.sweepOne(ctx, key)
		if err != nil {
			errs = append(errs, err)
		}

		// A LOST LEADERSHIP ENDS THE PASS, NOT ONLY THIS PATH. The recording write is
		// the first fenced transaction a pass makes, so it is where a fenced
		// controller finds out; the remaining paths are the successor's to sweep.
		// What a fenced controller may already have deleted before finding out is
		// harmless: a terminal lease is immutable, the successor evaluates the same
		// two proofs, and the delete is idempotent — see server.sweepStagedCredentials.
		if errors.Is(err, state.ErrLeadershipLost) || ctx.Err() != nil {
			return errors.Join(errs...)
		}
	}

	return errors.Join(errs...)
}

// sweepOne runs and records one pass over one path.
func (s *controllerCredentialSweep) sweepOne(ctx context.Context, key sweepKey) error {
	sweeper, err := s.sweeperFor(key)
	if err != nil {
		// A PATH THE SWEEPER REFUSES IS RECORDED TOO, or a node that registered a
		// path this build cannot address would be silently unswept forever.
		return s.record(ctx, codebuild.SweepReport{Region: key.region, Path: key.path}, err)
	}

	report, sweepErr := sweeper.Sweep(ctx, s.closure)

	return s.record(ctx, report, sweepErr)
}

// closure adapts the allocator's answer to the sweeper's question.
func (s *controllerCredentialSweep) closure(ctx context.Context, leaseID string) (codebuild.LeaseClosure, error) {
	c, err := s.alloc.LeaseClosure(ctx, leaseID)
	if err != nil {
		return codebuild.LeaseClosure{}, err
	}

	return codebuild.LeaseClosure{Known: c.Known, Terminal: c.Terminal, FinishedAt: c.FinishedAt}, nil
}

// record writes the pass down and returns the pass's own error, joined with a
// recording failure if there was one.
func (s *controllerCredentialSweep) record(
	ctx context.Context, report codebuild.SweepReport, sweepErr error,
) error {
	rec := state.CredentialSweepRecord{
		Region:       report.Region,
		Path:         report.Path,
		SweptAt:      s.now(),
		Removed:      report.Removed,
		Kept:         report.Kept,
		Unaccounted:  report.Unaccounted,
		ForeignNames: report.Foreign,
	}

	if sweepErr != nil {
		rec.Error = sweepErr.Error()
	}

	// ON A DETACHED CONTEXT, because the tick that ran this pass may be the one
	// being cancelled, and a pass that removed something must not go unrecorded
	// for it.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	if err := s.db.RecordCredentialSweep(rctx, rec); err != nil {
		return errors.Join(sweepErr, fmt.Errorf("record the sweep of %s: %w", report.Path, err))
	}

	return sweepErr
}

// sweeperFor builds or reuses the sweeper for one region and path.
func (s *controllerCredentialSweep) sweeperFor(key sweepKey) (*codebuild.RegistrationSweeper, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if sweeper, ok := s.sweepers[key]; ok {
		return sweeper, nil
	}

	sweeper, err := s.newSweeper(key.region, key.path)
	if err != nil {
		return nil, err
	}

	s.sweepers[key] = sweeper

	return sweeper, nil
}

// printCredentialSweeps is `billet status`'s account of the sweep: what it has
// removed, what it is waiting on, and which hosts it cannot sweep at all.
//
// IT NEVER FAILS THE COMMAND, for the reason printRollout does not: status is what
// somebody runs when something is already wrong.
func printCredentialSweeps(ctx context.Context, a *alloc.Allocator, db *state.DB) {
	sweeps, err := db.CredentialSweeps(ctx)
	if err != nil {
		fmt.Printf("staged    unavailable: %v\n", err)

		return
	}

	paths, err := a.CodeBuildRegistrationPaths(ctx)
	if err != nil {
		fmt.Printf("staged    unavailable: %v\n", err)

		return
	}

	for _, sw := range sweeps {
		fmt.Printf("staged    %s (%s): %d registration(s) removed in total, %d last pass; %d waiting "+
			"on their leases, %d naming leases this ledger has never seen, %d not billet's; last "+
			"swept %s\n",
			sw.Path, sw.Region, sw.RemovedTotal, sw.Removed, sw.Kept, sw.Unaccounted,
			sw.ForeignNames, formatSweptAt(sw.SweptAt))

		if sw.Error != "" {
			fmt.Printf("          the last pass stopped: %s\n", sw.Error)
		}
	}

	// A CODEBUILD HOST THAT NAMED NO PATH IS SAID OUT LOUD. Silence here would read
	// as "nothing to sweep", and the leak this exists for is one nobody sees.
	for _, p := range paths {
		if p.Path != "" && p.Region != "" {
			continue
		}

		fmt.Printf("staged    node %s registered without its registration path, so nothing "+
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
