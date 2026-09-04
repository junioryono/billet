package wiring

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/junioryono/godi/v5"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsssm"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
	"github.com/junioryono/billet/internal/wireshare"
)

// Deployment is the identity every piece of compute carries and every ledger
// row is bound to. Its own type so nothing can hand a constructor a hostname
// in its place, which happened once and failed silently on every boot order.
type Deployment string

// IdentityModule registers the deployment identity, minting it when the
// directory has none.
//
// FOUNDED HERE IN THE ORDINARY CASE, before the database is opened: whichever
// role starts first mints it and the other reads that same file. It is a module
// of its own because minting is a side effect an operator command reporting on
// a deployment must not have; the ledger's operator mode PEEKS instead.
func IdentityModule() godi.ModuleOption {
	return godi.NewModule("identity",
		godi.AddSingleton(newDeployment),
	)
}

func newDeployment(cfg *config.Config) (Deployment, error) {
	if cfg.Server == nil {
		return "", errors.New("wiring: the deployment identity lives in the server's identity directory")
	}

	id, err := state.DeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return "", err
	}

	return Deployment(id), nil
}

// AuthorityStore is the deployment's identity store, or nothing when the
// authority is kept as files.
//
// A STRUCT WITH A NIL FIELD RATHER THAN AN OPTIONAL REGISTRATION. godi's
// `optional` hands a constructor the zero value when nothing is registered,
// which for an interface is a nil that reads as "keep going"; here nil is a
// stated answer ("this deployment keeps its authority on disk") that every
// consumer checks by name.
type AuthorityStore struct {
	Store wireshare.Store
}

// authorityParameter is the leaf the node-wire authority lives under.
//
// BESIDE THE APP KEY AND UNDER THE SAME PREFIX, so one IAM statement covers a
// deployment's identity and two deployments cannot reach each other's.
const authorityParameter = "node-wire-authority"

// AuthorityModule registers the identity store.
func AuthorityModule() godi.ModuleOption {
	return godi.NewModule("authority",
		godi.AddSingleton(newAuthorityStore),
	)
}

func newAuthorityStore(cfg *config.Config, creds awscreds.Source) AuthorityStore {
	return AuthorityStore{Store: AuthorityStoreFor(cfg, creds)}
}

// AuthorityStoreFor is the deployment's identity store, or nil when it keeps
// its authority as files.
func AuthorityStoreFor(cfg *config.Config, creds awscreds.Source) wireshare.Store {
	if cfg.Server == nil || cfg.Server.IdentityBackendKind() != config.IdentitySSM {
		return nil
	}

	return newSSMAuthorityStore(cfg.Server.IdentitySSM(), creds)
}

// ssmAuthorityStore is wireshare.Store over Parameter Store.
//
// A THIN ADAPTER ON PURPOSE. Every rule about what may be published, what may be
// adopted and what must be refused lives in internal/wireshare, where it is
// testable without AWS; this translates two method calls and one sentinel.
type ssmAuthorityStore struct {
	client *awsssm.Client
	name   string
	kms    string
}

func newSSMAuthorityStore(ssm *config.IdentitySSMConfig, creds awscreds.Source) *ssmAuthorityStore {
	return &ssmAuthorityStore{
		client: awsssm.New(ssm.Region, creds),
		name:   awsssm.PathFor(ssm.Prefix, authorityParameter),
		kms:    ssm.KMSKeyID,
	}
}

func (s *ssmAuthorityStore) GetAuthority(ctx context.Context) ([]byte, error) {
	param, err := s.client.Get(ctx, s.name)
	if err != nil {
		if errors.Is(err, awsssm.ErrNotFound) {
			// TRANSLATED RATHER THAN PASSED THROUGH, so wireshare's rules are
			// written against its own sentinel and do not depend on which store a
			// deployment chose.
			return nil, wireshare.ErrNoAuthority
		}

		return nil, err
	}

	return []byte(param.Value), nil
}

func (s *ssmAuthorityStore) PutAuthority(ctx context.Context, body []byte) error {
	_, err := s.client.Put(ctx, s.name, string(body), awsssm.PutOptions{
		// OVERWRITE, WHICH IS THE ONE PLACE billet REPLACES ANY OF THIS. What is
		// being replaced is a COPY (the authority itself is the file layout on
		// each host) and refusing here would make a rotation unpublishable.
		Overwrite: true,
		KMSKeyID:  s.kms,
		Description: "billet node-wire certificate authority; every node in this " +
			"deployment's fleet verifies its control plane against this",
	})

	return err
}

// WithAuthorityLock runs fn while this host holds the authority lock.
//
// ONE PLACE THAT TAKES IT, so callers cannot disagree about whether they hold
// it, and none of them may call another that takes it, because a second flock
// on a separate descriptor in the SAME process is denied (measured, darwin).
func WithAuthorityLock(dir string, log *slog.Logger, fn func(dir string) error) error {
	lock, err := wirecert.LockAuthority(dir)
	if err != nil {
		return err
	}

	defer func() {
		if err := lock.Release(); err != nil {
			log.Warn("could not release the authority lock", "error", err)
		}
	}()

	return fn(dir)
}

// AdoptAuthority gives this host the authority the deployment already uses,
// before anything reads one.
//
// A NO-OP unless this deployment keeps its identity in a store. It takes the
// authority lock, because it writes the five files a rotation mutates in
// sequence.
func AdoptAuthority(
	ctx context.Context, store wireshare.Store, dir string, deployment Deployment, log *slog.Logger,
) error {
	if store == nil {
		return nil
	}

	return WithAuthorityLock(dir, log, func(dir string) error {
		adopted, err := wireshare.Adopt(ctx, store, dir, string(deployment), false)
		if err != nil {
			return err
		}

		if adopted == wireshare.AdoptedInstalled {
			log.Info("adopted this deployment's node-wire authority from the identity store")
		}

		return nil
	})
}

// PublishAuthority puts what this host holds into the store.
//
// A FAILURE HERE IS REPORTED AND NOT FATAL. The control plane is already
// serving by the time this runs, and a store that cannot be written is a reason
// to look at IAM rather than to take a working deployment offline; what it
// costs is that the OTHER host has nothing to adopt yet, which `billet ca sync`
// and `billet check` both say.
func PublishAuthority(
	ctx context.Context, store wireshare.Store, dir string, deployment Deployment, log *slog.Logger,
) {
	if store == nil {
		return
	}

	err := WithAuthorityLock(dir, log, func(dir string) error {
		return wireshare.Publish(ctx, store, dir, string(deployment))
	})
	if err != nil {
		log.Error("could not publish this deployment's node-wire authority to the identity "+
			"store, so a second controller has nothing to adopt", "error", err)

		return
	}

	log.Info("published this deployment's node-wire authority to the identity store")
}

// PublishRotatedAuthority carries a rotation or a retirement into the store,
// returning the error for the caller to report rather than acting on it: by
// the time this runs the operation on THIS host is complete and irreversible.
func PublishRotatedAuthority(
	ctx context.Context, store wireshare.Store, dir string, deployment Deployment, log *slog.Logger,
) (bool, error) {
	if store == nil {
		return false, nil
	}

	err := WithAuthorityLock(dir, log, func(dir string) error {
		return wireshare.Publish(ctx, store, dir, string(deployment))
	})
	if err != nil {
		return false, err
	}

	return true, nil
}

// IssuerModule registers the deployment's certificate authority for the
// commands that ISSUE: `billet ca *` and `billet nodes approve`.
//
// ITS OWN MODULE because wirecert.LoadOrCreateCA CREATES an authority when the
// directory is empty, which is right for the command minting the first bundle
// and wrong for a status report; the control plane reads its authority through
// the node wire's single read instead.
func IssuerModule() godi.ModuleOption {
	return godi.NewModule("issuer",
		godi.AddSingleton(newIssuer),
	)
}

func newIssuer(cfg *config.Config, deployment Deployment) (*wirecert.CA, error) {
	if cfg.Server == nil {
		return nil, errors.New("wiring: the authority lives in the server's identity directory")
	}

	ca, err := wirecert.LoadOrCreateCA(cfg.Server.IdentityDir, string(deployment))
	if err != nil {
		return nil, fmt.Errorf("node-wire authority: %w", err)
	}

	return ca, nil
}
