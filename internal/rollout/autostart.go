package rollout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/junioryono/billet/internal/version"
)

// Target is one immutable release, resolved outside this package.
//
// A VERSION AND THE DIGEST OF ITS MANIFEST, which is what a rollout persists.
// Notes are what the resolver wants said about the target when it is acted on —
// a guest-contract change, say — and decide nothing.
type Target struct {
	Version string
	Digest  string
	Notes   []string
}

// Resolver turns the deployment's channel or pin into one target that this
// build could install.
//
// OUTSIDE THIS PACKAGE, AND THAT IS THE LAYERING. Resolving a channel reaches
// the network and verifying a manifest reaches a signature library, and a ledger
// writer may do neither (the ledgerwriters rule in .golangci.yml says why). What
// crosses the seam is the answer: a target that has already passed the same
// compatibility preflight `billet rollout start` runs, or an error saying why
// there is none this tick.
type Resolver interface {
	Resolve(ctx context.Context) (Target, error)
}

// StartPolicy is what the deployment's config says about starting rollouts by
// itself.
type StartPolicy struct {
	// Enabled is release.automatic, read through its accessor.
	Enabled bool

	// OpenAt says whether a rollout may begin at a moment, or is nil for a
	// deployment with no maintenance window.
	OpenAt func(time.Time) bool

	// Channel is the channel followed, or empty for a deployment pinned to Pin.
	Channel string
	Pin     string

	// Rollout is the policy every automatic rollout is recorded with. Its
	// AllowDowngrade is ignored: an automatic start never downgrades.
	Rollout Policy
}

// source names how the target was chosen, for the record.
func (p StartPolicy) source() string {
	if p.Pin != "" {
		return "pin " + p.Pin
	}

	return p.Channel + " channel"
}

// Starter begins a rollout when the channel names a release the fleet is not on.
//
// THE OTHER HALF OF `release.automatic`. The Coordinator converges a rollout
// that exists; this is what makes one exist without an operator typing
// `billet rollout start`. Everything it decides is an observation — the ledger
// has no open rollout, the channel names something newer, the fleet is not
// already on it, nobody aborted that exact target — and the one write it makes is
// the same Store.Start the command makes.
type Starter struct {
	store      *Store
	fleet      Fleet
	resolve    Resolver
	policy     StartPolicy
	ourVersion string
	now        func() time.Time
	log        *slog.Logger
	// said is when each rate-limited message was last logged, keyed by what it
	// was about. In memory, like the coordinator's warnedAt: it bounds a
	// diagnostic and authorises nothing.
	said map[string]time.Time
}

// StarterOption configures a Starter.
type StarterOption func(*Starter)

// WithStarterClock replaces the clock, so a test can drive the window and the
// rate limit.
func WithStarterClock(now func() time.Time) StarterOption {
	return func(s *Starter) { s.now = now }
}

// WithStarterLogger sets where the starter reports.
func WithStarterLogger(log *slog.Logger) StarterOption {
	return func(s *Starter) { s.log = log }
}

// NewStarter builds the driver that begins rollouts.
//
// ourVersion is what this binary reports itself as, passed in for the reason the
// Coordinator's is: a package that read it itself could only be tested against
// the build running the test.
func NewStarter(store *Store, fleet Fleet, resolve Resolver, policy StartPolicy,
	ourVersion string, opts ...StarterOption,
) *Starter {
	s := &Starter{
		store:      store,
		fleet:      fleet,
		resolve:    resolve,
		policy:     policy,
		ourVersion: ourVersion,
		now:        time.Now,
		log:        slog.Default(),
		said:       make(map[string]time.Time),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// DefaultStartInterval is how often the starter looks at the channel.
//
// HOURLY, because a channel advances when a release is cut and nothing about it
// becomes true faster for being asked more often. The statement it reads is
// served from a branch rather than the API, so the cost of asking is a small
// fetch and not a share of anybody's rate limit.
const DefaultStartInterval = time.Hour

// startInitialDelay is how long after a control plane starts before the first
// look, so a restart does not begin a rollout before the fleet has re-registered
// and reported what it runs.
const startInitialDelay = time.Minute

// sayAgainAfter bounds how often one condition is reported.
//
// AN EXPIRED CHANNEL STATEMENT WOULD OTHERWISE BE WARNED ABOUT HOURLY, FOREVER,
// and a warning repeated every hour for a week is one nobody reads on the eighth
// day. Six hours keeps a condition visible without turning the journal into a
// list of the same sentence.
const sayAgainAfter = 6 * time.Hour

// Run looks on a cadence until the context ends.
func (s *Starter) Run(ctx context.Context, every time.Duration) {
	if every <= 0 {
		every = DefaultStartInterval
	}

	timer := time.NewTimer(startInitialDelay)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		// A TICK THAT FAILS IS NOT A STARTER THAT STOPS, for the coordinator's
		// reason: everything it reads can be transiently unavailable.
		if err := s.Tick(ctx); err != nil && ctx.Err() == nil {
			s.log.Warn("the automatic rollout check did not complete; trying again on the "+
				"next tick", "error", err)
		}

		timer.Reset(every)
	}
}

// Tick starts a rollout if, and only if, everything observable says one is due.
//
// EVERY STEP IS AN OBSERVATION, IN THE ORDER THAT COSTS LEAST. The config and
// the window are read first because they need nothing; the ledger next because
// an open rollout makes the rest moot; the channel after that because it is the
// one step that leaves the machine. An error is returned only for a ledger that
// could not be read — every other reason not to start is logged and waited out,
// because "the channel is unreachable" is a condition the next tick may find
// changed and not a failure of this process.
func (s *Starter) Tick(ctx context.Context) error {
	if !s.policy.Enabled {
		return nil
	}

	if s.policy.OpenAt != nil && !s.policy.OpenAt(s.now()) {
		s.log.Debug("outside the maintenance window; starting no rollout")

		return nil
	}

	if _, err := s.store.Open(ctx); err == nil {
		s.log.Debug("a rollout is already running; starting no other")

		return nil
	} else if !errors.Is(err, ErrNoRollout) {
		return err
	}

	target, err := s.resolve.Resolve(ctx)
	if err != nil {
		s.say("resolve", "the "+s.policy.source()+" could not be resolved to a target this "+
			"deployment can install; nothing starts until it can", "error", err.Error())

		return nil
	}

	if target.Version == "" || target.Digest == "" {
		s.say("target", "the "+s.policy.source()+" resolved to a target with no version or no "+
			"digest, which nothing here will record")

		return nil
	}

	hosts, err := s.fleet.Hosts(ctx)
	if err != nil {
		return fmt.Errorf("rollout: read the fleet: %w", err)
	}

	if len(hosts) == 0 {
		// A ROLLOUT WITH NO HOSTS IS REFUSED BY Start, and reaching that refusal
		// hourly on a control plane whose first node has not registered yet is
		// noise about an ordinary state.
		s.log.Debug("no node has registered, so there is nothing to roll out to")

		return nil
	}

	if fleetOn(target.Version, s.ourVersion, hosts) {
		s.log.Debug("the fleet is on the "+s.policy.source()+"'s target; nothing to do",
			"target", target.Version)

		return nil
	}

	// AN AUTOMATIC START NEVER DOWNGRADES. A channel that regressed is refused
	// by its publisher; a pin older than the running release is an operator's
	// decision to make by name, with `billet rollout start --allow-downgrade`.
	if order, ok := version.Compare(target.Version, s.ourVersion); ok && order < 0 {
		s.say("downgrade", "the "+s.policy.source()+" names "+target.Version+", which is older "+
			"than the "+s.ourVersion+" running here; an automatic rollout never downgrades. "+
			"`billet rollout start --version "+target.Version+" --allow-downgrade` is the "+
			"way to do it on purpose")

		return nil
	}

	// AN OPERATOR'S ABORT BEATS THE CHANNEL. Somebody abandoned a rollout to
	// exactly these bytes, with a reason; starting it again an hour later would
	// overrule them with nothing new to go on. The channel moving on to a
	// different release is what ends the refusal.
	//
	// THE NEWEST ROLLOUT TO THESE BYTES, WHEREVER IT SITS IN THE HISTORY: an abort
	// found by walking the last N rollouts of any target is one that N unrelated
	// rollouts later would be forgotten, and an operator's decision does not age
	// out because the fleet was busy.
	last, found, err := s.store.NewestForTarget(ctx, target.Digest)
	if err != nil {
		return err
	}

	if found && last.State == StateAborted {
		s.say("aborted", "the "+s.policy.source()+" names "+target.Version+", and rollout "+
			last.ID+" to that exact release was aborted ("+last.TerminalReason+
			"); nothing automatic restarts it. `billet rollout start` does, on purpose")

		return nil
	}

	names := make([]string, 0, len(hosts))
	for i := range hosts {
		names = append(names, hosts[i].Name)
	}

	// THE WINDOW IS ASKED AGAIN, AT THE MOMENT OF BEGINNING. Resolving a channel
	// and reading a fleet take as long as they take, and the window bounds when a
	// rollout may BEGIN, which is the write below and not the tick that led to it.
	if s.policy.OpenAt != nil && !s.policy.OpenAt(s.now()) {
		s.log.Debug("the maintenance window closed while the target was being resolved; " +
			"starting no rollout")

		return nil
	}

	policy := s.policy.Rollout
	policy.AllowDowngrade = false

	recorded, err := s.store.Start(ctx, StartRequest{
		Channel:       s.policy.Channel,
		TargetVersion: target.Version,
		TargetDigest:  target.Digest,
		PriorVersion:  s.ourVersion,
		Policy:        policy,
		CreatedBy:     "automatic (" + s.policy.source() + ")",
		Nodes:         names,
	})
	if err != nil {
		if errors.Is(err, ErrOpen) {
			// SOMEBODY STARTED ONE BETWEEN THE READ AND THE WRITE, which the store
			// decides rather than this; the next tick sees it as open.
			return nil
		}

		return err
	}

	for _, note := range target.Notes {
		s.log.Warn("about the rollout just started", "note", note, "rollout", recorded.ID)
	}

	s.log.Info("started a rollout, because the "+s.policy.source()+" advanced",
		"rollout", recorded.ID, "from", recorded.PriorVersion, "to", recorded.TargetVersion,
		"manifest", recorded.TargetDigest, "hosts", len(names))

	return nil
}

// fleetOn reports whether the control plane and every host already run a
// release.
//
// A HOST REPORTING NOTHING IS NOT ON IT. A build below the version that names
// its release cannot say what it runs, and "cannot say" must not be read as
// "already there" — that host is exactly one a rollout exists to move, or to
// block with a reason a person can read.
func fleetOn(target, ourVersion string, hosts []Host) bool {
	if ourVersion != target {
		return false
	}

	for i := range hosts {
		if hosts[i].Release != target {
			return false
		}
	}

	return true
}

// say logs one condition at warning level, at most once per sayAgainAfter.
func (s *Starter) say(key, msg string, attrs ...any) {
	now := s.now()

	if last, ok := s.said[key]; ok && now.Sub(last) < sayAgainAfter {
		return
	}

	s.said[key] = now

	s.log.Warn(msg, attrs...)
}
