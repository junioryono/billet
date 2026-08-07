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

// Spec is one instance to launch.
type Spec struct {
	// Name identifies the instance to the operator and to GitHub. It is the
	// runner name the JIT config was minted for, so the two must agree.
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

	// JITConfig is the single-use runner registration.
	//
	// IT IS A CREDENTIAL until the runner consumes it, and it is the reason this
	// struct is passed by value to a method rather than assembled into a command
	// line by the caller. A backend MUST NOT put it in argv, where every process
	// on the host can read it, and MUST NOT log it. Both mistakes look like
	// working code.
	JITConfig string
}

// Instance is a launched instance.
type Instance struct {
	// ID is the backend's own handle — a container id, a microVM id, an EC2
	// instance id. Opaque to everything above.
	ID string
	// Name echoes the spec, so a caller holding only an Instance can still say
	// which runner it is.
	Name string
}

// Provider launches and destroys the compute for one job at a time.
type Provider interface {
	// Kind reports which backend this is. Placement compares it against what a
	// lease requires, so a Firecracker lease cannot land on a Tart host.
	Kind() config.ProviderKind

	// Launch starts one instance running the job its JIT config names.
	//
	// It returns when the instance has been CREATED, not when the runner inside
	// it is ready. Readiness is a separate question with a separate timeout, and
	// conflating them is how a slow image pull becomes a launch failure.
	Launch(ctx context.Context, spec Spec) (*Instance, error)

	// Destroy removes an instance and everything it owns.
	//
	// MUST be idempotent: destroying an id that is already gone is success, not
	// an error. Teardown runs on paths that have already failed once, and an
	// error there turns a recoverable state into a stuck one.
	Destroy(ctx context.Context, id string) error
}
