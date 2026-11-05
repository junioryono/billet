package host

import (
	"context"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
)

// checkPolicyClient is the client `billet check` asks about a target's runner groups:
// on the deployment's App key, or on the tokens its broker mints.
func checkPolicyClient(ctx context.Context, cfg *config.Config, target config.GitHubTarget,
) (github.RunnerGroupPolicyClient, error) {
	if cfg.Server != nil && cfg.Server.TokenBroker != nil {
		broker, err := app.NewBrokerClient(cfg)
		if err != nil {
			return nil, err
		}

		return github.NewRunnerGroupPolicyClientWith(app.GitHubAPIBase, app.GitHubTargetOf(target), target.AppID,
			target.InstallationID, app.TargetTokens(broker, target)), nil
	}

	key, err := app.ResolveAppKey(ctx, cfg, target)
	if err != nil {
		return nil, err
	}

	return github.NewRunnerGroupPolicyClientAt(app.GitHubAPIBase, app.GitHubTargetOf(target), target.AppID,
		target.InstallationID, key), nil
}

// checkBrokerTokens is the check's GitHub line for a deployment holding no key: the
// broker minted a token for this target, and the token holds what billet asked for.
//
// WHAT IT CANNOT SAY, IT SAYS. Whether the App is installed there and not suspended
// takes the App's JWT to ask, so that is the broker's to prove, which it does before
// it mints. A broker that did not answer is could-not-tell, and never a verdict.
func checkBrokerTokens(ctx context.Context, env cli.Env, cfg *config.Config, target config.GitHubTarget,
	skipNetworkProbes bool,
) (githubVerdict, error) {
	if skipNetworkProbes {
		fmt.Fprintf(env.Stdout, "github   (verification skipped during maintenance)\n")

		return githubSkipped, nil
	}

	broker, err := app.NewBrokerClient(cfg)
	if err != nil {
		return githubFailed, err
	}

	token, err := app.TargetTokens(broker, target).InstallationToken(ctx)

	switch {
	case err == nil:
	case errors.Is(err, github.ErrTokenUnavailable), github.Undecided(err):
		fmt.Fprintf(env.Stdout, "github   UNVERIFIED: %v\n", err)
		fmt.Fprintf(env.Stdout, "         (the broker may be fine, and the App with it; this says the check "+
			"could not reach a verdict, and nothing may treat it as one)\n")

		return githubUnverifiable, nil
	default:
		fmt.Fprintf(env.Stdout, "github   FAILED: %v\n", err)

		return githubFailed, err
	}

	held := github.Installation{Permissions: token.Permissions()}
	if problems := held.PermissionMismatches(app.GitHubTargetOf(target).Scope(),
		cfg.TargetNeedsRunEvidence(target.Name)); len(problems) > 0 {
		err := fmt.Errorf("github: the broker's token for %s does not hold what billet asks for: %v",
			target.Path(), problems)
		fmt.Fprintf(env.Stdout, "github   FAILED: %v\n", err)

		return githubFailed, err
	}

	fmt.Fprintf(env.Stdout, "github   verified: the broker issued a token for %s, with exactly the permissions "+
		"requested for that %s; the broker proves the installation itself, with the key it holds\n", target.Path(), target.Scope())

	// A KEY'S TOKEN CARRIES ALL THE APP HOLDS, AND A BROKER'S ONLY WHAT ITS STANZA GRANTS, so
	// this can be missing here on an App that has it. Nothing fails without it but the
	// numbers: PermissionMismatches above has already refused a tier that needs it.
	if _, ok := token.Permissions()["actions"]; !ok {
		fmt.Fprintf(env.Stdout, "         WARNING: this token cannot read GitHub's job records, so every job keeps an "+
			"unknown queue time; add `actions_read: true` to this deployment's stanza in the broker's file\n")
	}

	return githubVerified, nil
}

// brokerAccessNote is gitHubAccessNote's paragraph for a target with no key here.
func brokerAccessNote(ctx context.Context, cfg *config.Config, target config.GitHubTarget) string {
	broker, err := app.NewBrokerClient(cfg)
	if err != nil {
		return fmt.Sprintf("billet could not reach this host's token broker to find out why: %v", err)
	}

	_, err = app.TargetTokens(broker, target).InstallationToken(ctx)

	switch {
	case err == nil || ctx.Err() != nil:
		return ""
	case errors.Is(err, github.ErrTokenUnavailable), github.Undecided(err):
		return fmt.Sprintf("billet asked the token broker for target %s and got no answer, so this may be "+
			"unrelated: %v", target.Name, err)
	default:
		return fmt.Sprintf("billet also asked the token broker, in case the failure above was a credential "+
			"problem. It is not healthy:\n  %v\n\n`billet check --config <path>` reports the same thing without "+
			"starting a control plane.", err)
	}
}
