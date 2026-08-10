// Package server is billet's control plane: the per-tier scale-set listeners
// and the scheduler that turns assigned jobs into launched instances.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// shutdownGrace bounds the teardown, so an unbounded Destroy or session close
	// cannot keep Run — and the renewal that outlives it — running forever.
	shutdownGrace time.Duration

	// retryFirst and retryMax pace the cleanup retries. retryFirst <= 0 turns
	// pacing off entirely, which only tests ask for.
	retryFirst time.Duration
	retryMax   time.Duration

	// runner turns assigned leases into compute. Never nil; see noRunner.
	runner Runner

	// observed is the last statistics GitHub reported. TotalAssignedJobs is the
	// documented scaling signal — counting messages is not, because a response
	// carries at most 50 and a large backlog is truncated.
	observed *Statistics
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
		stalePromise:  defaultStalePromise,
		shutdownGrace: defaultShutdownGrace,
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
	// defaultShutdownGrace bounds the whole teardown.
	//
	// Renewal no longer stops when the caller cancels, which is what lets the
	// release destroy compute without the reaper taking the capacity underneath
	// it. The cost of that is a wedged teardown renewing leases forever while Run
	// never returns — so the teardown gets a deadline of its own. Longer than any
	// ordinary destroy, short enough that an operator's interrupt is answered.
	defaultShutdownGrace = 90 * time.Second
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
	return func(l *Listener) { l.shutdownGrace = d }
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
	sweep, stopSweeping := context.WithCancel(ctx)
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
		stopCtx, endGrace := context.WithTimeout(context.WithoutCancel(ctx), l.shutdownGrace)
		defer endGrace()

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
		guard := make(chan struct{})
		defer close(guard)

		go func() {
			select {
			case <-stopCtx.Done():
				l.log.Error("this listener's teardown outran its shutdown grace; renewal is "+
					"stopping so the reaper can reclaim what it still holds, but the compute "+
					"it was destroying may still be running on its host",
					"tier", l.tier, "grace", l.shutdownGrace)

				stopBeating()
			case <-guard:
			}
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
		stopSweeping()

		if !waitWithin(stopCtx, &sweeping) {
			l.log.Error("a cleanup retry did not return within the shutdown grace; it may "+
				"still release a lease after this listener has stopped",
				"tier", l.tier, "grace", l.shutdownGrace)
		}

		// WHAT THE RELEASE WILL NOT COVER, done while there is still grace for it.
		// releaseAll visits held, running and acquiring; a completion whose lease
		// was already reaped is in none of them, so without this its container
		// outlives the process and the obligation lasts only until the next
		// shutdown — which is not what "only a successful destroy discharges it"
		// can mean.
		l.drainCleanup(stopCtx)

		if err := l.session.Close(stopCtx); err != nil {
			l.log.Warn("could not close message session; capacity is held until it expires",
				"tier", l.tier, "error", err)

			// NOT released. A session billet could not close may still be handing
			// this scale set work, and handing the capacity back would let another
			// tier escrow it while GitHub believes this one still has room. The
			// reaper expiring the lease is the safe way out.
			return
		}

		l.releaseAll(stopCtx)
	}()

	// Seeded from the session before the first poll. A restart does not replay
	// messages for work already assigned, so a listener that waits to be told
	// about a backlog sits idle in front of one.
	l.observed = l.session.Statistics()
	l.reportOrphanedBacklog()

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := l.refillEscrow(ctx); err != nil {
			return stopping(ctx, err)
		}

		msg, err := l.session.GetMessage(ctx, l.lastMessageID, l.capacity())

		// A timed-out long poll is the ordinary case. Poll again immediately —
		// the escrow is KEPT, because releasing and retaking it every poll
		// would hand the gap to another tier and produce exactly the flapping the
		// escrow exists to avoid.
		if errors.Is(err, ErrNoMessage) {
			continue
		}

		if err != nil {
			return stopping(ctx, fmt.Errorf("server: poll %s: %w", l.tier, err))
		}

		if err := l.handle(ctx, msg); err != nil {
			return stopping(ctx, err)
		}
	}
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
		err := l.complete(ctx, job)
		if err == nil {
			continue
		}

		l.log.Error("could not finish cleaning up a completed job; will retry",
			"tier", l.tier, "request", job.RequestID, "error", err)

		l.backOff(job.RequestID)
	}
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

// drainCleanup destroys compute for completions whose lease is already gone.
//
// These are invisible to releaseAll, which walks the lease maps, and the loop
// that would have retried them has just been stopped. A record whose lease is
// still held needs nothing here — releaseAll destroys it on the way past.
func (l *Listener) drainCleanup(ctx context.Context) {
	l.mu.Lock()

	orphaned := make([]Job, 0, len(l.cleanup))

	for id, entry := range l.cleanup {
		if _, running := l.running[id]; running {
			continue
		}

		if _, promised := l.acquiring[id]; promised {
			continue
		}

		orphaned = append(orphaned, entry.job)
	}

	l.mu.Unlock()

	// IGNORING THE BACKOFF, deliberately. It exists so a hopeless entry cannot
	// crowd out a live one across repeated passes; this is the last pass there
	// will ever be, so the only thing waiting achieves is losing the work.
	for _, job := range orphaned {
		if err := l.runner.Destroy(ctx, job.RequestID); err != nil {
			l.log.Error("could not destroy the compute for a completed job before stopping; "+
				"it is still running on its host and no lease accounts for it, so nothing "+
				"will reclaim it until that host is swept or restarted",
				"tier", l.tier, "request", job.RequestID, "error", err)

			continue
		}

		l.mu.Lock()
		delete(l.cleanup, job.RequestID)
		l.mu.Unlock()
	}
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
		if l.renew(ctx, lease) {
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
		if !l.renew(ctx, lease) {
			delete(l.running, id)

			// THE PENDING RETRY STAYS. Losing the lease ends this listener's claim
			// on the capacity, not its obligation to destroy what it started: the
			// container is still running on a host, and the record is the only
			// thing that will ask again.
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

		if !l.renew(ctx, p.lease) {
			delete(l.acquiring, id)
		}
	}
}

// renew heartbeats one lease and reports whether it is still this listener's.
func (l *Listener) renew(ctx context.Context, lease *alloc.Lease) bool {
	err := l.alloc.Heartbeat(ctx, lease.ID, lease.Epoch)
	if err == nil {
		return true
	}

	if ctx.Err() != nil {
		// Shutting down, or this renewal pass ran out of its own deadline. Keep it
		// either way: on shutdown the release path is about to hand it back, and on
		// a slow pass the allocator never said this lease was not ours. Dropping it
		// would stop advertising capacity billet still holds, on no evidence.
		return true
	}

	// FENCED means the allocator has given this lease to someone else, so it is
	// genuinely no longer ours and continuing to advertise it would be
	// advertising capacity somebody else now holds.
	//
	// Anything else is an operational failure — a busy database, a cancelled
	// statement — and the lease is probably still fine. It is kept, because
	// dropping it removes it from the release path too: the ledger keeps counting
	// it and only the reaper ever gets it back.
	if !errors.Is(err, alloc.ErrFenced) && !errors.Is(err, alloc.ErrLeaseNotFound) {
		l.log.Warn("could not renew an escrowed lease; keeping it",
			"tier", l.tier, "lease", lease.ID, "error", err)

		return true
	}

	l.log.Warn("lost an escrowed lease; no longer advertising it",
		"tier", l.tier, "lease", lease.ID, "error", err)

	return false
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

	leases, err := l.alloc.Escrow(ctx, l.tier, room)
	if err != nil {
		return fmt.Errorf("server: escrow for %s: %w", l.tier, err)
	}

	l.mu.Lock()
	l.held = append(l.held, leases...)
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

	// Topped up between the release and the acquisition, so the slot just freed is
	// available to back the offer that arrived with it. It may lose the race to
	// another tier — escrow is first-come — and that is a fairness question, not a
	// correctness one: what matters is that billet does not claim work it cannot
	// back. See TestAGreedyTierCanTakeTheWholeBudget.
	if len(msg.Available) > 0 {
		if err := l.refillEscrow(ctx); err != nil {
			return err
		}
	}

	// AVAILABLE is what gets acquired. Available is the offer; Assigned is the
	// confirmation that an offer was claimed. Acquiring from Assigned asks GitHub
	// to claim work it has already handed over, and drops every offer.
	if err := l.acquire(ctx, msg.Available); err != nil {
		return err
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

		return fmt.Errorf("server: %s acquired job requests it did not offer for "+
			"(unrequested %v, requested %v); refusing to continue against a scale-set "+
			"response that is not a subset of its request", l.tier, extra, reserved)
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
func (l *Listener) releaseAll(ctx context.Context) {
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
	// This does kill work in flight, which is honest rather than good: billet is
	// stopping and can no longer manage those jobs, so tearing them down and
	// letting GitHub reassign beats leaving containers nobody is tracking. A
	// graceful drain — stop taking new work, wait for the running jobs, then exit
	// — is the thing that makes a restart free, and it is filed rather than
	// smuggled in here.
	for requestID, lease := range running {
		if err := l.runner.Destroy(ctx, requestID); err != nil {
			// KEPT in `running`, not dropped. Clearing it here was the bug: the
			// listener stops heartbeating, the reaper terminalises the lease on
			// its TTL, and another tier escrows a machine whose container is
			// still alive. Holding the reference is what lets a supervisor's
			// restart find it again, and it is the only thing that makes "keep
			// the capacity" true rather than a slower way of losing it.
			l.log.Error("could not destroy the compute for a running job; its lease is kept "+
				"rather than released, and this instance needs manual cleanup if billet does "+
				"not come back",
				"tier", l.tier, "request", requestID, "lease", lease.ID, "error", err)

			l.mu.Lock()
			l.running[requestID] = lease
			l.mu.Unlock()

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
