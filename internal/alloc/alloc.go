// Package alloc is billet's global capacity allocator.
//
// Every runner billet launches is preceded by a LEASE, and a lease exists from
// the moment capacity is escrowed — before a scale-set listener advertises that
// capacity to GitHub — not from the moment a VM boots.
//
// That ordering is the whole design. Each runner tier is its own GitHub scale
// set with its own advertised maxCapacity. If each listener computed its own
// maximum independently, GitHub could fill all of them at once and the host
// would be overcommitted with nothing anywhere to stop it. Reserving on
// assignment is already too late: by then GitHub has made a promise billet
// cannot keep. So a listener may only advertise what this package has already
// set aside.
//
// Capacity is a VECTOR — vCPU, memory, per-tier concurrency, and per-node macOS
// licence slots — never a single integer. A host runs out of memory long before
// it runs out of cores, and Apple's two-guest limit is not expressible in either.
package alloc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// Phase is a lease's position in its lifecycle. The values are constrained by a
// CHECK in the schema, so a typo cannot sit in the open-lease index forever.
type Phase string

const (
	// PhaseCapacity means capacity is escrowed and advertised, but GitHub has not
	// yet handed us a job.
	PhaseCapacity Phase = "capacity"
	// PhaseAssigned means GitHub assigned a job to this lease.
	PhaseAssigned Phase = "assigned"
	// PhaseLaunching means a node is bringing the instance up.
	PhaseLaunching Phase = "launching"
	// PhaseOnline means the runner registered with GitHub.
	PhaseOnline Phase = "online"
	// PhaseBusy means the runner is executing the job.
	PhaseBusy Phase = "busy"
	// PhaseDone and PhaseFailed are terminal and release capacity.
	PhaseDone   Phase = "done"
	PhaseFailed Phase = "failed"
)

// validTransitions is the state machine, written down rather than implied by
// scattered UPDATE statements. A transition not listed here is refused.
//
// Terminal phases have no successors on purpose: a lease that has released its
// capacity must never re-acquire it by moving backwards, which is how a
// double-admit would look from the inside.
var validTransitions = map[Phase][]Phase{
	PhaseCapacity:  {PhaseAssigned, PhaseDone, PhaseFailed},
	PhaseAssigned:  {PhaseLaunching, PhaseDone, PhaseFailed},
	PhaseLaunching: {PhaseOnline, PhaseDone, PhaseFailed},
	PhaseOnline:    {PhaseBusy, PhaseDone, PhaseFailed},
	PhaseBusy:      {PhaseDone, PhaseFailed},
	PhaseDone:      nil,
	PhaseFailed:    nil,
}

// Terminal reports whether a phase releases capacity.
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseFailed }

func (p Phase) canMoveTo(next Phase) bool {
	for _, allowed := range validTransitions[p] {
		if allowed == next {
			return true
		}
	}

	return false
}

var (
	// ErrNoCapacity means the request would exceed a limit. It is an ordinary
	// outcome — the listener advertises less — not a failure.
	ErrNoCapacity = errors.New("alloc: no capacity available")
	// ErrLeaseNotFound means the lease does not exist, or is already terminal.
	ErrLeaseNotFound = errors.New("alloc: lease not found")
	// ErrFenced means the caller's epoch is stale: this lease was reclaimed and
	// handed to someone else. The caller must stop writing entirely.
	ErrFenced = errors.New("alloc: lease was reclaimed by another holder")
	// ErrBadTransition means the requested phase change is not in the state
	// machine.
	ErrBadTransition = errors.New("alloc: invalid phase transition")
	// ErrUnknownTier means the tier is not in the configured catalog.
	ErrUnknownTier = errors.New("alloc: unknown tier")
)

// Limits is the global ceiling the allocator escrows against.
type Limits struct {
	MaxVCPU   int
	MaxMemory config.ByteSize
}

// Lease is a capacity reservation. The Epoch is the fencing token: every write
// must present it, and a reclaim bumps it so the previous holder's writes stop
// matching.
type Lease struct {
	ID     string
	Tier   string
	Node   string
	Phase  Phase
	VCPU   int
	Memory config.ByteSize
	Epoch  int64
	RunID  int64
	JobID  int64
}

// Usage is the vector of what is currently held.
type Usage struct {
	VCPU   int
	Memory config.ByteSize
	Leases int
}

// Allocator hands out and reclaims capacity. Safe for concurrent use: every
// decision is one transaction against the single-writer state store, so a
// read-decide-record sequence cannot interleave with another.
type Allocator struct {
	db     *state.DB
	limits Limits
	tiers  map[string]config.Tier

	// leaseTTL is how long a lease survives without a heartbeat. A holder that
	// stops heartbeating has crashed, been partitioned, or been stopped; either
	// way its capacity must come back or the host slowly fills with ghosts.
	leaseTTL time.Duration

	// now is injectable so expiry can be tested without sleeping.
	now func() time.Time
}

// Option configures an Allocator.
type Option func(*Allocator)

// WithClock replaces the clock. Test-only in practice.
func WithClock(now func() time.Time) Option {
	return func(a *Allocator) { a.now = now }
}

// WithLeaseTTL sets how long a lease survives without a heartbeat.
func WithLeaseTTL(d time.Duration) Option {
	return func(a *Allocator) { a.leaseTTL = d }
}

// DefaultLeaseTTL is deliberately generous relative to the heartbeat interval.
// Reclaiming a lease whose holder is merely slow is worse than holding capacity
// a little longer: it hands a live job's slot to someone else.
const DefaultLeaseTTL = 90 * time.Second

// New builds an allocator over the given tier catalog.
func New(db *state.DB, limits Limits, tiers []config.Tier, opts ...Option) (*Allocator, error) {
	if db == nil {
		return nil, errors.New("alloc: nil state database")
	}

	if limits.MaxVCPU <= 0 || limits.MaxMemory <= 0 {
		return nil, fmt.Errorf(
			"alloc: limits must be positive (got %d vCPU, %s); without a ceiling there is nothing to escrow against",
			limits.MaxVCPU, limits.MaxMemory)
	}

	a := &Allocator{
		db:       db,
		limits:   limits,
		tiers:    make(map[string]config.Tier, len(tiers)),
		leaseTTL: DefaultLeaseTTL,
		now:      time.Now,
	}

	for i := range tiers {
		t := &tiers[i]
		a.tiers[t.Label] = *t
	}

	for _, opt := range opts {
		opt(a)
	}

	return a, nil
}

// Advertisable reports how many more instances of a tier billet could accept
// right now, given everything already escrowed.
//
// This is what a scale-set listener advertises to GitHub as maxCapacity. It is
// computed from the SAME ledger that Reserve writes to, which is what stops two
// tier listeners from each promising the whole machine.
func (a *Allocator) Advertisable(ctx context.Context, tier string) (int, error) {
	t, ok := a.tiers[tier]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownTier, tier)
	}

	var n int

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		n, err = a.headroom(ctx, tx, t)

		return err
	})

	return n, err
}

// headroom computes how many more of a tier fit. Every limit is applied, and the
// smallest wins — capacity is a vector, so "enough cores" says nothing about
// memory or about Apple's per-host guest limit.
func (a *Allocator) headroom(ctx context.Context, tx *sql.Tx, t config.Tier) (int, error) {
	used, err := a.usage(ctx, tx)
	if err != nil {
		return 0, err
	}

	byVCPU := (a.limits.MaxVCPU - used.VCPU) / t.VCPU
	byMemory := int((a.limits.MaxMemory - used.Memory) / t.Memory)

	n := min(byVCPU, byMemory)

	if t.MaxConcurrent > 0 {
		tierUsed, err := a.countOpenByTier(ctx, tx, t.Label)
		if err != nil {
			return 0, err
		}

		n = min(n, t.MaxConcurrent-tierUsed)
	}

	// Apple permits at most two macOS guests per Apple-branded host, counting
	// every running one regardless of which tier asked for it. Two individually
	// legal macOS tiers on one Mac still share one machine, so the limit is
	// enforced per NODE across tiers rather than per tier.
	if t.GuestOS == config.GuestMacOS && t.Node != "" {
		hostUsed, err := a.countOpenMacOSByNode(ctx, tx, t.Node)
		if err != nil {
			return 0, err
		}

		n = min(n, config.MacOSVMLimit-hostUsed)
	}

	return max(n, 0), nil
}

// Reserve escrows capacity for one instance of a tier.
//
// Call this BEFORE advertising to GitHub. The returned lease holds the capacity
// until it is released or expires, so a second Reserve sees a smaller machine.
func (a *Allocator) Reserve(ctx context.Context, tier string) (*Lease, error) {
	t, ok := a.tiers[tier]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTier, tier)
	}

	id, err := newLeaseID()
	if err != nil {
		return nil, err
	}

	lease := &Lease{
		ID:     id,
		Tier:   t.Label,
		Node:   t.Node,
		Phase:  PhaseCapacity,
		VCPU:   t.VCPU,
		Memory: t.Memory,
		Epoch:  0,
	}

	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		// Headroom and insert in ONE transaction. Checking outside it would be a
		// read followed by a hopeful write — measured at a 7x overcommit under
		// concurrency, which is exactly the race this package exists to prevent.
		room, err := a.headroom(ctx, tx, t)
		if err != nil {
			return err
		}

		if room < 1 {
			return fmt.Errorf("%w for tier %q", ErrNoCapacity, t.Label)
		}

		now := a.now().UTC()

		// node stays NULL until Bind. A reservation is CONSTRAINED to a node by
		// its tier's configuration, not yet BOUND to one — and the column has a
		// foreign key to nodes(name), which at reserve time may name a host that
		// has not registered yet (or ever). Writing the constraint here would make
		// escrow fail on a perfectly valid configuration.
		_, err = tx.ExecContext(ctx,
			`INSERT INTO leases
			   (id, tier, node, phase, vcpu, memory, epoch, created_at, heartbeat_at, expires_at)
			 VALUES (?, ?, NULL, ?, ?, ?, 0, ?, ?, ?)`,
			lease.ID, lease.Tier, string(PhaseCapacity), lease.VCPU, int64(lease.Memory),
			ts(now), ts(now), ts(now.Add(a.leaseTTL)))

		return err
	})
	if err != nil {
		return nil, err
	}

	return lease, nil
}

// Assign binds a reserved lease to a GitHub job.
func (a *Allocator) Assign(ctx context.Context, leaseID string, epoch, runID, jobID int64) error {
	return a.transition(ctx, leaseID, epoch, PhaseAssigned, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`UPDATE leases SET run_id = ?, job_id = ? WHERE id = ?`, runID, jobID, leaseID)

		return err
	})
}

// Advance moves a lease to the next phase, refusing anything the state machine
// does not allow.
func (a *Allocator) Advance(ctx context.Context, leaseID string, epoch int64, to Phase) error {
	return a.transition(ctx, leaseID, epoch, to, nil)
}

// Bind records which node is running a lease.
func (a *Allocator) Bind(ctx context.Context, leaseID string, epoch int64, node string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := a.load(ctx, tx, leaseID, epoch); err != nil {
			return err
		}

		_, err := tx.ExecContext(ctx,
			`UPDATE leases SET node = ? WHERE id = ? AND epoch = ?`, node, leaseID, epoch)

		return err
	})
}

// Heartbeat extends a lease's expiry. A holder that stops calling this loses the
// lease to Reap.
func (a *Allocator) Heartbeat(ctx context.Context, leaseID string, epoch int64) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := a.load(ctx, tx, leaseID, epoch); err != nil {
			return err
		}

		now := a.now().UTC()

		_, err := tx.ExecContext(ctx,
			`UPDATE leases SET heartbeat_at = ?, expires_at = ? WHERE id = ? AND epoch = ?`,
			ts(now), ts(now.Add(a.leaseTTL)), leaseID, epoch)

		return err
	})
}

// Release terminalizes a lease and returns its capacity.
//
// Idempotent: releasing an already-terminal lease succeeds, because a node
// retrying after a lost response must not be told its cleanup failed.
func (a *Allocator) Release(ctx context.Context, leaseID string, epoch int64, outcome Phase) error {
	if !outcome.Terminal() {
		return fmt.Errorf("%w: %q is not terminal", ErrBadTransition, outcome)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			if errors.Is(err, ErrLeaseNotFound) {
				// Already terminal or reaped. Cleanup is complete either way.
				return nil
			}

			return err
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = ? WHERE id = ? AND epoch = ?`,
			string(outcome), leaseID, epoch); err != nil {
			return err
		}

		return a.archive(ctx, tx, lease, outcome)
	})
}

// Reap terminalizes leases whose holders stopped heartbeating, and returns how
// many it reclaimed.
//
// The epoch is bumped as part of reclaiming, so a holder that comes back — a
// paused process, a healed partition — finds its writes refused rather than
// silently operating on a lease someone else now owns.
func (a *Allocator) Reap(ctx context.Context) (int, error) {
	var reaped int

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		now := a.now().UTC()

		expired, err := readExpiredLeases(ctx, tx, ts(now))
		if err != nil {
			return err
		}

		for i := range expired {
			l := &expired[i]
			if _, err := tx.ExecContext(ctx,
				`UPDATE leases SET phase = 'failed', epoch = epoch + 1 WHERE id = ? AND epoch = ?`,
				l.ID, l.Epoch); err != nil {
				return fmt.Errorf("alloc: reap lease %s: %w", l.ID, err)
			}

			if err := a.archive(ctx, tx, l, PhaseFailed); err != nil {
				return err
			}

			reaped++
		}

		return nil
	})

	return reaped, err
}

// readExpiredLeases is its own function so `defer rows.Close()` is usable: the
// caller runs inside a transaction and issues further statements, so the cursor
// must be closed before it continues.
func readExpiredLeases(ctx context.Context, tx *sql.Tx, cutoff string) ([]Lease, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id, tier, node, phase, vcpu, memory, epoch
		   FROM leases
		  WHERE phase NOT IN ('done','failed') AND expires_at <= ?`, cutoff)
	if err != nil {
		return nil, fmt.Errorf("alloc: find expired leases: %w", err)
	}
	defer rows.Close()

	var expired []Lease

	for rows.Next() {
		var (
			l    Lease
			node sql.NullString
			mem  int64
			ph   string
		)

		if err := rows.Scan(&l.ID, &l.Tier, &node, &ph, &l.VCPU, &mem, &l.Epoch); err != nil {
			return nil, fmt.Errorf("alloc: scan expired lease: %w", err)
		}

		l.Node, l.Phase, l.Memory = node.String, Phase(ph), config.ByteSize(mem)
		expired = append(expired, l)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alloc: iterate expired leases: %w", err)
	}

	return expired, nil
}

// Usage reports what is currently held.
func (a *Allocator) Usage(ctx context.Context) (Usage, error) {
	var u Usage

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		u, err = a.usage(ctx, tx)

		return err
	})

	return u, err
}

// transition performs a fenced, state-machine-checked phase change.
func (a *Allocator) transition(ctx context.Context, leaseID string, epoch int64, to Phase, extra func(*sql.Tx) error) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		if lease.Phase == to {
			// Idempotent: a retried transition is not an error.
			return nil
		}

		if !lease.Phase.canMoveTo(to) {
			return fmt.Errorf("%w: %s -> %s", ErrBadTransition, lease.Phase, to)
		}

		now := a.now().UTC()

		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = ?, heartbeat_at = ?, expires_at = ? WHERE id = ? AND epoch = ?`,
			string(to), ts(now), ts(now.Add(a.leaseTTL)), leaseID, epoch); err != nil {
			return fmt.Errorf("alloc: advance lease %s: %w", leaseID, err)
		}

		if extra != nil {
			if err := extra(tx); err != nil {
				return err
			}
		}

		if to.Terminal() {
			return a.archive(ctx, tx, lease, to)
		}

		return nil
	})
}

// load reads a lease and enforces the fence.
//
// The epoch check is what makes a reclaim safe. Without it, a holder that was
// declared dead and replaced would keep writing to a lease another process now
// owns — an orderly takeover becoming two concurrent owners of one slot.
func (a *Allocator) load(ctx context.Context, tx *sql.Tx, leaseID string, epoch int64) (*Lease, error) {
	var (
		l       Lease
		node    sql.NullString
		mem     int64
		ph      string
		runID   sql.NullInt64
		jobID   sql.NullInt64
		curEpoc int64
	)

	err := tx.QueryRowContext(ctx,
		`SELECT id, tier, node, phase, vcpu, memory, epoch, run_id, job_id
		   FROM leases WHERE id = ?`, leaseID).
		Scan(&l.ID, &l.Tier, &node, &ph, &l.VCPU, &mem, &curEpoc, &runID, &jobID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s", ErrLeaseNotFound, leaseID)
	case err != nil:
		return nil, fmt.Errorf("alloc: load lease %s: %w", leaseID, err)
	}

	if curEpoc != epoch {
		return nil, fmt.Errorf("%w: lease %s is at epoch %d, caller holds %d",
			ErrFenced, leaseID, curEpoc, epoch)
	}

	l.Node, l.Phase, l.Memory = node.String, Phase(ph), config.ByteSize(mem)
	l.Epoch, l.RunID, l.JobID = curEpoc, runID.Int64, jobID.Int64

	if l.Phase.Terminal() {
		return nil, fmt.Errorf("%w: %s is already %s", ErrLeaseNotFound, leaseID, l.Phase)
	}

	return &l, nil
}

func (a *Allocator) usage(ctx context.Context, tx *sql.Tx) (Usage, error) {
	var (
		u    Usage
		vcpu sql.NullInt64
		mem  sql.NullInt64
	)

	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(vcpu), 0), COALESCE(SUM(memory), 0), COUNT(*)
		   FROM leases WHERE phase NOT IN ('done','failed')`).
		Scan(&vcpu, &mem, &u.Leases)
	if err != nil {
		return u, fmt.Errorf("alloc: read usage: %w", err)
	}

	u.VCPU = int(vcpu.Int64)
	u.Memory = config.ByteSize(mem.Int64)

	return u, nil
}

func (a *Allocator) countOpenByTier(ctx context.Context, tx *sql.Tx, tier string) (int, error) {
	var n int

	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM leases WHERE tier = ? AND phase NOT IN ('done','failed')`, tier).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("alloc: count tier %s: %w", tier, err)
	}

	return n, nil
}

// countOpenMacOSByNode counts macOS guests destined for a node across ALL tiers,
// because Apple's limit belongs to the physical Mac rather than to any one tier.
//
// It counts by TIER SET rather than by leases.node, and that is deliberate: a
// reservation's node column is NULL until a node binds it, so a node-keyed count
// would see zero and let the third guest through during exactly the window the
// limit exists to cover. Config already guarantees every macOS tier declares its
// node, so the set of tiers pinned to a host IS the set of guests headed there.
func (a *Allocator) countOpenMacOSByNode(ctx context.Context, tx *sql.Tx, node string) (int, error) {
	macTiers := make([]any, 0, len(a.tiers))

	for label := range a.tiers {
		t := a.tiers[label]
		if t.GuestOS == config.GuestMacOS && t.Node == node {
			macTiers = append(macTiers, label)
		}
	}

	if len(macTiers) == 0 {
		return 0, nil
	}

	query := `SELECT COUNT(*) FROM leases WHERE phase NOT IN ('done','failed') AND tier IN (?` +
		repeatPlaceholders(len(macTiers)-1) + `)`

	var n int
	if err := tx.QueryRowContext(ctx, query, macTiers...).Scan(&n); err != nil {
		return 0, fmt.Errorf("alloc: count macOS guests for %s: %w", node, err)
	}

	return n, nil
}

// archive copies a finished lease into job_history before its row stops being
// interesting, so "why did this queue" is answerable after the fact.
func (a *Allocator) archive(ctx context.Context, tx *sql.Tx, l *Lease, outcome Phase) error {
	now := a.now().UTC()

	var node any
	if l.Node != "" {
		node = l.Node
	}

	_, err := tx.ExecContext(ctx,
		`INSERT INTO job_history (lease_id, tier, node, run_id, job_id, conclusion, queued_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (lease_id) DO UPDATE SET conclusion = excluded.conclusion, finished_at = excluded.finished_at`,
		l.ID, l.Tier, node, nullableID(l.RunID), nullableID(l.JobID), string(outcome), ts(now), ts(now))
	if err != nil {
		return fmt.Errorf("alloc: archive lease %s: %w", l.ID, err)
	}

	return nil
}

func nullableID(v int64) any {
	if v == 0 {
		return nil
	}

	return v
}

func repeatPlaceholders(n int) string {
	out := make([]byte, 0, n*2)
	for range n {
		out = append(out, ',', '?')
	}

	return string(out)
}

// timestampFormat is FIXED-WIDTH, and that is the entire point.
//
// The obvious choice, time.RFC3339Nano, uses ".999999999" — which STRIPS
// trailing zeros. A timestamp with zero nanoseconds therefore renders with no
// fraction at all ("...30Z"), and expiry is compared as a SQL string, where 'Z'
// (0x5A) sorts after '.' (0x2E). So "12:00:30Z" > "12:00:30.5Z", and an expired
// lease is silently NOT reaped when now falls in the same second with nonzero
// nanoseconds — its capacity stays held for up to a second longer than the TTL
// says, with nothing anywhere reporting it.
//
// ".000000000" always emits nine digits, so lexical order matches chronological
// order for every value this produces. Anything comparing these columns as
// strings depends on that; do not swap it back for a stdlib constant.
const timestampFormat = "2006-01-02T15:04:05.000000000Z07:00"

// ts renders a timestamp so string comparison matches chronological order.
func ts(t time.Time) string { return t.UTC().Format(timestampFormat) }

func newLeaseID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("alloc: generate lease id: %w", err)
	}

	return hex.EncodeToString(buf), nil
}
