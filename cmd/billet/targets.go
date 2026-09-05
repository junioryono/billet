package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/scaleset"
	"github.com/junioryono/billet/internal/wiring"
)

// githubTarget is a config target as the github package sees it.
func githubTarget(t config.GitHubTarget) github.Target {
	if t.IsRepository() {
		return github.RepositoryTarget(t.Owner(), t.RepositoryName())
	}

	return github.OrganizationTarget(t.Org)
}

// describeGitHubTarget names a target the way an operator reads it: its scope and
// its GitHub path.
func describeGitHubTarget(t config.GitHubTarget) string {
	if t.IsRepository() {
		return "repository " + t.Repository
	}

	return "org " + t.Org
}

// targetNames lists a config's target names, for a diagnostic.
func targetNames(cfg *config.Config) string {
	names := make([]string, 0, 2)
	for _, t := range cfg.GitHubTargets() {
		names = append(names, t.Name)
	}

	return strings.Join(names, ", ")
}

// targetByName resolves a --target flag against the config.
//
// AN EMPTY NAME MEANS THE ONLY TARGET, which is every deployment written before
// targets existed; with several declared, a command that acts on one has to be
// told which, because guessing between credentials is the mistake the flag
// exists to make impossible.
func targetByName(cfg *config.Config, name string) (config.GitHubTarget, error) {
	targets := cfg.GitHubTargets()
	if len(targets) == 0 {
		return config.GitHubTarget{}, errors.New("the config has no github section and no " +
			"targets list, so it names no organization or repository")
	}

	if name == "" {
		if len(targets) == 1 {
			return targets[0], nil
		}

		return config.GitHubTarget{}, fmt.Errorf("this deployment serves %d targets (%s); name "+
			"one with --target", len(targets), targetNames(cfg))
	}

	target, ok := cfg.GitHubTarget(name)
	if !ok {
		return config.GitHubTarget{}, fmt.Errorf("target %q is not one the config declares (%s)",
			name, targetNames(cfg))
	}

	return target, nil
}

// scaleSetGitHubURL is the web host targets live on, empty for github.com. A
// var beside githubAPIBase for the same reason: a test points both at a fake
// Actions service, and production never sets either.
var scaleSetGitHubURL = ""

// newScaleSetClientFor builds the GitHub client for one target, reading the
// key with the same hardened reader `billet check` uses.
//
// Shared so teardown and the server authenticate identically. A second,
// slightly-different construction is how one of them ends up talking to a
// different owner than the other.
func newScaleSetClientFor(ctx context.Context, cfg *config.Config, target config.GitHubTarget) (*scaleset.Client, error) {
	key, err := resolveAppKey(ctx, cfg, target)
	if err != nil {
		return nil, err
	}

	appIdentity := target.ClientID
	if appIdentity == "" {
		appIdentity = strconv.FormatInt(target.AppID, 10)
	}

	client, err := scaleset.New(scaleset.Config{
		Target:         githubTarget(target),
		GitHubURL:      scaleSetGitHubURL,
		APIURL:         githubAPIBase,
		ClientID:       appIdentity,
		InstallationID: target.InstallationID,
		PrivateKey:     string(key),
		AppID:          target.AppID,
	}, slog.Default())
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", target.Name, err)
	}

	return client, nil
}

// newScaleSetClients builds one client per target, in config order.
//
// ALL OF THEM BEFORE ANY IS USED: a control plane that could reach one owner's
// App and not another's must not start serving the one, because the tiers on
// the other would advertise nothing with nothing saying why.
func newScaleSetClients(ctx context.Context, cfg *config.Config) ([]wiring.Target, error) {
	targets := cfg.GitHubTargets()
	if len(targets) == 0 {
		return nil, errors.New("no github section or targets list in the config")
	}

	out := make([]wiring.Target, 0, len(targets))

	for _, target := range targets {
		client, err := newScaleSetClientFor(ctx, cfg, target)
		if err != nil {
			return nil, err
		}

		out = append(out, wiring.Target{Config: target, Client: client})
	}

	return out, nil
}

// appKeyFilePaths is every target's key path, for the lifecycle commands that
// inspect, contain and preserve the files a deployment is made of. Empty under
// the store backend, where no target names a file.
func appKeyFilePaths(cfg *config.Config) []string {
	var paths []string

	for _, target := range cfg.GitHubTargets() {
		if target.PrivateKeyPath != "" {
			paths = append(paths, target.PrivateKeyPath)
		}
	}

	return paths
}

// confirmTarget makes the operator type the target's GitHub path.
//
// Typed confirmation rather than y/N: this is destructive against somebody's
// organization or repository, and the cost of a stray keystroke is a tier that
// silently stops accepting work.
//
// Read on a goroutine so the context still wins. fmt.Scanln does not observe
// cancellation, so a Ctrl-C at the prompt would otherwise cancel ctx and leave
// the process blocked on stdin.
func confirmTarget(ctx context.Context, path string) error {
	fmt.Printf("\nType the target (%s) to confirm: ", path)

	typed := make(chan string, 1)
	failed := make(chan error, 1)

	go func() {
		var answer string

		if _, err := fmt.Scanln(&answer); err != nil {
			failed <- err

			return
		}

		typed <- answer
	}()

	select {
	case <-ctx.Done():
		fmt.Println()

		return ctx.Err()
	case err := <-failed:
		return fmt.Errorf("teardown cancelled: %w", err)
	case answer := <-typed:
		if answer != path {
			return errors.New("teardown cancelled")
		}

		return nil
	}
}
