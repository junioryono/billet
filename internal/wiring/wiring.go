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

	"github.com/junioryono/billet/internal/node"
	"github.com/junioryono/billet/internal/nodeplane"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/server"
)

// Provisioner adapts the client to the control plane's scale-set needs.
type Provisioner struct{ Client *scaleset.Client }

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
type JITSource struct{ Client *scaleset.Client }

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

// ValidateTrustedRunnerGroup verifies policy immediately before remote minting.
func (n NodeJIT) ValidateTrustedRunnerGroup(ctx context.Context, group string,
	workflows []string,
) error {
	return n.Client.ValidateTrustedRunnerGroup(ctx, group, workflows)
}
