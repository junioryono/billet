// Package provider launches and destroys the compute a job runs on.
//
// The interface is deliberately NARROW, and that is a decision rather than an
// oversight. The plan is explicit that a real contract eventually needs more
// than launch and destroy — readiness and boot timeout, console diagnostics,
// idempotency keys and post-restart reconciliation, graceful stop against forced
// kill, network policy, volume attach and quiesce, image preparation, spot
// interruption, capability negotiation. All of that is true and none of it is
// here, because every one of those shapes is a guess until a second backend
// needs it. Firecracker and EC2 will force the generalisation, and an interface
// designed against one implementation is usually wrong in ways only the second
// one reveals.
//
// What IS here is the part every backend must agree on: launch one instance for
// one job, destroy it, and never let the credential reach a place it can be
// read.
package provider

import (
	"context"

	"github.com/junioryono/billet/internal/config"
)

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

	// EVENTS THAT CARRY PULL-REQUEST CODE WITHOUT SAYING SO. Each of these was
	// on the trusted list and each is a way for outside code to arrive under a
	// name that sounds internal:
	//
	//   merge_group     runs the candidate MERGE COMMIT, which contains the pull
	//                   request's code, fork-authored included.
	//   workflow_run    is triggered BY another workflow — commonly a fork's PR
	//                   run — and the standard pattern is to download that run's
	//                   artifacts. This is the well-known artifact-poisoning
	//                   vector, and it was the worst thing on the old list.
	//   workflow_call   is a reusable workflow, which a pull-request workflow can
	//                   call. Whether the scale set reports the caller's event or
	//                   this one is not established, and an unverified assumption
	//                   is not a basis for granting trust.
	//   deployment      can name a PR preview ref. The event says nothing about
	//   deployment_status  whose code is at that ref.
	//
	// They are UNKNOWN rather than untrusted: billet is not asserting they are
	// hostile, only that the event name does not establish provenance. Unknown
	// fails closed, which is the same practical outcome and the honest label.
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
	Disk   config.ByteSize
	// SHM sizes /dev/shm. It is a tier parameter rather than a constant because
	// Postgres service containers and Chromium both fail on the default 64MB in
	// ways that look like unrelated crashes.
	SHM config.ByteSize

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
	Destroy(ctx context.Context, id string) error
}
