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
	// runners this scale set can accept right now. It returns (nil, nil) when the
	// poll times out with nothing to report, which is the ordinary case.
	GetMessage(ctx context.Context, lastMessageID int64, maxCapacity int) (*Message, error)
	// DeleteMessage acknowledges a message. An unacknowledged message is
	// redelivered, so everything derived from one must be idempotent.
	DeleteMessage(ctx context.Context, messageID int64) error
	// AcquireJobs claims assigned jobs and returns the ids actually acquired,
	// which may be fewer than asked for.
	AcquireJobs(ctx context.Context, requestIDs []int64) ([]int64, error)
	Close(ctx context.Context) error
}

// Message is one batch of scale-set news.
type Message struct {
	MessageID  int64
	Statistics *Statistics
	Assigned   []Job
	Completed  []Job
}

// Job identifies one workflow job.
type Job struct {
	RequestID int64
	RunID     int64
	JobID     int64
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
	defer l.releaseAll(context.WithoutCancel(ctx))

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		if err := l.refillEscrow(ctx); err != nil {
			return err
		}

		msg, err := l.session.GetMessage(ctx, l.lastMessageID, len(l.held))
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return err
			}

			return fmt.Errorf("server: poll %s: %w", l.tier, err)
		}

		// A timed-out long poll is the ordinary case, not an error. Poll again
		// immediately — the escrow is kept, because releasing and re-taking it
		// every 50 seconds would hand the gap to another tier and produce exactly
		// the flapping the escrow exists to avoid.
		if msg == nil {
			continue
		}

		if err := l.handle(ctx, msg); err != nil {
			return err
		}
	}
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

	if len(msg.Assigned) > 0 {
		if err := l.acquire(ctx, msg.Assigned); err != nil {
			return err
		}
	}

	if err := l.session.DeleteMessage(ctx, msg.MessageID); err != nil {
		return fmt.Errorf("server: acknowledge message %d: %w", msg.MessageID, err)
	}

	return nil
}

// acquire claims assigned jobs and binds them to escrowed leases.
func (l *Listener) acquire(ctx context.Context, jobs []Job) error {
	ids := make([]int64, 0, len(jobs))
	for _, j := range jobs {
		ids = append(ids, j.RequestID)
	}

	acquired, err := l.session.AcquireJobs(ctx, ids)
	if err != nil {
		return fmt.Errorf("server: acquire jobs for %s: %w", l.tier, err)
	}

	// FEWER than asked for is ordinary, not a failure. A job can be assigned and
	// then withdrawn — GitHub reassigns it elsewhere when it is not acquired in
	// time, and the same job may appear assigned and then completed-as-cancelled
	// up to three times.
	wanted := make(map[int64]Job, len(jobs))
	for _, j := range jobs {
		wanted[j.RequestID] = j
	}

	for _, id := range acquired {
		job, ok := wanted[id]
		if !ok {
			continue
		}

		if err := l.assign(ctx, job); err != nil {
			return err
		}
	}

	return nil
}

// assign moves one escrowed lease to the job GitHub gave it.
func (l *Listener) assign(ctx context.Context, job Job) error {
	if len(l.held) == 0 {
		// GitHub assigned more than was advertised. That is a protocol violation
		// rather than a race billet can absorb: admitting it would put work on a
		// host with no capacity set aside for it, which is the whole failure the
		// escrow exists to prevent.
		return fmt.Errorf("server: %s was assigned job %d with no escrowed capacity", l.tier, job.JobID)
	}

	lease := l.held[0]
	l.held = l.held[1:]

	if err := l.alloc.Assign(ctx, lease.ID, lease.Epoch, job.RunID, job.JobID); err != nil {
		return fmt.Errorf("server: assign lease %s: %w", lease.ID, err)
	}

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
