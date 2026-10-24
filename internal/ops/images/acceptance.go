package images

import (
	"errors"
	"fmt"
	"slices"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imageaccept"
)

// errAcceptanceUnknown is the refusal when a node holds no record of what its
// deployment's control plane accepts.
var errAcceptanceUnknown = errors.New("this node has not recorded what its control plane accepts " +
	"for guest images, which it does at every registration; start the node (`billet local up`) " +
	"and run this again once it has registered")

// acceptedComponents is what a generation on this node is completed with: the
// node's own images.accept together with what its deployment's control plane
// accepts, as the node last recorded it at a registration (internal/imageaccept),
// so a license accepted once in the control plane's config reaches every node.
//
// EITHER ACCEPTS. Each is an operator's act on this host's behalf, and a
// deployment that accepted a component on a node before its control plane said
// anything keeps it there.
//
// WHAT CANNOT BE TOLD REFUSES, never the node's own list alone: completing a
// generation with less than was accepted and promoting it moves every tier onto
// an image without what the deployment accepted. That is a record that cannot be
// read, a deployment that cannot be told, a name this build does not install
// (validateImages' reason), and no record of this deployment's control plane at
// all. A server section in the node's file is no substitute: it proves neither
// that the node dials that control plane nor that the control plane read this
// file.
func acceptedComponents(cfg *config.Config) ([]config.ImageComponent, error) {
	accepted := cfg.Images.Accepted()
	if cfg.Node == nil {
		return accepted, nil
	}

	rec, ok, err := imageaccept.Read(cfg.Node.StateDir)
	if err != nil {
		return nil, err
	}

	if ok {
		deployment, known, err := app.PeekNodeDeploymentID(cfg)
		if err != nil {
			return nil, fmt.Errorf("could not tell which deployment this node is in, so not whether "+
				"the image acceptance it recorded is its own: %w", err)
		}

		if !known {
			return nil, fmt.Errorf("this node recorded the image acceptance of deployment %s and has "+
				"no deployment identity to compare it with", rec.Deployment)
		}

		// ANOTHER DEPLOYMENT'S RECORD SAYS NOTHING ABOUT THIS ONE: the node has not
		// registered with its own control plane since it moved.
		ok = rec.Deployment == deployment
	}

	switch {
	case !ok:
		return nil, errAcceptanceUnknown
	case !rec.Carried:
		// A control plane older than the wire that carries it: this node's own list
		// is all there is, as it was before.
		return accepted, nil
	}

	for _, name := range rec.Components {
		if !config.IsImageComponent(name) {
			return nil, fmt.Errorf("the control plane accepts the image component %q, which this "+
				"billet does not install; upgrade billet on this node so it installs everything "+
				"its deployment accepted", name)
		}

		if !slices.Contains(accepted, config.ImageComponent(name)) {
			accepted = append(accepted, config.ImageComponent(name))
		}
	}

	slices.Sort(accepted)

	return accepted, nil
}
