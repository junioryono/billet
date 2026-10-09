package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
	"github.com/junioryono/billet/internal/state"
)

// WithRunner sets what turns assigned leases into running compute.
//
// THE DEFAULT DECLINES THE JOB, which is the opposite of what this comment used to
// claim. noRunner fails closed: it returns an error, the ordinary failed-launch
// path hands the capacity back, and GitHub reassigns. It does not hold capacity
// and it does not quietly succeed — see noRunner's own documentation, which said
// so correctly while this said the reverse.
// WithPoolLaunchBatch sets how many pool members one reconciliation starts at
// once. One, the default, launches each and waits for it before buying the next.
func WithPoolLaunchBatch(n int) Option {
	return func(l *Listener) { l.poolLaunchBatch = max(n, 1) }
}

func WithRunner(r dispatch.Runner) Option {
	return func(l *Listener) { l.runner = r }
}

// WithRunnerRegistry installs the GitHub side of safe runner retirement.
// WithCachePublication gives the listener its tier's effective cache
// configuration and the target's run evidence, which a completion's cache
// authority is decided from. Without it, or without evidence, every completion
// carries the zero authority and publishes nothing beyond the legacy rules.
func WithCachePublication(spec config.CacheSpec, evidence RunEvidence) Option {
	return func(l *Listener) {
		l.cacheSpec = spec
		l.runEvidence = evidence
	}
}

func WithRunnerRegistry(registry RunnerRegistry) Option {
	return func(l *Listener) { l.registry = registry }
}

// WithCompletionStore makes result delivery and its capacity settlement durable
// across listener restarts.
func WithCompletionStore(db *state.DB) Option {
	return withCompletionStore(db)
}

// withCompletionStore admits a fault-injecting store in package tests.
func withCompletionStore(store completionStore) Option {
	return func(l *Listener) { l.completionStore = store }
}

// defaultStalePromise is how long a promise may go unclaimed before billet warns.
// DIAGNOSTIC, not a deadline: an acquisition is a commitment to GitHub that no
// local timer can revoke, so a timed release would only mean billet had forgotten
// it owes a runner while GitHub still expects one.
const defaultStalePromise = 5 * time.Minute

const (
	// Enough redeliveries to outlive a transient ledger observation, while still
	// ending a deterministic loop without operator intervention.
	poisonQuarantineAfter = 3

	firstRetryEvery = 15 * time.Second
	// A node refusing this long will not answer sooner for being asked more often;
	// the point of the ceiling is that it keeps asking at all.
	maxRetryEvery = 5 * time.Minute
	// ONE. A node executes commands one at a time and each command's timeout starts
	// when it is QUEUED, so concurrent destroys against one node start N clocks against
	// a queue served in turn and the ones at the back expire.
	//
	// The useful shape is one command in flight PER NODE, which belongs in the plane —
	// only it knows which commands share a queue.
	teardownConcurrency = 1

	// The local half of the teardown. Short, because neither waits on a node, and
	// separate from each other because a session close that used most of a shared
	// budget would leave the releases — sequential, against one SQLite writer — to
	// fail on what was left.
	defaultCloseGrace   = 30 * time.Second
	defaultReleaseGrace = 30 * time.Second

	// Longer than any real teardown and plainly finite, so the four budgets cannot
	// sum past int64 and a watchdog always fires on a timescale an operator lives
	// on. Deliberately does NOT bound the drain; see maxDrainGrace.
	maxGrace = time.Hour

	// When the listener starts REPORTING that a drain is running long.
	//
	// SIX HOURS BECAUSE THAT IS THE LENGTH OF A JOB, not of a shutdown: GitHub's
	// timeout-minutes defaults to 360. Every other budget here bounds work BILLET
	// is doing; this one bounds nothing at all. Overrunning it is not a failure
	// state and never was — what changed is that it is no longer the moment the
	// jobs still running get destroyed.
	defaultDrainGrace = 6 * time.Hour

	// NO CEILING, expressed as one so setWithin keeps one shape. A day used to be
	// the limit, because past that magnitude a typo was likelier than the intent
	// and believing one meant waiting that long before DESTROYING the work. The
	// drain destroys nothing now and this value only decides when billet starts
	// reporting that it is still waiting, so an implausible number costs a quieter
	// log — and refusing an operator's config over that would be theatre.
	maxDrainGrace = time.Duration(math.MaxInt64)

	// Bounds the whole teardown. Renewal continues after the caller cancels, which lets
	// the release destroy compute without the reaper taking the capacity underneath it;
	// the cost is a wedged teardown renewing forever, so it gets its own deadline.
	//
	// IT MUST EXCEED A LEGITIMATE TEARDOWN. The node command timeout is TEN MINUTES, so
	// a destroy of a node that went quiet can outlast a short grace — after which the
	// watchdog stops renewal, the reaper reclaims, and another tier can take a machine
	// whose container is still being destroyed. This covers ONE such destroy; N of them
	// need N times this and will not get it, which is bounded and reported.
	defaultShutdownGrace = 12 * time.Minute
)

// Option configures a Listener.
type Option func(*Listener)

// WithStalePromiseAfter sets how long an acquired job may go unassigned before
// billet reports it. It does not reclaim anything — see defaultStalePromise.
func WithStalePromiseAfter(d time.Duration) Option {
	return func(l *Listener) { l.stalePromise = d }
}

// WithShutdownGrace bounds the teardown: how long the listener will spend
// destroying compute and closing its session before giving up and letting the
// reaper deal with what is left.
//
// Worth setting on a deployment whose provider is genuinely slow to destroy —
// the alternative to a grace that is too short is not a cleaner shutdown, it is
// leases nobody releases and containers nobody removes.
func WithShutdownGrace(d time.Duration) Option {
	return func(l *Listener) {
		if l.set("shutdown grace", d) {
			l.shutdownGrace = d
		}
	}
}

// WithHurrySignal gives the listener a channel whose closing ends the drain's
// wait.
//
// THE ONLY THING THAT ENDS A DRAIN WITH WORK STILL RUNNING, now that nothing
// bounds it. An operator who cannot wait needs a lever that stops the WAITING
// without stopping the teardown billet owes; what follows is the session close,
// the idle escrow, and the destroys a completion already asked for. The jobs
// still executing are left alone. Without this the only escape is killing the
// process, which loses billet's bookkeeping rather than the work — but leaves
// the operator no orderly way out.
func WithHurrySignal(c <-chan struct{}) Option {
	return func(l *Listener) { l.hurry = c }
}

// WithRestartHandoff makes a stop that arrives while the deployment is admitting
// work a handoff to the next control plane rather than a drain (#365, #368).
//
// A DRAIN WAITS FOR THE LONGEST RUNNING JOB, and on 2026-10-04 a consumer's
// converge stopped the reference deployment's controller for ninety minutes
// that way, timed out, and launched nothing in between. Worse, the drain lowers
// the advertisement to what is running, and a job GitHub re-offers in that
// window (it cancels a declined request after five minutes and offers the job
// again only to a scale set with room) was never offered again: 28 jobs stayed
// queued with no runner until somebody re-ran them.
//
// SO AN UNSEALED STOP IS A RESTART, and it hands over the way a fenced
// controller already does (abandon): the jobs keep running on their hosts and the
// next process re-adopts them, the message session is left open so GitHub goes on
// seeing the last advertisement and redelivers what it assigns to the successor
// (measured, 2026-09-04), and capacity comes back when its leases stop being
// renewed. Only the destroys this listener already owes, for jobs GitHub has
// concluded, still run. A SEALED or UNREADABLE admission keeps the drain: an
// operator who sealed asked for the deployment to stop taking work, and a stop
// that cannot prove otherwise is not entitled to call itself a restart. Stopping a
// deployment for good is sealing it first (`billet drain`, `local down`).
func WithRestartHandoff() Option {
	return func(l *Listener) { l.restartHandoff = true }
}

// WithLeadershipLostCheck supplies the question "has this process stopped being
// this deployment's controller", which the teardown asks before it acts on
// anything. Named like WithHurrySignal beside it: the listener option carries
// the specific name and the control-plane one that forwards it carries the
// short one.
//
// A PREDICATE RATHER THAN A CHANNEL, unlike the hurry signal beside it, and the
// difference is what each one means. A hurry is an EVENT an operator sends once,
// and a listener that was not watching when it arrived must still see it. This
// is a durable FACT about the process — `state.DB.LeadershipLost` latches and
// never clears — so the only thing a caller ever needs is to ask.
//
// NIL EVERYWHERE BUT THE CONTROL PLANE. Nothing else has a claim to lose.
func WithLeadershipLostCheck(fn func() bool) Option {
	return func(l *Listener) { l.leadershipLost = fn }
}

// WithHeartbeatOverrunReport tells fn when a heartbeat pass is still running as
// the next falls due. It is told WHILE the pass runs, from a goroutine of its
// own, so what it captures is the stall itself; fn must return at once.
func WithHeartbeatOverrunReport(fn func()) Option {
	return func(l *Listener) { l.heartbeatOverrun = fn }
}

// WithSessionReopen lets the listener replace a session whose poll failed after
// the client's own retries rather than return, because a listener returning
// stops every listener of the control plane, on every target (#207). open must
// wait out ErrSessionHeld itself, since the session being replaced may not have
// closed.
func WithSessionReopen(open func(context.Context) (Session, error)) Option {
	return func(l *Listener) { l.reopen = open }
}

// WithDrainGrace sets when a stopping listener starts REPORTING that its drain
// is running long. It bounds nothing.
//
// Validated against maxDrainGrace rather than maxGrace, because this is the one
// budget here that waits on somebody else's job rather than on billet's own
// teardown. See defaultDrainGrace.
func WithDrainGrace(d time.Duration) Option {
	return func(l *Listener) {
		if l.setWithin("drain grace", d, maxDrainGrace) {
			l.drainGrace = d
		}
	}
}

// set validates one budget and records the outcome against its field name,
// replacing whatever the last option said about that field. It reports whether
// the value may be used.
func (l *Listener) set(field string, d time.Duration) bool {
	return l.setWithin(field, d, maxGrace)
}

// setWithin is set with an explicit ceiling, so the drain can be validated
// against maxDrainGrace while every teardown budget keeps maxGrace.
//
// A parameter rather than a second copy of this bookkeeping: the "last value
// wins, including a correction" behaviour below is subtle enough that two
// implementations of it would drift.
func (l *Listener) setWithin(field string, d, ceiling time.Duration) bool {
	err := checkGrace(field, d, ceiling)

	if err != nil {
		l.configErrs[field] = err

		return false
	}

	delete(l.configErrs, field)

	return true
}

// configError is everything still wrong with this listener's configuration.
func (l *Listener) configError() error {
	fields := make([]string, 0, len(l.configErrs))
	for field := range l.configErrs {
		fields = append(fields, field)
	}

	slices.Sort(fields)

	errs := make([]error, 0, len(fields))
	for _, field := range fields {
		errs = append(errs, l.configErrs[field])
	}

	return errors.Join(errs...)
}

// WithFinishGraces bounds the two local phases of the teardown: closing the
// session, and releasing leases.
//
// Separate from the shutdown grace because they wait on nothing remote, and
// separate from each other because a slow close must not leave the releases to
// fail on what is left of a shared budget.
func WithFinishGraces(closing, releasing time.Duration) Option {
	return func(l *Listener) {
		if l.set("close grace", closing) {
			l.closeGrace = closing
		}

		if l.set("release grace", releasing) {
			l.releaseGrace = releasing
		}
	}
}

// sumBudgets adds the teardown budgets without letting them wrap.
//
// Three valid positive durations can overflow int64 and come out NEGATIVE, which
// context.WithTimeout reads as already expired — so an absurdly long grace would
// give a watchdog that fired instantly. Saturating is the honest answer.
func sumBudgets(budgets ...time.Duration) time.Duration {
	total := time.Duration(0)

	for _, b := range budgets {
		if total > math.MaxInt64-b {
			return time.Duration(math.MaxInt64)
		}

		total += b
	}

	return total
}

// checkGrace refuses a teardown budget that cannot mean what it says.
//
// A ZERO IS AN INSTRUCTION, NOT AN OMISSION: leaving the option unset already
// selects the default, and context.WithTimeout reads an explicit zero as "already
// over", which fails the session close on its first instruction.
//
// The ceiling matters for the same reason — three durations near MaxInt64 sum to a
// NEGATIVE one. It is a PARAMETER because the drain waits on somebody else's job
// and is bounded by maxDrainGrace.
func checkGrace(name string, d, ceiling time.Duration) error {
	switch {
	case d <= 0:
		return fmt.Errorf("server: %s must be positive, got %s", name, d)
	case d > ceiling:
		return fmt.Errorf("server: %s must be at most %s, got %s", name, ceiling, d)
	}

	return nil
}

// WithCleanupRetryPacing sets how long a failed cleanup retry waits, and the ceiling
// it doubles towards.
//
// A first value of zero or less turns pacing off, which only a test wants: in
// production it lets one unreachable node occupy each pass ahead of a node that has
// just come back.
func WithCleanupRetryPacing(first, ceiling time.Duration) Option {
	return func(l *Listener) {
		l.retryFirst, l.retryMax = first, ceiling
	}
}

// WithLogger sets the logger. The default is slog.Default().
func WithLogger(log *slog.Logger) Option {
	return func(l *Listener) { l.log = log }
}

// WithMaxCapacity caps what this listener will ever advertise.
//
// Zero means advertise nothing: connect, reconcile, poll, and tell GitHub there
// is no room — which is what makes a first run against a real organization safe.
//
// A negative value is rejected rather than clamped: "advertise -1" means the
// caller computed something wrong, and turning it into 0 hides that.
func WithMaxCapacity(ceiling int) Option {
	return func(l *Listener) { l.maxCapacity = &ceiling }
}

// WithAdmissionQueue shares one control plane's admission order between its
// listeners. See admissionQueue: it decides who buys, never who advertises.
func WithAdmissionQueue(q *admissionQueue) Option {
	return func(l *Listener) { l.order = q }
}
