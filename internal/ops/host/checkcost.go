package host

import (
	"fmt"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
)

// printRemoteCost bounds what this node's own declarations can cost per hour.
//
// EVERY REMOTE BACKEND, THROUGH app.RemoteShapes, and it used to read node.ec2 directly.
// A codebuild node declares ordered shapes with a price per hour for the same reason
// an ec2 node does — placement charges the first that fits — so reading one block by
// name meant a check that reported the cost exposure of an ec2 node and stayed silent
// for a codebuild one, which reads as compute that is free.
//
// THE NODE'S PROVIDER NAMES ITSELF in the line, because the two backends bill for
// different things: an ec2 shape is an instance-hour, a codebuild compute type is a
// build-minute rate expressed per hour, and an operator comparing the two numbers
// needs to know which they are looking at.
func PrintRemoteCost(env cli.Env, cfg *config.Config) error {
	shapes := app.RemoteShapes(cfg)
	if len(shapes) == 0 {
		return nil
	}

	maxVCPU := cfg.Node.MaxVCPU
	maxMemory := cfg.Node.MaxMemory
	if cfg.Server != nil {
		maxVCPU = min(maxVCPU, cfg.Server.MaxVCPU)
		maxMemory = min(maxMemory, cfg.Server.MaxMemory)
	}

	peak, err := config.RemotePeakHourlyExposure(maxVCPU, maxMemory, shapes)
	if err != nil {
		return err
	}

	fmt.Fprintf(env.Stdout, "%-8s <= %s compute (%s/month at 730h), from declared shape prices\n",
		string(cfg.Node.Provider)+" max", &peak, peak.ForHours(730))

	return nil
}

// spotLabel names the market a node buys in, because it decides whether a build
// can be killed by somebody else.
func spotLabel(spot bool) string {
	if spot {
		return "spot (a reclaim fails the build; github does not requeue it)"
	}

	return "on-demand"
}
