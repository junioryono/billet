package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsssm"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/wirecert"
	"github.com/junioryono/billet/internal/wireshare"
)

// authorityParameter is the leaf the node-wire authority lives under.
//
// BESIDE THE APP KEY AND UNDER THE SAME PREFIX, so one IAM statement covers a
// deployment's identity and two deployments cannot reach each other's.
const authorityParameter = "node-wire-authority"

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

func newSSMAuthorityStore(ssm *config.IdentitySSMConfig) *ssmAuthorityStore {
	return &ssmAuthorityStore{
		client: awsssm.New(ssm.Region, awscreds.Default()),
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
		// being replaced is a COPY — the authority itself is the file layout on each
		// host — and refusing here would make a rotation unpublishable.
		Overwrite: true,
		KMSKeyID:  s.kms,
		Description: "billet node-wire certificate authority; every node in this " +
			"deployment's fleet verifies its control plane against this",
	})

	return err
}

// authorityStoreFor is the deployment's identity store, or nil when it keeps its
// authority as files.
func authorityStoreFor(cfg *config.Config) wireshare.Store {
	if cfg.Server.IdentityBackendKind() != config.IdentitySSM {
		return nil
	}

	return newSSMAuthorityStore(cfg.Server.IdentitySSM())
}

// adoptSharedAuthority gives this host the authority the deployment already
// uses, before anything reads one.
//
// BEFORE serveNodeWire AND AFTER THE CLAIM, and both are load-bearing. The node
// wire's single authority read goes through LoadOrCreateCA, which CREATES one
// when the directory is empty — so a promoted standby that reached it first would
// mint a rival authority and drop the entire fleet, which is the failure this
// exists to stop. And a host that adopted before holding the claim would be
// writing identity material on the strength of nothing.
//
// IT TAKES THE AUTHORITY LOCK, because it writes the five files a rotation
// mutates in sequence.
func adoptSharedAuthority(
	ctx context.Context, cfg *config.Config, deployment string, log *slog.Logger,
) error {
	store := authorityStoreFor(cfg)
	if store == nil {
		return nil
	}

	return withAuthorityLock(cfg, log, func(dir string) error {
		adopted, err := wireshare.Adopt(ctx, store, dir, deployment, false)
		if err != nil {
			return err
		}

		if adopted == wireshare.AdoptedInstalled {
			log.Info("adopted this deployment's node-wire authority from the identity store")
		}

		return nil
	})
}

// publishSharedAuthority puts what this host holds into the store.
//
// AFTER serveNodeWire, because that is what creates an authority on a first
// controller and there is nothing to publish before it. On every later start the
// bytes are identical and the write is a no-op an operator never sees.
//
// A FAILURE HERE IS REPORTED AND NOT FATAL. The control plane is already serving
// by this point, and a store that cannot be written is a reason to look at IAM
// rather than to take a working deployment offline — what it costs is that the
// OTHER host has nothing to adopt yet, which `billet ca sync` and `billet check`
// both say.
func publishSharedAuthority(
	ctx context.Context, cfg *config.Config, deployment string, log *slog.Logger,
) {
	store := authorityStoreFor(cfg)
	if store == nil {
		return
	}

	err := withAuthorityLock(cfg, log, func(dir string) error {
		return wireshare.Publish(ctx, store, dir, deployment)
	})
	if err != nil {
		log.Error("could not publish this deployment's node-wire authority to the identity "+
			"store, so a second controller has nothing to adopt", "error", err)

		return
	}

	log.Info("published this deployment's node-wire authority to the identity store")
}

// publishRotatedAuthority carries a rotation or a retirement into the store.
//
// A ROTATION NOBODY ELSE HEARS ABOUT IS HALF A ROTATION. On a deployment with a
// second controller the store is how that host learns there is a new authority at
// all — and after a RETIREMENT it is how that host learns the previous pair is
// gone, which it would otherwise keep presenting a certificate from.
//
// REPORTED AND NOT FATAL, because by the time this runs the operation on THIS
// host is complete and irreversible. Returning an error would tell an operator
// their rotation failed when it did not; what they need is the one command that
// finishes the job.
func publishRotatedAuthority(ctx context.Context, cfg *config.Config, deployment string) {
	store := authorityStoreFor(cfg)
	if store == nil {
		return
	}

	log := slog.Default()

	err := withAuthorityLock(cfg, log, func(dir string) error {
		return wireshare.Publish(ctx, store, dir, deployment)
	})
	if err == nil {
		fmt.Println("Published the new authority to this deployment's identity store.")

		return
	}

	fmt.Fprintf(os.Stderr,
		"\nThis host is done, but publishing to the identity store failed:\n  %v\n\n"+
			"The other controller cannot see this change until it is published. Fix the\n"+
			"problem above and run:\n  billet ca sync --push\n", err)
}

// withAuthorityLock runs fn while this host holds the authority lock.
//
// ONE PLACE THAT TAKES IT, so the two callers cannot disagree about whether they
// hold it — and neither of them may call the other, because a second flock on a
// separate descriptor in the SAME process is denied.
func withAuthorityLock(cfg *config.Config, log *slog.Logger, fn func(dir string) error) error {
	dir := cfg.Server.IdentityDir

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
