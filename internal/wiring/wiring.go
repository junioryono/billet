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
