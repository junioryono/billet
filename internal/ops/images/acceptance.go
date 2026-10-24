package images

import (
	"fmt"
	"slices"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imageaccept"
)

// acceptedComponents is what a generation on this node is completed with: the
// node's own images.accept together with what its deployment's control plane
// accepts, as the node last recorded it at a registration (internal/imageaccept),
// so a license accepted once in the control plane's config reaches every node.
//
// EITHER ACCEPTS. Each is an operator's act on this host's behalf, and a
// deployment that accepted a component on a node before its control plane said
// anything keeps it there.
//
// A RECORD THAT CANNOT BE READ, OR WHOSE DEPLOYMENT CANNOT BE TOLD, IS AN ERROR,
// never the node's own list alone: completing a generation with less than was
// accepted and promoting it moves every tier onto an image without what the
// deployment accepted. A record another deployment wrote is not this one's, and
// is left out. A name this build does not install is refused rather than skipped,
// for validateImages' reason.
func acceptedComponents(cfg *config.Config) ([]config.ImageComponent, error) {
	accepted := cfg.Images.Accepted()
	if cfg.Node == nil {
		return accepted, nil
	}

	rec, ok, err := imageaccept.Read(cfg.Node.StateDir)
	if err != nil {
		return nil, err
	}

	if !ok {
		return accepted, nil
	}

	deployment, known, err := app.PeekNodeDeploymentID(cfg)
	if err != nil {
		return nil, fmt.Errorf("could not tell which deployment this node is in, so not whether "+
			"the image acceptance it recorded is its own: %w", err)
	}

	if !known {
		return nil, fmt.Errorf("this node recorded the image acceptance of deployment %s and has "+
			"no deployment identity to compare it with", rec.Deployment)
	}

	if rec.Deployment != deployment {
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
