// Package server is billet's control plane: the per-tier scale-set listeners
// and the scheduler that turns assigned jobs into launched instances.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/state"
)

// Session is the part of a GitHub scale-set message session billet uses.
//
// billet's own interface rather than the vendor's: the scale-set client is a
// public preview whose interfaces may change, and a four-method fake is what
// makes the capacity arithmetic testable without a GitHub organization.
type Session interface {
	// Returns ErrNoMessage when the poll times out with nothing to report, which
	// is the ordinary case.
	GetMessage(ctx context.Context, lastMessageID int64, maxCapacity int) (*Message, error)
	// An unacknowledged message is redelivered, so everything derived from one
	// must be idempotent.
	DeleteMessage(ctx context.Context, messageID int64) error
	// Returns the ids actually acquired, which may be fewer than asked for.
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	// What GitHub said when the session opened, or nil. The only view of a backlog
	// that predates the session.
	Statistics() *Statistics
	Close(ctx context.Context) error
}

// Runner turns an assigned lease into running compute, and tears it down again.
// It is the seam between the control plane and a host.
//
// Both methods are called OUTSIDE the escrow mutex: launching pulls images and
// talks to a hypervisor, and holding the mutex across that would stall every
// heartbeat behind it.
type Runner interface {
	// The lease is already durable and counted against the budget, so a failure
	// here means capacity is held for something that is not running; the caller
	// releases it.
	Launch(ctx context.Context, lease *alloc.Lease, job Job) error

	// MUST be idempotent: it runs on redelivered completions, on shutdown, and on
	// paths that have already failed once.
	Destroy(ctx context.Context, requestID int64) error
}

// CompletionAwareRunner receives GitHub's authoritative completed-job result.
// It is optional so runners that have no result-dependent teardown keep the
// smaller Runner contract.
type CompletionAwareRunner interface {
	DestroyCompleted(ctx context.Context, requestID int64, result string) error
}

// Sweeper is a Runner that can also find compute nothing is asking about.
//
// Optional, and asserted for rather than required: Launch and Destroy are
// per-job, but enumerating everything a backend runs is a whole-host operation a
// node may not be able to answer during a partition.
type Sweeper interface {
	// Sweep destroys compute whose lease is no longer open. Called after each
	// reap, because reaping is what MAKES a container an orphan.
	Sweep(ctx context.Context) error

	// Tend advances compute the runner holds capacity for: heartbeating those
	// leases, letting adopted work finish, and destroying what is confirmed
	// finished. The mirror of Sweep — that finds compute no lease is holding, this
	// holds leases whose compute is unaccounted for.
	Tend(ctx context.Context) error

	// KeepAlive renews held leases until the context ends, on its OWN clock.
	// Separate from Tend because renewal must not share a schedule with anything
	// that talks to a compute backend: a slow `docker ps` would delay the next
	// renewal past the lease TTL and let the reaper reclaim capacity held on
	// purpose. Blocks until ctx is done.
	KeepAlive(ctx context.Context)
}

// ErrCustody means the runner has taken responsibility for a lease's capacity,
// so the caller must NOT release it.
//
// Returned from Launch when compute may exist that could not be confirmed gone.
// Releasing then would hand the capacity back while a container is possibly
// still running on it.
var ErrCustody = errors.New("server: the runner is holding this lease's capacity")

// errNoRunner means no compute is attached to this control plane.
var errNoRunner = errors.New("server: no runner is configured, so nothing can start this job")

// noRunner is the default, and it FAILS CLOSED.
//
// Returning an error routes the job into the ordinary failed-launch path, so the
// capacity goes back and GitHub reassigns it. Reporting success would hold the
// capacity, run nothing, and hang the job until GitHub's pickup deadline with no
// error anywhere.
type noRunner struct{ log *slog.Logger }

func (n noRunner) Launch(_ context.Context, lease *alloc.Lease, job Job) error {
	n.log.Error("no runner is configured; declining this job rather than holding capacity "+
		"for something that will never start",
		"request", job.RequestID, "run", job.RunID, "lease", lease.ID)

	return errNoRunner
}

func (noRunner) Destroy(context.Context, int64) error { return nil }

// ErrNoMessage means a long poll timed out with nothing to report — the ordinary
// outcome, not a failure.
//
// A sentinel rather than (nil, nil), which the upstream client returns: a nil
// message with a nil error is indistinguishable from "something went wrong and
// nobody said so".
var ErrNoMessage = errors.New("server: no message")

// ErrUntrustworthySession marks a scale-set response billet cannot act on.
//
// FATAL WHENEVER IT ARRIVES, including in the middle of a shutdown. Once GitHub
// returns an id nobody offered for, billet cannot tell which of its commitments
// are real — so it must stop rather than keep operating that session, and a
// cancellation happening at the same moment must not turn that into a drain.
var ErrUntrustworthySession = errors.New("server: the scale set returned something billet cannot act on")

// Message is one batch of scale-set news.
type Message struct {
	MessageID  int64
	Statistics *Statistics
	// Available is work GitHub is OFFERING. Acquiring one of these is how a
	// scale set claims it.
	Available []Job
	// Assigned is work this scale set has been given, which is the confirmation
	// that an acquisition succeeded.
	Assigned  []Job
	Completed []Job
}

// Job identifies one workflow job.
//
// RequestID is the identity that matters: it is what AcquireJobs claims work by
// and what makes a redelivered message idempotent. GitHub's own JobID is a
// separate string field, so do not conflate the two.
type Job struct {
	RequestID int64
	RunID     int64
	// Result is GitHub's conclusion on a completed-job message. It is empty on
	// available and assigned messages.
	Result string
	// The GitHub event that queued this job — "push", "pull_request", "schedule".
	// The ONLY thing in a scale-set message that says how far the workload can be
	// trusted, which decides which backends may run it.
	Event string
}

// Statistics is GitHub's own view of the scale set.
//
// TotalAssignedJobs is the ONLY field to scale on. A message carries at most 50
// job entries and a large backlog is truncated, so counting what arrived
// undercounts exactly when the undercount is most expensive.
type Statistics struct {
	TotalAvailableJobs     int
	TotalAcquiredJobs      int
	TotalAssignedJobs      int
	TotalRunningJobs       int
	TotalRegisteredRunners int
	TotalBusyRunners       int
	TotalIdleRunners       int
}

// Listener runs one tier's scale set.
type Listener struct {
	alloc   *alloc.Allocator
	tier    string
	session Session
	log     *slog.Logger

	// Guards the escrow below: renewal and the poll loop touch held and running
	// concurrently.
	mu sync.Mutex
	// Escrowed capacity not yet given to a job.
	held []*alloc.Lease
	// Escrowed capacity that HAS been given to a job, keyed by request id so a
	// redelivered message is recognised rather than assigned twice.
	//
	// Both halves are advertised — see capacity(). The safety property is that the
	// number sent to GitHub is only ever capacity this listener took from the
	// allocator, never one computed from headroom.
	running map[int64]*alloc.Lease

	// Completions whose destroy failed. Releasing while the compute may still be
	// running is the overcommit the ordering exists to prevent, and GitHub's completion
	// has been acknowledged, so nothing else will ever ask again — retrying on the
	// renewal clock is what stops "held" becoming a leak.
	//
	// Entries carry their own next-attempt time: retries are sequential and one Destroy
	// can wait the full node command timeout.
	cleanup map[int64]*pendingCleanup
	// Escrow PROMISED to a request claimed from GitHub but not yet given, keyed by that
	// request id.
	//
	// An acquisition is an OBLIGATION lasting until the Assigned message arrives, not
	// an instantaneous count: capping at len(held) lets one lease be promised to B and
	// consumed by A's assignment. Reserving under the mutex before the network call
	// also closes the race with the heartbeat.
	acquiring map[int64]*promise

	lastMessageID int64

	// maxCapacity caps what this listener advertises. nil lets the escrow decide.
	maxCapacity *int

	stalePromise time.Duration

	// Bounds the remote half of the teardown, so an unbounded Destroy cannot keep
	// Run — and the renewal that outlives it — running forever. closeGrace and
	// releaseGrace bound the two local phases after it.
	shutdownGrace time.Duration
	closeGrace    time.Duration
	releaseGrace  time.Duration

	// retryFirst <= 0 turns retry pacing off entirely, which only tests ask for.
	retryFirst time.Duration
	retryMax   time.Duration

	// Requests a cleanup retry is inside a Destroy for right now. The shutdown
	// pass skips them rather than spending its budget on work already happening.
	destroying map[int64]bool

	// Stops the cleanup loop starting anything new. Set under l.mu before the loop
	// is cancelled, so "no new attempts" and "what is in flight" are one decision.
	sealed bool

	// A Listener is single-use; see Run.
	ran bool

	// When each lease was last successfully renewed. Turns "no answer" into a
	// bounded state: past the TTL without a confirmation the reaper may already
	// have taken the lease, so advertising it is advertising someone else's
	// capacity.
	confirmed map[string]time.Time

	// What each option refused, keyed by the field it sets, so a later option
	// replaces an earlier one's error along with its value.
	configErrs map[string]error

	// Never nil; see noRunner.
	runner Runner
	// completionStore keeps authoritative job results across an ACK followed by a
	// process stop, until the node accepts result-dependent teardown.
	completionStore *state.DB

	// TotalAssignedJobs is the documented scaling signal; counting messages is
	// not, because a response carries at most 50 and a large backlog is truncated.
	observed *Statistics

	// Bounds somebody else's JOB rather than billet's own teardown, so it has its
	// own ceiling. See maxDrainGrace.
	drainGrace time.Duration
	// Guarded by mu, and NOT the same thing as sealed.
	draining bool
	// Closing it ends the drain's wait without abandoning what is held. Read
	// here, never closed here.
	hurry <-chan struct{}
}

// NewListener builds a listener for one tier.
func NewListener(a *alloc.Allocator, tier string, session Session, opts ...Option) *Listener {
	l := &Listener{
		alloc:         a,
		tier:          tier,
		session:       session,
		log:           slog.Default(),
		running:       make(map[int64]*alloc.Lease),
		acquiring:     make(map[int64]*promise),
		cleanup:       make(map[int64]*pendingCleanup),
		destroying:    make(map[int64]bool),
		configErrs:    make(map[string]error),
		confirmed:     make(map[string]time.Time),
		stalePromise:  defaultStalePromise,
		shutdownGrace: defaultShutdownGrace,
		closeGrace:    defaultCloseGrace,
		releaseGrace:  defaultReleaseGrace,
		drainGrace:    defaultDrainGrace,
		retryFirst:    firstRetryEvery,
		retryMax:      maxRetryEvery,
	}

	for _, opt := range opts {
		opt(l)
	}

	if l.runner == nil {
		l.runner = noRunner{log: l.log}
	}

	return l
}

// WithRunner sets what turns assigned leases into running compute.
//
// THE DEFAULT DECLINES THE JOB, which is the opposite of what this comment used to
// claim. noRunner fails closed: it returns an error, the ordinary failed-launch
// path hands the capacity back, and GitHub reassigns. It does not hold capacity
// and it does not quietly succeed — see noRunner's own documentation, which said
// so correctly while this said the reverse.
func WithRunner(r Runner) Option {
	return func(l *Listener) { l.runner = r }
}

// WithCompletionStore makes result delivery and its capacity settlement durable
// across listener restarts.
func WithCompletionStore(db *state.DB) Option {
	return func(l *Listener) { l.completionStore = db }
}

// promise is escrow held for a request GitHub has been told billet will run, and
// when that undertaking was made.
//
// `at` IS DIAGNOSTIC ONLY — it drives one stale-promise warning and must not time
// the promise out; see defaultStalePromise.
type promise struct {
	lease *alloc.Lease
	at    time.Time
	// reported keeps a stale promise from logging on every heartbeat.
	reported bool
}

// pendingCleanup is a completion whose destroy has not succeeded yet.
type pendingCleanup struct {
	job Job
	// lease is the capacity this obligation still holds, when the entry was
	// created for a lease that is NOT in `running` — a launch that failed and
	// whose release did not land. complete looks here when the running map has
	// nothing, because the request id has already been given up.
	lease *alloc.Lease
	// outcome is how the lease should be archived, or empty for the ordinary
	// completion. A launch that never started must not be recorded as `done`:
	// "done" for a runner that never ran is a lie the history keeps.
	outcome alloc.Phase
	// releaseOnly means the runner has already proved there is no compute to
	// destroy: either Launch failed without custody, or a previous Destroy
	// succeeded. Retrying a remote destroy in either case can only delay the
	// ledger release, and during shutdown that delay can be a full node timeout.
	releaseOnly bool
	// Doubles after each failure, up to maxRetryEvery.
	wait time.Duration
	// Zero means immediately, which is what a freshly recorded failure wants.
	at time.Time
	// GitHub re-offers an unacquired job indefinitely, so the message is worth
	// exactly once per obligation.
	declined bool
}

// due reports whether this entry may be attempted at the given moment.
func (p *pendingCleanup) due(now time.Time) bool {
	return !now.Before(p.at)
}

// failed pushes the next attempt out, doubling the wait to a ceiling.
//
// The FIRST failure is what created the entry, so the first retry is immediate:
// the overwhelmingly common case is a node that was briefly busy, and making it
// wait would slow down every ordinary recovery to protect against the rare
// permanent one.
func (p *pendingCleanup) failed(now time.Time, first, ceiling time.Duration) {
	if first <= 0 {
		// Pacing off, for tests whose subject is what a retry does rather than when.
		p.wait, p.at = 0, time.Time{}

		return
	}

	switch {
	case p.wait == 0:
		p.wait = first
	case p.wait < ceiling:
		p.wait *= 2
	}

	if p.wait > ceiling {
		p.wait = ceiling
	}

	p.at = now.Add(p.wait)
}

// defaultStalePromise is how long a promise may go unclaimed before billet warns.
// DIAGNOSTIC, not a deadline: an acquisition is a commitment to GitHub that no
// local timer can revoke, so a timed release would only mean billet had forgotten
// it owes a runner while GitHub still expects one.
const defaultStalePromise = 5 * time.Minute

const (
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

	// How long the listener keeps polling for completions after being asked to stop.
	//
	// SIX HOURS BECAUSE THAT IS THE LENGTH OF A JOB, not of a shutdown: GitHub's
	// timeout-minutes defaults to 360. Every other budget here bounds work BILLET is
	// doing. Overrunning it is not a failure state.
	defaultDrainGrace = 6 * time.Hour

	// Separate from maxGrace: reusing the teardown's one-hour ceiling would refuse
	// every honest value for a fleet whose jobs run longer than an hour. A day is
	// still a ceiling — past this a typo is likelier than the intent.
	maxDrainGrace = 24 * time.Hour

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
// The drain is bounded by drainGrace, which is measured in hours because a job
// is. An operator who does not want to wait that long needs a lever that stops
// the WAITING without stopping the teardown; what follows is the ordinary
// destroy-and-release. Without it the only escape is killing the process, which
// strands exactly the containers the drain existed to protect.
func WithHurrySignal(c <-chan struct{}) Option {
	return func(l *Listener) { l.hurry = c }
}

// WithDrainGrace bounds how long a stopping listener waits for the jobs it is
// already running to finish before it destroys them.
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

// Run polls until the context is done.
//
// The order of operations is the design: capacity is escrowed BEFORE it is
// advertised, and only what the escrow actually returned is advertised.
//
// The other way round — advertise what this tier could theoretically take,
// reserve when GitHub assigns — over-admits by construction on any host with
// more than one tier: each listener computes a maximum from the same free pool,
// GitHub fills all of them at once, and reserving on assignment is too late.
//
// The vendor's own listener package computes a desired runner count itself,
// which is why billet does not use it.
func (l *Listener) Run(ctx context.Context) error {
	// REFUSED RATHER THAN CORRECTED: these budgets decide when billet stops
	// protecting capacity whose compute may still be running.
	if err := l.configError(); err != nil {
		return fmt.Errorf("server: listener for %s is misconfigured: %w", l.tier, err)
	}

	// SINGLE USE. Shutdown seals the cleanup loop permanently and closes the
	// session, so a second Run polls a closed session and its retries all return
	// "sealed" — which retryCleanup reads as success, so it neither retries nor
	// backs off nor complains, and every failed destroy from then on is silently
	// abandoned. Server builds a fresh listener per tier run.
	l.mu.Lock()
	reused := l.ran
	l.ran = true
	l.mu.Unlock()

	if reused {
		return fmt.Errorf("server: listener for %s has already run; build a new one", l.tier)
	}
	if err := l.restoreCompletions(ctx); err != nil {
		return err
	}

	// Heartbeats run on their OWN clock, not between polls. A long poll measured
	// ~88 seconds against a 90 second TTL on a real organization, and the vendor's
	// HTTP client permits far longer — so tying renewal to the poll cadence would
	// make the whole escrow depend on a timeout billet does not control.
	//
	// AND IT DOES NOT INHERIT THE CALLER'S CANCELLATION, which is what makes the
	// ordering below matter: renewal must outlive the session close, the release,
	// and every slow remote destroy the release performs. It ends after them.
	beat, stopBeating := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBeating()

	// SEPARATE LIFETIMES, because shutdown needs them in opposite orders: the
	// cleanup loop must finish before the release runs, and renewal must still be
	// running while it does.
	//
	// It does not inherit the caller's cancellation either, for the drain's sake —
	// a failed destroy has to keep being retried for as long as a job runs. The
	// teardown stops this loop explicitly.
	sweep, stopSweeping := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSweeping()

	var beating, sweeping sync.WaitGroup

	beating.Add(1)

	go func() {
		defer beating.Done()

		l.heartbeatLoop(beat)
	}()

	// A SEPARATE LOOP, NOT A STEP IN THE HEARTBEAT. A destroy can wait the full
	// command timeout, and hanging that off the heartbeat's tick would let one
	// unreachable host delay every renewal and expire the leases it was
	// protecting.
	sweeping.Add(1)

	go func() {
		defer sweeping.Done()

		l.cleanupLoop(sweep)
	}()

	// CLOSE THEN RELEASE, in that order, and the listener owns both so the order
	// cannot be split across two functions. The last maxCapacity GitHub saw stays
	// live until the session ends, so releasing escrow first leaves a positive
	// advertisement standing with nothing behind it.
	defer func() {
		// BOUNDED, because renewal continues after the caller cancels and neither Runner
		// nor Session promises to honour a context.
		//
		// ONE DEADLINE THAT EVERY PHASE INHERITS, not a sum they can outlive: renewal has
		// to outlast the whole teardown, and each phase is min(its own budget, what is
		// left).
		overall, endOverall := context.WithTimeout(context.WithoutCancel(ctx),
			l.teardownBudget())
		defer endOverall()

		renewCtx := overall

		// RENEWAL STOPS LAST, so it is deferred first. The release below destroys
		// whatever is still running, which is slow and remote, and every lease it
		// has not reached yet has to keep being renewed while it works.
		defer func() {
			stopBeating()
			beating.Wait()
		}()

		// AND RENEWAL STOPS ON THE GRACE EVEN IF THE TEARDOWN NEVER FINISHES. A wedged
		// listener that went on renewing would stop the reaper reclaiming; it leaks a
		// goroutine and says so, and the ledger recovers without it.
		//
		// Decided on `guard` alone rather than on whichever channel select picks, since
		// both are ready after a healthy teardown and select chooses uniformly.
		guard := make(chan struct{})

		var watching sync.WaitGroup

		watching.Add(1)

		go func() {
			defer watching.Done()

			select {
			case <-guard:
				return
			case <-renewCtx.Done():
			}

			select {
			case <-guard:
				// The teardown finished; the grace expiring afterwards means nothing.
				return
			default:
			}

			// THE COMPONENTS, not just the total: an operator reading "budget=13m"
			// cannot tell which of the three to change.
			l.log.Error("this listener's teardown outran its whole shutdown budget; renewal "+
				"is stopping so the reaper can reclaim what it still holds, but the compute "+
				"it was destroying may still be running on its host",
				"tier", l.tier,
				"destroy_grace", l.shutdownGrace,
				"close_grace", l.closeGrace,
				"release_grace", l.releaseGrace)

			stopBeating()
		}()

		defer func() {
			close(guard)
			watching.Wait()
		}()

		// CLEANUP IS STOPPED AND JOINED BEFORE ANY OF IT: a retry blocked in a remote
		// Destroy outlives Run and comes back to call alloc.Release against a database the
		// caller had every right to have closed.
		//
		// SEALED FIRST, then cancelled — cancelling says nothing about what the loop is
		// midway through starting.
		l.seal()
		stopSweeping()

		// ITS OWN PHASE, because this waits for a retry that is inside a Destroy and
		// so can take exactly as long as one. Sharing the destroy phase's budget
		// would let a stalled retry leave destroyAll with an already-dead context.
		joinCtx, endJoin := context.WithTimeout(overall, l.shutdownGrace)
		defer endJoin()

		if !waitWithin(joinCtx, &sweeping) {
			l.log.Error("a cleanup retry did not return within its shutdown budget; it may "+
				"still release a lease after this listener has stopped",
				"tier", l.tier, "grace", l.shutdownGrace)
		}

		// AND THE DESTROY BUDGET STARTS HERE, after the join rather than beside it.
		stopCtx, endGrace := context.WithTimeout(overall, l.shutdownGrace)
		defer endGrace()

		// ONE DESTROY PASS FOR EVERYTHING, before the session closes, so the release only
		// ever releases.
		//
		// No budget check: the join's context is min(shutdownGrace, what is left), so this
		// cannot be reached with the budget spent. A guard here could never fire.
		destroyed := l.destroyAll(stopCtx)

		// A FRESH BUDGET FOR THE LOCAL HALF, and one EACH. Sharing the destroy
		// pass's deadline would hand the close an already-expired context after a
		// slow destroy, skipping releaseAll; sharing one budget between close and
		// release lets a slow close starve the releases.
		closeCtx, endClose := context.WithTimeout(overall, l.closeGrace)
		defer endClose()

		// WHOSE FAULT, before the failure is attributed: a phase entered with an expired
		// budget fails without being attempted, and reporting that as "could not close
		// message session" blames the session for a deadline the destroys spent.
		//
		// STOPPED rather than reported, and reachable — Session does not promise to honour
		// an already-expired context either.
		if err := overall.Err(); err != nil {
			l.log.Error("the shutdown budget was gone before billet closed its session; "+
				"the capacity this listener holds is left for the reaper",
				"tier", l.tier, "budget", l.teardownBudget())

			return
		}

		if err := l.session.Close(closeCtx); err != nil {
			l.log.Warn("could not close message session; capacity is held until it expires",
				"tier", l.tier, "error", err)

			// NOT released. A session billet could not close may still be handing
			// this scale set work, and handing the capacity back would let another
			// tier escrow it while GitHub believes this one still has room. The
			// reaper expiring the lease is the safe way out.
			return
		}

		releaseCtx, endRelease := context.WithTimeout(overall, l.releaseGrace)
		defer endRelease()

		l.releaseAll(releaseCtx, destroyed)
	}()

	// A restart does not replay messages for work already assigned, so a listener
	// that waits to be told about a backlog sits idle in front of one.
	l.observed = l.session.Statistics()
	l.reportOrphanedBacklog()

	// THE DRAIN IS A STATE OF THIS LOOP, NOT A PHASE OF THE TEARDOWN. There ctx is
	// already cancelled and the long poll dead, so the listener could not be TOLD a job
	// finished: it would wait for news that cannot arrive and destroy the jobs anyway.
	//
	// So cancellation stops it taking NEW work and nothing else.
	pollCtx := ctx

	var (
		draining bool
		endDrain context.CancelFunc
	)

	defer func() {
		if endDrain != nil {
			endDrain()
		}
	}()

	for {
		// CHECKED HERE AND AFTER EVERY CALL, because the cancellation almost always
		// lands DURING one rather than between two: the listener spends nearly all
		// its life inside a long poll.
		if !draining && ctx.Err() != nil {
			draining = true
			// The budget starts at the cancellation, so this cannot be hoisted above
			// the loop; `draining` makes it a once-per-Run assignment.
			//nolint:fatcontext // Assigned at most once; see the comment above.
			pollCtx, endDrain = l.beginDrain(ctx)
		}

		if draining {
			if l.drained() {
				l.log.Info("everything running here has finished; stopping", "tier", l.tier)

				return ctx.Err()
			}

			if pollCtx.Err() != nil {
				// The drain's budget is gone, or a second signal cut it short, so
				// what is still running becomes the teardown's problem.
				//
				// NOT "GitHub will reassign them": reassignment is documented for a
				// job never acquired by a runner, not one a runner has already
				// started. Destroying a running container FAILS somebody's build.
				l.log.Warn("giving up on the jobs still running here; destroying them "+
					"will FAIL them, and GitHub does not requeue a job whose runner "+
					"vanished mid-execution",
					"tier", l.tier, "running", l.Running(), "grace", l.drainGrace)

				return ctx.Err()
			}
		}

		// BEFORE TOPPING UP, so the number this poll advertises already reflects a
		// machine that has gone.
		//
		// NOT WHILE DRAINING: the drain has just handed back the capacity nobody
		// was using, and topping it up again would re-advertise the tier it is
		// trying to leave, so the drain could never reach zero.
		if !draining {
			l.releaseStrandedEscrow(pollCtx)
		}

		if !draining {
			if err := l.refillEscrow(pollCtx); err != nil {
				if cancelledWhileServing(ctx, draining, err) {
					continue
				}

				return stopping(ctx, err)
			}
		}

		msg, err := l.session.GetMessage(pollCtx, l.lastMessageID, l.capacity())

		// A timed-out long poll is the ordinary case. Poll again immediately —
		// the escrow is KEPT, because releasing and retaking it every poll
		// would hand the gap to another tier and produce exactly the flapping the
		// escrow exists to avoid.
		if errors.Is(err, ErrNoMessage) {
			continue
		}

		if err != nil {
			if cancelledWhileServing(ctx, draining, err) {
				continue
			}

			return stopping(ctx, fmt.Errorf("server: poll %s: %w", l.tier, err))
		}

		if err := l.handle(pollCtx, msg); err != nil {
			if cancelledWhileServing(ctx, draining, err) {
				continue
			}

			return stopping(ctx, err)
		}
	}
}

// cancelledWhileServing reports whether a failed call is the shutdown arriving
// mid-call rather than a failure to report.
//
// Neither obvious test works. The clock alone swallows fatal errors that coincide
// with a cancellation; context.Canceled alone misses one landing inside handle(),
// which surfaces as a domain error that does not wrap it — so the drain is skipped
// intermittently for the most ordinary reason there is.
//
// So: once stopping, an error IS the shutdown unless it is one billet must stop for
// regardless. A scale-set response it cannot act on is the only one.
func cancelledWhileServing(ctx context.Context, draining bool, err error) bool {
	if draining || ctx.Err() == nil {
		return false
	}

	return !errors.Is(err, ErrUntrustworthySession)
}

// beginDrain stops this listener taking new work and hands back the capacity nobody
// is using, returning the context the drain polls on.
//
// THE ADVERTISEMENT IS NOT FORCED TO ZERO: what billet sends is the scale set's
// TOTAL capacity, so a constant zero while a job runs is untrue. Releasing the idle
// escrow makes capacity() fall to the work still in flight and reach zero by itself.
//
// It is a courtesy to GitHub's scheduler and NOT the guard — a redelivered message
// can still arrive, and the local check in handle is what refuses it.
func (l *Listener) beginDrain(ctx context.Context) (context.Context, context.CancelFunc) {
	// BOUNDED only as far as anything here can be: the deadline is observed between
	// calls, so a Session that blocks inside GetMessage forever is not bounded by it.
	//
	// Built FIRST because the release below needs a live context — ctx is already
	// cancelled, and alloc.Release would fail on it and strand the capacity.
	drainCtx, endDrain := context.WithTimeout(context.WithoutCancel(ctx), l.drainGrace)

	// A SECOND SIGNAL ENDS THE WAIT, not the teardown. The goroutine also selects
	// on drainCtx so it cannot outlive the drain.
	if l.hurry != nil {
		go func() {
			select {
			case <-l.hurry:
				endDrain()
			case <-drainCtx.Done():
			}
		}()
	}

	// NOT l.seal(), which stops the cleanup loop starting new destroys and belongs
	// at the teardown. A drain can last as long as a job, so sealing here would
	// leave every failed destroy un-retried for hours.
	//
	// MARKED BEFORE THE ESCROW GOES BACK: the other order leaves a window where
	// the capacity is released but an offer can still be accepted.
	l.mu.Lock()
	l.draining = true
	l.mu.Unlock()

	released := l.releaseIdleEscrow(drainCtx)

	l.log.Info("draining: not taking new work, waiting for what is already running",
		"tier", l.tier, "running", l.Running(), "released_idle_leases", released,
		"grace", l.drainGrace)

	return drainCtx, endDrain
}

// releaseStrandedEscrow hands back capacity whose machine has gone away.
//
// refillEscrow only ever ADDS, so when the host a reservation names disappears the
// promise does not move: GitHub goes on assigning against it and every job is
// acquired and then fails to launch.
//
// ONLY `held` LEASES: one has never been assigned, so giving it back costs a
// re-escrow at worst. Anything acquiring or running is somebody's job.
func (l *Listener) releaseStrandedEscrow(ctx context.Context) int {
	l.mu.Lock()
	snapshot := append([]*alloc.Lease(nil), l.held...)
	l.mu.Unlock()

	if len(snapshot) == 0 {
		return 0
	}

	ids := make([]string, 0, len(snapshot))
	for _, lease := range snapshot {
		ids = append(ids, lease.ID)
	}

	stranded, err := l.alloc.Stranded(ctx, ids)
	if err != nil {
		// NOT FATAL. Not knowing whether a machine is still there is a reason to
		// keep advertising what was already promised, not to tear it down: the
		// ledger will answer on the next pass, and releasing on a failed read
		// would hand back capacity over a database blip.
		l.log.Warn("could not check whether this tier's escrow still has machines behind it",
			"tier", l.tier, "error", err)

		return 0
	}

	if len(stranded) == 0 {
		return 0
	}

	gone := make(map[string]bool, len(stranded))
	for _, id := range stranded {
		gone[id] = true
	}

	released := 0

	for _, lease := range snapshot {
		if !gone[lease.ID] {
			continue
		}

		// PhaseDone, not PhaseFailed: nothing was attempted and nothing went
		// wrong. The reservation is simply being given back.
		if err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			// LEFT IN `held`, so it stays renewed and correctly counted as this
			// listener's, and is tried again on the next pass. Dropping it here
			// would leak the capacity until the reaper.
			l.log.Warn("could not release escrow whose machine is gone; it stays this "+
				"listener's and will be tried again",
				"tier", l.tier, "lease", lease.ID, "error", err)

			continue
		}

		l.mu.Lock()
		l.held = slices.DeleteFunc(l.held, func(h *alloc.Lease) bool { return h.ID == lease.ID })
		l.mu.Unlock()

		released++
	}

	if released > 0 {
		l.log.Info("released escrow whose machines are no longer in the fleet; this tier now "+
			"advertises less", "tier", l.tier, "released", released)
	}

	return released
}

// releaseIdleEscrow hands back every lease this listener holds but has not given
// to a job, reporting how many.
//
// Only `held`. `acquiring` has been promised to a job that is starting and
// `running` is backing a live container; releasing either would let another tier
// escrow capacity that is already spoken for.
func (l *Listener) releaseIdleEscrow(ctx context.Context) int {
	// A SNAPSHOT TO ITERATE, BUT EACH LEASE LEAVES `held` ONLY WHEN IT IS ACTUALLY
	// RELEASED. Out of `held` the heartbeat cannot see it, so a pass longer than a TTL
	// lets the reaper reclaim one, and appending it back would advertise capacity this
	// listener no longer owns. Deleting by id also avoids resurrecting a lease
	// heartbeatHeld just dropped.
	l.mu.Lock()
	snapshot := append([]*alloc.Lease(nil), l.held...)
	l.mu.Unlock()

	released := 0

	for _, lease := range snapshot {
		if err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			// LEFT IN `held`, so it is still renewed, still correctly counted as
			// this listener's, and tried again by the teardown's own release pass
			// on its own budget. Dropping it here would leak the capacity until the
			// reaper; advertising it after losing it would be worse.
			l.log.Warn("could not release idle escrow while draining; it stays this "+
				"listener's and will be released at shutdown",
				"tier", l.tier, "lease", lease.ID, "error", err)

			continue
		}

		l.mu.Lock()
		l.held = slices.DeleteFunc(l.held, func(h *alloc.Lease) bool { return h.ID == lease.ID })
		l.mu.Unlock()

		released++
	}

	return released
}

// isDraining reports whether this listener has been asked to stop and is
// waiting out the work it already has.
//
// Distinct from `sealed`, which stops the cleanup loop at teardown. The two are
// hours apart in the life of a drain and mean different things.
func (l *Listener) isDraining() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.draining
}

// drained reports whether this listener still owes anybody a running job.
//
// RUNNING ONLY, AND `acquiring` DELIBERATELY NOT: GitHub may never assign a promise,
// and what resolves one is the session ending — which happens in the teardown, on
// the far side of this wait. A drain waiting for it would spend its whole budget
// and destroy nothing.
func (l *Listener) drained() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.running) == 0
}

// capacity is what this listener advertises: TOTAL escrowed, not free.
//
// All three collections count, and each lease came from the allocator, so the sum
// across listeners is still bounded by the budget. Sending only the free half would
// shrink the advertisement every time a job started.
func (l *Listener) capacity() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.held) + len(l.acquiring) + len(l.running)
}

// Held returns the leases this listener has escrowed and not yet handed to a job.
//
// Exported for tests, which need lease IDENTITY rather than a count: an escrow
// that was lost and rebuilt has the same size and different ids.
func (l *Listener) Held() []*alloc.Lease {
	l.mu.Lock()
	defer l.mu.Unlock()

	return append([]*alloc.Lease(nil), l.held...)
}

// Acquiring reports how many offers this listener has escrow promised to and has
// not yet been assigned. Exported for tests, which cannot read the guarded field
// safely.
func (l *Listener) Acquiring() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.acquiring)
}

// Running reports how many jobs this listener currently has leases for. Exported
// for tests, which cannot read the guarded field safely.
func (l *Listener) Running() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.running)
}

// Backlog is what GitHub last said was assigned to this scale set and not yet
// finished.
//
// TotalAssignedJobs is the documented scaling signal; counting messages is not,
// because a response carries at most 50 and a large backlog is truncated.
func (l *Listener) Backlog() int {
	if l.observed == nil {
		return 0
	}

	return l.observed.TotalAssignedJobs
}

// reportOrphanedBacklog says out loud when GitHub believes this scale set is already
// running work billet has no lease for.
//
// A fresh listener holds nothing, so a non-zero TotalAssignedJobs means jobs were
// assigned before the process restarted. The ones still waiting are reassigned at
// the pickup deadline; the ones a dead runner had STARTED are not, and fail.
//
// Deliberately NOT a failure — those jobs are already lost, and refusing to start
// would strand the tier's remaining capacity too.
func (l *Listener) reportOrphanedBacklog() {
	backlog := l.Backlog()
	if backlog == 0 {
		return
	}

	l.log.Warn("github reports jobs already assigned to this scale set that billet has no lease "+
		"for; they were assigned before this process started. Any that no runner had picked up "+
		"are reassigned when github's pickup deadline passes; any that were already running "+
		"have lost their runner and will fail",
		"tier", l.tier, "assigned", backlog)
}

// stopping reports a shutdown as a shutdown.
//
// Cancelling the context does not produce a context error from everything it
// interrupts: SQLite surfaces an in-flight statement as "interrupted (9)", and an
// HTTP client can report a closed connection. The context is the authority — if
// it is done, that is the reason, whatever the layer underneath said.
func stopping(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return err
}

// heartbeatLoop renews this listener's leases until the context ends.
//
// The interval is a fraction of the TTL so a single missed beat — a busy
// database, a slow write — does not expire anything.
func (l *Listener) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(l.heartbeatInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// EACH PASS IS BOUNDED here rather than by the caller: this context does not inherit
			// the caller's cancellation, so without its own deadline an allocator call that
			// never returns holds l.mu forever, and the teardown blocks on that mutex behind
			// the defer that would have stopped this loop.
			pass, endPass := context.WithTimeout(ctx, l.heartbeatInterval())

			l.mu.Lock()
			l.heartbeatHeld(pass)
			l.mu.Unlock()

			endPass()
		}
	}
}

// cleanupLoop retries cleanup obligations on its own clock.
func (l *Listener) cleanupLoop(ctx context.Context) {
	ticker := time.NewTicker(l.heartbeatInterval())
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.retryCleanup(ctx)
		}
	}
}

// retryCleanup finishes cleanup obligations whose destroy or release failed.
//
// A release-only obligation never reaches the runner again: its entry records
// the proof that no compute exists. Every other obligation goes through complete,
// which refuses to release until Destroy confirms the compute is gone.
func (l *Listener) retryCleanup(ctx context.Context) {
	now := time.Now()

	l.mu.Lock()

	// ONLY THE ONES THAT ARE DUE: retries are sequential and a single Destroy can
	// wait the full node command timeout, so entries whose node has been refusing for
	// an hour would push the one that just recovered behind them.
	pending := make([]Job, 0, len(l.cleanup))

	for _, entry := range l.cleanup {
		if entry.due(now) {
			pending = append(pending, entry.job)
		}
	}

	l.mu.Unlock()

	// NO ERROR BRANCH, because attempt has none to give. A failure records its
	// own obligation and its own backoff before returning — the pacing lives with
	// the knowledge of what failed, rather than in a caller that would have to be
	// told.
	for _, job := range pending {
		l.attempt(ctx, job)
	}
}

// teardownBudget is how long the whole shutdown may take: the cleanup-loop join
// and the destroy pass each get the destroy budget, then the close and the
// release get theirs.
//
// Every phase derives from a deadline this far out, so each of them is min(its
// own budget, what is left) and none can outlive the renewal that protects them.
func (l *Listener) teardownBudget() time.Duration {
	return sumBudgets(l.shutdownGrace, l.shutdownGrace, l.closeGrace, l.releaseGrace)
}

// waitWithin waits for a WaitGroup, giving up when the context is done.
//
// Reports whether the wait completed. The watcher goroutine deliberately outlives
// a giving-up caller: the thing being waited for is by definition misbehaving.
func waitWithin(ctx context.Context, wg *sync.WaitGroup) bool {
	done := make(chan struct{})

	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// destroyAll tears down every piece of compute this listener is responsible for and
// reports which requests are confirmed gone.
//
// THE UNION OF running AND cleanup, each destroyed exactly once: the sets overlap,
// and idempotence makes a second call safe but not free.
//
// Concurrent, because each Destroy can wait the node command timeout. Bounded,
// because a node executes commands one at a time. The backoff is ignored — it
// exists so a hopeless record cannot crowd out a live one, and this is the last
// pass.
func (l *Listener) destroyAll(ctx context.Context) map[int64]bool {
	l.mu.Lock()

	requests := make([]Job, 0, len(l.running)+len(l.cleanup))

	// NOT WHAT A RETRY IS ALREADY INSIDE. That destroy is still happening, and a
	// second one would win the single teardown slot and spend the budget on work
	// already in progress while unrelated requests are never reached.
	skipped := make([]int64, 0, len(l.destroying))

	for id := range l.running {
		if l.destroying[id] {
			skipped = append(skipped, id)

			continue
		}

		job := Job{RequestID: id}
		if entry := l.cleanup[id]; entry != nil && entry.job.Result != "" {
			job = entry.job
		}
		requests = append(requests, job)
	}

	for id, entry := range l.cleanup {
		if entry.releaseOnly {
			continue
		}

		if l.destroying[id] {
			if _, running := l.running[id]; !running {
				skipped = append(skipped, id)
			}

			continue
		}

		if _, running := l.running[id]; !running {
			requests = append(requests, entry.job)
		}
	}

	l.mu.Unlock()

	for _, id := range skipped {
		l.log.Warn("not destroying this job's compute during shutdown because a cleanup "+
			"retry is still inside a destroy for it; that attempt is the one that counts, "+
			"and it did not return within its own budget",
			"tier", l.tier, "request", id)
	}

	var (
		mu   sync.Mutex
		done = make(map[int64]bool, len(requests))
		wg   sync.WaitGroup
		slot = make(chan struct{}, teardownConcurrency)
	)

	for _, job := range requests {
		wg.Add(1)

		go func(job Job) {
			defer wg.Done()
			requestID := job.RequestID

			// CHECKED BEFORE THE SELECT, because select picks uniformly among ready
			// cases rather than preferring a ready cancellation over a ready slot: with
			// an expired budget and a free slot, roughly half these goroutines would go
			// on to call Destroy anyway.
			if ctx.Err() == nil {
				select {
				case slot <- struct{}{}:
					defer func() { <-slot }()
				case <-ctx.Done():
				}
			}

			if ctx.Err() != nil {
				// NAMED, not silently skipped: returning quietly makes a request that was
				// never attempted indistinguishable from one destroyed, and a cleanup-only
				// record has no lease either, so the obligation evaporates on an ordinary
				// shutdown.
				l.log.Error("the shutdown grace ran out before billet tried to destroy this "+
					"job's compute; it was never attempted, and if no lease accounts for it "+
					"nothing will reclaim it until its host is swept or restarted",
					"tier", l.tier, "request", requestID)

				return
			}

			err := l.destroyCompleted(ctx, job)

			// CUSTODY DISCHARGES THE OBLIGATION RATHER THAN FAILING IT (#46).
			//
			// The node asked its backend to stop the guest without receiving proof it
			// stopped, and is holding the lease until the compute is provably gone.
			// Nothing here can improve on that: retrying re-issues a teardown whose
			// outcome is already being reconciled, and keeping the entry — lease
			// and all — has this listener releasing capacity the node's janitor is
			// about to release itself.
			//
			// Counted as done for exactly that reason. It is not a confirmed
			// destroy, but it IS a request that no longer needs anything from this
			// listener, which is what the caller reads this map for.
			held := errors.Is(err, ErrCustody)
			if held {
				l.forgetCompletion(ctx, job)
			}

			mu.Lock()
			done[requestID] = err == nil || held
			mu.Unlock()

			if held {
				// AND THE LEASE LEAVES `running`, WHICH IS THE HALF THAT MATTERS AT
				// SHUTDOWN.
				//
				// releaseAll releases every lease still in `running` whose request
				// this map marks destroyed — so marking a custody answer "done" and
				// leaving the lease behind would have shutdown release the very
				// capacity the node just took responsibility for, seconds after the
				// handoff. It has to be both or neither, and dropping it is correct:
				// the node's janitor holds this lease now, heartbeats it, and
				// releases it once the guest is provably gone. That janitor outlives
				// this listener.
				l.log.Info("the compute for this job was asked to stop and has not been "+
					"confirmed gone; the runner is holding its capacity until it is",
					"tier", l.tier, "request", requestID)

				l.mu.Lock()
				delete(l.running, requestID)
				delete(l.cleanup, requestID)
				l.mu.Unlock()

				return
			}

			if err != nil {
				l.log.Error("could not destroy the compute for a job before stopping; it is "+
					"still running on its host, and if no lease accounts for it nothing will "+
					"reclaim it until that host is swept or restarted",
					"tier", l.tier, "request", requestID, "error", err)

				return
			}

			l.mu.Lock()

			// AN ENTRY CARRYING A LEASE IS NOT DISCHARGED BY THE DESTROY ALONE.
			//
			// Most cleanup entries exist only to destroy compute, so confirming
			// that is the end of them. One parked by a failed launch also holds
			// CAPACITY whose release never landed, and deleting it here dropped the
			// last reference before releaseAll could see it — leaving the ledger
			// charging for a job that never started.
			if entry, ok := l.cleanup[requestID]; !ok || entry.lease == nil {
				delete(l.cleanup, requestID)
			}

			l.mu.Unlock()
		}(job)
	}

	wg.Wait()

	return done
}

// attempt runs one cleanup retry, marked as in flight for its duration.
//
// THE UNMARKING IS A DEFER, which is the whole reason this is a function. The
// mark makes the shutdown skip a request, so an entry that outlives its attempt
// hides that request from teardown permanently — a container nobody destroys and
// nobody mentions. A panic under complete would do it.
func (l *Listener) attempt(ctx context.Context, job Job) {
	l.mu.Lock()

	// CLAIMED UNDER THE SAME LOCK THAT SEALS, which is what makes the shutdown's
	// snapshot trustworthy. retryCleanup walks a snapshot and cancellation does not
	// stop it mid-list, so with only a context check the loop could finish a slow
	// destroy and start a brand new one for the next job while the teardown was
	// destroying it too. Checking a flag and taking the mark in two steps leaves the
	// same window one instruction wide.
	if l.sealed {
		l.mu.Unlock()

		return
	}

	entry, pending := l.cleanup[job.RequestID]
	if !pending {
		l.mu.Unlock()

		return
	}

	if entry.releaseOnly {
		l.mu.Unlock()
		l.releaseParked(ctx, job.RequestID)

		return
	}

	l.destroying[job.RequestID] = true
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		delete(l.destroying, job.RequestID)
		l.mu.Unlock()
	}()

	l.complete(ctx, job)
}

// seal stops the cleanup loop from starting any further destroys.
//
// Called BEFORE the loop is cancelled, because cancelling is a request and this is a
// fact. PERMANENT, and only safe because a listener is never reused.
func (l *Listener) seal() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sealed = true
}

// heartbeatInterval is how often held capacity is renewed: a third of the
// allocator's ACTUAL TTL, so two consecutive failures are survivable. Read from the
// ALLOCATOR, because with a shorter configured TTL a cadence derived from the
// default lets every lease expire between beats.
func (l *Listener) heartbeatInterval() time.Duration {
	if ttl := l.alloc.LeaseTTL(); ttl > 0 {
		return ttl / 3
	}

	return alloc.DefaultLeaseTTL / 3
}

// renewal is what a heartbeat established about one lease.
//
// FOUR OUTCOMES, NOT TWO. "Not renewable" would cover two different facts — the
// allocator SAYING the lease is not ours, and the allocator not answering at all
// — and only the first is evidence about who owns the compute.
type renewal int

const (
	// renewalOwned: the allocator confirmed the lease is still this listener's.
	renewalOwned renewal = iota
	// renewalLost: the allocator says it is not — fenced, or gone from the ledger.
	renewalLost
	// renewalUnknown: no answer, and not for long enough to matter yet.
	renewalUnknown
	// renewalStale: no answer for longer than a lease can survive, so the reaper
	// may already have taken it. Not evidence that it is lost, but no longer a
	// reason to advertise it.
	renewalStale
)

// advertisable reports whether a lease with this outcome may still be counted as
// capacity.
func (r renewal) advertisable() bool {
	return r == renewalOwned || r == renewalUnknown
}

// heartbeatHeld renews the leases this listener is advertising, and drops any it has
// lost.
//
// This is what makes the reaper safe to run at all: a lease expires after 90 seconds
// without a heartbeat while a long poll blocks for about 50, so escrow held across
// two polls would be reclaimed underneath a listener still advertising it.
//
// A lease that cannot be renewed is DROPPED rather than retried — failure means the
// allocator no longer agrees this listener owns it.
func (l *Listener) heartbeatHeld(ctx context.Context) {
	kept := l.held[:0]

	for _, lease := range l.held {
		// Escrow only: nothing has been launched against it, so dropping one owes
		// nobody anything. The ledger entry is left to the reaper.
		if l.renew(ctx, lease).advertisable() {
			kept = append(kept, lease)
		}
	}

	l.held = kept

	// RUNNING and ACQUIRING leases are renewed too. They are open in the ledger
	// exactly like held ones, so a lease whose job is in flight — or whose job
	// billet has promised to run — expires just as readily, and its capacity would
	// then be escrowed by another tier while GitHub still believes this scale set
	// has the job.
	for id, lease := range l.running {
		if l.renew(ctx, lease).advertisable() {
			continue
		}

		// THE LEASE GOES; THE OBLIGATION DOES NOT. This listener launched a container and
		// losing the ledger entry does not stop it running; GitHub will not send the
		// completion again. So the entry moves to the cleanup set, where only a successful
		// destroy discharges it — deleting it leaves the container reachable by nothing but
		// an optional Sweeper.
		delete(l.running, id)
		delete(l.confirmed, lease.ID)

		if _, pending := l.cleanup[id]; !pending {
			l.cleanup[id] = &pendingCleanup{job: Job{RequestID: id}}
		}
	}

	for id, p := range l.acquiring {
		// REPORTED, not reclaimed. Billet acquired this job and owes GitHub a
		// runner for it; releasing the escrow on a timer would not hand the work
		// back, because there is no way to hand it back. What it would do is let
		// another tier take the slot and leave the eventual assignment with
		// nothing behind it.
		//
		// So the lease is renewed like any other and the operator is told. The
		// capacity is genuinely still owed; the thing that resolves it is the
		// session ending, which releases every promise with it.
		if !p.reported && time.Since(p.at) > l.stalePromise {
			p.reported = true

			l.log.Warn("an acquired job has gone unassigned for a long time; its escrow is "+
				"still held because billet owes github a runner for it",
				"tier", l.tier, "request", id, "waited", time.Since(p.at).Round(time.Second))
		}

		// NOTHING WAS LAUNCHED for a promise, so unlike a running lease there is no
		// compute to owe anyone. The acquisition billet made to GitHub cannot be
		// honoured without capacity, and no local record makes it honourable.
		if !l.renew(ctx, p.lease).advertisable() {
			delete(l.acquiring, id)
			delete(l.confirmed, p.lease.ID)
		}
	}

	l.pruneConfirmed()
}

// pruneConfirmed drops renewal timestamps for leases this listener no longer
// holds.
//
// REBUILT FROM THE LIVE SETS rather than deleted at each departure. A lease
// leaves by many routes — completion, release, fencing, a reap, a failed launch,
// the shutdown drain — and a delete on each is a list that silently goes stale
// the next time a route is added. This map is bounded by what the listener
// actually holds, which the capacity budget already bounds, so one sweep per
// heartbeat costs nothing and cannot be forgotten.
func (l *Listener) pruneConfirmed() {
	if len(l.confirmed) == 0 {
		return
	}

	live := make(map[string]struct{}, len(l.held)+len(l.running)+len(l.acquiring))

	for _, lease := range l.held {
		live[lease.ID] = struct{}{}
	}

	for _, lease := range l.running {
		live[lease.ID] = struct{}{}
	}

	for _, p := range l.acquiring {
		live[p.lease.ID] = struct{}{}
	}

	for id := range l.confirmed {
		if _, ok := live[id]; !ok {
			delete(l.confirmed, id)
		}
	}
}

// renew heartbeats one lease and reports whether it is still this listener's.
func (l *Listener) renew(ctx context.Context, lease *alloc.Lease) renewal {
	err := l.alloc.Heartbeat(ctx, lease.ID, lease.Epoch)
	if err == nil {
		l.confirmed[lease.ID] = time.Now()

		return renewalOwned
	}

	// AN ANSWER OUTRANKS A DEADLINE. The allocator can say "this lease is not
	// yours" and have that answer arrive a moment after the pass deadline expired;
	// checking ctx.Err() first would discard it and keep advertising a lease
	// somebody else now holds. A context error explains why there is no answer, so
	// it may only be consulted when there is none.
	//
	// DEFENSIVE, and honestly labelled as such: no test drives it, because with
	// the real allocator a cancelled context fails inside the driver before any
	// query runs, so ErrFenced and a dead context cannot be produced together on
	// demand. The interleaving that reaches it — cancellation landing between a
	// successful query and this check — is real but not schedulable from a test.
	// The ordering costs nothing and the alternative is a known-wrong precedence.
	if errors.Is(err, alloc.ErrFenced) || errors.Is(err, alloc.ErrLeaseNotFound) {
		l.log.Warn("lost an escrowed lease; no longer advertising it",
			"tier", l.tier, "lease", lease.ID, "error", err)

		delete(l.confirmed, lease.ID)

		return renewalLost
	}

	// NO ANSWER. Shutting down, the pass ran out of its own deadline, or the database
	// is busy — the allocator never said this lease was not ours, so it is kept.
	// Dropping it would remove it from the release path too, and the ledger would keep
	// counting it until the reaper got it back.
	//
	// BUT NOT FOREVER. "No evidence it is lost" stops being a reason once the TTL has
	// passed without a single confirmed renewal: by then the reaper can have taken it,
	// and advertising capacity that is now someone else's is the exact double-admission
	// the escrow exists to prevent.
	//
	// The clock starts when the lease is TRACKED, not when the first renewal fails —
	// otherwise a never-confirmed lease gets an extra TTL it never earned. Every entry
	// point seeds `confirmed`, so a missing entry means the lease is not one of ours to
	// renew.
	last, seen := l.confirmed[lease.ID]
	if !seen {
		l.log.Warn("renewing a lease this listener never recorded; treating it as unknown",
			"tier", l.tier, "lease", lease.ID)

		l.confirmed[lease.ID] = time.Now()
	}

	if seen && time.Since(last) > l.alloc.LeaseTTL() {
		l.log.Error("could not renew an escrowed lease for longer than its TTL; it is no "+
			"longer being advertised, because the reaper may already have reclaimed it",
			"tier", l.tier, "lease", lease.ID, "since", time.Since(last).Round(time.Second),
			"error", err)

		delete(l.confirmed, lease.ID)

		return renewalStale
	}

	l.log.Warn("could not renew an escrowed lease; keeping it",
		"tier", l.tier, "lease", lease.ID, "error", err)

	return renewalUnknown
}

// refillEscrow tops the escrow up to what this tier could use.
//
// Escrow returns what it could actually give, which may be nothing when another
// tier holds the capacity. Advertising zero is a correct answer: it tells GitHub
// to assign this tier no work, which is exactly true.
func (l *Listener) refillEscrow(ctx context.Context) error {
	room, err := l.alloc.Headroom(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: headroom for %s: %w", l.tier, err)
	}

	// Headroom already excludes what this listener holds — those leases are open
	// in the ledger — so this tops the total up rather than doubling it.

	if l.maxCapacity != nil {
		// Capped BEFORE the escrow, not after. Escrowing capacity this listener
		// has promised not to advertise would hold it away from every other tier
		// for nothing.
		if ceiling := *l.maxCapacity - l.capacity(); room > ceiling {
			room = ceiling
		}
	}

	if room <= 0 {
		return nil
	}

	// STAMPED BEFORE THE CALL, which is the only place a second clock can be safely
	// wrong. Sampling after l.mu lets a slow heartbeat pass hold the mutex and date a
	// lease TTL/3 late; sampling after Escrow returns is no better, because the
	// goroutine can be descheduled between the commit and the sample. Both errors run
	// in the same direction: a lease dated later than it really is stays advertisable
	// after the reaper could already have taken it.
	//
	// Taken beforehand, the error runs the other way — the lease is dated slightly
	// EARLIER than the allocator's own expiry basis, so the worst case is dropping one
	// billet still owns. That costs a re-escrow; the opposite costs two tiers the same
	// machine. The real fix is for Escrow to return the allocator's authoritative
	// expiry, filed with the rest of the lifecycle work.
	created := time.Now()

	leases, err := l.alloc.Escrow(ctx, l.tier, room)
	if err != nil {
		return fmt.Errorf("server: escrow for %s: %w", l.tier, err)
	}

	l.mu.Lock()

	l.held = append(l.held, leases...)

	// THE UNCERTAINTY CLOCK STARTS HERE, at the only point a lease id enters this
	// listener at all — everything after this moves leases between held,
	// acquiring and running, so they are already tracked.
	//
	// Starting it at the first FAILED renewal instead would hand a never-confirmed
	// lease an extra TTL it had not earned: escrowed at t=0 and expiring at t=TTL,
	// a lease whose first heartbeat failed at t=TTL/3 would have its clock set
	// there and stay advertised past t=4TTL/3, long after the reaper could have
	// taken it.
	for _, lease := range leases {
		l.confirmed[lease.ID] = created
	}

	l.mu.Unlock()

	return nil
}

// handle processes one message and acknowledges it.
//
// lastMessageID is advanced at the END, and only once the acknowledgement lands.
//
// It is sent to the SERVICE as ?lastMessageId= (session_client.go getMessage),
// so it is a claim about what billet is done with rather than a local note. What
// the source actually proves is narrower than it is tempting to state: the
// client only shows that the parameter is SENT. The service's own contract, per
// the client's doc comment, is that an undeleted message is returned again. How
// the queue treats the parameter is not established by anything billet can read,
// and this comment previously asserted it did.
//
// Advancing it after the acknowledgement is the conservative order under either
// reading. If the parameter does filter, advancing early skips a message whose
// work never happened; if it does not, advancing late costs one redelivery of a
// message that is safe to re-handle. The asymmetry is the whole argument.
//
// Nothing exercises it yet, because every failure here is currently fatal — the
// session ends and a fresh one starts the cursor at zero. That is the fragile
// part: the cursor was only correct as a side effect of an unrelated decision
// about error severity, so the first non-fatal error path anyone adds inherits
// the question.
func (l *Listener) handle(ctx context.Context, msg *Message) error {
	// COMPLETED IS PROCESSED FIRST. Otherwise the cycle never closes — the lease stays
	// open until the reaper expires it — and it must come first because GitHub batches
	// the completion of one job with the offer of its replacement: acquiring before
	// releasing claims the replacement while still holding the finished job's lease.
	//
	// `finished` is scoped to THIS MESSAGE, which is the whole lifetime the problem
	// has. A batch can carry Assigned and Completed for the same request — an
	// assigned-then-cancelled job — and it does not need to survive the call, because a
	// redelivery rebuilds it before the assignments are read. A longer-lived map would
	// silently skip a request id GitHub requeued after cancelling it.
	finished := make(map[int64]struct{}, len(msg.Completed))

	for _, job := range msg.Completed {
		finished[job.RequestID] = struct{}{}

		if err := l.recordCompletion(ctx, job); err != nil {
			return err
		}
		l.complete(ctx, job)
	}

	// A zero advertisement is not the same as refusing work, and this is where
	// the difference is enforced.
	//
	// AdvertiseNothing stops billet asking for jobs; it does not stop GitHub
	// delivering a message that was already queued, or redelivering one that was
	// never acknowledged. Acquiring from that would claim a job nothing can run —
	// the precise outcome the dry run exists to avoid — so the refusal is local
	// as well as advertised.
	if l.maxCapacity != nil && *l.maxCapacity == 0 {
		if len(msg.Available) > 0 || len(msg.Assigned) > 0 {
			l.log.Warn("declining work while advertising no capacity",
				"tier", l.tier, "available", len(msg.Available), "assigned", len(msg.Assigned))
		}

		if err := l.acknowledge(ctx, msg); err != nil {
			return err
		}

		// Advanced here too. The invariant is "a successful acknowledgement
		// advances the cursor", and an early return that acknowledges without
		// advancing is the same class of inconsistency the reordering fixed.
		l.lastMessageID = msg.MessageID

		return nil
	}

	// NO NEW WORK WHILE DRAINING, and the refusal has to be HERE rather than in
	// the advertisement.
	//
	// Releasing the idle escrow drops the number billet sends, which asks GitHub
	// to stop offering — it does not stop a message already queued from arriving,
	// or an unacknowledged one from being redelivered. Both would otherwise be
	// acquired, because the refill immediately below would take the capacity back
	// and the acquisition would then be perfectly backed. The drain would never
	// converge: every offer it accepted would extend it.
	//
	// ONLY OFFERS. Assigned still runs and Completed still completes — those are
	// promises made before the drain started, and abandoning them would strand the
	// leases the drain is waiting on and leave GitHub holding a job nothing will
	// launch.
	if l.isDraining() {
		if len(msg.Available) > 0 {
			l.log.Info("declining an offer while draining",
				"tier", l.tier, "available", len(msg.Available))
		}
	} else {
		// Topped up between the release and the acquisition, so the slot just freed
		// is available to back the offer that arrived with it. It may lose the race
		// to another tier — escrow is first-come — and that is a fairness question,
		// not a correctness one: what matters is that billet does not claim work it
		// cannot back. See TestAGreedyTierCanTakeTheWholeBudget.
		if len(msg.Available) > 0 {
			if err := l.refillEscrow(ctx); err != nil {
				return err
			}
		}

		// AVAILABLE is what gets acquired. Available is the offer; Assigned is the
		// confirmation that an offer was claimed. Acquiring from Assigned asks
		// GitHub to claim work it has already handed over, and drops every offer.
		if err := l.acquire(ctx, msg.Available); err != nil {
			return err
		}
	}

	// ASSIGNED is what consumes escrow. The lease is bound here because this is
	// the point at which the work is definitely billet's to run.
	//
	// LAUNCHING HAPPENS AFTER, outside the escrow mutex that assign holds. A
	// launch pulls images and talks to a hypervisor; doing it under the mutex
	// would stall every heartbeat behind it, and heartbeats are what keep the
	// escrow alive. So assign returns what it bound and the launches follow.
	for _, job := range msg.Assigned {
		if _, over := finished[job.RequestID]; over {
			continue
		}

		lease, needsCompute, err := l.assign(ctx, job)
		if err != nil {
			return err
		}

		if !needsCompute {
			// Declined, or a redelivery of something already running. Either way
			// there is nothing new to start.
			continue
		}

		if err := l.launch(ctx, lease, job); err != nil {
			return err
		}
	}

	if msg.Statistics != nil {
		l.observed = msg.Statistics
	}

	if err := l.acknowledge(ctx, msg); err != nil {
		return err
	}

	// Only now. An unacknowledged message is redelivered, and re-handling one is
	// safe once the acquisition outcome is KNOWN: completions rebuild their own
	// tombstone set, offers already promised are skipped by reserve, and
	// assignments are idempotent by request id. Skipping a message is not
	// recoverable, so late beats early.
	//
	// That safety does NOT extend to an ambiguous acquisition. If AcquireJobs
	// commits at GitHub and the response is lost, billet sees an error, unreserves
	// every id, and has no way to learn it now owes runners for them — AcquireJobs
	// being one-way means there is nothing to ask. Today that cannot bite, because
	// an acquisition error ends the session and the whole control plane; anyone
	// making it non-fatal has to solve the unknown-outcome case first, and the
	// answer is not "retry".
	l.lastMessageID = msg.MessageID

	return nil
}

// acknowledge tells GitHub the message was handled. An unacknowledged message is
// redelivered, which is why everything above it has to be idempotent.
func (l *Listener) acknowledge(ctx context.Context, msg *Message) error {
	if err := l.session.DeleteMessage(ctx, msg.MessageID); err != nil {
		return fmt.Errorf("server: acknowledge message %d: %w", msg.MessageID, err)
	}

	return nil
}

// acquire claims the offers this listener has escrow to back, reserving that
// escrow first.
//
// An acquisition is a PROMISE to run the job, so the lease is moved out of held
// and bound to the request id BEFORE the network call. Checking a count and
// leaving the leases where they were is what allowed one lease to back two
// promises, and what let the heartbeat spend a lease out from under an
// acquisition already in flight.
//
// Claiming fewer offers than were made is normal and not a loss: an unacquired
// offer goes to another scale set or is re-offered, whereas an acquisition
// billet cannot back is a job that goes nowhere at all.
func (l *Listener) acquire(ctx context.Context, available []Job) error {
	if len(available) == 0 {
		return nil
	}

	reserved := l.reserve(available)
	if len(reserved) == 0 {
		return nil
	}

	acquired, err := l.session.AcquireJobs(ctx, reserved)
	if err != nil {
		// Nothing was promised, so nothing stays reserved.
		l.unreserve(reserved)

		return fmt.Errorf("server: acquire jobs for %s: %w", l.tier, err)
	}

	// GitHub returns what it ACTUALLY gave, which can be fewer than were asked for —
	// another scale set can win the same offer. Escrow reserved for an offer billet
	// did not get goes back immediately.
	//
	// A response that is not a subset of the request means billet cannot tell what it
	// has committed to: an id nobody offered for has no reservation to match, and a
	// body wrong about that id may be wrong about the reserved ids too.
	//
	// This STOPS the listener, unlike an unbacked assignment, which declines and
	// carries on. An unbacked assignment is reachable by ordinary races — a heartbeat
	// drops a fenced lease, a restart loses a promise — so killing the control plane
	// over one is disproportionate. A response outside its own contract is not
	// reachable by any race, and stopping is also the remedy: the session is recreated
	// and GitHub redelivers whatever was unacknowledged.
	if extra := missing(acquired, reserved); len(extra) > 0 {
		// Everything, not just the unmatched ids. Which commitments are real is
		// exactly what is no longer known.
		l.unreserve(reserved)

		return fmt.Errorf("%w: %s acquired job requests it did not offer for "+
			"(unrequested %v, requested %v); refusing to continue against a scale-set "+
			"response that is not a subset of its request",
			ErrUntrustworthySession, l.tier, extra, reserved)
	}

	// GitHub returns what it ACTUALLY gave, which can be fewer than were asked
	// for — another scale set can win the same offer. Escrow reserved for an
	// offer billet did not get goes back immediately; holding it would strand
	// capacity waiting for an assignment that is never coming.
	l.unreserve(missing(reserved, acquired))

	return nil
}

// reserve moves escrow from held into acquiring, one lease per offer, and
// returns the request ids it could back.
func (l *Listener) reserve(available []Job) []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	ids := make([]int64, 0, len(available))

	for _, job := range available {
		// Already promised, and therefore NOT returned to the caller.
		//
		// Returning it looked harmless and was not: the caller unreserves whatever
		// GitHub does not grant, so a re-offer of a request billet had already
		// acquired would tear down the earlier, successful promise the moment the
		// second acquisition came back partial. The lease then backed the next
		// offer instead, and the assignment for the original request found
		// nothing. unreserve may only undo reservations THIS call created.
		if _, ok := l.acquiring[job.RequestID]; ok {
			continue
		}

		if _, ok := l.running[job.RequestID]; ok {
			continue
		}

		// AND NOT WHILE ITS LAST CONTAINER IS STILL OWED. A request whose compute
		// this listener has not managed to destroy is not a free request id: the
		// cleanup retry addresses a Destroy BY REQUEST ID, so taking the same id
		// again would give the old retry the power to destroy the new job's
		// container and release the new job's lease. Request id alone cannot tell
		// two incarnations apart, so the id stays occupied until the first one is
		// discharged.
		if entry, ok := l.cleanup[job.RequestID]; ok {
			// SAID ONCE. GitHub re-offers a job nobody acquires for as long as it is
			// queued, so warning per offer turns one stuck obligation into a line
			// every poll for as long as the node stays away.
			if !entry.declined {
				entry.declined = true

				l.log.Warn("declining a job whose previous run left compute billet has not "+
					"managed to destroy; it stays queued until that is cleaned up, and this "+
					"is reported once rather than on every offer",
					"tier", l.tier, "request", job.RequestID)
			}

			continue
		}

		if len(l.held) == 0 {
			l.log.Warn("declining an offer with no escrow to back it",
				"tier", l.tier, "request", job.RequestID)

			continue
		}

		l.acquiring[job.RequestID] = &promise{lease: l.held[0], at: time.Now()}
		l.held = l.held[1:]

		ids = append(ids, job.RequestID)
	}

	return ids
}

// unreserve returns promised escrow to held.
func (l *Listener) unreserve(ids []int64) {
	if len(ids) == 0 {
		return
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	for _, id := range ids {
		if p, ok := l.acquiring[id]; ok {
			delete(l.acquiring, id)
			l.held = append(l.held, p.lease)
		}
	}
}

// missing returns the ids that were asked for and not granted.
func missing(asked, granted []int64) []int64 {
	if len(granted) == 0 {
		return asked
	}

	got := make(map[int64]struct{}, len(granted))
	for _, id := range granted {
		got[id] = struct{}{}
	}

	var lost []int64

	for _, id := range asked {
		if _, ok := got[id]; !ok {
			lost = append(lost, id)
		}
	}

	return lost
}

// assign moves one escrowed lease to the job GitHub gave it, and reports which
// lease that was.
//
// Returning the lease is what lets the caller launch OUTSIDE this function's
// mutex. The bool says whether there is anything to launch at all — a request
// already running, or one declined for want of escrow, needs nothing started.
// Explicit rather than a nil lease, because "no value and no error" is exactly
// the shape a caller mishandles without noticing.
func (l *Listener) assign(ctx context.Context, job Job) (*alloc.Lease, bool, error) {
	// Held for the whole function, INCLUDING the allocator write.
	//
	// Releasing it around the write to keep heartbeats snappy looks obviously
	// right and is not: it opens a window where the write succeeds but a
	// concurrent heartbeat has already dropped the lease, so the assignment is
	// durable in the ledger and tracked nowhere in memory — capacity leaked until
	// the reaper expires it. The window is worth nothing anyway. handle() runs
	// only on the poll goroutine, so heartbeat is the sole concurrent writer, and
	// what it waits for is one local SQLite transaction against a 30-second beat.
	l.mu.Lock()
	defer l.mu.Unlock()

	// Already ours. An unacknowledged message is redelivered, so this is an
	// ordinary event rather than a fault — and consuming a second lease for one
	// job would leak capacity that nothing ever gives back.
	if _, ok := l.running[job.RequestID]; ok {
		return nil, false, nil
	}

	// NOR WHILE THE PREVIOUS RUN'S COMPUTE IS STILL OWED. Same reasoning as reserve: a
	// pending cleanup is addressed by request id, so accepting an assignment for that
	// id hands the old retry a container and a lease that belong to the new job.
	//
	// AND THIS IS NOT A DECLINE. Leaving a request out of AcquireJobs is a real
	// non-acquisition — GitHub can offer it to another scale set or offer it again.
	// There is no equivalent call for a job already ASSIGNED to this scale set: billet
	// does not launch it, acknowledges the message, and the job waits for GitHub's
	// pickup deadline to reassign it. A delay, not a loss, and the least bad option
	// here — holding the message unacknowledged would re-deliver it every poll and
	// block every message behind it. Doing better needs the assignment held locally
	// until the obligation clears, which needs a launch identity rather than a request
	// id. Tracked separately.
	if _, ok := l.cleanup[job.RequestID]; ok {
		l.log.Error("cannot start an assigned job while its previous run's compute is still "+
			"waiting to be destroyed; billet is not launching it and GitHub will reassign it "+
			"after its pickup deadline",
			"tier", l.tier, "request", job.RequestID)

		return nil, false, nil
	}

	// The lease this assignment was PROMISED, if billet acquired the offer. This
	// is the ordinary path: acquire reserved a lease against this exact request id
	// and nothing else can have spent it in the meantime.
	var lease *alloc.Lease

	p, promised := l.acquiring[job.RequestID]
	if promised {
		lease = p.lease
	}

	if !promised {
		// No promise on file. GitHub can legitimately assign work this listener
		// never saw an offer for — after a restart, or when the offer was handled
		// by a process that is gone — so a free lease is used if there is one.
		if len(l.held) == 0 {
			// DECLINED, not fatal. Being assigned more than was advertised looks
			// like a protocol violation, but GitHub over-assigning is not the only
			// way to get here: billet's own escrow can vanish underneath an
			// acquisition — the heartbeat drops a fenced lease, a restart loses the
			// promise. Returning an error would kill the listener and take the
			// whole control plane down with it, stranding every tier's capacity
			// over one job.
			//
			// Declining keeps the invariant that matters: nothing runs without
			// escrow. The job is not acquired, GitHub reassigns it, and the
			// operator gets a loud line rather than an outage.
			l.log.Error("assigned a request with no escrow to back it; declining it",
				"tier", l.tier, "request", job.RequestID, "run", job.RunID)

			return nil, false, nil
		}

		lease = l.held[0]
	}

	if err := l.alloc.Assign(ctx, lease.ID, lease.Epoch, job.RunID, job.RequestID); err != nil {
		return nil, false, fmt.Errorf("server: assign lease %s: %w", lease.ID, err)
	}

	// Moved into running only AFTER the assignment is durable. Consuming it first
	// meant a failed Assign left the lease open in the database and absent from
	// the release path — capacity that nothing hands back and nothing reports,
	// until the reaper's TTL expires it.
	if promised {
		delete(l.acquiring, job.RequestID)
	} else {
		l.held = l.held[1:]
	}

	l.running[job.RequestID] = lease

	return lease, true, nil
}

// launch starts the compute for a lease, and hands the capacity back if it will
// not start.
//
// Called with the mutex NOT held. A launch pulls images and talks to a
// hypervisor, and holding the escrow mutex across that stalls every heartbeat.
//
// A failed launch RELEASES the lease rather than keeping it. That is the
// opposite of the rule for a failed session close, and the difference is whether
// anything is running: a lease whose compute never started is backing nothing, so
// holding it withholds capacity from every other tier for no reason. GitHub
// reassigns the job when its pickup deadline passes.
func (l *Listener) launch(ctx context.Context, lease *alloc.Lease, job Job) error {
	err := l.runner.Launch(ctx, lease, job)

	if err == nil {
		// STILL OURS? The mutex was released for the duration of the launch, and
		// the heartbeat runs in that window. If it found this lease fenced or
		// missing it dropped it — the allocator has already given the capacity to
		// somebody else — and the compute that just started is now referenced by
		// nothing and accounted for by nothing.
		//
		// Destroying it is the only honest outcome: the machine is not billet's
		// to use any more.
		l.mu.Lock()
		_, stillOurs := l.running[job.RequestID]
		l.mu.Unlock()

		if stillOurs {
			return nil
		}

		l.log.Error("the lease was reclaimed while its job was starting; destroying the "+
			"compute, which is no longer backed by any capacity",
			"tier", l.tier, "request", job.RequestID, "lease", lease.ID)

		if destroyErr := l.runner.Destroy(ctx, job.RequestID); destroyErr != nil {
			l.log.Error("could not destroy compute whose lease was reclaimed; it is running "+
				"unaccounted for and needs manual cleanup",
				"tier", l.tier, "request", job.RequestID, "error", destroyErr)
		}

		return nil
	}

	// THE CAPACITY IS NOT HANDED BACK IF THE RUNNER IS STILL HOLDING IT.
	//
	// A launch that failed ambiguously may have started something, and the runner
	// says so by returning ErrCustody: it has taken the lease into its own
	// janitor, will keep heartbeating it, and will release it once the compute is
	// confirmed gone. Releasing here as well would double-count the capacity —
	// the listener would re-advertise it while a container is possibly running.
	if errors.Is(err, ErrCustody) {
		l.log.Warn("a job failed to start and its compute could not be confirmed gone; "+
			"the runner is holding the capacity until it is",
			"tier", l.tier, "request", job.RequestID, "lease", lease.ID, "error", err)

		l.mu.Lock()
		delete(l.running, job.RequestID)
		l.mu.Unlock()

		return nil
	}

	l.log.Error("could not start the compute for an assigned job; handing the capacity back",
		"tier", l.tier, "request", job.RequestID, "run", job.RunID,
		"lease", lease.ID, "error", err)

	// Not fatal. The launch already failed; failing the listener as well would
	// take every tier down over one job that GitHub will simply reassign.
	relErr := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseFailed)
	if relErr != nil {
		l.log.Warn("could not release the lease of a job that failed to start",
			"tier", l.tier, "lease", lease.ID, "error", relErr)
	}

	// THE REQUEST ID IS ALWAYS GIVEN UP; THE OBLIGATION MOVES.
	//
	// Keeping the lease in `running` after a release that did not land was worse
	// than dropping it, and in a way that does not heal. Nothing retries a
	// release from that map — the cleanup loop walks its own — and the heartbeat
	// renews the entry forever, because the lease is still open at the same
	// epoch. Meanwhile the request id is wedged: GitHub reassigns a job that
	// never started, and `assign` treats a redelivery for an id already in
	// `running` as its own work and silently swallows it, every time. The
	// advertisement counts a phantom and a drain waits its full grace for a
	// completion that cannot arrive.
	//
	// So an unsettled release parks the lease in the cleanup set, which is the
	// mechanism that already exists for exactly this — retried on its own clock,
	// backing off, and refusing a re-assignment of that id until it clears.
	// Archived as FAILED, because nothing ran: recording it as done would put a
	// lie in the history.
	l.mu.Lock()

	delete(l.running, job.RequestID)

	if !releaseSettled(relErr) {
		if l.cleanup == nil {
			l.cleanup = make(map[int64]*pendingCleanup)
		}

		if _, pending := l.cleanup[job.RequestID]; !pending {
			l.cleanup[job.RequestID] = &pendingCleanup{
				job: job, lease: lease, outcome: alloc.PhaseFailed, releaseOnly: true,
			}
		}
	}

	l.mu.Unlock()

	return nil
}

// leaseFor is the lease this listener currently associates with a request, from
// wherever it is being tracked.
func (l *Listener) leaseFor(requestID int64) *alloc.Lease {
	l.mu.Lock()
	defer l.mu.Unlock()

	if lease, ok := l.running[requestID]; ok {
		return lease
	}

	if p, ok := l.acquiring[requestID]; ok {
		return p.lease
	}

	if entry, ok := l.cleanup[requestID]; ok {
		return entry.lease
	}

	return nil
}

// releaseSettled reports whether a release attempt ended the lease's claim on
// capacity, one way or another.
//
// A CONCLUSIVE ERROR IS AS GOOD AS SUCCESS, and telling the two apart from an
// outage is the whole point. ErrFenced means somebody else already reclaimed it
// and this caller's epoch is stale; ErrLeaseNotFound means it is gone or already
// terminal. Neither can be improved by holding a reference and trying again —
// but a busy database or a cancelled context can.
func releaseSettled(err error) bool {
	return err == nil || errors.Is(err, alloc.ErrFenced) || errors.Is(err, alloc.ErrLeaseNotFound)
}

// releaseAbsent returns capacity after the runner has proved no compute exists.
// A fenced release may name a lease the reaper quarantined while the proof was
// in flight; ResolveQuarantine is the only operation that turns that proof into
// returned capacity.
func (l *Listener) releaseAbsent(ctx context.Context, requestID int64, lease *alloc.Lease,
	outcome alloc.Phase,
) error {
	relErr := l.alloc.Release(ctx, lease.ID, lease.Epoch, outcome)
	if !errors.Is(relErr, alloc.ErrFenced) {
		return relErr
	}

	if err := l.alloc.ResolveQuarantine(ctx, lease.ID, outcome); err == nil {
		l.log.Warn("a cleanup release found its lease quarantined after compute was "+
			"confirmed gone; the capacity is back",
			"tier", l.tier, "request", requestID, "lease", lease.ID)

		return nil
	} else if !errors.Is(err, alloc.ErrLeaseNotFound) {
		return err
	}

	return relErr
}

// releaseParked retries the local half of cleanup after the runner has proved no
// compute exists. It reports whether it found and handled a release-only entry.
//
// The allocator call stays outside the listener mutex. A SQLite writer can be
// busy behind an operator command, and holding the mutex there would stall lease
// renewal for every unrelated job in this tier.
func (l *Listener) releaseParked(ctx context.Context, requestID int64) (bool, bool) {
	l.mu.Lock()
	entry, ok := l.cleanup[requestID]
	if !ok || !entry.releaseOnly || entry.lease == nil {
		l.mu.Unlock()

		return false, false
	}

	lease := entry.lease
	outcome := entry.outcome
	if outcome == "" {
		outcome = alloc.PhaseDone
	}
	l.mu.Unlock()

	relErr := l.releaseAbsent(ctx, requestID, lease, outcome)

	l.mu.Lock()
	defer l.mu.Unlock()

	current, stillPending := l.cleanup[requestID]
	if !stillPending || current != entry {
		return true, false
	}

	if !releaseSettled(relErr) {
		entry.failed(time.Now(), l.retryFirst, l.retryMax)
		l.log.Error("could not release capacity after compute was confirmed absent; it stays "+
			"held until this is retried",
			"tier", l.tier, "request", requestID, "lease", lease.ID, "error", relErr)

		return true, false
	}

	delete(l.cleanup, requestID)
	delete(l.running, requestID)
	delete(l.acquiring, requestID)

	return true, true
}

func (l *Listener) recordCompletion(ctx context.Context, job Job) error {
	if l.completionStore == nil || job.Result == "" {
		return nil
	}

	lease, outcome, releaseOnly := l.completionRelease(job.RequestID)
	completion := state.PendingCompletion{
		Tier: l.tier, RequestID: job.RequestID, RunID: job.RunID, Result: job.Result,
	}
	if lease != nil {
		completion.LeaseID = lease.ID
		completion.LeaseEpoch = lease.Epoch
		completion.Outcome = string(outcome)
		completion.ReleaseOnly = releaseOnly
	}
	if err := l.completionStore.PutPendingCompletion(ctx, completion); err != nil {
		return fmt.Errorf("server: preserve completed result for %s request %d before acknowledging it: %w",
			l.tier, job.RequestID, err)
	}

	return nil
}

// completionRelease snapshots the capacity obligation before GitHub's message
// can be acknowledged. A node teardown and a ledger release are one ordered
// completion; restart recovery needs the fenced lease identity to finish the
// second half if the process stops between them.
func (l *Listener) completionRelease(requestID int64) (*alloc.Lease, alloc.Phase, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if entry := l.cleanup[requestID]; entry != nil && entry.lease != nil {
		outcome := entry.outcome
		if outcome == "" {
			outcome = alloc.PhaseDone
		}

		return entry.lease, outcome, entry.releaseOnly
	}
	if lease := l.running[requestID]; lease != nil {
		return lease, alloc.PhaseDone, false
	}
	if promise := l.acquiring[requestID]; promise != nil {
		return promise.lease, alloc.PhaseDone, false
	}

	return nil, "", false
}

func (l *Listener) forgetCompletion(ctx context.Context, job Job) {
	if l.completionStore == nil || job.Result == "" {
		return
	}
	l.forgetCompletionRequest(ctx, job.RequestID)
}

func (l *Listener) forgetCompletionRequest(ctx context.Context, requestID int64) {
	if l.completionStore == nil {
		return
	}
	if err := l.completionStore.DeletePendingCompletion(ctx, l.tier, requestID); err != nil {
		l.log.Error("a completed job settled, but its durable obligation could not be removed; a restart will safely settle it again",
			"tier", l.tier, "request", requestID, "error", err)
	}
}

func (l *Listener) restoreCompletions(ctx context.Context) error {
	if l.completionStore == nil {
		return nil
	}
	completions, err := l.completionStore.PendingCompletions(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: restore pending completions for %s: %w", l.tier, err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, completion := range completions {
		job := Job{RequestID: completion.RequestID, RunID: completion.RunID, Result: completion.Result}
		if entry := l.cleanup[job.RequestID]; entry != nil {
			entry.job = job
			if completion.LeaseID != "" {
				entry.lease = &alloc.Lease{ID: completion.LeaseID, Epoch: completion.LeaseEpoch}
				entry.outcome = alloc.Phase(completion.Outcome)
				entry.releaseOnly = completion.ReleaseOnly
			}

			continue
		}
		entry := &pendingCleanup{job: job}
		if completion.LeaseID != "" {
			entry.lease = &alloc.Lease{ID: completion.LeaseID, Epoch: completion.LeaseEpoch}
			entry.outcome = alloc.Phase(completion.Outcome)
			entry.releaseOnly = completion.ReleaseOnly
		}
		l.cleanup[job.RequestID] = entry
	}

	return nil
}

// complete releases the lease a finished job was running on.
//
// Idempotent, because a redelivered Completed message must not fail: a job billet
// has already released is simply not in the map. Release with PhaseDone rather
// than inspecting a conclusion — the lease's job is finished either way, and the
// outcome belongs to job history rather than to the capacity ledger.
// IT CANNOT FAIL, and the signature says so.
//
// Everything that can go wrong here — a destroy the node refused, a release the
// ledger could not take — is recorded as a pending obligation and retried on the
// cleanup clock. Returning an error would be worse than useless: complete runs
// on the poll path, where an error stops the listener, cancels every other
// listener, and destroys every job running on this host.
//
// Returning nothing keeps that from being re-learned. An error return that is
// always nil is an invitation to wire the next failure mode through it, and the
// branch handling it would never run.
func (l *Listener) complete(ctx context.Context, job Job) {
	// A previous attempt already established that no compute exists. Repeating a
	// remote destroy adds no proof, and on shutdown it can wait a full node timeout
	// before the local release that is the only work left.
	if handled, settled := l.releaseParked(ctx, job.RequestID); handled {
		if settled {
			l.forgetCompletion(ctx, job)
		}

		return
	}

	// DESTROYED BEFORE RELEASED, and outside the mutex.
	//
	// Releasing first would hand the capacity to another tier while this job's
	// container or microVM is still running on the host — the budget would be
	// satisfied on paper and overcommitted in fact. Same shape as closing the
	// session before releasing the escrow.
	//
	// Idempotent by contract, so a redelivered completion, a request this
	// listener never launched, and a second attempt after a failure all reach
	// this safely.
	//
	// THE LEASE IS NOTED BEFORE THE DESTROY, because the maps can change during
	// it. A remote destroy has no bound, and the heartbeat runs the whole time:
	// if it finds this lease fenced — which is what the reaper quarantining it
	// looks like — it drops the entry and records a cleanup obligation carrying
	// no lease. The destroy then SUCCEEDS, proving the container is gone, and
	// the code that would resolve the quarantine has nothing left to name. The
	// capacity stays charged until a node happens to report an inventory, or
	// forever if that node never comes back.
	before := l.leaseFor(job.RequestID)

	if err := l.destroyCompleted(ctx, job); err != nil {
		// THE RUNNER IS HOLDING IT, SO THIS LISTENER LETS GO (#46).
		//
		// A backend whose teardown is asynchronous cannot answer this call with
		// proof that the compute is gone. The node takes the lease into its own
		// janitor instead, keeps heartbeating it, and releases it once the compute
		// is provably gone.
		//
		// So there is nothing here to release and nothing to retry. Recording a
		// cleanup obligation would re-issue a terminate on every pass for the life
		// of the process while the outcome is already being reconciled, and keeping
		// the lease in `running` would have two parties heartbeating one lease.
		//
		// The request id is given up either way: GitHub has been told this job is
		// finished, and a redelivered completion must not find this listener still
		// claiming the job.
		if errors.Is(err, ErrCustody) {
			l.forgetCompletion(ctx, job)
			l.log.Info("the compute for a finished job was asked to stop and has not been "+
				"confirmed gone; the runner is holding its capacity until it is",
				"tier", l.tier, "request", job.RequestID)

			l.mu.Lock()
			delete(l.running, job.RequestID)
			delete(l.cleanup, job.RequestID)
			l.mu.Unlock()

			return
		}

		// NOT released, and NOT fatal. Two separate decisions.
		//
		// Not released, because the compute may still be running and freeing the
		// capacity now is exactly the overcommit this ordering prevents. The lease
		// stays in `running`, so it keeps being heartbeated and keeps being
		// counted — which is what makes holding it safe rather than a slow leak.
		//
		// Not fatal, because a listener error cancels every other listener and
		// their shutdowns then destroy every running job on the host. A docker
		// daemon hiccup while cleaning up ONE finished job would take down the
		// fleet. That is the same disproportion already rejected for an unbacked
		// assignment.
		l.log.Error("could not destroy the compute for a finished job; keeping its capacity "+
			"held and retrying, because releasing it now would let another tier use a "+
			"machine this job may still be on",
			"tier", l.tier, "request", job.RequestID, "error", err)

		// AND RETRIED, which is what turns "held" into something other than a leak.
		//
		// The comment above was true and incomplete: holding the capacity is safe,
		// and holding it FOREVER is not. Nothing else was ever going to try again —
		// GitHub's completion has been acknowledged, the node has already destroyed
		// what it had, and the lease sits in `running` being heartbeated by this
		// listener for the life of the process.
		l.mu.Lock()

		// ONLY IF THIS LISTENER ACTUALLY HOLDS THE LEASE. A completion can arrive
		// for a job this listener never assigned — a restart lost the in-memory map
		// while the lease lived on for the reaper — and recording those would grow
		// the map with entries whose retry can never accomplish anything.
		_, held := l.running[job.RequestID]

		if entry, ok := l.cleanup[job.RequestID]; ok {
			// A heartbeat can create the obligation before GitHub's completion
			// arrives. Keep the authoritative result so its retry takes the same
			// result-dependent teardown path as this attempt.
			if job.Result != "" {
				entry.job = job
			}
			// A RETRY THAT FAILED AGAIN, so it waits longer before the next one.
			entry.failed(time.Now(), l.retryFirst, l.retryMax)
		} else if _, promised := l.acquiring[job.RequestID]; held || promised ||
			(l.completionStore != nil && job.Result != "") {
			if l.cleanup == nil {
				l.cleanup = make(map[int64]*pendingCleanup)
			}

			// Recorded ready to run: the first retry is immediate because a node
			// that was briefly busy is far and away the common case.
			l.cleanup[job.RequestID] = &pendingCleanup{job: job}
		}

		// AN EXISTING RECORD IS NEVER DROPPED HERE, unlike the rule above.
		//
		// Losing the lease does not prove the container is gone. The record exists
		// because this listener launched something and could not destroy it; once the
		// lease is fenced or reaped the capacity is someone else's, but the compute is
		// still ours to remove, and GitHub will not redeliver the completion that would
		// ask again. Sweeper is not a substitute: it is OPTIONAL on the Runner
		// interface, so a non-sweeping runner leaves the container until the host
		// restarts.
		//
		// Entries back off (retryEvery, capped at maxRetryEvery) so a node that is never
		// coming back cannot occupy every pass ahead of one that has just recovered. The
		// map is in memory, so this does not survive a restart; durable cleanup state is
		// tracked separately.

		l.mu.Unlock()

		return
	}

	l.mu.Lock()

	lease, ok := l.running[job.RequestID]

	// A RELEASE THAT NEVER LANDED PARKED THE LEASE HERE. The request id was given
	// up so a redelivery is not swallowed, and this is the only remaining
	// reference to the capacity it holds.
	outcome := alloc.PhaseDone

	if entry, parked := l.cleanup[job.RequestID]; parked && entry.lease != nil && !ok {
		lease, ok = entry.lease, true

		if entry.outcome != "" {
			outcome = entry.outcome
		}
	}

	// OR THE ONE NOTED BEFORE THE DESTROY, if the heartbeat dropped it while that
	// was in flight. AFTER the parked branch on purpose: that one carries an
	// outcome as well as a lease, and taking the bare reference first would
	// archive a job that never started as `done`.
	if !ok && before != nil {
		lease, ok = before, true
	}

	if !ok {
		// A job can also complete while it is still only PROMISED — GitHub cancels
		// an assignment no runner picks up in time, and that cancellation arrives
		// as a completion. The reserved escrow has to come back, or it is held for
		// an assignment that will never arrive.
		var p *promise

		if p, ok = l.acquiring[job.RequestID]; ok {
			lease = p.lease
		}
	}

	if !ok {
		// Not ours, or already released. Both are ordinary: GitHub can report a
		// job completed that this listener never assigned, if a restart lost the
		// in-memory map while the lease lives on in the ledger for the reaper.
		//
		// Nothing left to retry either way — including for a lease that was fenced
		// or reaped out from under this listener, whose entry would otherwise sit
		// in the map being retried for the life of the process.
		delete(l.cleanup, job.RequestID)
		l.mu.Unlock()
		l.forgetCompletion(ctx, job)

		return
	}

	relErr := l.alloc.Release(ctx, lease.ID, lease.Epoch, outcome)

	// FENCED NO LONGER PROVES THE CAPACITY CAME BACK.
	//
	// It used to: the reaper terminalized whatever it took, so a stale epoch
	// meant the lease was already finished. Quarantine changed that — a lease
	// with compute behind it is moved aside and KEPT CHARGED, so a release
	// refused for a stale epoch may be refused by a lease that is still holding
	// its host's capacity.
	//
	// This listener can settle it, because it has the one thing the quarantine is
	// waiting for: the destroy above SUCCEEDED, so the compute is confirmed gone.
	// That is the same proof a node offers, and it goes through the same door.
	// Terminalizing at the current epoch instead would be the dangerous version
	// of this — it would free the capacity on the strength of the epoch alone,
	// which says nothing about whether a container exists.
	if errors.Is(relErr, alloc.ErrFenced) {
		// WITH THE OUTCOME THIS PATH ALREADY KNOWS. The job finished and its
		// compute is confirmed gone; archiving it as failed because the reaper got
		// to the lease first would record a job GitHub reported completed as one
		// that did not.
		if err := l.alloc.ResolveQuarantine(ctx, lease.ID, outcome); err == nil {
			l.log.Warn("a finished job's lease had been quarantined before its release "+
				"landed; its compute is confirmed gone, so the capacity is back",
				"tier", l.tier, "request", job.RequestID, "lease", lease.ID)

			relErr = nil
		} else if !errors.Is(err, alloc.ErrLeaseNotFound) {
			// Not quarantined, or the ledger could not answer. Either way this is
			// not settled by us.
			relErr = err
		}
	}

	if err := relErr; !releaseSettled(err) {
		// PARKED AND NOT RETURNED, because of who is calling.
		//
		// complete runs on the poll path as well as the cleanup loop, and there
		// the returned error is FATAL: it stops the listener, which cancels every
		// other listener, whose shutdowns destroy every job running on this host.
		// A busy database while GitHub happens to report a completion would take
		// the whole deployment down.
		//
		// That is also self-defeating. The reason for returning the error was to
		// keep the obligation alive for a retry — and the retry runs in the
		// process the error kills. So the obligation is recorded here, where it
		// will be picked up, and the caller is told the completion was handled.
		if l.cleanup == nil {
			l.cleanup = make(map[int64]*pendingCleanup)
		}

		if entry, pending := l.cleanup[job.RequestID]; pending {
			entry.lease, entry.outcome, entry.releaseOnly = lease, outcome, true
			entry.failed(time.Now(), l.retryFirst, l.retryMax)
		} else {
			l.cleanup[job.RequestID] = &pendingCleanup{
				job: job, lease: lease, outcome: outcome, releaseOnly: true,
			}
		}

		l.log.Error("could not release the lease of a finished job; its capacity is held "+
			"until this is retried",
			"tier", l.tier, "request", job.RequestID, "lease", lease.ID, "error", err)

		delete(l.running, job.RequestID)
		delete(l.acquiring, job.RequestID)
		l.mu.Unlock()

		return
	}

	// RELEASED, so the job is finally over and there is nothing to retry.
	delete(l.running, job.RequestID)
	delete(l.acquiring, job.RequestID)
	delete(l.cleanup, job.RequestID)
	l.mu.Unlock()
	l.forgetCompletion(ctx, job)
}

func (l *Listener) destroyCompleted(ctx context.Context, job Job) error {
	if runner, ok := l.runner.(CompletionAwareRunner); ok && job.Result != "" {
		return runner.DestroyCompleted(ctx, job.RequestID, job.Result)
	}

	return l.runner.Destroy(ctx, job.RequestID)
}

// releaseAll hands back capacity that was escrowed and never used.
//
// Given a context that outlives cancellation, because the ordinary reason for
// getting here is that the context was cancelled — and escrowed capacity that is
// never released is capacity no tier can use until the reaper expires it.
func (l *Listener) releaseAll(ctx context.Context, destroyed map[int64]bool) {
	// SNAPSHOT under the mutex, tear down OUTSIDE it.
	//
	// Destroy talks to a docker daemon or a remote node and has no bound. Holding
	// the escrow mutex across that starves the heartbeat, which is the thing
	// keeping every OTHER lease alive — so a single hung teardown could expire
	// the leases it was trying to protect, and could stop the process exiting at
	// all.
	l.mu.Lock()

	held := append([]*alloc.Lease(nil), l.held...)
	running := make(map[int64]*alloc.Lease, len(l.running))

	for id, lease := range l.running {
		running[id] = lease
	}

	promised := make([]*alloc.Lease, 0, len(l.acquiring))
	for _, p := range l.acquiring {
		promised = append(promised, p.lease)
	}

	// PARKED OBLIGATIONS TOO. A failed launch whose release did not land keeps
	// its lease here rather than in `running`, and shutdown is the last chance
	// anything in this process has to hand that capacity back.
	parked := make(map[int64]*pendingCleanup, len(l.cleanup))

	for id, entry := range l.cleanup {
		if entry.lease != nil {
			parked[id] = entry
		}
	}

	l.held = nil
	l.acquiring = make(map[int64]*promise)
	l.mu.Unlock()

	// WHAT DID NOT LAND IS PUT BACK, rather than dropped on the floor. These were
	// cleared above so nothing new could take them mid-shutdown; a release that
	// failed for a reason a retry could fix has to stay somewhere the next pass —
	// or a supervisor's restart — can still find it.
	var stuck []*alloc.Lease

	release := func(lease *alloc.Lease) {
		err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone)
		if err != nil {
			l.log.Warn("could not release escrowed capacity",
				"tier", l.tier, "lease", lease.ID, "error", err)

			if !releaseSettled(err) {
				stuck = append(stuck, lease)
			}
		}
	}

	for _, lease := range held {
		release(lease)
	}

	// RUNNING leases are DESTROYED before they are released: freeing the capacity while
	// a container is still on the host lets another tier escrow it. A lease whose
	// compute will not die is NOT released — capacity the reaper reclaims late is
	// recoverable, capacity handed out twice is not.
	//
	// This kills work in flight and FAILS those builds; GitHub does not requeue a job a
	// runner has already started. It is still right: billet is stopping and can no
	// longer manage them, so a failed build beats containers nobody is tracking.
	//
	// A patient shutdown rarely arrives here with anything running — the drain waits
	// first. The ways in that did not wait are a second signal, an untrustworthy
	// session, and a Run that returned on an error it could not continue past.
	//
	// DESTROYED ALREADY, by destroyAll, which is why this only releases.
	for requestID, lease := range running {
		if !destroyed[requestID] {
			// NOT RELEASED, and kept in `running`. Freeing capacity whose container
			// is still alive is the overcommit the whole ordering exists to prevent;
			// holding the reference is what lets a supervisor's restart find it
			// again, and it is the only thing that makes "keep the capacity" true
			// rather than a slower way of losing it.
			l.log.Error("could not destroy the compute for a running job; its lease is kept "+
				"rather than released, and this instance needs manual cleanup if billet does "+
				"not come back",
				"tier", l.tier, "request", requestID, "lease", lease.ID)

			continue
		}

		err := l.releaseAbsent(ctx, requestID, lease, alloc.PhaseDone)
		if !releaseSettled(err) {
			l.log.Warn("could not release completed capacity after compute was confirmed absent",
				"tier", l.tier, "request", requestID, "lease", lease.ID, "error", err)
			stuck = append(stuck, lease)

			continue
		}
		l.forgetCompletionRequest(ctx, requestID)
	}

	// Promised escrow too. The acquisition was made to GitHub, but nothing has
	// been assigned yet and nothing can be launched, so holding it past shutdown
	// would strand it until the reaper.
	for _, lease := range promised {
		release(lease)
	}

	// And the parked ones, with the outcome they were parked with: a launch that
	// never started did not finish, whatever the ordinary path records.
	for id, entry := range parked {
		outcome := entry.outcome
		if outcome == "" {
			outcome = alloc.PhaseDone
		}
		err := l.releaseAbsent(ctx, id, entry.lease, outcome)
		if err != nil {
			l.log.Warn("could not release cleanup capacity after compute was confirmed absent",
				"tier", l.tier, "request", id, "lease", entry.lease.ID, "error", err)

			if !releaseSettled(err) {
				stuck = append(stuck, entry.lease)
			}
		}
		if releaseSettled(err) {
			l.forgetCompletionRequest(ctx, id)
		}
	}

	// WHAT DID NOT LAND IS THE REAPER'S, and saying so is the honest version.
	//
	// An earlier attempt put these back on `held` "so the next pass can find
	// them", which was wrong in a way worth recording: releaseAll runs once, from
	// Run's teardown, on a listener that is single-use — there is no next pass,
	// and a restart builds a new listener that has never heard of this map. The
	// reference was findable by nothing.
	//
	// What actually recovers them is the reaper, within a TTL, because nothing is
	// left to heartbeat them. That is a real mechanism rather than an imagined
	// one, and it is why this is a warning rather than a failure.
	if len(stuck) > 0 {
		l.log.Warn("shutting down with escrow whose release did not land; the reaper reclaims "+
			"it once it stops being renewed",
			"tier", l.tier, "leases", len(stuck))
	}
}
