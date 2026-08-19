package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
)

func cmdCache(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "disable" && args[0] != "enable" {
		return errors.New("usage: billet cache <disable|enable> (--org <owner> | --repository <owner/repository>)")
	}
	action := args[0]
	fs := newFlagSet("billet cache " + action)
	cfgPath := addConfigFlag(fs)
	organisation := fs.String("org", "", "GitHub organisation whose repositories this policy covers")
	repository := fs.String("repository", "", "GitHub owner/repository this policy covers")
	if err := parse(fs, args[1:]); err != nil {
		return err
	}
	scope, label, err := cachePolicyScope(*organisation, *repository)
	if err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	if cfg.Server == nil {
		return fmt.Errorf("%s has no server section, so it names no central cache policy", *cfgPath)
	}
	db, err := state.OpenAdmin(ctx, cfg.Server.StateDir)
	if err != nil {
		return err
	}
	defer db.Close()
	enabled := action == "enable"
	if err := db.SetActionsCacheEnabled(ctx, scope, enabled); err != nil {
		return err
	}
	status := "disabled"
	if enabled {
		status = "enabled"
	}
	fmt.Printf("transparent Actions caching is %s for %s\n", status, label)

	return nil
}

func cachePolicyScope(organisation, repository string) (state.ActionsCacheScope, string, error) {
	if (organisation == "") == (repository == "") {
		return state.ActionsCacheScope{}, "", errors.New("choose exactly one of --org or --repository")
	}
	if organisation != "" {
		return state.ActionsCacheScope{Owner: organisation}, "organisation " + organisation, nil
	}
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return state.ActionsCacheScope{}, "", errors.New("--repository must be owner/repository")
	}

	return state.ActionsCacheScope{Owner: owner, Repository: name}, "repository " + repository, nil
}
