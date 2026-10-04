// Package lease is the vocabulary a lease is described in: its phases, the
// lease record itself, and what a node observed a lease's job do (the cache
// outcomes, billet's own disruptions, the job's measured usage).
//
// THE WORDS, NOT THE RULES. The state machine that decides which phase may
// follow which, placement, and every write stay in internal/alloc, which is the
// ledger's; this package holds what both roles and the node wire need to name a
// lease without importing the ledger writer. It imports nothing of billet's but
// internal/config.
package lease

import (
	"errors"

	"github.com/junioryono/billet/internal/config"
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

// Terminal reports whether a phase releases capacity.
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseFailed }

// ErrLeaseNotFound means the lease does not exist, or is already terminal.
//
// THE MESSAGE KEEPS ITS alloc: PREFIX: callers have matched it by errors.Is
// since the ledger first returned it, and a report quoting it should read the
// same after the move.
var ErrLeaseNotFound = errors.New("alloc: lease not found")

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
	// BuildCaches is what the node observed each build cache do for this job.
	BuildCaches
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
	// by alloc's Heartbeat as alloc.ErrForceRelease rather than acted on behind
	// the node's back.
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
