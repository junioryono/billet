package fleetops

import (
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/app"

	"github.com/junioryono/billet/internal/config"
)

// teardownTargets decides which scale sets `billet teardown` acts on, and says
// whether the answer came from outside the config.
//
// A TIER THE CONFIG NO LONGER DECLARES IS STILL DELETABLE, and that is the whole
// reason this is not a lookup. Removing a tier from billet.yaml is the ordinary
// way to stop offering a size, and it used to strand the scale set that tier had
// created: the config was the only index, so nothing could name the object
// afterwards, and GitHub's UI cannot remove a scale set either. The object left
// behind advertises nothing, so a job aimed at that label queues rather than
// failing — billet's characteristic failure, reached by an ordinary config edit.
//
// The expected labels are NOT lost with the tier definition. billet names a
// scale set after its tier's runs_on (its label unless it names one) and labels
// it with the same string, so the name is
// enough to check against and no --force is needed to delete one this way. The
// runner group is the part the config was carrying, so an undeclared tier takes
// it from the operator instead.
//
// --all stays scoped to declared tiers. Enumerating what billet owns on an
// organization is a different question with a different failure mode, and "delete
// everything I can find" is not something a destructive command should infer.
func teardownTargets(tiers []config.Tier, tier, group string, force bool) ([]config.Tier, bool, error) {
	if tier == "" {
		if group != "" {
			return nil, false, errors.New(
				"--runner-group names where to look for one --tier; it does nothing for --all")
		}

		if len(tiers) == 0 {
			return nil, false, errors.New("the config declares no tiers, so there is nothing to delete")
		}

		return tiers, false, nil
	}

	// --tier names a scale set, which is a tier's runs_on: one per target, so
	// several targets may each declare one by that name.
	var (
		matched   []config.Tier
		elsewhere bool
	)

	for i := range tiers {
		// GITHUB'S IDENTITY, WITHOUT CASE, for the name and the group alike: a
		// case-only difference in either names the declared set itself, and
		// taking it for another would delete the set this promises to keep.
		if !strings.EqualFold(tiers[i].ScaleSetName(), tier) {
			continue
		}

		// ANOTHER GROUP IS ANOTHER SCALE SET. A set of the declared tier's name
		// left in a group the tier has moved out of is exactly what a control
		// plane refuses to start beside (#140), and this is how it is removed:
		// the flag addresses that set, never the declared one, which keeps its
		// own group. It is deleted as an undeclared set is, by name, behind the
		// label check and the confirmation, and teardownCommand says which set it
		// is and that the declared one is left alone.
		if group != "" && !strings.EqualFold(groupOrDefault(group), groupOrDefault(tiers[i].RunnerGroup)) {
			elsewhere = true

			continue
		}

		matched = append(matched, tiers[i])
	}

	if len(matched) > 0 {
		return matched, false, nil
	}

	if elsewhere {
		if force {
			return nil, false, fmt.Errorf("--force skips the check that %q in runner group %q carries the "+
				"label billet would have given it, and the config declares that name only in another "+
				"group, so nothing else says this set is billet's", tier, group)
		}

		return []config.Tier{{Label: tier, RunnerGroup: group}}, true, nil
	}

	if group == "" {
		// A tier's own label names nothing on GitHub once it has a runs_on, so the
		// operator is asking either for the tier's set, by its other name, or for
		// a set left under the old one, which needs its group like any other.
		for i := range tiers {
			if tiers[i].Label == tier {
				return nil, false, fmt.Errorf(
					"tier %q answers to %q on GitHub; pass --tier %q to delete its scale set, "+
						"or name a runner group with --runner-group to delete a set left under %q",
					tier, tiers[i].ScaleSetName(), tiers[i].ScaleSetName(), tier)
			}
		}

		return nil, false, fmt.Errorf(
			"%q is not a tier in the config, so nothing says which runner group it is in; "+
				"name it with --runner-group (billet's default group is %q)",
			tier, groupOrDefault(""))
	}

	if force {
		return nil, false, fmt.Errorf(
			"--force skips the check that %q carries the label billet would have given it, "+
				"and that check is the only evidence this scale set is billet's — the name "+
				"and group both came from you. Declare the tier in the config to force it",
			tier)
	}

	return []config.Tier{{Label: tier, RunnerGroup: group}}, true, nil
}

// declaredGroup is the runner group a declared tier answering to name is in.
func declaredGroup(tiers []config.Tier, name string) (string, bool) {
	for i := range tiers {
		if strings.EqualFold(tiers[i].ScaleSetName(), name) {
			return groupOrDefault(tiers[i].RunnerGroup), true
		}
	}

	return "", false
}

// tiersOnTarget narrows the config's tiers to one target's, so a scale-set name
// several targets declare can be torn down on one of them, and a set one target
// no longer declares is undeclared there whatever the others say.
func TiersOnTarget(cfg *config.Config, name string) ([]config.Tier, error) {
	target, err := app.TargetByName(cfg, name)
	if err != nil {
		return nil, err
	}

	var out []config.Tier

	for i := range cfg.Tiers {
		if resolved, ok := cfg.TierTarget(&cfg.Tiers[i]); ok && resolved.Name == target.Name {
			out = append(out, cfg.Tiers[i])
		}
	}

	return out, nil
}

// tierDisplay names a tier for an operator, with the runs-on name workflows use
// when it is not the label.
func TierDisplay(t *config.Tier) string {
	if t.ScaleSetName() == t.Label {
		return t.Label
	}

	return t.Label + " (runs-on " + t.ScaleSetName() + ")"
}
