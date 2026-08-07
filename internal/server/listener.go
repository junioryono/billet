// Package server is billet's control plane: the per-tier scale-set listeners
// and the scheduler that turns assigned jobs into launched instances.
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

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

	// held is the capacity this listener has escrowed and is therefore entitled
	// to advertise. It is the whole of the safety property: the number sent to
	// GitHub is len(held), never a number computed from headroom.
	held []*alloc.Lease

	lastMessageID int64

	// maxCapacity, when set, caps what this listener advertises. nil means the
	// escrow decides, which is the ordinary case.
	maxCapacity *int

	// observed is the last statistics GitHub reported. TotalAssignedJobs is the
	// documented scaling signal — counting messages is not, because a response
	// carries at most 50 and a large backlog is truncated.
	observed *Statistics
}

// NewListener builds a listener for one tier.
func NewListener(a *alloc.Allocator, tier string, session Session, opts ...Option) *Listener {
	l := &Listener{
		alloc:   a,
		tier:    tier,
		session: session,
		log:     slog.Default(),
	}

	for _, opt := range opts {
		opt(l)
	}

	return l
}

// Option configures a Listener.
type Option func(*Listener)

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
		stopCtx := context.WithoutCancel(ctx)

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

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Heartbeat BEFORE topping up, so the count advertised below reflects what
		// this listener still actually owns.
		l.heartbeatHeld(ctx)

		if err := l.refillEscrow(ctx); err != nil {
			return stopping(ctx, err)
		}

		msg, err := l.session.GetMessage(ctx, l.lastMessageID, len(l.held))

		// A timed-out long poll is the ordinary case. Poll again immediately —
		// the escrow is KEPT, because releasing and retaking it every 50 seconds
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
	if len(l.held) == 0 {
		return
	}

	kept := l.held[:0]

	for _, lease := range l.held {
		if err := l.alloc.Heartbeat(ctx, lease.ID, lease.Epoch); err != nil {
			if ctx.Err() != nil {
				// Shutting down. Keep it: the release path is about to hand it
				// back, and dropping it here would leak it instead.
				kept = append(kept, lease)

				continue
			}

			l.log.Warn("lost an escrowed lease; no longer advertising it",
				"tier", l.tier, "lease", lease.ID, "error", err)

			continue
		}

		kept = append(kept, lease)
	}

	l.held = kept
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

	if l.maxCapacity != nil {
		// Capped BEFORE the escrow, not after. Escrowing capacity this listener
		// has promised not to advertise would hold it away from every other tier
		// for nothing.
		if room > *l.maxCapacity {
			room = *l.maxCapacity
		}
	}

	if room <= 0 {
		return nil
	}

	leases, err := l.alloc.Escrow(ctx, l.tier, room)
	if err != nil {
		return fmt.Errorf("server: escrow for %s: %w", l.tier, err)
	}

	l.held = append(l.held, leases...)

	return nil
}

// handle processes one message and acknowledges it.
func (l *Listener) handle(ctx context.Context, msg *Message) error {
	// Recorded BEFORE the work, so a crash mid-handling redelivers rather than
	// skips. Everything derived from a message must be idempotent for the same
	// reason: an unacknowledged message comes back.
	l.lastMessageID = msg.MessageID

	// AVAILABLE is what gets acquired. Available is the offer; Assigned is the
	// confirmation that an offer was claimed. Acquiring from Assigned asks GitHub
	// to claim work it has already handed over, and drops every offer.
	if len(msg.Available) > 0 {
		if _, err := l.session.AcquireJobs(ctx, requestIDs(msg.Available)); err != nil {
			return fmt.Errorf("server: acquire jobs for %s: %w", l.tier, err)
		}
	}

	// ASSIGNED is what consumes escrow. The lease is bound here because this is
	// the point at which the work is definitely billet's to run.
	for _, job := range msg.Assigned {
		if err := l.assign(ctx, job); err != nil {
			return err
		}
	}

	if msg.Statistics != nil {
		l.observed = msg.Statistics
	}

	if err := l.session.DeleteMessage(ctx, msg.MessageID); err != nil {
		return fmt.Errorf("server: acknowledge message %d: %w", msg.MessageID, err)
	}

	return nil
}

// requestIDs pulls the ids AcquireJobs claims work by.
func requestIDs(jobs []Job) []int64 {
	ids := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.RequestID)
	}

	return ids
}

// assign moves one escrowed lease to the job GitHub gave it.
func (l *Listener) assign(ctx context.Context, job Job) error {
	if len(l.held) == 0 {
		// GitHub assigned more than was advertised. That is a protocol violation
		// rather than a race billet can absorb: admitting it would put work on a
		// host with no capacity set aside for it, which is the whole failure the
		// escrow exists to prevent.
		return fmt.Errorf("server: %s was assigned request %d with no escrowed capacity",
			l.tier, job.RequestID)
	}

	lease := l.held[0]

	if err := l.alloc.Assign(ctx, lease.ID, lease.Epoch, job.RunID, job.RequestID); err != nil {
		return fmt.Errorf("server: assign lease %s: %w", lease.ID, err)
	}

	// Dropped from held only AFTER the assignment is durable. Shortening the
	// slice first meant a failed Assign left the lease open in the database and
	// absent from the release list — capacity that nothing hands back and nothing
	// reports, until the reaper's TTL expires it.
	l.held = l.held[1:]

	return nil
}

// releaseAll hands back capacity that was escrowed and never used.
//
// Given a context that outlives cancellation, because the ordinary reason for
// getting here is that the context was cancelled — and escrowed capacity that is
// never released is capacity no tier can use until the reaper expires it.
func (l *Listener) releaseAll(ctx context.Context) {
	for _, lease := range l.held {
		if err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			l.log.Warn("could not release escrowed capacity",
				"tier", l.tier, "lease", lease.ID, "error", err)
		}
	}

	l.held = nil
}
