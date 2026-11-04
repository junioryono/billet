package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// TargetPlaceholder is what a multi-target tier's label carries, and each tier
// it expands to has replaced with its target's name.
//
// WRITTEN BY THE OPERATOR RATHER THAN DERIVED. A tier's label is its identity in
// the ledger, its leases, the advertisement and every status line, so the name
// each target's tier gets is one the file says in so many words, not one a rule
// the reader has to know produced. Spelled so YAML reads it unquoted: a value
// beginning with `{` is a flow mapping.
const TargetPlaceholder = "${target}"

// TierTargets is which targets one tier declaration serves: every target the
// config declares (`targets: all`), or the targets it names (`targets: [a, b]`).
//
// A SCALAR AND A LIST, AND THE TWO NEVER MEAN THE SAME THING. `all` is the one
// scalar accepted; a list is names only, so a target that happens to be called
// "all" is still reached by `targets: [all]` and is never confused with every
// target.
type TierTargets struct {
	All   bool
	Names []string
	// written is a `targets:` key read from a file, so `targets: []` is an empty
	// declaration refused by name rather than the absence of one.
	written bool
}

// WritesTargets reports whether the tier writes `targets` at all, rather than
// `target:` or nothing.
func (t Tier) WritesTargets() bool {
	return t.Targets.written || t.Targets.All || len(t.Targets.Names) > 0
}

// UnmarshalYAML reads `all` or a list of target names.
func (s *TierTargets) UnmarshalYAML(n *yaml.Node) error {
	switch n.Kind {
	case yaml.ScalarNode:
		if n.Value != "all" {
			return fmt.Errorf("line %d: targets is `all` or a list of target names, not %q",
				n.Line, n.Value)
		}

		*s = TierTargets{All: true, written: true}

		return nil
	case yaml.SequenceNode:
		var names []string
		if err := n.Decode(&names); err != nil {
			return err
		}

		*s = TierTargets{Names: names, written: true}

		return nil
	default:
		return fmt.Errorf("line %d: targets is `all` or a list of target names", n.Line)
	}
}

// MarshalYAML writes the declaration the way it is read.
func (s TierTargets) MarshalYAML() (any, error) {
	if s.All {
		return "all", nil
	}

	return s.Names, nil
}

// expandTierTargets turns every multi-target declaration into one tier per
// target, in the order the targets are declared.
//
// AFTER `sizes` AND BEFORE DEFAULTS AND VALIDATION, for ExpandTierSizes' reason:
// defaultTierTargets, materializeCacheScope and every rule downstream judge the
// tiers that will exist. A method, because which targets there are is the
// config's to say.
//
// A FILE THAT DECLARES NO TARGETS KEEPS ONE TIER PER TARGET THE DECLARATION
// NAMES, or one for the default target under `all`. A node's file carries no
// github block and the node never launches from the catalogue (the control
// plane sends each launch's tier), but it does read it: the image reaper
// protects the generations its tiers pin, and the host's preflight sizes their
// disks. Dropping the declaration there would leave a pinned generation
// unprotected, so the node keeps a tier of each shape under names it can hold,
// and validateTierTargets leaves their targets to the control plane as it does
// a node-only file's other tiers.
func (c *Config) expandTierTargets() error {
	declared := c.GitHubTargets()

	names := make([]string, 0, len(declared))
	for _, t := range declared {
		names = append(names, t.Name)
	}

	expanded, err := ExpandTierTargets(c.Tiers, names)
	if err != nil {
		return err
	}

	c.Tiers = expanded

	return nil
}

// ExpandTierTargets expands every tier that writes `targets` into one tier per
// target among targets, the names the config declares. A tier that does not is
// returned unchanged, and the result is a new slice.
//
// Exported for the reason ExpandTierSizes is: alloc.New takes a catalogue
// directly and refuses a declaration that never went through here.
func ExpandTierTargets(tiers []Tier, targets []string) ([]Tier, error) {
	out := make([]Tier, 0, len(tiers))

	var errs []error

	// made is which declaration made a label, and for which target.
	type made struct{ declaration, target string }

	fromDeclaration := make(map[string]made)

	for i := range tiers {
		t := tiers[i]
		where := tierWhere(t)

		if !t.WritesTargets() {
			if strings.Contains(t.Label, TargetPlaceholder) || strings.Contains(t.RunsOn, TargetPlaceholder) {
				errs = append(errs, fmt.Errorf("%s: %s is replaced by each target's name only in a "+
					"tier that writes `targets`, and this one does not", where, TargetPlaceholder))

				continue
			}

			out = append(out, t)

			continue
		}

		serves, err := declarationTargets(where, t, targets)
		if err != nil {
			errs = append(errs, err)

			continue
		}

		for _, name := range serves {
			one := t
			one.Targets = TierTargets{}
			one.Target = name
			one.inventoryOnly = len(targets) == 0
			// THE LABEL AS WRITTEN, which `sizes` has already suffixed when the
			// entry was a ladder; expandOne kept the written one.
			one.Declaration = t.Declaration
			if one.Declaration == "" {
				one.Declaration = t.Label
			}
			one.Label = strings.ReplaceAll(t.Label, TargetPlaceholder, name)
			one.RunsOn = strings.ReplaceAll(t.RunsOn, TargetPlaceholder, name)

			// CLONED, for expandOne's reason: a Tier holds slices and maps, and one
			// target's tier must not share another's backing storage.
			one.Workflows = slices.Clone(t.Workflows)
			one.Providers = slices.Clone(t.Providers)
			one.Command = slices.Clone(t.Command)
			one.Launch = cloneLaunch(t.Launch)

			if other, dup := fromDeclaration[one.Label]; dup {
				errs = append(errs, fmt.Errorf("tier %q is made twice, by %q and %q; each target's "+
					"tier needs a label of its own", one.Label, other.declaration, one.Declaration))
			}

			fromDeclaration[one.Label] = made{declaration: one.Declaration, target: name}

			out = append(out, one)
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	// A HAND-WRITTEN TIER WITH AN EXPANDED TIER'S LABEL is named with the
	// declaration that made it, because validateTiers' duplicate-label message
	// would name only the label, and the operator wrote that label nowhere.
	seen := make(map[string]bool, len(out))
	for i := range out {
		label := out[i].Label
		if seen[label] {
			if by, ok := fromDeclaration[label]; ok {
				return nil, fmt.Errorf("tier %q is defined more than once: the declaration %q makes "+
					"it for target %q, and another tier is spelled that way", label, by.declaration, by.target)
			}
		}

		seen[label] = true
	}

	return out, nil
}

// declarationTargets checks one declaration and returns the targets it serves.
func declarationTargets(where string, t Tier, targets []string) ([]string, error) {
	var errs []error

	if t.Target != "" {
		errs = append(errs, fmt.Errorf("%s: target and targets are both written, which are two "+
			"spellings of which targets the tier serves; keep one", where))
	}

	if !strings.Contains(t.Label, TargetPlaceholder) {
		errs = append(errs, fmt.Errorf("%s: a tier that writes `targets` becomes one tier per "+
			"target, and each needs a label of its own; write %s in the label where the target's "+
			"name goes, for example %q", where, TargetPlaceholder, TargetPlaceholder+"-"+t.Label))
	}

	// THE SCALE SET'S NAME IS WRITTEN, because the label default would give each
	// target's scale set its own name, and one `runs-on` name across every target
	// is the reason to write one declaration rather than several.
	if t.RunsOn == "" {
		errs = append(errs, fmt.Errorf("%s: runs_on is required in a tier that writes `targets`; it "+
			"is the name workflows put in `runs-on`, and without it each target's scale set would "+
			"be named after its own label", where))
	}

	// A CACHE SCOPE BELONGS TO ONE OWNER, and TierTargetPolicyErrors keeps a
	// tier's scope inside its target's, so one written here would be refused on
	// every target but one. A repository target's is derived where it can be.
	if t.CacheScope != nil {
		errs = append(errs, fmt.Errorf("%s: cache_scope names one owner and repository, and a tier "+
			"that writes `targets` serves several; remove it (a repository target's scope is "+
			"derived), or write that target's tier by hand", where))
	}

	if len(t.Sizes) > 0 {
		errs = append(errs, fmt.Errorf("%s: `sizes` was not expanded before `targets`", where))
	}

	var serves []string

	switch {
	case len(targets) == 0 && t.Targets.All:
		// A node's file: see expandTierTargets.
		serves = []string{DefaultTargetName}
	case t.Targets.All:
		serves = slices.Clone(targets)
	default:
		seen := make(map[string]bool, len(t.Targets.Names))

		for _, name := range t.Targets.Names {
			switch {
			case seen[name]:
				errs = append(errs, fmt.Errorf("%s: targets lists %q twice", where, name))
			case len(targets) > 0 && !slices.Contains(targets, name):
				errs = append(errs, fmt.Errorf("%s: targets names %q, which is not one this config "+
					"declares (%s)", where, name, strings.Join(targets, ", ")))
			default:
				serves = append(serves, name)
			}

			seen[name] = true
		}
	}

	if len(t.Targets.Names) == 0 && !t.Targets.All {
		errs = append(errs, fmt.Errorf("%s: targets is an empty list; write `all` or the targets "+
			"this tier serves", where))
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	return serves, nil
}
