package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

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
	// maxCapacity, when set, caps every listener. See WithMaxCapacity.
	maxCapacity *int
	// reapEvery is how often abandoned capacity is reclaimed.
	reapEvery time.Duration
}

// ControlPlaneOption configures a Server.
type ControlPlaneOption func(*Server)

// WithReapInterval sets how often abandoned capacity is reclaimed.
//
// Exposed so a test can make the reaper actually fire. The default is slow
// relative to any test, which meant the first version of the reaper/heartbeat
// interaction test never reached the reaper at all — it passed and proved
// nothing, which is the same trap as an instant fake finishing before a
// goroutine runs.
func WithReapInterval(d time.Duration) ControlPlaneOption {
	return func(s *Server) { s.reapEvery = d }
}

// AdvertiseNothing makes every listener advertise zero capacity.
//
// It exists so the whole path — App auth, scale-set reconciliation, session,
// long poll — can be exercised against a REAL organization without accepting a
// job that nothing in this repository can yet launch. Accepting one would strand
// somebody's CI, which is a worse first contact than not connecting at all.
func AdvertiseNothing() ControlPlaneOption {
	return func(s *Server) {
		zero := 0
		s.maxCapacity = &zero
	}
}

// New builds a control plane over a configured tier catalog.
func New(
	a *alloc.Allocator, prov Provisioner, tiers []config.Tier,
	owner string, log *slog.Logger, opts ...ControlPlaneOption,
) *Server {
	if log == nil {
		log = slog.Default()
	}

	s := &Server{
		alloc: a, prov: prov, tiers: tiers, log: log, owner: owner,
		reapEvery: defaultReapInterval,
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
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

	// Reaped ONCE before anything is escrowed, and then on a timer.
	//
	// A hard kill leaves escrowed leases in SQLite with no process behind them.
	// Nothing else ever removes those: every restart counts them against headroom,
	// so a host that was SIGKILLed while holding capacity comes back advertising
	// less, and eventually advertises nothing at all. The failure is permanent and
	// silent, which is the worst pair.
	//
	// The startup pass is what makes a restart recover rather than accumulate. The
	// timer is for a lease whose holder dies mid-run.
	if n, err := s.alloc.Reap(ctx); err != nil {
		return fmt.Errorf("server: reclaim abandoned capacity: %w", err)
	} else if n > 0 {
		s.log.Info("reclaimed capacity from leases with no live holder", "leases", n)
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

	wg.Add(1)

	go func() {
		defer wg.Done()

		s.reapPeriodically(runCtx)
	}()

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

	// The LISTENER closes the session, not this function. Closing has to happen
	// before the escrow is released, and splitting the two across functions is
	// what put them in the wrong order: Go runs Run's defer first, so the escrow
	// went back while the advertisement was still live.

	opts := []Option{WithLogger(s.log)}
	if s.maxCapacity != nil {
		opts = append(opts, WithMaxCapacity(*s.maxCapacity))
	}

	return NewListener(s.alloc, t.Label, session, opts...).Run(ctx)
}

// reapPeriodically reclaims capacity from leases whose holder stopped
// heartbeating, until the context is done.
//
// It is deliberately a WARNING and not a failure: a reap that cannot run leaves
// capacity stranded, which is bad, but stopping the control plane over it strands
// all of the capacity rather than some.
func (s *Server) reapPeriodically(ctx context.Context) {
	ticker := time.NewTicker(s.reapEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.alloc.Reap(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return
				}

				s.log.Warn("could not reclaim abandoned capacity", "error", err)

				continue
			}

			if n > 0 {
				s.log.Info("reclaimed capacity from leases with no live holder", "leases", n)
			}
		}
	}
}

// defaultReapInterval is how often abandoned capacity is reclaimed. Frequent
// enough that a crashed holder does not strand capacity for long, rare enough
// that it is not a meaningful load on the one authoritative writer.
const defaultReapInterval = 30 * time.Second

// onlyCancellation reports whether every error is the shutdown itself.
func onlyCancellation(errs []error) bool {
	for _, err := range errs {
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			return false
		}
	}

	return true
}
