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
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/dispatch"
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

// RunnerRegistry removes a GitHub registration before its guest is destroyed.
// A failed removal leaves compute and capacity held so GitHub cannot race a new
// assignment onto a guest Billet is tearing down.
type RunnerRegistry interface {
	RemoveRunner(ctx context.Context, runnerID int64, runnerName string) error
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

// errNoRunner means no compute is attached to this control plane.
var errNoRunner = errors.New("server: no runner is configured, so nothing can start this job")

// noRunner is the default, and it FAILS CLOSED.
//
// Returning an error routes the job into the ordinary failed-launch path, so the
// capacity goes back and GitHub reassigns it. Reporting success would hold the
// capacity, run nothing, and hang the job until GitHub's pickup deadline with no
// error anywhere.
type noRunner struct{ log *slog.Logger }

func (n noRunner) Launch(_ context.Context, lease *alloc.Lease, job dispatch.Job) error {
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
// local ledger may still converge, so Run retries it. Quarantine may acknowledge
// it only when no candidate work is held. Other refusals remain immediately fatal.
var errQuarantinableCompletion = fmt.Errorf("%w: the completion has no safe identity",
	ErrUntrustworthySession)

// errQuarantinableStarted marks an immutable contradiction confined to one
// same-tier pool member. Operational failures must remain retryable and leave
// the source unacknowledged rather than destroying a runner on a failed read.
var errQuarantinableStarted = fmt.Errorf("%w: the started identity contradicts its pool member",
	ErrUntrustworthySession)

// poisonedMessageError carries the valid completions beside a poison so they can
// be made durable after the whole message is finally acknowledged. A candidate
// hold forbids that acknowledgement even after the quarantine retry budget.
type poisonedMessageError struct {
	cause       error
	completions []dispatch.Job
	held        bool
}

func (e *poisonedMessageError) Error() string { return e.cause.Error() }
func (e *poisonedMessageError) Unwrap() error { return e.cause }

// Message is one batch of scale-set news.
type Message struct {
	MessageID  int64
	Statistics *Statistics
	// Available is work GitHub is OFFERING. Acquiring one of these is how a
	// scale set claims it.
	Available []dispatch.Job
	// Assigned is work this scale set has been given, which is the confirmation
	// that an acquisition succeeded.
	Assigned []dispatch.Job
	// Started binds a registered pool member to the job it actually consumed.
	// GitHub may choose a different member than the assignment that caused Billet
	// to scale up.
	Started   []dispatch.Job
	Completed []dispatch.Job
}

// identityWriteLimit bounds one job-identity write, which is a diagnostic on
// the poll path.
const identityWriteLimit = 2 * time.Second

// historyJob is what a message said about its job, as job_history keeps it.
func historyJob(j dispatch.Job) alloc.HistoryJob {
	return alloc.HistoryJob{JobID: j.JobID, Owner: j.Owner, Repository: j.Repository,
		WorkflowRef: j.WorkflowRef, Name: j.JobName, Event: j.Event}
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

	// poolLaunchBatch is how many pool members a reconciliation starts at once;
	// zero or one launches each before buying the next. See WithPoolLaunchBatch.
	poolLaunchBatch int
	// batchBuying is set, under mu, while a pool pass buys its batch, and
	// batchBought records that it bought: the admission order re-dates the tier
	// once for the pass rather than once per purchase.
	batchBuying bool
	batchBought bool

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

	// TEST-ONLY boundaries for losing backing after admission captures its turn
	// or refill target. Nil in every deployment; neither replaces the operation.
	beforePoolReconcile func()
	beforeEscrowRefill  func()

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
	// redelivered message is recognised rather than assigned twice. Every entry
	// was bought from the allocator before its runner launched; that purchase,
	// not the advertisement, is what keeps the fleet within its budget.
	running map[int64]*alloc.Lease
	// Resolved aliases retain consumed offer facts while the lease is running.
	// A durable busy pool binding supersedes these intended-job facts.
	runningJobs map[int64]actualJobIdentity
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

	// Guarded by mu; a failed exchange never becomes a confirmed advertisement.
	capacitySent      *int
	capacityConfirmed *int
	capacityExchange  string

	lastMessageID int64
	// Guarded by mu. A hold blocks reconciliation until its message is settled
	// or the session closes; an operational error must not discard it.
	heldMessageID *int64

	// maxCapacity caps what this listener advertises. nil lets the escrow decide.
	maxCapacity *int

	// order decides whether this tier may buy capacity while another waits. Nil
	// for a standalone listener, which has no peers to be fair between.
	order *admissionQueue

	// waitingFor is how much of GitHub's assigned work this tier could not buy
	// capacity for at its last reconciliation. Guarded by mu; reported, never
	// scheduled on.
	waitingFor int

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
	runner   dispatch.Runner
	registry RunnerRegistry
	// offline is what reconcilePool remembers about idle members between polls,
	// and now its clock; nil is the wall clock. See retireOfflineMembers.
	offline offlineWatch
	now     func() time.Time
	// claimRetirement stands in for alloc.RetirePoolRunner on the offline path,
	// so a test can fail the ledger write after GitHub accepted the withdrawal.
	claimRetirement func(context.Context, string) error
	// cacheSpec and runEvidence decide what a completed job's caches may
	// publish. See WithCachePublication.
	cacheSpec   config.CacheSpec
	runEvidence RunEvidence
	// completionStore keeps authoritative job results across an ACK followed by a
	// process stop, until the node accepts result-dependent teardown.
	completionStore completionStore
	// writeJobResult isolates diagnostic write failures in tests. Nil uses the
	// allocator directly; identity reads and completion settlement are unaffected.
	writeJobResult func(context.Context, string, string, int64) error

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
	// restartHandoff makes a stop while admission is open a handoff rather than a
	// drain; see WithRestartHandoff. handingOff is Run's decision, read only by
	// its own teardown on the same goroutine.
	restartHandoff bool
	handingOff     bool

	// leadershipLost answers whether this process has stopped being this
	// deployment's controller. Nil outside the control plane; see
	// WithLeadershipLost.
	leadershipLost func() bool

	// heartbeatOverrun is told when a heartbeat pass is still running as the
	// next falls due. Nil outside the control plane; see
	// WithHeartbeatOverrunReport.
	heartbeatOverrun func()

	// reopen opens a replacement session after a poll failed past the client's
	// own retries. Nil for a standalone listener, whose Run returns that failure
	// instead; see WithSessionReopen.
	reopen func(context.Context) (Session, error)
	// Recovery state, all of it Run's goroutine's. recoverCause is set from a
	// recoverable poll failure until a replacement session opens, and
	// sessionFailures counts failed polls and attempts since the last poll that
	// answered. sessionClosed means the current session is known closed, so
	// nothing closes it again. closing and opening are a Close and an open
	// recoverSession started and has not seen finish: the vendored client holds
	// a per-target mutex no context reaches, so they run aside and a later
	// attempt, or the teardown, waits for the same call rather than starting
	// another.
	recoverCause    error
	sessionFailures int
	sessionClosed   bool
	closing         chan error
	opening         chan openedSession
}

// openedSession is what an open recoverSession started came back with.
type openedSession struct {
	session Session
	err     error
}

// NewListener builds a listener for one tier.
func NewListener(a *alloc.Allocator, tier string, session Session, opts ...Option) *Listener {
	l := &Listener{
		alloc:         a,
		tier:          tier,
		session:       session,
		log:           slog.Default(),
		runningJobs:   make(map[int64]actualJobIdentity),
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

// fenced reports that this listener must tear down without acting on anything.
func (l *Listener) fenced() bool {
	return l.leadershipLost != nil && l.leadershipLost()
}

// Run polls until the context is done.
//
// The order of operations is the design: a lease is bought atomically from the
// allocator BEFORE each runner launches, and nothing launches without one. The
// advertisement is a steady ceiling (steadyAdvertisement) that nothing backs, so
// GitHub may assign more across tiers than the fleet can run at once; the
// surplus waits, assigned, until a purchase succeeds (#140).
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
		overall, endOverall := context.WithTimeout(context.WithoutCancel(ctx), l.teardownBudget())
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

		// A HANDOFF STOPS HERE: the session stays open and nothing is released, as
		// WithRestartHandoff says. Closing the session is what would let GitHub
		// lower the advertisement and orphan what it re-offers meanwhile, and the
		// escrow behind that advertisement is reclaimed once it stops being
		// renewed, exactly as after a crash.
		if l.handingOff {
			l.mu.Lock()
			held, promised := len(l.held), len(l.acquiring)
			l.mu.Unlock()

			l.log.Info("handed over: closed no message session and handed back no capacity, "+
				"for the next control plane to take over", "tier", l.tier, "running", l.Running(),
				"held", held, "promised", promised)

			return
		}

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

		// A session recoverSession already closed carries no advertisement, so the
		// release below is licensed without closing it twice; one whose close it
		// started is awaited rather than closed again.
		if !l.sessionClosed {
			closeSession := func() error { return l.session.Close(closeCtx) }

			// ASIDE WHILE A RECOVERY IS UNDER WAY: an open it left behind can hold
			// the vendored client's per-target mutex, which this Close would wait
			// on and no context reaches. A close it already started is awaited.
			if l.closing != nil || l.recoverCause != nil {
				closeSession = func() error {
					if l.closing == nil {
						l.startClose(closeCtx, func() {})
					}

					_, err := l.awaitClose(closeCtx)

					return err
				}
			}

			if err := closeSession(); err != nil {
				l.log.Warn("could not close message session; capacity is held until it expires",
					"tier", l.tier, "error", err)

				// NOT released. A session billet could not close may still be handing
				// this scale set work, and handing the capacity back would let another
				// tier escrow it while GitHub believes this one still has room. The
				// reaper expiring the lease is the safe way out.
				return
			}
		}
		l.mu.Lock()
		l.heldMessageID = nil
		l.mu.Unlock()

		releaseCtx, endRelease := context.WithTimeout(overall, l.releaseGrace)
		defer endRelease()

		l.releaseAll(releaseCtx, destroyed)
	}()

	if err := l.refreshAdoptedCapacity(ctx); err != nil {
		l.noteStop(ctx)

		return err
	}

	// A restart does not replay messages for work already assigned, so a listener
	// that waits to be told about a backlog sits idle in front of one.
	l.observed = l.session.Statistics()
	l.reportOrphanedBacklog()
	if l.observed != nil {
		if err := l.reconcilePool(ctx, l.observed.TotalAssignedJobs); err != nil {
			l.noteStop(ctx)

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
		// Polls that returned, the idle slots last judged, and the poll count
		// when that set was first seen.
		polled    int
		idleSeen  map[int64]bool
		idleSince int
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
			if l.handsOff(ctx) {
				l.handingOff = true
				l.log.Info("handing over to the next control plane: the jobs running here keep "+
					"running and are re-adopted, and no message session is closed, so what "+
					"GitHub assigns meanwhile is redelivered to the next session",
					"tier", l.tier, "running", l.Running())

				return ctx.Err()
			}

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

			// A WHOLE POLL AFTER EVERY SLOT IN THE SET WAS FIRST SEEN IDLE, because a
			// Started for one can be in the next message. A slot joining the set
			// restarts the window; one leaving it does not.
			idle := l.idleMembersOnly(pollCtx)

			switch {
			case idle == nil:
				idleSeen = nil
			case idleSeen == nil || !subsetOf(idle, idleSeen):
				idleSeen, idleSince = idle, polled
			case polled > idleSince:
				l.log.Info("only pool runners that never started a job remain; stopping and "+
					"leaving them registered, with their capacity charged, for the next "+
					"control plane to adopt",
					"tier", l.tier, "idle", len(idle))

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
		// BEFORE THE POOL RECONCILES, so capacity the force returns is available to
		// this poll's launches rather than the next one's.
		//
		// NOT GUARDED BY !draining. An operator who drained, waited, and gave up
		// waiting is precisely who runs this, and refusing to act during a drain
		// would leave them with no orderly way out of the state the drain put them
		// in.
		l.forceDestroy(pollCtx)

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
			// affirmative per-registration evidence, which retireOfflineMembers
			// supplies for a runner GitHub reports offline; an online idle member
			// still waits for the aggregate to move.
			//
			// The failure direction is the safe one. A registration that lingers
			// keeps its lease non-terminal, so the quiescence barrier stays
			// un-quiet and a drain keeps waiting and keeps reporting what it is
			// waiting for. The seal under-delivers visibly rather than reporting a
			// deployment quiesced that is not.
			if l.observed != nil && !l.messageHeld() {
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

		// A DRAINING OR SEALED LISTENER ADVERTISES WHAT IS COMMITTED, which falls to
		// the work in flight and reaches zero by itself; a constant zero while a job
		// runs is untrue, since what billet sends is the scale set's total capacity.
		// Otherwise every tier advertises its steady ceiling.
		advertised := l.committedCapacity()
		withdrawnWhenSent := true

		if !draining && !quiesced {
			advertised = l.steadyAdvertisement()
			withdrawnWhenSent = false
		}
		// A RECOVERY THE DRAIN'S START CUT SHORT RESUMES BEFORE ANY POLL, because
		// the session in hand is the one it was replacing.
		if l.recoverCause != nil {
			if stop, rerr := l.recoverOrStop(ctx, pollCtx, draining, l.recoverCause); stop {
				return rerr
			}

			continue
		}

		l.reportCapacity(pollCtx, &advertised, "in flight")
		msg, err := l.session.GetMessage(pollCtx, l.lastMessageID, advertised)
		if err == nil || errors.Is(err, ErrNoMessage) {
			polled++
			l.sessionFailures = 0
			l.reportCapacity(pollCtx, &advertised, "confirmed")
		} else {
			l.reportCapacity(context.WithoutCancel(pollCtx), nil, "ambiguous")
		}

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

			if l.observed != nil && !l.messageHeld() {
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
			// THE ADVERTISEMENT IS NOT A FLOOR UNDER THE ESCROW ANY MORE. It is a
			// steady ceiling that nothing backs (steadyAdvertisement), so keeping
			// escrow up to it would hold idle capacity forever — #116 again. What
			// is kept is what is committed; a runner buys its own lease at launch.
			if !draining && !quiesced {
				l.releaseIdleEscrowAbove(pollCtx, l.targetCapacity())
			}
			l.reportCapacity(pollCtx, nil, "")

			continue
		}

		if err != nil {
			// AS ITSELF, AHEAD OF THE CANCELLATION, which stopping would report in
			// its place during a drain.
			if sessionFatal(err) {
				return fmt.Errorf("server: poll %s: %w", l.tier, err)
			}

			if cancelledWhileServing(ctx, draining, err) {
				continue
			}

			if l.drainEnded(pollCtx, draining, err) {
				continue
			}

			// A POLL FAILURE IS THIS TIER'S OWN TROUBLE WITH GITHUB, answered by
			// replacing the session without stopping any other tier, unless there
			// is nothing to reopen through or this process is no longer the
			// controller, which must act on nothing.
			if l.reopen == nil {
				return stopping(ctx, fmt.Errorf("server: poll %s: %w", l.tier, err))
			}

			if l.fenced() {
				return l.fencedRecovery(err)
			}

			if stop, rerr := l.recoverOrStop(ctx, pollCtx, draining, err); stop {
				return rerr
			}

			continue
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

		// Kept: what this message's offers and assignments can consume, and no
		// more — the advertisement backs nothing (see the empty-exchange branch).
		if !draining && !quiesced {
			l.releaseIdleEscrowAbove(pollCtx, l.messageCapacityTarget(msg))
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

				if poison.held {
					// Ending the session preserves redelivery of held assignments.
					return stopping(ctx, poison)
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
			l.releaseIdleEscrowAbove(pollCtx, l.targetCapacity())
		}
		l.reportCapacity(pollCtx, nil, "")
	}
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
