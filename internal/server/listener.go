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
	// finished remembers request ids this listener has already completed, so a
	// redelivered message cannot consume a second lease for a job that is over.
	//
	// Needed because billet acknowledges a message AFTER handling it, unlike the
	// vendor's own listener, which deletes the message first (listener.go:210) and
	// so handles everything at most once. Acking last is the safer choice for
	// capacity — a crash mid-handling redelivers rather than silently drops a job
	// — but it is only safe if everything derived from a message is idempotent,
	// and `running` alone is not: complete() removes the entry, so a redelivery of
	// the same batch no longer recognises the request.
	//
	// In memory and bounded, deliberately. Making it durable would mean deciding
	// that GitHub never re-offers a request id it once reported completed, and
	// nothing in the vendor's types or docs says that — assigned-then-cancelled is
	// a documented lifecycle. A permanent tombstone built on an assumption billet
	// cannot check is the same mistake as the runner-group allowlist. This covers
	// the redelivery window, which is what the defect actually is, and a restart
	// loses it exactly as it loses `running`.
	finished     map[int64]struct{}
	finishedRing []int64

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
		alloc:    a,
		tier:     tier,
		session:  session,
		log:      slog.Default(),
		running:  make(map[int64]*alloc.Lease),
		finished: make(map[int64]struct{}),
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
	// Heartbeats run on their OWN clock, not between polls.
	//
	// A long poll is nominally 50 seconds against a 90 second lease TTL, which
	// looks like enough margin and is not: the vendor's HTTP client allows a
	// request to take minutes once slow responses and retries are counted. A poll
	// that outlives the TTL leaves its leases unrenewed, the reaper terminalises
	// them, another tier escrows the capacity — and the poll then returns an
	// assignment backed by a lease that is no longer this listener's.
	//
	// Tying renewal to the poll cadence made the safety of the whole escrow
	// depend on a timeout billet does not control.
	beat, stopBeating := context.WithCancel(ctx)
	defer stopBeating()

	go l.heartbeatLoop(beat)

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

// capacity is what this listener advertises: TOTAL escrowed, not free.
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

	return len(l.held) + len(l.running)
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
			l.mu.Lock()
			l.heartbeatHeld(ctx)
			l.mu.Unlock()
		}
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

	// RUNNING leases are renewed too. They are open in the ledger exactly like
	// held ones, so a lease whose job is in flight expires just as readily — and
	// its capacity would then be escrowed by another tier while GitHub still
	// believes this scale set is running the job.
	for id, lease := range l.running {
		if !l.renew(ctx, lease) {
			delete(l.running, id)
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
		// Shutting down. Keep it: the release path is about to hand it back, and
		// dropping it here would leak it instead.
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
func (l *Listener) handle(ctx context.Context, msg *Message) error {
	// Recorded BEFORE the work, so a crash mid-handling redelivers rather than
	// skips. Everything derived from a message must be idempotent for the same
	// reason: an unacknowledged message comes back.
	l.lastMessageID = msg.MessageID

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

		return l.acknowledge(ctx, msg)
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
	for _, job := range msg.Completed {
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
	//
	// Acquisition is capped at FREE escrow, which advertising is not. maxCapacity
	// is the scale set's total, and GitHub subtracts what it already has assigned
	// — but a batch can still offer more than billet can currently back, and an
	// acquisition is a promise to run the job. Claiming beyond the free escrow
	// strands whatever cannot be backed, and the assignment that follows kills the
	// listener. Advertise total; acquire only what is held.
	if offers := l.acquirable(msg.Available); len(offers) > 0 {
		if _, err := l.session.AcquireJobs(ctx, offers); err != nil {
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

	return l.acknowledge(ctx, msg)
}

// acknowledge tells GitHub the message was handled. An unacknowledged message is
// redelivered, which is why everything above it has to be idempotent.
func (l *Listener) acknowledge(ctx context.Context, msg *Message) error {
	if err := l.session.DeleteMessage(ctx, msg.MessageID); err != nil {
		return fmt.Errorf("server: acknowledge message %d: %w", msg.MessageID, err)
	}

	return nil
}

// acquirable returns the offers this listener has free escrow to back.
//
// Fewer than offered is normal and not a loss: an unacquired offer goes to
// another scale set or is re-offered, whereas an acquisition billet cannot back
// is a job that goes nowhere.
func (l *Listener) acquirable(available []Job) []int64 {
	l.mu.Lock()
	room := len(l.held)
	l.mu.Unlock()

	if room > len(available) {
		room = len(available)
	}

	if room < len(available) {
		l.log.Warn("declining offers with no escrow to back them",
			"tier", l.tier, "offered", len(available), "acquiring", room)
	}

	return requestIDs(available[:room])
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
		return nil
	}

	// Already OVER. `running` cannot answer this: complete() removes the entry, so
	// a redelivered batch — or one carrying Assigned and Completed for the same
	// request, which is what an assigned-then-cancelled job looks like — would
	// consume a fresh lease for a job that has finished, and record a second
	// history entry against it.
	if _, ok := l.finished[job.RequestID]; ok {
		return nil
	}

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

	// Moved from held to running only AFTER the assignment is durable. Shortening
	// the slice first meant a failed Assign left the lease open in the database
	// and absent from the release list — capacity that nothing hands back and
	// nothing reports, until the reaper's TTL expires it.
	l.held = l.held[1:]
	l.running[job.RequestID] = lease

	return nil
}

// complete releases the lease a finished job was running on.
//
// Idempotent, because a redelivered Completed message must not fail: a job billet
// has already released is simply not in the map. Release with PhaseDone rather
// than inspecting a conclusion — the lease's job is finished either way, and the
// outcome belongs to job history rather than to the capacity ledger.
func (l *Listener) complete(ctx context.Context, job Job) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	// Recorded BEFORE the lookup, and unconditionally.
	//
	// A completion for a request this listener has no lease for still has to be
	// remembered: the same batch may carry the Assigned for it — GitHub can assign
	// and then cancel a job that is not picked up in time — and processing
	// completions first would otherwise let that assignment consume a lease for a
	// job already known to be over.
	l.markFinished(job.RequestID)

	lease, ok := l.running[job.RequestID]
	if !ok {
		// Not ours, or already released. Both are ordinary: GitHub can report a
		// job completed that this listener never assigned, if a restart lost the
		// in-memory map while the lease lives on in the ledger for the reaper.
		return nil
	}

	if err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
		return fmt.Errorf("server: release lease %s for finished request %d: %w",
			lease.ID, job.RequestID, err)
	}

	delete(l.running, job.RequestID)

	return nil
}

// maxFinished bounds the completed-request memory.
//
// It only has to outlive a redelivery, which is one message, so this is generous
// by orders of magnitude. Bounded regardless, because the alternative is a map
// that grows for the life of the process — one entry per job this tier ever ran.
const maxFinished = 4096

// markFinished remembers a completed request id, evicting the oldest.
func (l *Listener) markFinished(requestID int64) {
	if _, ok := l.finished[requestID]; ok {
		return
	}

	l.finished[requestID] = struct{}{}
	l.finishedRing = append(l.finishedRing, requestID)

	if len(l.finishedRing) > maxFinished {
		delete(l.finished, l.finishedRing[0])
		l.finishedRing = l.finishedRing[1:]
	}
}

// releaseAll hands back capacity that was escrowed and never used.
//
// Given a context that outlives cancellation, because the ordinary reason for
// getting here is that the context was cancelled — and escrowed capacity that is
// never released is capacity no tier can use until the reaper expires it.
func (l *Listener) releaseAll(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()

	release := func(lease *alloc.Lease) {
		if err := l.alloc.Release(ctx, lease.ID, lease.Epoch, alloc.PhaseDone); err != nil {
			l.log.Warn("could not release escrowed capacity",
				"tier", l.tier, "lease", lease.ID, "error", err)
		}
	}

	for _, lease := range l.held {
		release(lease)
	}

	// RUNNING leases are released too, and that is right only while nothing can
	// actually run a job. Once the node runtime exists these must be handed to
	// it rather than released — a job in flight whose lease is freed lets another
	// tier escrow capacity the host is still using.
	for _, lease := range l.running {
		release(lease)
	}

	l.held = nil
	l.running = make(map[int64]*alloc.Lease)
}
