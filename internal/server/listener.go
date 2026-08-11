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
)

// Session is the part of a GitHub scale-set message session billet uses.
//
// It is billet's own interface, not the vendor's, for two reasons. The
// scale-set client is a PUBLIC PREVIEW whose own README says interfaces may
// change, and a preview dependency reaching into the scheduler is how a
// third-party release note turns into a rewrite. And a fake that satisfies four
// methods is what makes the capacity arithmetic testable without a GitHub
// organization to point at.
type Session interface {
	// GetMessage long-polls for work, advertising maxCapacity as the number of
	// runners this scale set can accept right now. It returns ErrNoMessage when
	// the poll times out with nothing to report, which is the ordinary case.
	GetMessage(ctx context.Context, lastMessageID int64, maxCapacity int) (*Message, error)
	// DeleteMessage acknowledges a message. An unacknowledged message is
	// redelivered, so everything derived from one must be idempotent.
	DeleteMessage(ctx context.Context, messageID int64) error
	// AcquireJobs claims assigned jobs and returns the ids actually acquired,
	// which may be fewer than asked for.
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	// Statistics reports what GitHub said when the session opened, or nil if it
	// said nothing. It is the only view of a backlog that predates the session.
	Statistics() *Statistics
	Close(ctx context.Context) error
}

// Runner turns an assigned lease into running compute, and tears it down again.
//
// The seam between the control plane and a host. Today the only implementation
// runs in this process (`billet server --dev`); the node split puts a remote one
// behind the same two methods, which is the point of naming it now rather than
// calling a provider directly from the listener.
//
// Both methods are called OUTSIDE the escrow mutex, deliberately. Launching
// pulls images and talks to a hypervisor; holding the mutex across that would
// stall every heartbeat behind it, and heartbeats are what keep the escrow
// alive.
type Runner interface {
	// Launch starts the compute for a lease that has just been assigned.
	//
	// The lease is already durable and already counted against the budget by the
	// time this is called, so a failure here means capacity is held for something
	// that is not running — the caller releases it rather than leaving it.
	Launch(ctx context.Context, lease *alloc.Lease, job Job) error

	// Destroy removes whatever Launch started for a request.
	//
	// MUST be idempotent: it runs on redelivered completions, on shutdown, and on
	// paths that have already failed once.
	Destroy(ctx context.Context, requestID int64) error
}

// Sweeper is a Runner that can also find compute nothing is asking about.
//
// Optional, and asserted for rather than required, because the two questions are
// answerable by different things. Launch and Destroy are per-job and a remote
// node will always implement them; enumerating everything a backend is running
// is a whole-host operation that a node may not be able to answer during a
// partition — and a control plane that refused to start without it would be
// trading a real capability for a hypothetical one.
type Sweeper interface {
	// Sweep destroys compute whose lease is no longer open.
	//
	// Called after each reap, because reaping is what MAKES a container an
	// orphan: the lease it was running under is exactly what the reaper has just
	// terminalized. Anything else that leaks compute — a stray a failed launch
	// could not confirm, a Destroy the backend refused — is picked up by the same
	// pass, which is why a failed cleanup is survivable rather than permanent.
	Sweep(ctx context.Context) error

	// Tend advances compute the runner is holding capacity for: heartbeating
	// those leases so the reaper does not terminalize them, letting adopted work
	// finish, and destroying what is confirmed finished or unwanted.
	//
	// Paired with Sweep rather than separate because the two are the same
	// obligation seen from opposite ends — Sweep finds compute no lease is
	// holding, Tend holds leases whose compute is unaccounted for.
	Tend(ctx context.Context) error

	// KeepAlive renews held leases until the context ends, on its OWN clock.
	//
	// Separate from Tend because renewal must not share a schedule with anything
	// that talks to a compute backend. Tend and Sweep both make unbounded
	// provider calls, and they run after the reaper on a shared tick — so a slow
	// `docker ps` delays the next renewal without delaying the next reap, and
	// anything longer than the lease TTL lets the reaper reclaim capacity that is
	// being held on purpose.
	//
	// Blocks until ctx is done; the caller runs it in a goroutine.
	KeepAlive(ctx context.Context)
}

// ErrCustody means the runner has taken responsibility for a lease's capacity,
// so the caller must NOT release it.
//
// Returned from Launch when compute may exist that could not be confirmed gone.
// Releasing the lease then would hand the capacity back while a container is
// possibly still running on it — the exact over-commitment the reconciliation
// work exists to prevent, arrived at by treating "the launch failed" as "nothing
// is using the host".
var ErrCustody = errors.New("server: the runner is holding this lease's capacity")

// errNoRunner means no compute is attached to this control plane.
var errNoRunner = errors.New("server: no runner is configured, so nothing can start this job")

// noRunner is the default, and it FAILS CLOSED.
//
// It used to log and return success, which is the worst of both: billet would
// tell itself the job had started, hold the capacity, and never run anything.
// The job then hangs until GitHub's pickup deadline with no error anywhere —
// a silent black hole that looks healthy from every angle.
//
// Returning an error routes it into the ordinary failed-launch path instead: the
// capacity goes straight back and GitHub reassigns the job to something that can
// actually run it. A control plane with no compute is still coherent — it
// advertises, acquires, and accounts — it just cannot pretend to execute.
type noRunner struct{ log *slog.Logger }

func (n noRunner) Launch(_ context.Context, lease *alloc.Lease, job Job) error {
	n.log.Error("no runner is configured; declining this job rather than holding capacity "+
		"for something that will never start",
		"request", job.RequestID, "run", job.RunID, "lease", lease.ID)

	return errNoRunner
}

func (noRunner) Destroy(context.Context, int64) error { return nil }

// ErrNoMessage means a long poll timed out with nothing to report. It is the
// ordinary outcome, not a failure — the caller polls again.
//
// A sentinel rather than (nil, nil), which the upstream client returns and this
// package deliberately does not propagate: a nil message with a nil error is
// indistinguishable from "something went wrong and nobody said so", and every
// caller has to remember which of the two it is looking at.
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
// RequestID is the identity that matters. It is what AcquireJobs claims work by,
// and it is what makes a redelivered message idempotent — GitHub's own JobID is
// a separate string field, so a schema that stored an int64 under the name
// job_id was recording the request id under a name that would look correct to
// anyone later trying to correlate a lease with GitHub's API. Migration 8
// renamed the column to say what it holds.
type Job struct {
	RequestID int64
	RunID     int64
	// Event is the GitHub event that queued this job — "push", "pull_request",
	// "schedule". It is carried because it is the ONLY thing in a scale-set
	// message that says anything about how much the workload can be trusted, and
	// that decides which backends may run it.
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

	// mu guards the escrow below.
	//
	// Needed only since heartbeats moved onto their own clock: renewal and the
	// poll loop now touch held and running concurrently, where before every
	// change happened on one goroutine. That is the cost of not tying lease
	// renewal to a poll whose duration billet does not control.
	mu sync.Mutex
	// held is escrowed capacity not yet given to a job.
	held []*alloc.Lease
	// running is escrowed capacity that HAS been given to a job, keyed by the
	// request id GitHub identifies it by.
	//
	// Both halves are escrowed, and both are advertised — see capacity(). The
	// safety property is that the number sent to GitHub is only ever capacity
	// this listener took from the allocator, never a number computed from
	// headroom. Keyed by request id so a redelivered message is recognised: an
	// unacknowledged message comes back, and assigning it twice would consume a
	// second lease for one job.
	running map[int64]*alloc.Lease

	// cleanup holds completions whose destroy failed.
	//
	// A FAILED DESTROY DELIBERATELY KEEPS THE CAPACITY HELD — releasing it while
	// the compute may still be running is the overcommit the whole ordering exists
	// to prevent — but nothing else was ever going to try again. GitHub's
	// completion has been acknowledged, so it will not be redelivered; the lease
	// sits in `running` and this listener heartbeats it for the life of the
	// process. Retrying on the renewal clock is what turns "held" into something
	// other than a leak.
	// Entries carry their own next-attempt time, because a node that is never
	// coming back must not occupy every pass ahead of one that has just recovered:
	// retries are sequential and a single Destroy can wait the full node command
	// timeout, so N hopeless entries cost N times that before a live one is tried.
	cleanup map[int64]*pendingCleanup
	// acquiring is escrow PROMISED to a request billet has claimed from GitHub but
	// has not yet been given, keyed by the request id it was promised to.
	//
	// This is the state the escrow was missing, and its absence was one bug wearing
	// two faces. Capping acquisitions at len(held) is an instantaneous count, and
	// an acquisition is not instantaneous: it is an obligation that lasts until the
	// Assigned message arrives. So one lease could be promised to request B by an
	// acquisition and then consumed by the assignment of request A — the lease is
	// still sitting in held, because nothing had claimed it — and B's assignment
	// arrived to find nothing left. The same lease also backed two consecutive
	// Available batches for the same reason.
	//
	// Reserving the lease under the mutex BEFORE the network call is also what
	// closes the race with the heartbeat: a lease promised to an acquisition is no
	// longer in held, so nothing else can spend it while AcquireJobs is in flight.
	acquiring map[int64]*promise

	lastMessageID int64

	// maxCapacity, when set, caps what this listener advertises. nil means the
	// escrow decides, which is the ordinary case.
	maxCapacity *int

	// stalePromise is how long a promise may go unclaimed before it is reported.
	stalePromise time.Duration

	// shutdownGrace bounds the remote half of the teardown, so an unbounded
	// Destroy cannot keep Run — and the renewal that outlives it — running
	// forever. closeGrace and releaseGrace bound the two local phases after it.
	shutdownGrace time.Duration
	closeGrace    time.Duration
	releaseGrace  time.Duration

	// retryFirst and retryMax pace the cleanup retries. retryFirst <= 0 turns
	// pacing off entirely, which only tests ask for.
	retryFirst time.Duration
	retryMax   time.Duration

	// destroying is the requests a cleanup retry is inside a Destroy for right
	// now. The shutdown pass skips them: that destroy is already happening, and
	// issuing a second one only spends the teardown budget on work in progress.
	destroying map[int64]bool

	// sealed stops the cleanup loop starting anything new. Set under l.mu before
	// the loop is cancelled, so "no new attempts" and "what is in flight" are one
	// decision rather than two — see attempt.
	sealed bool

	// ran records that Run has been called. A Listener is single-use; see Run.
	ran bool

	// confirmed is when each lease was last successfully renewed, keyed by lease
	// id. It is what turns "no answer" into a bounded state: past the TTL without
	// a confirmation the reaper may already have taken the lease, so advertising
	// it is advertising capacity that is now someone else's.
	confirmed map[string]time.Time

	// configErrs is what each option refused, keyed by the field it sets, so a
	// later option replaces an earlier one's error along with its value. Appending
	// instead made an invalid value permanently fatal even when a subsequent
	// option had corrected it — which is not how last-value-wins options behave
	// anywhere else, and layered defaults do exactly that.
	configErrs map[string]error

	// runner turns assigned leases into compute. Never nil; see noRunner.
	runner Runner

	// observed is the last statistics GitHub reported. TotalAssignedJobs is the
	// documented scaling signal — counting messages is not, because a response
	// carries at most 50 and a large backlog is truncated.
	observed *Statistics

	// drainGrace bounds the DRAIN, which is a different kind of wait from every
	// other budget here and so has its own ceiling. The others bound a teardown —
	// work billet is doing. This one bounds somebody else's JOB.
	drainGrace time.Duration
	// draining says this listener has been asked to stop and is finishing what it
	// already has. Guarded by mu, and NOT the same thing as sealed.
	draining bool
	// hurry, when closed, ends the drain's wait early. It is how a second signal
	// says "stop waiting" without also saying "abandon what you are holding".
	// Read here and never closed here.
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

// WithRunner sets what turns assigned leases into running compute. The default
// accounts for capacity and launches nothing.
func WithRunner(r Runner) Option {
	return func(l *Listener) { l.runner = r }
}

// Option configures a Listener. for a request GitHub has been told billet will
// run, and the moment that undertaking was made.
//
// `at` IS DIAGNOSTIC ONLY. It drives one stale-promise warning and nothing else,
// and this paragraph is load-bearing because the obvious reading of a timestamp
// on a stuck resource is that it should time out. It must not: an acquisition is
// a commitment to GitHub, AcquireJobs is one-way, and no local clock can revoke
// it. A timed release hands nothing back — it only means billet has forgotten it
// owes a runner, and the freed slot goes to another tier while the assignment is
// still coming. That version was written, reviewed, and reverted; see
// defaultStalePromise.
//
// A promise ends in exactly four ways, none of them local to this struct: the
// assignment arrives, a completion or cancellation arrives, the heartbeat finds
// the lease fenced, or the session ends and releaseAll hands it back.
type promise struct {
	lease *alloc.Lease
	at    time.Time
	// reported keeps a stale promise from logging on every heartbeat.
	reported bool
}

// pendingCleanup is a completion whose destroy has not succeeded yet.
type pendingCleanup struct {
	job Job
	// wait is how long to leave it after the most recent failure, doubling each
	// time up to maxRetryEvery.
	wait time.Duration
	// at is when it may next be attempted. Zero means immediately, which is what
	// a freshly recorded failure wants: the node may have blinked.
	at time.Time
	// declined keeps a request billet cannot take yet from saying so on every
	// poll. GitHub re-offers an unacquired job indefinitely, so the message is
	// worth exactly once per obligation.
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
		// Pacing off. Used by tests whose subject is what a retry DOES rather than
		// when it runs; the pacing itself has a test of its own.
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

// defaultStalePromise is how long a promise may go unclaimed before billet says
// so. It is a DIAGNOSTIC threshold, not a deadline.
//
// The first version of this released the escrow when the threshold passed, and
// that was wrong in a way worth writing down: an acquisition is a commitment
// made to GitHub, and no local timer can revoke it. AcquireJobs is one-way —
// there is no decline or release endpoint on the session client — and
// DeleteMessage acknowledges a NOTIFICATION rather than refusing a job. So a
// timed release does not hand the work back; it only means billet has forgotten
// it owes a runner, while GitHub still expects one. The reclaimed slot then goes
// to another tier and the assignment, when it arrives, has nothing behind it.
//
// Holding the lease is the lesser evil, and the honest one: it is capacity billet
// genuinely still owes. See TestAStalePromiseIsReportedAndKept.
const defaultStalePromise = 5 * time.Minute

const (
	// firstRetryEvery is the pause after a retry fails for the first time.
	firstRetryEvery = 15 * time.Second
	// maxRetryEvery is the ceiling. A node that has been refusing for this long is
	// not about to answer sooner for being asked more often, and the point of the
	// ceiling is that it keeps asking at all.
	maxRetryEvery = 5 * time.Minute
	// teardownConcurrency is how many destroys the shutdown runs at once.
	//
	// ONE, and the two failed attempts at a larger number are worth keeping. A
	// node executes commands one at a time and each command's timeout starts when
	// it is QUEUED, so N concurrent destroys start N ten-minute clocks against a
	// queue that serves them in turn: the ones at the back expire while the node
	// works happily through the front, and healthy jobs are recorded as failed
	// destroys with their leases held back.
	//
	// Capping the fan-out at four did not fix that, because Destroy BROADCASTS to
	// every live node — it is addressed by request id, and only the node holding
	// that request knows whether it is theirs. Four concurrent destroys therefore
	// still queue four commands on every node.
	//
	// So the only safe number here is one, and real concurrency has to come from
	// the runner instead: one command in flight per node, parallel across nodes,
	// which needs a destroy addressed to the lease's own node rather than
	// broadcast. That is tracked separately. In the ordinary case a destroy takes
	// seconds and sequence costs nothing; the pathological case is a node that
	// went quiet, and concurrency never helped there anyway.
	teardownConcurrency = 1

	// closeGrace and releaseGrace bound the local half of the teardown, separately
	// from the remote destroys AND from each other.
	//
	// Short, because neither waits on a node. Separate, because a session close
	// that used most of a shared budget left the releases — sequential, against
	// one SQLite writer — to fail on what was left, turning a clean shutdown into
	// a tier's worth of capacity withheld until the reaper.
	defaultCloseGrace   = 30 * time.Second
	defaultReleaseGrace = 30 * time.Second

	// maxGrace is the largest teardown budget a caller may ask for.
	//
	// Longer than any real teardown and plainly finite, so the four budgets cannot
	// sum to something int64 cannot hold — and so a watchdog always fires on a
	// timescale an operator lives on.
	//
	// It deliberately does NOT bound the drain; see maxDrainGrace.
	maxGrace = time.Hour

	// defaultDrainGrace bounds how long the listener keeps polling for
	// completions after it has been asked to stop, before it gives up waiting and
	// destroys whatever is still running.
	//
	// SIX HOURS BECAUSE THAT IS THE LENGTH OF A JOB, not the length of a
	// shutdown: GitHub's jobs.<job_id>.timeout-minutes defaults to 360. Every
	// other budget in this file bounds work BILLET is doing, where an hour is
	// already generous. This one bounds work somebody ELSE is doing, which is why
	// it is three orders of magnitude larger and has a separate ceiling.
	//
	// Overrunning it is not a failure state. The drain stops waiting and the
	// ordinary teardown destroys what is left — exactly what happened on every
	// shutdown before a drain existed.
	defaultDrainGrace = 6 * time.Hour

	// maxDrainGrace is the largest drain a caller may ask for.
	//
	// Separate from maxGrace because the two bound different things. Reusing the
	// teardown's one-hour ceiling here would refuse every honest value for a fleet
	// whose jobs run longer than an hour — which is most of them — and push those
	// operators back onto the job-killing restart the drain replaced.
	//
	// A day is still a ceiling: past this a typo is likelier than the intent, and
	// a service manager's stop timeout sized from a year-long drain would wait
	// effectively forever.
	maxDrainGrace = 24 * time.Hour

	// defaultShutdownGrace bounds the whole teardown.
	//
	// Renewal no longer stops when the caller cancels, which is what lets the
	// release destroy compute without the reaper taking the capacity underneath
	// it. The cost of that is a wedged teardown renewing leases forever while Run
	// never returns — so the teardown gets a deadline of its own.
	//
	// IT MUST EXCEED A LEGITIMATE TEARDOWN, and the first value chosen did not.
	// 90 seconds looked like a courteous answer to an operator's interrupt, but
	// the node command timeout is TEN MINUTES: an ordinary destroy of a node that
	// accepted the command and then went quiet takes longer than the whole grace,
	// so the watchdog would stop renewal, the reaper would reclaim the capacity,
	// and another tier could take a machine whose container was still being
	// destroyed. That is the precise failure the grace exists to avoid, caused by
	// the grace.
	//
	// This covers ONE such destroy with room to spare, and destroys run one at a
	// time — so N jobs whose nodes have all gone quiet need N times this and will
	// not get it. That is a known and deliberate gap rather than an oversight: the
	// alternative on offer is a grace of N times ten minutes, which is not a
	// grace, and concurrency cannot fix it while Destroy broadcasts to every node.
	// What it costs is bounded and reported — every request the pass could not
	// reach is named in the log, and its lease is kept rather than released, so
	// the reaper reclaims late instead of the capacity being handed out twice.
	//
	// The real fix is a destroy addressed to the lease's own node, which makes one
	// in-flight command per node safe and the whole teardown roughly one timeout
	// again. Tracked separately.
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

// WithDrainGrace bounds how long a stopping listener waits for the jobs it is
// already running to finish before it destroys them.
//
// Validated against maxDrainGrace rather than maxGrace, because this is the one
// budget here that waits on somebody else's job rather than on billet's own
// teardown. See defaultDrainGrace.
// WithHurrySignal gives the listener a channel whose closing ends the drain's
// wait.
//
// The drain is bounded by drainGrace, which is measured in hours because a job
// is. An operator who does not want to wait that long needs a lever that stops
// the WAITING without stopping the teardown — closing this is that lever, and
// what follows is the ordinary destroy-and-release the drain was standing in
// front of. Without it the only escape is killing the process, which strands
// exactly the containers the drain existed to protect.
func WithHurrySignal(c <-chan struct{}) Option {
	return func(l *Listener) { l.hurry = c }
}

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
// context.WithTimeout reads as already expired — so an operator who set an
// absurdly long grace would get a watchdog that fired instantly and stopped
// renewing while the destroys ran on. Saturating is the honest answer: a budget
// nobody will reach, rather than one that has already passed.
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
// SILENTLY SUBSTITUTING A DEFAULT WAS WRONG, and the argument for it was that
// zero is what a caller passes by leaving a field unset. It is not: omitting the
// option already selects the default, so passing zero is an explicit instruction
// — and context.WithTimeout reads it as "already over", which would fail the
// session close on its first instruction and skip every release. Quietly running
// for twelve minutes instead of the -1s somebody's arithmetic produced is not
// safer, it is the same misconfiguration with the evidence removed.
//
// The ceiling matters for the same reason. Three durations near MaxInt64 sum to
// a NEGATIVE one, which is an expired deadline; saturating instead gives a
// watchdog that fires in about 292 years, which is not a watchdog. A bound that
// is plainly longer than any real teardown and plainly finite avoids both.
//
// The ceiling is a PARAMETER because one of these budgets is not like the
// others: the drain waits on somebody else's job and is bounded by
// maxDrainGrace, while every teardown budget waits on billet's own work and is
// bounded by maxGrace. See maxDrainGrace for why they cannot be one number.
func checkGrace(name string, d, ceiling time.Duration) error {
	switch {
	case d <= 0:
		return fmt.Errorf("server: %s must be positive, got %s", name, d)
	case d > ceiling:
		return fmt.Errorf("server: %s must be at most %s, got %s", name, ceiling, d)
	}

	return nil
}

// WithCleanupRetryPacing sets how long a failed cleanup retry waits before the
// next attempt, and the ceiling that wait doubles towards.
//
// A first value of zero or less turns pacing off, so every pass tries every
// pending record. That is only ever what a test wants: in production it lets one
// permanently unreachable node occupy each pass ahead of a node that has just
// come back, because retries are sequential and a Destroy can wait the full node
// command timeout.
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
// is no room. That is what makes a first run against a real organization safe
// while no node runtime exists to launch anything — the whole path is exercised
// and no job is ever accepted.
//
// A negative value is rejected rather than clamped, because "advertise -1" means
// the caller computed something wrong and quietly turning that into 0 hides it.
func WithMaxCapacity(ceiling int) Option {
	return func(l *Listener) { l.maxCapacity = &ceiling }
}

// Run polls until the context is done.
//
// The order of operations here is the design, and it is not the obvious one:
// capacity is escrowed BEFORE it is advertised, and only what the escrow
// actually returned is advertised.
//
// Doing it the other way round — advertise what this tier could theoretically
// take, reserve when GitHub assigns — over-admits by construction on any host
// with more than one tier. Each listener computes a maximum from the same free
// pool, GitHub fills all of them at once, and by the time anyone notices, the
// jobs are already assigned. Reserving on assignment is too late: GitHub has
// made a promise billet cannot keep.
//
// The vendor's own listener package computes a desired runner count itself,
// which is why billet does not use it.
func (l *Listener) Run(ctx context.Context) error {
	// REFUSED RATHER THAN CORRECTED. These budgets decide when billet stops
	// protecting capacity whose compute may still be running, so a caller whose
	// arithmetic produced a nonsense one has to hear about it rather than get
	// twelve quiet minutes of something they did not ask for.
	if err := l.configError(); err != nil {
		return fmt.Errorf("server: listener for %s is misconfigured: %w", l.tier, err)
	}

	// SINGLE USE, said out loud rather than left as a property of the caller.
	//
	// Shutdown seals the cleanup loop permanently and closes the session, so a
	// second Run gets a listener that polls a closed session and whose retries all
	// return "sealed" — which retryCleanup reads as success, so it neither retries
	// nor backs off nor complains. Every completion whose destroy fails from then
	// on is silently abandoned.
	//
	// Server builds a fresh listener per tier run, so nothing in production does
	// this. A test did, with a comment saying it was restarting the way the
	// control plane restarts — which is exactly the misunderstanding an exported
	// Run with no guard invites.
	l.mu.Lock()
	reused := l.ran
	l.ran = true
	l.mu.Unlock()

	if reused {
		return fmt.Errorf("server: listener for %s has already run; build a new one", l.tier)
	}

	// Heartbeats run on their OWN clock, not between polls.
	//
	// A long poll was assumed to be about 50 seconds against a 90 second lease
	// TTL, which looks like comfortable margin. MEASURED against a real
	// organization it ran ~88 seconds — two seconds inside the TTL, on the first
	// poll ever made. The vendor's HTTP client also permits far longer once slow
	// responses and retries are counted. A poll that outlives the TTL leaves its
	// leases unrenewed, the reaper terminalises
	// them, another tier escrows the capacity — and the poll then returns an
	// assignment backed by a lease that is no longer this listener's.
	//
	// Tying renewal to the poll cadence made the safety of the whole escrow
	// depend on a timeout billet does not control.
	// AND IT DOES NOT INHERIT THE CALLER'S CANCELLATION, which is what made the
	// ordering below matter in the first place.
	//
	// `beat` used to be a child of ctx, so a caller cancelling to shut down killed
	// renewal at that instant — before the session close, before the release, and
	// before every slow remote destroy the release performs. Stopping the
	// heartbeat last was therefore decoration: it had already stopped. A teardown
	// slower than the lease TTL let the reaper take leases whose compute was still
	// being destroyed, and another tier could escrow that capacity while the
	// container was still on the host.
	//
	// The listener owns when renewal ends, and it ends after the release.
	beat, stopBeating := context.WithCancel(context.WithoutCancel(ctx))
	defer stopBeating()

	// SEPARATE LIFETIMES, not just separate clocks, because shutdown needs them
	// in opposite orders: the cleanup loop must be finished before the release
	// runs, and renewal must still be running while it does.
	//
	// AND IT DOES NOT INHERIT THE CALLER'S CANCELLATION EITHER, for the drain's
	// sake. This was a child of ctx, which was right while cancellation meant
	// "tear down now": the loop stopped at the same instant the polling did.
	// A drain keeps the listener alive for as long as a job runs, and a failed
	// destroy has to keep being retried across all of it — a cleanup record that
	// stopped being retried the moment the operator pressed Ctrl-C would sit
	// untouched for hours and then be handed to a teardown that has one grace to
	// finish it. The teardown still stops this loop explicitly, so the shutdown
	// ordering below is unchanged.
	sweep, stopSweeping := context.WithCancel(context.WithoutCancel(ctx))
	defer stopSweeping()

	var beating, sweeping sync.WaitGroup

	beating.Add(1)

	go func() {
		defer beating.Done()

		l.heartbeatLoop(beat)
	}()

	// A SEPARATE LOOP, NOT A STEP IN THE HEARTBEAT. Retrying a destroy means
	// broadcasting to nodes and waiting up to the command timeout, and the
	// heartbeat loop exists precisely so that renewal is never behind something
	// slow. Hanging this off the same tick would let one unreachable host delay
	// every renewal on the listener and expire the leases it was protecting —
	// which is the failure the separate clock was introduced to prevent.
	sweeping.Add(1)

	go func() {
		defer sweeping.Done()

		l.cleanupLoop(sweep)
	}()

	// CLOSE THEN RELEASE, in that order, and the listener owns both so the order
	// cannot be split across two functions again.
	//
	// The last maxCapacity GitHub saw stays live until the session ends. Releasing
	// escrow first therefore leaves a positive advertisement standing with nothing
	// backing it: a restart re-escrows that capacity while GitHub may still act on
	// the old promise, which breaks the central invariant during a clean shutdown
	// of all things. It was split across Run and its caller, and Go's defer order
	// put them the wrong way round.
	defer func() {
		// BOUNDED, because renewal no longer stops when the caller cancels.
		//
		// That is what lets the release destroy compute without the reaper taking
		// the capacity underneath it, and the price is a teardown that can no
		// longer be ended from outside: a Destroy or a session Close that ignores
		// cancellation would leave Run wedged forever, renewing every lease it
		// holds so the reaper can never reclaim them either, and the process unable
		// to exit. Neither Runner nor Session promises a bound, so the listener
		// imposes one.
		// ONE DEADLINE THAT EVERY PHASE INHERITS, rather than a sum the phases can
		// outlive.
		//
		// Renewal has to outlast the whole teardown: it used to stop when the
		// destroy budget expired, leaving the close and the release running with
		// nothing renewing, so held and promised leases could expire while the
		// session — and the positive maxCapacity GitHub last saw — was still open.
		// That is the close-before-release invariant broken by expiry rather than
		// by a call.
		//
		// Giving renewal the SUM of the three budgets did not establish that,
		// because each later phase started its own fresh budget when the previous
		// one finished: a destroy that ran to the summed deadline was followed by a
		// close with a full closeGrace still ahead of it. Deriving every phase from
		// one overall deadline makes each of them min(its own budget, what is left)
		// — so no phase can outlive renewal, by construction rather than by
		// arithmetic.
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

		// AND IT STOPS ON THE GRACE EVEN IF THE TEARDOWN NEVER FINISHES, which a
		// deadline on its own cannot deliver.
		//
		// The grace bounds everything that honours a context: the wait below, and
		// any Session or Runner that respects the one it is handed. NOTHING can
		// bound a Destroy that ignores its context outright, and neither interface
		// forbids one — so a bad implementation can still wedge Run, and pretending
		// otherwise would be the more dangerous comment to leave here.
		//
		// What must not ALSO happen is that a wedged listener goes on renewing.
		// That is what stops the reaper reclaiming, turning one stuck teardown into
		// capacity no operator gets back without killing the process. So renewal
		// has a deadline of its own: a wedged listener leaks a goroutine and says
		// so, and the ledger recovers without it.
		// JOINED, and it decides on `guard` alone rather than on whichever channel
		// select happens to pick. Both are ready once a healthy teardown finishes
		// and cancels the grace, and select chooses uniformly among ready cases —
		// so a perfectly clean shutdown could report that it had overrun, which is
		// a lie in the logs about the one condition an operator would act on.
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

			// THE COMPONENTS, not just the total. The sum is not a knob: an operator
			// reading "budget=13m" cannot tell which of the three to change, and
			// raising the wrong one only delays the reclaim rather than giving the
			// destroys longer.
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

		// AND CLEANUP IS STOPPED AND JOINED BEFORE ANY OF IT. Cancelling was not
		// enough: a retry blocked in a remote Destroy outlived Run, and came back
		// to call alloc.Release against a database the caller had every right to
		// have closed by then. Nothing joined it, so the only thing deciding
		// whether that happened was how long the node took to answer.
		//
		// The wait is bounded by the same grace: a runner that ignores its context
		// must not be able to hold the process open, and a bounded misbehaviour
		// that says so is better than an unbounded one that does not.
		// SEALED FIRST, then cancelled. Cancelling asks the loop to stop and says
		// nothing about what it is midway through starting; sealing settles that
		// under the same mutex the shutdown snapshot reads.
		l.seal()
		stopSweeping()

		// ITS OWN PHASE, with its own copy of the destroy budget.
		//
		// This waits for a retry that is inside a Destroy, so it can take exactly as
		// long as a destroy can — and it used to share the destroy phase's budget
		// with the destroys themselves. A retry that stalled for the whole grace
		// therefore left destroyAll starting with an already-dead context: every
		// remaining request reported as never attempted, its compute still running
		// and its lease left for the reaper. That is the close starving the release
		// again, one phase further up.
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

		// ONE DESTROY PASS FOR EVERYTHING, before the session closes.
		//
		// This used to be two: a drain for cleanup records, then releaseAll
		// destroying running jobs on its way past. That destroyed anything in both
		// sets twice, and — worse — ran them one after the other under a single
		// deadline, so a slow drain could eat the grace the release still needed.
		// Now the union is destroyed once, concurrently, and the release only ever
		// releases.
		// NO BUDGET CHECK HERE, deliberately: this point cannot be reached with the
		// overall budget spent. The join's context is min(shutdownGrace, what is
		// left), so when it returns there is always shutdownGrace + closeGrace +
		// releaseGrace still to run. A guard was written for this and removed — it
		// could never fire, and its mutant survived for exactly that reason, which
		// reads identically to missing coverage.
		destroyed := l.destroyAll(stopCtx)

		// A FRESH BUDGET FOR THE LOCAL HALF, because it was being starved by the
		// remote one — and one EACH, because the close was then starving the
		// release.
		//
		// Closing the session and releasing leases are fast and mostly local, but
		// they shared the destroy pass's deadline, so a slow destroy handed the
		// close an already-expired context; it failed, the early return skipped
		// releaseAll, and leases whose compute had been destroyed SUCCESSFULLY were
		// left for the reaper. Giving the pair one shared budget fixed that and
		// left a smaller version of it: a close that takes nearly all of it leaves
		// the releases — which run one at a time against a single SQLite writer —
		// to fail on the remainder, so a clean shutdown can still withhold the
		// whole tier's capacity until the reaper.
		closeCtx, endClose := context.WithTimeout(overall, l.closeGrace)
		defer endClose()

		// WHOSE FAULT, before the failure is attributed. A phase entered with an
		// expired overall budget fails without being attempted, and reporting that
		// as "could not close message session" blames the session for a deadline
		// the destroys had already spent.
		// STOPPED, not merely reported, and this one is reachable: a Destroy that
		// ignores its context can run past the whole budget and then return.
		// Logging and carrying on meant calling Close with an already-expired
		// context — and Session does not promise to honour one either, so a phase
		// just declared hopeless could block indefinitely, past the deadline
		// announced as final. There is nothing left to spend.
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

	// Seeded from the session before the first poll. A restart does not replay
	// messages for work already assigned, so a listener that waits to be told
	// about a backlog sits idle in front of one.
	l.observed = l.session.Statistics()
	l.reportOrphanedBacklog()

	// THE DRAIN IS A STATE OF THIS LOOP, NOT A PHASE OF THE TEARDOWN, and that is
	// the whole design rather than a detail of it.
	//
	// Putting it in the deferred teardown is the obvious place and cannot work:
	// by the time a deferred function runs, ctx is cancelled and the long poll is
	// dead, so the listener can no longer be TOLD that a job finished. A drain
	// there would wait for news that can never arrive, spend its entire budget,
	// and then destroy the jobs anyway — slower than not draining at all.
	//
	// So cancellation stops the listener taking NEW work and nothing else. It
	// keeps polling on a context of its own, because reporting is the handover:
	// the completion that empties `running` arrives through the same long poll
	// that offers new jobs.
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
		// THE TRANSITION IS CHECKED HERE AND AFTER EVERY CALL, because the
		// cancellation almost always lands DURING one rather than between two.
		// The listener spends nearly all of its life inside a long poll, so a
		// version that only looked at the top of the loop saw the poll fail with
		// context.Canceled first and returned — draining exactly never.
		if !draining && ctx.Err() != nil {
			draining = true
			// The budget has to START HERE, at the cancellation, which is why this
			// cannot be hoisted above the loop: a deadline created when Run began
			// would already be spent by the time anybody asked billet to stop.
			// `draining` makes it a once-per-Run assignment, not a per-iteration one.
			//nolint:fatcontext // Assigned at most once; see the comment above.
			pollCtx, endDrain = l.beginDrain(ctx)
		}

		if draining {
			if l.drained() {
				l.log.Info("everything running here has finished; stopping", "tier", l.tier)

				return ctx.Err()
			}

			if pollCtx.Err() != nil {
				// The drain's budget is gone, or a second signal cut it short.
				// What is still running is now the teardown's problem, and it
				// destroys it exactly as it did before a drain existed.
				// NOT "GitHub will reassign them". It does not: reassignment is
				// documented for a job assigned to a scale set but never acquired
				// by a runner, and says nothing about one a runner has already
				// started. Destroying a running container FAILS that job, and a
				// message that calls it a reassignment tells an operator the cost
				// is nothing when the cost is somebody's build.
				l.log.Warn("giving up on the jobs still running here; destroying them "+
					"will FAIL them, and GitHub does not requeue a job whose runner "+
					"vanished mid-execution",
					"tier", l.tier, "running", l.Running(), "grace", l.drainGrace)

				return ctx.Err()
			}
		}

		// NOT WHILE DRAINING. Escrow is how this listener claims capacity it
		// intends to advertise, and the drain has just handed back the capacity
		// nobody was using. Topping it up again would re-advertise the tier it is
		// trying to leave, and the drain could never reach zero.
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

// cancelledWhileServing reports whether a failed call should be read as the
// shutdown arriving mid-call rather than as a failure to report.
//
// THE FIRST VERSION LOOKED ONLY AT THE CLOCK, and swallowed every error that
// happened to coincide with a cancellation — including the fatal ones. The
// second looked only for context.Canceled, and was worse in a quieter way: a
// cancellation that lands inside handle() surfaces as whatever domain error that
// path produces, and those do not wrap the context error. The drain was then
// skipped for the most ordinary reason there is, intermittently, which is the
// shape of a feature that works in tests and not on a real machine.
//
// So the question is asked the other way round. Once the caller has asked billet
// to stop, an error is the shutdown arriving UNLESS it is one billet must stop
// for regardless of when it arrives. There is one of those, and it is named:
// a scale-set response billet cannot act on means it no longer knows which of
// its commitments are real, and draining against that session would be operating
// on exactly the state that is not safe to operate on.
func cancelledWhileServing(ctx context.Context, draining bool, err error) bool {
	if draining || ctx.Err() == nil {
		return false
	}

	return !errors.Is(err, ErrUntrustworthySession)
}

// beginDrain stops this listener taking new work and hands back the capacity
// nobody is using, returning the context the drain polls on.
//
// THE ADVERTISEMENT IS NOT FORCED TO ZERO, and the first version of this did
// force it. What billet sends on each poll is documented as the scale set's
// TOTAL capacity, so a constant zero while a job runs is a statement that is not
// true. Releasing the idle escrow instead makes capacity() — held + acquiring +
// running — fall to exactly the work still in flight, shrink as each job
// finishes, and reach zero by itself at the moment the drain is complete. The
// number GitHub sees stays honest and the completion condition needs no second
// source of truth.
//
// The advertisement is a courtesy to GitHub's scheduler and NOT the guard: a
// queued message can still arrive, and a redelivered one certainly can. What
// actually refuses the work is the local check in handle, for the same reason
// the dry run needs one.
func (l *Listener) beginDrain(ctx context.Context) (context.Context, context.CancelFunc) {
	// BOUNDED, and bounded only as far as anything here can be. The deadline is
	// observed between calls, so a Session that returns — however slowly — is
	// bounded by it, while one that blocks inside GetMessage forever is not. That
	// is the same limit the teardown documents for a Destroy that ignores its
	// context, and neither interface forbids one; saying so is more useful than a
	// comment claiming a guarantee this cannot give.
	//
	// The drain's context is built FIRST because the release below needs a live
	// one: ctx is already cancelled by the time this is called, and handing a
	// cancelled context to alloc.Release would fail every one of them and strand
	// the very capacity this is giving back.
	drainCtx, endDrain := context.WithTimeout(context.WithoutCancel(ctx), l.drainGrace)

	// A SECOND SIGNAL ENDS THE WAIT, not the teardown. What follows is the
	// ordinary destroy-and-release; only the waiting is cut short.
	//
	// The goroutine also selects on drainCtx so it cannot outlive the drain. A
	// version that only waited on hurry would park forever in every process that
	// drains normally, which is all of them.
	if l.hurry != nil {
		go func() {
			select {
			case <-l.hurry:
				endDrain()
			case <-drainCtx.Done():
			}
		}()
	}

	// NOT l.seal(), WHICH MEANS SOMETHING ELSE. Sealing stops the cleanup loop
	// starting new destroys, and belongs at the teardown where it is. A drain can
	// last as long as a job, so sealing here would leave every failed destroy
	// un-retried for hours and then hand the whole backlog to a teardown with one
	// grace to clear it — the opposite of what the drain is for.
	//
	// MARKED BEFORE THE ESCROW GOES BACK. The other order leaves a window in
	// which the capacity has been released but an offer can still be accepted, so
	// the listener would claim a job it no longer has anything to back.
	l.mu.Lock()
	l.draining = true
	l.mu.Unlock()

	released := l.releaseIdleEscrow(drainCtx)

	l.log.Info("draining: not taking new work, waiting for what is already running",
		"tier", l.tier, "running", l.Running(), "released_idle_leases", released,
		"grace", l.drainGrace)

	return drainCtx, endDrain
}

// releaseIdleEscrow hands back every lease this listener holds but has not given
// to a job, reporting how many.
//
// Only `held`. `acquiring` has been promised to a job that is starting and
// `running` is backing a live container; releasing either would let another tier
// escrow capacity that is already spoken for, which is the overcommit the whole
// escrow exists to prevent.
func (l *Listener) releaseIdleEscrow(ctx context.Context) int {
	// A SNAPSHOT TO ITERATE, BUT EACH LEASE LEAVES `held` ONLY WHEN IT IS
	// ACTUALLY RELEASED. The first version took the whole slice out up front and
	// appended the failures back afterwards, which is wrong twice over.
	//
	// While a lease is out of `held` the heartbeat cannot see it, so it stops
	// being renewed — and a release pass that runs longer than a lease TTL would
	// let the reaper reclaim one, after which appending it back would have this
	// listener advertising capacity it no longer owns. That is the double
	// admission the whole escrow exists to prevent, reached through the code that
	// was meant to hand capacity back.
	//
	// It also raced the heartbeat's own pruning: heartbeatHeld drops leases it has
	// lost, and replacing the slice wholesale would resurrect one it had just
	// dropped. Deleting by id leaves both free to act on the same collection.
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
// RUNNING ONLY, AND `acquiring` DELIBERATELY NOT. A promise is escrow held for
// work billet claimed from GitHub and has not been assigned, and GitHub may
// never assign it — another scale set can win the same offer. The listener keeps
// such a promise on purpose: renewLoop reports it as stale and holds its lease,
// because the thing that resolves it is the session ending, which releases every
// promise with it.
//
// The session ends in the teardown, which is on the far side of this wait. So a
// drain that waited for `acquiring` to empty would be waiting for something only
// the step after it can do: it would spend its entire budget, destroy nothing,
// and have achieved a six-hour pause. The end-to-end suite found exactly that —
// a control plane that would not stop, with running=0.
//
// A promise assigned DURING the drain is still launched and still waited for,
// because by then it is running work like any other.
func (l *Listener) drained() bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.running) == 0
}

// capacity is what this listener advertises: TOTAL escrowed, not free.
//
// All three collections count. Every lease in each of them was taken from the
// allocator, so the sum across listeners is still bounded by the budget.
//
// maxCapacity is the scale set's total capacity, not its spare — the vendor's own
// listener sends a configured maximum that does not move as jobs are assigned.
// Sending only the free half shrank the advertisement every time a job started,
// so a tier with room for two runners told GitHub "1" the moment the first job
// landed and the second slot went unused.
//
// The invariant still holds, and this is why the two halves are counted
// together: every lease in either was taken from the allocator, so the sum
// across listeners is still bounded by the budget.
func (l *Listener) capacity() int {
	l.mu.Lock()
	defer l.mu.Unlock()

	return len(l.held) + len(l.acquiring) + len(l.running)
}

// Held returns the leases this listener has escrowed and not yet handed to a job.
//
// Exported for tests, which cannot read the guarded field safely, and which need
// lease IDENTITY rather than a count: an escrow that was lost and rebuilt has the
// same size and different ids.
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
// TotalAssignedJobs is the documented scaling signal, and counting messages is
// explicitly not: a response carries at most 50 job entries and a large backlog
// is truncated, so the count is wrong exactly when it matters most.
func (l *Listener) Backlog() int {
	if l.observed == nil {
		return 0
	}

	return l.observed.TotalAssignedJobs
}

// reportOrphanedBacklog says out loud when GitHub believes this scale set is
// already running work that billet has no lease for.
//
// This is what the session statistics are FOR, and until a node runtime exists
// it is the only honest thing to do with them. A fresh listener holds nothing,
// so a non-zero TotalAssignedJobs means jobs were assigned to this scale set
// before the process restarted: GitHub is waiting on runners that died with it.
// Those jobs sit until GitHub's pickup deadline and are then reassigned, which
// looks from the outside like billet silently dropping work.
//
// Deliberately NOT a failure. The jobs are already lost by the time this runs
// and refusing to start would strand the tier's remaining capacity too. Nor is
// it a reconciliation — recovering them needs a node runtime that can adopt a
// running instance, which does not exist yet. Saying it plainly is what makes
// the gap visible instead of mysterious.
func (l *Listener) reportOrphanedBacklog() {
	backlog := l.Backlog()
	if backlog == 0 {
		return
	}

	l.log.Warn("github reports jobs already assigned to this scale set that billet has no lease "+
		"for; they were assigned before this process started and will be reassigned when "+
		"github's pickup deadline passes",
		"tier", l.tier, "assigned", backlog)
}

// stopping reports a shutdown as a shutdown.
//
// Cancelling the context does not produce a context error from everything it
// interrupts: SQLite surfaces an in-flight statement as "interrupted (9)", and
// an HTTP client can report a closed connection. Wrapping those and returning
// them makes an ordinary stop look like a fault — the operator reads a driver
// code where the truth is "you asked it to stop".
//
// The context is the authority. If it is done, that is the reason, whatever the
// layer underneath happened to say on its way out.
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
			// EACH PASS IS BOUNDED, and it has to be bounded here rather than by the
			// caller: this context deliberately does not inherit the caller's
			// cancellation, so without a deadline of its own an allocator call that
			// never returns holds l.mu forever. The teardown then blocks taking that
			// mutex, and the defer that would have stopped this loop is behind the
			// teardown. Renewal deadlocks the shutdown that was waiting for it.
			//
			// One interval, so a pass that overruns is abandoned and the next tick
			// tries again with three chances inside a TTL.
			pass, endPass := context.WithTimeout(ctx, l.heartbeatInterval())

			l.mu.Lock()
			l.heartbeatHeld(pass)
			l.mu.Unlock()

			endPass()
		}
	}
}

// cleanupLoop retries completions whose destroy failed, on its own clock.
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

// retryCleanup finishes completions whose destroy failed.
//
// Idempotent by contract on the runner's side, so retrying a destroy that
// actually succeeded is free — and the ONE thing that must not happen is
// releasing the lease before the compute is confirmed gone, which complete
// already refuses to do.
func (l *Listener) retryCleanup(ctx context.Context) {
	now := time.Now()

	l.mu.Lock()

	// ONLY THE ONES THAT ARE DUE. Everything here is retried in one sequential
	// pass and a single Destroy can wait the full node command timeout, so
	// attempting entries whose node has been refusing for an hour would push the
	// one that just recovered behind them.
	pending := make([]Job, 0, len(l.cleanup))

	for _, entry := range l.cleanup {
		if entry.due(now) {
			pending = append(pending, entry.job)
		}
	}

	l.mu.Unlock()

	for _, job := range pending {
		err := l.attempt(ctx, job)
		if err == nil {
			continue
		}

		l.log.Error("could not finish cleaning up a completed job; will retry",
			"tier", l.tier, "request", job.RequestID, "error", err)

		l.backOff(job.RequestID)
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
// Reports whether the wait completed. The watcher goroutine outlives a giving-up
// caller, which is deliberate: the thing being waited for is by definition
// misbehaving, and a goroutine parked on a WaitGroup is the cheapest way to
// contain it.
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

// destroyAll tears down every piece of compute this listener is responsible for
// and reports which requests are confirmed gone.
//
// THE UNION OF running AND cleanup, each destroyed exactly once. Those two sets
// overlap — a completion whose destroy failed keeps its lease in `running` — and
// destroying from each separately meant doing it twice for the overlap while
// running them in sequence under one deadline. Idempotence makes a second call
// safe, not free: it is another remote round trip against the same grace.
//
// Concurrent, because each Destroy can wait the node command timeout and in
// sequence no shutdown grace could ever be both long enough for a healthy
// teardown and short enough to be called a grace. Bounded, because a node
// executes commands one at a time: firing every request at once starts every
// timeout at once, so the later ones expire while the node is working through
// the earlier ones and healthy jobs are recorded as failed destroys.
//
// The backoff is ignored on purpose. It exists so a hopeless record cannot crowd
// out a live one across repeated passes, and this is the last pass there will be.
func (l *Listener) destroyAll(ctx context.Context) map[int64]bool {
	l.mu.Lock()

	requests := make([]int64, 0, len(l.running)+len(l.cleanup))

	// NOT WHAT A RETRY IS ALREADY INSIDE. The join gives that retry a whole
	// destroy budget to return in; when it does not, the destroy is still
	// happening, and putting the same request into this pass issues a SECOND one
	// that then wins the single teardown slot and spends the second budget on work
	// already in progress. Unrelated requests are never reached — which is the
	// starvation the separate join phase was meant to end, arriving by another
	// route.
	skipped := make([]int64, 0, len(l.destroying))

	for id := range l.running {
		if l.destroying[id] {
			skipped = append(skipped, id)

			continue
		}

		requests = append(requests, id)
	}

	for id := range l.cleanup {
		if l.destroying[id] {
			if _, running := l.running[id]; !running {
				skipped = append(skipped, id)
			}

			continue
		}

		if _, running := l.running[id]; !running {
			requests = append(requests, id)
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

	for _, requestID := range requests {
		wg.Add(1)

		go func() {
			defer wg.Done()

			// CHECKED BEFORE THE SELECT, because select does not prefer a ready
			// cancellation over a ready slot — it picks uniformly among whatever is
			// ready. With an already-expired budget and a free slot, roughly half
			// these goroutines went on to call Destroy anyway, so "we stop when the
			// budget is gone" was true only on average. It also made a mutation run
			// a coin toss, which is how it was found.
			if ctx.Err() == nil {
				select {
				case slot <- struct{}{}:
					defer func() { <-slot }()
				case <-ctx.Done():
				}
			}

			if ctx.Err() != nil {
				// NAMED, not silently skipped. Returning quietly left a request that
				// was never even attempted indistinguishable from one that was
				// destroyed: no outcome, no log, and for a cleanup-only record no
				// lease either — so the obligation simply evaporated on an ORDINARY
				// shutdown, not merely on a kill.
				l.log.Error("the shutdown grace ran out before billet tried to destroy this "+
					"job's compute; it was never attempted, and if no lease accounts for it "+
					"nothing will reclaim it until its host is swept or restarted",
					"tier", l.tier, "request", requestID)

				return
			}

			err := l.runner.Destroy(ctx, requestID)

			mu.Lock()
			done[requestID] = err == nil
			mu.Unlock()

			if err != nil {
				l.log.Error("could not destroy the compute for a job before stopping; it is "+
					"still running on its host, and if no lease accounts for it nothing will "+
					"reclaim it until that host is swept or restarted",
					"tier", l.tier, "request", requestID, "error", err)

				return
			}

			l.mu.Lock()
			delete(l.cleanup, requestID)
			l.mu.Unlock()
		}()
	}

	wg.Wait()

	return done
}

// attempt runs one cleanup retry, marked as in flight for its duration.
//
// THE UNMARKING IS A DEFER, and that is the whole reason this is a function. The
// mark makes the shutdown skip a request on the grounds that a retry is already
// destroying it, so an entry that outlives its attempt hides that request from
// teardown permanently — a container nobody destroys and nobody mentions. A
// panic anywhere under complete would have done it, and the safety of the skip
// depends on this pairing being unbreakable rather than merely written down.
func (l *Listener) attempt(ctx context.Context, job Job) error {
	l.mu.Lock()

	// CLAIMED UNDER THE SAME LOCK THAT SEALS, which is what makes the shutdown's
	// snapshot trustworthy.
	//
	// retryCleanup walks a snapshot of due jobs and cancellation does not stop it
	// mid-list: with only a context check, the loop could finish a slow destroy,
	// see the shutdown had already skipped past, and start a BRAND NEW one for the
	// next job — which the teardown was destroying at the same moment. Two
	// broadcasts to every node for one request, outside the single teardown slot,
	// and possibly after Run had returned. Checking a flag and then taking the
	// mark in two steps leaves the same window one instruction wide.
	if l.sealed {
		l.mu.Unlock()

		return nil
	}

	l.destroying[job.RequestID] = true
	l.mu.Unlock()

	defer func() {
		l.mu.Lock()
		delete(l.destroying, job.RequestID)
		l.mu.Unlock()
	}()

	return l.complete(ctx, job)
}

// seal stops the cleanup loop from starting any further destroys.
//
// Called BEFORE the loop is cancelled, because cancelling is a request and this
// is a fact: once it returns, every request not already marked in `destroying`
// is the teardown's alone.
//
// PERMANENT, AND THAT IS ONLY SAFE BECAUSE A LISTENER IS NEVER REUSED. Server
// builds and runs one in a single expression — `NewListener(...).Run(ctx)` —
// so nothing holds a reference across a Run. If that changes, a supervisor that
// retries a tier by calling Run again on the same value gets a listener whose
// cleanup loop is dead for the rest of the process: retrying nothing, reporting
// nothing, and skipping every request at the next shutdown. Add an unseal, or
// keep building a new one.
func (l *Listener) seal() {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.sealed = true
}

// backOff pushes a failed retry's next attempt out.
//
// Called for a release failure here and from complete for a destroy failure, so
// both halves of a retry that could not finish are paced the same way.
func (l *Listener) backOff(requestID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if entry, ok := l.cleanup[requestID]; ok {
		entry.failed(time.Now(), l.retryFirst, l.retryMax)
	}
}

// heartbeatInterval is how often held capacity is renewed: a third of the
// allocator's ACTUAL TTL, so two consecutive failures are survivable.
//
// Read from the allocator rather than from DefaultLeaseTTL. Deriving it from the
// default made the cadence right only for a default-configured allocator: with a
// shorter TTL every lease expired between beats, the reaper collected it, the
// listener re-escrowed, and advertised capacity climbed to six times the budget.
// A test caught it; nothing in the type system would have.
func (l *Listener) heartbeatInterval() time.Duration {
	if ttl := l.alloc.LeaseTTL(); ttl > 0 {
		return ttl / 3
	}

	return alloc.DefaultLeaseTTL / 3
}

// renewal is what a heartbeat established about one lease.
//
// THREE OUTCOMES, NOT TWO, and collapsing them is what made "stop advertising an
// uncertain lease" delete the obligation attached to it. "Not renewable" covers
// two different facts — the allocator SAYING the lease is not ours, and the
// allocator not answering at all — and only the first is evidence about who owns
// the compute.
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

// heartbeatHeld renews the leases this listener is advertising, and drops any it
// has lost.
//
// This is what makes the reaper safe to run at all. A lease expires after
// alloc.DefaultLeaseTTL without a heartbeat — 90 seconds — while a long poll
// blocks for about 50, so escrow held across two polls would be reclaimed
// underneath a listener that is still advertising it. Another tier could then
// escrow the same capacity, which is precisely the double-admission the escrow
// exists to prevent. Turning the reaper on without this would have broken the
// central invariant rather than protected it.
//
// A lease that cannot be renewed is DROPPED rather than retried. Failure here
// means the allocator no longer agrees this listener owns it — reaped, or fenced
// by a new holder — and continuing to advertise it would be advertising capacity
// somebody else now has.
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

		// THE LEASE GOES; THE OBLIGATION DOES NOT. This listener launched a
		// container for that request and losing the ledger entry does not stop it
		// running — a fence means another holder owns the CAPACITY, and a stale
		// renewal means nobody knows who owns it. Neither is a reason to forget the
		// compute, and GitHub will not send the completion again.
		//
		// So the entry moves to the cleanup set, where the only thing that
		// discharges it is a successful destroy. Deleting it outright left the
		// container untracked and reachable by nothing but an optional Sweeper.
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

	// AN ANSWER OUTRANKS A DEADLINE, and the order used to be the other way
	// round. The allocator can say "this lease is not yours" and have that answer
	// arrive a moment after the pass deadline expired; checking ctx.Err() first
	// discarded it and kept advertising a lease somebody else now holds. A context
	// error explains why there is no answer, so it may only be consulted when
	// there is none.
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

	// NO ANSWER. Shutting down, the pass ran out of its own deadline, or the
	// database is busy — the allocator never said this lease was not ours, so it
	// is kept. Dropping it would remove it from the release path too, and the
	// ledger would keep counting it until the reaper got it back.
	//
	// BUT NOT FOREVER. "No evidence it is lost" stops being a reason once the TTL
	// has passed without a single confirmed renewal: by then the reaper can have
	// taken it, and advertising capacity that is now someone else's is the exact
	// double-admission the escrow exists to prevent. Uncertainty for longer than
	// a lease can survive is not uncertainty.
	// The clock starts when the lease is TRACKED, not when the first renewal
	// fails. Starting it here handed a never-confirmed lease an extra TTL it had
	// never earned: escrowed at t=0 and expiring at t=TTL, its first failed
	// renewal at t=TTL/3 would set the clock there, and it stayed advertised past
	// t=4TTL/3 — well after the reaper could have taken it. Every entry point
	// seeds `confirmed` now, so a missing entry means the lease is not one of
	// ours to renew.
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

	// STAMPED BEFORE THE CALL, which is the only place a second clock can be
	// safely wrong.
	//
	// This has now been moved twice. Sampling after l.mu let a slow heartbeat pass
	// hold the mutex and date a lease TTL/3 late; sampling after Escrow RETURNED
	// was better and still wrong, because the goroutine can be descheduled between
	// the commit and the sample and date it arbitrarily late. Every one of those
	// errors runs in the same direction: a lease dated later than it really is
	// stays advertisable after the reaper could already have taken it.
	//
	// Taken beforehand, the error runs the other way — the lease is dated slightly
	// EARLIER than the allocator's own expiry basis, so the worst case is dropping
	// one billet still owns. That costs a re-escrow and the reaper reclaims late;
	// the opposite costs two tiers the same machine. The right fix is for Escrow
	// to return the allocator's authoritative expiry, which is filed with the rest
	// of the lifecycle work.
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
	// It used to start at the first FAILED renewal instead, which handed a
	// never-confirmed lease an extra TTL it had not earned: escrowed at t=0 and
	// expiring at t=TTL, a lease whose first heartbeat failed at t=TTL/3 had its
	// clock set there and stayed advertised past t=4TTL/3, long after the reaper
	// could have taken it.
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

	// COMPLETED IS PROCESSED FIRST, and the order is the fix.
	//
	// Without this the cycle never closes: the lease stays open until the reaper
	// expires it, holding capacity for a job that finished and recording the wrong
	// conclusion against it. But it has to come FIRST, because GitHub batches the
	// completion of one job with the offer of its replacement — it considers the
	// slot free the moment the job ends. Acquiring before releasing meant billet
	// claimed the replacement while still holding the finished job's lease, then
	// released it, and had nothing left to back the claim.
	//
	// finished is scoped to THIS MESSAGE and nothing longer, which is the whole
	// lifetime the problem has. A batch can carry Assigned and Completed for the
	// same request — that is an assigned-then-cancelled job, which GitHub does to
	// one no runner picks up in time — and processing completions first would
	// otherwise let that assignment take a lease for a job already over.
	//
	// It does not need to survive the call. A message is immutable, so a
	// redelivery carries the same completions and rebuilds this set before the
	// assignments are read. The previous version kept a 4096-entry map on the
	// listener, which bought nothing and cost something real: a request id GitHub
	// requeued after cancelling it would be silently skipped, and billet would sit
	// on the assignment until it timed out. A fixed count was never the semantic
	// lifetime of the fact.
	finished := make(map[int64]struct{}, len(msg.Completed))

	for _, job := range msg.Completed {
		finished[job.RequestID] = struct{}{}

		if err := l.complete(ctx, job); err != nil {
			return err
		}
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

	// GitHub returns what it ACTUALLY gave, which can be fewer than were asked
	// for — another scale set can win the same offer. Escrow reserved for an
	// offer billet did not get goes back immediately; holding it would strand
	// capacity waiting for an assignment that is never coming.
	// A response is only meaningful as a subset of the request, and a response
	// that is not one is not merely noteworthy — it means billet cannot tell what
	// it has committed to. An id nobody offered for has no reservation to match
	// it to, and if the response body is wrong about that id it may be wrong
	// about the others, so the reserved ids cannot be trusted either.
	//
	// This STOPS the listener, unlike an unbacked assignment, which declines and
	// carries on. The difference is what each condition means. An unbacked
	// assignment is reachable by ordinary races — the heartbeat drops a fenced
	// lease, a restart loses a promise — so killing the control plane over one is
	// disproportionate. A response outside its own contract is not reachable by
	// any race; it means the API broke, and continuing means acting on state that
	// cannot be reasoned about. Stopping is also the remedy: the session is
	// recreated, and GitHub redelivers or reassigns whatever was unacknowledged.
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

	// NOR WHILE THE PREVIOUS RUN'S COMPUTE IS STILL OWED. Same reasoning as
	// reserve: a pending cleanup is addressed by request id, so accepting an
	// assignment for that id hands the old retry a container and a lease that
	// belong to the new job.
	//
	// AND THIS IS NOT A DECLINE, whatever the reserve path can honestly call
	// itself. Leaving a request out of AcquireJobs is a real non-acquisition —
	// GitHub can offer it to another scale set or offer it again. There is no
	// equivalent call for a job already ASSIGNED to this scale set: billet simply
	// does not launch it, acknowledges the message, and the job waits for GitHub's
	// pickup deadline to reassign it. That is a delay, not a loss, and it is the
	// least bad option available here — holding the message unacknowledged instead
	// would re-deliver it every poll and block every message behind it.
	//
	// Doing better needs the assignment held locally until the obligation clears,
	// which needs a launch identity rather than a request id. Tracked separately.
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
			// DECLINED, not fatal. This used to return an error, on the grounds
			// that being assigned more than was advertised is a protocol violation
			// rather than a race billet can absorb. That was right when GitHub
			// over-assigning was the only way to get here; it is not any more.
			// Billet's own escrow can vanish underneath an acquisition — the
			// heartbeat drops a fenced lease, a restart loses the promise — and
			// killing the listener takes the whole control plane down with it,
			// stranding every tier's capacity over one job.
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

	// Best effort, and deliberately not fatal. The launch already failed; failing
	// the listener as well would take every tier down over one job that GitHub
	// will simply reassign.
	if relErr := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseFailed); relErr != nil {
		l.log.Warn("could not release the lease of a job that failed to start",
			"tier", l.tier, "lease", lease.ID, "error", relErr)
	}

	l.mu.Lock()
	delete(l.running, job.RequestID)
	l.mu.Unlock()

	return nil
}

// complete releases the lease a finished job was running on.
//
// Idempotent, because a redelivered Completed message must not fail: a job billet
// has already released is simply not in the map. Release with PhaseDone rather
// than inspecting a conclusion — the lease's job is finished either way, and the
// outcome belongs to job history rather than to the capacity ledger.
func (l *Listener) complete(ctx context.Context, job Job) error {
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
	if err := l.runner.Destroy(ctx, job.RequestID); err != nil {
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
			// A RETRY THAT FAILED AGAIN, so it waits longer before the next one.
			entry.failed(time.Now(), l.retryFirst, l.retryMax)
		} else if _, promised := l.acquiring[job.RequestID]; held || promised {
			if l.cleanup == nil {
				l.cleanup = make(map[int64]*pendingCleanup)
			}

			// Recorded ready to run: the first retry is immediate because a node
			// that was briefly busy is far and away the common case.
			l.cleanup[job.RequestID] = &pendingCleanup{job: job}
		}

		// AND AN EXISTING RECORD IS NEVER DROPPED HERE, which is a different rule
		// from the one above and was briefly conflated with it.
		//
		// Losing the lease does not prove the container is gone. A record exists
		// because this listener launched something and could not destroy it; if
		// the lease is then fenced or reaped, the obligation is unchanged — the
		// capacity is someone else's, but the compute is still ours to remove, and
		// GitHub will not redeliver the completion that would ask again.
		//
		// Dropping it looked safe because both shipped runners implement Sweeper,
		// which destroys compute no lease is holding. But Sweeper is OPTIONAL on
		// the Runner interface, so that reasoning makes correctness depend on which
		// runner is plugged in — with a non-sweeping one, nothing destroys the
		// container until the host restarts. An obligation is not discharged by
		// noticing that something else would probably have covered it.
		//
		// The pile-up this can cause is real, and an earlier version of this comment
		// argued it away incorrectly. The claim was that a record's lease stays in
		// `running` being renewed, so its capacity is never reissued and the map is
		// bounded by the tier's capacity. That holds only while the lease is OURS —
		// and the case this whole rule is about is the one where it is not. Once the
		// lease is reaped the capacity IS reissued, a new job can take it, complete,
		// fail to destroy, and add another record. Ownership loss is exactly what
		// unbinds the growth.
		//
		// So retention needs scheduling around it rather than a reassuring argument:
		// entries back off (retryEvery, capped at maxRetryEvery) so a node that is
		// never coming back cannot occupy every pass ahead of one that has just
		// recovered. What that does NOT fix is surviving a restart — the map is in
		// memory, so a process that dies still forgets. That needs durable cleanup
		// state and is tracked separately.

		l.mu.Unlock()

		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	lease, ok := l.running[job.RequestID]

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

		return nil
	}

	if err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		// THE ENTRY STAYS, and that is the whole point of keeping one. Deleting it
		// when the DESTROY succeeded lost the retry to a transient release failure:
		// the lease stayed in `running` being renewed forever, GitHub will not
		// redeliver a completion it has already acknowledged, and nothing else was
		// ever going to try the release again.
		return fmt.Errorf("server: release lease %s for finished request %d: %w",
			lease.ID, job.RequestID, err)
	}

	// RELEASED, so the job is finally over and there is nothing to retry.
	delete(l.running, job.RequestID)
	delete(l.acquiring, job.RequestID)
	delete(l.cleanup, job.RequestID)

	return nil
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

	l.held = nil
	l.acquiring = make(map[int64]*promise)
	l.mu.Unlock()

	release := func(lease *alloc.Lease) {
		if err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			l.log.Warn("could not release escrowed capacity",
				"tier", l.tier, "lease", lease.ID, "error", err)
		}
	}

	for _, lease := range held {
		release(lease)
	}

	// RUNNING leases are DESTROYED before they are released, and the comment that
	// used to sit here predicted this: releasing them was only right while nothing
	// could actually run a job. Now that something can, freeing the capacity while
	// a container or microVM is still on the host lets another tier escrow it and
	// overcommits the machine.
	//
	// A lease whose compute will not die is NOT released. Capacity that the reaper
	// reclaims late is recoverable; capacity handed out twice is not.
	//
	// This does kill work in flight, and it FAILS those builds — GitHub requeues
	// a job that was assigned but never picked up, and says nothing about one a
	// runner has already started. Tearing them down is still right: billet is
	// stopping and can no longer manage those jobs, so a failed build beats
	// containers nobody is tracking.
	//
	// AN ORDINARY SHUTDOWN RARELY GETS HERE WITH ANYTHING RUNNING, because the
	// drain waits first and this only inherits what outlived its budget. A Run
	// that returns for its own reasons — an untrustworthy session, a poll error
	// it cannot continue past — never drained at all, so for those this is the
	// first and only stop and everything still running is destroyed.
	//
	// DESTROYED ALREADY, by destroyAll, which is why this only releases. Doing
	// both here meant the teardown could not be planned as a whole: the drain and
	// the release each had their own idea of what needed destroying and neither
	// could see the other's work.
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

		release(lease)
	}

	// Promised escrow too. The acquisition was made to GitHub, but nothing has
	// been assigned yet and nothing can be launched, so holding it past shutdown
	// would strand it until the reaper.
	for _, lease := range promised {
		release(lease)
	}
}
