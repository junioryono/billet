// Package alloc is billet's global capacity allocator.
//
// Every runner is preceded by a LEASE, and a lease exists from the moment capacity
// is escrowed — before a listener advertises it to GitHub — not from the moment a VM
// boots. That ordering is the design: each tier is its own scale set with its own
// advertised maxCapacity, so listeners computing their own would let GitHub fill all
// of them at once, and reserving on assignment is already too late.
//
// Capacity is a VECTOR — vCPU, memory, per-tier concurrency, per-node macOS licence
// slots — never a single integer.
package alloc

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/state/ledgerdb"
	"github.com/junioryono/billet/internal/version"
)

// querier is state.Querier under a local name.
//
// Aliased so a read-only helper can name it without each file importing the
// store package — and so Enrollments, whose own parameter is called `state`, can
// still refer to the type at all.
type querier = state.Querier

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
	// PhaseCustody means the node is preserving compute it inherited or can no
	// longer manage as an ordinary running command. The capacity stays charged.
	PhaseCustody Phase = "custody"
	// PhaseTeardown means the node asked its backend to remove compute but has not
	// confirmed it stopped. It is the operator-visible proof obligation.
	PhaseTeardown Phase = "teardown"
	// PhaseQuarantine means a lease that had compute behind it stopped being
	// heartbeated, and the compute has not been confirmed gone.
	//
	// STILL CHARGED TO ITS HOST, which is the whole point. Terminalizing an
	// expired running lease frees the capacity at once while the container keeps
	// running until the node next sweeps, so another tier can escrow that slot in
	// between and two jobs land on a machine sized for one. Capacity reclaimed
	// late is recoverable; capacity handed out twice is not.
	//
	// It leaves only on PROOF: the node destroys the container and says so, or it
	// re-registers reporting an inventory this lease is not in. An operator can
	// force it for a machine that is never coming back, which is the one case
	// proof can never arrive for.
	PhaseQuarantine Phase = "quarantine"
	// PhaseDone and PhaseFailed are terminal and release capacity.
	PhaseDone   Phase = "done"
	PhaseFailed Phase = "failed"
)

// validTransitions is the state machine, written down rather than implied by
// scattered UPDATE statements. A transition not listed here is refused.
//
// Terminal phases have no successors: a lease that has released its capacity must
// never re-acquire it by moving backwards.
var validTransitions = map[Phase][]Phase{
	PhaseCapacity:  {PhaseAssigned, PhaseDone, PhaseFailed},
	PhaseAssigned:  {PhaseLaunching, PhaseDone, PhaseFailed},
	PhaseLaunching: {PhaseOnline, PhaseCustody, PhaseTeardown, PhaseQuarantine, PhaseDone, PhaseFailed},
	PhaseOnline:    {PhaseBusy, PhaseCustody, PhaseTeardown, PhaseQuarantine, PhaseDone, PhaseFailed},
	PhaseBusy:      {PhaseCustody, PhaseTeardown, PhaseQuarantine, PhaseDone, PhaseFailed},
	PhaseCustody:   {PhaseTeardown, PhaseQuarantine, PhaseDone, PhaseFailed},
	PhaseTeardown:  {PhaseQuarantine, PhaseDone, PhaseFailed},
	// Quarantine ends only in a terminal phase: it is a lease being cleaned up,
	// never one going back to work.
	PhaseQuarantine: {PhaseDone, PhaseFailed},
	PhaseDone:       nil,
	PhaseFailed:     nil,
}

// Terminal reports whether a phase releases capacity.
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseFailed }

// requiresPlacement reports whether a phase presumes a host is running the
// instance. Entering one without a bound, still-legal placement means something
// launched work the allocator never authorised.
//
// QUARANTINE IS NOT ONE OF THEM. A quarantined lease is being cleaned up rather
// than placed, and re-checking placement on the way out would refuse the very
// release that ends it — on a host that is, by definition, in trouble.
func requiresPlacement(p Phase) bool {
	return p == PhaseLaunching || p == PhaseOnline || p == PhaseBusy
}

// hadCompute reports whether a phase means a container may exist for this lease.
//
// The line the reaper draws: escrow nobody launched can be terminalized on
// expiry, and anything past it cannot, because expiry says the holder stopped
// heartbeating and never that the compute stopped.
func hadCompute(p Phase) bool {
	return p == PhaseLaunching || p == PhaseOnline || p == PhaseBusy ||
		p == PhaseCustody || p == PhaseTeardown
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
	// ErrWrongSite means a host reported a different location while it still has
	// work bound to it there. Distinct from ErrWrongProvider because the fix is
	// different: one is a backend change, the other is a machine that moved.
	ErrWrongSite = errors.New("alloc: node reports a different site")
	// ErrNotPlaced means a lease reached a phase that presumes a host without
	// ever being bound to one.
	ErrNotPlaced = errors.New("alloc: lease has no bound node")
	// ErrNotPlaceable means a lease carries too little recorded placement
	// information to verify a host is legal for it — a row predating the
	// columns the checks read. It fails closed rather than skipping the checks.
	ErrNotPlaceable = errors.New("alloc: lease cannot be placed safely")
	// ErrForceRelease means an operator asserted that custody's compute is gone.
	// The node must drop its local proof obligation and terminalize the lease.
	ErrForceRelease = errors.New("alloc: operator requested forced release")
	// ErrFleetHeld means a remote fleet's shared capacity is already claimed — by
	// another host, or by this host's own outstanding work under a different fleet.
	//
	// A SENTINEL RATHER THAN PROSE because the node acts on it: a registration
	// refused this way must not be retried into a loop, since nothing on the refused
	// machine can resolve it — the fix is an operator giving that node its own fleet,
	// moving it to on-demand compute, or decommissioning the host that holds the
	// claim, which proves no compute remains there first.
	//
	// IT IS NOT ABOUT LIVENESS, and the first version's message said it was. A node
	// being unreachable says nothing about whether its builds are still drawing on
	// the fleet — `ForgetEveryNode` marks every host not-live whenever a control
	// plane starts — so a claim that lapsed with liveness would let a differently
	// named node advertise a fleet's whole capacity on top of live work.
	ErrFleetHeld = errors.New("alloc: that fleet's shared capacity is already claimed")
)

// Limits is the global ceiling the allocator escrows against.
type Limits struct {
	MaxVCPU   int
	MaxMemory config.ByteSize

	// Nodes is per-host policy keyed by node name. Build it with
	// config.Config.NodePolicies so the runtime checks and the load-time guard read
	// the same rules.
	//
	// A node absent from the map is unconstrained in guest OS and falls back to
	// config.DefaultMacOSVMLimit — the licence rather than "unlimited", so a mistyped
	// node name costs a scheduling constraint rather than a licence violation.
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
	// Provider is the backend the lease is ACTUALLY on, empty until it is bound.
	//
	// Chosen at Bind, from Providers. What a lease MAY run on is decided when it
	// is reserved; what it IS running on is only knowable once a host has taken
	// it.
	Provider config.ProviderKind

	// Providers is what the lease MAY run on, most preferred first, copied from the
	// tier when the lease was reserved.
	//
	// Copied rather than looked up: a tier's configuration can change while a lease is
	// open, and a placement decision has to be answerable from the lease itself.
	Providers []config.ProviderKind
	Phase     Phase
	// VCPU and Memory are what the lease is CHARGED. For an EC2 lease this is
	// the selected purchasable shape, which may be larger than the tier asked for.
	VCPU   int
	Memory config.ByteSize
	// RequestedVCPU and RequestedMemory are the tier's requirement. They stay
	// fixed while EC2 fallback may resize the charged shape around them.
	RequestedVCPU   int
	RequestedMemory config.ByteSize
	// InstanceType is the EC2 shape currently authorised for purchase. Empty for
	// backends whose charged resources are the requested resources.
	InstanceType string
	// Site is the placed host's registered site at escrow, recorded on the row
	// so the history a terminalization copies names where the job ran.
	Site string
	// PriceUSDPerHour is the charged shape's price at the moment it was
	// charged, written at escrow and again by a fallback resize. Zero for a
	// host-backed lease, which buys nothing. Never re-read from the node's
	// catalogue: a node may re-register with new prices while this lease is
	// open, and the history has to say what was bought.
	PriceUSDPerHour config.USDPerHour
	// ImageCache, CacheGeneration and ActionsCache are what the node observed
	// the cache do for this job, first observation kept. Empty means nothing
	// was observed; see CacheObservation.
	ImageCache      ImageCache
	CacheGeneration string
	ActionsCache    ActionsCache
	// PreferenceRank is the chosen target provider's position in the tier's
	// preference list. It orders unbound listener escrow so a shrink releases
	// fallback capacity before preferred capacity. Unbound capacity is not adopted
	// across a listener restart, so this runtime ordering fact is not persisted.
	PreferenceRank int
	// HeldSince is set when the lease enters custody or an unconfirmed teardown.
	HeldSince string
	// HolderIncarnation is the incarnation of the node process that last took
	// responsibility for the compute: the one that bound the lease, or the one
	// that moved it into custody or teardown. Empty means no process recorded
	// one. Compared with the node's current incarnation by reports, and by
	// nothing that decides capacity — see migration 45.
	HolderIncarnation string
	// ForceRelease asks the node holding custody to relinquish it. It is carried
	// by Heartbeat as ErrForceRelease rather than acted on behind the node's back.
	ForceRelease bool
	// FailureReason is an external fact that decided a running job cannot finish,
	// such as an EC2 Spot interruption warning. It is written before teardown so
	// recovery preserves why the lease will fail.
	FailureReason string
	// Disruption is what billet's OWN infrastructure did to this lease while its
	// job may still have been running, and DisruptedAt is when billet observed
	// it. Empty means nothing did.
	//
	// SEPARATE FROM FailureReason, and not interchangeable with it: a lease with
	// a failure reason is adopted as outcome=failed, discard=true — which
	// destroys a guest — while a disruption decides nothing at all and is only
	// ever read beside GitHub's own result for the job.
	Disruption  Disruption
	DisruptedAt string
	Epoch       int64
	RunID       int64
	RequestID   int64
}

// Usage is the vector of what is currently held.
type Usage struct {
	VCPU   int
	Memory config.ByteSize
	Leases int
}

// LeaseJob is the GitHub identity assigned to one lease.
type LeaseJob struct {
	Tier      string
	RunID     int64
	RequestID int64
}

// Allocator hands out and reclaims capacity. Safe for concurrent use: every
// decision is one transaction against the single-writer state store, so a
// read-decide-record sequence cannot interleave with another.
type Allocator struct {
	db     *state.DB
	limits Limits
	tiers  map[string]config.Tier

	// leaseTTL is how long a lease survives without a heartbeat. A holder that stops
	// heartbeating has crashed, been partitioned, or been stopped; either way its
	// capacity must come back or the host slowly fills with ghosts.
	leaseTTL time.Duration

	// placement decides which of several equally preferred hosts a reservation is
	// aimed at. Empty is treated as pack; see WithPlacement.
	placement config.PlacementPolicy

	// now is injectable so expiry can be tested without sleeping.
	now func() time.Time
}

// Option configures an Allocator.
type Option func(*Allocator)

// WithPlacement chooses how a reservation picks among equally preferred hosts.
//
// Empty means pack, which is the safer failure: spreading strands capacity in
// fragments too small for a large tier, and a job that cannot be placed is worse
// than one that shares a disk.
func WithPlacement(p config.PlacementPolicy) Option {
	return func(a *Allocator) { a.placement = p.Or() }
}

// WithClock replaces the clock. Test-only in practice.
func WithClock(now func() time.Time) Option {
	return func(a *Allocator) { a.now = now }
}

// LeaseTTL reports how long a lease survives without a heartbeat.
//
// Exported because a holder has to renew FASTER than this: one deriving its cadence
// from DefaultLeaseTTL is correct only when the default is in use, and a shorter
// configured TTL then silently expires every lease it holds.
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
// heartbeat behind it — turning a backlog into more expiries. Callers reap on a
// timer, so a batch that fills is drained by the next tick.
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

	// Deep-copied rather than aliased: Limits is a value type, but the map, the GuestOS
	// slices and the MacOSVMLimit pointers inside it are shared with the caller, so a
	// shallow copy would still let a caller raise a host's cap after construction.
	perNode := make(map[string]config.NodePolicy, len(limits.Nodes))

	for node, p := range limits.Nodes {
		// The SAME rules config applies, not a second hand-written copy that can drift.
		// It covers the raw fields rather than the effective limit, because MacOSLimit()
		// normalizes a negative macos_vm_limit to zero when the allowlist excludes macOS.
		//
		// The map KEY is how every lookup finds this policy, so a key that is not the
		// canonical node name silently detaches the policy from its host.
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

	// Validate every precondition the allocator's arithmetic depends on. config.Load
	// enforces most of these, but this constructor is exported and cannot prove its
	// catalog came through that path — and the failure modes are bad: a VCPU or Memory
	// of zero divides by zero in headroom, a macOS tier with no node skips the licence
	// cap, a negative MaxConcurrent reads as unlimited, and a duplicate label silently
	// shadows a tier.
	for i := range tiers {
		t := &tiers[i]

		// The macOS guests an unpinned tier's declared hosts permit between them;
		// zero for every other tier.
		unpinnedMacOSTotal := 0

		switch {
		case t.Label == "":
			return nil, fmt.Errorf("alloc: tiers[%d] has no label", i)

		// AN UNEXPANDED LADDER IS REFUSED BY NAME, before the vcpu check below
		// would report it as a tier of zero.
		//
		// `sizes` is a template that config.Parse turns into one tier per entry,
		// and it does that before defaults and before validation so nothing
		// downstream ever meets one. This constructor is exported and cannot prove
		// its catalogue came through Parse — the alloc.New rule — and the failure
		// without this is silent in the worst way: the tier has vcpu zero, so the
		// message would be about headroom dividing by it, and the sizes the
		// operator actually declared would be a field nothing ever read.
		case len(t.Sizes) > 0:
			return nil, fmt.Errorf("alloc: tier %q still carries a `sizes` ladder, so this "+
				"catalogue never went through config.Parse. Expand it with "+
				"config.ExpandTierSizes first; the sizes it declares would otherwise be a "+
				"field nothing reads, and this tier would be one of zero vCPU", t.Label)

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
		case len(t.InterceptionErrors(fmt.Sprintf("tier %q", t.Label))) > 0:
			return nil, fmt.Errorf("alloc: %w",
				errors.Join(t.InterceptionErrors(fmt.Sprintf("tier %q", t.Label))...))
		case len(t.PoolPolicyErrors(fmt.Sprintf("tier %q", t.Label))) > 0:
			return nil, fmt.Errorf("alloc: %w",
				errors.Join(t.PoolPolicyErrors(fmt.Sprintf("tier %q", t.Label))...))

		case len(t.GuestOSProviderErrors(fmt.Sprintf("tier %q", t.Label))) > 0:
			return nil, fmt.Errorf("alloc: %w",
				errors.Join(t.GuestOSProviderErrors(fmt.Sprintf("tier %q", t.Label))...))
		case !t.GuestOS.Valid():
			// Same reasoning from the other side: an unknown guest OS matches no
			// allowlist, so it either strands the lease or reads as a value some
			// host happens to permit.
			return nil, fmt.Errorf(
				"alloc: tier %q has guest_os %q, which is not a known guest OS", t.Label, t.GuestOS)
		case t.GuestOS == config.GuestMacOS && strings.TrimSpace(t.Node) == "" &&
			len(t.AcceptableProviders()) < 2:
			// One backend, no host named: nothing is gained by leaving it unpinned,
			// and the load-time guard reads the licence off the pin. Several backends
			// behind one label are the shape that cannot be pinned, and placement
			// counts macOS guests per host either way.
			return nil, fmt.Errorf(
				"alloc: macOS tier %q names no node; Apple's per-host guest limit cannot be enforced without one",
				t.Label)
		case t.GuestOS == config.GuestMacOS && strings.TrimSpace(t.Node) == "" && t.Reserved > 0:
			return nil, fmt.Errorf(
				"alloc: macOS tier %q names no node and reserves %d guests; a reservation is held "+
					"against one host's licence", t.Label, t.Reserved)
		case t.GuestOS == config.GuestMacOS && strings.TrimSpace(t.Node) == "":
			// THE SAME RULE config.Load APPLIES, because this constructor is exported:
			// an unpinned macOS tier is counted against the hosts its backends
			// declare, so every listed backend needs a declared policy that permits
			// macOS, and a remote one must say what its fleet holds — macOSLimit
			// would otherwise hand a fleet Apple's per-host number.
			total, err := unpinnedMacOSHosts(t, limits.Nodes)
			if err != nil {
				return nil, fmt.Errorf("alloc: macOS tier %q: %w", t.Label, err)
			}
			unpinnedMacOSTotal = total
		}

		if _, dup := a.tiers[t.Label]; dup {
			return nil, fmt.Errorf("alloc: duplicate tier label %q", t.Label)
		}

		// A pin is validated, not silently normalized away: trimming unconditionally would
		// turn `node: "   "` into an unpinned tier, and a name like "mac mini" matches no
		// node that could ever register, so the lease would escrow capacity and never
		// bind.
		normalized := *t

		// DETACHED from the caller's slice: `*t` is a shallow copy, so the provider list
		// would stay aliased to whatever the caller holds, and a mutation after validation
		// would change what future leases record with nothing re-checking it.
		normalized.Providers = slices.Clone(t.Providers)

		// The aggregate bound config.Load applies to an unpinned macOS tier: its
		// max_concurrent defaults to what the declared hosts permit between them
		// and may not exceed it, since the excess is capacity advertised to GitHub
		// that no host could ever run.
		if unpinnedMacOSTotal > 0 {
			switch {
			case normalized.MaxConcurrent == 0:
				normalized.MaxConcurrent = unpinnedMacOSTotal
			case normalized.MaxConcurrent > unpinnedMacOSTotal:
				return nil, fmt.Errorf(
					"alloc: macOS tier %q: max_concurrent must be between 1 and %d, the macOS guests "+
						"the declared hosts permit between them", t.Label, unpinnedMacOSTotal)
			}
		}

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

	// THE FLOORS MUST FIT, together. Individually legal reservations can sum past the
	// machine, and the failure is invisible: every tier deducts every other tier's unmet
	// floor, so floors exceeding the budget make EVERY tier compute zero headroom and
	// the deployment quietly advertise nothing.
	//
	// CHECKED BY DIVISION, NEVER BY MULTIPLYING FIRST. `reserved * vcpu` is unchecked
	// arithmetic on a config value, so a large enough one WRAPS NEGATIVE — and a negative
	// total passes a "does it fit" test, after which every tier subtracts a negative
	// floor, which ADDS to its headroom.
	if err := checkFloorsFit(a.tiers, limits); err != nil {
		return nil, err
	}

	// AND THE macOS DIMENSION, which vCPU and memory do not cover. A Mac caps concurrent
	// macOS guests per HOST across every tier targeting it, so two macOS tiers on one Mac
	// can be individually legal and jointly unfillable — and such a floor holds vCPU and
	// memory back while never being fillable.
	//
	// config.Load rejects this from the other direction; this constructor is exported and
	// cannot assume its catalogue came through Load.
	if err := a.checkMacOSFloors(); err != nil {
		return nil, err
	}

	for _, opt := range opts {
		opt(a)
	}

	// Options are validated AFTER they are applied: a zero TTL creates leases that are
	// already expired, and a nil clock panics on first use rather than at
	// construction.
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
// DIAGNOSTIC ONLY — never advertise this number to GitHub. It reserves nothing, so
// two listeners can each read four free slots and each advertise four, and Reserve
// cannot retract a promise GitHub has already received. Advertise what Escrow
// returns.
func (a *Allocator) Headroom(ctx context.Context, tier string) (int, error) {
	t, ok := a.tiers[tier]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownTier, tier)
	}

	var n int

	err := a.db.View(ctx, func(tx querier) error {
		var err error
		n, err = a.headroom(ctx, tx, t)

		return err
	})

	return n, err
}

// Escrow reserves up to want instances of a tier and returns the leases it actually
// took. len(result) is what a scale-set listener may advertise.
//
// Reading headroom and then advertising it are two steps with a gap between them,
// and the gap is where two listeners promise the same slots. Escrow makes the
// promise and the reservation one act. Taking fewer than requested is ordinary.
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
		// BEFORE THE HEADROOM DECISION, so a sealed deployment says it is sealed
		// rather than looking merely full. Reaching the insert is not guaranteed —
		// a fleet with no room returns early — and a listener that heard "no
		// capacity" during a drain would never learn a seal was the reason.
		//
		// Still inside the transaction: the check that MATTERS is the one beside
		// the insert, and this is the same read a moment earlier so the answer
		// cannot differ within one transaction.
		if err := refuseIfSealed(ctx, tx); err != nil {
			return err
		}

		// MEASURED ONCE AND SPENT DOWN, because none of these choices is durable until the
		// transaction commits: asking the ledger again per lease would return the same
		// fleet every time and aim every reservation at the same machine. The same
		// measurement answers how much room there is and where to put it.
		room, place, err := a.headroomWithPlacer(ctx, tx, t)
		if err != nil {
			return err
		}

		take := min(want, room)
		leases = make([]*Lease, 0, take)

		for range take {
			target, cost, ok := place.next(t)
			if !ok {
				// The fleet ran out before the ceiling did. Headroom is the smaller of the two, so
				// this should not happen — but returning what was placed is the safe reading, and
				// inserting an unplaced lease is the failure this design exists to prevent.
				break
			}

			lease, err := a.insertLease(ctx, tx, t, target, place.siteOf(target), cost,
				place.rank[target])
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
// closed set that cannot contain a comma, and a text column keeps the row legible
// to anyone opening the database to work out why a job would not place.
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
// caller's empty-list check refuses the placement. Dropping bad entries and keeping
// the rest would be fail-OPEN: "bogus,docker" would still authorize a docker node,
// so a corrupted placement fact silently becomes a narrower valid one.
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

func encodeEC2Shapes(shapes []config.EC2InstanceType) (string, error) {
	if len(shapes) == 0 {
		return "", nil
	}

	b, err := json.Marshal(shapes)
	if err != nil {
		return "", fmt.Errorf("alloc: encode EC2 shapes: %w", err)
	}

	return string(b), nil
}

// decodeRemoteShapes reads a node's stored shape catalogue and re-validates it.
//
// THE PROVIDER IS A PARAMETER BECAUSE THE DIAGNOSTIC NAMES A CONFIG KEY, and the
// key differs per backend: an ec2 node's shapes came out of
// node.ec2.instance_types and a codebuild node's out of
// node.codebuild.compute_types. It read the stored bytes and reported the ec2 key
// unconditionally, which is a message pointing at a field that is not in the
// operator's file.
//
// The column is still named ec2_shapes. Renaming a shipped on-disk column to tidy
// a name is a migration for nothing; what matters is that one validator sees every
// catalogue.
func decodeRemoteShapes(kind config.ProviderKind, s string) ([]config.RemoteShape, error) {
	if s == "" {
		return nil, nil
	}

	var shapes []config.RemoteShape
	if err := json.Unmarshal([]byte(s), &shapes); err != nil {
		return nil, fmt.Errorf("alloc: decode %s shapes: %w", kind, err)
	}

	if errs := config.CheckRemoteShapes(kind, shapes); len(errs) > 0 {
		return nil, fmt.Errorf("alloc: invalid registered %s shapes: %w", kind, errors.Join(errs...))
	}

	return shapes, nil
}

func sameEC2PlacementShapes(kind config.ProviderKind, encoded string,
	next []config.RemoteShape,
) (bool, error) {
	current, err := decodeRemoteShapes(kind, encoded)
	if err != nil {
		return false, err
	}
	if len(current) != len(next) {
		return false, nil
	}
	for i := range current {
		if current[i].Type != next[i].Type || current[i].VCPU != next[i].VCPU ||
			current[i].Memory != next[i].Memory {
			return false, nil
		}
	}

	return true, nil
}

// checkPlacement reports whether a lease may run on a node, under the policy in
// force RIGHT NOW.
//
// Called from Bind and again on entry to launching: a lease can be bound while
// still in `capacity`, so a policy that tightens in between would otherwise let an
// instance start on a host that no longer permits it.
func (a *Allocator) checkPlacement(ctx context.Context, tx querier, lease *Lease, node string) error {
	if !a.allowsGuestOS(node, lease.GuestOS) {
		return fmt.Errorf("%w: lease %s is a %s guest and node %q does not permit that guest OS",
			ErrGuestOSNotAllowed, lease.ID, lease.GuestOS, node)
	}

	// A lease with no acceptable providers records nothing to compare against and FAILS
	// CLOSED. Tolerating it would be a bypass: such a lease may still be unbound, so it
	// is unplaced work whose backend nothing can verify, free to bind to a host running
	// anything.
	if len(lease.Providers) == 0 {
		// "Release it" rather than "reap it": Reap only collects leases whose TTL
		// has expired, so while a holder keeps heartbeating it returns zero forever
		// and the advice would be unfollowable.
		//
		// The message names BOTH situations that reach here. A row written before
		// providers were recorded is genuinely old; a row whose stored list billet
		// cannot interpret — a provider from a newer version, seen after a
		// downgrade — is valid NEWER data this binary must refuse.
		return fmt.Errorf(
			"%w: lease %s records no provider list this version can interpret, so it cannot "+
				"be placed safely — it predates provider recording, or names a backend a newer "+
				"billet wrote; release it, or stop its holder and let it expire",
			ErrNotPlaceable, lease.ID)
	}

	// The node's REGISTERED provider, not one from config: the registration is what the
	// host itself reported rather than what a catalog claims about it.
	registered, err := state.ReadQueries(tx).ReadNodeProvider(ctx, node)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return fmt.Errorf("%w: node %q is not registered", ErrWrongNode, node)
	case err != nil:
		return fmt.Errorf("alloc: read node %s: %w", node, err)
	}

	// MEMBERSHIP, not equality, and that one word is the whole of provider failover: a
	// tier listing several backends may be placed on a host running any of them.
	//
	// The list is ORDERED, and the order is not consulted here: this answers "may this
	// lease run on the node that asked", while preference is a choice among candidates
	// made at escrow.
	if !slices.Contains(lease.Providers, config.ProviderKind(registered)) {
		return fmt.Errorf("%w: lease %s accepts %v but node %q runs %q",
			ErrWrongProvider, lease.ID, lease.Providers, node, registered)
	}

	return nil
}

// describeFleet renders a fleet for a diagnostic, so an empty one reads as the state it
// is rather than as an empty pair of quotes an operator cannot search their config for.
func describeFleet(arn string) string {
	if arn == "" {
		return "on-demand (no fleet)"
	}

	return strconv.Quote(arn)
}

// restoreFleetRemedy is the instruction that puts a node back on the fleet it holds.
//
// SEPARATE FROM describeFleet BECAUSE A DESCRIPTION IS NOT A VALUE. Interpolating the
// description into "put node.codebuild.fleet_arn back to %q" produced `back to
// "on-demand (no fleet)"` — an instruction to set a key to a string that is not a
// fleet ARN and that config would refuse. Going back to on-demand means REMOVING the
// key, which is a different sentence rather than a different value.
func restoreFleetRemedy(arn string) string {
	if arn == "" {
		return "Remove node.codebuild.fleet_arn"
	}

	return "Put node.codebuild.fleet_arn back to " + strconv.Quote(arn)
}

// unpinnedMacOSHosts checks that every backend an unpinned macOS tier lists has
// a declared policy with macOS capacity, and that a remote backend's policy
// states its limit rather than inheriting Apple's. It answers the guests those
// hosts permit between them, which bounds the tier's max_concurrent.
func unpinnedMacOSHosts(t *config.Tier, policies map[string]config.NodePolicy) (int, error) {
	total := 0
	for _, provider := range t.AcceptableProviders() {
		capacity := 0
		for _, p := range policies {
			if p.Provider != provider || !p.AllowsGuestOS(config.GuestMacOS) {
				continue
			}
			if p.MacOSVMLimit == nil && !provider.RunsOnHost() {
				return 0, fmt.Errorf("node %q runs %s, a managed fleet, and declares no macos_vm_limit; "+
					"Apple's per-host allowance is not that fleet's capacity", p.Name, provider)
			}
			if limit := p.MacOSLimit(); limit > 0 {
				capacity += limit
			}
		}
		if capacity == 0 {
			return 0, fmt.Errorf("no declared node runs %s with macOS capacity above zero, so the "+
				"tier's guests cannot be counted against a host", provider)
		}
		total += capacity
	}

	return total, nil
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

// headroom computes how many more of a tier fit. Every limit is applied and the
// smallest wins — capacity is a vector, so "enough cores" says nothing about memory
// or about the per-host macOS guest limit.
func (a *Allocator) headroom(ctx context.Context, tx querier, t config.Tier) (int, error) {
	n, _, err := a.headroomWithPlacer(ctx, tx, t)

	return n, err
}

// headroomWithPlacer is headroom, handing back the fleet it measured.
//
// MEASURED ONCE PER TRANSACTION. Escrow asks how much room there is and then
// asks where to put it, and building the placer twice meant walking the whole
// fleet twice on the single writer connection every listener is waiting for.
// total() only reads, so the placer that answered the first question is exactly
// the one that should answer the second — and reusing it removes any chance of
// the two disagreeing.
func (a *Allocator) headroomWithPlacer(
	ctx context.Context, tx querier, t config.Tier,
) (int, *placer, error) {
	used, err := a.usage(ctx, tx)
	if err != nil {
		return 0, nil, err
	}

	// FLOORS ARE NOT DEDUCTED HERE. They are held against the machines that could
	// keep them, in reserveFloors, which is where the promise lives and the only
	// place it can be checked honestly; deducting in both would double-count.
	//
	// Subtracting every unmet floor from the deployment ceiling cannot ask whether
	// any MACHINE could keep the floor, so a reservation on a tier with no
	// suitable host anywhere — a Tart tier on a fleet of Docker boxes — would take
	// the ceiling from tiers that are perfectly placeable.
	place, owedVCPU, owedMemory, err := a.placerWithFloors(ctx, tx, t)
	if err != nil {
		return 0, nil, err
	}

	place.deploymentVCPU = max(a.limits.MaxVCPU-used.VCPU-owedVCPU, 0)
	place.deploymentMemory = max(a.limits.MaxMemory-used.Memory-owedMemory, 0)

	n := place.total(t)

	if t.MaxConcurrent > 0 {
		tierUsed, err := a.countOpenByTier(ctx, tx, t.Label)
		if err != nil {
			return 0, nil, err
		}

		n = min(n, t.MaxConcurrent-tierUsed)
	}

	// WHAT THE MACHINES CAN ACTUALLY HOLD, which is the other half of the answer.
	// Everything above bounds the DEPLOYMENT and none of it knows whether any single
	// machine has room: with a 64 vCPU box and an 8 vCPU Mac mini under a ceiling of
	// 120, that arithmetic would escrow 120 vCPU and be unable to place most of it.
	//
	// So the answer is the SMALLER of the two. The fleet term stops billet promising
	// more than the machines can hold; the deployment term stops it exceeding what the
	// operator allowed, which is what keeps a one-box install behaving as before.
	//
	// The per-host macOS licence lives in placer.roomFor, which place.total above
	// has already applied per candidate. It USED to say headroomOn, and that was
	// false before headroomOn was deleted: nothing in production ever called it,
	// so the sentence sent a reader to a second implementation that never ran.
	return max(n, 0), place, nil
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
		// Before the headroom decision, for the reason Escrow gives.
		if err := refuseIfSealed(ctx, tx); err != nil {
			return err
		}

		// Headroom and insert in ONE transaction. Checking outside it would be a read
		// followed by a hopeful write — measured at a 7x overcommit under concurrency.
		room, place, err := a.headroomWithPlacer(ctx, tx, t)
		if err != nil {
			return err
		}

		if room < 1 {
			return fmt.Errorf("%w for tier %q", ErrNoCapacity, t.Label)
		}

		target, cost, ok := place.next(t)
		if !ok {
			return fmt.Errorf("%w for tier %q", ErrNoCapacity, t.Label)
		}

		lease, err = a.insertLease(ctx, tx, t, target, place.siteOf(target), cost,
			place.rank[target])

		return err
	})
	if err != nil {
		return nil, err
	}

	return lease, nil
}

// insertLease writes one escrowed lease. Callers must already hold a transaction
// in which they have confirmed headroom.
//
// THE ADMISSION CHECK LIVES HERE because this is the only place a lease is
// created — `Escrow` and `Reserve` are separate transactions that both end up
// here, and a check on either one alone would leave the other open. Putting it
// at the choke point also means a caller added later inherits it rather than
// having to remember it.
//
// It is INSIDE the caller's transaction, for the same reason the headroom check
// is: reading admission and then inserting hopefully is not a check, and the
// thing it fails to exclude is a seal that commits in between.
func (a *Allocator) insertLease(
	ctx context.Context, tx *sql.Tx, t config.Tier, target, site string, cost placementCost,
	preferenceRank int,
) (*Lease, error) {
	if err := refuseIfSealed(ctx, tx); err != nil {
		return nil, err
	}

	id, err := newLeaseID()
	if err != nil {
		return nil, err
	}

	now := a.now().UTC()

	// `node` stays NULL until Bind, while `target_node` records the constraint: a
	// reservation is CONSTRAINED to a node by its tier's config and only later BOUND
	// to one. `node` keeps its foreign key because binding proves the node registered;
	// `target_node` cannot, because at reserve time it may name a host that has not
	// started yet.
	//
	// macos_slot is stored rather than re-derived, so renaming a tier or restarting
	// against a different catalog cannot reclassify leases already in flight.
	//
	// EVERY LEASE NAMES ITS MACHINE. A reservation charged to the deployment and to no
	// host would leave the fleet's remaining room unchanged, so billet would advertise
	// the same slots repeatedly.
	var targetNode sql.NullString
	if target != "" {
		targetNode = sql.NullString{String: target, Valid: true}
	}

	macSlot := 0
	if t.GuestOS == config.GuestMacOS {
		macSlot = 1
	}

	if err := state.WriteQueries(tx).InsertLease(ctx, ledgerdb.InsertLeaseParams{
		ID:              id,
		Tier:            t.Label,
		TargetNode:      targetNode,
		MacosSlot:       int64(macSlot),
		GuestOs:         string(t.GuestOS),
		Providers:       encodeProviders(t.AcceptableProviders()),
		Phase:           string(PhaseCapacity),
		Vcpu:            int64(cost.vcpu),
		Memory:          int64(cost.memory),
		RequestedVcpu:   int64(t.VCPU),
		RequestedMemory: int64(t.Memory),
		InstanceType:    cost.instanceType,
		// THE PRICE IS WRITTEN WHEN THE SHAPE IS CHARGED. A node may re-register
		// with a new catalogue while this lease is open, and the history a
		// terminalization copies has to say what was bought at the time.
		Site:               site,
		PriceMicrosPerHour: int64(cost.price),
		CreatedAt:          ts(now),
		HeartbeatAt:        ts(now),
		ExpiresAt:          ts(now.Add(a.leaseTTL)),
	}); err != nil {
		return nil, fmt.Errorf("alloc: insert lease: %w", err)
	}

	return &Lease{
		ID:              id,
		Tier:            t.Label,
		TargetNode:      target,
		MacOSSlot:       macSlot == 1,
		GuestOS:         t.GuestOS,
		Providers:       t.AcceptableProviders(),
		Phase:           PhaseCapacity,
		VCPU:            cost.vcpu,
		Memory:          cost.memory,
		RequestedVCPU:   t.VCPU,
		RequestedMemory: t.Memory,
		InstanceType:    cost.instanceType,
		Site:            site,
		PriceUSDPerHour: cost.price,
		PreferenceRank:  preferenceRank,
		Epoch:           0,
	}, nil
}

// Assign binds a reserved lease to a GitHub job.
//
// Retrying with the SAME job is idempotent. Retrying with a DIFFERENT job is
// ErrConflict, not success: an escrowed slot holds one job, and returning nil while
// keeping the original would leave the caller believing a job is scheduled that
// nothing will run.
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

		if err := state.WriteQueries(tx).AssignLease(ctx, ledgerdb.AssignLeaseParams{
			Phase:       string(PhaseAssigned),
			RunID:       sql.NullInt64{Int64: runID, Valid: true},
			RequestID:   sql.NullInt64{Int64: requestID, Valid: true},
			HeartbeatAt: ts(now),
			ExpiresAt:   ts(now.Add(a.leaseTTL)),
			ID:          leaseID,
			Epoch:       epoch,
		}); err != nil {
			return fmt.Errorf("alloc: assign lease %s: %w", leaseID, err)
		}

		// Record the queue entry now, so job_history carries a real assignment
		// time rather than one fabricated at terminalization.
		return a.recordAssignment(ctx, tx, lease, runID, requestID, now)
	})
}

// attribution is the host a history row belongs to.
//
// COALESCE(node, target_node), the way the rest of the arithmetic reads a lease.
// `node` is filled at bind and escrow chose the machine long before that, so a
// lease that never binds — assigned by GitHub, then the process dies — recorded
// no host at all, and the jobs most worth investigating are exactly the ones
// that end that way.
func attribution(l *Lease) sql.NullString {
	switch {
	case l.Node != "":
		return sql.NullString{String: l.Node, Valid: true}
	case l.TargetNode != "":
		return sql.NullString{String: l.TargetNode, Valid: true}
	default:
		return sql.NullString{}
	}
}

// recordAssignment opens the history row at assignment time.
func (a *Allocator) recordAssignment(ctx context.Context, tx *sql.Tx, l *Lease, runID, requestID int64, now time.Time) error {
	node := attribution(l)

	if err := state.WriteQueries(tx).RecordJobAssignment(ctx, ledgerdb.RecordJobAssignmentParams{
		LeaseID:    l.ID,
		Tier:       l.Tier,
		Node:       node,
		RunID:      sql.NullInt64{Int64: runID, Valid: true},
		RequestID:  sql.NullInt64{Int64: requestID, Valid: true},
		QueuedAt:   ts(now),
		AssignedAt: sql.NullString{String: ts(now), Valid: true},
	}); err != nil {
		return fmt.Errorf("alloc: record assignment for %s: %w", l.ID, err)
	}

	return nil
}

// Advance moves a lease to the next phase, refusing anything the state machine
// does not allow.
func (a *Allocator) Advance(ctx context.Context, leaseID string, epoch int64, to Phase) error {
	return a.transition(ctx, leaseID, epoch, to, nil)
}

// MarkDeregistered records that a lease's GitHub runner registration has been
// removed, which is what ActiveRunnerLeases keys on instead of phase. A teardown
// whose compute destroy is still retrying is deregistered, so it stops
// over-counting against the assignment deficit and no longer drops a freshly
// acquired job; a teardown or quarantine whose runner was never deregistered
// stays counted, so a replacement is not launched against a runner GitHub can
// still schedule.
//
// It is NOT epoch-fenced, and deliberately so. Deregistration is a monotonic
// fact about GitHub, not about who holds the lease: once RemoveRunner has
// succeeded the runner is gone whatever the lease's epoch. A reap that
// quarantines the lease bumps the epoch, but a quarantined lease has only
// terminal successors and a reap never relaunches on the same row — new capacity
// is always a fresh lease id — so no live runner can ever occupy a row this flag
// has set. Fencing it here would let a reap between RemoveRunner and the mark
// strand a gone runner's lease as counted forever. Missing rows and terminal
// leases are harmless no-ops.
func (a *Allocator) MarkDeregistered(ctx context.Context, leaseID string) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		return state.WriteQueries(tx).MarkLeaseDeregistered(ctx, leaseID)
	})
}

// Resize authorises one remote shape before the node asks its API to buy it.
//
// The first fitting shape is charged at escrow. A later fallback is a new
// purchase decision, so its larger resource vector must fit atomically before the
// launch request is allowed onto the wire.
//
// IT SERVES EVERY REMOTE BACKEND, and it used to demand `ec2` by name. A codebuild
// lease reaching a fallback compute type would have been refused here — so the
// launch would fail rather than resize, which loses the fallback the ordered list
// exists for. Keyed on RunsOnHost for the same reason the registration branch is:
// a host-backed lease has no shape to resize, because its capacity IS the machine.
func (a *Allocator) Resize(
	ctx context.Context, leaseID string, epoch int64, instanceType string,
	vcpu int, memory config.ByteSize,
) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		if lease.Phase != PhaseLaunching {
			return fmt.Errorf("%w: lease %s is %s; a purchased shape may change only while "+
				"launching", ErrBadTransition, leaseID, lease.Phase)
		}

		if lease.Provider.RunsOnHost() || !lease.Provider.Valid() || lease.Node == "" {
			return fmt.Errorf("%w: lease %s is on %q, not a bound node of a backend that buys "+
				"shapes", ErrWrongProvider, leaseID, lease.Provider)
		}

		capacity, err := state.WriteQueries(tx).ReadNodeCapacity(ctx, lease.Node)
		if err != nil {
			return fmt.Errorf("alloc: read shapes for node %s: %w", lease.Node, err)
		}

		nodeVCPU, nodeMemory := int(capacity.TotalVcpu), capacity.TotalMemory

		shapes, err := decodeRemoteShapes(lease.Provider, capacity.Ec2Shapes)
		if err != nil {
			return err
		}

		declared := false

		var price config.USDPerHour

		for _, shape := range shapes {
			if shape.Type == instanceType && shape.VCPU == vcpu && shape.Memory == memory {
				declared = true
				price = shape.PriceUSDPerHour

				break
			}
		}

		if !declared {
			return fmt.Errorf("%w: node %s did not register EC2 shape %q (%d vCPU, %s)",
				ErrNotPlaceable, lease.Node, instanceType, vcpu, memory)
		}

		if vcpu < lease.RequestedVCPU || memory < lease.RequestedMemory {
			return fmt.Errorf("%w: EC2 shape %q (%d vCPU, %s) does not hold lease %s's "+
				"requested %d vCPU and %s", ErrNotPlaceable, instanceType, vcpu, memory,
				leaseID, lease.RequestedVCPU, lease.RequestedMemory)
		}

		if lease.InstanceType == instanceType && lease.VCPU == vcpu && lease.Memory == memory {
			return nil
		}

		fleetUsed, err := a.usage(ctx, tx)
		if err != nil {
			return err
		}

		// Authorize the replacement as if the current shape had first been
		// returned to the same host. The remaining fleet is then charged for every
		// other tier's outstanding floor exactly as initial placement is. Checking
		// only the raw ceilings lets a fallback spend capacity another tier was
		// promised.
		free, err := a.fleetResources(ctx, tx)
		if err != nil {
			return err
		}
		free.vcpu[lease.Node] += lease.VCPU
		free.memory[lease.Node] += lease.Memory
		owedVCPU, owedMemory, err := a.reserveFloors(ctx, tx, lease.Tier, free)
		if err != nil {
			return err
		}

		deploymentVCPU := a.limits.MaxVCPU - (fleetUsed.VCPU - lease.VCPU) - owedVCPU
		deploymentMemory := a.limits.MaxMemory - (fleetUsed.Memory - lease.Memory) - owedMemory
		if vcpu > free.vcpu[lease.Node] || memory > free.memory[lease.Node] ||
			vcpu > deploymentVCPU || memory > deploymentMemory ||
			vcpu > nodeVCPU || memory > config.ByteSize(nodeMemory) {
			return fmt.Errorf("%w: EC2 fallback %q needs %d vCPU and %s, which would exceed "+
				"the node or deployment budget or consume another tier's reserved capacity",
				ErrNoCapacity, instanceType, vcpu, memory)
		}

		// THE PRICE MOVES WITH THE SHAPE: a fallback is a different purchase at a
		// different rate, and it is recorded here, when it is charged.
		if err := state.WriteQueries(tx).ResizeLease(ctx, ledgerdb.ResizeLeaseParams{
			Vcpu:               int64(vcpu),
			Memory:             int64(memory),
			InstanceType:       instanceType,
			PriceMicrosPerHour: int64(price),
			ID:                 leaseID,
			Epoch:              epoch,
		}); err != nil {
			return fmt.Errorf("alloc: resize lease %s for EC2 shape %s: %w", leaseID, instanceType, err)
		}

		return nil
	})
}

// Bind records which node is running a lease.
//
// A lease pinned to a node may only bind to THAT node: without the check, a macOS
// lease pinned to one Mac could bind to another while its licence slot stayed
// charged to the first, and the second host would accept guests beyond Apple's
// limit with every individual decision looking correct.
//
// Rebinding elsewhere is refused rather than silently overwritten; repeating the
// same bind is idempotent, because a node retrying after a lost response must not
// be told its own success was a conflict.
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

		// Answered BEFORE the allowlist, because a repeat changes nothing: if the host's
		// policy tightened after this lease was placed, refusing the retry un-binds
		// nothing and turns a no-op into an error a node would read as "tear this down".
		// The policy gates NEW placements.
		if lease.Node == node {
			return nil // idempotent repeat
		}

		if lease.Node != "" {
			return fmt.Errorf("%w: lease %s is already bound to node %q, cannot rebind to %q",
				ErrWrongNode, leaseID, lease.Node, node)
		}

		// A FIRST binding for a lease already in a running phase means its node went
		// missing rather than that it was never placed, and adopting it onto a new host
		// would create a second owner of one slot.
		//
		// `leases.node` is ON DELETE SET NULL, so deleting a node blanks the column for
		// every lease it was running: the lease LOOKS unbound while the original host is
		// still executing the job and still holds a valid epoch.
		//
		// This refusal NARROWS the route and does not close it. A lease still in `assigned`
		// when its node is deleted is not yet running, so it binds elsewhere legally while
		// the stale original holds the same epoch — ownership is recorded, not proven.
		// Closing that needs fencing at deletion or a durable holder identity, which
		// belong with node lifecycle; there is deliberately no delete path here.
		if requiresPlacement(lease.Phase) {
			return fmt.Errorf(
				"%w: lease %s is already %s; a first binding now would make node %q a second owner",
				ErrWrongNode, leaseID, lease.Phase, node)
		}

		// The host's guest-OS allowlist is enforced HERE because this is the first point at
		// which the host is known: a lease with no target_node names no host at reserve
		// time, so config validation cannot rule out a placement it never sees.
		//
		// The guest OS comes from the lease's own column, not the live catalog.
		if err := a.checkPlacement(ctx, tx, lease, node); err != nil {
			return err
		}

		// THE CHOSEN BACKEND IS RECORDED HERE, and only here. What a lease MAY run
		// on is decided when it is reserved; what it IS running on is only knowable
		// once a host has taken it, and keeping them apart is what lets one label
		// span two kinds of machine.
		//
		// Read back from the node's registration rather than assumed from the list,
		// so the column says what is true rather than what was preferred.
		registered, err := state.WriteQueries(tx).ReadNodeProvider(ctx, node)
		if err != nil {
			return fmt.Errorf("alloc: read the provider of node %s: %w", node, err)
		}

		// AND WHICH PROCESS ON IT, read in the same transaction. The node binding
		// is the process launching the compute, and its incarnation is the
		// durable name of that process — NOT its registration epoch, which moves
		// on every registration including the same process registering again
		// after a control-plane restart. A report compares it with the row's
		// incarnation later to say whether the holder was replaced.
		//
		// A replacement registering between the wire's incarnation check and
		// this read records the replacement's incarnation for compute the old
		// process is launching, which reads as NOT replaced: the conservative
		// direction, and that launch is answered with custody regardless.
		holder, err := state.WriteQueries(tx).ReadNodeIncarnation(ctx, node)
		if err != nil {
			return fmt.Errorf("alloc: read the incarnation of node %s: %w", node, err)
		}

		if err := state.WriteQueries(tx).BindLease(ctx, ledgerdb.BindLeaseParams{
			Node:              sql.NullString{String: node, Valid: true},
			ChosenProvider:    registered,
			HolderIncarnation: holder,
			ID:                leaseID,
			Epoch:             epoch,
		}); err != nil {
			return fmt.Errorf("alloc: bind lease %s: %w", leaseID, err)
		}

		return nil
	})
}

// Heartbeat extends a lease's expiry. A holder that stops calling this loses the
// lease to Reap.
func (a *Allocator) Heartbeat(ctx context.Context, leaseID string, epoch int64) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}
		if lease.ForceRelease {
			return fmt.Errorf("%w: lease %s on node %s", ErrForceRelease, leaseID, lease.Node)
		}

		now := a.now().UTC()

		return state.WriteQueries(tx).HeartbeatLease(ctx, ledgerdb.HeartbeatLeaseParams{
			HeartbeatAt: ts(now),
			ExpiresAt:   ts(now.Add(a.leaseTTL)),
			ID:          leaseID,
			Epoch:       epoch,
		})
	})
}

// Failure reasons billet writes for ITSELF, as opposed to the free-form text an
// external party's warning carries.
//
// A TEARDOWN'S OUTCOME HAS TO SURVIVE THE PROCESS THAT DECIDED IT. A node holds
// the outcome of a teardown in memory — a launch that failed ambiguously is a
// job that never ran — while the ledger records only the phase, and the same
// phase serves a completed job's teardown. A restart adopting that lease would
// reconstruct `done` for a runner that never started.
// So the node records why the lease will fail BEFORE it first reports the hold,
// through the same route an external reason takes, and adoption reads it back.
//
// THE EXACT VALUE IS WHAT MarkFailure KEYS ON, never the prefix. The reason
// arrives over the wire from a node, so a rule keyed on a prefix anybody can
// type would let a caller suppress the disruption token an external reclaim
// must carry; and the reasons below do not all mean the same thing. A launch
// that never ran and a runner found idle are billet's own conclusions about
// compute no job was on, so no token is written beside them (`reclaimed` would
// attribute a failed build to an interruption that never happened). A job
// destroyed under an operator's custody bound WAS running, and billet ended it,
// which is a disruption in every sense the attribution report records.
const (
	// LaunchFailedReason marks a launch that failed after possibly starting
	// something, whose compute is being removed without ever having run a job.
	LaunchFailedReason = "billet:launch-failed"
	// HeldPastLimitReason marks a job an operator's custody bound destroyed.
	HeldPastLimitReason = "billet:held-past-limit"
	// RunnerRetiredReason marks adopted compute whose runner registration was
	// found idle or absent and removed, so it can never run a job.
	RunnerRetiredReason = "billet:runner-retired"
	// ForceReleasedReason marks a lease an operator released with --force, on
	// their own assertion that its compute was gone.
	ForceReleasedReason = "billet:force-released"
)

// disruptionFor maps a failure reason to the token recorded beside it, if any.
//
// EXACT MATCHES ONLY. Anything not on this list is an external party's reason
// and is recorded as a reclaim, which is what it was before these existed.
//
// AND THE LEDGER'S OWN EVIDENCE OUTRANKS THE TEXT. The reason arrives over the
// wire from a node, so a node that sent `launch-failed` or `runner-retired`
// for a job that ran would otherwise suppress the token an external reclaim
// must carry. Both of those reasons CLAIM no job was ever on the compute, and
// the ledger knows whether that is true: job_history.started_at is written
// when GitHub reports the job started, before any node can send a reason about
// it. A claim the ledger contradicts gets the reclaim token, which is what any
// other text gets. What is still a node's to choose is WHICH disruption token
// a lease that did run a job carries — every one of them is a disruption, so
// the report shows one either way. The operator's own reason never comes
// through here: MarkFailure refuses it.
func disruptionFor(reason string, started bool) (Disruption, bool) {
	switch reason {
	case LaunchFailedReason, RunnerRetiredReason:
		if started {
			return DisruptionReclaimed, true
		}

		return "", false
	case ForceReleasedReason:
		return "", false
	case HeldPastLimitReason:
		return DisruptionHeldPastLimit, true
	default:
		return DisruptionReclaimed, true
	}
}

// MarkFailure records why a still-open lease is destined to fail.
//
// Separate from Release because the fact often arrives before compute is gone,
// and capacity must remain charged throughout that interval.
func (a *Allocator) MarkFailure(ctx context.Context, leaseID string, epoch int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("alloc: a failure reason must not be empty")
	}
	if reason == inventoryAbsenceFailureReason {
		return errors.New("alloc: that failure reason is reserved for inventory reconciliation")
	}
	// THE OPERATOR'S REASON IS WRITTEN BY THE OPERATOR'S COMMAND, in the same
	// statement that requests or applies the release. From here it could only be
	// a node asserting an operator did something, which it cannot know.
	if reason == ForceReleasedReason {
		return errors.New("alloc: that failure reason is reserved for billet leases release --force")
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.load(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		return a.markFailureTx(ctx, tx, lease, reason)
	})
}

// markFailureTx records a reason against a loaded lease, and its token, in the
// caller's transaction.
//
// Writes the reason back onto the lease it was given, so a caller that goes on
// to archive the row in the same transaction carries it into the history.
func (a *Allocator) markFailureTx(ctx context.Context, tx *sql.Tx, lease *Lease, reason string) error {
	if lease.FailureReason != "" && lease.FailureReason != reason {
		return fmt.Errorf("%w: lease %s already records failure reason %q, cannot replace it with %q",
			ErrConflict, lease.ID, lease.FailureReason, reason)
	}

	q := state.WriteQueries(tx)

	err := q.MarkLeaseFailure(ctx, ledgerdb.MarkLeaseFailureParams{
		FailureReason: reason,
		ID:            lease.ID,
		Epoch:         lease.Epoch,
	})
	if err != nil {
		return fmt.Errorf("alloc: record failure reason for lease %s: %w", lease.ID, err)
	}

	lease.FailureReason = reason

	// AND THE SAME FACT IN BILLET'S OWN WORDS, where there is one. The reason
	// above is free-form text a node supplied; this is a token from a closed
	// set, which is what a report may act on. Both describe one event, so
	// they are written in one transaction — an external reclaim recorded
	// without the token would be invisible to `billet leases failures`
	// forever. A reason for compute no job was ever on carries none — and
	// whether a job was on it is the ledger's to say, never the reason's.
	started, err := q.ReadJobStarted(ctx, lease.ID)
	if err != nil {
		return fmt.Errorf("alloc: read whether lease %s ran a job: %w", lease.ID, err)
	}

	token, disrupted := disruptionFor(reason, started)
	if !disrupted {
		return nil
	}

	marked, err := markLeaseDisruptedTx(ctx, tx, lease.ID, token, a.now())
	if err != nil {
		return err
	}
	if marked {
		lease.Disruption, lease.DisruptedAt = token, ts(a.now().UTC())
	}

	return nil
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
		// Treating every not-found as success would make releasing an id that never existed
		// return nil, and re-releasing a `done` lease as `failed` return nil too.
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

		if err := state.WriteQueries(tx).SetLeasePhase(ctx, ledgerdb.SetLeasePhaseParams{
			Phase: string(outcome),
			ID:    leaseID,
			Epoch: epoch,
		}); err != nil {
			return fmt.Errorf("alloc: release lease %s: %w", leaseID, err)
		}

		return a.archive(ctx, tx, lease, outcome)
	})
}

// ReleaseFailed terminalizes a lease as failed and records why, in one
// transaction.
//
// THE LISTENER'S OWN FAILURES GO THROUGH HERE. A launch that failed
// conclusively never held compute and archives at once, so nothing outlives a
// process here — the reason exists for the report, where a failure nothing
// explains is a row an operator cannot act on. Written in the release's own
// transaction so the archive copies it, and idempotent exactly as Release is: a
// lease already failed is left alone whatever it recorded, and a lease that
// finished as done is refused rather than rewritten. A reason already on the
// row stands, because the earlier fact is the one that can still have been
// causal.
func (a *Allocator) ReleaseFailed(ctx context.Context, leaseID string, epoch int64, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return errors.New("alloc: a failure reason must not be empty")
	}
	if reason == inventoryAbsenceFailureReason || reason == ForceReleasedReason {
		return fmt.Errorf("alloc: failure reason %q is reserved", reason)
	}

	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		lease, err := a.loadAny(ctx, tx, leaseID, epoch)
		if err != nil {
			return err
		}

		if lease.Phase.Terminal() {
			if lease.Phase != PhaseFailed {
				return fmt.Errorf("%w: lease %s already finished as %s, cannot re-finish as %s",
					ErrConflict, leaseID, lease.Phase, PhaseFailed)
			}

			// THE REAPER GOT THERE FIRST, AND ARCHIVED A FAILURE IT COULD NOT
			// EXPLAIN. An escrow-only lease whose launch failed and whose release
			// was parked expires into a plain failed row, with no reason, before
			// the retry lands; the caller still knows why, and the fact is as true
			// as it was a TTL ago. Written onto both rows where nothing explains
			// the failure yet; a reason already there stands.
			if lease.FailureReason == "" {
				q := state.WriteQueries(tx)
				if err := q.BackfillFailureReason(ctx,
					ledgerdb.BackfillFailureReasonParams{FailureReason: reason, ID: leaseID}); err != nil {
					return fmt.Errorf("alloc: explain the failure of lease %s: %w", leaseID, err)
				}
				if err := q.BackfillLeaseFailureReason(ctx,
					ledgerdb.BackfillLeaseFailureReasonParams{FailureReason: reason, ID: leaseID}); err != nil {
					return fmt.Errorf("alloc: explain the failure of lease %s: %w", leaseID, err)
				}
			}

			return nil
		}

		if lease.FailureReason == "" {
			if err := a.markFailureTx(ctx, tx, lease, reason); err != nil {
				return err
			}
		}

		if err := state.WriteQueries(tx).SetLeasePhase(ctx, ledgerdb.SetLeasePhaseParams{
			Phase: string(PhaseFailed),
			ID:    leaseID,
			Epoch: epoch,
		}); err != nil {
			return fmt.Errorf("alloc: release lease %s: %w", leaseID, err)
		}

		return a.archive(ctx, tx, lease, PhaseFailed)
	})
}

// NodeRegistration is what a host tells the ledger about itself.
//
// A STRUCT RATHER THAN FIVE ARGUMENTS: two are strings meaning entirely different
// things and two are numbers that are not interchangeable, so positionally,
// transposing any pair compiles and produces a fleet that is wrong in a way that
// surfaces as bad placement rather than as an error.
type NodeRegistration struct {
	// Name is what tiers pin to and what certificates authorise.
	Name string
	// Provider is the compute backend this host runs. A host is the authority on
	// this, which is why it is reported rather than read from a catalogue.
	Provider config.ProviderKind
	// Site is where this machine is, or empty for a deployment that has not
	// needed the distinction.
	Site string
	// VCPU and Memory are what this host CONTRIBUTES, which is not necessarily
	// what it has — see config.NodeConfig.Contribution.
	VCPU   int
	Memory config.ByteSize
	// EC2Shapes are the ordered shapes this node may buy, whatever its remote
	// backend calls them. Empty for a backend that runs work on its own host.
	//
	// The name is the wire's and the ledger column's; see nodeapi.RegisterRequest.
	EC2Shapes []config.RemoteShape
	// Incarnation is the value this node PROCESS minted for its whole life and
	// presents on every request, or empty for a host that presented none.
	//
	// RECORDED SO A LEASE CAN NAME ITS HOLDER. The registration epoch names a
	// registration, and the same process registers again after every
	// control-plane restart; this names the process. A lease copies it at Bind
	// and on entry to custody or teardown, and `billet leases` compares the two
	// to say whether the process holding a lease is the one the host runs now.
	// Nothing authorises anything from it.
	Incarnation string
	// CodeBuildFleet is the reserved-capacity fleet this node's builds run on, or
	// empty for on-demand compute.
	//
	// RECORDED AND ENFORCED, unlike Release and Digest beside it. A reserved fleet
	// is one shared pool, so two live nodes naming it each advertise all of it —
	// which is escrow promising GitHub twice what AWS will run, from two config
	// files neither of which is wrong alone. This is the only place both are
	// visible, so a duplicate is refused here.
	CodeBuildFleet string
	// CodeBuildJITPath and CodeBuildRegion are where this node stages each
	// build's single-use runner registration, and the region it does it in.
	//
	// RECORDED SO THE CONTROL PLANE CAN SWEEP THE PATH. A node that dies between
	// staging a registration and reaching any of the three places that remove one
	// leaks exactly one parameter, and from the provider alone "no build for this
	// lease" and "the build has not appeared yet" are the same observation. What
	// authorises the delete is the LEDGER (the lease terminal, and closed longer
	// ago than any build could still run), which only the control plane holds —
	// and it holds no node.codebuild block, so the path has to arrive here. Both
	// or neither: a path without a region cannot be reached and a region without
	// a path names nothing. Empty on a host that registered before it could say,
	// which `billet status` names rather than reads as swept.
	CodeBuildJITPath string
	CodeBuildRegion  string
	// Release is the node binary, and WireMin/WireMax the wire versions it said
	// it speaks. WireVersion is the one registration settled on.
	//
	// RECORDED, NOT ENFORCED. Nothing in placement or capacity reads these — they
	// exist so an operator can see which hosts still hold an old protocol open,
	// and so a later release knows when it may stop supporting one. Negotiation
	// has exactly ONE authority and it is the wire; a second opinion here would be
	// a second place for the fleet's version to be decided. Zero and empty mean
	// "not recorded" and are reported as unknown, which is what a host registered
	// before this existed leaves behind.
	Release string
	// Digest is the sha256 of the signed release manifest that produced this
	// host's binary, or empty when nothing on that machine can say.
	//
	// OVERWRITTEN ON EVERY REGISTRATION, like Release and for the same reason: the
	// answer to "what is this machine running" is whatever it just said, and
	// keeping an older value would leave a converged host reported against bytes
	// it has replaced.
	//
	// A CLAIM, LIKE EVERY OTHER FIELD HERE, and one that authorises nothing. What
	// it does is let a rollout tell a host that installed the manifest it decided
	// on from one that installed something else — where before there was only a
	// version string the two would have shared.
	Digest      string
	WireMin     int
	WireMax     int
	WireVersion int
}

// checkWireTuple refuses a wire record that cannot describe any real build.
//
// TWO SHAPES ARE LEGAL AND NOTHING BETWEEN THEM. All zero is "not recorded",
// which is what a row written before this existed leaves behind and what every
// reader renders as unknown. Otherwise the three numbers must nest:
// 0 < min <= negotiated <= max. Clamping a bad value instead of refusing it is
// what turns a caller's mistake into a report nobody can tell is wrong.
func checkWireTuple(name string, reg NodeRegistration) error {
	if reg.WireMin == 0 && reg.WireMax == 0 && reg.WireVersion == 0 {
		return nil
	}

	if reg.WireMin <= 0 || reg.WireVersion < reg.WireMin || reg.WireMax < reg.WireVersion {
		return fmt.Errorf(
			"alloc: node %s reported protocol minimum %d, negotiated %d and newest %d; "+
				"those cannot all be true of one build, and `billet status` decides from "+
				"them whether an old protocol is safe to stop supporting",
			name, reg.WireMin, reg.WireVersion, reg.WireMax)
	}

	return nil
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
func (a *Allocator) RegisterNode(ctx context.Context, reg NodeRegistration) (int64, error) {
	name, kind := reg.Name, reg.Provider

	if name == "" {
		return 0, errors.New("alloc: a node must have a name")
	}

	if kind == "" {
		return 0, fmt.Errorf("alloc: node %s registered no provider, so nothing could be placed "+
			"on it safely", name)
	}

	// AND A PROVIDER BILLET DOES NOT RECOGNISE IS NOT A PROVIDER. This is exported,
	// so it cannot assume its caller came through config.Load or the wire — the
	// alloc.New rule. It matters more than it reads: every placement question is
	// answered from this string, and `RunsOnHost` is an allowlist, so an unknown
	// backend is treated as REMOTE. It would therefore be expected to declare a
	// shape catalogue, be charged the shape it buys, and be exempted from the
	// overcommit comparison — three decisions taken about a name nothing implements.
	if !kind.Valid() {
		return 0, fmt.Errorf("alloc: node %s registered provider %q, which this billet does not "+
			"implement; placement, shape accounting and the host-contribution rules are all "+
			"decided from it, so a name billet cannot classify is refused rather than guessed at",
			name, kind)
	}

	// A CONTRIBUTION OF NOTHING FAILS NOWHERE ELSE, which is why it fails here: a node
	// recorded with zero capacity registers cleanly, joins the fleet, and is simply
	// never chosen, so the tier it was meant to serve advertises nothing while the
	// machine looks healthy.
	if reg.VCPU <= 0 {
		return 0, fmt.Errorf("alloc: node %s contributes %d vcpu; a host that offers no cores "+
			"can never be given work", name, reg.VCPU)
	}

	if reg.Memory <= 0 {
		return 0, fmt.Errorf("alloc: node %s contributes %s of memory; a host that offers none "+
			"can never be given work", name, reg.Memory)
	}

	// RE-APPLIED HERE BECAUSE THIS IS EXPORTED, the same rule the tier catalogue
	// follows: a caller that did not come through the wire must not be able to
	// write a tuple that contradicts itself. Nothing authorises compute from these
	// columns, but `billet status` decides from them whether an old protocol is
	// safe to RETIRE — and `min=14, max=12, negotiated=13` reads as a converged
	// host, which retires a protocol a live machine still needs.
	if err := checkWireTuple(name, reg); err != nil {
		return 0, err
	}

	shapes := slices.Clone(reg.EC2Shapes)
	for i := range shapes {
		shapes[i].Type = strings.TrimSpace(shapes[i].Type)
	}

	// A REMOTE BACKEND DECLARES WHAT IT MAY BUY; A HOST-BACKED ONE IS THE MACHINE.
	//
	// Keyed on RunsOnHost rather than on `== ec2`, so a second remote backend is
	// covered without anybody remembering — which is the same reason RunsOnHost is
	// an allowlist. Getting this wrong in the permissive direction is the
	// expensive one: a codebuild node whose shapes were refused registers with no
	// catalogue, placement then cannot charge the shape it buys, and the lease is
	// charged the smaller TIER request instead. That is an overcommit on a machine
	// nobody can inspect.
	switch {
	case !kind.RunsOnHost():
		if errs := config.CheckRemoteShapes(kind, shapes); len(errs) > 0 {
			return 0, fmt.Errorf("alloc: node %s reported an invalid %s shape catalogue: %w",
				name, kind, errors.Join(errs...))
		}

	default:
		if len(shapes) > 0 {
			return 0, fmt.Errorf("alloc: node %s runs %s, which runs work on the host itself, "+
				"but reported a purchasable shape catalogue", name, kind)
		}
	}

	// A FLEET IS A CODEBUILD FACT AND NOTHING ELSE'S. Accepting one from another
	// backend would record a shared-pool claim against a host that draws on no
	// pool, and the uniqueness refusal below would then keep a perfectly good
	// second node out of the fleet over a field its provider never reads.
	fleet := strings.TrimSpace(reg.CodeBuildFleet)
	if fleet != "" && kind != config.ProviderCodeBuild {
		return 0, fmt.Errorf("alloc: node %s runs %s but reported a codebuild fleet; only a "+
			"codebuild node draws on one", name, kind)
	}

	// THE PATH IS A CODEBUILD FACT TOO, and it is re-validated here because this is
	// exported: the control plane will LIST and DELETE under whatever lands in this
	// column, so a value that did not come through config.Load gets the same rule
	// the config applies (config.CheckSSMParameterPath is that one rule). A path
	// from another backend would send the sweep to a place that backend never
	// writes; a path without a region cannot be reached at all.
	jitPath, region := strings.TrimSpace(reg.CodeBuildJITPath), strings.TrimSpace(reg.CodeBuildRegion)

	switch {
	case jitPath == "" && region == "":
		// Nothing declared; ordinary for every other backend and for a codebuild
		// node below the wire version that carries it.
	case kind != config.ProviderCodeBuild:
		return 0, fmt.Errorf("alloc: node %s runs %s but reported a codebuild registration path; "+
			"only a codebuild node stages registrations", name, kind)
	case jitPath == "" || region == "":
		return 0, fmt.Errorf("alloc: node %s reported codebuild registration path %q in region %q; "+
			"the two travel together, because a path without a region cannot be reached and a "+
			"region without a path names nothing", name, jitPath, region)
	default:
		if err := config.CheckSSMParameterPath(jitPath); err != nil {
			return 0, fmt.Errorf("alloc: node %s reported a codebuild registration path %w", name, err)
		}

		if err := config.CheckCodeBuildRegion(region); err != nil {
			return 0, fmt.Errorf("alloc: node %s reported a codebuild registration region: %w", name, err)
		}
	}

	encodedShapes, err := encodeEC2Shapes(shapes)
	if err != nil {
		return 0, err
	}

	var epoch int64

	err = a.db.Tx(ctx, func(tx *sql.Tx) error {
		now := ts(a.now().UTC())

		// A HOST MAY NOT CHANGE ITS BACKEND WHILE IT IS RUNNING WORK.
		//
		// Overwriting the provider would falsify every lease already bound there:
		// each recorded the backend it chose at bind, so the ledger would say a job
		// runs on firecracker while the host calls itself docker — and later checks
		// read the NODE's row, so they would go on authorizing the lease.
		//
		// Rewriting chosen_provider instead would be worse: it relabels compute
		// that is already running. So this is refused, and the operator's route is
		// to drain the host and re-register it. An unbound node changes freely.
		q := state.WriteQueries(tx)

		// THE ZERO ROW IS THE FIRST-REGISTRATION CASE, and reading the fields
		// unconditionally is safe for exactly that reason: on sql.ErrNoRows every
		// one of them is empty, which is what "nothing to contradict" looks like.
		existing, err := q.ReadNodeRegistration(ctx, name)
		current, currentSite := existing.Provider, existing.Site
		currentShapes, currentFleet := existing.Ec2Shapes, existing.CodebuildFleet
		existed := true

		switch {
		case errors.Is(err, sql.ErrNoRows):
			// First registration; nothing to contradict.
			existed = false
		case err != nil:
			return fmt.Errorf("alloc: read node %s: %w", name, err)

		// A HOST DOES NOT MOVE WHILE IT IS RUNNING WORK, for the same reason it may not
		// change its backend: the leases here recorded where they are, and site is where a
		// cache lives, so the ledger would point later placements at storage in a different
		// building from the containers already running.
		//
		// WORK MERELY AIMED HERE COUNTS TOO, so both guards attribute a lease the way the
		// rest of the arithmetic does — COALESCE(node, target_node). `node` is filled at
		// bind, but escrow chose the machine long before that.
		//
		// EXPIRED IDLE WORK DOES NOT GET A VOTE. billet registers the node BEFORE the
		// server starts and the reaper runs inside that server, so a registration refused
		// on the strength of abandoned escrow prevents the only process that could clear
		// it. The cutoff is the reaper's own.
		//
		// A RUNNING LEASE STILL DOES, EXPIRED OR NOT, because expiry proves only that the
		// control-plane holder stopped heartbeating — never that the container stopped.
		// QUARANTINE IS THE SAME ANSWER WRITTEN DOWN: the reaper reached exactly that
		// conclusion about this lease, so it is the LAST phase that should let a host
		// change its backend. Leaving it out made the reaper's own verdict the thing
		// that unlocked the move.
		// Reading the two the same way let a host change its backend out from under work
		// the new backend cannot see: the docker container keeps running, tart
		// reconciliation cannot enumerate it, the reaper frees the lease, and the next
		// escrow puts a second job on a machine still running the first. This does not
		// deadlock, because the reaper lives in the server, the server starts without this
		// node, and a terminalised lease stops counting here.
		//
		// Capacity is different and IS overwritten below.
		case currentSite != reg.Site:
			outstanding, err := q.CountLiveWorkOnNode(ctx, ledgerdb.CountLiveWorkOnNodeParams{
				Node: name,
				Now:  now,
			})
			if err != nil {
				return fmt.Errorf("alloc: count the leases on node %s: %w", name, err)
			}

			if outstanding > 0 {
				return fmt.Errorf(
					"%w: node %s is recorded at site %q and now reports %q, but %d lease(s) are "+
						"still outstanding against it there. Put node.site back to %q and start "+
						"billet; once those jobs finish (or their leases expire) the host is "+
						"free to move",
					ErrWrongSite, name, currentSite, reg.Site, outstanding, currentSite)
			}

		case current != string(kind):
			outstanding, err := q.CountLiveWorkOnNode(ctx, ledgerdb.CountLiveWorkOnNodeParams{
				Node: name,
				Now:  now,
			})
			if err != nil {
				return fmt.Errorf("alloc: count the leases on node %s: %w", name, err)
			}

			if outstanding > 0 {
				// THE WAY OUT IS SPELLED OUT, because this fires during startup —
				// cmd registers the node before it recovers anything — so an
				// operator who changes node.provider with work still bound finds
				// billet refusing to start, at the worst possible moment. "Drain the
				// host" would not be actionable advice: there is no drain command,
				// and billet is not running to accept one.
				return fmt.Errorf(
					"%w: node %s is registered as %q and now reports %q, but %d lease(s) are "+
						"still outstanding against it and recorded the old backend. Put "+
						"node.provider back to %q and start billet; once those jobs finish (or "+
						"their leases expire) the host is free to change",
					ErrWrongProvider, name, current, kind, outstanding, current)
			}
		}

		// A HOST DOES NOT CHANGE FLEETS WHILE IT IS RUNNING WORK, and the first version
		// overwrote the fleet unconditionally.
		//
		// The consequence is the uniqueness refusal defeating itself: a node
		// re-registering from fleet A to fleet B — or to on-demand — RELEASES A's claim
		// while its existing builds still draw on A, after which another node may take
		// A and advertise its whole capacity on top of them. Site and provider are
		// guarded for exactly this reason; the fleet is the same kind of fact and was
		// missing the same guard.
		//
		// A TRANSITION TO OR FROM AN EMPTY FLEET COUNTS. Moving a node from a reserved
		// fleet to on-demand releases the claim just as surely as moving it to another
		// fleet does, so the comparison is on the strings rather than on "did it name
		// one".
		if existed && currentFleet != fleet {
			outstanding, err := q.CountLiveWorkOnNode(ctx, ledgerdb.CountLiveWorkOnNodeParams{
				Node: name,
				Now:  now,
			})
			if err != nil {
				return fmt.Errorf("alloc: count the leases on node %s: %w", name, err)
			}

			if outstanding > 0 {
				return fmt.Errorf(
					"%w: node %s is recorded on codebuild fleet %s and now reports %s, but %d "+
						"lease(s) are still outstanding against it. Those builds still draw on "+
						"the old fleet, so releasing its claim would let another node advertise "+
						"its whole capacity on top of them. %s and start billet; once those "+
						"jobs finish (or their leases expire) the host is free to change",
					ErrFleetHeld, name, describeFleet(currentFleet), describeFleet(fleet),
					outstanding, restoreFleetRemedy(currentFleet))
			}
		}

		shapesMatch := true
		if existed && currentShapes != "" {
			shapesMatch, err = sameEC2PlacementShapes(kind, currentShapes, shapes)
			if err != nil {
				return fmt.Errorf("alloc: read the previous shape catalogue for node %s: %w", name, err)
			}
		}
		if !shapesMatch {
			outstanding, err := q.CountLiveWorkOnNode(ctx, ledgerdb.CountLiveWorkOnNodeParams{
				Node: name,
				Now:  now,
			})
			if err != nil {
				return fmt.Errorf("alloc: count the leases on node %s: %w", name, err)
			}

			if outstanding > 0 {
				// THE FIELD IS THE NODE'S OWN, not ec2's. This message named
				// `instance_types` for every remote backend, so a codebuild operator
				// was sent to a key their config does not contain — and kind.ShapeField
				// existed for exactly this and was not consulted here.
				return fmt.Errorf(
					"alloc: node %s changed its %s shape catalogue while %d lease(s) are still "+
						"outstanding; restore the old ordered %s until they finish",
					name, kind, outstanding, kind.ShapeField())
			}
		}

		// CAPACITY AND SITE ARE OVERWRITTEN ON EVERY REGISTRATION, unlike the provider
		// above: a host that comes back offering less has been reconfigured, and leases
		// already open keep their capacity charged, so the only effect is that no new work
		// fits until enough of them drain.
		//
		// THE EPOCH COMES BACK OUT. It is this row's fencing token, and returning it is
		// what lets a later "this node is gone" be attributed to the incarnation that
		// actually went. RETURNING makes that one statement rather than a write and a
		// hopeful read.
		//
		// THE BUILD AND THE WIRE ARE OVERWRITTEN TOO, and for the same reason as
		// capacity: a host that comes back has been replaced or upgraded, and the
		// answer to "what is this machine running" is whatever it just said. There
		// is nothing to grandfather, because nothing reads these to authorise
		// anything.
		// THE FIRST EPOCH IS ONE, NOT ZERO, and taking the column default was a
		// defect that hid for as long as nothing read the number for anything but
		// equality.
		//
		// Zero is what every other recorded-at-registration field in this row means
		// by "the binary that wrote me did not record this" — an absent release, a
		// wire version of zero. A rollout asks whether a host's CURRENT epoch is
		// higher than the one an instruction was sent against, so an epoch that is
		// legitimately zero is indistinguishable from one that was never recorded,
		// and the rollout declines to conclude anything. That is the ORDINARY case:
		// a node registers once, an operator starts a rollout, and the host is
		// dispatched at epoch zero — after which no rollback on that machine could
		// ever be detected and it would hold the cohort forever.
		// ONE NODE PER RESERVED FLEET, AND THIS IS THE ONLY PLACE THAT CAN SAY SO.
		//
		// A reserved CodeBuild fleet is one shared pool of instances. Two nodes
		// naming it each register the fleet's whole capacity as their own
		// contribution, so escrow advertises it twice and the deployment promises
		// GitHub more concurrent jobs than AWS will run — the overcommit escrow
		// exists to prevent, arriving from two config files neither of which is
		// wrong on its own. Config validation cannot see it: the two files are on
		// two machines.
		//
		// THE CLAIM SURVIVES LIVENESS LOSS, and the first version scoped it to
		// `live = 1` — which reads as reasonable and is not. LIVENESS SAYS NOTHING
		// ABOUT REMOTE COMPUTE: `ForgetEveryNode` marks the whole fleet not-live
		// whenever a control plane STARTS, and a node that merely disconnected is
		// not-live while its builds keep running. So a differently-named node could
		// claim the same fleet across an ordinary restart and advertise its full
		// capacity on top of work the old node still holds.
		//
		// THE ORDINARY REPLACEMENT STILL WORKS because it reuses the node NAME, which
		// this query excludes — that is the case an operator actually performs, and it
		// needs nothing. What is refused is a claim under a DIFFERENT name, and the
		// route for that is `billet nodes decommission`, which already exists and
		// already requires a compute proof before it will forget a host. Sending an
		// operator to a command that proves the fleet is idle is the whole point;
		// letting liveness stand in for that proof is the thing that was wrong.
		//
		// AND A FORCED DECOMMISSION IS NOT A PROOF EITHER, which is the same mistake
		// one step further on. `--force` exists precisely for the host nothing could
		// ask, and it records the exclusion as `decommission_proven = 0` so every
		// later drain and `billet status` keep saying so. Reading only
		// decommissioned_at therefore released a fleet on the strength of an
		// operator saying "take it out of the set" — which says nothing about whether
		// builds are still drawing on it. Only a PROVEN exclusion releases the claim;
		// the derivation is `provedTx`, and the barrier tests own how a proof is
		// established.
		//
		// IT IS INSIDE THE TRANSACTION, so the read and the insert cannot be split
		// by a second registration: SQLite's single writer plus BEGIN IMMEDIATE is
		// what makes "no other node holds this" true at the moment it is written,
		// rather than true when somebody looked.
		if fleet != "" {
			holder, err := q.FleetClaimHolder(ctx, ledgerdb.FleetClaimHolderParams{
				Fleet: fleet,
				Name:  name,
			})

			switch {
			case errors.Is(err, sql.ErrNoRows):
				// Nobody else holds it.
			case err != nil:
				return fmt.Errorf("alloc: read the holder of codebuild fleet %s: %w", fleet, err)
			default:
				return fmt.Errorf("%w: node %s reports codebuild fleet %s, which node %s "+
					"already draws on. A reserved fleet is one shared pool of instances, so two "+
					"nodes on it would each advertise its whole capacity and this deployment "+
					"would promise GitHub more concurrent jobs than AWS will run — and neither "+
					"a node being unreachable nor a --force exclusion says anything about "+
					"whether its builds are still using the fleet. To move the fleet to this "+
					"node, run `billet drain --wait` and then `billet nodes decommission %s`, "+
					"which releases the claim only once something has PROVED no compute "+
					"remains there. To run both, give this node its own fleet or point it at "+
					"on-demand compute by removing node.codebuild.fleet_arn",
					ErrFleetHeld, name, fleet, holder, holder)
			}
		}

		// A HOST THAT COMES BACK IS A MEMBER AGAIN, and the statement is what says
		// so: it clears the decommission and the drained flag as part of the same
		// upsert that bumps the epoch. See internal/state/queries/nodes.sql.
		// THE HIGHEST RELEASE NEVER GOES DOWN, and it is read here, inside the
		// same transaction, so the comparison and the write are one decision. A
		// host coming back on an older release keeps the newer mark; a report can
		// then say so. Only a release tag is ever kept: a development build names
		// nothing that can be ordered, and storing it would make every later
		// comparison "could not tell" for as long as the row lives.
		highest, err := highestRelease(ctx, q, name, reg.Release)
		if err != nil {
			return err
		}

		registered, err := q.UpsertNodeRegistration(ctx, ledgerdb.UpsertNodeRegistrationParams{
			Name:           name,
			HighestRelease: highest,
			Provider:       string(kind),
			Site:           reg.Site,
			TotalVcpu:      int64(reg.VCPU),
			TotalMemory:    int64(reg.Memory),
			Ec2Shapes:      encodedShapes,
			LastSeenAt:     now,
			NodeRelease:    strings.TrimSpace(reg.Release),
			WireMin:        int64(max(reg.WireMin, 0)),
			WireMax:        int64(max(reg.WireMax, 0)),
			WireVersion:    int64(max(reg.WireVersion, 0)),
			NodeDigest:     strings.TrimSpace(reg.Digest),
			CodebuildFleet: fleet,
			Incarnation:    strings.TrimSpace(reg.Incarnation),
			// OVERWRITTEN ON EVERY REGISTRATION, like the release: the answer to
			// "where does this host stage registrations" is whatever it just said.
			// A host that moves paths leaves the old one unswept, which is said in
			// docs/deploying/aws-codebuild.md rather than guarded here — refusing the
			// move would
			// keep a correctly reconfigured host out of the fleet over a path nothing
			// on it writes to any more.
			CodebuildJitPath: jitPath,
			CodebuildRegion:  region,
		})
		if err != nil {
			return fmt.Errorf("alloc: register node %s: %w", name, err)
		}

		epoch = registered

		return nil
	})
	if err != nil {
		return 0, err
	}

	return epoch, nil
}

// highestRelease decides what a host's highest-release column holds after a
// registration reporting `release`.
//
// THREE ANSWERS, NONE OF THEM A REFUSAL. The reported release is a tag newer than
// the recorded one, or there is no recorded one: the reported release. It is a
// tag that is older or equal: the recorded one stays. It is not a release tag at
// all: the recorded one stays, because "(devel)" cannot be ordered against
// anything and must not become the mark every later registration is measured
// against.
func highestRelease(ctx context.Context, q state.ReadOps, name, release string) (string, error) {
	release = strings.TrimSpace(release)

	recorded, err := q.ReadNodeHighestRelease(ctx, name)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		recorded = ""
	case err != nil:
		return "", fmt.Errorf("alloc: read the highest release %s has registered with: %w",
			name, err)
	}

	if !version.IsRelease(release) {
		return recorded, nil
	}

	if recorded == "" {
		return release, nil
	}

	if order, ok := version.Compare(release, recorded); ok && order > 0 {
		return release, nil
	}

	return recorded, nil
}

// NodeGone records that the control plane has given up on a host.
//
// FENCED ON THE EPOCH. Registration commits to the ledger BEFORE it takes the
// plane's mutex, and expiry holds that mutex while dropping the old entry — so a
// host that restarts quickly could commit its new registration and then be marked
// dead by the expiry of the incarnation it replaced. A no-op once the epoch has
// moved, which is the point.
func (a *Allocator) NodeGone(ctx context.Context, name string, epoch int64) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		// THE FENCE IS READ RATHER THAN LEFT TO THE UPDATE's WHERE CLAUSE, because
		// a second statement now depends on it. The liveness write below no-ops on
		// a stale epoch by itself; the disruption write cannot be expressed against
		// the nodes row, so a superseded incarnation would otherwise mark the
		// leases of the incarnation that replaced it.
		var current int64

		current, err := state.WriteQueries(tx).ReadNodeEpoch(ctx, name)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return nil
		case err != nil:
			return fmt.Errorf("alloc: read the epoch of node %s: %w", name, err)
		}

		if current != epoch {
			return nil
		}

		if err := state.WriteQueries(tx).MarkNodeNotLive(ctx, ledgerdb.MarkNodeNotLiveParams{
			Name:  name,
			Epoch: epoch,
		}); err != nil {
			return fmt.Errorf("alloc: mark node %s gone: %w", name, err)
		}

		// GIVING UP ON A HOST IS AN OBSERVATION ABOUT THE JOBS ON IT, and this is
		// the only place it can be recorded. A lease whose host vanishes does not
		// expire — this control plane goes on renewing it — so it is never
		// quarantined and no inventory ever reports its guest absent. Nothing
		// later can reconstruct the moment either: the host re-registers, live
		// goes back to 1, and the fact is gone.
		//
		// DELIBERATELY NOT DONE BY ForgetEveryNode. That marks the whole fleet
		// unreachable because a control plane has just started and has formed no
		// judgement, which is the opposite of an observation.
		if _, err := markNodeDisruptedTx(
			ctx, tx, name, DisruptionNodeForgotten, a.now()); err != nil {
			return err
		}

		return nil
	})
}

// ForgetEveryNode marks the whole fleet unreachable, for a control plane that has
// just started.
//
// LIVENESS IS THE PLANE'S JUDGEMENT, and a plane that has just started has not
// formed one. Every node re-registers within a poll, so the cost is a brief zero
// that is also the truth.
func (a *Allocator) ForgetEveryNode(ctx context.Context) error {
	return a.db.Tx(ctx, func(tx *sql.Tx) error {
		q := state.WriteQueries(tx)

		if err := q.ForgetEveryNode(ctx); err != nil {
			return fmt.Errorf("alloc: forget the fleet: %w", err)
		}

		// AND WHAT THEY LAST SAID THEY WERE RUNNING. A report received by a
		// PREVIOUS control-plane process says nothing about now, and the node
		// epoch does not move on a plane restart — so without this the record
		// would survive with a matching epoch and read as current to anything
		// looking at it. A plane that has just started has no judgement about any
		// host, which is what the line above already says about liveness.
		if err := q.DeleteEveryNodeInventory(ctx); err != nil {
			return fmt.Errorf("alloc: forget what the fleet reported running: %w", err)
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
	var reaped, quarantined int

	err := a.db.Tx(ctx, func(tx *sql.Tx) error {
		// Reset inside the transaction: a retry or a rollback must not leave a count from a
		// previous attempt.
		reaped, quarantined = 0, 0

		now := a.now().UTC()

		expired, err := readExpiredLeases(ctx, tx, ts(now), reapBatchSize)
		if err != nil {
			return err
		}

		for i := range expired {
			l := &expired[i]

			// A LEASE THAT MAY HAVE A CONTAINER IS NOT TERMINALIZED, it is
			// quarantined: the capacity stays charged to its host until something
			// proves the compute is gone. Escrow nobody launched is a different
			// thing and still ends here.
			to := PhaseFailed
			if hadCompute(l.Phase) {
				to = PhaseQuarantine
			}

			if err := state.WriteQueries(tx).ReclaimLease(ctx, ledgerdb.ReclaimLeaseParams{
				Phase:      string(to),
				Quarantine: string(PhaseQuarantine),
				HeldAt:     ts(now),
				ID:         l.ID,
				Epoch:      l.Epoch,
			}); err != nil {
				return fmt.Errorf("alloc: reap lease %s: %w", l.ID, err)
			}

			if to == PhaseQuarantine {
				// NOT ARCHIVED YET. The lease has not finished; the history row is
				// written when it actually terminalizes, with the outcome it
				// actually had.
				quarantined++

				continue
			}

			if err := a.archive(ctx, tx, l, PhaseFailed); err != nil {
				return err
			}

			reaped++
		}

		return nil
	})

	if err != nil {
		// The transaction rolled back, so nothing was reclaimed. A nonzero count alongside
		// an error invites a caller to believe progress was made and stop retrying.
		return 0, err
	}

	// SAID OUT LOUD, because this is capacity that has NOT come back and an
	// operator watching the advertised number needs to know why. A quarantined
	// lease is waiting for its host to confirm the container is gone; one that
	// never will needs `billet leases release --force`.
	if quarantined > 0 {
		slog.Default().Warn("leases expired with compute that has not been confirmed gone; their "+
			"capacity stays charged until the host says it is destroyed",
			"count", quarantined)
	}

	return reaped, nil
}

// readExpiredLeases is its own function so `defer rows.Close()` is usable: the
// caller runs inside a transaction and issues further statements, so the cursor
// must be closed before it continues.
func readExpiredLeases(ctx context.Context, tx *sql.Tx, cutoff string, limit int) ([]Lease, error) {
	// run_id and request_id are selected because archive needs them: without them a
	// reaped lease lands in job_history with NULL attribution, so the jobs most worth
	// investigating are the ones that lose their identity.
	//
	// Ordered and LIMITed so one transaction cannot hold the single writer connection
	// while it drains an arbitrarily large backlog.
	rows, err := state.WriteQueries(tx).ListExpiredLeases(ctx, ledgerdb.ListExpiredLeasesParams{
		Cutoff:  cutoff,
		MaxRows: int64(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("alloc: find expired leases: %w", err)
	}
	expired := make([]Lease, 0, len(rows))

	for i := range rows {
		row := &rows[i]

		// THE PROVIDER IS CARRIED TOO, because archive copies it: a projection
		// that left it out archived every reaped lease as having run on nothing.
		expired = append(expired, Lease{
			ID:                row.ID,
			Tier:              row.Tier,
			Node:              row.Node.String,
			TargetNode:        row.TargetNode.String,
			MacOSSlot:         row.MacosSlot == 1,
			Provider:          config.ProviderKind(row.ChosenProvider),
			Phase:             Phase(row.Phase),
			VCPU:              int(row.Vcpu),
			Memory:            config.ByteSize(row.Memory),
			RequestedVCPU:     int(row.RequestedVcpu),
			RequestedMemory:   config.ByteSize(row.RequestedMemory),
			InstanceType:      row.InstanceType,
			Site:              row.Site,
			PriceUSDPerHour:   config.USDPerHour(row.PriceMicrosPerHour),
			ImageCache:        ImageCache(row.ImageCache),
			CacheGeneration:   row.CacheGeneration,
			ActionsCache:      ActionsCache(row.ActionsCache),
			HeldSince:         row.HeldAt,
			ForceRelease:      row.ForceRelease == 1,
			HolderIncarnation: row.HolderIncarnation,
			FailureReason:     row.FailureReason,
			Disruption:        Disruption(row.Disruption),
			DisruptedAt:       row.DisruptedAt,
			Epoch:             row.Epoch,
			RunID:             row.RunID.Int64,
			RequestID:         row.RequestID.Int64,
		})
	}

	return expired, nil
}

// Usage reports what is currently held.
func (a *Allocator) Usage(ctx context.Context) (Usage, error) {
	var u Usage

	err := a.db.View(ctx, func(tx querier) error {
		var err error
		u, err = a.usage(ctx, tx)

		return err
	})

	return u, err
}

// ActiveRunnerLeases reports tier capacity assigned to a runner GitHub can still
// route a job to. It includes restart-adopted compute that predates the durable
// pool_runners journal, so aggregate scale reconciliation does not replace a
// runner the node is still holding.
//
// It counts every non-terminal lease EXCEPT one whose GitHub registration has
// been removed (`deregistered`, set when RemoveRunner succeeds). Phase alone
// cannot make this distinction: a completed-job teardown has been deregistered
// and must stop counting, or its lingering compute-destroy retry over-counts
// against the assignment deficit and drops a freshly acquired job; an
// ambiguous-launch custody teardown or a reaped-but-still-registered idle runner
// has NOT been deregistered and must keep counting, or the grow loop launches a
// replacement against a runner GitHub can still schedule — a double-schedule
// whose losing side is also a dropped job. The deregistration signal is what
// separates the two.
func (a *Allocator) ActiveRunnerLeases(ctx context.Context, tier string) (int, error) {
	var count int64

	err := a.db.View(ctx, func(tx querier) error {
		var err error
		count, err = state.ReadQueries(tx).CountActiveRunnerLeases(ctx, tier)

		return err
	})
	if err != nil {
		return 0, fmt.Errorf("alloc: count active runner leases for %s: %w", tier, err)
	}

	return int(count), nil
}

// ServiceableRunnerLeaseIDs reports restart-surviving runner capacity that may
// still serve the scale set. Teardown and quarantine remain charged for safety
// but are cleanup obligations, not capacity GitHub may schedule against.
func (a *Allocator) ServiceableRunnerLeaseIDs(ctx context.Context, tier string) ([]string, error) {
	var ids []string
	err := a.db.View(ctx, func(tx querier) error {
		var err error

		ids, err = state.ReadQueries(tx).ListServiceableRunnerLeaseIDs(ctx, tier)

		return err
	})
	if err != nil {
		return nil, fmt.Errorf("alloc: list serviceable runner leases for %s: %w", tier, err)
	}

	return ids, nil
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
			// Idempotent: a retried transition is not an error — but a REPORT OF A
			// HOLD NAMES ITS HOLDER, and the process reporting a custody or teardown
			// the lease is already in may not be the one that put it there. A
			// restart adopts what it finds and reports the same phase; without this
			// the process that died stays the durable holder while its replacement
			// renews and tends the compute, and `billet leases` calls a live holder
			// replaced. A superseded process still draining reports through here
			// too, and is attributed to the host's current process: the
			// conservative direction, since a lease it keeps tending is held.
			if to == PhaseCustody || to == PhaseTeardown {
				return a.refreshHolder(ctx, tx, lease)
			}

			// A REPEATED BUSY REPORT STILL LOOKS BACK, because a reason may have
			// landed between the first report and this one.
			if to == PhaseBusy {
				return a.recordJobStartTx(ctx, tx, leaseID, a.now().UTC())
			}

			return nil
		}

		now := a.now().UTC()
		heldAt := lease.HeldSince
		holder := lease.HolderIncarnation
		if to == PhaseCustody || to == PhaseTeardown {
			if heldAt == "" {
				heldAt = ts(now)
			}

			// THE PROCESS TAKING THE OBLIGATION IS RECORDED AS ITS HOLDER, and it
			// need not be the one that launched the compute: a restart adopts what
			// it finds. Its name is the node's CURRENT incarnation. A superseded
			// process draining its custody writes the replacement's here, which
			// makes a report say the holder was NOT replaced — the conservative
			// reading, since a draining process is a live holder. A host the
			// ledger never registered records nothing, which reports as unknown.
			current, err := state.WriteQueries(tx).ReadNodeIncarnation(ctx, lease.Node)

			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return fmt.Errorf("alloc: read the incarnation of node %s: %w", lease.Node, err)
			default:
				holder = current
			}
		}

		if err := state.WriteQueries(tx).HoldLease(ctx, ledgerdb.HoldLeaseParams{
			Phase:             string(to),
			HeldAt:            heldAt,
			HeartbeatAt:       ts(now),
			ExpiresAt:         ts(now.Add(a.leaseTTL)),
			HolderIncarnation: holder,
			ID:                leaseID,
			Epoch:             epoch,
		}); err != nil {
			return fmt.Errorf("alloc: advance lease %s: %w", leaseID, err)
		}

		// A JOB STARTED, AND THE LEDGER KEEPS THAT PAST THE PHASE. Custody and
		// teardown forget whether the lease was ever busy, and a reason a node
		// records later may claim the launch never ran; started_at is what
		// contradicts it (see disruptionFor). A lease with no history row ran
		// no job GitHub told billet about, and the update matches nothing.
		if to == PhaseBusy {
			if err := a.recordJobStartTx(ctx, tx, leaseID, now); err != nil {
				return err
			}
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

// recordJobStartTx records that GitHub started a job on a lease, once, and
// repairs a token the start contradicts.
//
// THE START AND A REASON ARE NOT ORDERED. GitHub's message stream and the node
// wire are independent, so a node can record `launch-failed` for a lease
// after the job has begun but before this control plane processes the
// JobStarted that says so; markFailureTx then finds no start and writes no
// token. So the start looks back: a reason on the lease that claims no job ran
// is now contradicted, and the token an external reclaim must carry is written
// here, in the transaction that learns the job started. Callers run this on
// the idempotent edges too — a repeated busy report, a pool member already
// busy with the same job — because the reason may have landed between two of
// them. The guard behind markLeaseDisruptedTx keeps a token already written
// and refuses one for a job GitHub has already reported on.
func (a *Allocator) recordJobStartTx(ctx context.Context, tx *sql.Tx, leaseID string, now time.Time) error {
	q := state.WriteQueries(tx)

	if err := q.RecordJobStart(ctx, ledgerdb.RecordJobStartParams{
		StartedAt: sql.NullString{String: ts(now), Valid: true},
		LeaseID:   leaseID,
	}); err != nil {
		return fmt.Errorf("alloc: record the job start of lease %s: %w", leaseID, err)
	}

	settlement, err := q.ReadLeaseSettlement(ctx, leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("alloc: read the reason of lease %s at its job start: %w", leaseID, err)
	}

	if token, disrupted := disruptionFor(settlement.FailureReason, true); disrupted &&
		(settlement.FailureReason == LaunchFailedReason || settlement.FailureReason == RunnerRetiredReason) {
		if _, err := markLeaseDisruptedTx(ctx, tx, leaseID, token, now); err != nil {
			return err
		}
	}

	return nil
}

// refreshHolder re-records the host's current process as a held lease's holder.
func (a *Allocator) refreshHolder(ctx context.Context, tx *sql.Tx, lease *Lease) error {
	if lease.Node == "" {
		return nil
	}

	current, err := state.WriteQueries(tx).ReadNodeIncarnation(ctx, lease.Node)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("alloc: read the incarnation of node %s: %w", lease.Node, err)
	}

	if current == "" || current == lease.HolderIncarnation {
		return nil
	}

	if err := state.WriteQueries(tx).RefreshLeaseHolder(ctx, ledgerdb.RefreshLeaseHolderParams{
		HolderIncarnation: current,
		ID:                lease.ID,
		Epoch:             lease.Epoch,
	}); err != nil {
		return fmt.Errorf("alloc: re-record the holder of lease %s: %w", lease.ID, err)
	}

	return nil
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
func (a *Allocator) loadAny(ctx context.Context, tx querier, leaseID string, epoch int64) (*Lease, error) {
	row, err := state.ReadQueries(tx).ReadLease(ctx, leaseID)

	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: %s", ErrLeaseNotFound, leaseID)
	case err != nil:
		return nil, fmt.Errorf("alloc: load lease %s: %w", leaseID, err)
	}

	// THE FENCE IS CHECKED BEFORE ANYTHING ELSE IS BELIEVED ABOUT THE ROW. A
	// holder declared dead and replaced must not keep operating on a lease
	// somebody else now owns.
	if row.Epoch != epoch {
		return nil, fmt.Errorf("%w: lease %s is at epoch %d, caller holds %d",
			ErrFenced, leaseID, row.Epoch, epoch)
	}

	return &Lease{
		ID:                row.ID,
		Tier:              row.Tier,
		Node:              row.Node.String,
		TargetNode:        row.TargetNode.String,
		MacOSSlot:         row.MacosSlot == 1,
		GuestOS:           config.GuestOS(row.GuestOs),
		Providers:         decodeProviders(row.Providers),
		Provider:          config.ProviderKind(row.ChosenProvider),
		Phase:             Phase(row.Phase),
		VCPU:              int(row.Vcpu),
		Memory:            config.ByteSize(row.Memory),
		RequestedVCPU:     int(row.RequestedVcpu),
		RequestedMemory:   config.ByteSize(row.RequestedMemory),
		InstanceType:      row.InstanceType,
		Site:              row.Site,
		PriceUSDPerHour:   config.USDPerHour(row.PriceMicrosPerHour),
		ImageCache:        ImageCache(row.ImageCache),
		CacheGeneration:   row.CacheGeneration,
		ActionsCache:      ActionsCache(row.ActionsCache),
		HeldSince:         row.HeldAt,
		ForceRelease:      row.ForceRelease == 1,
		HolderIncarnation: row.HolderIncarnation,
		FailureReason:     row.FailureReason,
		Disruption:        Disruption(row.Disruption),
		DisruptedAt:       row.DisruptedAt,
		Epoch:             row.Epoch,
		RunID:             row.RunID.Int64,
		RequestID:         row.RequestID.Int64,
	}, nil
}

func (a *Allocator) usage(ctx context.Context, tx querier) (Usage, error) {
	row, err := state.ReadQueries(tx).TotalUsage(ctx)
	if err != nil {
		return Usage{}, fmt.Errorf("alloc: read usage: %w", err)
	}

	return Usage{
		VCPU:   int(row.Vcpu),
		Memory: config.ByteSize(row.Memory),
		Leases: int(row.Leases),
	}, nil
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

	err := a.db.View(ctx, func(tx querier) error {
		var epoch int64

		epoch, err := state.ReadQueries(tx).ReadLeaseEpoch(ctx, leaseID)

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

// JobForLease reads the job identity assigned to a lease, including a terminal one.
//
// GitHub's completed-job message may omit runnerRequestId while still naming the
// ephemeral runner. The runner name carries the lease id, and this durable lookup
// recovers the request id without trusting an in-memory ownership map that a restart
// has erased. Terminal rows remain readable because an unacknowledged completion can
// be redelivered after teardown and release already settled.
func (a *Allocator) JobForLease(ctx context.Context, leaseID string) (LeaseJob, error) {
	var job LeaseJob

	err := a.db.View(ctx, func(tx querier) error {
		var runID, requestID sql.NullInt64

		row, err := state.ReadQueries(tx).ReadLeaseJob(ctx, leaseID)

		job.Tier, runID, requestID = row.Tier, row.RunID, row.RequestID

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s", ErrLeaseNotFound, leaseID)
		case err != nil:
			return fmt.Errorf("alloc: read job identity for lease %s: %w", leaseID, err)
		}

		job.RunID = runID.Int64
		job.RequestID = requestID.Int64

		return nil
	})

	return job, err
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
// LAUNCHING, ONLINE, BUSY, CUSTODY and TEARDOWN — not merely "not terminal". A lease in the
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

	err := a.db.View(ctx, func(tx querier) error {
		rows, err := state.ReadQueries(tx).ListLeaseIDsOnNode(ctx,
			ledgerdb.ListLeaseIDsOnNodeParams{
				Node:      sql.NullString{String: node, Valid: true},
				Launching: string(PhaseLaunching),
				Online:    string(PhaseOnline),
				Busy:      string(PhaseBusy),
				Custody:   string(PhaseCustody),
				Teardown:  string(PhaseTeardown),
			})
		if err != nil {
			return fmt.Errorf("alloc: list open leases on %s: %w", node, err)
		}

		for _, id := range rows {
			open[id] = true
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return open, nil
}

// checkMacOSFloors reports macOS reservations a host's licence cap can never
// satisfy.
func (a *Allocator) checkMacOSFloors() error {
	reservedByNode := make(map[string]int)

	labels := make([]string, 0, len(a.tiers))
	for label := range a.tiers {
		labels = append(labels, label)
	}

	sort.Strings(labels)

	for _, label := range labels {
		t := a.tiers[label]

		if t.GuestOS != config.GuestMacOS || t.Reserved == 0 {
			continue
		}

		// A macOS tier is required to name a node, checked above, so this is
		// always a real host.
		reservedByNode[t.Node] += t.Reserved

		if limit := a.macOSLimit(t.Node); reservedByNode[t.Node] > limit {
			return fmt.Errorf(
				"alloc: macOS tiers on node %q reserve %d guests between them but the host "+
					"permits %d; the surplus could never be filled and would hold vCPU and "+
					"memory back from every other tier for as long as billet runs",
				t.Node, reservedByNode[t.Node], limit)
		}
	}

	return nil
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

// countOpenPerTier reports how many non-terminal leases each tier holds ON A
// MACHINE THAT CAN STILL SERVE THEM.
//
// THE SAME DEFINITION OF A USABLE HOST THAT PLACEMENT USES, and the two
// disagreeing is what made a floor stop being a promise. Placement asks for
// `live = 1 AND drained = 0`, so a machine that is gone offers nothing — while
// this count asked only whether a lease was non-terminal, wherever it was aimed.
// Escrow stranded on a vanished host therefore read as "the reservation is
// already met", nothing was held back for it, and another tier was offered the
// last machine that could have served it. The stranded lease is then released,
// and the reserved tier has none of its promised slots and nowhere to put one.
//
// A lease aimed at NO machine still counts. There is nothing to prove it
// stranded, and treating it as unmet would reserve room on top of leases that
// are perfectly fine.
func (a *Allocator) countOpenPerTier(ctx context.Context, tx querier) (map[string]int, error) {
	rows, err := state.ReadQueries(tx).CountOpenPerTier(ctx)
	if err != nil {
		return nil, fmt.Errorf("alloc: count open leases per tier: %w", err)
	}

	held := make(map[string]int, len(a.tiers))

	for _, row := range rows {
		held[row.Tier] = int(row.OpenLeases)
	}

	return held, nil
}

func (a *Allocator) countOpenByTier(ctx context.Context, tx querier, tier string) (int, error) {
	n, err := state.ReadQueries(tx).CountOpenInTier(ctx, tier)
	if err != nil {
		return 0, fmt.Errorf("alloc: count tier %s: %w", tier, err)
	}

	return int(n), nil
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

	err := a.db.View(ctx, func(tx querier) error {
		// NOT NULL, because a row is inserted at ASSIGNMENT with no conclusion and
		// only filled in when the lease terminalizes. A job in flight is not an
		// outcome, and scanning one into a string fails outright.
		rows, err := state.ReadQueries(tx).ListJobConclusionsForRequest(ctx,
			sql.NullInt64{Int64: requestID, Valid: true})
		if err != nil {
			return fmt.Errorf("alloc: read job history for request %d: %w", requestID, err)
		}

		for _, conclusion := range rows {
			outcomes = append(outcomes, conclusion.String)
		}

		return nil
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

	err := a.db.View(ctx, func(tx querier) error {
		row, err := state.ReadQueries(tx).ReadJobConclusion(ctx, leaseID)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: %s has not been archived", ErrLeaseNotFound, leaseID)
		case err != nil:
			return fmt.Errorf("alloc: read job history for %s: %w", leaseID, err)
		}

		// A NULL CONCLUSION IS "HAS NOT BEEN ARCHIVED" TOO, and it is the same
		// answer as no row at all. The history row is opened at ASSIGNMENT and the
		// conclusion is written only when the lease terminalizes, so a job in
		// flight has a row and no verdict.
		//
		// SAID EXPLICITLY BECAUSE THE CONVERSION CHANGED WHAT SILENCE LOOKS LIKE.
		// The hand-written scan took this column into a plain string, so a NULL
		// FAILED -- loudly and unhelpfully, but it failed. Generated code reads a
		// sql.NullString, so without this the caller would be handed "" beside a
		// nil error and could not tell an unfinished job from an empty verdict.
		if !row.Valid {
			return fmt.Errorf("%w: %s has not been archived", ErrLeaseNotFound, leaseID)
		}

		conclusion = row.String

		return nil
	})
	if err != nil {
		return "", err
	}

	return conclusion, nil
}

// EndedLeaseNode reports the host a lease's job was attributed to, from the
// history row that outlives the lease.
//
// WHAT AUTHORISES A REGISTRATION REMOVAL FOR A LEASE THAT IS OVER. The lease
// row's placement is gone once it terminalizes, and membership alone would let
// any registered node name another host's ended lease and withdraw its runner;
// the history row keeps the attribution. Empty when the job was never
// attributed to a host, which a caller must treat as no permission.
func (a *Allocator) EndedLeaseNode(ctx context.Context, leaseID string) (string, error) {
	var node string

	err := a.db.View(ctx, func(tx querier) error {
		row, err := state.ReadQueries(tx).ReadJobNode(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s has no job history", ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read the host of lease %s: %w", leaseID, err)
		}

		node = row.String

		return nil
	})

	return node, err
}

// HistoryFailureReason reports the durable explanation attached to a finished
// lease. An empty string means no external failure was recorded.
func (a *Allocator) HistoryFailureReason(ctx context.Context, leaseID string) (string, error) {
	var reason string

	err := a.db.View(ctx, func(tx querier) error {
		var err error

		reason, err = state.ReadQueries(tx).ReadJobFailureReason(ctx, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: %s has not been archived", ErrLeaseNotFound, leaseID)
		}
		if err != nil {
			return fmt.Errorf("alloc: read job history reason for %s: %w", leaseID, err)
		}

		return nil
	})

	return reason, err
}

// archive copies a finished lease into job_history before its row stops being
// interesting, so "why did this queue" is answerable after the fact.
func (a *Allocator) archive(ctx context.Context, tx *sql.Tx, l *Lease, outcome Phase) error {
	now := a.now().UTC()

	node := attribution(l)

	// COALESCE on update, so terminalizing never erases what assignment recorded.
	// A caller that does not select the ids — Reap — would otherwise arrive with
	// NULLs and wipe real attribution from the leases most worth investigating.
	// A DISRUPTION IS NEVER ERASED, the same rule node/run_id/request_id already
	// follow here and for the same reason: not every caller loads the columns, and
	// an archive that arrives without them must not wipe an observation an earlier
	// one recorded. The two move together, keyed on the token, so a report can
	// never show a time with nothing to attribute it to. The cache observations
	// follow the same rule.
	//
	// THE PLACEMENT FACTS COME FROM THE LEASE, never from a catalogue: the price
	// is what the row recorded when the shape was charged, and the row is what
	// is about to be reaped.
	err := state.WriteQueries(tx).ArchiveJobHistory(ctx, ledgerdb.ArchiveJobHistoryParams{
		LeaseID:            l.ID,
		Tier:               l.Tier,
		Node:               node,
		RunID:              nullableID(l.RunID),
		RequestID:          nullableID(l.RequestID),
		Conclusion:         sql.NullString{String: string(outcome), Valid: true},
		FailureReason:      l.FailureReason,
		Disruption:         string(l.Disruption),
		DisruptedAt:        l.DisruptedAt,
		ChosenProvider:     string(l.Provider),
		InstanceType:       l.InstanceType,
		Vcpu:               int64(l.VCPU),
		Memory:             int64(l.Memory),
		Site:               l.Site,
		PriceMicrosPerHour: int64(l.PriceUSDPerHour),
		ImageCache:         string(l.ImageCache),
		CacheGeneration:    l.CacheGeneration,
		ActionsCache:       string(l.ActionsCache),
		QueuedAt:           ts(now),
		FinishedAt:         sql.NullString{String: ts(now), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("alloc: archive lease %s: %w", l.ID, err)
	}

	return nil
}

func nullableID(v int64) sql.NullInt64 {
	if v == 0 {
		return sql.NullInt64{}
	}

	return sql.NullInt64{Int64: v, Valid: true}
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

// ErrAdmissionSealed means the deployment is not accepting new work.
//
// A DISTINCT ERROR, not ErrNoCapacity, because they mean opposite things to a
// caller: no capacity is a transient fact about a full fleet that the next poll
// may find changed, while a seal is a decision somebody made and only somebody
// can undo. A listener that logged "no capacity" while an operator was draining
// would send them looking at their hardware.
var ErrAdmissionSealed = errors.New("alloc: this deployment is not accepting new work")

// sealedError says who sealed the deployment and when, because the operator
// reading it is often not the one who sealed it.
//
// IT WRAPS THE SENTINEL rather than reproducing its text. Building the message
// with errors.New leaves a string that READS correctly and that errors.Is cannot
// match, so every caller distinguishing a seal from a full fleet would silently
// fall through to the wrong branch.
func sealedError(a state.Admission) error {
	if a.Mode == state.AdmissionUnknown {
		return fmt.Errorf("%w: billet could not read the admission state, and will not admit "+
			"work against a state it cannot read", ErrAdmissionSealed)
	}

	var detail string

	if a.Actor != "" {
		detail += fmt.Sprintf(", sealed by %s", a.Actor)
	}
	if a.ChangedAt != "" {
		detail += fmt.Sprintf(" at %s", a.ChangedAt)
	}
	if a.Reason != "" {
		detail += fmt.Sprintf(" (%s)", a.Reason)
	}

	return fmt.Errorf("%w%s", ErrAdmissionSealed, detail)
}

// Admission reports whether the deployment is accepting new work.
//
// On the read-only pool: a status command must not reserve the single writer
// slot to answer a question, and this one is asked by every operator wondering
// why their jobs are queueing.
func (a *Allocator) Admission(ctx context.Context) (state.Admission, error) {
	return a.db.Admission(ctx)
}

// refuseIfSealed reads the deployment's admission state and refuses new work.
//
// A LEDGER BILLET COULD NOT READ IS NOT ONE THAT SAID YES. Failing closed here
// costs an escrow the next poll retries; failing open would admit work into a
// deployment somebody sealed to stop exactly that.
func refuseIfSealed(ctx context.Context, tx *sql.Tx) error {
	admission, err := state.ReadAdmission(ctx, tx)
	if err != nil {
		return fmt.Errorf("%w: billet could not read whether this deployment is "+
			"accepting work: %w", ErrAdmissionSealed, err)
	}

	if admission.Sealed() {
		return sealedError(admission)
	}

	return nil
}
