package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/awsssm"
	"github.com/junioryono/billet/internal/config"
)

// publishAppKey puts a GitHub App private key into this deployment's identity
// store, and refuses to replace one.
//
// NO-OVERWRITE IS THE WHOLE CONTRACT, and it is the same one reserveKeyFile
// enforces on a filesystem. GitHub issues an App private key exactly once and
// there is no re-issue, so a publication that could replace something is a
// publication that can destroy the only copy of a credential. Parameter Store has
// conditional create natively, which is why this is three lines rather than the
// four review rounds the file path took.
func publishAppKey(ctx context.Context, cfg *config.Config, pem []byte) error {
	ssm := cfg.Server.IdentitySSM()
	if ssm == nil {
		return errors.New(
			"this deployment does not keep its App key in an identity store")
	}

	name := appKeyPath(ssm)

	_, err := awsssm.New(ssm.Region, awscreds.Default()).Put(ctx, name, string(pem), awsssm.PutOptions{
		Overwrite: false,
		KMSKeyID:  ssm.KMSKeyID,
		Description: "billet GitHub App private key; GitHub issues this once and cannot " +
			"re-issue it, so do not delete it while the deployment exists",
	})

	switch {
	case errors.Is(err, awsssm.ErrAlreadyExists):
		return fmt.Errorf(
			"%s already holds a value, and billet will not replace it: a GitHub App private "+
				"key is issued once and cannot be re-issued, so overwriting one destroys the "+
				"only copy. If that value is a key this deployment no longer uses, remove it "+
				"deliberately and run this again", name)
	case err != nil:
		return fmt.Errorf("publish the App key to %s: %w", name, err)
	}

	return nil
}

// githubAppStoreKey publishes an App key that is already on disk.
//
// IT EXISTS FOR ONE RECOVERY, and that recovery is the reason the onboarding
// writes a local copy first. `billet github-app create` on a store-backed
// deployment saves the key to a file and THEN publishes it; if the publish fails
// — expired credentials, an IAM policy without ssm:PutParameter, a KMS key the
// role cannot use — the App is registered and its unrepeatable key is on disk
// with nowhere to go. Without this, the only way forward would be creating a
// SECOND App.
//
// It reads through the same validating reader the file backend uses, so a value
// that is not a key is refused here rather than at the first token mint.
func githubAppStoreKey(ctx context.Context, args []string) error {
	fs := newFlagSet("github-app store-key")
	from := fs.String("from", "", "the file holding the App private key")
	configPath := fs.String("config", "", "path to billet.yaml")

	if err := parse(fs, args); err != nil {
		return err
	}

	if *from == "" {
		return errors.New("billet github-app store-key --from <path to the App private key>")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	if cfg.Server.IdentityBackendKind() != config.IdentitySSM {
		return fmt.Errorf(
			"this deployment keeps its App key as a file (server.identity.backend is %s), so "+
				"there is nothing to publish it to. github.private_key_path is where it goes",
			cfg.Server.IdentityBackendKind())
	}

	pem, err := readPrivateKey(*from)
	if err != nil {
		return err
	}

	if err := publishAppKey(ctx, cfg, pem); err != nil {
		return err
	}

	fmt.Printf("Published the App private key to %s\n", appKeyLocation(cfg))
	fmt.Printf("\nThe copy at %s is now a second copy of an unrepeatable credential.\n"+
		"Remove it once `billet check` reports this deployment healthy.\n", *from)

	return nil
}

// storeAppKeyDuringOnboarding publishes a freshly issued key, having already
// written it to disk.
//
// THE LOCAL FILE IS THE STAGING AREA, which is the reserveKeyFile model extended
// rather than replaced. A publication straight from memory would have exactly one
// failure mode with no way back: the App registered, the key gone. Writing it
// down first means every failure below leaves a file an operator can publish with
// `billet github-app store-key`, which is what the message says.
func storeAppKeyDuringOnboarding(ctx context.Context, cfgPath, keyPath string, pem []byte) {
	cfg, err := config.Load(cfgPath)
	if err == nil {
		err = publishAppKey(ctx, cfg, pem)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr,
			"\nThe App was created and its key saved to %s, but publishing it to this "+
				"deployment's identity store failed:\n  %v\n\n"+
				"The key is NOT lost. Fix the problem above and run:\n"+
				"  billet github-app store-key --from %s\n",
			keyPath, err, keyPath)

		return
	}

	fmt.Printf("Published the private key to %s\n", appKeyLocation(cfg))
	fmt.Printf("The copy at %s is a second copy of an unrepeatable credential; "+
		"remove it once `billet check` reports this deployment healthy.\n", keyPath)
}
