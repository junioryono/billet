// Package provider launches and destroys the compute a job runs on.
//
// The interface is deliberately NARROW, and that is a decision rather than an
// oversight. The plan is explicit that a real contract eventually needs more
// than launch and destroy — readiness and boot timeout, console diagnostics,
// idempotency keys and post-restart reconciliation, graceful stop against forced
// kill, network policy, volume attach and quiesce, image preparation, spot
// interruption, capability negotiation. All of that is true and none of it is
// here, because every one of those shapes is a guess until a second backend
// needs it. EC2 landed as that second backend and appeared to force NO change to
// this interface. That reading was wrong rather than merely early: it did force
// one, and Destroy carries it now. A teardown that has been REQUESTED is not a
// guest that has STOPPED, and every caller assumed the latter because the only
// backend in existence when they were written could promise it. The prediction
// that a second backend would widen this was right; what was wrong was
// concluding otherwise before anything had ever run on it.
//
// What IS here is the part every backend must agree on: launch one instance for
// one job, destroy it, and never let the credential reach a place it can be
// read.
package provider

import (
	"context"
	"strconv"

	"github.com/junioryono/billet/internal/config"
)

// MaxVolumes is the maximum number of cache disks one job can attach.
const MaxVolumes = 5

// VolumeSlotID is the stable Firecracker drive id and in-jail path for one slot.
func VolumeSlotID(slot int) string { return "cache" + strconv.Itoa(slot) }

// VolumeMount is a block device a backend attaches to one instance.
type VolumeMount struct {
	// Device is the mapped host block-device path.
	Device string
	// Path is where the cooperative guest mounts it.
	Path string
}

// VolumeAttacher replaces pre-reserved block-device slots on a running instance.
type VolumeAttacher interface {
	AttachVolume(ctx context.Context, instanceID string, slot int, device string) error
	DetachVolume(ctx context.Context, instanceID string, slot int, device string) error
}

// GuestVolumeLocator translates a storage handle into the stable device name a
// guest sees. Firecracker has fixed virtio slots; EC2's NVMe name follows the
// EBS volume identity instead of the attachment name requested from its API.
type GuestVolumeLocator interface {
	GuestVolumeDevice(slot int, device string) string
}

// TrustClass says how much authority a configured runner pool receives, which
// decides what may run it. The zero value is UNKNOWN and every backend refuses it.
type TrustClass int

const (
	// TrustUnknown is the zero value, and it fails closed. A caller that has not
	// classified a job has not established it is safe to run anywhere weak.
	TrustUnknown TrustClass = iota
	// TrustUntrusted is a pool built to admit arbitrary code. It requires a real
	// isolation boundary.
	TrustUntrusted
	// TrustTrusted is a pool whose non-default GitHub runner group is restricted
	// to the exact workflows its operator declared.
	TrustTrusted
)

func (t TrustClass) String() string {
	switch t {
	case TrustUntrusted:
		return "untrusted"
	case TrustTrusted:
		return "trusted"
	case TrustUnknown:
		return "unknown"
	default:
		return "unknown"
	}
}

// Spec is one instance to launch.
type Spec struct {
	// Name is BILLET's handle for the instance, and it must encode the lease.
	//
	// Not GitHub's runner name, which is a different thing that can differ. This
	// is what reconciliation reads back: after a crash the only surviving link
	// between a running instance and the lease that authorised it is the name the
	// instance carries, so a name that does not encode the lease makes an orphan
	// unattributable. See InstanceName.
	Name string
	// Image is the tier's image reference, interpreted by the backend: a
	// container image for docker, a rootfs for firecracker, an AMI for ec2.
	Image string

	VCPU   int
	Memory config.ByteSize
	// InstanceType is the EC2 shape selected and charged at escrow. Empty for
	// host-backed providers.
	InstanceType string
	// AuthorizeShape atomically changes that charge before EC2 attempts a
	// fallback. ok=false means the shape would exceed a budget; err means the
	// ledger could not make the decision and the launch must stop.
	AuthorizeShape func(context.Context, string, int, config.ByteSize) (ok bool, err error)
	// Disk is the root volume capacity. Firecracker grows its per-job RBD clone
	// and ext4 filesystem to at least this size; EC2 requests it from EBS. Zero
	// means the backend's image default. Host Docker ignores it.
	Disk config.ByteSize
	// SHM sizes /dev/shm. It is a tier parameter rather than a constant because
	// Postgres service containers and Chromium both fail on the default 64MB in
	// ways that look like unrelated crashes.
	SHM config.ByteSize
	// Volumes are cache devices known before boot. A cache endpoint reserves the
	// remaining slots so a cooperative guest can request them at runtime.
	Volumes []VolumeMount
	// CacheEndpoint and CacheToken let the managed guest request runtime volumes
	// from its node. The token is a session identity, not a boundary within the
	// guest: workflow code has passwordless sudo and Docker-root equivalence.
	CacheEndpoint string
	CacheToken    string
	// ActionsProxy and ActionsCAPEM opt this guest into the authenticated results
	// proxy. Both must be present or absent together.
	ActionsProxy string
	ActionsCAPEM string
	// BuildKitCacheMountLimit is the tier's byte ceiling for each persistent
	// BuildKit cache-mount record. It is meaningful only with CacheEndpoint.
	BuildKitCacheMountLimit config.ByteSize
	// RegistryMirrors are site-local public pull-through caches made available to
	// a managed guest. The zero value sends the guest directly upstream.
	RegistryMirrors config.RegistryMirrors

	// Command starts the runner inside the instance.
	//
	// REQUIRED, and backends refuse an empty one, for the same reason Trust's zero
	// value is refused: the alternative is not a failure but a SUCCESS that does
	// nothing. A container image's default command is usually a shell, so
	// launching without this starts a container that exits immediately — the CLI
	// reports success, billet logs a started runner, and the job sits queued
	// forever. Found on the first job billet was ever given.
	//
	// A []string rather than a string so no backend has to guess at word
	// splitting, and so an image needing arguments does not have to smuggle them
	// through a shell.
	Command []string

	// Trust is how much this workload is trusted. The zero value is UNKNOWN and
	// backends refuse it, which is what makes an unclassified job impossible to
	// route somewhere weak by omission rather than by decision.
	Trust TrustClass

	// JITConfig is the single-use runner registration.
	//
	// IT IS A CREDENTIAL until the runner consumes it, and it is the reason this
	// struct is passed by value to a method rather than assembled into a command
	// line by the caller. A backend MUST NOT put it in argv, where every process
	// on the host can read it, and MUST NOT log it. Both mistakes look like
	// working code.
	JITConfig string
}

// Instance is a unit of compute the backend knows about. It may be running,
// stopped, or retained as a terminal record.
type Instance struct {
	// ID is the backend's own handle — a container id, a microVM id, an EC2
	// instance id. Opaque to everything above.
	ID string
	// Name echoes the spec, so a caller holding only an Instance can still say
	// which runner it is.
	Name string

	// Running reports whether the instance is still executing.
	//
	// The difference between "this job is in progress" and "this job is over and
	// the container is a corpse holding a name and a disk", which is exactly the
	// question an adopted instance has to be asked repeatedly. A backend that
	// cannot tell should report true: treating an unknown state as finished would
	// destroy live work, and treating it as running only delays a cleanup.
	Running bool

	// Terminal reports that the backend positively observed this compute reach a
	// state from which it cannot execute or return to running. It is stronger than
	// !Running: a stopped instance is not executing but still exists and may retain
	// disks. The zero value is deliberately not proof.
	Terminal bool
}

// InterruptionNotice is an external warning that a provider will take compute
// away. Receipt is opaque acknowledgement state and must never be logged.
type InterruptionNotice struct {
	InstanceID string
	Action     string
	Receipt    string
	// Problem is set when the queue delivered something that was not a usable
	// interruption event. It is safe diagnostic prose, never the message body.
	Problem string
}

// InterruptionSource is implemented by a backend that can observe external
// reclaim warnings. It is deliberately optional: host-backed providers have no
// remote service that can take their compute away.
type InterruptionSource interface {
	NextInterruption(ctx context.Context) (*InterruptionNotice, error)
	AcknowledgeInterruption(ctx context.Context, notice *InterruptionNotice) error
}

// StagedCredentialReaper is implemented by a backend that stages a runner
// registration OUTSIDE the compute it launches, and therefore has something to clean
// up that destroying the compute does not remove.
//
// OPTIONAL, because most backends have nothing to reap. A docker or firecracker guest
// receives its registration directly and a terminated EC2 instance takes its user data
// with it; a CodeBuild build cannot be handed one at all — `StartBuild` has no field
// for a secret — so its registration lives in Parameter Store and outlives the build.
//
// THE CALLER SUPPLIES THE PROOF, which is the whole reason this is a separate contract
// rather than something the provider infers. Deleting a staged registration before the
// compute is finished with it produces a runner that never registers and a job that
// queues until GitHub gives up, and a provider asking "is that build done" on its own
// can only answer from an inventory that is eventually consistent.
//
// THE PRECONDITION IS THE PROOF, NOT A PARTICULAR CALL SITE, and this comment named a
// call site until a second one was added — which is how a rule whose stated reason has
// gone stale becomes worse than no rule. Billet may call this wherever it has already
// established that the lease's compute is gone. Today that is two places, and both
// hold an explicit terminal record or a confirmed destroy: custody settlement, and the
// failed-launch cleanup that confirms on such a record without needing a destroy at
// all. Anywhere else is a caller inferring, which is the thing this contract exists to
// keep out of the provider.
//
// IT MUST BE IDEMPOTENT. The ordinary teardown path already removes the credential when
// it confirms a build terminal, so this is usually a second call about something that
// is already absent, and an absent credential is success.
type StagedCredentialReaper interface {
	ReapStagedCredential(ctx context.Context, instanceName string) error
}

// InstanceName is billet's handle for the compute backing a lease.
//
// Derived rather than stored, and that is the whole trick: it means a running
// instance can be matched back to its lease with nothing but its own name, so
// reconciliation after a crash needs no durable side table and no schema change.
// The lease id is unique, so the name is too.
func InstanceName(leaseID string) string { return "billet-" + leaseID }

// LeaseOf reverses InstanceName, and reports whether the name was billet's.
func LeaseOf(instanceName string) (string, bool) {
	const prefix = "billet-"

	if len(instanceName) <= len(prefix) || instanceName[:len(prefix)] != prefix {
		return "", false
	}

	return instanceName[len(prefix):], true
}

// Teardown says how far a Destroy actually got.
//
// THE DISTINCTION EXISTS BECAUSE ONE BACKEND CANNOT MAKE THE PROMISE THE OTHERS
// CAN. `docker rm --force` returns when the container is gone, so its caller may
// treat a successful Destroy as proof. EC2 cannot: TerminateInstances returns
// when the request is accepted, while an idempotent NotFound may be an eventually
// consistent miss. Neither confirms the guest is gone.
//
// Callers used to read every Destroy the docker way, and the consequence was not
// money. Destroy is reached on paths where the guest is still working — a drain,
// a custody teardown, an operator killing a job — and the listener releases the
// lease on success. So a new job could start while the old guest was still
// finishing a deploy or a migration: two concurrent effects on something outside
// billet, which is worse than the over-commit the destroy-then-release ordering
// was written to prevent, and not bounded by anything.
//
// THE ZERO VALUE IS THE SAFE ONE, deliberately, exactly as TrustClass's is. A
// backend that forgets to say, or a new one written against this interface
// without reading it, reports "I could not prove it stopped" — and the caller
// holds the capacity until something else proves it. The opposite default frees
// capacity for a guest that is still running, and nothing recovers from that.
type Teardown int

const (
	// TeardownRequested means the backend requested teardown and the compute may
	// still be running. It is also the idempotent answer when the backend cannot
	// currently see the target. Capacity stays charged until a sustained absence
	// or another causal result proves the compute is gone.
	TeardownRequested Teardown = iota
	// TeardownStopped means the compute is CONFIRMED gone. Only a backend whose
	// teardown is synchronous may return this.
	TeardownStopped
)

func (t Teardown) String() string {
	switch t {
	case TeardownStopped:
		return "stopped"
	case TeardownRequested:
		return "requested"
	default:
		return "requested"
	}
}

// Provider launches and destroys the compute for one job at a time.
type Provider interface {
	// Accepts reports whether this backend may run work of that trust class.
	//
	// Separate from Launch so a caller can ask BEFORE doing anything expensive or
	// irreversible. Minting a runner registration and then being refused leaves
	// that registration on GitHub with nothing to consume it — one orphan per
	// pull request, accumulating quietly.
	Accepts(trust TrustClass) error

	// Kind reports which backend this is. Placement compares it against what a
	// lease requires, so a Firecracker lease cannot land on a Tart host.
	Kind() config.ProviderKind

	// Launch starts one instance running the job its JIT config names.
	//
	// It returns when the instance has been CREATED, not when the runner inside
	// it is ready. Readiness is a separate question with a separate timeout, and
	// conflating them is how a slow image pull becomes a launch failure.
	Launch(ctx context.Context, spec Spec) (*Instance, error)

	// Find reports the instance with that name, and whether there was one.
	//
	// This is what makes a failed launch answerable rather than guessed at. An
	// error from Launch does not prove nothing started — a cancelled context can
	// kill the CLI after the daemon accepted the request, and a remote API can
	// commit and lose the response — so the only honest way to find out is to
	// ask. Retrying instead is how one job becomes two runners.
	//
	// The bool is explicit rather than a nil pointer, because the caller's next
	// move on a hit is to DESTROY: "there is nothing here" and "something went
	// wrong and you got a zero value" must not look alike at a call site with
	// that consequence.
	Find(ctx context.Context, name string) (*Instance, bool, error)

	// List reports every instance this backend is running for billet.
	//
	// The input to reconciliation: anything here whose lease is no longer open is
	// an orphan, and orphans are the residue of every crash between starting an
	// instance and recording it.
	List(ctx context.Context) ([]*Instance, error)

	// Destroy removes an instance and everything it owns.
	//
	// MUST be idempotent: destroying an id that is already gone is success, not
	// an error. Teardown runs on paths that have already failed once, and an
	// error there turns a recoverable state into a stuck one.
	//
	// The Teardown says whether the compute is CONFIRMED gone or merely asked to
	// go, and the two are not the same fact — see Teardown. A backend must not
	// report TeardownStopped on the strength of an API accepting the request, and
	// must not report it on the strength of an absence its own service is allowed
	// to be wrong about.
	Destroy(ctx context.Context, id string) (Teardown, error)
}

// Quota is a ceiling billet does not own, and cannot raise.
//
// EXTERNAL, WHICH IS WHY IT IS REPORTED RATHER THAN ENFORCED. A tier's own
// capacity is billet's arithmetic and the allocator holds it exactly; an account
// limit is somebody else's, is raised by a support request rather than a config
// change, and can move without billet hearing about it. What billet owes an
// operator is the sentence before the first job, not a gate afterwards.
type Quota struct {
	// Name is what the vendor calls it, and Code is the identifier an operator
	// types into a console or a support request. Both are carried so a report is
	// actionable without translating billet's wording back into the vendor's.
	Name string
	Code string
	// Limit and Unit are the ceiling and what it counts.
	Limit float64
	Unit  string
	// Scope says, in billet's own terms, what the limit is about — "concurrent
	// builds of BUILD_GENERAL1_MEDIUM", "running on-demand instances" — so a
	// reader can see which of their settings it bounds.
	Scope string
	// Shape names the declared shape this limit is about, empty for one that is
	// not about a shape at all.
	//
	// THE COMPARISON IS NOT MADE HERE, and this field is why. A backend knows
	// the vendor's codes; it does not know the deployment's budget —
	// node.max_vcpu and node.max_memory are NodeConfig's, not the backend
	// block's — so working out how many of a shape that budget could escrow is
	// billet's own arithmetic and belongs where the whole config is visible.
	// Naming the shape is what lets `billet check` do it without the backend
	// having to know.
	Shape string
}

// QuotaReporter is implemented by a backend whose account imposes ceilings
// billet can read.
//
// AN OPTIONAL CAPABILITY, DELIBERATELY, and the rule from billet-capacity
// applies: an optional capability may not carry a safety invariant. Nothing
// here gates a launch, an escrow or an admission — a backend that does not
// implement it simply has nothing to report, which is the honest answer for
// every host-run backend, whose ceiling is the machine and is already measured.
type QuotaReporter interface {
	// Quotas reports the external ceilings this node's configuration runs
	// against.
	//
	// IT MAY RETURN BOTH a partial slice AND an error, and callers print both:
	// several lookups make several calls, and one that failed must not discard
	// the ones that answered. That is the same shape `billet check` already uses
	// for its advisory probes.
	Quotas(ctx context.Context) ([]Quota, error)
}
