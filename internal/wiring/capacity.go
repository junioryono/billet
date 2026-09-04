package wiring

import (
	"errors"
	"fmt"

	"github.com/junioryono/godi/v5"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

// allocatorOptions is the group an alloc.Option is registered into.
//
// A GROUP RATHER THAN A PARAMETER, because options are appended and never
// replaced: production registers the placement policy, a test registers
// alloc.WithClock beside it, and neither has to know the other exists. godi
// hands a constructor an empty slice for a group nobody joined (measured in
// scope.GetGroup), so a set with no options builds.
const allocatorOptions = "alloc-options"

// CapacityModule registers the allocator over the ledger and the config.
//
// alloc.New re-applies the catalogue's rules, so it is built wherever the
// config is loaded, before any claim: a tier the catalogue refuses stops the
// process at startup rather than at a failover.
func CapacityModule() godi.ModuleOption {
	return godi.NewModule("capacity",
		godi.AddSingleton(newAllocator),
	)
}

// AllocatorOption registers one alloc.Option for the allocator to be built with.
func AllocatorOption(opt alloc.Option) godi.ModuleOption {
	return godi.AddSingleton(func() alloc.Option { return opt }, godi.Group(allocatorOptions))
}

type allocatorParams struct {
	godi.In

	DB      *state.DB
	Config  *config.Config
	Options []alloc.Option `group:"alloc-options"`
}

func newAllocator(p allocatorParams) (*alloc.Allocator, error) {
	if p.Config.Server == nil {
		return nil, errors.New("wiring: the capacity allocator needs a server section")
	}

	a, err := alloc.New(p.DB, alloc.Limits{
		MaxVCPU:   p.Config.Server.MaxVCPU,
		MaxMemory: p.Config.Server.MaxMemory,
		Nodes:     p.Config.NodePolicies(),
	}, p.Config.Tiers, p.Options...)
	if err != nil {
		return nil, fmt.Errorf("capacity allocator: %w", err)
	}

	return a, nil
}
