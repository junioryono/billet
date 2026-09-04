package wiring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/junioryono/godi/v5"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsssm"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/scaleset"
)

// appKeyParameter is the leaf the App key lives under in Parameter Store.
//
// A FIXED NAME UNDER THE PREFIX, so `billet check` on the standby can read the
// key the primary published without either being told the other's name. A
// per-host name would let two controllers each authenticate against an
// organization while a perfectly good key sits in the account under a name
// nothing looks at.
const appKeyParameter = "github-app-key"

// AppKeyPath is where a deployment's App key lives in Parameter Store.
func AppKeyPath(ssm *config.IdentitySSMConfig) string {
	return awsssm.PathFor(ssm.Prefix, appKeyParameter)
}

// GitHubModule registers the App key, the one scale-set client, and the
// adapters the control plane and the node wire consume it through.
//
// THE KEY IS READ AT BUILD, so a set that includes this module is one whose
// role needs GitHub: the server, teardown, check. An operator command that only
// reads the ledger does not include it, or `billet status` would start
// requiring a readable App key.
func GitHubModule() godi.ModuleOption {
	return godi.NewModule("github",
		godi.AddSingleton(newAppKey),
		godi.AddSingleton(newScaleSetClient),
		godi.AddSingleton(func(client *scaleset.Client) Provisioner { return Provisioner{Client: client} }),
		godi.AddSingleton(func(client *scaleset.Client) NodeJIT { return NodeJIT{Client: client} }),
	)
}

func newAppKey(ctx context.Context, cfg *config.Config, creds awscreds.Source) (github.AppKey, error) {
	if cfg.GitHub == nil {
		return nil, errors.New("no github section in the config")
	}

	return ReadAppKey(ctx, cfg, creds)
}

// ReadAppKey reads the GitHub App private key from wherever this deployment
// keeps it.
//
// ONE RESOLVER FOR EVERY CALLER. The key was read at five sites, each naming
// cfg.GitHub.PrivateKeyPath; a store that is not a file would otherwise have to
// be taught to every one of them, and the one that got missed would be the one
// that decides whether a control plane can mint a token. The file path goes
// through the validating reader: one descriptor opened O_NONBLOCK so a FIFO
// cannot hang it, a regular file, no group or other permission bits, a bounded
// read, and actually parsed. None of that has an equivalent in a store, where
// the equivalent is IAM.
func ReadAppKey(ctx context.Context, cfg *config.Config, creds awscreds.Source) (github.AppKey, error) {
	if cfg.Server == nil || cfg.Server.IdentityBackendKind() != config.IdentitySSM {
		return github.ReadPrivateKeyFile(cfg.GitHub.PrivateKeyPath)
	}

	ssm := cfg.Server.IdentitySSM()

	param, err := awsssm.New(ssm.Region, creds).Get(ctx, AppKeyPath(ssm))
	if err != nil {
		if errors.Is(err, awsssm.ErrNotFound) {
			// NAMED, BECAUSE THE REMEDY IS A COMMAND. A deployment configured for
			// the store and carrying no key has either never onboarded or is
			// pointed at another deployment's prefix, and both are things an
			// operator fixes rather than debugs.
			return nil, fmt.Errorf(
				"this deployment keeps its GitHub App key in Parameter Store and there is "+
					"nothing at %s. Run `billet github-app create` to register an App and "+
					"publish its key there, or check that server.identity.aws_ssm.prefix names "+
					"this deployment's path", AppKeyPath(ssm))
		}

		return nil, err
	}

	// PARSED HERE TOO, so the store path refuses exactly what the file path
	// refuses. A value that is not a key is a value somebody put there, and
	// finding that out at the first token mint is finding it out on the wire.
	key := github.AppKey(param.Value)
	if err := key.Validate(); err != nil {
		return nil, fmt.Errorf(
			"the value at %s is not a usable GitHub App private key: %w", AppKeyPath(ssm), err)
	}

	return key, nil
}

// newScaleSetClient builds the GitHub client from the config and the key.
//
// ONE CONSTRUCTION, so the server and teardown authenticate identically. A
// second, slightly different construction is how one of them ends up talking to
// a different organization than the other.
func newScaleSetClient(cfg *config.Config, key github.AppKey, log *slog.Logger) (*scaleset.Client, error) {
	if cfg.GitHub == nil {
		return nil, errors.New("no github section in the config")
	}

	appIdentity := cfg.GitHub.ClientID
	if appIdentity == "" {
		appIdentity = strconv.FormatInt(cfg.GitHub.AppID, 10)
	}

	return scaleset.New(scaleset.Config{
		ConfigURL:      "https://github.com/" + cfg.GitHub.Org,
		ClientID:       appIdentity,
		InstallationID: cfg.GitHub.InstallationID,
		PrivateKey:     string(key),
		Org:            cfg.GitHub.Org,
		AppID:          cfg.GitHub.AppID,
	}, log)
}
