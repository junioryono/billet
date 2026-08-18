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
// guest that has STOPPED (#46), and every caller assumed the latter because the
// only backend in existence when they were written could promise it. The
// prediction that a second backend would widen this was right; what was wrong
// was concluding otherwise before anything had ever run on it.
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

// TrustClass says how much the workload is trusted, which decides what may run
// it. The zero value is UNKNOWN and every backend must treat it as untrusted.
type TrustClass int

const (
	// TrustUnknown is the zero value, and it fails closed. A caller that has not
	// classified a job has not established it is safe to run anywhere weak.
	TrustUnknown TrustClass = iota
	// TrustUntrusted is fork-pull-request work: arbitrary code from someone
	// outside the organization. It requires a real isolation boundary.
	TrustUntrusted
	// TrustTrusted is work from the repository itself — a push, a schedule, a
	// dispatch, or a same-repo pull request.
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

// Classify decides how much a workload is trusted from the event that queued it.
//
// DELIBERATELY CONSERVATIVE, and the reason is a limitation of the protocol
// rather than caution for its own sake. A scale-set message carries an event
// name and the repository it came from; it does NOT say whether a pull request
// came from a fork. So billet cannot tell "a teammate opened a PR" from "a
// stranger opened a PR against a public repo" — and those differ by whether
// arbitrary outside code is about to run on your hardware.
//
// Given it cannot tell, it assumes the worse one. EVERY pull request is
// untrusted. That is stricter than necessary for a private repository with two
// members, and it is the only safe default for a tool other people will point at
// public ones. A deployment that knows better can widen it deliberately; nothing
// widens by accident.
//
// An unrecognised event is unknown, not trusted, for the same reason: GitHub adds
// events, and a new one must not inherit permission from a switch statement
// written before it existed.
func Classify(event string) TrustClass {
	switch event {
	case "pull_request", "pull_request_target":
		return TrustUntrusted

	// EVENTS THAT CARRY PULL-REQUEST CODE WITHOUT SAYING SO. Each is a way for outside
	// code to arrive under a name that sounds internal:
	//
	// 	merge_group     runs the candidate MERGE COMMIT, which contains the pull
	// 	                request's code, fork-authored included.
	// 	workflow_run    is triggered BY another workflow, commonly a fork's PR run, and
	// 	                the standard pattern downloads that run's artifacts — the
	// 	                well-known artifact-poisoning vector.
	// 	workflow_call   is a reusable workflow a pull-request workflow can call, and
	// 	                which event the scale set reports is not established.
	// 	deployment      can name a PR preview ref; the event says nothing about whose
	// 	deployment_status  code is at that ref.
	//
	// UNKNOWN rather than untrusted: billet is not asserting they are hostile, only that
	// the event name does not establish provenance. Unknown fails closed.
	case "merge_group", "workflow_run", "workflow_call",
		"deployment", "deployment_status":
		return TrustUnknown

	// What is left is code that reached the repository through someone with
	// write access, which is the only provenance an event name can actually
	// establish. repository_dispatch needs an authorised credential to fire, so
	// it belongs here — with the caveat that whoever holds that credential is
	// inside the trust boundary.
	case "push", "schedule", "workflow_dispatch", "release", "repository_dispatch":
		return TrustTrusted

	default:
		// Includes the empty string. GitHub adds events, and a new one must not
		// inherit permission from a switch written before it existed.
		return TrustUnknown
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
	Disk           config.ByteSize
	// SHM sizes /dev/shm. It is a tier parameter rather than a constant because
	// Postgres service containers and Chromium both fail on the default 64MB in
	// ways that look like unrelated crashes.
	SHM config.ByteSize
	// Volumes are cache devices known before boot. A cache endpoint reserves the
	// remaining slots so a cooperative guest can request them at runtime.
	Volumes []VolumeMount
	// CacheEndpoint and CacheToken let the guest request runtime volumes from its
	// node. They are paired, and the token is unique to this one microVM.
	CacheEndpoint string
	CacheToken    string
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

// Instance is a unit of compute the backend knows about. It may or may not
// still be running — see Running.
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
// money (#46). Destroy is reached on paths where the guest is still working — a
// drain, a custody teardown, an operator killing a job — and the listener
// releases the lease on success. So a new job could start while the old guest was
// still finishing a deploy or a migration: two concurrent effects on something
// outside billet, which is worse than the over-commit the destroy-then-release
// ordering was written to prevent, and not bounded by anything.
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
	// to be wrong about (#48).
	Destroy(ctx context.Context, id string) (Teardown, error)
}
