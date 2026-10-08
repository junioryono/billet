package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/scaleset"
)

// cmdTeardown removes the scale sets billet created.
//
// It exists because there is no other way to remove them. A scale set created
// through the API has no delete control in GitHub's UI — the org's runner list
// shows it with no options menu, and its detail page offers statistics and
// nothing else. Without this command, billet creates objects on somebody's
// organization that they cannot clean up by hand.
//
// Deliberately NOT part of any automatic path. Nothing about stopping the server
// should delete a scale set: an operator restarting billet, or running it on a
// second host, would find their tiers dismantled underneath them. Teardown is a
// thing an operator asks for, once, on purpose.
func cmdTeardown(ctx context.Context, env cli.Env, args []string) error {
	fs := cli.NewFlagSet("billet teardown", env.Stdout)
	cfgPath := cli.AddConfigFlag(fs)
	tier := fs.String("tier", "", "delete the scale set with this name (a tier's runs_on, which defaults to its label)")
	all := fs.Bool("all", false, "delete every tier's scale set")
	force := fs.Bool("force", false,
		"delete even if the scale set's labels are not this tier's (requires --tier)")
	group := fs.String("runner-group", "",
		"the runner group to look in, for a --tier the config no longer declares")
	targetName := fs.String("target", "",
		"the one target to act on for --tier (default: every target declaring it, or the only one)")
	yes := fs.Bool("yes", false, "skip the confirmation prompt")

	if err := cli.Parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	// Checked HERE rather than inside the client. config.Load accepts a node-only
	// config with no github section, and everything below resolves a target — so
	// without this a node config fails obscurely instead of explaining itself.
	if len(cfg.GitHubTargets()) == 0 {
		return fmt.Errorf("%s has no github section and no targets, so it names no "+
			"organization or repository to delete anything from", *cfgPath)
	}

	// "Delete everything" is NEVER the default for a destructive command. An omitted
	// --tier is indistinguishable from `--tier "$TIER"` with TIER unset, so a script
	// with an empty variable would delete every scale set in the org while looking
	// like it asked for one.
	switch {
	case *all && *tier != "":
		return errors.New("pass either --tier or --all, not both")
	case *all && *force:
		return errors.New("--force applies to one scale set at a time, so it needs --tier")
	case !*all && *tier == "":
		return errors.New("name a tier with --tier, or pass --all to delete every tier's " +
			"scale set")
	}

	if *all && *targetName != "" {
		return errors.New("--target scopes one --tier; --all walks every target")
	}

	// --target SCOPES THE NAME TO ONE TARGET. Several targets may declare one
	// scale-set name, and one may have stopped declaring it while another still
	// does; unscoped, the name matches the live set and the leftover stays.
	candidates := cfg.Tiers
	if *targetName != "" {
		if candidates, err = tiersOnTarget(cfg, *targetName); err != nil {
			return err
		}
	}

	wanted, undeclared, err := teardownTargets(candidates, *tier, *group, *force)
	if err != nil {
		return err
	}

	// SAID OUT LOUD, because this is the one path that acts on something the
	// config does not describe. The operator is deleting by name in a group they
	// named, and nothing was cross-checked against a tier definition.
	if undeclared {
		fmt.Fprintf(env.Stdout, "%q is not a tier in %s. Deleting it by name from runner group %q.\n\n",
			*tier, *cfgPath, groupOrDefault(*group))
	}

	// EVERY TARGET IN CONFIG ORDER, each with its own client and its own
	// confirmation: a scale set lives on exactly one owner, and the credential
	// that deletes it is that owner's App.
	for _, target := range cfg.GitHubTargets() {
		mine := make([]config.Tier, 0, len(wanted))

		for i := range wanted {
			t := &wanted[i]

			switch {
			case undeclared:
				// An undeclared tier names its target with --target, or has the
				// only target there is.
				resolved, err := targetByName(cfg, *targetName)
				if err != nil {
					return err
				}

				if resolved.Name == target.Name {
					mine = append(mine, *t)
				}
			default:
				resolved, ok := cfg.TierTarget(t)
				if ok && resolved.Name == target.Name {
					mine = append(mine, *t)
				}
			}
		}

		if len(mine) == 0 {
			continue
		}

		if err := teardownOnTarget(ctx, env, cfg, target, mine, *force, *yes); err != nil {
			return err
		}
	}

	fmt.Fprintln(env.Stdout, "Done.")

	return nil
}

// teardownOnTarget removes the wanted scale sets from one target.
func teardownOnTarget(
	ctx context.Context, env cli.Env, cfg *config.Config, target config.GitHubTarget,
	wanted []config.Tier, force, yes bool,
) error {
	client, err := newScaleSetClientFor(ctx, cfg, target)
	if err != nil {
		return err
	}

	path := target.Path()

	// The ACTUAL objects, fetched before anything is destroyed. An operator
	// confirming a destructive act should be shown what is on GitHub, not the
	// names they typed into their own config.
	fmt.Fprintf(env.Stdout, "This deletes the following from %s (target %s):\n\n", describeGitHubTarget(target), target.Name)

	present := make([]config.Tier, 0, len(wanted))

	for i := range wanted {
		t := &wanted[i]

		set, labels, err := client.Describe(ctx, t.ScaleSetName(), t.RunnerGroup)
		if err != nil {
			return err
		}

		if set == nil {
			fmt.Fprintf(env.Stdout, "  %-32s not present\n", t.ScaleSetName())

			if err := forgetScaleSet(ctx, cfg, path, groupOrDefault(t.RunnerGroup), t.ScaleSetName()); err != nil {
				fmt.Fprintf(env.Stdout, "  %-32s billet could not forget it (%v); the control plane "+
					"will keep reporting it\n", t.ScaleSetName(), err)
			}

			continue
		}

		present = append(present, *t)

		fmt.Fprintf(env.Stdout, "  %-32s id %d, group %s, labels %v\n", t.ScaleSetName(), set.ID, set.Group, labels)
	}

	if len(present) == 0 {
		fmt.Fprintln(env.Stdout, "\nNothing to do here.")

		return nil
	}

	fmt.Fprintln(env.Stdout, "\nRunners already registered to them are removed by GitHub.")

	if !yes {
		if err := confirmTarget(ctx, env, path); err != nil {
			return err
		}
	}

	for i := range present {
		t := &present[i]

		deleted, err := client.DeleteScaleSet(ctx, t.ScaleSetName(), t.RunnerGroup, []string{t.ScaleSetName()}, force)
		if err != nil {
			return err
		}

		// Reported distinctly. Absence is scoped to the runner group being asked
		// about, so a tier created under a different group reports "not present"
		// here while the original survives — and an operator who reads that as
		// "deleted" walks away from an object that is still there.
		if !deleted {
			fmt.Fprintf(env.Stdout, "%s: nothing in runner group %q; if it was created under a different "+
				"group it is still there\n", t.ScaleSetName(), groupOrDefault(t.RunnerGroup))

			continue
		}

		if err := forgetScaleSet(ctx, cfg, path, groupOrDefault(t.RunnerGroup), t.ScaleSetName()); err != nil {
			fmt.Fprintf(env.Stdout, "%s: deleted, but billet could not forget it had created it (%v); "+
				"the control plane will keep reporting it until this is cleared\n", t.ScaleSetName(), err)
		}
	}

	return nil
}

// groupOrDefault names the runner group a tier resolves to, for messages.
func groupOrDefault(group string) string {
	if group == "" {
		return scaleset.DefaultRunnerGroup
	}

	return group
}
