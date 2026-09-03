package main

import (
	"errors"
	"fmt"

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
// scale set after its tier and labels it with the same string, so the name is
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

	for i := range tiers {
		if tiers[i].Label == tier {
			// Its own runner group, not the flag's: the config still describes
			// this one, and letting a flag override it would delete from a group
			// the tier was never in while reporting success.
			if group != "" && groupOrDefault(group) != groupOrDefault(tiers[i].RunnerGroup) {
				return nil, false, fmt.Errorf(
					"tier %q is declared with runner group %q, so --runner-group %q would "+
						"look in the wrong place; drop the flag to use the declared group",
					tier, groupOrDefault(tiers[i].RunnerGroup), group)
			}

			return []config.Tier{tiers[i]}, false, nil
		}
	}

	if group == "" {
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
