package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

// ScaleSet is one provisioned scale set.
type ScaleSet struct {
	ID    int
	Name  string
	Group string
}

// Provisioner creates scale sets and opens message sessions on them.
//
// Billet's own interface, implemented by internal/scaleset. Two methods is the
// whole of what the control plane needs from GitHub's API, and keeping it that
// small is what lets the scheduler be tested against a fake.
type Provisioner interface {
	// EnsureScaleSet makes a tier's scale set exist. It must be idempotent: it is
	// called on every start, and a scale set an operator created by hand is
	// adopted rather than treated as a conflict.
	EnsureScaleSet(ctx context.Context, name, group string, labels []string) (*ScaleSet, error)
	// Session opens a long-poll session on one scale set.
	Session(ctx context.Context, scaleSetID int, owner string) (Session, error)
}

// Server is billet's control plane: one listener per tier, one shared capacity
// budget between them.
type Server struct {
	alloc *alloc.Allocator
	prov  Provisioner
	tiers []config.Tier
	log   *slog.Logger
	// owner identifies this process to GitHub's message queue, so a session left
	// by a crashed run can be told apart from a live one.
	owner string
}

// New builds a control plane over a configured tier catalog.
func New(a *alloc.Allocator, prov Provisioner, tiers []config.Tier, owner string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}

	return &Server{alloc: a, prov: prov, tiers: tiers, log: log, owner: owner}
}

// Run reconciles a scale set per tier and listens on all of them until the
// context is done.
//
// Reconciliation happens FIRST and for every tier, before any listener starts.
// Starting listeners as their scale sets appear would mean a tier whose
// reconciliation fails leaves the others running against a budget they are
// quietly splitting with a tier that will never take work — and the operator
// sees a half-configured control plane reported as healthy.
func (s *Server) Run(ctx context.Context) error {
	if len(s.tiers) == 0 {
		return errors.New("server: no tiers configured; there is nothing to listen for")
	}

	sets := make(map[string]*ScaleSet, len(s.tiers))

	for i := range s.tiers {
		t := &s.tiers[i]

		set, err := s.prov.EnsureScaleSet(ctx, t.Label, t.RunnerGroup, []string{t.Label})
		if err != nil {
			return fmt.Errorf("server: reconcile scale set for tier %s: %w", t.Label, err)
		}

		if set == nil {
			return fmt.Errorf("server: no scale set for tier %s and no error explaining why", t.Label)
		}

		sets[t.Label] = set

		s.log.Info("scale set ready", "tier", t.Label, "scale_set", set.ID, "group", set.Group)
	}

	// Cancelled together. One listener failing takes the others down rather than
	// leaving a control plane that is advertising for some tiers and silent for
	// others — a state whose only symptom is jobs queueing forever on the tiers
	// nobody is listening to.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)

	for i := range s.tiers {
		t := &s.tiers[i]

		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := s.runTier(runCtx, t, sets[t.Label]); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()

				cancel()
			}
		}()
	}

	wg.Wait()

	// A cancelled context is how this stops on purpose, so it is not an error to
	// report — but a listener that failed for its own reason is, even if the
	// cancellation it triggered arrived at the others first.
	joined := errors.Join(errs...)
	if joined != nil && !onlyCancellation(errs) {
		return joined
	}

	return nil
}

// runTier opens a session and runs one listener on it.
func (s *Server) runTier(ctx context.Context, t *config.Tier, set *ScaleSet) error {
	session, err := s.prov.Session(ctx, set.ID, s.owner)
	if err != nil {
		return fmt.Errorf("server: open session for tier %s: %w", t.Label, err)
	}

	// Closed with a context that outlives cancellation: the ordinary way here is
	// the context being cancelled, and a session left open on GitHub's side is
	// one a restart has to wait out.
	defer func() {
		if err := session.Close(context.WithoutCancel(ctx)); err != nil {
			s.log.Warn("could not close message session", "tier", t.Label, "error", err)
		}
	}()

	return NewListener(s.alloc, t.Label, session, WithLogger(s.log)).Run(ctx)
}

// onlyCancellation reports whether every error is the shutdown itself.
func onlyCancellation(errs []error) bool {
	for _, err := range errs {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return false
		}
	}

	return true
}
