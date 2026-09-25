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
	if len(args) > 0 && args[0] == "conformance" {
		return cmdCacheConformance(ctx, args[1:])
	}

	if len(args) > 0 && args[0] == "status" {
		return cmdCacheStatus(ctx, args[1:])
	}

	// Run inside a guest by the go command, bazel and git, never by a person.
	if len(args) > 0 && args[0] == "gocacheprog" {
		return cmdCacheGoCacheProg(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "credential-helper" {
		return cmdCacheCredentialHelper(ctx, args[1:])
	}
	if len(args) > 0 && args[0] == "git-credential" {
		return cmdCacheGitCredential(ctx, args[1:])
	}

	if len(args) == 0 || args[0] != "disable" && args[0] != "enable" {
		return errors.New("usage: billet cache <disable|enable|status|conformance> [flags]")
	}
	action := args[0]
	fs := newFlagSet("billet cache " + action)
	cfgPath := addConfigFlag(fs)
	organisation := fs.String("org", "", "GitHub organisation whose repositories this policy covers")
	repository := fs.String("repository", "", "GitHub owner/repository this policy covers")
	kindFlag := fs.String("kind", "all", "the cache this policy covers: "+
		"docker, sticky, actions, git, bazel, go, or all")
	if err := parse(fs, args[1:]); err != nil {
		return err
	}
	scope, label, err := cachePolicyScope(*organisation, *repository)
	if err != nil {
		return err
	}
	kind, what, err := cachePolicyKind(*kindFlag)
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
	db, err := openStateAdmin(ctx, cfg)
	if err != nil {
		return err
	}
	defer db.Close()
	enabled := action == "enable"
	if err := db.SetCacheEnabled(ctx, kind, scope, enabled); err != nil {
		return err
	}
	status := "disabled"
	if enabled {
		status = "enabled"
	}
	fmt.Printf("%s %s for %s\n", what, status, label)
	if enabled && kind != state.AllCaches {
		fmt.Printf("(a block for every cache on the same scope, if there is one, still stands; " +
			"remove it with --kind all)\n")
	}

	return nil
}

// cachePolicyKind is the ledger's kind for a --kind flag, and how to say it.
func cachePolicyKind(flag string) (string, string, error) {
	if flag == "all" {
		return state.AllCaches, "every cache is", nil
	}
	if kind := config.CacheKind(flag); kind.Valid() {
		return flag, "the " + flag + " cache is", nil
	}

	return "", "", fmt.Errorf("--kind %q is not one of docker, sticky, actions, git, bazel, go, all", flag)
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
