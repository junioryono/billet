package cache

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
)

// cmdCacheGit is `billet cache git`: what an operator does to the node's git
// mirrors by hand.
func cmdCacheGit(_ context.Context, env cli.Env, args []string) error {
	if len(args) == 0 || args[0] != "forget" {
		return errors.New("usage: billet cache git forget --repository <owner/repo>")
	}
	flags := cli.NewFlagSet("billet cache git forget", env.Stdout)
	cfgPath := cli.AddConfigFlag(flags)
	repository := flags.String("repository", "", "the GitHub owner/repository whose mirrors this node drops")
	if err := cli.Parse(flags, args[1:]); err != nil {
		return err
	}
	owner, repo, ok := strings.Cut(*repository, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return errors.New("--repository must be owner/repository")
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Node == nil {
		return fmt.Errorf("%s has no node section: run this on the node whose mirrors it should drop", *cfgPath)
	}

	forgotten, err := app.ForgetGitMirrors(cfg, owner, repo)
	for _, mirror := range forgotten {
		fmt.Fprintf(env.Stdout, "forgot the git mirror %s: nothing more is served from it, and the node removes "+
			"it at the repository's next fetch or a cleanup, which it tries every six hours and puts off while a "+
			"fetch is being served from it\n", mirror)
	}
	if err != nil {
		return err
	}
	if len(forgotten) == 0 {
		fmt.Fprintf(env.Stdout, "this node keeps no git mirror of %s\n", *repository)
	}

	return nil
}
