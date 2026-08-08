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
// it runs out of cores, and a host's macOS guest limit is not expressible in
// either.
package alloc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
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

// requiresPlacement reports whether a phase presumes a host is running the
// instance. Entering one without a bound, still-legal placement means something
// launched work the allocator never authorised.
func requiresPlacement(p Phase) bool {
	return p == PhaseLaunching || p == PhaseOnline || p == PhaseBusy
}

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
	// ErrWrongNode means a bind would place a lease on a node other than the one
	// it is pinned to, or rebind one that is already placed.
	ErrWrongNode = errors.New("alloc: lease cannot be bound to that node")
	// ErrConflict means a retry contradicts what was already recorded — the same
	// lease assigned to a different job, or released with a different outcome.
	ErrConflict = errors.New("alloc: retry contradicts the recorded operation")
	// ErrGuestOSNotAllowed means the host does not permit the lease's guest OS.
	// Distinct from ErrWrongNode: the lease is not pinned anywhere, the chosen
	// host simply may not run that kind of guest.
	ErrGuestOSNotAllowed = errors.New("alloc: node does not permit that guest OS")
	// ErrWrongProvider means the node runs a different compute backend than the
	// lease requires — a Firecracker lease cannot run on a Tart host.
	ErrWrongProvider = errors.New("alloc: node runs a different provider")
	// ErrNotPlaced means a lease reached a phase that presumes a host without
	// ever being bound to one.
	ErrNotPlaced = errors.New("alloc: lease has no bound node")
	// ErrNotPlaceable means a lease carries too little recorded placement
	// information to verify a host is legal for it — a row predating the
	// columns the checks read. It fails closed rather than skipping the checks.
	ErrNotPlaceable = errors.New("alloc: lease cannot be placed safely")
)

// Limits is the global ceiling the allocator escrows against.
type Limits struct {
	MaxVCPU   int
	MaxMemory config.ByteSize

	// Nodes is per-host policy keyed by node name. Build it with
	// config.Config.NodePolicies so the runtime checks and the load-time guard
	// read the same rules rather than two copies that can drift.
	//
	// A node absent from the map is unconstrained in guest OS and falls back to
	// config.DefaultMacOSVMLimit. An unconfigured Apple host is still bound by
	// Apple's licence, so the absent case must be the licence rather than
	// "unlimited" — a mistyped node name then costs a scheduling constraint, not
	// a licence violation.
	Nodes map[string]config.NodePolicy
}

// Lease is a capacity reservation. The Epoch is the fencing token: every write
// must present it, and a reclaim bumps it so the previous holder's writes stop
// matching.
type Lease struct {
	ID   string
	Tier string
	// Node is the node that actually bound this lease; empty until Bind.
	Node string
	// TargetNode is the node the lease is CONSTRAINED to by its tier's config.
	// Recorded at reserve time so placement survives a catalog change.
	TargetNode string
	// MacOSSlot records whether this lease consumes one of its host's macOS
	// guest licences. Stored rather than re-derived for the same reason.
	MacOSSlot bool
	// GuestOS is what this lease boots, recorded at reserve time so a tier
	// redefined underneath an in-flight lease cannot reclassify it. Bind checks
	// it against the target host's allowlist.
	GuestOS config.GuestOS
	// Provider is the backend this lease needs, recorded for the same reason.
	// Bind compares it against the node's REGISTERED provider: a Firecracker
	// lease cannot run on a Tart host.
	// Provider is the backend the lease is ACTUALLY on, empty until it is bound.
	//
	// It used to be set at reserve time, which quietly made a tier's backend a
	// property of the reservation and so pinned every lease to one host kind
	// before anything knew where it would run. It is chosen at Bind now, from
	// Providers.
	Provider config.ProviderKind

	// Providers is what the lease MAY run on, most preferred first, copied from
	// the tier when the lease was reserved.
	//
	// Copied rather than looked up. The tier's configuration can change while a
	// lease is open — an operator edits the file and restarts — and a placement
	// decision has to be answerable from the lease itself, the same reason the
	// single provider was recorded before it.
	Providers []config.ProviderKind
	Phase     Phase
	VCPU      int
	Memory    config.ByteSize
	Epoch     int64
	RunID     int64
	RequestID int64
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

// LeaseTTL reports how long a lease survives without a heartbeat.
//
// Exported because a holder has to renew FASTER than this, and a holder that
// derives its cadence from DefaultLeaseTTL instead is correct only when the
// default is in use — a configured shorter TTL then expires every lease it
// holds, silently, while it waits for a beat that comes too late.
func (a *Allocator) LeaseTTL() time.Duration { return a.leaseTTL }

// WithLeaseTTL sets how long a lease survives without a heartbeat.
func WithLeaseTTL(d time.Duration) Option {
	return func(a *Allocator) { a.leaseTTL = d }
}

// DefaultLeaseTTL is deliberately generous relative to the heartbeat interval.
// Reclaiming a lease whose holder is merely slow is worse than holding capacity
// a little longer: it hands a live job's slot to someone else.
const DefaultLeaseTTL = 90 * time.Second

// reapBatchSize bounds one Reap transaction.
//
// Reap holds the store's single writer connection for the whole batch, so an
// unbounded scan of a large expired backlog would block every reservation and
// heartbeat behind it — turning a backlog into more expiries, which is a
// feedback loop rather than a slowdown. Callers reap on a timer; a batch that
// fills is simply drained by the next tick.
const reapBatchSize = 256

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

	// Deep-copied rather than aliased: Limits is a value type, but the map, the
	// GuestOS slices and the MacOSVMLimit pointers inside it are all shared with
	// the caller. Copying only the map would still let a caller widen a host's
	// allowlist or raise its cap after construction, moving a licence limit out
	// from under leases already counted against it. NodePolicy.Clone owns that
	// knowledge so it lives in one place.
	perNode := make(map[string]config.NodePolicy, len(limits.Nodes))

	for node, p := range limits.Nodes {
		// The SAME rules config applies, not a second hand-written copy that can
		// drift into disagreeing about which hosts are legal. This covers the
		// raw fields rather than the effective limit — a negative
		// macos_vm_limit slipped past a check reading MacOSLimit(), which
		// normalizes it to zero whenever the allowlist excludes macOS.
		// The map KEY is how every lookup finds this policy, so a key that is not
		// the canonical node name silently detaches the policy from its host: the
		// tier's node is normalized, the key is not, the lookup misses, and an
		// explicit macos_vm_limit of 0 is replaced by Apple's default of 2. The
		// policy appears to be enforced and is not.
		//
		// Two checks compose to prevent that, and there is deliberately no third.
		// An explicit "the key has no surrounding whitespace" test used to sit
		// here and could never fire in either order: Validate rejects a padded
		// NAME outright (the label pattern is anchored, so the padding is part of
		// what must match), and a key that differs from a valid name is caught
		// below. Two mutation runs were what established that — the case written
		// to cover it stayed green with the check deleted.
		if errs := p.Validate(fmt.Sprintf("alloc: node %q", node)); len(errs) > 0 {
			return nil, errors.Join(errs...)
		}

		if p.Name != node {
			return nil, fmt.Errorf("alloc: node key %q does not match its policy name %q", node, p.Name)
		}

		perNode[node] = p.Clone()
	}

	limits.Nodes = perNode

	a := &Allocator{
		db:       db,
		limits:   limits,
		tiers:    make(map[string]config.Tier, len(tiers)),
		leaseTTL: DefaultLeaseTTL,
		now:      time.Now,
	}

	// Validate every precondition the allocator's arithmetic and limits depend
	// on. config.Load enforces most of these, but this constructor is exported
	// and cannot prove its catalog came through that path — and the failure modes
	// are bad: VCPU or Memory of zero is a division by zero in headroom, a macOS
	// tier with no node skips the licence cap entirely, a negative MaxConcurrent
	// reads as unlimited, and a duplicate label silently shadows a tier.
	for i := range tiers {
		t := &tiers[i]

		switch {
		case t.Label == "":
			return nil, fmt.Errorf("alloc: tiers[%d] has no label", i)
		case t.VCPU <= 0:
			return nil, fmt.Errorf("alloc: tier %q has vcpu %d; headroom divides by it", t.Label, t.VCPU)
		case t.Memory <= 0:
			return nil, fmt.Errorf("alloc: tier %q has memory %s; headroom divides by it", t.Label, t.Memory)
		case t.MaxConcurrent < 0:
			return nil, fmt.Errorf("alloc: tier %q has negative max_concurrent %d", t.Label, t.MaxConcurrent)

		case t.Reserved < 0:
			return nil, fmt.Errorf("alloc: tier %q has negative reserved %d", t.Label, t.Reserved)

		case t.MaxConcurrent > 0 && t.Reserved > t.MaxConcurrent:
			// A floor above the ceiling is unsatisfiable by construction: the
			// reservation holds capacity back from every other tier, and this tier
			// can never use it.
			return nil, fmt.Errorf(
				"alloc: tier %q reserves %d but caps itself at %d; the reservation could never "+
					"be filled and would strand capacity",
				t.Label, t.Reserved, t.MaxConcurrent)
		// THE SAME RULES config.Load APPLIES, because this constructor is
		// documented as unable to assume its catalogue came through Load — a
		// caller can build tiers in code, and this one was accepting tiers that
		// package refuses. The macOS case is not a tidiness issue: placement only
		// tests list membership, so a `[tart, ec2]` macOS tier would have bound a
		// macOS lease to an EC2 node, which is the Apple-hardware invariant gone.
		case len(t.ProviderErrors(fmt.Sprintf("tier %q", t.Label))) > 0:
			return nil, fmt.Errorf("alloc: %w",
				errors.Join(t.ProviderErrors(fmt.Sprintf("tier %q", t.Label))...))

		case len(t.GuestOSProviderErrors(fmt.Sprintf("tier %q", t.Label))) > 0:
			return nil, fmt.Errorf("alloc: %w",
				errors.Join(t.GuestOSProviderErrors(fmt.Sprintf("tier %q", t.Label))...))
		case !t.GuestOS.Valid():
			// Same reasoning from the other side: an unknown guest OS matches no
			// allowlist, so it either strands the lease or reads as a value some
			// host happens to permit.
			return nil, fmt.Errorf(
				"alloc: tier %q has guest_os %q, which is not a known guest OS", t.Label, t.GuestOS)
		case t.GuestOS == config.GuestMacOS && strings.TrimSpace(t.Node) == "":
			return nil, fmt.Errorf(
				"alloc: macOS tier %q names no node; Apple's per-host guest limit cannot be enforced without one",
				t.Label)
		}

		if _, dup := a.tiers[t.Label]; dup {
			return nil, fmt.Errorf("alloc: duplicate tier label %q", t.Label)
		}

		// A pin is validated, not silently normalized away. Trimming
		// unconditionally turned `node: "   "` into an unpinned tier here — the
		// same silent loss of a placement constraint that config.Load was fixed
		// to reject, still reachable through this constructor. And a name like
		// "mac mini" matches no node that could ever register, so the lease would
		// escrow capacity and never bind.
		normalized := *t

		// DETACHED from the caller's slice. `*t` is a shallow copy, so the
		// provider list stayed aliased to whatever the caller still holds — and a
		// mutation after validation would change what future leases record with
		// nothing re-checking it. That is the snapshot invariant undone from the
		// inside, by the one package that depends on it most.
		normalized.Providers = slices.Clone(t.Providers)

		if t.Node != "" {
			normalized.Node = strings.TrimSpace(t.Node)

			if normalized.Node == "" {
				return nil, fmt.Errorf(
					"alloc: tier %q pins node %q, which is blank; a pin that normalizes away silently "+
						"unpins the tier", t.Label, t.Node)
			}

			if err := config.ValidateNodeName(fmt.Sprintf("alloc: tier %q", t.Label), normalized.Node); err != nil {
				return nil, err
			}
		}

		a.tiers[t.Label] = normalized
	}

	// THE FLOORS MUST FIT, together.
	//
	// Individually legal reservations can sum past the machine, and the failure
	// is invisible where it happens: every tier deducts every other tier's unmet
	// floor, so if the floors exceed the budget then EVERY tier computes zero
	// headroom and the whole deployment quietly advertises nothing. A control
	// plane that accepts no work while reporting no error is the worst outcome
	// this package can produce.
	//
	// CHECKED BY DIVISION, NEVER BY MULTIPLYING FIRST. `reserved * vcpu` is
	// unchecked integer arithmetic on a value that comes from a config file, so a
	// large enough reservation WRAPS NEGATIVE — and a negative total passes a
	// "does it fit" test comfortably. Every tier would then subtract a negative
	// unmet floor, which ADDS to its headroom, and Reserve would hand out
	// capacity far past the ceiling this package exists to enforce.
	//
	// Comparing against the remaining budget divided by the tier's size asks the
	// same question without ever forming the product, and the subtraction
	// afterwards is safe precisely because the comparison has just bounded it.
	if err := checkFloorsFit(a.tiers, limits); err != nil {
		return nil, err
	}

	for _, opt := range opts {
		opt(a)
	}

	// Options are validated AFTER they are applied: a zero TTL creates leases
	// that are already expired, so Reap recycles live capacity immediately, and a
	// nil clock panics on first use rather than at construction.
	if a.leaseTTL <= 0 {
		return nil, fmt.Errorf("alloc: lease TTL must be positive, got %s", a.leaseTTL)
	}

	if a.now == nil {
		return nil, errors.New("alloc: clock must not be nil")
	}

	return a, nil
}

// Headroom reports how many more instances of a tier would fit right now.
//
// DIAGNOSTIC ONLY — never advertise this number to GitHub. It reserves nothing,
// so two tier listeners can each read four free slots and each advertise four,
// and the atomicity of Reserve cannot retract a promise GitHub has already
// received. Advertise what Escrow returns, which is capacity actually set aside.
func (a *Allocator) Headroom(ctx context.Context, tier string) (int, error) {
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

// Escrow reserves up to want instances of a tier and returns the leases it
// actually took. len(result) is what a scale-set listener may advertise.
//
// Reading headroom and then advertising it are two steps with a gap between
// them, and the gap is where two listeners promise the same slots. Escrow closes
// it by making the promise and the reservation one act: whatever comes back is
// already held, so a listener asking immediately afterwards sees a smaller
// machine. Taking fewer than requested is the ordinary case, not an error.
func (a *Allocator) Escrow(ctx context.Context, tier string, want int) ([]*Lease, error) {
	if want < 0 {
		return nil, fmt.Errorf("alloc: want must not be negative, got %d", want)
	}

	t, ok := a.tiers[tier]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownTier, tier)
	}

	if want == 0 {
		return nil, nil
	}

	var leases []*Lease

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		room, err := a.headroom(ctx, tx, t)
		if err != nil {
			return err
		}

		take := min(want, room)
		leases = make([]*Lease, 0, take)

		for range take {
			lease, err := a.insertLease(ctx, tx, t)
			if err != nil {
				return err
			}

			leases = append(leases, lease)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return leases, nil
}

// encodeProviders renders a preference list for the ledger.
//
// Comma-separated rather than JSON: a provider kind is a short identifier from a
// closed set that cannot contain a comma, the value is read back by exactly one
// function, and a text column keeps the row legible to anyone opening the
// database to work out why a job would not place.
func encodeProviders(ps []config.ProviderKind) string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, string(p))
	}

	return strings.Join(out, ",")
}

// decodeProviders reads a preference list back, preserving order.
//
// FAILS CLOSED on anything it does not fully understand, returning nil so the
// caller's empty-list check refuses the placement. Dropping the bad entries and
// keeping the rest was fail-OPEN: a stored value of "bogus,docker" still
// authorized a docker node, which means a corrupted or truncated placement fact
// silently became a narrower but still-valid one. An empty element is the same
// story — ",docker," is not a list billet wrote.
func decodeProviders(s string) []config.ProviderKind {
	if s == "" {
		return nil
	}

	parts := strings.Split(s, ",")
	out := make([]config.ProviderKind, 0, len(parts))

	for _, p := range parts {
		kind := config.ProviderKind(p)
		if !kind.Valid() {
			return nil
		}

		out = append(out, kind)
	}

	return out
}

// checkPlacement reports whether a lease may run on a node, under the policy in
// force RIGHT NOW.
//
// Called from Bind and again on entry to launching, deliberately. Binding is not
// launching: a lease can be bound while still in `capacity`, so a policy that
// tightens in between would otherwise let an instance start on a host that no
// longer permits it — the check having passed at a moment that has since become
// irrelevant. Re-checking at the launch boundary is what makes the guarantee
// "this placement is legal now" rather than "was legal once".
func (a *Allocator) checkPlacement(ctx context.Context, tx *sql.Tx, lease *Lease, node string) error {
	if !a.allowsGuestOS(node, lease.GuestOS) {
		return fmt.Errorf("%w: lease %s is a %s guest and node %q does not permit that guest OS",
			ErrGuestOSNotAllowed, lease.ID, lease.GuestOS, node)
	}

	// A lease with no acceptable providers records nothing to compare against and
	// FAILS CLOSED. Tolerating it would be a bypass rather than a courtesy: such
	// a lease may still be unbound, so it is not old work already placed — it is
	// unplaced work whose backend nothing can verify, free to bind to a host
	// running anything. The same rows are the ones migration 7 cannot reliably
	// classify by guest OS either, since macos_slot only became truthful at
	// migration 5.
	if len(lease.Providers) == 0 {
		// "Release it" rather than "reap it": Reap only collects leases whose TTL
		// has expired, so while a holder keeps heartbeating it returns zero
		// forever and the advice would be unfollowable.
		// Two different situations reach here and the message used to name only
		// one. A row written before providers were recorded is genuinely old; a
		// row whose stored list billet cannot interpret — a provider from a newer
		// version, seen after a downgrade — is perfectly valid NEWER data that
		// this binary must refuse. Telling an operator their fresh lease
		// "predates provider recording" sends them looking for history that is not
		// there.
		return fmt.Errorf(
			"%w: lease %s records no provider list this version can interpret, so it cannot "+
				"be placed safely — it predates provider recording, or names a backend a newer "+
				"billet wrote; release it, or stop its holder and let it expire",
			ErrNotPlaceable, lease.ID)
	}

	// The node's REGISTERED provider, not one from config: a Firecracker lease
	// cannot run on a Tart host, and the registration is what the host itself
	// reported rather than what a catalog claims about it.
	var registered string

	switch err := tx.QueryRowContext(ctx,
		`SELECT provider FROM nodes WHERE name = ?`, node).Scan(&registered); {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: node %q is not registered", ErrWrongNode, node)
	case err != nil:
		return fmt.Errorf("alloc: read node %s: %w", node, err)
	}

	// MEMBERSHIP, not equality, and that one word is the whole of provider
	// failover. A tier that lists several backends may be placed on a host
	// running any of them, so losing the machine at home does not take the
	// `runs-on` label down with it.
	//
	// The list is ORDERED, and the order is not consulted here on purpose: this
	// function answers "may this lease run on the node that asked", and today the
	// node is always the one binding itself. Preference is a choice among
	// candidates, and choosing needs a chooser — which arrives with the node
	// running in its own process.
	if !slices.Contains(lease.Providers, config.ProviderKind(registered)) {
		return fmt.Errorf("%w: lease %s accepts %v but node %q runs %q",
			ErrWrongProvider, lease.ID, lease.Providers, node, registered)
	}

	return nil
}

// macOSLimit is the cap on concurrent macOS guests for a host. See Limits.Nodes
// for why an unlisted node gets Apple's default rather than no limit at all.
func (a *Allocator) macOSLimit(node string) int {
	if p, ok := a.limits.Nodes[node]; ok {
		return p.MacOSLimit()
	}

	return config.DefaultMacOSVMLimit
}

// allowsGuestOS reports whether a host may run a guest OS. An undeclared host is
// unconstrained, which is what a deployment that never wrote a nodes section
// relies on.
func (a *Allocator) allowsGuestOS(node string, os config.GuestOS) bool {
	p, ok := a.limits.Nodes[node]
	if !ok {
		return true
	}

	return p.AllowsGuestOS(os)
}

// headroom computes how many more of a tier fit. Every limit is applied, and the
// smallest wins — capacity is a vector, so "enough cores" says nothing about
// memory or about the per-host macOS guest limit.
func (a *Allocator) headroom(ctx context.Context, tx *sql.Tx, t config.Tier) (int, error) {
	used, err := a.usage(ctx, tx)
	if err != nil {
		return 0, err
	}

	// WHAT OTHER TIERS ARE OWED IS NOT AVAILABLE HERE.
	//
	// Headroom used to be the whole of what was left, which let one tier hold the
	// entire budget: the others then advertised zero, their jobs queued at GitHub
	// indefinitely, and nothing in billet was behaving incorrectly. A tier with
	// small instances wins that race simply by fitting more often.
	//
	// Only UNMET floors are deducted. A tier already holding its reservation
	// competes for the remainder on equal terms, so capacity is never idled
	// waiting for work that has not arrived.
	owedVCPU, owedMemory, err := a.unmetFloors(ctx, tx, t.Label)
	if err != nil {
		return 0, err
	}

	byVCPU := (a.limits.MaxVCPU - used.VCPU - owedVCPU) / t.VCPU
	byMemory := int((a.limits.MaxMemory - used.Memory - owedMemory) / t.Memory)

	n := min(byVCPU, byMemory)

	if t.MaxConcurrent > 0 {
		tierUsed, err := a.countOpenByTier(ctx, tx, t.Label)
		if err != nil {
			return 0, err
		}

		n = min(n, t.MaxConcurrent-tierUsed)
	}

	// A host caps concurrent macOS guests, counting every running one regardless
	// of which tier asked for it. Two individually legal macOS tiers on one Mac
	// still share one machine, so the limit is enforced per NODE across tiers
	// rather than per tier.
	if t.GuestOS == config.GuestMacOS && t.Node != "" {
		hostUsed, err := a.countOpenMacOSByNode(ctx, tx, t.Node)
		if err != nil {
			return 0, err
		}

		n = min(n, a.macOSLimit(t.Node)-hostUsed)
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

	var lease *Lease

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
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

		lease, err = a.insertLease(ctx, tx, t)

		return err
	})
	if err != nil {
		return nil, err
	}

	return lease, nil
}

// insertLease writes one escrowed lease. Callers must already hold a transaction
// in which they have confirmed headroom.
func (a *Allocator) insertLease(ctx context.Context, tx *sql.Tx, t config.Tier) (*Lease, error) {
	id, err := newLeaseID()
	if err != nil {
		return nil, err
	}

	now := a.now().UTC()

	// `node` stays NULL until Bind, while `target_node` records the constraint.
	// They answer different questions: a reservation is CONSTRAINED to a node by
	// its tier's config, and only later BOUND to one. `node` keeps its foreign
	// key because binding proves the node registered; `target_node` cannot have
	// one, because at reserve time it may name a host that has not started yet.
	//
	// macos_slot is stored rather than re-derived, so renaming a tier, changing
	// its guest_os, or restarting against a different catalog cannot silently
	// reclassify leases that are already in flight.
	var targetNode any
	if t.Node != "" {
		targetNode = t.Node
	}

	macSlot := 0
	if t.GuestOS == config.GuestMacOS {
		macSlot = 1
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO leases
		   (id, tier, node, target_node, macos_slot, guest_os, providers, phase, vcpu, memory, epoch,
		    created_at, heartbeat_at, expires_at)
		 VALUES (?, ?, NULL, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?)`,
		id, t.Label, targetNode, macSlot, string(t.GuestOS), encodeProviders(t.AcceptableProviders()),
		string(PhaseCapacity), t.VCPU, int64(t.Memory),
		ts(now), ts(now), ts(now.Add(a.leaseTTL))); err != nil {
		return nil, fmt.Errorf("alloc: insert lease: %w", err)
	}

	return &Lease{
		ID:         id,
		Tier:       t.Label,
		TargetNode: t.Node,
		MacOSSlot:  macSlot == 1,
		GuestOS:    t.GuestOS,
		Providers:  t.AcceptableProviders(),
		Phase:      PhaseCapacity,
		VCPU:       t.VCPU,
		Memory:     t.Memory,
		Epoch:      0,
	}, nil
}

// Assign binds a reserved lease to a GitHub job.
//
// Retrying with the SAME job is idempotent. Retrying with a DIFFERENT job is
// ErrConflict, not success: an escrowed slot holds one job, and quietly
// returning nil while keeping the original assignment would leave the caller
// believing a job is scheduled that nothing will ever run.
func (a *Allocator) Assign(ctx context.Context, leaseID string, epoch, runID, requestID int64) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		if lease.Phase == PhaseAssigned {
			if lease.RunID != runID || lease.RequestID != requestID {
				return fmt.Errorf("%w: lease %s already holds run %d job %d, cannot reassign to run %d job %d",
					ErrConflict, leaseID, lease.RunID, lease.RequestID, runID, requestID)
			}

			return nil
		}

		if !lease.Phase.canMoveTo(PhaseAssigned) {
			return fmt.Errorf("%w: %s -> %s", ErrBadTransition, lease.Phase, PhaseAssigned)
		}

		now := a.now().UTC()

		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = ?, run_id = ?, request_id = ?, heartbeat_at = ?, expires_at = ?
			  WHERE id = ? AND epoch = ?`,
			string(PhaseAssigned), runID, requestID, ts(now), ts(now.Add(a.leaseTTL)), leaseID, epoch); err != nil {
			return fmt.Errorf("alloc: assign lease %s: %w", leaseID, err)
		}

		// Record the queue entry now, so job_history carries a real assignment
		// time rather than one fabricated at terminalization.
		return a.recordAssignment(ctx, tx, lease, runID, requestID, now)
	})
}

// recordAssignment opens the history row at assignment time.
func (a *Allocator) recordAssignment(ctx context.Context, tx *sql.Tx, l *Lease, runID, requestID int64, now time.Time) error {
	var node any
	if l.Node != "" {
		node = l.Node
	}

	_, err := tx.ExecContext(ctx,
		`INSERT INTO job_history (lease_id, tier, node, run_id, request_id, queued_at, assigned_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (lease_id) DO UPDATE SET
		   run_id = excluded.run_id, request_id = excluded.request_id, assigned_at = excluded.assigned_at`,
		l.ID, l.Tier, node, runID, requestID, ts(now), ts(now))
	if err != nil {
		return fmt.Errorf("alloc: record assignment for %s: %w", l.ID, err)
	}

	return nil
}

// Advance moves a lease to the next phase, refusing anything the state machine
// does not allow.
func (a *Allocator) Advance(ctx context.Context, leaseID string, epoch int64, to Phase) error {
	return a.transition(ctx, leaseID, epoch, to, nil)
}

// Bind records which node is running a lease.
//
// A lease pinned to a node may only bind to THAT node. Without the check, a
// macOS lease pinned to one Mac could be bound to another while its licence slot
// stayed charged to the first — and the second host would then accept guests
// beyond Apple's limit with every individual decision looking correct.
//
// Rebinding to a different node is refused rather than silently overwritten;
// repeating the same bind is idempotent, because a node retrying after a lost
// response must not be told its own success was a conflict.
func (a *Allocator) Bind(ctx context.Context, leaseID string, epoch int64, node string) error {
	if node == "" {
		return errors.New("alloc: node must not be empty")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		if lease.TargetNode != "" && lease.TargetNode != node {
			return fmt.Errorf("%w: lease %s is pinned to node %q, cannot bind to %q",
				ErrWrongNode, leaseID, lease.TargetNode, node)
		}

		// Answered BEFORE the allowlist, because a repeat changes nothing. If the
		// host's policy tightened after this lease was placed, refusing the retry
		// un-binds nothing — the guest is running either way — and turns a
		// harmless no-op into a hard error that a node retrying past a transient
		// failure would read as "tear this job down". The policy gates NEW
		// placements; an existing one is the reaper's problem, not Bind's.
		if lease.Node == node {
			return nil // idempotent repeat
		}

		if lease.Node != "" {
			return fmt.Errorf("%w: lease %s is already bound to node %q, cannot rebind to %q",
				ErrWrongNode, leaseID, lease.Node, node)
		}

		// A FIRST binding for a lease already in a running phase means its node
		// went missing rather than that it was never placed, and adopting it onto
		// a new host would create a second owner of one slot.
		//
		// `leases.node` is ON DELETE SET NULL, so deleting a node silently blanks
		// the column for every lease it was running. The lease then LOOKS unbound
		// while the original host is still executing the job and still holds a
		// valid epoch, so its heartbeats keep succeeding — bind it elsewhere and
		// two hosts own the same lease. Reap cannot resolve that while either one
		// keeps heartbeating.
		//
		// This refusal narrows the route; it does NOT close it, and the difference
		// matters before node removal is built. A lease still in `assigned` when
		// its node is deleted is not yet running, so it binds to a new host
		// perfectly legally — and the stale original still holds the same epoch,
		// so its own Advance is authorized against whatever node the row now
		// names rather than against the caller. Ownership is recorded, not
		// proven: nothing here identifies WHO is asking.
		//
		// Closing that needs fencing at deletion, or a durable holder identity
		// checked on every authorization. Both belong with node lifecycle, which
		// does not exist yet — there is deliberately no delete path in this
		// package, so the residual hole is not reachable through this binary.
		if requiresPlacement(lease.Phase) {
			return fmt.Errorf(
				"%w: lease %s is already %s; a first binding now would make node %q a second owner",
				ErrWrongNode, leaseID, lease.Phase, node)
		}

		// The host's guest-OS allowlist is enforced HERE because this is the
		// first point at which the host is known. A lease with no target_node
		// names no host at reserve time, so config validation cannot rule out a
		// placement it never sees — a scheduler that simply picked a node with
		// free capacity would otherwise put a Linux guest on a macOS-only Mac.
		//
		// The guest OS comes from the lease's own column, not the live catalog,
		// so a tier redefined underneath an in-flight lease cannot reclassify it.
		if err := a.checkPlacement(ctx, tx, lease, node); err != nil {
			return err
		}

		// THE CHOSEN BACKEND IS RECORDED HERE, and only here.
		//
		// It used to be written at reserve time from the tier, which made a
		// backend a property of the reservation and pinned the lease before
		// anything knew where it would run. What a lease MAY run on is decided
		// when it is reserved; what it IS running on is only knowable once a host
		// has taken it. Keeping them apart is what lets one label span two kinds
		// of machine.
		//
		// Read back from the node's registration rather than assumed from the
		// list, so the column says what is true rather than what was preferred.
		var registered string

		if err := tx.QueryRowContext(ctx,
			`SELECT provider FROM nodes WHERE name = ?`, node).Scan(&registered); err != nil {
			return fmt.Errorf("alloc: read the provider of node %s: %w", node, err)
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET node = ?, chosen_provider = ? WHERE id = ? AND epoch = ?`,
			node, registered, leaseID, epoch); err != nil {
			return fmt.Errorf("alloc: bind lease %s: %w", leaseID, err)
		}

		return nil
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
		// loadAny, not load: an idempotency decision needs to SEE the terminal row.
		// Treating every not-found as success meant releasing an id that never
		// existed returned nil, and re-releasing a `done` lease as `failed` also
		// returned nil while history kept saying `done`.
		lease, err := a.loadAny(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		if lease.Phase.Terminal() {
			if lease.Phase != outcome {
				return fmt.Errorf("%w: lease %s already finished as %s, cannot re-finish as %s",
					ErrConflict, leaseID, lease.Phase, outcome)
			}

			return nil // idempotent repeat of the same outcome
		}

		if _, err := tx.ExecContext(ctx,
			`UPDATE leases SET phase = ? WHERE id = ? AND epoch = ?`,
			string(outcome), leaseID, epoch); err != nil {
			return fmt.Errorf("alloc: release lease %s: %w", leaseID, err)
		}

		return a.archive(ctx, tx, lease, outcome)
	})
}

// RegisterNode records a host and what it runs, so leases can be placed on it.
//
// A node exists in this table because it TOLD billet it exists, not because
// somebody wrote it in a config file. That is the whole reason placement
// compares a lease against the REGISTERED provider rather than a catalog entry:
// a host that says it runs Firecracker is the authority on that, and a catalog
// claiming otherwise is the thing that should lose.
//
// Upsert, because a host re-registers every time it starts. The epoch is bumped
// on re-registration so a previous instance of the same host — a process that
// was killed and came back, or one that hung and returned — finds its writes
// refused rather than operating on leases the new instance now owns.
func (a *Allocator) RegisterNode(ctx context.Context, name string, kind config.ProviderKind) error {
	if name == "" {
		return errors.New("alloc: a node must have a name")
	}

	if kind == "" {
		return fmt.Errorf("alloc: node %s registered no provider, so nothing could be placed "+
			"on it safely", name)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		now := ts(a.now().UTC())

		// A HOST MAY NOT CHANGE ITS BACKEND WHILE IT IS RUNNING WORK.
		//
		// Re-registration used to overwrite the provider freely, which quietly
		// falsified every lease already bound there: each one recorded the backend
		// it chose at bind, and after the change the ledger said a job was running
		// on firecracker while the host called itself docker. Later checks read
		// the NODE's row, so they went on authorizing the lease — the fact that
		// had become wrong was the one nothing re-read.
		//
		// Rewriting chosen_provider instead would be worse: it would relabel
		// compute that is already running, making the record agree with the
		// catalogue by lying about the past.
		//
		// So this is refused, and the operator's route is the honest one — drain
		// the host, then re-register it. An unbound node changes freely.
		var current string

		switch err := tx.QueryRowContext(ctx,
			`SELECT provider FROM nodes WHERE name = ?`, name).Scan(&current); {
		case errors.Is(err, sql.ErrNoRows):
			// First registration; nothing to contradict.
		case err != nil:
			return fmt.Errorf("alloc: read node %s: %w", name, err)

		case current != string(kind):
			var bound int

			if err := tx.QueryRowContext(ctx,
				`SELECT COUNT(*) FROM leases WHERE node = ? AND phase NOT IN ('done','failed')`,
				name).Scan(&bound); err != nil {
				return fmt.Errorf("alloc: count the leases on node %s: %w", name, err)
			}

			if bound > 0 {
				// THE WAY OUT IS SPELLED OUT, because this fires during startup —
				// cmd registers the node before it recovers anything — so an
				// operator who changes node.provider with work still bound finds
				// billet refusing to start, at the worst possible moment. "Drain the
				// host" would not be actionable advice: there is no drain command,
				// and billet is not running to accept one.
				return fmt.Errorf(
					"%w: node %s is registered as %q and now reports %q, but %d lease(s) are "+
						"still bound to it and recorded the old backend. Put node.provider back "+
						"to %q and start billet; once those jobs finish (or their leases expire "+
						"and are reaped) the host is free to change",
					ErrWrongProvider, name, current, kind, bound, current)
			}
		}

		if _, err := tx.ExecContext(ctx,
			`INSERT INTO nodes (name, provider, last_seen_at)
			 VALUES (?, ?, ?)
			 ON CONFLICT (name) DO UPDATE SET
			   provider     = excluded.provider,
			   last_seen_at = excluded.last_seen_at,
			   epoch        = nodes.epoch + 1`,
			name, string(kind), now); err != nil {
			return fmt.Errorf("alloc: register node %s: %w", name, err)
		}

		return nil
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
		// Reset inside the transaction: a retry, or a rollback, must not leave a
		// count from a previous attempt. Reap previously reported leases it had
		// reclaimed even when the transaction rolled back and reclaimed none.
		reaped = 0

		now := a.now().UTC()

		expired, err := readExpiredLeases(ctx, tx, ts(now), reapBatchSize)
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

	if err != nil {
		// The transaction rolled back, so nothing was reclaimed. Reporting a
		// nonzero count alongside an error invites a caller to believe progress
		// was made and stop retrying.
		return 0, err
	}

	return reaped, nil
}

// readExpiredLeases is its own function so `defer rows.Close()` is usable: the
// caller runs inside a transaction and issues further statements, so the cursor
// must be closed before it continues.
func readExpiredLeases(ctx context.Context, tx *sql.Tx, cutoff string, limit int) ([]Lease, error) {
	// run_id and request_id are selected because archive needs them: without them a
	// reaped lease lands in job_history with NULL attribution, so the very jobs
	// worth investigating are the ones that lose their identity.
	//
	// Ordered and LIMITed so one transaction cannot hold the single writer
	// connection while it drains an arbitrarily large backlog, blocking every
	// reservation and heartbeat behind it.
	rows, err := tx.QueryContext(ctx,
		`SELECT id, tier, node, target_node, macos_slot, phase, vcpu, memory, epoch, run_id, request_id
		   FROM leases
		  WHERE phase NOT IN ('done','failed') AND expires_at <= ?
		  ORDER BY expires_at
		  LIMIT ?`, cutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("alloc: find expired leases: %w", err)
	}
	defer rows.Close()

	var expired []Lease

	for rows.Next() {
		var (
			l          Lease
			node       sql.NullString
			targetNode sql.NullString
			macSlot    int
			mem        int64
			ph         string
			runID      sql.NullInt64
			requestID  sql.NullInt64
		)

		if err := rows.Scan(&l.ID, &l.Tier, &node, &targetNode, &macSlot, &ph,
			&l.VCPU, &mem, &l.Epoch, &runID, &requestID); err != nil {
			return nil, fmt.Errorf("alloc: scan expired lease: %w", err)
		}

		l.Node, l.TargetNode = node.String, targetNode.String
		l.MacOSSlot, l.Phase, l.Memory = macSlot == 1, Phase(ph), config.ByteSize(mem)
		l.RunID, l.RequestID = runID.Int64, requestID.Int64
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

		// State-machine legality first, so an illegal transition reports itself as
		// one. capacity -> online is refused whatever the placement is, and
		// answering it with "bind it to a node first" sends the caller off to do
		// something that would not help.
		//
		// Skipped when the phase is unchanged, because a repeat is not a move and
		// validTransitions deliberately lists no self-edges.
		if lease.Phase != to && !lease.Phase.canMoveTo(to) {
			return fmt.Errorf("%w: %s -> %s", ErrBadTransition, lease.Phase, to)
		}

		// Placement is validated BEFORE the idempotent return, unlike Bind.
		//
		// The two look symmetrical and are not. A repeated Bind records nothing
		// new, so answering it with nil is genuinely a no-op. A repeated
		// Advance(launching) is read by its caller as "you may launch" — that
		// nil is an AUTHORIZATION, not an acknowledgement. Returning it without
		// checking lets a recovery loop re-ask about a row already sitting in
		// launching and get permission for a placement that is no longer legal,
		// or was never verifiable.
		//
		// Every phase from launching onwards presumes a host is running the
		// instance, so each requires a bound node — not just the launching edge,
		// which would leave a row written by an older binary with
		// phase='launching' and node=NULL free to walk on to online.
		if requiresPlacement(to) {
			if lease.Node == "" {
				// The advice depends on the phase the lease is in NOW, not on the
				// one it is trying to reach.
				//
				// From `assigned` this is the ordinary "you forgot to bind"
				// case, and Bind still accepts it. From a phase that already
				// requires placement the lease is an orphan — its node row went
				// away — and Bind refuses to adopt it, so recommending a bind
				// would send the operator into a second refusal. Only ending the
				// lease works there, and saying so for BOTH cases would tell
				// someone to destroy work that just needed binding.
				fix := "bind it to a node first"
				if requiresPlacement(lease.Phase) {
					fix = "release it, or stop its holder and let it expire"
				}

				return fmt.Errorf("%w: lease %s is %s with no bound node; %s",
					ErrNotPlaced, leaseID, lease.Phase, fix)
			}

			// Re-checked against CURRENT policy rather than trusted from bind
			// time. Binding is not launching: a lease may be bound while still in
			// `capacity`, and a policy tightened in between would otherwise let
			// the instance start on a host that no longer permits it.
			if err := a.checkPlacement(ctx, tx, lease, lease.Node); err != nil {
				return err
			}
		}

		if lease.Phase == to {
			// Idempotent: a retried transition is not an error.
			return nil
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
	l, err := a.loadAny(ctx, tx, leaseID, epoch)
	if err != nil {
		return nil, err
	}

	if l.Phase.Terminal() {
		return nil, fmt.Errorf("%w: %s is already %s", ErrLeaseNotFound, leaseID, l.Phase)
	}

	return l, nil
}

// loadAny is load without the terminal-phase filter, for callers deciding
// idempotency — they must be able to see that a lease already finished, and how.
func (a *Allocator) loadAny(ctx context.Context, tx *sql.Tx, leaseID string, epoch int64) (*Lease, error) {
	var (
		l          Lease
		node       sql.NullString
		targetNode sql.NullString
		macSlot    int
		guestOS    string
		providers  string
		chosen     string
		mem        int64
		ph         string
		runID      sql.NullInt64
		requestID  sql.NullInt64
		curEpoch   int64
	)

	err := tx.QueryRowContext(ctx,
		`SELECT id, tier, node, target_node, macos_slot, guest_os, providers, chosen_provider,
		        phase, vcpu, memory, epoch, run_id, request_id
		   FROM leases WHERE id = ?`, leaseID).
		Scan(&l.ID, &l.Tier, &node, &targetNode, &macSlot, &guestOS, &providers, &chosen,
			&ph, &l.VCPU, &mem, &curEpoch, &runID, &requestID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s", ErrLeaseNotFound, leaseID)
	case err != nil:
		return nil, fmt.Errorf("alloc: load lease %s: %w", leaseID, err)
	}

	// The fence is checked before anything else is believed about the row.
	if curEpoch != epoch {
		return nil, fmt.Errorf("%w: lease %s is at epoch %d, caller holds %d",
			ErrFenced, leaseID, curEpoch, epoch)
	}

	l.Node, l.TargetNode = node.String, targetNode.String
	l.MacOSSlot, l.Phase, l.Memory = macSlot == 1, Phase(ph), config.ByteSize(mem)
	l.GuestOS = config.GuestOS(guestOS)
	l.Providers, l.Provider = decodeProviders(providers), config.ProviderKind(chosen)
	l.Epoch, l.RunID, l.RequestID = curEpoch, runID.Int64, requestID.Int64

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

// Lease reads one lease by id, whatever epoch it is at.
//
// No epoch argument, deliberately, and it is the only reader shaped that way.
// Every other path holds a lease it was handed and passes the epoch back as a
// fence. This one exists for a caller that has just found ORPHANED compute and
// knows only the id encoded in its name — it has no epoch to present, because
// the process that held one is gone. Reading without a fence is safe here
// precisely because nothing is being mutated on the strength of it; the caller
// takes the epoch from the row and presents it to Release, which fences
// normally. A lease that moved in between fails there, which is correct.
//
// Returns ErrLeaseNotFound for a lease that is absent OR already terminal, so a
// caller cleaning up cannot mistake "already finished" for "still holding
// capacity".
func (a *Allocator) Lease(ctx context.Context, leaseID string) (*Lease, error) {
	var out *Lease

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		var epoch int64

		err := tx.QueryRowContext(ctx, `SELECT epoch FROM leases WHERE id = ?`, leaseID).Scan(&epoch)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrLeaseNotFound, leaseID)
		case err != nil:
			return fmt.Errorf("alloc: read lease %s: %w", leaseID, err)
		}

		l, err := a.loadAny(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		if l.Phase.Terminal() {
			return fmt.Errorf("%w: %s is already %s", ErrLeaseNotFound, leaseID, l.Phase)
		}

		out = l

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// LaunchedLeaseIDs reports the leases on a node that could legitimately have
// compute running for them.
//
// Reconciliation's other half. A node can enumerate the compute it is running,
// but an instance alone does not say whether it is still WANTED — that is a fact
// about the lease, and it lives here. Anything running whose id is absent from
// this set is an orphan.
//
// Scoped to one node deliberately. A node must never reason about instances it
// does not own, and a set containing every node's leases would let a bug on one
// host spare an orphan on another.
//
// LAUNCHING, ONLINE and BUSY — not merely "not terminal". A lease in the
// capacity or assigned phase has nothing running for it by construction, because
// the launch path commits Bind and Advance(launching) before it asks a provider
// to create anything. Including those phases would spare an instance that no
// phase authorises, which is precisely the orphan the caller is hunting.
//
// I first wrote this as "not terminal" and justified the wider predicate with a
// race that does not exist: the caller lists instances BEFORE calling this, so
// anything it is judging was already created, and anything already created
// already has a lease at launching or beyond.
func (a *Allocator) LaunchedLeaseIDs(ctx context.Context, node string) (map[string]bool, error) {
	open := make(map[string]bool)

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM leases WHERE node = ? AND phase IN (?,?,?)`,
			node, PhaseLaunching, PhaseOnline, PhaseBusy)
		if err != nil {
			return fmt.Errorf("alloc: list open leases on %s: %w", node, err)
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var id string

			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("alloc: scan an open lease on %s: %w", node, err)
			}

			open[id] = true
		}

		return rows.Err()
	})
	if err != nil {
		return nil, err
	}

	return open, nil
}

// checkFloorsFit reports whether every tier's reservation can be honoured at
// once, without ever multiplying a config-supplied count by a size.
//
// Deterministic order, so the tier named in the error is stable across runs. A
// map iteration would blame a different tier each time for the same
// misconfiguration, which is a miserable thing to debug.
func checkFloorsFit(tiers map[string]config.Tier, limits Limits) error {
	labels := make([]string, 0, len(tiers))
	for label := range tiers {
		labels = append(labels, label)
	}

	sort.Strings(labels)

	remainingVCPU := limits.MaxVCPU
	remainingMemory := limits.MaxMemory

	for _, label := range labels {
		t := tiers[label]

		if t.Reserved == 0 {
			continue
		}

		if t.Reserved > remainingVCPU/t.VCPU {
			return fmt.Errorf(
				"alloc: tier %q reserves %d instances of %d vCPU, which does not fit the %d vCPU "+
					"left after the other tiers' reservations; every tier would compute zero "+
					"headroom and billet would advertise nothing",
				label, t.Reserved, t.VCPU, remainingVCPU)
		}

		if config.ByteSize(t.Reserved) > remainingMemory/t.Memory {
			return fmt.Errorf(
				"alloc: tier %q reserves %d instances of %s, which does not fit the %s "+
					"left after the other tiers' reservations; every tier would compute zero "+
					"headroom and billet would advertise nothing",
				label, t.Reserved, t.Memory, remainingMemory)
		}

		// Safe now: the comparisons above proved each product fits in what is
		// left, so neither subtraction can wrap or go negative.
		remainingVCPU -= t.Reserved * t.VCPU
		remainingMemory -= config.ByteSize(t.Reserved) * t.Memory
	}

	return nil
}

// unmetFloors reports the capacity other tiers are guaranteed and do not yet
// hold, which the caller may not take.
//
// One grouped query rather than one per tier: a loop of counts would be a
// database call per catalogue entry inside the transaction that every
// reservation waits on.
//
// A tier is "owed" only the part of its floor it is missing. Deducting the whole
// floor would idle capacity a tier has already claimed, and deducting nothing
// once a floor is met is what keeps a reservation a guarantee rather than a
// quota — above the floor, everyone competes.
func (a *Allocator) unmetFloors(
	ctx context.Context, tx *sql.Tx, forTier string,
) (int, config.ByteSize, error) {
	held, err := a.countOpenPerTier(ctx, tx)
	if err != nil {
		return 0, 0, err
	}

	var (
		vcpu   int
		memory config.ByteSize
	)

	for label := range a.tiers {
		// A tier never holds capacity back from itself: its own floor is not an
		// obstacle to filling it.
		if label == forTier {
			continue
		}

		t := a.tiers[label]
		if t.Reserved == 0 {
			continue
		}

		missing := t.Reserved - held[label]
		if missing <= 0 {
			continue
		}

		vcpu += missing * t.VCPU
		memory += config.ByteSize(missing) * t.Memory
	}

	return vcpu, memory, nil
}

// countOpenPerTier reports how many non-terminal leases each tier holds.
func (a *Allocator) countOpenPerTier(ctx context.Context, tx *sql.Tx) (map[string]int, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT tier, COUNT(*) FROM leases WHERE phase NOT IN ('done','failed') GROUP BY tier`)
	if err != nil {
		return nil, fmt.Errorf("alloc: count open leases per tier: %w", err)
	}

	defer func() { _ = rows.Close() }()

	held := make(map[string]int, len(a.tiers))

	for rows.Next() {
		var (
			label string
			n     int
		)

		if err := rows.Scan(&label, &n); err != nil {
			return nil, fmt.Errorf("alloc: scan per-tier lease counts: %w", err)
		}

		held[label] = n
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("alloc: read per-tier lease counts: %w", err)
	}

	return held, nil
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
// It reads the DURABLE columns written at reserve time, not the live tier map.
// Deriving placement from the catalog meant renaming a tier, flipping its
// guest_os, or restarting against a different config silently reclassified
// leases already in flight — and a lease bound to a different node than its tier
// named would be charged to the wrong host entirely.
func (a *Allocator) countOpenMacOSByNode(ctx context.Context, tx *sql.Tx, node string) (int, error) {
	var n int

	// COALESCE(node, target_node): once a lease is bound, the node it actually
	// landed on is the one whose licence it consumes.
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM leases
		  WHERE phase NOT IN ('done','failed')
		    AND macos_slot = 1
		    AND COALESCE(node, target_node) = ?`, node).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("alloc: count macOS guests for %s: %w", node, err)
	}

	return n, nil
}

// HistoryOutcomesForRequest reports how every archived lease for one job request
// was recorded, newest last.
//
// A job can have more than one lease across a restart — GitHub redelivers an
// unacknowledged assignment, and the listener escrows a fresh lease for it — so
// "what happened to request N" is a list rather than a value. That plurality is
// the point: it is how a caller distinguishes a redelivery that was correctly
// refused from one that was silently dropped.
func (a *Allocator) HistoryOutcomesForRequest(ctx context.Context, requestID int64) ([]string, error) {
	var outcomes []string

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx,
			// NOT NULL, because a row is inserted at ASSIGNMENT with no conclusion
			// and only filled in when the lease terminalizes. A job in flight is not
			// an outcome, and scanning one into a string fails outright.
			`SELECT conclusion FROM job_history
			  WHERE request_id = ? AND conclusion IS NOT NULL
			  ORDER BY finished_at`, requestID)
		if err != nil {
			return fmt.Errorf("alloc: read job history for request %d: %w", requestID, err)
		}

		defer func() { _ = rows.Close() }()

		for rows.Next() {
			var conclusion string

			if err := rows.Scan(&conclusion); err != nil {
				return fmt.Errorf("alloc: scan job history for request %d: %w", requestID, err)
			}

			outcomes = append(outcomes, conclusion)
		}

		return rows.Err()
	})
	if err != nil {
		return nil, err
	}

	return outcomes, nil
}

// HistoryOutcome reports how a finished lease was recorded.
//
// The only DURABLE statement about what happened to a job, which is what makes
// it the right thing for a test to assert against. An in-memory field that feeds
// the archive is an input, not a record: a test reading it stays green even if
// the write hardcodes the wrong value.
func (a *Allocator) HistoryOutcome(ctx context.Context, leaseID string) (string, error) {
	var conclusion string

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx,
			`SELECT conclusion FROM job_history WHERE lease_id = ?`, leaseID).Scan(&conclusion)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s has not been archived", ErrLeaseNotFound, leaseID)
		case err != nil:
			return fmt.Errorf("alloc: read job history for %s: %w", leaseID, err)
		}

		return nil
	})
	if err != nil {
		return "", err
	}

	return conclusion, nil
}

// archive copies a finished lease into job_history before its row stops being
// interesting, so "why did this queue" is answerable after the fact.
func (a *Allocator) archive(ctx context.Context, tx *sql.Tx, l *Lease, outcome Phase) error {
	now := a.now().UTC()

	var node any
	if l.Node != "" {
		node = l.Node
	}

	// COALESCE on update, so terminalizing never erases what assignment recorded.
	// Reap in particular used to arrive with NULL ids because it did not select
	// them, overwriting real attribution on the very leases worth investigating.
	_, err := tx.ExecContext(ctx,
		`INSERT INTO job_history (lease_id, tier, node, run_id, request_id, conclusion, queued_at, finished_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (lease_id) DO UPDATE SET
		   conclusion  = excluded.conclusion,
		   finished_at = excluded.finished_at,
		   node        = COALESCE(excluded.node, job_history.node),
		   run_id      = COALESCE(excluded.run_id, job_history.run_id),
		   request_id      = COALESCE(excluded.request_id, job_history.request_id)`,
		l.ID, l.Tier, node, nullableID(l.RunID), nullableID(l.RequestID), string(outcome), ts(now), ts(now))
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
