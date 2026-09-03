// Package server is billet's control plane: the per-tier scale-set listeners
// and the scheduler that turns assigned jobs into launched instances.
package server

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"slices"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/provider"
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

// RunnerRegistry removes a GitHub registration before its guest is destroyed.
// A failed removal leaves compute and capacity held so GitHub cannot race a new
// assignment onto a guest Billet is tearing down.
type RunnerRegistry interface {
	RemoveRunner(ctx context.Context, runnerID int64, runnerName string) error
}

// CompletionAwareRunner receives GitHub's authoritative completed-job result.
// It is optional so runners that have no result-dependent teardown keep the
// smaller Runner contract.
type CompletionAwareRunner interface {
	DestroyCompleted(ctx context.Context, requestID int64, result string) error
}

// BoundCompletionAwareRunner reconciles teardown with the node and lease that
// actually held compute before a control-plane restart erased live ownership.
type BoundCompletionAwareRunner interface {
	DestroyCompletedBound(
		ctx context.Context,
		requestID int64,
		result, leaseID, nodeName string,
		leaseEpoch int64,
		outcome alloc.Phase,
	) error
}

// completionStore is the durable half of result-dependent teardown.
type completionStore interface {
	PutPendingCompletion(ctx context.Context, completion state.PendingCompletion) (state.PendingCompletionDisposition, error)
	RetirePendingCompletion(ctx context.Context, tier string, requestID, messageID int64) error
	AcknowledgePendingCompletion(ctx context.Context, tier string, requestID, messageID int64) error
	PendingCompletions(ctx context.Context, tier string) ([]state.PendingCompletion, error)
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

// ErrHolderUnavailable means result-dependent teardown has not reached the
// process that holds the compute, so its durable completion must be retried.
var ErrHolderUnavailable = errors.New("server: the completion holder is unavailable")

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

// errQuarantinableCompletion is an untrustworthy completion that creates no
// unknown remote commitment. Its payload cannot change on redelivery, but the
// local ledger may still converge, so Run retries it before acknowledging it
// away. Other untrustworthy responses remain immediately fatal.
var errQuarantinableCompletion = fmt.Errorf("%w: the completion has no safe identity",
	ErrUntrustworthySession)

// errQuarantinableStarted marks an immutable contradiction confined to one
// same-tier pool member. Operational failures must remain retryable and leave
// the source unacknowledged rather than destroying a runner on a failed read.
var errQuarantinableStarted = fmt.Errorf("%w: the started identity contradicts its pool member",
	ErrUntrustworthySession)

// poisonedMessageError carries the valid completions beside a poison so they can
// be made durable after the whole message is finally acknowledged.
type poisonedMessageError struct {
	cause       error
	completions []Job
}

func (e *poisonedMessageError) Error() string { return e.cause.Error() }
func (e *poisonedMessageError) Unwrap() error { return e.cause }

// Message is one batch of scale-set news.
type Message struct {
	MessageID  int64
	Statistics *Statistics
	// Available is work GitHub is OFFERING. Acquiring one of these is how a
	// scale set claims it.
	Available []Job
	// Assigned is work this scale set has been given, which is the confirmation
	// that an acquisition succeeded.
	Assigned []Job
	// Started binds a registered pool member to the job it actually consumed.
	// GitHub may choose a different member than the assignment that caused Billet
	// to scale up.
	Started   []Job
	Completed []Job
}

// Job identifies one workflow job.
//
// RequestID is billet's numeric scheduler identity. GitHub's positive
// runnerRequestId is used unchanged; a direct assignment carrying zero receives
// a durable negative id keyed by JobID, so concurrent jobs never alias at zero.
type Job struct {
	RequestID int64
	RunID     int64
	// RunnerID and RunnerName identify the pool member GitHub actually bound.
	// They are authoritative only on JobStarted and JobCompleted messages.
	RunnerID int64
	// JobID is GitHub's stable workflow-job identity. It is required when the
	// direct-assignment path sends RequestID zero.
	JobID string
	// CompletionID is the scale-set message that delivered the result. A
	// redelivery keeps it; a later reuse of RequestID receives a different one.
	CompletionID int64
	// RunnerName is GitHub's name for the ephemeral runner. Completed messages
	// can omit RequestID, so the billet-issued name is the durable route back to
	// the lease and its assigned request.
	RunnerName string
	// Result is GitHub's conclusion on a completed-job message. It is empty on
	// available and assigned messages.
	Result string
	// The GitHub event that queued this job — retained for diagnostics only. A JIT
	// runner joins a pool before GitHub chooses its job, so event is not launch
	// authority; the tier's static trust policy is.
	Event string
	// Owner, Repository and WorkflowRef are GitHub's authenticated cache scope.
	// They come from the scale-set assignment, never from a workflow-controlled
	// environment variable or by decoding the Actions runtime token.
	Owner       string
	Repository  string
	WorkflowRef string
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

	// heartbeatLock runs immediately before mu is taken at the top of a heartbeat
	// pass. TEST-ONLY and nil in every deployment; it gates the acquisition
	// rather than performing it, so it cannot get the mutex wrong.
	//
	// THE SEAM IS THE LOCK RATHER THAN A HOOK ABOVE IT, and that is the whole
	// reason it exists. What the ordering below has to get right is which side of
	// the lock the pass's deadline is built on, and a hook that merely runs first
	// proves only that it ran — the goroutine can be descheduled between it and
	// the deadline, which leaves a test asserting a scheduling accident. Entering
	// this proves that everything sequenced BEFORE the lock has already happened,
	// which is exactly the fact the test needs and the only one that makes the
	// wrong ordering fail causally rather than usually.
	heartbeatLock func()

	// Guards the escrow below: renewal and the poll loop touch held and running
	// concurrently.
	mu sync.Mutex
	// Escrowed capacity not yet given to a job.
	held []*alloc.Lease
	// heldOrder restores the allocator's issuance order after a partial
	// acquisition returns a lease to held. Provider rank alone cannot preserve
	// pack/spread/name placement among targets on the same backend.
	heldOrder     map[string]uint64
	nextHeldOrder uint64
	// releasing removes a shrink candidate from heartbeat selection without
	// making its still-owned capacity disappear while Allocator.Release is in
	// flight. A concurrent held loss is therefore visible before another
	// candidate is chosen.
	releasing map[string]*alloc.Lease
	// releaseCapacity is a test seam for staging the heartbeat/release race. Nil
	// uses the allocator directly; no production option can replace it.
	releaseCapacity func(context.Context, string, int64, alloc.Phase) error
	// Escrowed capacity that HAS been given to a job, keyed by request id so a
	// redelivered message is recognised rather than assigned twice.
	//
	// Both halves are advertised — see capacity(). The safety property is that the
	// number sent to GitHub is only ever capacity this listener took from the
	// allocator, never one computed from headroom.
	running map[int64]*alloc.Lease
	// Restart-surviving runner leases held by the node rather than this
	// listener. They count in GitHub's total pool capacity but do not enter the
	// listener's heartbeat or teardown ownership.
	adopted map[string]bool

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
	runner   Runner
	registry RunnerRegistry
	// completionStore keeps authoritative job results across an ACK followed by a
	// process stop, until the node accepts result-dependent teardown.
	completionStore completionStore

	// TotalAssignedJobs is the documented scaling signal; counting messages is
	// not, because a response carries at most 50 and a large backlog is truncated.
	observed *Statistics

	// Bounds somebody else's JOB rather than billet's own teardown, so it has its
	// own ceiling. See maxDrainGrace.
	drainGrace time.Duration
	// Guarded by mu, and NOT the same thing as sealed.
	draining bool
	// drainStarted is when this listener began draining, and drainWarnedAt when
	// it last said the drain was running long. Both guarded by mu.
	//
	// A DRAIN THAT CANNOT END ITSELF HAS TO BE AUDIBLE. drainGrace stopped being
	// a deadline — it never destroys anything now — so the only thing left that
	// can tell an operator their deployment has been draining for a day is billet
	// saying so on a cadence.
	drainStarted  time.Time
	drainWarnedAt time.Time
	// quiesced is set while the DEPLOYMENT's admission is sealed.
	//
	// SEPARATE FROM draining, which means this process is shutting down and ends
	// with the listener returning. A seal is a state the deployment can leave: an
	// operator resumes and the listener must go back to work, so borrowing
	// `draining` would turn `billet drain` into a control-plane shutdown — and a
	// listener returning cancels every other listener, whose teardown destroys
	// the compute they hold.
	quiesced bool
	// Closing it ends the drain's wait without abandoning what is held. Read
	// here, never closed here.
	hurry <-chan struct{}

	// leadershipLost answers whether this process has stopped being this
	// deployment's controller. Nil outside the control plane; see
	// WithLeadershipLost.
	leadershipLost func() bool
}

// NewListener builds a listener for one tier.
func NewListener(a *alloc.Allocator, tier string, session Session, opts ...Option) *Listener {
	l := &Listener{
		alloc:         a,
		tier:          tier,
		session:       session,
		log:           slog.Default(),
		running:       make(map[int64]*alloc.Lease),
		adopted:       make(map[string]bool),
		acquiring:     make(map[int64]*promise),
		cleanup:       make(map[int64]*pendingCleanup),
		destroying:    make(map[int64]bool),
		configErrs:    make(map[string]error),
		confirmed:     make(map[string]time.Time),
		heldOrder:     make(map[string]uint64),
		releasing:     make(map[string]*alloc.Lease),
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

// WithRunnerRegistry installs the GitHub side of safe runner retirement.
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
	// failureReason is why a failed outcome will be archived, when this
	// listener is the party that knows — a launch it watched fail — so a
	// release that lands on a later attempt records the same reason the first
	// attempt would have. Empty for every other obligation.
	failureReason string
	// releaseOnly means the runner has already proved there is no compute to
	// destroy: either Launch failed without custody, or a previous Destroy
	// succeeded. Retrying a remote destroy in either case can only delay the
	// ledger release, and during shutdown that delay can be a full node timeout.
	releaseOnly bool
	// retireOnly means teardown and capacity settlement are complete, but the
	// durable tombstone still needs to be written before this id can be reused.
	retireOnly bool
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

// fenced reports that this listener must tear down without acting on anything.
func (l *Listener) fenced() bool {
	return l.leadershipLost != nil && l.leadershipLost()
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
		// A FENCED CONTROLLER ACTS ON NOTHING, AND THIS IS ASKED BEFORE ANY OF IT.
		//
		// Every step below is an authoritative act — destroying compute, closing
		// this deployment's message session, handing capacity back — and a process
		// that has stopped being the controller has the right to none of them. The
		// successor is already running and performs every one correctly: it holds
		// the same leases and destroys the same compute, GitHub expires the session
		// this one abandons (which is the path openSession already waits out after
		// every ungraceful restart), and its startup Reap reclaims the escrow once
		// the heartbeats stop.
		//
		// SO A FENCED STOP IS DELIBERATELY A HARD KILL, which is a recovery billet
		// already implements rather than a gap. Running guests keep running and are
		// re-adopted; the leases stay charged, which is the safe direction, because
		// freeing a slot whose compute is live is the overcommit the whole escrow
		// ordering exists to prevent.
		//
		// A LEDGER THAT COULD NOT BE READ IS NOT THIS, AND DELIBERATELY DOES NOT
		// ABANDON. checkLeadership refuses that write and reports the storage fault
		// without latching, so a database blip during a shutdown still runs the
		// teardown below — which is right, and the reasoning is the reverse of the
		// usual fail-closed one. What makes abandoning safe is that a successor
		// DEMONSTRABLY EXISTS and owns every obligation this process is dropping.
		// An unreadable claim is no evidence of one, and the destroys skipped here
		// are for jobs GitHub has already concluded: refusing to finish them
		// because a query timed out strands containers on somebody's host for a
		// build that ended. The write is still refused, which is where the
		// could-not-tell has to be answered no.
		if l.fenced() {
			l.abandon(ctx, &sweeping, &beating, stopSweeping, stopBeating)

			return
		}

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
		// FALSE: a shutdown tears down the completions it OWES and never the jobs
		// still executing. This process stopping says nothing about whether that
		// work should end, and ending it fails builds GitHub will not requeue.
		destroyed := l.destroyAll(stopCtx, false, nil)

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

	if err := l.refreshAdoptedCapacity(ctx); err != nil {
		return err
	}

	// A restart does not replay messages for work already assigned, so a listener
	// that waits to be told about a backlog sits idle in front of one.
	l.observed = l.session.Statistics()
	l.reportOrphanedBacklog()
	if l.observed != nil {
		if err := l.reconcilePool(ctx, l.observed.TotalAssignedJobs); err != nil {
			return err
		}
	}

	// THE DRAIN IS A STATE OF THIS LOOP, NOT A PHASE OF THE TEARDOWN. There ctx is
	// already cancelled and the long poll dead, so the listener could not be TOLD a job
	// finished: it would wait for news that cannot arrive and destroy the jobs anyway.
	//
	// So cancellation stops it taking NEW work and nothing else.
	pollCtx := ctx

	var (
		draining        bool
		endDrain        context.CancelFunc
		poisonMessageID int64
		poisonRefusals  int
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

			l.warnDrainOverrun()

			if pollCtx.Err() != nil {
				// A SECOND SIGNAL, and it is the only way here now that the drain
				// carries no deadline. It ends the WAITING and nothing else: the
				// jobs still running are LEFT running, their guests keep going, the
				// node goes on holding them, and the next control plane re-adopts
				// their leases. Destroying them was what this used to do, and it
				// failed builds GitHub does not requeue.
				l.log.Warn("no longer waiting for the jobs still running here; they are "+
					"LEFT RUNNING and their capacity stays charged until a host proves "+
					"the compute is gone. Their leases are re-adopted when a control "+
					"plane returns",
					"tier", l.tier, "running", l.Running(),
					"waited", time.Since(l.drainStartedAt()).Truncate(time.Second))

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
			if err := l.refreshAdoptedCapacity(pollCtx); err != nil {
				if cancelledWhileServing(ctx, draining, err) {
					continue
				}

				return stopping(ctx, err)
			}
		}

		// ASKED EVERY POLL, because a seal arrives from another process and there
		// is nothing to notify this one. A poll is the natural cadence: it is how
		// often this listener reconsiders everything else, and a seal that takes
		// effect one poll later is a seal that takes effect before the next
		// opportunity to accept work.
		// MARK ONLY. Handing the escrow back HERE is the same defect this loop
		// fixes after the poll: it would release the capacity behind an
		// advertisement GitHub still holds, before any poll has carried the
		// smaller number. The release happens once a poll has landed.
		quiesced, admissionKnown := l.markAdmission(pollCtx)

		// ONE CALL SITE, AND DELIBERATELY ONE. A force-destroy is the only
		// operation that fails a running build, so every additional place it can be
		// reached from is another path somebody has to prove cannot fire by
		// accident. It costs a poll of latency, exactly as a seal does — and for the
		// same reason: the request arrives from another process, and a poll is how
		// often this listener reconsiders everything else.
		//
		// BEFORE THE ESCROW IS TOPPED UP, so capacity the force returns is available
		// to this poll rather than the next one.
		//
		// NOT GUARDED BY !draining. An operator who drained, waited, and gave up
		// waiting is precisely who runs this, and refusing to act during a drain
		// would leave them with no orderly way out of the state the drain put them
		// in.
		l.forceDestroy(pollCtx)

		if !draining && !quiesced {
			if err := l.prepareEscrow(pollCtx); err != nil {
				if cancelledWhileServing(ctx, draining, err) {
					continue
				}

				return stopping(ctx, err)
			}
		}

		if !draining {
			// RECONCILED EVEN WHILE QUIESCED, but to GITHUB'S OWN ASSIGNED COUNT
			// rather than to zero.
			//
			// Forcing zero here would destroy running jobs. `PoolRunnerIdle` means
			// "no Started message has been processed locally", NOT "idle at
			// GitHub": between GitHub starting a job on a registered runner and
			// this listener handling that message, the member reads idle while it
			// is working. TotalAssignedJobs is the source-side fact that keeps such
			// a member out of the surplus, and overriding it removes the only proof
			// there is.
			//
			// WHAT THIS DOES NOT GUARANTEE, stated rather than assumed. The
			// retirement that removes a registration depends on `desired` falling,
			// and `desired` is GitHub's aggregate, refreshed only by a message
			// carrying statistics. A sealed listener advertises its actual capacity,
			// which falls to zero — and a zero-capacity scale set receives no work
			// OR STATISTICS, as targetCapacity records. So the last observation can
			// freeze, and while it does, `active == desired` keeps every locally
			// idle member out of the surplus and its registration in place.
			//
			// There is no fix here that is honest. Retiring on a cached aggregate is
			// retiring on a number that may predate the job now running on that
			// member, which is the round-one defect wearing a different hat; the
			// codebase's own rule is that a freshness check on your own record is
			// not a causal fence on somebody else's snapshot. Closing it needs
			// affirmative per-registration evidence, which is the durable
			// deregistration signal rather than something this loop can infer.
			//
			// The failure direction is the safe one. A registration that lingers
			// keeps its lease non-terminal, so the quiescence barrier stays
			// un-quiet and a drain keeps waiting and keeps reporting what it is
			// waiting for. The seal under-delivers visibly rather than reporting a
			// deployment quiesced that is not.
			if l.observed != nil {
				if err := l.reconcilePool(pollCtx, l.observed.TotalAssignedJobs); err != nil {
					// reconcilePool launches runners, which can take minutes; a
					// cancellation landing mid-launch must enter the drain so the
					// jobs already running finish, not stop the listener and have
					// the deferred teardown destroy them.
					if cancelledWhileServing(ctx, draining, err) {
						continue
					}

					return stopping(ctx, err)
				}
			}
		}

		// WHAT IS STILL HERE, not what billet would like. capacity() falls to the
		// work in flight as the idle escrow goes back, and reaches zero by itself
		// — which is the same shape a drain uses, and for the same reason: a
		// constant zero while a job runs is untrue, since what billet sends is the
		// scale set's total capacity.
		// WITHDRAWING ADVERTISES WHAT IS COMMITTED, not what is held. The escrow
		// stays until a poll has carried the smaller number, because until then
		// GitHub's live advertisement is still the old one and an assignment
		// against it has to find backing.
		advertised := l.committedCapacity()
		withdrawnWhenSent := true

		if !draining && !quiesced {
			advertised = l.advertisedCapacity()
			withdrawnWhenSent = false
		}
		msg, err := l.session.GetMessage(pollCtx, l.lastMessageID, advertised)

		// A timed-out long poll is the ordinary case. Poll again immediately —
		// the escrow is KEPT, because releasing and retaking it every poll
		// would hand the gap to another tier and produce exactly the flapping the
		// escrow exists to avoid.
		if errors.Is(err, ErrNoMessage) {
			// BEFORE reconcilePool, and that ordering is load-bearing rather than
			// tidy. This branch's reconcile is NOT guarded by `!draining` — unlike
			// the pre-poll one — and it launches out of `held`: `assignPoolSlot`
			// takes l.held[0] when nothing is acquiring. That was inert only
			// because beginDrain used to empty `held` before any of this ran, and
			// this commit stopped it doing that.
			//
			// Left after the reconcile, a listener told to stop starts a runner,
			// that runner enters `running` so the drain can never reach zero on it,
			// and the teardown destroys it when the grace expires — a failed build,
			// on a runner created after the drain began.
			//
			// The poll has already landed here, so releasing is licensed.
			//
			// SAID PLAINLY: no test distinguishes this ordering, and the mutation
			// that moves it back below the reconcile survives. Several fixtures
			// were tried — a deficit held open with a failing launcher, statistics
			// far above what the host can run — and in none of them did the
			// drain-time reconcile reach a launch: by then the deficit is closed
			// or the escrow has gone another way. So the hazard is argued, not
			// demonstrated. It is kept because it costs nothing and restores the
			// invariant beginDrain used to provide for free, not because anything
			// proves it.
			l.handBackIdleEscrow(pollCtx, draining || (quiesced && admissionKnown))

			if l.observed != nil {
				if err := l.reconcilePool(pollCtx, l.observed.TotalAssignedJobs); err != nil {
					// reconcilePool launches runners, which can take minutes; a
					// cancellation landing mid-launch must enter the drain so the
					// jobs already running finish, not stop the listener and have
					// the deferred teardown destroy them.
					if cancelledWhileServing(ctx, draining, err) {
						continue
					}

					if l.drainEnded(pollCtx, draining, err) {
						continue
					}

					return stopping(ctx, err)
				}
			}
			if !draining && !quiesced {
				l.releaseIdleEscrowAbove(pollCtx, max(advertised, l.targetCapacity()))
			}

			continue
		}

		if err != nil {
			if cancelledWhileServing(ctx, draining, err) {
				continue
			}

			if l.drainEnded(pollCtx, draining, err) {
				continue
			}

			return stopping(ctx, fmt.Errorf("server: poll %s: %w", l.tier, err))
		}
		// ASKED AGAIN, because the poll it just returned from can last most of a
		// minute and a seal arrives from another process. Observed only before the
		// poll, a seal landing while GetMessage was blocked would not be seen
		// until the next iteration — and the message in hand can carry an offer,
		// which the guard in handle would then permit against escrow still held.
		// MARKED, BUT NOT YET HANDED BACK. The message in hand can carry an
		// assignment GitHub made against capacity advertised before the seal, and
		// `assign` backs an unpromised one from `held` — so the escrow has to
		// outlive `handle`. Marking here still refuses any OFFER in the same
		// message.
		quiesced, admissionKnown = l.markAdmission(pollCtx)

		if !draining && !quiesced {
			l.releaseIdleEscrowAbove(pollCtx,
				max(advertised, l.messageCapacityTarget(msg)))
		}

		handled := l.handle(pollCtx, msg)

		// AFTER handle, so an assignment in that message kept its backing — and
		// only if the advertisement THIS poll sent was already the withdrawn one.
		//
		// A seal discovered by the re-read above arrived while GetMessage was
		// blocked, so the number GitHub currently holds is the full pre-seal one
		// and nothing has yet carried a smaller figure. Releasing on it would be
		// the very defect this commit exists to fix, surviving on the one path
		// where the withdrawal is discovered late. It defers to the next
		// iteration, which is what the ErrNoMessage branch already does.
		//
		// AND ONLY ON A HANDLED MESSAGE. Several handler failures return before
		// the assignment loop, so a mixed batch can carry an assignment made
		// against the older, larger advertisement that was never processed —
		// releasing its backing here recreates the same window. A failure keeps
		// the escrow for the teardown's close-then-release.
		if handled == nil && withdrawnWhenSent {
			l.handBackIdleEscrow(pollCtx, draining || (quiesced && admissionKnown))
		}

		if err := handled; err != nil {
			if poison, ok := errors.AsType[*poisonedMessageError](err); ok {
				if poisonRefusals == 0 || poisonMessageID != msg.MessageID {
					poisonMessageID = msg.MessageID
					poisonRefusals = 0
				}
				poisonRefusals++

				if poisonRefusals < poisonQuarantineAfter {
					l.log.Error("a completion message has a deterministic identity refusal; keeping it unacknowledged for another delivery before quarantine",
						"tier", l.tier, "message", msg.MessageID, "attempt", poisonRefusals,
						"quarantine_after", poisonQuarantineAfter, "error", poison)

					continue
				}

				handled := *msg
				handled.Completed = poison.completions
				if err := l.acknowledge(pollCtx, &handled); err != nil {
					if l.drainEnded(pollCtx, draining, err) {
						continue
					}

					return stopping(ctx, err)
				}
				l.acknowledgeCompletions(pollCtx, &handled)
				l.lastMessageID = msg.MessageID
				l.log.Error("quarantining a deterministically invalid completion message after repeated refusals; its invalid completions were discarded and the listener remains live",
					"tier", l.tier, "message", msg.MessageID, "attempts", poisonRefusals,
					"error", poison)
				poisonMessageID = 0
				poisonRefusals = 0

				continue
			}

			if cancelledWhileServing(ctx, draining, err) {
				continue
			}

			if l.drainEnded(pollCtx, draining, err) {
				continue
			}

			return stopping(ctx, err)
		}

		poisonMessageID = 0
		poisonRefusals = 0
		if !draining {
			l.releaseIdleEscrowAbove(pollCtx, max(advertised, l.targetCapacity()))
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

// drainEnded reports that this failure is the drain itself having been ended.
//
// IT WAS drainBudgetGone, AND THERE IS NO BUDGET ANY MORE. Nothing expires a
// drain; it ends when the work finishes or when a second signal cancels it. The
// name and the message both described a deadline, which would have sent the next
// reader looking for one.
//
// THE EXIT BRANCH IS AT THE TOP OF THE LOOP, so a call that fails BELOW it with
// the cancelled drain context must send the loop back round rather than return —
// otherwise the branch that decides never runs.
//
// That is not a tidiness point. Measured when a budget still existed: the drain
// announced itself, the admission read consumed the whole of it and warned, and
// then the listener returned in silence. `cancelledWhileServing` deliberately
// answers false once draining, so every error below the branch took `stopping`,
// which returns ctx.Err() without a word. The line nobody saw is the one naming
// which jobs were left running on their hosts — the only record an operator gets
// that anything is still out there.
//
// It cannot spin: the top of the loop sees the same cancelled context and returns.
//
// THE ERROR IS REPORTED RATHER THAN DROPPED. It was already being discarded —
// `stopping` returns ctx.Err() whenever the caller's context is cancelled, and
// during a drain it always is, so every one of these sites has been throwing the
// cause away since they were written. That is defensible for a cancellation and
// not for a broken ledger, and the two are indistinguishable in a journal that
// shows neither.
func (l *Listener) drainEnded(pollCtx context.Context, draining bool, err error) bool {
	if !draining || pollCtx.Err() == nil {
		return false
	}

	// Only when the error is something other than the cancellation itself, or
	// every ending would carry a line saying the context was cancelled — which
	// the exit line above it already says better.
	if err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		l.log.Warn("a call failed as the drain was ending; it is ending anyway and this "+
			"is what it was doing", "tier", l.tier, "error", err)
	}

	return true
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
	// NOT BOUNDED BY A DEADLINE, and that is deliberate. This context used to
	// expire after drainGrace, at which point the loop returned and the teardown
	// destroyed whatever was still running — a timer failing somebody's build. A
	// job may run for days; elapsed time is not evidence that it stopped making
	// progress, and billet imposes no job limit of its own.
	//
	// drainGrace survives as the moment billet starts SAYING the drain is long
	// (see warnDrainOverrun). It no longer ends anything.
	//
	// Built FIRST because the release below needs a live context — ctx is already
	// cancelled, and alloc.Release would fail on it and strand the capacity.
	drainCtx, endDrain := context.WithCancel(context.WithoutCancel(ctx))

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
	l.drainStarted = time.Now()
	l.mu.Unlock()

	// THE ESCROW IS NOT HANDED BACK HERE, and that is the fix for the rule this
	// file states at the teardown: the last maxCapacity GitHub saw stays live
	// until the session ends, so releasing escrow first leaves a positive
	// advertisement standing with nothing behind it. GitHub can assign against
	// that number, and the assignment arrives to find `held` empty and is
	// declined — a job delayed, and a loud error line in the middle of a
	// deliberate drain that reads exactly like the alarming cases it shares its
	// wording with.
	//
	// The loop advertises committedCapacity() from here on, and hands the escrow
	// back once a poll has carried that smaller number.
	l.log.Info("draining: not taking new work, waiting for what is already running for "+
		"as long as it takes",
		"tier", l.tier, "running", l.Running(), "held_until_next_poll", l.idleEscrow(),
		"report_after", l.drainGrace)

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
		delete(l.heldOrder, lease.ID)
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
		delete(l.heldOrder, lease.ID)
		l.mu.Unlock()

		released++
	}

	return released
}

// releaseIdleEscrowAbove hands back only unassigned leases above target total
// capacity. Acquiring, running, adopted, and already-releasing members remain
// counted in the target but can never be selected for release here.
func (l *Listener) releaseIdleEscrowAbove(ctx context.Context, target int) {
	released := 0
	for {
		l.mu.Lock()
		total := len(l.held) + len(l.releasing) + len(l.acquiring) + len(l.running) +
			len(l.adopted)
		if total <= target || len(l.held) == 0 {
			l.mu.Unlock()
			break
		}

		// Worst placement first. Moving it rather than snapshotting every surplus
		// candidate makes a concurrent heartbeat loss visible before another is
		// selected, while releasing still counts as owned until the allocator
		// confirms otherwise.
		lease := l.held[len(l.held)-1]
		l.held = l.held[:len(l.held)-1]
		l.releasing[lease.ID] = lease
		l.mu.Unlock()

		release := l.alloc.Release
		if l.releaseCapacity != nil {
			release = l.releaseCapacity
		}
		err := release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone)

		l.mu.Lock()
		_, stillTracked := l.releasing[lease.ID]
		delete(l.releasing, lease.ID)
		definitivelyLost := !stillTracked || errors.Is(err, alloc.ErrFenced) ||
			errors.Is(err, alloc.ErrLeaseNotFound)
		if err == nil || definitivelyLost {
			delete(l.confirmed, lease.ID)
			delete(l.heldOrder, lease.ID)
			if err == nil {
				released++
			}
		} else {
			l.held = append(l.held, lease)
			l.sortHeld()
		}
		l.mu.Unlock()

		if err != nil {
			if definitivelyLost {
				l.log.Warn("surplus idle escrow was lost while its release was in flight; it is no longer advertised",
					"tier", l.tier, "lease", lease.ID, "error", err)
				continue
			}
			l.log.Warn("could not release surplus idle escrow; it stays this listener's and will be retried",
				"tier", l.tier, "lease", lease.ID, "error", err)
			break
		}
	}

	if released > 0 {
		l.log.Info("released idle escrow above this tier's assigned demand",
			"tier", l.tier, "released", released, "target_capacity", target)
	}
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

// drainStartedAt is when this listener began draining, or the zero time.
func (l *Listener) drainStartedAt() time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.drainStarted
}

// drainWarnEvery bounds how often an overrunning drain repeats itself.
//
// Often enough that an operator watching a log learns the deployment is still
// waiting, rarely enough that a multi-day drain does not write a book.
const drainWarnEvery = 15 * time.Minute

// warnDrainOverrun says that a drain has run past drainGrace, and keeps saying it.
//
// THIS IS WHAT drainGrace IS NOW FOR. It used to be a deadline that destroyed
// what was still running; a drain that cannot end itself needs the operator to
// know it is happening, and a threshold crossed silently is a fleet that looks
// wedged. What it reports is what an automation asking "why is this not done"
// needs: how long, and what is still here.
func (l *Listener) warnDrainOverrun() {
	l.mu.Lock()

	started, warned := l.drainStarted, l.drainWarnedAt
	running := len(l.running)

	if started.IsZero() {
		l.mu.Unlock()

		return
	}

	now := time.Now()
	waited := now.Sub(started)

	if waited < l.drainGrace || (!warned.IsZero() && now.Sub(warned) < drainWarnEvery) {
		l.mu.Unlock()

		return
	}

	l.drainWarnedAt = now
	grace := l.drainGrace
	l.mu.Unlock()

	l.log.Warn("still draining, and this will not time out: billet waits for a running "+
		"job for as long as it runs, because elapsed time is not evidence that one "+
		"stopped making progress. Nothing here will be destroyed by waiting",
		"tier", l.tier, "running", running, "waited", waited.Truncate(time.Second),
		"drain_timeout", grace)
}

// capacity is what this listener advertises: TOTAL escrowed, not free.
//
// All three collections count, and each lease came from the allocator, so the sum
// across listeners is still bounded by the budget. Sending only the free half would
// shrink the advertisement every time a job started.
func (l *Listener) capacity() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.held) + len(l.releasing) + len(l.acquiring) + len(l.running) +
		len(l.adopted)
}

// idleEscrow is capacity this listener holds and nothing is using.
func (l *Listener) idleEscrow() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.held)
}

// committedCapacity is what this listener owes GitHub: work it has promised or
// is running, with idle escrow excluded.
//
// IT IS WHAT A WITHDRAWAL ADVERTISES. Handing the escrow back first and then
// advertising the smaller number gets the order backwards — see the teardown's
// own rule, that the last maxCapacity GitHub saw stays live until the session
// ends, so releasing escrow first leaves a positive advertisement standing with
// nothing behind it. Lowering the number first and releasing after the poll that
// carried it keeps the advertisement backed at every instant.
func (l *Listener) committedCapacity() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.releasing) + len(l.acquiring) + len(l.running) + len(l.adopted)
}

// advertisedCapacity may be lower than capacity while a shrink is pending. The
// surplus remains escrowed until GetMessage returns after sending this lower
// value, so GitHub and the allocator never disagree about who may use it.
func (l *Listener) advertisedCapacity() int {
	return min(l.capacity(), l.targetCapacity())
}

// targetCapacity keeps one backed discovery slot beyond GitHub's current
// assigned count. A zero-capacity scale set receives no work or statistics, but
// an idle listener holding every free lease starves every peer indefinitely.
func (l *Listener) targetCapacity() int {
	return l.targetCapacityFor(l.observed, 0)
}

func (l *Listener) targetCapacityFor(observed *Statistics, offered int) int {
	desired := 0
	if observed != nil && observed.TotalAssignedJobs > 0 {
		desired = observed.TotalAssignedJobs
	}
	l.mu.Lock()
	active := len(l.acquiring) + len(l.running) + len(l.adopted)
	l.mu.Unlock()
	if offered > math.MaxInt-active {
		desired = math.MaxInt
	} else {
		desired = max(desired, active+offered)
	}

	target := desired
	if target < math.MaxInt {
		target++
	}
	if l.maxCapacity != nil && target > *l.maxCapacity {
		target = *l.maxCapacity
	}
	if target < 0 {
		return 0
	}

	return target
}

// messageCapacityTarget is what the returned response can consume before any
// blocking acquisition, launch, or reconciliation begins. Available and
// Assigned are added because a direct assignment need not have appeared in this
// process's offer set; over-counting a job present in both is bounded by the
// message and safer than releasing its backing.
func (l *Listener) messageCapacityTarget(msg *Message) int {
	work := len(msg.Available)
	if len(msg.Assigned) > math.MaxInt-work {
		work = math.MaxInt
	} else {
		work += len(msg.Assigned)
	}
	observed := l.observed
	if msg.Statistics != nil {
		observed = msg.Statistics
	}

	return l.targetCapacityFor(observed, work)
}

func (l *Listener) refreshAdoptedCapacity(ctx context.Context) error {
	ids, err := l.alloc.ServiceableRunnerLeaseIDs(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: refresh adopted capacity for %s: %w", l.tier, err)
	}
	serviceable := make(map[string]bool, len(ids))
	for _, id := range ids {
		serviceable[id] = true
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	managed := make(map[string]bool, len(l.running))
	for _, lease := range l.running {
		managed[lease.ID] = true
	}
	for id := range l.adopted {
		if !serviceable[id] || managed[id] {
			delete(l.adopted, id)
		}
	}
	for id := range serviceable {
		if !managed[id] {
			l.adopted[id] = true
		}
	}

	return nil
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
// interrupts: an HTTP client can report a closed connection, and the vendored
// scale-set client composes its own text. The context is the authority — if it
// is done, that is the reason, whatever the layer underneath said.
//
// THE LEDGER IS NO LONGER ONE OF THOSE LAYERS, and this comment used to name it.
// SQLite surfaced an interrupted statement as "interrupted (9)"; that is now
// translated where it happens, by state.asCancellation, which does it ONLY for
// the driver's own interrupt code and therefore still reports SQLITE_CORRUPT and
// SQLITE_IOERR as themselves.
//
// WHICH IS THE COST OF THIS ONE, AND THE REASON NOT TO REACH FOR IT MORE WIDELY.
// It is a blanket collapse: a genuine storage fault racing a SIGTERM comes out of
// here as a bare cancellation, and `onlyCancellation` then discards it. It is
// used on the poll loop's paths, where a cancellation is the OVERWHELMING case
// and the alternative is a drain that reports nothing. Adding it to a startup
// path — which a round of this already tried — buys nothing the state layer does
// not already give and loses the fault.
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
			l.heartbeatPass(ctx)
		}
	}
}

// heartbeatPass renews everything this listener holds, once.
//
// EACH PASS IS BOUNDED so that the allocator calls made under l.mu cannot run
// on unboundedly. The loop's own context is already detached from Run's caller,
// so cancellation will not end them; what does is this deadline, which the
// allocator's context-aware SQLite operations honour. Without it a slow pass
// holds l.mu, and the teardown blocks on that mutex behind the defer that would
// have stopped the loop.
//
// THE BUDGET STARTS AFTER THE LOCK, AND THAT ORDER IS THE WHOLE POINT. Started
// at the tick, a pass spends its deadline WAITING FOR BILLET'S OWN MUTEX and
// then asks the allocator nothing — after which `renew` reads that silence as
// the allocator's rather than as its own. Past the TTL that is `renewalStale`,
// which drops a lease out of `running` and parks it in the cleanup set for a
// destroy: an ordinary stall — `assign` and the completion paths hold this mutex
// across allocator writes — manufacturing the conclusion that a healthy job's
// lease may already have been reaped, and tearing down somebody's build for it.
// Bounding the ALLOCATOR CALLS is what the deadline is for, and they do not
// begin until the lock is held.
//
// A function rather than the loop body so both the cancellation and the unlock
// are deferred: a panic in heartbeatHeld would otherwise leave the mutex held.
func (l *Listener) heartbeatPass(ctx context.Context) {
	l.lockForHeartbeat()

	pass, endPass := context.WithTimeout(ctx, l.heartbeatInterval())

	// REGISTERED IN THIS ORDER so they run in the other one: the mutex is
	// released before the pass's timer is stopped, rather than held across it.
	defer endPass()
	defer l.mu.Unlock()

	l.heartbeatHeld(pass)
}

// lockForHeartbeat takes the escrow mutex for one heartbeat pass.
//
// An indirection only so a test can stand exactly at the lock boundary. THE GATE
// DOES NOT REPLACE THE ACQUISITION: production takes l.mu here whether or not a
// gate is installed, so no callback can leave this returning without the mutex,
// take it twice, or release one it does not hold. See the heartbeatLock field
// for why the seam has to be at the lock and not above it.
func (l *Listener) lockForHeartbeat() {
	if l.heartbeatLock != nil {
		l.heartbeatLock()
	}

	l.mu.Lock()
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
	// A REPLACED CONTROLLER STARTS NOTHING NEW, AND THIS IS THE ONE LOOP THAT
	// COULD. Destroy talks to a host rather than to the ledger, so nothing about
	// the fence reaches it: the refusal that fences this process stops its writes
	// and cannot stop this. The signal cancels the whole plane within a scheduling
	// quantum, and this closes the window between the two — a tick landing inside
	// it would tear down compute whose lease the successor now holds.
	if l.fenced() {
		return
	}

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
	for i := range pending {
		l.attempt(ctx, pending[i])
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

// abandon ends a listener whose process is no longer this deployment's
// controller, without acting on anything it holds.
//
// IT STILL STOPS ITS OWN GOROUTINES, and that is the one thing it must do. A
// cleanup retry or a heartbeat pass outliving Run reaches the allocator after
// the caller was entitled to close the ledger — the same failure the ordinary
// teardown joins for. Their writes are refused by the fence either way; what is
// being prevented is the goroutine, not the write.
//
// THE JOIN IS BOUNDED AND THEREFORE NOT A GUARANTEE, exactly as the ordinary
// teardown's is, and saying so is the point: a cleanup retry can be inside a
// provider Destroy that does not honour cancellation, and no budget here can
// reach into one. What this promises is that no NEW external action starts —
// retryCleanup refuses outright once fenced — and that a retry still running
// when the grace expires is REPORTED rather than left silent. Making it a real
// guarantee means bounding every provider operation, which is a change to the
// Runner contract rather than to this teardown.
//
// SEALED FIRST, THEN CANCELLED, exactly as the ordinary teardown does it:
// cancelling says nothing about what the cleanup loop is midway through
// starting.
//
// IT SAYS SO BEFORE IT WAITS. The join is bounded by shutdownGrace, which is
// minutes, and an operator watching a control plane stop needs to know why now
// rather than after it.
func (l *Listener) abandon(
	ctx context.Context,
	sweeping, beating *sync.WaitGroup,
	stopSweeping, stopBeating context.CancelFunc,
) {
	l.seal()
	stopSweeping()
	stopBeating()

	l.mu.Lock()
	running, pending := len(l.running), len(l.cleanup)
	l.mu.Unlock()

	l.log.Warn("this process is no longer this deployment's controller, so it is stopping "+
		"without destroying compute, closing its message session or handing back capacity — "+
		"the jobs running here keep running and whichever controller replaced this one "+
		"adopts them, GitHub expires the session this one is abandoning, and the leases come "+
		"back once they stop being renewed",
		"tier", l.tier, "running", running, "cleanup", pending)

	// ITS OWN BUDGET, DETACHED FROM ctx. A teardown runs because ctx is already
	// done, so a join bounded by it would not wait at all.
	overall, endOverall := context.WithTimeout(context.WithoutCancel(ctx), l.teardownBudget())
	defer endOverall()

	// A PHASE EACH, under one overall cap, exactly as the ordinary teardown does
	// it. Sharing a single deadline between the two lets a cleanup retry stuck
	// inside a slow Destroy spend the whole grace and leave the renewal join with
	// none — which then reports "lease renewal did not stop" about a loop that was
	// never waited for, blaming one half for the other's overrun.
	sweepCtx, endSweep := context.WithTimeout(overall, l.shutdownGrace)
	defer endSweep()

	if !waitWithin(sweepCtx, sweeping) {
		l.log.Error("a cleanup retry did not return before this listener stopped; the ledger "+
			"refuses its writes, but it may still outlive the handle on it",
			"tier", l.tier, "grace", l.shutdownGrace)
	}

	beatCtx, endBeat := context.WithTimeout(overall, l.shutdownGrace)
	defer endBeat()

	if !waitWithin(beatCtx, beating) {
		l.log.Error("lease renewal did not stop before this listener did",
			"tier", l.tier, "grace", l.shutdownGrace)
	}
}

// destroyAll tears down compute this listener is responsible for and reports
// which requests are confirmed gone.
//
// includeRunning DECIDES WHETHER THIS FAILS SOMEBODY'S BUILD, and it is the whole
// difference between a shutdown and an emergency. `cleanup` holds destroy
// obligations for jobs GitHub has already CONCLUDED, so tearing one down ends
// nothing — it is the teardown that completion asked for. `running` holds jobs
// that are still executing, and destroying one fails that build, because GitHub
// does not requeue a job whose runner vanished after it started.
//
// SHUTDOWN PASSES FALSE. Nothing about this process stopping is evidence that the
// work on its hosts should end: the guests keep running, the node goes on holding
// them, and a restart re-adopts them through ServiceableRunnerLeaseIDs. Only an
// operator saying so passes true, through forceDestroy.
//
// scope NARROWS IT TO WHAT AN OPERATOR ACTUALLY APPROVED, and nil means everything
// this listener holds. A force enumerates its targets, shows them to a person and
// records them durably before anything is destroyed, so the destroy pass must act
// on that set and not on whatever the listener happens to hold by the time it
// runs — otherwise a job that started between the diagnostic and the confirmation
// is destroyed without ever having been approved. Shutdown passes nil because it
// approves nothing and destroys only what a completion already asked for.
//
// Concurrent, because each Destroy can wait the node command timeout. Bounded,
// because a node executes commands one at a time. The backoff is ignored — it
// exists so a hopeless record cannot crowd out a live one, and this is the last
// pass.
func (l *Listener) destroyAll(
	ctx context.Context, includeRunning bool, scope map[int64]bool,
) map[int64]bool {
	// NIL IS EVERYTHING, AN EMPTY MAP IS NOTHING, and the two must not collapse.
	// A force whose targets have all been settled hands an empty map, and reading
	// that as "no filter" would destroy every job the listener holds — the exact
	// unapproved teardown the scope exists to prevent.
	inScope := func(id int64) bool { return scope == nil || scope[id] }

	l.mu.Lock()

	requests := make([]Job, 0, len(l.running)+len(l.cleanup))

	// NOT WHAT A RETRY IS ALREADY INSIDE. That destroy is still happening, and a
	// second one would win the single teardown slot and spend the budget on work
	// already in progress while unrelated requests are never reached.
	skipped := make([]int64, 0, len(l.destroying))

	if includeRunning {
		for id := range l.running {
			if !inScope(id) {
				continue
			}

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
	}

	// ALREADY ADDED ONLY IF THE LOOP ABOVE RAN. The sets overlap — a completion
	// whose destroy failed keeps its lease in `running` AND a record here — and
	// this guard exists so such a request is destroyed once rather than once per
	// set. It was written when the running loop was unconditional, so reading it
	// as "skip anything also running" silently drops the destroy a COMPLETED job
	// is owed whenever includeRunning is false, which is every shutdown.
	alreadyAdded := func(id int64) bool {
		if !includeRunning {
			return false
		}

		_, running := l.running[id]

		return running
	}

	for id, entry := range l.cleanup {
		if entry.releaseOnly || entry.retireOnly {
			continue
		}

		if !inScope(id) {
			continue
		}

		if l.destroying[id] {
			if !alreadyAdded(id) {
				skipped = append(skipped, id)
			}

			continue
		}

		if !alreadyAdded(id) {
			requests = append(requests, entry.job)
		}
	}

	// CLAIMED UNDER THE SAME LOCK THAT SELECTED THEM, and this direction of the
	// exclusion used to be missing. `attempt` marks a request while it is inside a
	// destroy and this pass skips those — but nothing stopped a cleanup retry
	// STARTING on a request this pass had already picked. At shutdown that was
	// inert, because seal() stops the retry loop before any of this runs. A force
	// runs on a live listener with the retry loop working, so the guard has to hold
	// in both directions or two destroys race for one node's single command slot.
	for i := range requests {
		l.destroying[requests[i].RequestID] = true
	}

	claimed := requests

	defer func() {
		l.mu.Lock()

		for i := range claimed {
			delete(l.destroying, claimed[i].RequestID)
		}

		l.mu.Unlock()
	}()

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

	for i := range requests {
		wg.Add(1)

		go func(job *Job) {
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

			lease, outcome, _ := l.completionRelease(requestID)
			err := l.destroyCompleted(ctx, *job, lease, outcome)

			// CUSTODY DISCHARGES THE OBLIGATION RATHER THAN FAILING IT.
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
			persisted := true
			retired := true
			if held {
				retired = l.forgetCompletion(ctx, *job)
				if !retired {
					l.parkRetirement(*job)
				}
			} else if err == nil {
				if lease == nil {
					retired = l.forgetCompletion(ctx, *job)
					persisted = retired
					if !retired {
						l.parkRetirement(*job)
					}
				} else if persistErr := l.recordReleaseOnly(ctx, *job, lease, outcome); persistErr != nil {
					persisted = false
					l.parkReleaseOnly(*job, lease, outcome)
					l.log.Error("compute was confirmed absent, but its release-only obligation could not be made durable; capacity stays held until this is retried",
						"tier", l.tier, "request", requestID, "lease", lease.ID, "error", persistErr)
				}
			}

			mu.Lock()
			done[requestID] = (err == nil && persisted) || held
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
				if retired {
					if entry := l.cleanup[requestID]; entry == nil ||
						entry.job.CompletionID == 0 || entry.job.CompletionID == job.CompletionID {
						delete(l.cleanup, requestID)
					}
				}
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
			if entry, ok := l.cleanup[requestID]; (!ok || entry.lease == nil) && retired {
				delete(l.cleanup, requestID)
			}

			l.mu.Unlock()
		}(&requests[i])
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

	if entry.retireOnly {
		retireJob := entry.job
		l.mu.Unlock()
		l.retireParked(ctx, entry, retireJob)

		return
	}

	if entry.releaseOnly {
		l.mu.Unlock()
		if handled, settled := l.releaseParked(ctx, job.RequestID); handled && settled {
			if !l.forgetCompletion(ctx, job) {
				l.parkRetirement(job)
			}
		}

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
		} else {
			delete(l.heldOrder, lease.ID)
		}
	}

	l.held = kept

	// A surplus release may wait behind another SQLite writer. It remains owned
	// and counted until Release confirms otherwise, so it needs the same heartbeat
	// protection as ordinary idle escrow while that call is in flight.
	for id, lease := range l.releasing {
		if l.renew(ctx, lease).advertisable() {
			continue
		}

		delete(l.releasing, id)
		delete(l.confirmed, id)
		delete(l.heldOrder, id)
	}

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
			delete(l.heldOrder, p.lease.ID)
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

	live := make(map[string]struct{},
		len(l.held)+len(l.releasing)+len(l.running)+len(l.acquiring))

	for _, lease := range l.held {
		live[lease.ID] = struct{}{}
	}
	for id := range l.releasing {
		live[id] = struct{}{}
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
	return l.refillEscrowTo(ctx, math.MaxInt)
}

// prepareEscrow fills ordinary polling to assigned demand plus one discovery
// slot. Surplus is not released until GetMessage has returned after sending the
// lower advertisement; releasing first would let another tier claim capacity
// GitHub could still assign against here.
func (l *Listener) prepareEscrow(ctx context.Context) error {
	return l.refillEscrowTo(ctx, l.targetCapacity())
}

func (l *Listener) refillEscrowTo(ctx context.Context, target int) error {
	room, err := l.alloc.Headroom(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: headroom for %s: %w", l.tier, err)
	}

	// Headroom already excludes what this listener holds — those leases are open
	// in the ledger — so this tops the total up rather than doubling it.
	if target != math.MaxInt {
		if ceiling := target - l.capacity(); room > ceiling {
			room = ceiling
		}
	}

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

	// A SEALED DEPLOYMENT IS NOT A BROKEN ONE, and the difference is the whole
	// fleet. An escrow error returns from the listener, one listener returning
	// cancels every other listener, and their teardown destroys the compute they
	// are holding — so surfacing a deliberate seal as an ordinary error would
	// make `drain` the most destructive command billet has, killing exactly the
	// jobs it exists to protect.
	//
	// Having nothing to escrow is the correct reading of a seal anyway: the
	// listener advertises nothing new and carries on heartbeating, settling
	// completions and tearing down what finishes, which is what draining IS.
	if errors.Is(err, alloc.ErrAdmissionSealed) {
		l.log.Info("not escrowing: this deployment is not accepting new work",
			"tier", l.tier, "reason", err)

		return nil
	}

	if err != nil {
		return fmt.Errorf("server: escrow for %s: %w", l.tier, err)
	}

	l.mu.Lock()

	l.trackHeld(leases)
	l.held = append(l.held, leases...)
	l.sortHeld()

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
	// STARTS PRECEDE COMPLETIONS EVEN WHEN GITHUB BATCHES THEM TOGETHER. The
	// start is the authoritative runner-to-job binding; resolving the completion
	// first would either settle the request that caused launch or mistake a busy
	// runner for idle surplus.
	for i := range msg.Started {
		job, err := l.identifyStarted(ctx, msg.Started[i])
		if err != nil {
			if errors.Is(err, errQuarantinableStarted) {
				l.quarantineStarted(ctx, msg.Started[i], err)
				continue
			}

			return err
		}
		l.log.Info("a pooled runner started a job", "tier", l.tier,
			"runner", job.RunnerName, "request", job.RequestID, "job", job.JobID)
	}

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
	completed := make([]Job, 0, len(msg.Completed))
	poisoned := make([]error, 0, len(msg.Completed))

	for i := range msg.Completed {
		job := msg.Completed[i]
		job.CompletionID = msg.MessageID
		var err error
		job, err = l.identifyCompletion(ctx, job)
		if err != nil {
			if errors.Is(err, errQuarantinableCompletion) {
				poisoned = append(poisoned, err)

				continue
			}

			return err
		}
		completed = append(completed, job)
	}

	handled := *msg
	handled.Completed = completed

	var poison error
	if len(poisoned) > 0 {
		poison = &poisonedMessageError{
			cause:       errors.Join(poisoned...),
			completions: slices.Clone(completed),
		}
	}

	for i := range completed {
		job := completed[i]
		l.log.Info("received a completed job", "tier", l.tier, "request", job.RequestID,
			"runner", job.RunnerName, "result", job.Result)
		// THE DELIVERY IS TERMINAL EVEN WHEN ITS TEARDOWN ALREADY SETTLED. A
		// failed acknowledgement redelivers the whole batch, including an Assigned
		// entry for an assigned-then-cancelled job. Retired means "do not destroy
		// again", not "the assignment is live again". Stale means a newer delivery
		// already decided this request, so the older assignment is no more runnable.
		finished[job.RequestID] = struct{}{}

		disposition, err := l.recordCompletion(ctx, job)
		if err != nil {
			return err
		}
		if disposition != state.PendingCompletionActionable {
			continue
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

		if poison != nil {
			return poison
		}

		if err := l.acknowledge(ctx, &handled); err != nil {
			return err
		}
		l.acknowledgeCompletions(ctx, &handled)

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
	// A SEAL REFUSES OFFERS FOR THE SAME REASON A DRAIN DOES, and in the same
	// place. Lowering the advertisement asks GitHub to stop offering; it does not
	// stop a queued message arriving or an unacknowledged one being redelivered,
	// and either would otherwise be acquired against escrow the refill takes
	// straight back.
	if l.isDraining() || l.isQuiesced() {
		if len(msg.Available) > 0 {
			l.log.Info("declining an offer: this deployment is not taking new work",
				"tier", l.tier, "available", len(msg.Available),
				"reason", refusalReason(l.isDraining()))
		}
	} else {
		// Topped up between the release and the acquisition, so the slot just freed
		// is available to back the offer that arrived with it. It may lose the race
		// to another tier — escrow is first-come — but idle listeners ordinarily
		// keep only a discovery slot, and configured floors protect guarantees under
		// real contention. What matters here is that billet never claims work it
		// cannot back.
		if len(msg.Available) > 0 {
			observed := l.observed
			if msg.Statistics != nil {
				observed = msg.Statistics
			}
			if err := l.refillEscrowTo(ctx,
				l.targetCapacityFor(observed, len(msg.Available))); err != nil {
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
	assignmentDeficit := -1
	if msg.Statistics != nil && msg.Statistics.TotalAssignedJobs >= 0 && l.alloc != nil {
		active, err := l.activePoolMembers(ctx)
		if err != nil {
			return err
		}
		assignmentDeficit = max(msg.Statistics.TotalAssignedJobs-active, 0)
	}
	for i := range msg.Assigned {
		job, err := l.identifyAssigned(ctx, msg.Assigned[i])
		if err != nil {
			return err
		}
		if _, over := finished[job.RequestID]; over {
			continue
		}
		if assignmentDeficit == 0 {
			l.releasePromise(job.RequestID)
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
		if assignmentDeficit > 0 {
			assignmentDeficit--
		}
	}

	if msg.Statistics != nil {
		l.observed = msg.Statistics
		if err := l.reconcilePool(ctx, msg.Statistics.TotalAssignedJobs); err != nil {
			return err
		}
	}

	if poison != nil {
		return poison
	}

	if err := l.acknowledge(ctx, &handled); err != nil {
		return err
	}
	l.acknowledgeCompletions(ctx, &handled)

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

// quarantineStarted confines a contradictory identity to the one pool member.
// A scale-set message is shared infrastructure; taking every tier down over one
// runner makes an already-bad registration a fleet-wide outage.
func (l *Listener) quarantineStarted(ctx context.Context, job Job, cause error) {
	l.log.Error("a pooled runner reported a contradictory job identity; retiring only that member",
		"tier", l.tier, "runner", job.RunnerName, "runner_id", job.RunnerID,
		"job", job.JobID, "error", cause)
	if l.alloc != nil && job.RunnerName != "" {
		member, err := l.alloc.PoolRunnerByName(ctx, job.RunnerName)
		if errors.Is(err, alloc.ErrLeaseNotFound) {
			if leaseID, ok := provider.LeaseOf(job.RunnerName); ok {
				if legacy, leaseErr := l.alloc.JobForLease(ctx, leaseID); leaseErr == nil &&
					legacy.Tier == l.tier && legacy.RequestID != 0 {
					regErr := l.alloc.RegisterPoolRunner(ctx, alloc.PoolRunner{LeaseID: leaseID,
						Tier: l.tier, LaunchRequestID: legacy.RequestID, RunnerID: job.RunnerID,
						RunnerName: job.RunnerName})
					if regErr == nil {
						member, err = l.alloc.PoolRunnerByName(ctx, job.RunnerName)
					} else {
						err = regErr
					}
				}
			}
		}
		if err == nil {
			if member.Tier != l.tier {
				l.log.Error("refused to quarantine a runner owned by another tier",
					"tier", l.tier, "runner", job.RunnerName, "owner_tier", member.Tier)
				return
			}
			if retireErr := l.alloc.RetirePoolRunner(ctx, member.LeaseID); retireErr != nil &&
				!errors.Is(retireErr, alloc.ErrLeaseNotFound) {
				l.log.Error("could not journal the contradictory runner's retirement",
					"tier", l.tier, "runner", job.RunnerName, "error", retireErr)
				return
			}
			member.Status = alloc.PoolRunnerRetiring
			l.retirePoolMember(ctx, member)
			return
		}
		if !errors.Is(err, alloc.ErrLeaseNotFound) {
			l.log.Error("could not resolve the contradictory runner for retirement",
				"tier", l.tier, "runner", job.RunnerName, "error", err)
			return
		}
	}
	if l.registry != nil && (job.RunnerID > 0 || job.RunnerName != "") {
		if err := l.registry.RemoveRunner(ctx, job.RunnerID, job.RunnerName); err != nil {
			l.log.Error("could not remove an unrecognized contradictory registration",
				"tier", l.tier, "runner", job.RunnerName, "error", err)
		}
		// Deliberately NOT MarkDeregistered here. The removed name resolves to a
		// lease only by string, and RemoveRunner returning nil proves only that
		// this name is absent — the encoded lease may be another tier's, or still
		// in flight and about to register its own runner. Marking it would
		// false-exclude a live runner (a double-schedule); leaving it counted is
		// at worst a transient over-count that terminalization clears.
	}
}

// reconcilePool matches a tier to GitHub's authoritative assigned-job count.
// Growth creates anonymous physical members because individual job entries are
// lifecycle data and may be truncated. Only idle members are selected for
// shrinkage; a busy member belongs to a job regardless of what any aggregate
// says while messages are in flight.
func (l *Listener) reconcilePool(ctx context.Context, desired int) error {
	if l.alloc == nil || desired < 0 {
		return nil
	}
	runners, err := l.alloc.PoolRunners(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: read runner pool for %s reconciliation: %w", l.tier, err)
	}

	for i := range runners {
		if runners[i].Status == alloc.PoolRunnerRetiring {
			l.retirePoolMember(ctx, runners[i])
		}
	}

	runners, err = l.alloc.PoolRunners(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: refresh runner pool for %s reconciliation: %w", l.tier, err)
	}
	active, err := l.alloc.ActiveRunnerLeases(ctx, l.tier)
	if err != nil {
		return fmt.Errorf("server: count active runner leases for %s reconciliation: %w", l.tier, err)
	}
	for active < desired {
		lease, job, ok, err := l.assignPoolSlot(ctx)
		if err != nil {
			return err
		}
		if !ok {
			break
		}
		if err := l.launch(ctx, lease, job); err != nil {
			return err
		}
		active++
	}
	surplus := active - desired
	if surplus <= 0 {
		return nil
	}
	for i := range runners {
		if surplus == 0 {
			break
		}
		if runners[i].Status != alloc.PoolRunnerIdle {
			continue
		}
		if err := l.alloc.RetirePoolRunner(ctx, runners[i].LeaseID); err != nil {
			l.log.Error("could not claim an idle pool member for scale-down", "tier", l.tier,
				"runner", runners[i].RunnerName, "error", err)
			continue
		}
		runners[i].Status = alloc.PoolRunnerRetiring
		l.retirePoolMember(ctx, runners[i])
		surplus--
	}

	return nil
}

func (l *Listener) activePoolMembers(ctx context.Context) (int, error) {
	active, err := l.alloc.ActiveRunnerLeases(ctx, l.tier)
	if err != nil {
		return 0, fmt.Errorf("server: read runner pool for %s assignment: %w", l.tier, err)
	}

	return active, nil
}

// assignPoolSlot turns one escrowed lease into a physical runner whose durable
// identity is the lease rather than one entry from GitHub's truncated message.
func (l *Listener) assignPoolSlot(ctx context.Context) (*alloc.Lease, Job, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	var (
		lease     *alloc.Lease
		promiseID int64
	)
	for id, promised := range l.acquiring {
		if lease == nil || promised.at.Before(l.acquiring[promiseID].at) ||
			(promised.at.Equal(l.acquiring[promiseID].at) && id < promiseID) {
			lease = promised.lease
			promiseID = id
		}
	}
	fromPromise := lease != nil
	if !fromPromise {
		if len(l.held) == 0 {
			return nil, Job{}, false, nil
		}
		lease = l.held[0]
	}

	requestID, err := l.alloc.IdentifyPoolSlot(ctx, lease.ID)
	if err != nil {
		return nil, Job{}, false, fmt.Errorf("server: identify pool slot for lease %s: %w", lease.ID, err)
	}
	if err := l.alloc.Assign(ctx, lease.ID, lease.Epoch, 0, requestID); err != nil {
		return nil, Job{}, false, fmt.Errorf("server: assign pool slot lease %s: %w", lease.ID, err)
	}

	if fromPromise {
		delete(l.acquiring, promiseID)
	} else {
		l.held = l.held[1:]
	}
	delete(l.heldOrder, lease.ID)
	l.running[requestID] = lease

	return lease, Job{RequestID: requestID}, true, nil
}

// retirePoolMember removes routing before compute, then returns its capacity.
// The retiring row is the crash-recovery journal: any failed phase is retried on
// the next scale-set message before another idle member is selected.
func (l *Listener) retirePoolMember(ctx context.Context, member alloc.PoolRunner) {
	lease, err := l.alloc.Lease(ctx, member.LeaseID)
	if err != nil && !errors.Is(err, alloc.ErrLeaseNotFound) {
		l.log.Error("could not read a retiring pool member's lease", "tier", l.tier,
			"runner", member.RunnerName, "lease", member.LeaseID, "error", err)
		return
	}
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		lease = nil
	}
	job := Job{RequestID: member.LaunchRequestID, RunnerID: member.RunnerID,
		RunnerName: member.RunnerName}
	if err := l.destroyCompleted(ctx, job, lease, alloc.PhaseDone); err != nil {
		if errors.Is(err, ErrCustody) {
			// KEPT UNTIL CUSTODY SETTLES. A recovery tombstone is the fence that
			// stops a delayed JobStarted recreating a busy binding after the node
			// was already told to tear this guest down. The next reconciliation
			// retries and forgets it only after custody no longer owns compute.
			l.dropPoolMember(member)
			return
		}
		l.log.Error("could not retire an idle pool member; its capacity stays held", "tier", l.tier,
			"runner", member.RunnerName, "error", err)
		return
	}
	if lease != nil {
		if err := l.releaseAbsent(ctx, member.LaunchRequestID, lease, alloc.PhaseDone, ""); !releaseSettled(err) {
			l.log.Error("could not release a retired pool member; its capacity stays held", "tier", l.tier,
				"runner", member.RunnerName, "lease", lease.ID, "error", err)
			return
		}
	}
	if err := l.alloc.ForgetPoolRunner(ctx, member.LeaseID); err != nil {
		l.log.Warn("a retired pool member's journal could not be removed", "tier", l.tier,
			"runner", member.RunnerName, "error", err)
		return
	}
	l.dropPoolMember(member)
}

func (l *Listener) dropPoolMember(member alloc.PoolRunner) {
	l.mu.Lock()
	delete(l.running, member.LaunchRequestID)
	delete(l.acquiring, member.LaunchRequestID)
	delete(l.confirmed, member.LeaseID)
	l.mu.Unlock()
}

// identifyAssigned gives a direct assignment its durable scheduler identity.
func (l *Listener) identifyAssigned(ctx context.Context, job Job) (Job, error) {
	if job.RequestID != 0 {
		return job, nil
	}
	if l.alloc == nil {
		return Job{}, fmt.Errorf("%w: %s assigned job %q without a request id, and no ledger is available to identify it",
			ErrUntrustworthySession, l.tier, job.JobID)
	}

	requestID, err := l.alloc.IdentifyDirectJob(ctx, job.JobID)
	if err != nil {
		return Job{}, fmt.Errorf("%w: %s cannot identify directly assigned job %q: %w",
			ErrUntrustworthySession, l.tier, job.JobID, err)
	}
	job.RequestID = requestID

	return job, nil
}

// identifyStarted records which job a registered pool member actually consumed.
func (l *Listener) identifyStarted(ctx context.Context, job Job) (Job, error) {
	if l.alloc == nil {
		return Job{}, fmt.Errorf("%w: %s started runner %q without a ledger",
			ErrUntrustworthySession, l.tier, job.RunnerName)
	}
	if job.RunnerID <= 0 || job.RunnerName == "" || job.JobID == "" {
		return Job{}, fmt.Errorf("%w: %s received an incomplete started identity for runner %q",
			errQuarantinableStarted, l.tier, job.RunnerName)
	}
	identified, err := l.identifyAssigned(ctx, job)
	if err != nil {
		return Job{}, err
	}
	member, err := l.alloc.PoolRunnerByName(ctx, job.RunnerName)
	leaseID := member.LeaseID
	switch {
	case errors.Is(err, alloc.ErrLeaseNotFound):
		var ok bool
		leaseID, ok = provider.LeaseOf(job.RunnerName)
		if !ok {
			return Job{}, fmt.Errorf("%w: started runner %q has no Billet lease identity",
				errQuarantinableStarted, job.RunnerName)
		}
		legacy, leaseErr := l.alloc.JobForLease(ctx, leaseID)
		if leaseErr != nil {
			if errors.Is(leaseErr, alloc.ErrLeaseNotFound) {
				return Job{}, fmt.Errorf("%w: cannot adopt unknown started runner %q",
					errQuarantinableStarted, job.RunnerName)
			}
			return Job{}, fmt.Errorf("server: read lease identity for started runner %q: %w",
				job.RunnerName, leaseErr)
		}
		if legacy.Tier != l.tier || legacy.RequestID == 0 {
			return Job{}, fmt.Errorf("%w: started runner %q resolves outside tier %q",
				ErrUntrustworthySession, job.RunnerName, l.tier)
		}
		if regErr := l.alloc.RegisterPoolRunner(ctx, alloc.PoolRunner{LeaseID: leaseID,
			Tier: l.tier, LaunchRequestID: legacy.RequestID, RunnerName: job.RunnerName}); regErr != nil {
			if errors.Is(regErr, alloc.ErrConflict) {
				return Job{}, fmt.Errorf("%w: cannot adopt started runner %q: %w",
					errQuarantinableStarted, job.RunnerName, regErr)
			}
			return Job{}, fmt.Errorf("server: adopt pool runner %q: %w", job.RunnerName, regErr)
		}
	case err != nil:
		return Job{}, fmt.Errorf("server: read pool runner %q: %w", job.RunnerName, err)
	case member.Tier != l.tier:
		return Job{}, fmt.Errorf("%w: started runner %q belongs to tier %q, not %q",
			ErrUntrustworthySession, job.RunnerName, member.Tier, l.tier)
	}

	if _, err := l.alloc.StartPoolRunner(ctx, leaseID, l.tier, job.RunnerID,
		job.RunnerName, identified.RequestID, identified.RunID, identified.JobID); err != nil {
		if errors.Is(err, alloc.ErrConflict) || errors.Is(err, alloc.ErrLeaseNotFound) {
			return Job{}, fmt.Errorf("%w: cannot bind started runner %q: %w",
				errQuarantinableStarted, job.RunnerName, err)
		}
		return Job{}, fmt.Errorf("server: bind started runner %q: %w", job.RunnerName, err)
	}
	return identified, nil
}

// identifyCompletion resolves a zero wire id through the runner's lease, or
// through job id when there is no durable lease identity to recover.
func (l *Listener) identifyCompletion(ctx context.Context, job Job) (Job, error) {
	if job.RunnerName == "" && job.RequestID != 0 {
		return job, nil
	}
	if l.alloc == nil {
		return Job{}, fmt.Errorf("%w: %s completed runner %q without a request id, and no ledger is available to resolve it",
			ErrUntrustworthySession, l.tier, job.RunnerName)
	}

	// THE RUNNER, NOT runnerRequestId, IS THE COMPUTE IDENTITY. The request id
	// describes the job that completed and may belong to a different pool member.
	if job.RunnerName != "" {
		binding, err := l.alloc.PoolRunnerByName(ctx, job.RunnerName)
		switch {
		case err == nil:
			if binding.Tier != l.tier || binding.LaunchRequestID == 0 {
				return Job{}, fmt.Errorf("%w: completed runner %q belongs to tier %q",
					ErrUntrustworthySession, job.RunnerName, binding.Tier)
			}
			if err := l.restorePoolLease(ctx, binding); err != nil {
				return Job{}, err
			}
			if binding.Status == alloc.PoolRunnerBusy {
				if job.JobID != "" && binding.JobID != "" && job.JobID != binding.JobID {
					return Job{}, fmt.Errorf("%w: completed runner %q names job %q after starting %q",
						ErrUntrustworthySession, job.RunnerName, job.JobID, binding.JobID)
				}
				if job.RequestID != 0 && binding.ActualRequestID != 0 &&
					job.RequestID != binding.ActualRequestID {
					return Job{}, fmt.Errorf("%w: completed runner %q names request %d after starting %d",
						ErrUntrustworthySession, job.RunnerName, job.RequestID, binding.ActualRequestID)
				}
			}
			job.RequestID = binding.LaunchRequestID
			return job, nil
		case !errors.Is(err, alloc.ErrLeaseNotFound):
			return Job{}, fmt.Errorf("%w: cannot resolve completed runner %q: %w",
				ErrUntrustworthySession, job.RunnerName, err)
		}
	}

	if job.RequestID != 0 {
		return job, nil
	}

	leaseID, ok := provider.LeaseOf(job.RunnerName)
	if ok {
		identity, err := l.alloc.JobForLease(ctx, leaseID)
		switch {
		case err == nil:
			if identity.Tier != l.tier || identity.RequestID == 0 {
				return Job{}, fmt.Errorf("%w: completed runner %q resolves to tier %q request %d, not tier %q",
					ErrUntrustworthySession, job.RunnerName, identity.Tier, identity.RequestID, l.tier)
			}
			// A RUNNER AND ITS INTENDED JOB ARE A POOL, NOT A PAIR, and treating a
			// disagreement as a broken contract took the control plane down. Within
			// one scale set GitHub hands an assigned job to whichever registered
			// runner is free, so two jobs launched seconds apart can swap runners —
			// measured live, the first time a full suite ran concurrently: the
			// runner billet started for request -49 completed the job mapped to
			// -52, honestly. The old fatal made GitHub redeliver the same message
			// to every fresh session, which is a restart loop with no exit.
			//
			// THE RUNNER'S LEASE IS THE IDENTITY THAT SETTLES. The completion says
			// THIS guest finished with THIS result, which is exactly what its
			// capacity release and cache settlement need. The job-side facts (run
			// id, job id) stay from the message because they describe the job that
			// really ran here. Pool reconciliation separately scales down any idle
			// registration that was reserved for this job but never consumed it.
			if job.RunID != 0 && identity.RunID != 0 && job.RunID != identity.RunID {
				l.log.Warn("a completed runner ran a job from a different run than the one "+
					"it was launched for; github pools assigned jobs across a scale set's "+
					"runners, so its lease settles under the run it actually executed",
					"tier", l.tier, "runner", job.RunnerName,
					"ran", job.RunID, "launched_for", identity.RunID)
			}
			if job.JobID != "" {
				mapped, exists, err := l.alloc.DirectJobIdentity(ctx, job.JobID)
				if err != nil {
					return Job{}, fmt.Errorf("%w: %s cannot cross-check completed job %q: %w",
						ErrUntrustworthySession, l.tier, job.JobID, err)
				}
				if exists && mapped != identity.RequestID {
					l.log.Warn("a completed runner ran a different assigned job than the one "+
						"it was launched for; github pools assigned jobs across a scale set's "+
						"runners, so this runner's lease settles with the result while idle "+
						"surplus is retired from the authoritative assigned-job count",
						"tier", l.tier, "runner", job.RunnerName,
						"launched_for", identity.RequestID, "job", job.JobID, "ran", mapped)
				}
			}

			job.RequestID = identity.RequestID
			if job.RunID == 0 {
				job.RunID = identity.RunID
			}

			return job, nil
		case !errors.Is(err, alloc.ErrLeaseNotFound):
			return Job{}, fmt.Errorf("%w: %s cannot resolve completed runner %q: %w",
				ErrUntrustworthySession, l.tier, job.RunnerName, err)
		}
	}

	if job.JobID != "" {
		return l.identifyAssigned(ctx, job)
	}

	return Job{}, fmt.Errorf("%w: %s completed runner %q without a request id, job id, or resolvable billet lease",
		errQuarantinableCompletion, l.tier, job.RunnerName)
}

// restorePoolLease reconnects restart-safe pool identity to the completion
// machinery, whose durable result record must carry the exact lease and epoch.
// Without this handoff a post-restart completion could destroy compute and then
// forget which capacity it had proved safe to release.
func (l *Listener) restorePoolLease(ctx context.Context, binding alloc.PoolRunner) error {
	lease, err := l.alloc.Lease(ctx, binding.LeaseID)
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: cannot restore completed runner %q lease %s: %w",
			ErrUntrustworthySession, binding.RunnerName, binding.LeaseID, err)
	}
	if lease.Tier != l.tier || lease.RequestID != binding.LaunchRequestID {
		return fmt.Errorf("%w: pool runner %q lease %s identifies tier %q request %d, want tier %q request %d",
			ErrUntrustworthySession, binding.RunnerName, binding.LeaseID, lease.Tier,
			lease.RequestID, l.tier, binding.LaunchRequestID)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if prior := l.running[binding.LaunchRequestID]; prior != nil && prior.ID != lease.ID {
		return fmt.Errorf("%w: request %d is already bound to lease %s, not pool lease %s",
			ErrUntrustworthySession, binding.LaunchRequestID, prior.ID, lease.ID)
	}
	// This lease is now managed in running, so it is no longer adopted compute;
	// dropping it here avoids a one-iteration double-count before the next
	// refreshAdoptedCapacity would reconcile it.
	delete(l.adopted, lease.ID)
	l.running[binding.LaunchRequestID] = lease

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

// acknowledgeCompletions records that GitHub will not redeliver the
// source message. The store removes a row only once it is also retired.
func (l *Listener) acknowledgeCompletions(ctx context.Context, msg *Message) {
	persistCtx, cancel := withoutCancelWithin(ctx, l.releaseGrace)
	defer cancel()
	for i := range msg.Completed {
		job := &msg.Completed[i]
		if l.completionStore != nil {
			if err := l.completionStore.AcknowledgePendingCompletion(
				persistCtx, l.tier, job.RequestID, msg.MessageID,
			); err != nil {
				l.log.Error("a completion acknowledgement could not be made durable; recovery remains conservatively blocked",
					"tier", l.tier, "request", job.RequestID, "message", msg.MessageID, "error", err)
			}
		}
		if l.alloc != nil {
			if err := l.alloc.AcknowledgePoolRunner(persistCtx, l.tier, job.RequestID); err != nil {
				l.log.Error("a pool runner acknowledgement could not be made durable; its retired identity remains conservatively reserved",
					"tier", l.tier, "request", job.RequestID, "message", msg.MessageID, "error", err)
			}
		}
	}
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

	identified := make([]Job, 0, len(available))
	protocolFor := make(map[int64]int64, len(available))
	internalFor := make(map[int64]int64, len(available))
	for i := range available {
		protocolID := available[i].RequestID
		job, err := l.identifyAssigned(ctx, available[i])
		if err != nil {
			return err
		}
		identified = append(identified, job)
		protocolFor[job.RequestID] = protocolID
		if prior, duplicate := internalFor[protocolID]; duplicate && prior != job.RequestID {
			return fmt.Errorf("%w: %s offered distinct jobs %d and %d under the same runner request id %d; the acquisition response cannot distinguish them",
				ErrUntrustworthySession, l.tier, prior, job.RequestID, protocolID)
		}
		internalFor[protocolID] = job.RequestID
	}

	reservedInternal := l.reserve(identified)
	if len(reservedInternal) == 0 {
		return nil
	}
	reservedProtocol := make([]int64, 0, len(reservedInternal))
	for _, internalID := range reservedInternal {
		protocolID := protocolFor[internalID]
		reservedProtocol = append(reservedProtocol, protocolID)
	}

	acquiredProtocol, err := l.session.AcquireJobs(ctx, reservedProtocol)
	if err != nil {
		// Nothing was promised, so nothing stays reserved.
		l.unreserve(reservedInternal)

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
	if extra := missing(acquiredProtocol, reservedProtocol); len(extra) > 0 {
		// Everything, not just the unmatched ids. Which commitments are real is
		// exactly what is no longer known.
		l.unreserve(reservedInternal)

		return fmt.Errorf("%w: %s acquired job requests it did not offer for "+
			"(unrequested %v, requested %v); refusing to continue against a scale-set "+
			"response that is not a subset of its request",
			ErrUntrustworthySession, l.tier, extra, reservedProtocol)
	}

	// GitHub returns what it ACTUALLY gave, which can be fewer than were asked
	// for — another scale set can win the same offer. Escrow reserved for an
	// offer billet did not get goes back immediately; holding it would strand
	// capacity waiting for an assignment that is never coming.
	acquiredInternal := make([]int64, 0, len(acquiredProtocol))
	for _, protocolID := range acquiredProtocol {
		acquiredInternal = append(acquiredInternal, internalFor[protocolID])
	}
	l.unreserve(missing(reservedInternal, acquiredInternal))

	return nil
}

// reserve moves escrow from held into acquiring, one lease per offer, and
// returns the request ids it could back.
func (l *Listener) reserve(available []Job) []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()

	ids := make([]int64, 0, len(available))

	for i := range available {
		job := &available[i]
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
	l.sortHeld()
}

// releasePromise returns request-scoped escrow once an existing anonymous pool
// member already backs the assignment. Keeping it would reserve a second
// machine for one desired runner.
func (l *Listener) releasePromise(id int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if promised, ok := l.acquiring[id]; ok {
		delete(l.acquiring, id)
		l.held = append(l.held, promised.lease)
		l.sortHeld()
	}
}

func (l *Listener) trackHeld(leases []*alloc.Lease) {
	for _, lease := range leases {
		if _, tracked := l.heldOrder[lease.ID]; tracked {
			continue
		}
		l.nextHeldOrder++
		l.heldOrder[lease.ID] = l.nextHeldOrder
	}
}

// sortHeld keeps provider preference ahead of fallback placement, then restores
// the allocator's issuance order within a provider after unreserve appends.
func (l *Listener) sortHeld() {
	slices.SortStableFunc(l.held, func(a, b *alloc.Lease) int {
		if rank := cmp.Compare(a.PreferenceRank, b.PreferenceRank); rank != 0 {
			return rank
		}
		return cmp.Compare(l.heldOrder[a.ID], l.heldOrder[b.ID])
	})
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
	delete(l.heldOrder, lease.ID)

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
			binding, regErr := l.alloc.PoolRunnerByLease(ctx, lease.ID)
			if errors.Is(regErr, alloc.ErrLeaseNotFound) {
				regErr = l.alloc.RegisterPoolRunner(ctx, alloc.PoolRunner{LeaseID: lease.ID,
					Tier: l.tier, LaunchRequestID: job.RequestID,
					RunnerName: provider.InstanceName(lease.ID)})
			} else if regErr == nil && (binding.Tier != l.tier ||
				binding.LaunchRequestID != job.RequestID) {
				regErr = fmt.Errorf("%w: lease is registered for tier %q request %d",
					alloc.ErrConflict, binding.Tier, binding.LaunchRequestID)
			}
			if regErr != nil {
				return fmt.Errorf("server: verify pool runner for lease %s: %w", lease.ID, regErr)
			}
			return nil
		}

		l.log.Error("the lease was reclaimed while its job was starting; destroying the "+
			"compute, which is no longer backed by any capacity",
			"tier", l.tier, "request", job.RequestID, "lease", lease.ID)

		if destroyErr := l.destroyCompleted(ctx, job, lease, alloc.PhaseFailed); destroyErr != nil {
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
	registrationCtx, registrationCancel := withoutCancelWithin(ctx, l.releaseGrace)
	defer registrationCancel()
	binding, bindingErr := l.alloc.PoolRunnerByLease(registrationCtx, lease.ID)
	if bindingErr == nil {
		if retireErr := l.alloc.RetirePoolRunner(registrationCtx, lease.ID); retireErr != nil {
			l.log.Error("a failed launch's registration could not be claimed for cleanup; its capacity stays held",
				"tier", l.tier, "request", job.RequestID, "lease", lease.ID, "error", retireErr)
			return nil
		}
		binding.Status = alloc.PoolRunnerRetiring
		l.retirePoolMember(registrationCtx, binding)
		return nil
	}
	if !errors.Is(bindingErr, alloc.ErrLeaseNotFound) {
		l.log.Error("a failed launch's registration could not be resolved; its capacity stays held",
			"tier", l.tier, "request", job.RequestID, "lease", lease.ID, "error", bindingErr)
		return nil
	}

	l.log.Error("could not start the compute for an assigned job; handing the capacity back",
		"tier", l.tier, "request", job.RequestID, "run", job.RunID,
		"lease", lease.ID, "error", err)

	// Not fatal. The launch already failed; failing the listener as well would
	// take every tier down over one job that GitHub will simply reassign.
	//
	// WITH ITS REASON, IN THE SAME TRANSACTION. Nothing outlives a process here
	// — the lease never held compute and archives at once — so the reason is
	// for the report: `billet leases failures` shows a failure nothing explains
	// as a row an operator cannot act on, and the node writes the same reason
	// for a launch that failed after starting something.
	relErr := l.alloc.ReleaseFailed(ctx, lease.ID, lease.Epoch, alloc.LaunchFailedReason)
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
				failureReason: alloc.LaunchFailedReason,
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
// terminal; ErrConflict means it is terminal with the opposite outcome. None can
// be improved by holding a reference and trying again — but a busy database or a
// cancelled context can.
func releaseSettled(err error) bool {
	return err == nil || errors.Is(err, alloc.ErrFenced) || errors.Is(err, alloc.ErrLeaseNotFound) ||
		errors.Is(err, alloc.ErrConflict)
}

// releaseAbsent returns capacity after the runner has proved no compute exists.
// A fenced release may name a lease the reaper quarantined while the proof was
// in flight; ResolveQuarantine is the only operation that turns that proof into
// returned capacity.
func (l *Listener) releaseAbsent(ctx context.Context, requestID int64, lease *alloc.Lease,
	outcome alloc.Phase, reason string,
) error {
	relErr := l.release(ctx, lease, outcome, reason)
	if !errors.Is(relErr, alloc.ErrFenced) {
		return relErr
	}

	if err := l.resolveQuarantine(ctx, lease, outcome, reason); err == nil {
		l.log.Warn("a cleanup release found its lease quarantined after compute was "+
			"confirmed gone; the capacity is back",
			"tier", l.tier, "request", requestID, "lease", lease.ID)

		return nil
	} else if !errors.Is(err, alloc.ErrLeaseNotFound) {
		return err
	}

	return relErr
}

// release terminalizes a lease with the outcome this listener decided, and
// with its reason when the outcome is a failure this listener explains.
//
// ONE TRANSACTION FOR BOTH, because a release that lands leaves nothing to
// carry the reason afterwards: the archive copies what the row holds at that
// moment, and a reason written a call later would explain a history row that
// has already closed. An ordinary release — a finished job, or a failure
// somebody else already explained — is exactly what it was.
func (l *Listener) release(ctx context.Context, lease *alloc.Lease, outcome alloc.Phase, reason string) error {
	if outcome == alloc.PhaseFailed && reason != "" {
		return l.alloc.ReleaseFailed(ctx, lease.ID, lease.Epoch, reason)
	}

	return l.alloc.Release(ctx, lease.ID, lease.Epoch, outcome)
}

// resolveQuarantine settles a quarantined lease with the outcome this listener
// decided, carrying its reason when the outcome is a failure it explains —
// the reaper reaches a parked launch failure before the retry does, and the
// reason must not be lost to the route the release took.
func (l *Listener) resolveQuarantine(ctx context.Context, lease *alloc.Lease, outcome alloc.Phase, reason string) error {
	if outcome == alloc.PhaseFailed && reason != "" {
		return l.alloc.ResolveQuarantineFailed(ctx, lease.ID, reason)
	}

	return l.alloc.ResolveQuarantine(ctx, lease.ID, outcome)
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
	job := entry.job
	outcome := entry.outcome
	reason := entry.failureReason
	if outcome == "" {
		outcome = alloc.PhaseDone
	}
	l.mu.Unlock()

	if err := l.recordReleaseOnly(ctx, job, lease, outcome); err != nil {
		l.mu.Lock()
		current, stillPending := l.cleanup[requestID]
		if stillPending && current == entry {
			entry.failed(time.Now(), l.retryFirst, l.retryMax)
		}
		l.mu.Unlock()
		l.log.Error("compute is confirmed absent, but capacity stays held until its release-only obligation is durable",
			"tier", l.tier, "request", requestID, "lease", lease.ID, "error", err)

		return true, false
	}

	relErr := l.releaseAbsent(ctx, requestID, lease, outcome, reason)

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

func (l *Listener) recordCompletion(
	ctx context.Context,
	job Job,
) (state.PendingCompletionDisposition, error) {
	if l.completionStore == nil || job.Result == "" {
		// NOTHING DURABLE TO BE SUPERSEDED BY. With no completion store there is
		// no stored delivery to compare this one against, so it is actionable by
		// construction and its result may be recorded.
		lease, _, _ := l.completionRelease(job.RequestID)
		l.recordJobResult(ctx, job, leaseIDOf(lease))

		return state.PendingCompletionActionable, nil
	}

	lease, outcome, releaseOnly := l.completionRelease(job.RequestID)
	completion := state.PendingCompletion{
		Tier: l.tier, RequestID: job.RequestID, RunID: job.RunID, Result: job.Result,
		MessageID: job.CompletionID,
	}
	if lease != nil {
		completion.LeaseID = lease.ID
		completion.LeaseEpoch = lease.Epoch
		completion.LeaseNode = lease.Node
		completion.Outcome = string(outcome)
		completion.ReleaseOnly = releaseOnly
	}
	disposition, err := l.completionStore.PutPendingCompletion(ctx, completion)
	if err != nil {
		return state.PendingCompletionActionable, fmt.Errorf("server: preserve completed result for %s request %d before acknowledging it: %w",
			l.tier, job.RequestID, err)
	}

	// AFTER THE DISPOSITION, NEVER BEFORE IT, and that ordering is the whole
	// safety of the record.
	//
	// GitHub reuses a request id and the escrow maps are keyed on it, so a STALE
	// redelivery of an old completion resolves to whatever lease holds that id
	// NOW. Recorded ahead of this decision, an old job's result lands on its
	// replacement's history: it fabricates an attributed failure for a job that
	// has not finished, and worse, it makes disruptionGuard refuse a real
	// disruption against that lease — a recorded result is exactly how the guard
	// knows GitHub has already reported.
	//
	// AND AGAINST THE LEASE THIS DELIVERY WAS RECORDED WITH, the snapshot
	// PutPendingCompletion has just made durable, rather than a second read of a
	// map that may have moved underneath it.
	if disposition == state.PendingCompletionActionable {
		l.recordJobResult(ctx, job, leaseIDOf(lease))
	}

	return disposition, nil
}

// recordJobResult stores GitHub's own conclusion for a finished job, so that a
// failure can later be read beside whatever billet's infrastructure was doing to
// that lease. It never re-runs anything and decides nothing.
//
// IT CANNOT FAIL THE CALLER, and that is not tidiness. recordCompletion's error
// is fatal: it stops this listener, which cancels every other listener, whose
// shutdowns destroy the jobs running on this host. A busy database while GitHub
// happened to report a completion would take the deployment down for the sake of
// a diagnostic — the same disproportion complete() already refuses for an
// unbacked assignment.
//
// THE LEASE IS HANDED IN RATHER THAN LOOKED UP, so it is the same one the caller
// settled the delivery's disposition against. Re-reading the escrow here would
// reopen the reused-request-id hole the caller just closed. Where the caller has
// none, the lease id encoded in the runner's name is the only route left after a
// restart lost the maps — the same fallback complete() uses.
//
// WHAT STAYS OPEN, said rather than papered over. That fallback resolves a lease
// this listener does NOT hold, so `pending_completions.lease_id` is empty for it
// — correctly, since teardown there is not this process's to finish — and the row
// carries no runner name either. So for that one class, a crash between
// PutPendingCompletion and this write is recovered by GitHub's redelivery and
// only by that: if the cleanup loop settles and retires the row first, every
// later delivery is refused and the diagnostic is gone. Closing it needs the
// result's own lease identity persisted beside the teardown one, which is a
// column and a migration for a missing report line — worth doing when something
// else needs that column, and not worth a seventh change to this path now. A
// line here that looked like it closed it would be worse: the obvious one
// (recording again from complete) survives every mutation, because complete's
// restored job carries no runner name to resolve.
func leaseIDOf(lease *alloc.Lease) string {
	if lease == nil {
		return ""
	}

	return lease.ID
}

func (l *Listener) recordJobResult(ctx context.Context, job Job, leaseID string) {
	if l.alloc == nil || job.Result == "" {
		return
	}

	if leaseID == "" && job.RunnerName != "" {
		if encoded, ok := provider.LeaseOf(job.RunnerName); ok {
			leaseID = encoded
		}
	}

	if leaseID == "" {
		return
	}

	if err := l.alloc.RecordJobResult(ctx, leaseID, job.Result, job.RunID); err != nil {
		l.log.Warn("could not record what github concluded about a finished job; "+
			"`billet leases failures` may not be able to show whether billet's own "+
			"infrastructure was disrupted while its lease could still have been "+
			"running it",
			"tier", l.tier, "request", job.RequestID, "lease", leaseID, "error", err)
	}
}

// recordReleaseOnly makes successful node teardown durable before the local
// lease release is attempted. It deliberately outlives cancellation for one
// bounded local-write budget: otherwise shutdown would preserve a row that asks
// restart recovery to contact a node for compute already proved absent.
func (l *Listener) recordReleaseOnly(
	ctx context.Context,
	job Job,
	lease *alloc.Lease,
	outcome alloc.Phase,
) error {
	if l.completionStore == nil || job.Result == "" || lease == nil {
		return nil
	}
	if outcome == "" {
		outcome = alloc.PhaseDone
	}
	persistCtx, cancel := withoutCancelWithin(ctx, l.releaseGrace)
	defer cancel()
	completion := state.PendingCompletion{
		Tier: l.tier, RequestID: job.RequestID, RunID: job.RunID, Result: job.Result,
		LeaseID: lease.ID, LeaseEpoch: lease.Epoch, LeaseNode: lease.Node,
		Outcome: string(outcome), ReleaseOnly: true, MessageID: job.CompletionID,
	}
	disposition, err := l.completionStore.PutPendingCompletion(persistCtx, completion)
	if err != nil {
		return fmt.Errorf("server: preserve release-only completion for %s request %d: %w",
			l.tier, job.RequestID, err)
	}
	if disposition != state.PendingCompletionActionable {
		return fmt.Errorf("server: completion for %s request %d message %d is no longer current",
			l.tier, job.RequestID, job.CompletionID)
	}

	return nil
}

// withoutCancelWithin survives an ordinary caller cancellation without extending
// an existing shutdown deadline.
func withoutCancelWithin(ctx context.Context, grace time.Duration) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < grace {
		return context.WithDeadline(base, deadline)
	}

	return context.WithTimeout(base, grace)
}

// parkReleaseOnly retains capacity whose compute is gone until persistence and
// allocator release both settle.
func (l *Listener) parkReleaseOnly(job Job, lease *alloc.Lease, outcome alloc.Phase) {
	if lease == nil {
		return
	}
	if outcome == "" {
		outcome = alloc.PhaseDone
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cleanup == nil {
		l.cleanup = make(map[int64]*pendingCleanup)
	}
	entry := l.cleanup[job.RequestID]
	if entry != nil && entry.job.CompletionID > job.CompletionID {
		return
	}
	if entry == nil {
		entry = &pendingCleanup{job: job}
		l.cleanup[job.RequestID] = entry
	} else if job.Result != "" {
		entry.job = job
	}
	entry.lease = lease
	entry.outcome = outcome
	entry.releaseOnly = true
	entry.failed(time.Now(), l.retryFirst, l.retryMax)
}

// parkUnreachable keeps a completion's obligation without renewing its lease.
//
// The entry carries the lease so the retry can address the same holder, and is
// NOT release-only: nothing has proved the compute absent, so neither the retry
// loop nor a shutdown may release it (releaseAll refuses a parked entry without
// proof). Leaving `running` is the whole effect — the heartbeat pass renews
// what is in `running`, and this lease is now the reaper's to quarantine unless
// the process holding its compute renews it.
func (l *Listener) parkUnreachable(job Job, lease *alloc.Lease, outcome alloc.Phase) {
	if outcome == "" {
		outcome = alloc.PhaseDone
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.cleanup == nil {
		l.cleanup = make(map[int64]*pendingCleanup)
	}

	// A LATER DELIVERY OWNS THE OBLIGATION, and this one changes nothing — the
	// lease included, which stays wherever that delivery left it.
	entry := l.cleanup[job.RequestID]
	if entry != nil && entry.job.CompletionID > job.CompletionID {
		return
	}

	delete(l.running, job.RequestID)
	delete(l.confirmed, lease.ID)

	if entry == nil {
		entry = &pendingCleanup{job: job}
		l.cleanup[job.RequestID] = entry
	} else if job.Result != "" {
		entry.job = job
	}

	entry.lease = lease
	entry.outcome = outcome
	entry.releaseOnly = false
	entry.failed(time.Now(), l.retryFirst, l.retryMax)
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

func (l *Listener) forgetCompletion(ctx context.Context, job Job) bool {
	if l.completionStore != nil && job.Result != "" {
		if err := l.completionStore.RetirePendingCompletion(
			ctx, l.tier, job.RequestID, job.CompletionID,
		); err != nil {
			l.log.Error("a completed job settled, but its durable tombstone could not be written; its request id stays blocked until this is retried",
				"tier", l.tier, "request", job.RequestID, "error", err)

			return false
		}
	}
	if l.alloc != nil {
		if err := l.alloc.SettlePoolRunner(ctx, l.tier, job.RequestID); err != nil {
			l.log.Error("a completed pool runner settled, but its physical identity could not be preserved through source acknowledgement",
				"tier", l.tier, "request", job.RequestID, "error", err)

			return false
		}
	}

	return true
}

func (l *Listener) parkRetirement(job Job) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cleanup == nil {
		l.cleanup = make(map[int64]*pendingCleanup)
	}
	entry := l.cleanup[job.RequestID]
	if entry != nil && entry.job.CompletionID > job.CompletionID {
		return
	}
	if entry == nil {
		entry = &pendingCleanup{}
		l.cleanup[job.RequestID] = entry
	}
	entry.job = job
	entry.lease = nil
	entry.releaseOnly = false
	entry.retireOnly = true
	entry.failed(time.Now(), l.retryFirst, l.retryMax)
}

// retireParked retries only the durable tombstone, never the teardown it follows.
func (l *Listener) retireParked(ctx context.Context, entry *pendingCleanup, job Job) {
	if !l.forgetCompletion(ctx, job) {
		return
	}
	l.mu.Lock()
	if l.cleanup[job.RequestID] == entry {
		delete(l.cleanup, job.RequestID)
	}
	l.mu.Unlock()
}

// retryParkedRetirement reports whether this delivery is already past teardown.
func (l *Listener) retryParkedRetirement(ctx context.Context, job Job) bool {
	l.mu.Lock()
	entry := l.cleanup[job.RequestID]
	if entry == nil || !entry.retireOnly ||
		(entry.job.CompletionID != 0 && entry.job.CompletionID != job.CompletionID) {
		l.mu.Unlock()

		return false
	}
	retireJob := entry.job
	l.mu.Unlock()
	l.retireParked(ctx, entry, retireJob)

	return true
}

// deleteCompletionCleanup cannot let an older message remove a reused id's obligation.
func (l *Listener) deleteCompletionCleanup(job Job) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.cleanup[job.RequestID]
	if entry == nil {
		return
	}
	if entry.job.CompletionID != 0 && entry.job.CompletionID != job.CompletionID {
		return
	}
	delete(l.cleanup, job.RequestID)
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
	for i := range completions {
		completion := &completions[i]
		job := Job{RequestID: completion.RequestID, RunID: completion.RunID, Result: completion.Result,
			CompletionID: completion.MessageID}
		if completion.Retired {
			continue
		}
		if entry := l.cleanup[job.RequestID]; entry != nil {
			entry.job = job
			if completion.LeaseID != "" {
				entry.lease = &alloc.Lease{ID: completion.LeaseID, Epoch: completion.LeaseEpoch,
					Node: completion.LeaseNode}
				entry.outcome = alloc.Phase(completion.Outcome)
				entry.releaseOnly = completion.ReleaseOnly
			}

			continue
		}
		entry := &pendingCleanup{job: job}
		if completion.LeaseID != "" {
			entry.lease = &alloc.Lease{ID: completion.LeaseID, Epoch: completion.LeaseEpoch,
				Node: completion.LeaseNode}
			entry.outcome = alloc.Phase(completion.Outcome)
			entry.releaseOnly = completion.ReleaseOnly
		}
		l.cleanup[job.RequestID] = entry
	}

	l.mu.Unlock()

	// THE RESULT IS RE-RECORDED FROM THE DURABLE ROW, OUTSIDE THE MUTEX.
	//
	// PutPendingCompletion commits before RecordJobResult, so a process that died
	// between them left GitHub's conclusion on disk and nothing in job_history.
	// The completion is then settled by the cleanup clock and its tombstone
	// retired, after which every redelivery is classified as already handled and
	// the diagnostic is lost for good. This is the one place that still holds the
	// evidence, so it is where the second write is retried.
	//
	// READ FROM THE ROWS RATHER THAN FROM l.cleanup, so the result and the lease
	// come from ONE durable delivery. The map's entries are pointers the cleanup
	// loop mutates, and a restored row merged onto a pre-existing entry can leave
	// this job beside another delivery's lease — which is the reused-request-id
	// hole again, reached from the other side.
	//
	// First-observation-wins makes it idempotent for the ordinary restart, where
	// the result is already recorded and this costs one read each. The count is
	// bounded by the completions in flight, and it runs before the poll loop
	// starts, so it cannot delay anything that is serving.
	// A RETIRED ROW IS INCLUDED HERE AND EXCLUDED ABOVE, and the asymmetry is the
	// point. Retired means the TEARDOWN obligation settled; it says nothing about
	// whether the diagnostic was written, and the row still carries GitHub's word
	// and the lease it belongs to. Skipping it strands the very case this recovery
	// exists for: the result write failed, teardown then settled and retired the
	// row, and every redelivery from that moment is classified as already handled
	// — so nothing else will ever look at the evidence again before an
	// acknowledgement deletes it.
	for i := range completions {
		completion := &completions[i]
		if completion.LeaseID == "" {
			continue
		}

		l.recordJobResult(ctx, Job{RequestID: completion.RequestID, RunID: completion.RunID,
			Result: completion.Result, CompletionID: completion.MessageID}, completion.LeaseID)
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
	if job.RunnerName != "" {
		if leaseID, ok := provider.LeaseOf(job.RunnerName); ok {
			if err := l.alloc.RetirePoolRunner(ctx, leaseID); err != nil &&
				!errors.Is(err, alloc.ErrLeaseNotFound) {
				l.log.Error("could not mark a completed pool runner for retirement",
					"tier", l.tier, "runner", job.RunnerName, "error", err)
				return
			}
		}
	}
	// A redelivery after teardown and release settled can only retry the durable
	// tombstone. Repeating teardown here could address replacement compute if the
	// source reused the request id after an acknowledgement failure.
	if l.retryParkedRetirement(ctx, job) {
		return
	}

	// A previous attempt already established that no compute exists. Repeating a
	// remote destroy adds no proof, and on shutdown it can wait a full node timeout
	// before the local release that is the only work left.
	if handled, settled := l.releaseParked(ctx, job.RequestID); handled {
		if settled {
			if !l.forgetCompletion(ctx, job) {
				l.parkRetirement(job)
			}
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
	before, beforeOutcome, _ := l.completionRelease(job.RequestID)

	if err := l.destroyCompleted(ctx, job, before, beforeOutcome); err != nil {
		// THE RUNNER IS HOLDING IT, SO THIS LISTENER LETS GO.
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
			retired := l.forgetCompletion(ctx, job)
			if !retired {
				l.parkRetirement(job)
			}
			l.log.Info("the compute for a finished job was asked to stop and has not been "+
				"confirmed gone; the runner is holding its capacity until it is",
				"tier", l.tier, "request", job.RequestID)

			l.mu.Lock()
			delete(l.running, job.RequestID)
			if retired {
				if entry := l.cleanup[job.RequestID]; entry == nil ||
					entry.job.CompletionID == 0 || entry.job.CompletionID == job.CompletionID {
					delete(l.cleanup, job.RequestID)
				}
			}
			l.mu.Unlock()

			return
		}

		// THE HOLDER CANNOT BE REACHED, SO THIS LISTENER STOPS RENEWING AND KEEPS
		// THE OBLIGATION.
		//
		// The plane bound this completion to the process that launched the
		// compute, and that process is gone: dead, or replaced by one that
		// truthfully knows nothing about the build. Treating that as an ordinary
		// failed destroy — keep the lease in `running`, keep renewing, retry —
		// was a loop with no exit: the lease never expired, so it was never
		// quarantined, so the replacement's inventory could never settle it, so
		// every retry got the same answer, for as long as this process ran. It
		// showed as capacity charged with nothing held, and `--force` refused it
		// as busy.
		//
		// Renewal is what a party holding COMPUTE does, and this listener holds
		// none. Whoever does — a superseded process draining its custody, a
		// replacement that adopted the build — renews the lease itself; if nobody
		// does, the reaper quarantines it and its capacity stays charged until a
		// proof arrives, which is the identical protection one phase over. The
		// retry keeps asking, through the same bound destroy, and the plane
		// settles it from the replacement's inventory once the grace has passed.
		if errors.Is(err, ErrHolderUnavailable) && before != nil {
			l.parkUnreachable(job, before, beforeOutcome)

			l.log.Warn("a finished job's compute is bound to a node process this control plane "+
				"cannot reach; its lease is no longer renewed here and stays charged until "+
				"whoever holds the compute renews it or its host proves the compute gone",
				"tier", l.tier, "request", job.RequestID, "lease", before.ID, "error", err)

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

	durableLease, durableOutcome, _ := l.completionRelease(job.RequestID)
	if durableLease == nil {
		durableLease, durableOutcome = before, beforeOutcome
	}
	if err := l.recordReleaseOnly(ctx, job, durableLease, durableOutcome); err != nil {
		l.parkReleaseOnly(job, durableLease, durableOutcome)
		l.log.Error("compute was confirmed absent, but its release-only obligation could not be made durable; capacity stays held until this is retried",
			"tier", l.tier, "request", job.RequestID, "lease", durableLease.ID, "error", err)

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
		l.mu.Unlock()
		if l.forgetCompletion(ctx, job) {
			l.deleteCompletionCleanup(job)
		} else {
			l.parkRetirement(job)
		}

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
	l.mu.Unlock()
	if l.forgetCompletion(ctx, job) {
		l.deleteCompletionCleanup(job)
	} else {
		l.parkRetirement(job)
	}
}

func (l *Listener) destroyCompleted(
	ctx context.Context,
	job Job,
	lease *alloc.Lease,
	outcome alloc.Phase,
) error {
	if l.registry != nil {
		var binding alloc.PoolRunner
		var err error
		if lease != nil && lease.ID != "" {
			binding, err = l.alloc.PoolRunnerByLease(ctx, lease.ID)
		} else if job.RunnerName != "" {
			binding, err = l.alloc.PoolRunnerByName(ctx, job.RunnerName)
		}
		if err != nil && !errors.Is(err, alloc.ErrLeaseNotFound) {
			return fmt.Errorf("server: resolve runner registration before teardown: %w", err)
		}
		if errors.Is(err, alloc.ErrLeaseNotFound) && lease != nil && lease.ID != "" {
			binding.RunnerName = provider.InstanceName(lease.ID)
			err = nil
		}
		// Withdraw the registration by whatever identity is available: prefer the
		// durable binding, fall back to the identity the completion itself carries
		// (an id-only completion never populates the binding, which is resolved
		// from the lease and runner name). Skip removal only when there is
		// genuinely no registration to withdraw anywhere — no binding, no lease,
		// no runner id or name — because RemoveRunner(0, "") is refused "needs an
		// id or name" every time and would wedge the completion's capacity in an
		// unbounded teardown retry. An absent registration needs no removal.
		removeID, removeName := binding.RunnerID, binding.RunnerName
		if removeID == 0 && removeName == "" {
			removeID, removeName = job.RunnerID, job.RunnerName
		}
		if err == nil && (removeID > 0 || removeName != "") {
			if err := l.registry.RemoveRunner(ctx, removeID, removeName); err != nil {
				return fmt.Errorf("server: remove runner %q before teardown: %w", removeName, err)
			}
			// The runner is gone from GitHub. Stop counting this lease as a live
			// runner even if the compute-destroy below retries, so a lingering
			// teardown does not over-count against the assignment deficit. The
			// mark is unfenced on purpose: a reap may quarantine this lease before
			// we get here, and the runner is gone regardless. A failure here only
			// leaves it counted, which is the safe direction.
			if lease != nil && lease.ID != "" {
				if err := l.alloc.MarkDeregistered(ctx, lease.ID); err != nil {
					l.log.Warn("could not record runner deregistration; the lease stays counted until it terminalizes",
						"tier", l.tier, "lease", lease.ID, "error", err)
				}
			}
		}
	}
	if outcome == "" {
		outcome = alloc.PhaseDone
	}
	if runner, ok := l.runner.(BoundCompletionAwareRunner); ok && job.Result != "" &&
		lease != nil && lease.ID != "" && lease.Node != "" {
		return runner.DestroyCompletedBound(
			ctx, job.RequestID, job.Result, lease.ID, lease.Node, lease.Epoch, outcome)
	}
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
	retirements := make([]Job, 0, len(l.cleanup))

	for id, entry := range l.cleanup {
		if entry.retireOnly {
			retirements = append(retirements, entry.job)
		}
		if entry.lease != nil {
			parked[id] = entry
		}
	}

	l.held = nil
	l.acquiring = make(map[int64]*promise)
	l.mu.Unlock()
	for i := range retirements {
		l.forgetCompletion(ctx, retirements[i])
	}

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

	// A RUNNING LEASE IS RELEASED ONLY IF ITS COMPUTE WAS CONFIRMED GONE, and on
	// an ordinary shutdown none of them is: destroyAll is called with
	// includeRunning false, so anything still executing is deliberately left
	// alive. Freeing that capacity would be the overcommit this ordering exists
	// to prevent — a container on the host and another tier escrowing its slot.
	//
	// SO THE CAPACITY STAYS CHARGED, and that is the intended outcome rather than
	// a leak. The guest keeps running, the node keeps holding it, and the next
	// control plane adopts the lease through ServiceableRunnerLeaseIDs. Capacity
	// the reaper reclaims late is recoverable; a build failed to reclaim it early
	// is not.
	//
	// The entries that DO arrive here confirmed are the ones an operator forced,
	// and completions whose destroy this shutdown owed.
	for requestID, lease := range running {
		if !destroyed[requestID] {
			// NOT AN ERROR, and it used to be logged as one. On every ordinary
			// shutdown this is the normal path for every running job, and calling
			// it "needs manual cleanup" sent operators looking for containers to
			// remove by hand — which is exactly the work billet does not want them
			// doing, because that compute is somebody's job and it is still fine.
			l.log.Info("leaving a running job alone; its lease and capacity stay charged "+
				"until a host proves the compute is gone, and the next control plane "+
				"re-adopts it",
				"tier", l.tier, "request", requestID, "lease", lease.ID)

			continue
		}

		err := l.releaseAbsent(ctx, requestID, lease, alloc.PhaseDone, "")
		if !releaseSettled(err) {
			l.log.Warn("could not release completed capacity after compute was confirmed absent",
				"tier", l.tier, "request", requestID, "lease", lease.ID, "error", err)
			stuck = append(stuck, lease)

			continue
		}
		if entry := parked[requestID]; entry != nil {
			l.forgetCompletion(ctx, entry.job)
		}
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
		// A RELEASE-ONLY ENTRY ALREADY HAS ABSENCE PROOF. Every other parked
		// entry still needs destroyAll to confirm teardown or hand custody to the
		// runner. In particular, a completion restored before its holder registers
		// must keep its authoritative result and lease unchanged across shutdown.
		if !entry.releaseOnly && !destroyed[id] {
			l.log.Error("not releasing cleanup capacity because its compute was not confirmed absent; the durable completion remains pending for the next process",
				"tier", l.tier, "request", id, "lease", entry.lease.ID)

			continue
		}

		outcome := entry.outcome
		if outcome == "" {
			outcome = alloc.PhaseDone
		}
		if err := l.recordReleaseOnly(ctx, entry.job, entry.lease, outcome); err != nil {
			l.log.Warn("not releasing cleanup capacity because its release-only obligation is not durable",
				"tier", l.tier, "request", id, "lease", entry.lease.ID, "error", err)

			continue
		}
		err := l.releaseAbsent(ctx, id, entry.lease, outcome, entry.failureReason)
		if err != nil {
			l.log.Warn("could not release cleanup capacity after compute was confirmed absent",
				"tier", l.tier, "request", id, "lease", entry.lease.ID, "error", err)

			if !releaseSettled(err) {
				stuck = append(stuck, entry.lease)
			}
		}
		if releaseSettled(err) {
			l.forgetCompletion(ctx, entry.job)
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

// isQuiesced reports whether the deployment's admission is sealed.
func (l *Listener) isQuiesced() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.quiesced
}

// refusalReason names which of the two states declined an offer, because they
// mean different things to whoever reads the log: a drain ends with this process
// stopping, a seal ends when an operator resumes.
func refusalReason(draining bool) string {
	if draining {
		return "draining"
	}

	return "admission sealed"
}

// markAdmission brings this listener into line with the deployment's admission
// state, and reports whether it is now sealed. It CHANGES NO CAPACITY — handing
// escrow back is handBackIdleEscrow's job, and the split is the invariant.
//
// MARK FIRST, RELEASE SECOND, and this ordering is DEFENSIVE rather than load
// bearing today — said plainly because the mutation survives. Reversing the two
// leaves a window where the capacity is gone but the flag is not yet set, and an
// offer accepted in it takes the escrow straight back so the quiesce never
// converges. Nothing can reach that window while handle runs on the poll loop's
// own goroutine, which is what actually prevents it. Keep the order anyway: it
// costs nothing, and the thing that makes it safe is an incidental property of
// where handle is called from, not a rule anybody restated when moving it.
//
// AN UNREADABLE STATE COUNTS AS SEALED, consistently with the ledger: escrow is
// already refused when admission cannot be read, so a listener that kept
// accepting offers would be claiming work it cannot back. The cost is a poll
// spent not accepting, which the next one recovers; the alternative is admitting
// work into a deployment somebody sealed.
//
// IT NEVER RETURNS AN ERROR, and that is deliberate rather than lazy. An error
// out of this loop stops the listener, one listener stopping cancels every
// other, and their teardown destroys the compute they hold. A transient database
// blip must not be able to do that.
func (l *Listener) markAdmission(ctx context.Context) (bool, bool) {
	if l.alloc == nil {
		return false, true
	}

	admission, err := l.alloc.Admission(ctx)

	sealed, known := admission.Sealed(), err == nil
	if err != nil {
		// REFUSING IS FAIL-CLOSED; HANDING CAPACITY BACK IS NOT, and separating
		// the two is what this second return value is for. Declining offers on a
		// read that failed costs a poll and cannot admit work billet cannot back.
		// Handing the escrow back is an ACTION premised on knowing the deployment
		// is sealed — which is exactly what just failed to be established — and it
		// is not free: a listener that returns its escrow on a transient database
		// blip hands the gap to another tier and retakes it on the next poll,
		// which is the flapping the escrow exists to prevent.
		//
		// So an unreadable state quiesces and keeps what it holds.
		sealed = true

		l.log.Warn("could not read whether this deployment is taking new work; declining "+
			"offers until it can be read, and keeping the capacity already escrowed",
			"tier", l.tier, "error", err)
	}

	l.mu.Lock()
	was := l.quiesced
	l.quiesced = sealed
	l.mu.Unlock()

	// SAID ONLY WHEN IT IS KNOWN. "This deployment is no longer taking new work"
	// is a claim about the DEPLOYMENT, and a read that failed establishes nothing
	// about it — this listener's own caution is the only fact there is. Logging
	// it anyway is how "could not verify" becomes a verdict in somebody's journal.
	switch {
	case sealed && known && !was:
		l.log.Info("this deployment is no longer taking new work; handing back idle capacity "+
			"and letting what is running finish", "tier", l.tier)
	case !sealed && was:
		l.log.Info("this deployment is taking work again", "tier", l.tier)
	}

	return sealed, known
}

// handBackIdleEscrow returns capacity a sealed listener is holding and cannot
// use. It is separate from marking, and it runs AFTER any message already in
// hand has been handled.
//
// WHY IT WAITS FOR THE MESSAGE. GitHub can assign work this listener holds no
// in-memory promise for — after a restart, or on the direct-assignment path —
// and `assign` backs such a job from `held`. Releasing before `handle` empties
// `held`, so a seal landing during a long poll would decline a job GitHub had
// already assigned against capacity advertised before the seal. That is
// revoking a commitment already made, which is the one thing sealing must not
// do: it refuses NEW work, and an assignment in hand is not new work.
//
// Marking still happens first, so nothing can accept an OFFER out of that same
// message while this waits.
func (l *Listener) handBackIdleEscrow(ctx context.Context, sealed bool) {
	if !sealed || l.alloc == nil {
		return
	}

	if released := l.releaseIdleEscrow(ctx); released > 0 {
		l.log.Info("handed back idle capacity", "tier", l.tier, "leases", released)
	}
}

// forceDestroy carries out an operator's explicit decision to destroy compute
// that is still running a job.
//
// THE ONLY THING IN BILLET THAT FAILS A BUILD ON PURPOSE, and everything about
// its shape exists to keep it from being reached any other way. A drain timeout,
// a second signal, a systemd TimeoutStopSec, a failed rollout and a lost
// leadership epoch all leave running work alone; only a durable record naming an
// actor, a reason and an exact set of leases reaches this.
//
// IT ACTS ON THE RECORDED SET AND NOT ON WHAT THIS LISTENER HAPPENS TO HOLD. The
// operator was shown a list and approved that list; a job that started between
// the diagnostic and the confirmation was never approved, and destroying it here
// would be the implicit teardown the whole mechanism refuses.
//
// IT NEVER RETURNS AN ERROR, for the reason markAdmission does not: an error out
// of the poll loop stops this listener, one listener stopping cancels every
// other, and their teardown runs. A database blip must not be able to do that, so
// a failure here is logged and retried on the next poll — the record is durable
// and does not expire.
func (l *Listener) forceDestroy(ctx context.Context) {
	if l.alloc == nil {
		return
	}

	open, found, err := l.alloc.OpenForceDestroy(ctx)
	if err != nil {
		l.log.Warn("could not read whether an operator has asked for running compute to be "+
			"destroyed; nothing was destroyed, and this is retried on the next poll",
			"tier", l.tier, "error", err)

		return
	}

	if !found {
		return
	}

	targets, err := l.alloc.PendingForceTargets(ctx, open.Generation, l.tier)
	if err != nil {
		l.log.Warn("could not read which leases a force-destroy covers; nothing was "+
			"destroyed, and this is retried on the next poll",
			"tier", l.tier, "force", open.Generation, "error", err)

		return
	}

	if len(targets) == 0 {
		return
	}

	l.log.Warn("DESTROYING RUNNING COMPUTE because an operator asked for it; the jobs on "+
		"these leases fail, and GitHub does not requeue a job whose runner vanished "+
		"after it started",
		"tier", l.tier, "force", open.Generation, "actor", open.Actor,
		"reason", open.Reason, "leases", len(targets))

	// SPLIT BY WHO CAN REACH THE COMPUTE, not by phase. A lease this listener
	// launched is in its own escrow and goes through the full destroy pass, with
	// its custody, completion-persistence and parking behaviour. A lease that
	// outlived a control plane is ADOPTED — the node holds the guest and this
	// process holds no object for it — so the request id from the durable record
	// is the only handle there is. Keying the whole operation on the in-memory map
	// would make a force silently do nothing after exactly the restart that most
	// often precedes one.
	scope := make(map[int64]bool, len(targets))
	adopted := make([]state.ForceTarget, 0, len(targets))
	unaddressable := make([]state.ForceTarget, 0)

	l.mu.Lock()

	for i := range targets {
		t := &targets[i]

		if t.SchedulerRequest == 0 {
			unaddressable = append(unaddressable, *t)

			continue
		}

		_, running := l.running[t.SchedulerRequest]
		_, pending := l.cleanup[t.SchedulerRequest]

		if running || pending {
			scope[t.SchedulerRequest] = true

			continue
		}

		adopted = append(adopted, *t)
	}

	l.mu.Unlock()

	destroyed := map[int64]bool{}
	if len(scope) > 0 {
		destroyed = l.destroyAll(ctx, true, scope)
	}

	// SETTLED ONCE, AND TRACKED. Without this a target the adopted loop already
	// recorded is walked again below, which settles it a second time with a LESS
	// accurate detail and logs a second error about it. The settlement itself is
	// idempotent, so the damage is a diagnostic that contradicts the one above it —
	// which is exactly what an operator reads when they are trying to work out
	// what happened to a host.
	settled := make(map[string]bool, len(targets))

	settle := func(t *state.ForceTarget, disposition, detail string) {
		settled[t.LeaseID] = true

		l.settleForced(ctx, open.Generation, t, disposition, detail)
	}

	for i := range adopted {
		t := &adopted[i]

		// STRAIGHT AT THE RUNNER, because there is nothing else to go through. The
		// node is holding this guest on its own; Destroy is required to be
		// idempotent, so asking about compute that has already gone costs a no-op.
		if err := l.runner.Destroy(ctx, t.SchedulerRequest); err != nil {
			settle(t, state.ForceTargetFailed,
				fmt.Sprintf("destroying adopted compute failed: %v", err))

			continue
		}

		destroyed[t.SchedulerRequest] = true
	}

	for i := range unaddressable {
		t := &unaddressable[i]

		// NO SCHEDULER IDENTITY IS A CONCLUSIVE FAILURE FOR THIS LEASE, not a
		// reason to leave the request open. Nothing billet has can name the compute
		// to a node, so no amount of retrying reaches it, and an open request that
		// can never finish blocks the next force.
		settle(t, state.ForceTargetFailed,
			"this lease carries no scheduler request, so no node can be told which "+
				"compute to destroy")
	}

	for i := range targets {
		t := &targets[i]

		if t.SchedulerRequest == 0 || settled[t.LeaseID] {
			continue
		}

		if !destroyed[t.SchedulerRequest] {
			// NOT PROOF THE CONTAINER SURVIVED, and nothing is released on it. The
			// lease stays charged and the row says `failed`, which is what an
			// operator reads when a force reports it did not finish.
			settle(t, state.ForceTargetFailed,
				"the destroy did not confirm; this lease stays charged because a failed "+
					"teardown is not evidence the compute is gone")

			continue
		}

		switch err := l.alloc.ForceTerminate(ctx, t.LeaseID); {
		case errors.Is(err, alloc.ErrForceHeld):
			settle(t, state.ForceTargetFailed,
				"a node took custody of this lease before its capacity could be returned; "+
					"resolve it with `billet leases release --force` once you know the "+
					"compute is gone")
		case err != nil:
			settle(t, state.ForceTargetFailed,
				fmt.Sprintf("the compute was destroyed but its capacity could not be "+
					"returned: %v", err))
		default:
			// THE LEASE LEAVES `running` HERE, and forgetting to do this was a real
			// defect rather than untidiness. destroyAll does not clear that map — on
			// a shutdown, releaseAll is what walks it — so a forced lease stayed
			// there pointing at a row that is now terminal, and capacity() went on
			// counting it. The listener would then advertise a slot it does not
			// have until the next heartbeat happened to get ErrLeaseNotFound and
			// drop it, which is an overcommit window that exists only because
			// nobody removed a map entry.
			l.mu.Lock()
			delete(l.running, t.SchedulerRequest)
			delete(l.cleanup, t.SchedulerRequest)
			l.mu.Unlock()

			settle(t, state.ForceTargetDestroyed, "")
		}
	}
}

// settleForced records what became of one forced lease.
//
// A SETTLEMENT THAT DOES NOT LAND IS LEFT PENDING, deliberately. The compute is
// already destroyed either way; what is lost is the record, and re-observing the
// target on the next poll re-runs an idempotent destroy against compute that has
// gone. The alternative — treating the write as best effort — leaves a request
// open forever with nothing saying why.
func (l *Listener) settleForced(
	ctx context.Context, generation int64, t *state.ForceTarget, disposition, detail string,
) {
	if err := l.alloc.SettleForceTarget(ctx, generation, t.LeaseID, disposition,
		detail); err != nil {
		l.log.Error("could not record what became of a lease an operator forced; the "+
			"force-destroy stays open and this is retried on the next poll",
			"tier", l.tier, "force", generation, "lease", t.LeaseID,
			"disposition", disposition, "error", err)

		return
	}

	if disposition == state.ForceTargetDestroyed {
		l.log.Warn("destroyed a running job's compute on an operator's instruction; that "+
			"build fails and GitHub will not requeue it",
			"tier", l.tier, "force", generation, "lease", t.LeaseID, "node", t.Node,
			"run", t.RunID, "request", t.SchedulerRequest)

		return
	}

	l.log.Error("a lease an operator asked to be destroyed was not settled as destroyed",
		"tier", l.tier, "force", generation, "lease", t.LeaseID, "node", t.Node,
		"run", t.RunID, "detail", detail)
}
