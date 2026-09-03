package server

import (
	"context"
	"fmt"
)

// scaleSetKey identifies one scale set: an organization's runner group plus the
// label, since the same label in two groups is two objects.
//
// A struct rather than a joined string, because a delimiter is a decision about
// characters the two halves may contain and neither the state store nor GitHub
// promises anything about that.
type scaleSetKey struct {
	group string
	label string
}

// defaultRunnerGroup is where a scale set lands when a tier names no group.
//
// A copy of scaleset.DefaultRunnerGroup, which is a copy of the service
// client's. It cannot be imported here — internal/scaleset imports this package,
// and depguard confines the vendor client to that one — so the two are pinned
// together by a test rather than by the compiler.
const defaultRunnerGroup = "default"

// groupOrDefault resolves the group a tier's scale set actually lands in.
//
// A tier naming no group gets the service's default one, so comparing the raw
// field against a recorded group would report every such tier as an orphan of
// itself.
func groupOrDefault(group string) string {
	if group == "" {
		return defaultRunnerGroup
	}

	return group
}

// reportUndeclaredScaleSets names every scale set billet recorded creating that
// no configured tier claims.
//
// WHY THIS EXISTS AT ALL: removing a tier from the config is the ordinary way to
// stop offering a size, and the scale set it created stays on the organization
// advertising nothing — so a job aimed at that label queues rather than failing,
// which is billet's characteristic failure reached by an ordinary config edit.
// Nothing said so, because the config was the only index and the service client
// cannot enumerate a runner group: its list call always filters by name, so
// billet can ask about a scale set it can name and no others.
//
// BEFORE RECONCILING, and from the CONFIG rather than from what reconciled.
// After the loop it missed the two cases that matter most: removing the LAST
// tier returns before reaching it, and one tier failing to reconcile suppressed
// every warning, including rows nothing to do with that tier.
//
// REPORTED, NEVER ACTED ON. Deleting one is a thing an operator asks for, once,
// on purpose; a control plane that tidied up on startup would dismantle the
// tiers of an operator running a second config against the same ledger.
func (s *Server) reportUndeclaredScaleSets(
	ctx context.Context, declared map[scaleSetKey]struct{},
) error {
	if s.completionStore == nil || s.org == "" {
		return nil
	}

	recorded, err := s.completionStore.ScaleSets(ctx, s.org)
	if err != nil {
		return fmt.Errorf("server: read recorded scale sets: %w", err)
	}

	for _, rec := range recorded {
		if _, ok := declared[scaleSetKey{group: rec.RunnerGroup, label: rec.Label}]; ok {
			continue
		}

		s.log.Warn("a scale set billet created is no longer declared by any tier; "+
			"it advertises nothing, so a job using that label queues rather than failing. "+
			"Remove it with `billet teardown --tier <label> --runner-group <group>`",
			"tier", rec.Label, "group", rec.RunnerGroup, "scale_set", rec.ID)
	}

	return nil
}
