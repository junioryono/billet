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
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
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
	RunnerRegistry
}

type trustedRunnerGroupValidator interface {
	ValidateTrustedRunnerGroup(ctx context.Context, group string, wantWorkflows []string) error
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

	// org is the GitHub organization these scale sets belong to. A scale set is
	// org-scoped, so a record of one is only meaningful beside the org it is in.
	org string
	// maxCapacity, when set, caps every listener. See WithMaxCapacity.
	maxCapacity *int
	// reapEvery is how often abandoned capacity is reclaimed.
	reapEvery time.Duration
	// hurry, when closed, ends every listener's drain wait early.
	hurry <-chan struct{}

	// leadershipLost reports that this process has stopped being this
	// deployment's controller, so no listener may act on what it holds. Nil
	// outside the control plane; see WithLeadershipLost.
	leadershipLost func() bool
	// drainTimeout is when every listener starts REPORTING that it is still
	// waiting for its running jobs. It bounds nothing.
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
	// completionStore is shared by listeners but rows are scoped by tier.
	completionStore *state.DB
	// cleanupRetry, when set, paces every listener's cleanup retries. Tests
	// only: the defaults are minutes, and a scenario about a completion that
	// settles on a later attempt would otherwise wait them out for real.
	cleanupRetry *[2]time.Duration

	// onReap, when set, is called with the number of leases each reap pass
	// reclaimed. Tests only, and it exists because a test of what the reaper must
	// NOT reclaim passes trivially against a reaper that was never started.
	onReap func(int)

	// credentialSweeper removes staged runner registrations the ledger has proved
	// dead, or is nil when nothing in this deployment stages one outside its
	// compute. See WithStagedCredentialSweeper.
	credentialSweeper StagedCredentialSweeper
	// onCredentialSweep, when set, is called after each pass with its error.
	// Tests only, for the reason onReap exists.
	onCredentialSweep func(error)

	// converge drives the durable fleet rollout, or is nil when this deployment
	// has no way to reach its nodes.
	//
	// NIL IS A REAL CASE rather than a test convenience: a control plane whose
	// runner is not the node plane — an in-process docker runner on one machine —
	// has nobody to send an upgrade command to, and starting a coordinator that
	// could never dispatch would leave a rollout open forever reporting nothing.
	converge *rollout.Coordinator
	// convergeEvery paces the coordinator. Zero selects the default.
	convergeEvery time.Duration
}

// ControlPlaneOption configures a Server.
type ControlPlaneOption func(*Server)

// WithReapInterval sets how often abandoned capacity is reclaimed.
//
// Exposed so a test can make the reaper actually fire: the default is slow
// relative to any test, so a reaper/heartbeat test left on it never reaches the
// reaper and proves nothing while passing.
func WithReapInterval(d time.Duration) ControlPlaneOption {
	return func(s *Server) { s.reapEvery = d }
}

// StagedCredentialSweeper removes runner registrations that were staged OUTSIDE
// the compute they were minted for and that nothing else will ever remove.
//
// A CodeBuild build cannot be handed a secret, so its registration lives in
// Parameter Store and outlives the build; a node that dies between staging one
// and settling its lease leaks it. Only the ledger can authorise the delete — the
// lease terminal, and closed longer ago than any build could still be running —
// and only this process holds the ledger, which is why the sweep runs here on the
// reaper's clock rather than on a node.
type StagedCredentialSweeper interface {
	// SweepStagedCredentials runs one pass. An error is reported and the next
	// tick tries again; it never stops the control plane.
	SweepStagedCredentials(ctx context.Context) error
}

// WithStagedCredentialSweeper attaches the sweep. Without it nothing is swept,
// which is right for a deployment with no backend that stages a credential.
func WithStagedCredentialSweeper(s StagedCredentialSweeper) ControlPlaneOption {
	return func(srv *Server) { srv.credentialSweeper = s }
}

// WithNodeRunner attaches the compute every listener launches onto.
//
// Without it the listeners fail closed: they account for capacity and decline
// the work, rather than accepting jobs nothing can run.
func WithNodeRunner(r Runner) ControlPlaneOption {
	return func(s *Server) { s.runner = r }
}

// WithOrganization names the GitHub organization this control plane serves, so
// the scale sets it records can be told apart from another organization's under
// the same state directory.
func WithOrganization(org string) ControlPlaneOption {
	return func(s *Server) { s.org = org }
}

// WithCompletionLedger durably preserves authoritative results until nodes accept them.
func WithCompletionLedger(db *state.DB) ControlPlaneOption {
	return func(s *Server) { s.completionStore = db }
}

// WithCleanupRetry sets how soon, and at most how far apart, every
// listener retries a cleanup obligation whose destroy or release failed.
//
// Exposed for the same reason WithReapInterval is: the defaults are sized for a
// node that will not answer sooner for being asked more often, and a test of a
// completion that settles on a LATER attempt proves nothing if the later attempt
// never comes.
func WithCleanupRetry(first, ceiling time.Duration) ControlPlaneOption {
	return func(s *Server) { s.cleanupRetry = &[2]time.Duration{first, ceiling} }
}

// WithDrainTimeout sets when every listener starts REPORTING that its drain is
// running long.
//
// IT IS NOT A DEADLINE. A drain waits for the jobs already running for as long as
// they run, and nothing destroys one on the way out. This is the operator's key —
// server.drain_timeout — reaching the code that honours it; see defaultDrainGrace
// for why the default is the length of a job rather than the length of a
// shutdown.
func WithDrainTimeout(d time.Duration) ControlPlaneOption {
	return func(s *Server) { s.drainTimeout = &d }
}

// WithHurry gives every listener the channel that ends its drain wait early.
//
// This is the operator's second signal reaching the code that honours it. See
// WithHurrySignal for what a listener does with it.
func WithHurry(c <-chan struct{}) ControlPlaneOption {
	return func(s *Server) { s.hurry = c }
}

// WithLeadershipLost gives every listener the question that decides whether its
// teardown may act on anything at all.
//
// SUPPLIED BY cmd/billet RATHER THAN DERIVED HERE, because the fact belongs to
// the ledger handle: `state.DB.LeadershipLost` latches inside the write
// transaction that was refused, which is before the refusal reaches any listener
// and therefore before any of them starts unwinding. A server that inferred it
// from an error would tell the one listener that saw it and leave its siblings
// tearing down as if nothing had happened.
func WithLeadershipLost(fn func() bool) ControlPlaneOption {
	return func(s *Server) { s.leadershipLost = fn }
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
		// REPORTED EVEN HERE, and especially here. Removing the LAST tier is the
		// config edit most likely to strand a scale set, and returning first made
		// the one case that guarantees an orphan the one case that never named it.
		// The refusal still stands — a control plane with nothing to listen for is
		// a mistake — but the operator leaves with what they left behind.
		if err := s.reportUndeclaredScaleSets(ctx, nil); err != nil {
			return err
		}

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

	// THE ROLLOUT IS DRIVEN BY THE CONTROL PLANE, which is what makes
	// `billet rollout start` mean anything. Without this the decision is a durable
	// record nobody acts on: every rollout stays open forever and blocks the next.
	//
	// IT RUNS BESIDE THE LISTENERS AND CANNOT STOP THEM. A pass that fails is
	// logged and retried on the next tick — an upgrade that cannot proceed must
	// never take a scheduler down with it, because a control plane that stopped
	// scheduling to report an upgrade problem has turned a slow rollout into an
	// outage.
	if s.converge != nil {
		converging, stopConverging := context.WithCancel(ctx)
		defer stopConverging()

		go s.converge.Run(converging, s.convergeInterval())
	}

	sets := make(map[string]*ScaleSet, len(s.tiers))

	declared := make(map[scaleSetKey]struct{}, len(s.tiers))
	for i := range s.tiers {
		declared[scaleSetKey{
			group: groupOrDefault(s.tiers[i].RunnerGroup),
			label: s.tiers[i].Label,
		}] = struct{}{}
	}

	if err := s.reportUndeclaredScaleSets(ctx, declared); err != nil {
		return err
	}

	for i := range s.tiers {
		t := &s.tiers[i]
		if t.Trust == config.WorkloadTrusted {
			validator, ok := s.prov.(trustedRunnerGroupValidator)
			if !ok {
				return fmt.Errorf("server: tier %s is trusted, but its provisioner cannot verify runner-group policy", t.Label)
			}
			if err := validator.ValidateTrustedRunnerGroup(ctx, t.RunnerGroup, t.Workflows); err != nil {
				return fmt.Errorf("server: refuse trusted tier %s: %w", t.Label, err)
			}
		}

		set, err := s.prov.EnsureScaleSet(ctx, t.Label, t.RunnerGroup, []string{t.Label})
		if err != nil {
			return fmt.Errorf("server: reconcile scale set for tier %s: %w", t.Label, err)
		}

		if set == nil {
			return fmt.Errorf("server: no scale set for tier %s and no error explaining why", t.Label)
		}

		sets[t.Label] = set

		if s.completionStore != nil {
			rec := state.ScaleSetRecord{
				Org: s.org, RunnerGroup: set.Group, Label: t.Label, ID: set.ID,
			}
			if err := s.completionStore.RecordScaleSet(ctx, rec); err != nil {
				return fmt.Errorf("server: record scale set for tier %s: %w", t.Label, err)
			}
		}

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

// sessionRetry is how long to wait between attempts at a session another one is
// holding.
//
// A PACE, NOT A LIMIT. What is being waited for is GitHub expiring a session an
// abandoned control plane left behind, and asking more often does not make that
// happen sooner.
//
// A VAR SO A TEST CAN DRIVE THE RETRY WITHOUT WAITING, which is the same seam
// every other pace in this repository uses.
var sessionRetryFor = 30 * time.Second

// ErrSessionHeld means a message session for this scale set is already
// outstanding, held by a control plane that did not close it.
//
// A SENTINEL BECAUSE THE ANSWER IS TO WAIT, NOT TO FAIL. Every other reason a
// session cannot open is a reason to report and stop; this one resolves by
// itself when GitHub expires the abandoned session, and it is the ordinary state
// after any restart that was not graceful.
var ErrSessionHeld = errors.New("server: this scale set already has an active message " +
	"session, held by a control plane that did not close it")

// runTier opens a session and runs one listener on it.
func (s *Server) runTier(ctx context.Context, t *config.Tier, set *ScaleSet) error {
	session, err := s.openSession(ctx, t, set)
	if err != nil {
		return err
	}

	// The LISTENER closes the session, not this function. Closing has to happen
	// before the escrow is released, and splitting the two across functions is
	// what put them in the wrong order: Go runs Run's defer first, so the escrow
	// went back while the advertisement was still live.

	return NewListener(s.alloc, t.Label, session, s.listenerOpts()...).Run(ctx)
}

// openSession takes this tier's message session, waiting out one an abandoned
// control plane left behind.
//
// A SESSION IS SINGLE-HOLDER AND GITHUB DOES NOT LET A SUCCESSOR DISPLACE ONE.
// Measured against a real organization rather than assumed: opening a second
// session for a scale set whose first was abandoned answers `409 Conflict ...
// already has an active session for owner`. So the recovery path a restart
// depends on is not "take over", it is "wait for the abandoned one to expire".
//
// RETURNING THE ERROR WAS WRONG, AND IT WAS THE WHOLE BEHAVIOUR BEFORE THIS. A
// control plane killed and restarted — which is every upgrade, every crash, every
// `systemctl restart` — met that 409 and FAILED TO START, taking the tier's
// listener down with it. Nothing said why, and the fix from the outside looked
// like "wait a while and try again", which is exactly what this does instead.
//
// A JOB IS NOT LOST WHILE THIS WAITS. GitHub queues a job for 24 hours when no
// runner is available, and compute already running is held by its node and
// re-adopted; what the wait costs is scheduling latency, which is the tradeoff
// ADR-001 already accepts by choosing recovery in minutes over HA.
func (s *Server) openSession(ctx context.Context, t *config.Tier, set *ScaleSet) (Session, error) {
	for attempt := 1; ; attempt++ {
		session, err := s.prov.Session(ctx, set.ID, s.owner)
		if err == nil {
			return session, nil
		}

		if !errors.Is(err, ErrSessionHeld) {
			return nil, fmt.Errorf("server: open session for tier %s: %w", t.Label, err)
		}

		s.log.Warn("this scale set still has the message session an earlier control "+
			"plane left behind, so this one cannot open its own yet; waiting for GitHub "+
			"to expire it. Queued jobs are not lost while this waits",
			"tier", t.Label, "attempt", attempt, "retry-in", sessionRetryFor)

		// A TIMER RATHER THAN time.After, WHICH THIS REPOSITORY BANS. `time.After`
		// holds its timer until it fires whatever else the select does, so a control
		// plane shut down while waiting keeps one alive for the rest of the pace. It
		// is a small leak here and the rule is not worth a local exception.
		timer := time.NewTimer(sessionRetryFor)

		select {
		case <-ctx.Done():
			timer.Stop()

			return nil, ctx.Err()
		case <-timer.C:
		}
	}
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
	if s.prov != nil {
		opts = append(opts, WithRunnerRegistry(s.prov))
	}
	if s.completionStore != nil {
		opts = append(opts, WithCompletionStore(s.completionStore))
	}

	if s.cleanupRetry != nil {
		opts = append(opts, WithCleanupRetryPacing(s.cleanupRetry[0], s.cleanupRetry[1]))
	}

	// FORWARDED WHENEVER IT WAS SET, including a zero or negative one. Filtering
	// those out here would hide them from the listener's own validation and leave
	// Run using a default nobody asked for; passing them through means configError
	// refuses to start, which is what every other budget does.
	if s.hurry != nil {
		opts = append(opts, WithHurrySignal(s.hurry))
	}

	if s.leadershipLost != nil {
		opts = append(opts, WithLeadershipLostCheck(s.leadershipLost))
	}

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

			// AND THE STAGED CREDENTIALS, LAST. Reaping is what terminalizes the
			// lease of a holder that died, and the sweep acts only on leases closed
			// for longer than a build can run, so ordering buys nothing today; it
			// is placed after the compute sweep so a tick that is slow to reach it
			// delays litter rather than capacity.
			s.sweepStagedCredentials(ctx)

			if s.onReap != nil {
				s.onReap(n)
			}
		}
	}
}

// sweepStagedCredentials runs one pass of the credential sweep, if there is one.
//
// A FENCED CONTROL PLANE SWEEPS NOTHING. Deleting a parameter is an act on the
// world in this deployment's name, and once the ledger has refused this process
// as its controller the successor performs it — the same rule the listeners'
// teardown follows.
//
// WHAT THIS CHECK CANNOT DO IS FENCE A DELETE THAT IS ALREADY IN FLIGHT, and that
// is accepted rather than closed, because the delete cannot diverge from what the
// successor would do. The flag latches only when a write transaction meets the
// successor's epoch, and a pass writes only when it records itself — so a
// controller replaced mid-pass may delete a few names before it finds out. Each
// of those names passed two proofs that no controller can read differently: the
// lease is TERMINAL, which has no successor phase and no writer, and closed longer
// ago than the service window by the ledger's clock AND the parameter's own AWS
// write time. The successor reaches the same verdict on the same row and the
// delete is idempotent. Holding the single writer open across an AWS call to make
// the delete a fenced write would buy nothing and put network latency inside the
// ledger's one writer slot, which is the thing every read-only-on-View rule in
// this repository exists to keep out of it.
func (s *Server) sweepStagedCredentials(ctx context.Context) {
	if s.credentialSweeper == nil {
		return
	}

	if s.leadershipLost != nil && s.leadershipLost() {
		return
	}

	err := s.credentialSweeper.SweepStagedCredentials(ctx)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("could not sweep staged runner registrations; the next tick tries again",
			"error", err)
	}

	if s.onCredentialSweep != nil {
		s.onCredentialSweep(err)
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

// WithRolloutCoordinator gives the control plane the driver for a fleet rollout.
//
// OPTIONAL, because a deployment whose runner is not the node plane has nobody to
// dispatch an upgrade to — and a coordinator that could never dispatch would hold
// a rollout open forever reporting nothing.
func WithRolloutCoordinator(c *rollout.Coordinator, every time.Duration) ControlPlaneOption {
	return func(s *Server) {
		s.converge = c
		s.convergeEvery = every
	}
}

// defaultConvergeEvery is how often the rollout coordinator looks.
//
// SLOW ON PURPOSE. Every transition it makes is driven by an observation the
// ledger already holds — a host's reported release, its liveness — and none of
// them becomes true faster for being asked about more often. What it costs to ask
// is a read on the query-only pool; what it costs to ask constantly is that read
// competing with scheduling for the life of the deployment.
const defaultConvergeEvery = 30 * time.Second

func (s *Server) convergeInterval() time.Duration {
	if s.convergeEvery > 0 {
		return s.convergeEvery
	}

	return defaultConvergeEvery
}
