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
	// warnNoSweeper makes the missing-capability warning appear once rather than
	// on every tick, which would bury it.
	warnNoSweeper sync.Once

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
	// drainTimeout is how long every listener waits for its running jobs on
	// shutdown.
	//
	// A POINTER so "never configured" is distinguishable from "configured as
	// zero". Guarding on `> 0` instead made WithDrainTimeout(0) silently select
	// the default — which is the exact substitution checkGrace refuses to make,
	// for the reason it gives: omitting the option already selects the default,
	// so passing zero is an explicit instruction and swallowing it removes the
	// evidence of the mistake.
	drainTimeout *time.Duration
	// runner is handed to every listener, so an assigned lease becomes compute.
	// nil means none is attached and the listeners fail closed.
	runner Runner

	// onReap, when set, is called with the number of leases each reap pass
	// reclaimed. Tests only, and it exists because a test of what the reaper must
	// NOT reclaim passes trivially against a reaper that was never started.
	onReap func(int)
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

// WithNodeRunner attaches the compute every listener launches onto.
//
// Without it the listeners fail closed: they account for capacity and decline
// the work, rather than accepting jobs nothing can run.
func WithNodeRunner(r Runner) ControlPlaneOption {
	return func(s *Server) { s.runner = r }
}

// WithDrainTimeout sets how long every listener waits for the jobs it is
// already running before a shutdown destroys them.
//
// This is the operator's key — server.drain_timeout — reaching the code that
// honours it. See defaultDrainGrace for why the default is the length of a job
// rather than the length of a shutdown.
func WithDrainTimeout(d time.Duration) ControlPlaneOption {
	return func(s *Server) { s.drainTimeout = &d }
}

// OptionsFromConfig is the control-plane configuration implied by billet.yaml.
//
// It lives here rather than in cmd/billet so the whole chain — the YAML key, the
// parse, the control-plane option, and the listener it configures — is one
// package's worth of code and can be tested end to end. Assembling it at the
// call site left the only link that matters, a value reaching a listener,
// spanning two packages with a test on neither side of the join.
func OptionsFromConfig(cfg *config.Config) ([]ControlPlaneOption, error) {
	if cfg == nil {
		return nil, nil
	}

	drain, err := cfg.Server.DrainTimeoutDuration()
	if err != nil {
		return nil, err
	}

	return []ControlPlaneOption{WithDrainTimeout(drain)}, nil
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

	// STARTED BEFORE ANYTHING SLOW, and before the first reap.
	//
	// Recovery renews each adopted lease once as it adopts, but scale-set
	// reconciliation runs between that and the startup reap — a network round
	// trip per tier against GitHub, which can exceed a lease TTL on a bad day.
	// The reaper would then terminalize a lease billet is deliberately holding,
	// and a listener would advertise its capacity while the container ran on.
	if sweeper, ok := s.runner.(Sweeper); ok {
		keepAlive, stopKeepAlive := context.WithCancel(ctx)
		defer stopKeepAlive()

		go sweeper.KeepAlive(keepAlive)
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

	return NewListener(s.alloc, t.Label, session, s.listenerOpts()...).Run(ctx)
}

// listenerOpts is what every listener this control plane starts is configured
// with.
//
// Factored out of runTier so a test can assert that a control-plane option
// actually reaches a listener. Asserting on the option alone passes while the
// value never leaves the config file, which is the whole failure worth catching
// here.
func (s *Server) listenerOpts() []Option {
	opts := []Option{WithLogger(s.log)}
	if s.maxCapacity != nil {
		opts = append(opts, WithMaxCapacity(*s.maxCapacity))
	}

	if s.runner != nil {
		opts = append(opts, WithRunner(s.runner))
	}

	// FORWARDED WHENEVER IT WAS SET, including a zero or negative one. Filtering
	// those out here would hide them from the listener's own validation and leave
	// Run using a default nobody asked for; passing them through means configError
	// refuses to start, which is what every other budget does.
	if s.drainTimeout != nil {
		opts = append(opts, WithDrainGrace(*s.drainTimeout))
	}

	return opts
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

			// SWEPT AFTER REAPING, IN THAT ORDER. The reap is what terminalizes the
			// lease of a holder that died, so a container that was legitimate a
			// moment ago becomes an orphan during this very tick. Sweeping first
			// would consistently miss exactly the case the pair exists for.
			s.sweep(ctx)

			if s.onReap != nil {
				s.onReap(n)
			}
		}
	}
}

// sweep asks the runner to destroy compute no lease is holding, if it can.
//
// Failures are logged rather than returned: this runs on a timer beside the
// reaper, and taking the control plane down because one container would not die
// would convert a bounded over-commitment into a total outage. The next tick
// tries again, which is the whole point of doing this on a timer.
func (s *Server) sweep(ctx context.Context) {
	sweeper, ok := s.runner.(Sweeper)
	if !ok {
		// SAID OUT LOUD, once per tick's worth of surprise. A runner that cannot
		// enumerate its compute has no orphan protection at all: after a reap frees
		// capacity, whatever was running under that lease stays running while
		// replacement work is admitted against the same vCPU. That is a real
		// degradation and it must not be inferable only from a missing log line.
		s.warnNoSweeper.Do(func() {
			s.log.Warn("the attached runner cannot enumerate its compute, so billet " +
				"cannot detect or remove instances whose lease is gone; a crash or a " +
				"failed cleanup will over-commit this host until it is restarted")
		})

		return
	}

	// TENDED BEFORE SWEEPING. Tend is what keeps a held lease alive and what
	// takes an adopted container out of custody when it finishes; sweeping first
	// would judge those instances against a lease set Tend was about to change.
	if err := sweeper.Tend(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}

		s.log.Warn("could not advance compute the runner is holding capacity for", "error", err)
	}

	if err := sweeper.Sweep(ctx); err != nil {
		if ctx.Err() != nil {
			return
		}

		s.log.Warn("could not remove compute that no lease is holding", "error", err)
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
