package server

import (
	"context"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
)

// scaleSetKey identifies one scale set within a target: its runner group plus
// its name (a tier's ScaleSetName), since the same name in two groups is two
// objects.
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

// judgeUndeclaredScaleSets names every scale set billet recorded creating, on
// any target this control plane serves, that no configured tier claims, and
// refuses to start while one of them carries a declared tier's name where
// GitHub could route that tier's jobs to it.
//
// declared is keyed by the target's GitHub path, then by the scale set.
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
// ONE SCALE SET PER NAME IN A ROUTING SCOPE (#140, design item 9). A job whose
// runs-on names a scale set can be given to any scale set of that name it can
// reach: in another runner group of the same target, or at the organization a
// repository belongs to. When billet left one of those behind, a declared
// tier's jobs can go to a scale set nothing polls, and wait there however much
// room the tier has. That is refused, with the teardown that clears it named,
// rather than warned: the warning is the case that left a deployment serving a
// tier half its jobs never reached. Which runner groups a repository may use
// is GitHub's policy and is not read here, so a name in another group counts
// whether or not the group admits the repository; and only the records of the
// targets this control plane serves, and their organizations, are read.
//
// STILL NEVER ACTED ON. Deleting one is a thing an operator asks for, once, on
// purpose; a control plane that tidied up on startup would dismantle the tiers
// of an operator running a second config against the same ledger.
func (s *Server) judgeUndeclaredScaleSets(
	ctx context.Context, declared map[string]map[scaleSetKey]struct{},
) error {
	if s.completionStore == nil || len(s.targets) == 0 {
		return nil
	}

	var collisions []string

	// EVERY TARGET, not only those with a tier left: removing the last tier of a
	// target is the edit most likely to strand its scale set.
	for _, target := range s.targets {
		path := target.Config.Path()

		recorded, err := s.completionStore.ScaleSets(ctx, path)
		if err != nil {
			return fmt.Errorf("server: read recorded scale sets for %s: %w", path, err)
		}

		for _, rec := range recorded {
			if _, ok := declared[path][scaleSetKey{group: rec.RunnerGroup, label: rec.Label}]; ok {
				continue
			}

			if claimant, ok := s.routedTo(declared, target.Config, rec.Label); ok {
				collisions = append(collisions, fmt.Sprintf("scale set %q in runner group %q on %s (id %d) "+
					"carries the name %s; remove it with `billet teardown --tier %s --runner-group %s --target %s`",
					rec.Label, rec.RunnerGroup, path, rec.ID, claimant, rec.Label, rec.RunnerGroup, target.Config.Name))

				continue
			}

			s.log.Warn("a scale set billet created is no longer declared by any tier; "+
				"it advertises nothing, so a job using that name in runs-on queues rather than failing. "+
				"Remove it with `billet teardown --tier <name> --runner-group <group> --target <target>`",
				"name", rec.Label, "group", rec.RunnerGroup, "scale_set", rec.ID,
				"target", target.Config.Name, "path", path)
		}

		// A REPOSITORY'S JOBS GO TO ITS ORGANIZATION'S SCALE SETS TOO. When this
		// control plane serves the organization, the loop above reads its
		// records; when it does not, they are read here, and only a name one of
		// this repository's tiers answers to matters.
		owner := target.Config.Owner()
		if !target.Config.IsRepository() || s.serves(owner) {
			continue
		}

		above, err := s.completionStore.ScaleSets(ctx, owner)
		if err != nil {
			return fmt.Errorf("server: read recorded scale sets for %s: %w", owner, err)
		}

		for _, rec := range above {
			if claimant, ok := claimedOn(declared[path], rec.Label); ok {
				collisions = append(collisions, fmt.Sprintf("scale set %q in runner group %q on the "+
					"organization %s (id %d) carries the name tier %s answers to on %s; this control plane "+
					"does not serve %s, so remove it with `billet teardown --tier %s --runner-group %s --target "+
					"<that organization's target>` from a configuration that declares it",
					rec.Label, rec.RunnerGroup, owner, rec.ID, claimant, path, owner, rec.Label, rec.RunnerGroup))
			}
		}
	}

	if len(collisions) > 0 {
		return fmt.Errorf("server: refusing to start: a scale set billet created and no tier declares would "+
			"take jobs a declared tier answers to, and nothing polls it, so those jobs would wait there:\n  %s",
			strings.Join(collisions, "\n  "))
	}

	return nil
}

// routedTo names the declared tier a job for name could be routed away from by
// an undeclared scale set of that name on target: one on the same target in
// another runner group, one of the organization's repositories when target is
// an organization, or the organization when target is a repository.
func (s *Server) routedTo(declared map[string]map[scaleSetKey]struct{}, target config.GitHubTarget,
	name string,
) (string, bool) {
	path := target.Path()

	if label, ok := claimedOn(declared[path], name); ok {
		return fmt.Sprintf("%q answers to on %s", label, path), true
	}

	for _, other := range s.targets {
		otherPath := other.Config.Path()
		if otherPath == path {
			continue
		}

		related := (!target.IsRepository() && other.Config.IsRepository() &&
			strings.EqualFold(other.Config.Owner(), path)) ||
			(target.IsRepository() && !other.Config.IsRepository() &&
				strings.EqualFold(otherPath, target.Owner()))
		if !related {
			continue
		}

		if label, ok := claimedOn(declared[otherPath], name); ok {
			return fmt.Sprintf("%q answers to on %s", label, otherPath), true
		}
	}

	return "", false
}

// claimedOn reports the name a declared scale set of one target answers to
// when it is name, compared as GitHub compares them, without case.
func claimedOn(declared map[scaleSetKey]struct{}, name string) (string, bool) {
	for key := range declared {
		if strings.EqualFold(key.label, name) {
			return key.label, true
		}
	}

	return "", false
}

// serves reports whether this control plane serves the target at path.
func (s *Server) serves(path string) bool {
	for _, target := range s.targets {
		if strings.EqualFold(target.Config.Path(), path) {
			return true
		}
	}

	return false
}
