package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsssm"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
)

// appKeyParameter is the leaf the App private key lives under in the identity
// store.
//
// ONE NAME, DERIVED IN ONE PLACE. A path spelled at each call site is a path that
// eventually differs by one character between the command that writes it and the
// command that reads it, and the failure is a control plane that cannot
// authenticate against an organization while a perfectly good key sits in the
// account under a name nothing looks at.
const appKeyParameter = "github-app-key"

// appKeyPath is where one target's App key lives in Parameter Store: the bare
// leaf for the default target, so every deployment onboarded before targets
// existed keeps its key where it was, and a suffixed leaf for the rest.
func appKeyPath(ssm *config.IdentitySSMConfig, target config.GitHubTarget) string {
	return awsssm.PathFor(ssm.Prefix, target.KeyName(appKeyParameter))
}

// resolveAppKey reads one target's GitHub App private key, from wherever this
// deployment keeps it.
//
// ONE RESOLVER FOR EVERY CALLER, and that is the point rather than a tidy-up.
// The key was read at five sites, each naming cfg.GitHub.PrivateKeyPath; a store
// that is not a file would otherwise have to be taught to every one of them, and
// the one that got missed would be the one that decides whether a control plane
// can mint a token. It takes the TARGET rather than the config alone for the
// same reason: a deployment serving several owners holds one key per owner, and
// a reader of "the" key is a reader of one of them.
//
// THE FILE PATH IS UNCHANGED AND STILL GOES THROUGH readPrivateKey, which is the
// validating reader: one descriptor opened O_NONBLOCK so a FIFO cannot hang it, a
// regular file, no group or other permission bits, a bounded read, and actually
// parsed. None of that has an equivalent in a store, where the equivalent is IAM.
func resolveAppKey(ctx context.Context, cfg *config.Config, target config.GitHubTarget) ([]byte, error) {
	if cfg.Server.IdentityBackendKind() != config.IdentitySSM {
		return readPrivateKey(target.PrivateKeyPath)
	}

	ssm := cfg.Server.IdentitySSM()
	path := appKeyPath(ssm, target)

	param, err := awsssm.New(ssm.Region, awscreds.Default()).Get(ctx, path)
	if err != nil {
		if errors.Is(err, awsssm.ErrNotFound) {
			// NAMED, BECAUSE THE REMEDY IS A COMMAND. A deployment configured for the
			// store and carrying no key has either never onboarded or is pointed at
			// another deployment's prefix, and both are things an operator fixes
			// rather than debugs.
			return nil, fmt.Errorf(
				"this deployment keeps its GitHub App keys in Parameter Store and there is "+
					"nothing at %s for target %s. Run `billet github-app create --target %s` to "+
					"register an App and publish its key there, or check that "+
					"server.identity.aws_ssm.prefix names this deployment's path",
				path, target.Name, target.Name)
		}

		return nil, err
	}

	// PARSED HERE TOO, so the store path refuses exactly what the file path
	// refuses. A value that is not a key is a value somebody put there, and
	// finding that out at the first token mint is finding it out on the wire.
	key := []byte(param.Value)
	if err := github.ValidatePrivateKey(key); err != nil {
		return nil, fmt.Errorf(
			"the value at %s is not a usable GitHub App private key: %w", path, err)
	}

	return key, nil
}

// appKeyLocation is what a diagnostic calls the place one target's key lives.
//
// FOR AN OPERATOR'S BENEFIT AND NOTHING ELSE. `billet check` reports which of the
// two a deployment is using, because "the App key is fine" is a different fact
// depending on where it was read from and an operator debugging a failover needs
// to know which one they are looking at.
func appKeyLocation(cfg *config.Config, target config.GitHubTarget) string {
	if cfg.Server.IdentityBackendKind() != config.IdentitySSM {
		return target.PrivateKeyPath
	}

	return "AWS Parameter Store " + appKeyPath(cfg.Server.IdentitySSM(), target)
}
