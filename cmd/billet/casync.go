package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
	"github.com/junioryono/billet/internal/wireshare"
)

// cmdCASync carries this deployment's node-wire authority between its
// controllers.
//
// WHAT IT IS FOR IS A ROTATION. A controller adopts an authority automatically
// when it has none, which covers a host being added; what it cannot do on its own
// is decide that a host holding a DIFFERENT authority is the one that is behind.
// billet cannot tell a host left behind by `billet ca rotate` from a host pointed
// at the wrong deployment, and the file it would be writing over is the key every
// node in the fleet verifies against — so that judgement is an operator's, and
// this is where they make it.
//
// TWO DIRECTIONS, AND THE DEFAULT IS THE SAFE ONE. Without --push this host takes
// what the store has; with it, the store takes what this host has. Neither
// direction deletes anything: a replaced local authority is moved aside, because
// what is being set aside is a private key and an operator who chose the wrong
// direction has to be able to put it back.
func cmdCASync(ctx context.Context, args []string) error {
	fs := newFlagSet("ca sync")
	cfgPath := fs.String("config", "", "path to billet.yaml")
	push := fs.Bool("push", false,
		"publish this host's authority to the identity store instead of adopting from it")
	force := fs.Bool("force", false,
		"replace this host's authority with the published one (it is moved aside, not deleted)")

	if err := parse(fs, args); err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	store := authorityStoreFor(cfg)
	if store == nil {
		return fmt.Errorf(
			"this deployment keeps its node-wire authority as files in %s (server.identity."+
				"backend is %s), so there is nothing to synchronise it with. Copy the "+
				"directory, or set an identity store",
			cfg.Server.IdentityDir, cfg.Server.IdentityBackendKind())
	}

	// THE HOST'S OWN IDENTITY, PEEKED RATHER THAN MINTED. This command runs on a
	// machine that may be being prepared, and state.DeploymentID CREATES one where
	// there is none — a `ca sync` that minted an identity as a side effect would
	// be deciding what deployment this host is.
	deployment, ok, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
	if err != nil {
		return err
	}

	if !ok {
		return fmt.Errorf(
			"%s holds no deployment identity, so billet does not know which deployment's "+
				"authority to synchronise. Run `billet check` on this host first",
			cfg.Server.IdentityDir)
	}

	log := slog.Default()

	if *push {
		if *force {
			return fmt.Errorf(
				"--force applies to adopting an authority, not to publishing one; a push " +
					"replaces the store's copy either way")
		}

		if err := withAuthorityLock(cfg, log, func(dir string) error {
			return wireshare.Publish(ctx, store, dir, deployment)
		}); err != nil {
			return err
		}

		fmt.Println("Published this host's node-wire authority to the identity store.")
		fmt.Println("A controller that holds a DIFFERENT one still refuses to adopt it; " +
			"run `billet ca sync --force` there once you are sure this is the right one.")

		return nil
	}

	var adopted wireshare.Adopted

	if err := withAuthorityLock(cfg, log, func(dir string) error {
		var err error
		adopted, err = wireshare.Adopt(ctx, store, dir, deployment, *force)

		return err
	}); err != nil {
		return err
	}

	switch adopted {
	case wireshare.AdoptedNothing:
		fmt.Println("The identity store holds no authority for this deployment yet.")
		fmt.Println("Run `billet ca sync --push` on the controller that has one.")
	case wireshare.AdoptedAlreadyHeld:
		fmt.Println("This host already holds the authority the identity store publishes.")
	case wireshare.AdoptedInstalled:
		fmt.Printf("This host now holds this deployment's node-wire authority (%s).\n",
			currentAuthorityFingerprint(cfg.Server.IdentityDir))
		fmt.Println("Restart the control plane here so it serves the authority it now holds.")
	}

	return nil
}

// currentAuthorityFingerprint is what this host holds, for a report.
//
// BEST EFFORT, because it runs after a successful install and a failure to read
// it back must not turn a completed operation into an error the operator reads as
// a failure.
func currentAuthorityFingerprint(stateDir string) string {
	authority, err := wirecert.ReadAuthority(stateDir)
	if err != nil {
		return "unreadable"
	}

	cert, err := wirecert.ParseAuthorityPair(
		authority.Present["ca.key"], authority.Present["ca.crt"])
	if err != nil {
		return "unreadable"
	}

	return wirecert.FingerprintOfCert(cert)
}
