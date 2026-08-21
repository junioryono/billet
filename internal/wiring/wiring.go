// Package wiring adapts the scale-set client to what the control plane and the
// node consume.
//
// It exists because internal/scaleset returns its OWN types: the alternative is
// that package importing internal/server purely to name a two-field struct,
// which points the dependency the wrong way for a package whose whole job is to
// keep a preview API at arm's length.
//
// It is a package rather than a few funcs in main because the end-to-end test
// assembles billet the same way the CLI does, and a hand-copied adapter defeats
// that. The copy in the test had already drifted — it dereferenced a scale set
// the client returns as nil for "no such set", which the original checks for —
// and a test that exercises different wiring than production is testing the
// wrong program.
package wiring

import (
	"context"
	"errors"

	"github.com/junioryono/billet/internal/alloc"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
)

// Provisioner adapts the client to the control plane's scale-set needs.
type Provisioner struct{ Client *scaleset.Client }

type poolRunnerStore interface {
	PoolRunnerByLease(context.Context, string) (alloc.PoolRunner, error)
	PreserveRecoveredBusyPoolRunner(context.Context, alloc.PoolRunner) error
	RetireRecoveredPoolRunner(context.Context, string) (alloc.PoolRunner, error)
	RetirePoolRunner(context.Context, string) error
}

type runnerRecoveryClient interface {
	RecoverRunner(context.Context, string) (billetgithub.RunnerRecovery, error)
	RemoveRunner(context.Context, int64, string) error
}

// EnsureScaleSet makes a tier's scale set exist.
func (p Provisioner) EnsureScaleSet(
	ctx context.Context, name, group string, labels []string,
) (*server.ScaleSet, error) {
	set, err := p.Client.EnsureScaleSet(ctx, name, group, labels)
	if err != nil {
		return nil, err
	}

	return &server.ScaleSet{ID: set.ID, Name: set.Name, Group: set.Group}, nil
}

// Session opens a long-poll session on one scale set.
func (p Provisioner) Session(ctx context.Context, scaleSetID int, owner string) (server.Session, error) {
	return p.Client.Session(ctx, scaleSetID, owner)
}

// RemoveRunner removes routing before the control plane tears compute down.
func (p Provisioner) RemoveRunner(ctx context.Context, runnerID int64, runnerName string) error {
	return p.Client.RemoveRunner(ctx, runnerID, runnerName)
}

// ValidateTrustedRunnerGroup verifies a trusted tier's workflow boundary.
func (p Provisioner) ValidateTrustedRunnerGroup(ctx context.Context, group string,
	workflows []string,
) error {
	return p.Client.ValidateTrustedRunnerGroup(ctx, group, workflows)
}

// JITSource adapts the client to what the node needs to mint registrations.
type JITSource struct {
	Client *scaleset.Client
	Pool   poolRunnerStore
}

// Describe resolves a tier's scale set, reporting a nil set when there is none.
//
// The nil check is the whole reason this is shared code. Describe returns
// (nil, nil, nil) for a scale set that does not exist — a perfectly reasonable
// contract, and one that segfaults the moment a caller assumes an error would
// have been returned instead.
func (j JITSource) Describe(ctx context.Context, name, group string) (*node.Set, []string, error) {
	set, labels, err := j.Client.Describe(ctx, name, group)
	if err != nil || set == nil {
		return nil, labels, err
	}

	return &node.Set{ID: set.ID, Name: set.Name}, labels, nil
}

// JITConfig mints a single-use runner registration.
func (j JITSource) JITConfig(
	ctx context.Context, scaleSetID int, runnerName, workFolder string,
) (node.Registration, error) {
	return j.Client.JITConfig(ctx, scaleSetID, runnerName, workFolder)
}

// RemoveRunner removes a failed local launch's GitHub registration.
func (j JITSource) RemoveRunner(
	ctx context.Context, _ string, runnerID int64, runnerName string,
) error {
	return j.Client.RemoveRunner(ctx, runnerID, runnerName)
}

// EnsureRunnerRemoved resolves a restart-surviving registration from the
// control-plane journal before recovered compute is touched.
func (j JITSource) EnsureRunnerRemoved(ctx context.Context, leaseID string) error {
	if j.Pool == nil {
		return errors.New("wiring: runner identity storage is unavailable")
	}
	binding, err := j.Pool.PoolRunnerByLease(ctx, leaseID)
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := j.Pool.RetirePoolRunner(ctx, leaseID); err != nil {
		return err
	}
	return j.Client.RemoveRunner(ctx, binding.RunnerID, binding.RunnerName)
}

// RecoverRunner preserves a legacy quarantined registration while it is busy,
// or removes it while idle before the node tears its compute down.
func (j JITSource) RecoverRunner(ctx context.Context, leaseID, tier string,
	requestID int64, runnerName string,
) (node.RunnerRecovery, error) {
	return recoverRunner(ctx, j.Pool, j.Client, leaseID, tier, requestID, runnerName)
}

func recoverRunner(ctx context.Context, pool poolRunnerStore, client runnerRecoveryClient,
	leaseID, tier string, requestID int64, runnerName string,
) (node.RunnerRecovery, error) {
	if pool == nil {
		return "", errors.New("wiring: runner identity storage is unavailable")
	}
	if client == nil {
		return "", errors.New("wiring: runner recovery client is unavailable")
	}
	binding, err := pool.PoolRunnerByLease(ctx, leaseID)
	if err == nil {
		if binding.Tier != tier || binding.LaunchRequestID != requestID {
			return "", errors.New("wiring: durable runner identity does not match its lease")
		}
		switch binding.Status {
		case alloc.PoolRunnerBusy:
			if binding.ActualRequestID != 0 || binding.RunID != 0 || binding.JobID != "" {
				return node.RunnerRecoveryTracked, nil
			}
			runnerName = binding.RunnerName
		case alloc.PoolRunnerIdle:
			runnerName = binding.RunnerName
		case alloc.PoolRunnerRetiring:
			if err := client.RemoveRunner(ctx, binding.RunnerID, binding.RunnerName); err != nil {
				return "", err
			}
			return node.RunnerRecoveryRetired, nil
		case alloc.PoolRunnerRetired:
			return node.RunnerRecoveryRetired, nil
		default:
			return "", errors.New("wiring: durable runner identity has an unknown status")
		}
	}
	if err != nil && !errors.Is(err, alloc.ErrLeaseNotFound) {
		return "", err
	}
	recovery, err := client.RecoverRunner(ctx, runnerName)
	if err != nil {
		return "", err
	}
	if recovery.Present && binding.LeaseID != "" && binding.RunnerID != 0 &&
		binding.RunnerID != recovery.RunnerID {
		return "", errors.New("wiring: recovered runner id changed; refusing replacement identity")
	}
	if recovery.Busy {
		if err := pool.PreserveRecoveredBusyPoolRunner(ctx, alloc.PoolRunner{
			LeaseID: leaseID, Tier: tier, LaunchRequestID: requestID,
			RunnerID: recovery.RunnerID, RunnerName: runnerName,
		}); err != nil {
			return "", err
		}
		return node.RunnerRecoveryBusy, nil
	}
	// REMOTE FIRST, THEN AN ATOMIC LOCAL CLAIM. JobStarted is authoritative and
	// may land while either GitHub call is in flight; the final store transaction
	// must see and preserve that busy binding. A crash between the two is safe:
	// the idle row and charged quarantine remain, and the next recovery observes
	// the now-absent registration before claiming it again.
	if recovery.Present {
		if err := client.RemoveRunner(ctx, recovery.RunnerID, runnerName); err != nil {
			return "", err
		}
	}
	settled, err := pool.RetireRecoveredPoolRunner(ctx, leaseID)
	if errors.Is(err, alloc.ErrLeaseNotFound) {
		return node.RunnerRecoveryRetired, nil
	}
	if err != nil {
		return "", err
	}
	if settled.Status == alloc.PoolRunnerBusy &&
		(settled.ActualRequestID != 0 || settled.RunID != 0 || settled.JobID != "") {
		return node.RunnerRecoveryTracked, nil
	}
	return node.RunnerRecoveryRetired, nil
}

// ValidateTrustedRunnerGroup verifies policy immediately before local minting.
func (j JITSource) ValidateTrustedRunnerGroup(ctx context.Context, group string,
	workflows []string,
) error {
	return j.Client.ValidateTrustedRunnerGroup(ctx, group, workflows)
}

// NodeJIT is the same source, shaped for the node wire.
//
// TWO SHAPES FOR ONE THING, and the duplication is deliberate. internal/nodeplane
// declares its own JITSet and JITRegistration so the transport does not import
// the runtime it serves — they sit on opposite sides of a process boundary, and
// coupling them would defeat the point of having one. The cost is this adapter;
// the benefit is that neither side moves because the other was edited.
type NodeJIT struct{ Client *scaleset.Client }

// Describe finds a scale set for the wire.
//
// The nil-set-means-absent contract is preserved rather than flattened: the node
// treats absence as a reason to stop, and a zero-valued set would have it launch
// against scale set 0.
func (n NodeJIT) Describe(
	ctx context.Context, name, group string,
) (*nodeplane.JITSet, []string, error) {
	set, labels, err := n.Client.Describe(ctx, name, group)
	if err != nil || set == nil {
		return nil, labels, err
	}

	return &nodeplane.JITSet{ID: set.ID, Name: set.Name}, labels, nil
}

// JITConfig mints a registration for a remote node.
func (n NodeJIT) JITConfig(
	ctx context.Context, scaleSetID int, runnerName, workFolder string,
) (nodeplane.JITRegistration, error) {
	return n.Client.JITConfig(ctx, scaleSetID, runnerName, workFolder)
}

// RemoveRunner removes a remote node's failed-launch registration.
func (n NodeJIT) RemoveRunner(ctx context.Context, runnerID int64, runnerName string) error {
	return n.Client.RemoveRunner(ctx, runnerID, runnerName)
}

// RecoverRunner resolves the exact legacy registration for a remote node.
func (n NodeJIT) RecoverRunner(
	ctx context.Context, runnerName string,
) (nodeplane.JITRunnerRecovery, error) {
	recovery, err := n.Client.RecoverRunner(ctx, runnerName)
	return nodeplane.JITRunnerRecovery{RunnerID: recovery.RunnerID,
		Present: recovery.Present, Busy: recovery.Busy}, err
}

// ValidateTrustedRunnerGroup verifies policy immediately before remote minting.
func (n NodeJIT) ValidateTrustedRunnerGroup(ctx context.Context, group string,
	workflows []string,
) error {
	return n.Client.ValidateTrustedRunnerGroup(ctx, group, workflows)
}
