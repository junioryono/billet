package app

import (
	"context"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
)

// OpenJobRecords builds what an operator command reads one job's GitHub record
// with: the App client of the one target that served the job, reading with
// that target's installation token and nothing else.
//
// AT DISPLAY TIME, never on the control plane's path. It costs one App key read
// and a few requests per `billet jobs show`, and it depends on GitHub still
// holding the job: a run GitHub has deleted has no steps to read.
//
// THE TARGET IS THE JOB'S TIER'S, checked against the job's repository, because
// the installation that can read a repository is the one whose owner holds it.
// A tier the config no longer declares falls back to the one target whose
// owner holds the repository; none or several is refused rather than guessed,
// since a guess between installations asks the wrong owner's App.
func OpenJobRecords(ctx context.Context, cfgPath, tier, repository string) (github.JobRecords, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	target, err := jobTarget(cfg, tier, repository)
	if err != nil {
		return nil, err
	}
	key, err := ResolveAppKey(ctx, cfg, target)
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", target.Name, err)
	}

	return github.NewJobRecordsAt(GitHubAPIBase, GitHubTargetOf(target), target.AppID,
		target.InstallationID, key), nil
}

// jobTarget is the target that served a job of tier in repository.
func jobTarget(cfg *config.Config, tier, repository string) (config.GitHubTarget, error) {
	owner, _, ok := config.SplitRepository(repository)
	if !ok {
		return config.GitHubTarget{}, fmt.Errorf("the job's repository %q is not owner/name", repository)
	}
	if t, declared := cfg.TierByLabel(tier); declared {
		target, ok := cfg.TierTarget(t)
		if !ok {
			return config.GitHubTarget{}, fmt.Errorf("tier %q names no target this config declares", tier)
		}
		if !holds(target, owner, repository) {
			return config.GitHubTarget{}, fmt.Errorf("tier %q belongs to %s, which does not hold %q",
				tier, DescribeGitHubTarget(target), repository)
		}

		return target, nil
	}

	var holders []config.GitHubTarget
	for _, target := range cfg.GitHubTargets() {
		if holds(target, owner, repository) {
			holders = append(holders, target)
		}
	}
	switch len(holders) {
	case 1:
		return holders[0], nil
	case 0:
		return config.GitHubTarget{}, fmt.Errorf("tier %q is no longer declared and no target holds %q",
			tier, repository)
	default:
		return config.GitHubTarget{}, fmt.Errorf("tier %q is no longer declared and %d targets "+
			"could hold %q; refusing to guess between their Apps", tier, len(holders), repository)
	}
}

// holds reports whether a target's installation is the one for a repository:
// the organization that owns it, or that repository itself. GitHub's logins and
// repository names compare without case.
func holds(target config.GitHubTarget, owner, repository string) bool {
	if target.IsRepository() {
		return strings.EqualFold(target.Repository, repository)
	}

	return strings.EqualFold(target.Org, owner)
}
