package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/rollout"
	"github.com/junioryono/billet/internal/state"
)

// `billet rollout registration --node N --incarnation I` is the controller's
// half of an endpoint migration's proof: whether the ledger, bound to the
// node's deployment, holds the incarnation the migrated node presents as
// that node's current live registration. It opens the ledger exactly as
// `rollout status` does (the read-only inspection, re-executed as the
// ledger's owner when run as root, with the child's exit preserved) and
// polls ONE SNAPSHOT at a time, the binding judged at every poll against
// the host's identity, until the row confirms, the wait elapses, or the
// ledger refuses. The epoch is reported and never compared: a registration
// under the same incarnation bumps it, and a comparison would read a
// re-registration as news.

// registrationAnswer is the confirmation: exactly the members the receipt's
// reader decodes.
type registrationAnswer struct {
	Schema      int             `json:"schema"`
	Outcome     string          `json:"outcome"`
	Node        string          `json:"node"`
	Incarnation string          `json:"incarnation"`
	Epoch       int64           `json:"epoch"`
	Live        bool            `json:"live"`
	Deployment  registrationDep `json:"deployment"`
}

// registrationTimeout is the timeout, with the last row seen or null.
type registrationTimeout struct {
	Schema      int              `json:"schema"`
	Outcome     string           `json:"outcome"`
	Node        string           `json:"node"`
	Incarnation string           `json:"incarnation"`
	Deployment  registrationDep  `json:"deployment"`
	Last        *registrationRow `json:"last"`
}

type registrationDep struct {
	Bound bool   `json:"bound"`
	ID    string `json:"id"`
}

type registrationRow struct {
	Incarnation string `json:"incarnation"`
	Epoch       int64  `json:"epoch"`
	Live        bool   `json:"live"`
}

// registrationPoll takes one snapshot; a variable so a test can fail a poll.
var registrationPoll = func(ctx context.Context, store *rollout.Store) (rollout.StatusSnapshot, error) {
	return store.StatusSnapshot(ctx)
}

func cmdRolloutRegistration(ctx context.Context, args []string) error {
	fs := newFlagSet("billet rollout registration")
	cfgPath := addConfigFlag(fs)
	node := fs.String("node", "", "the node whose registration is asked about")
	incarnation := fs.String("incarnation", "", "the incarnation the migrated node presents (32 hex characters)")
	wait := fs.Duration("wait", 5*time.Minute, "how long to poll the ledger for that incarnation")
	environmentFile := fs.String("environment-file", "", "read the PostgreSQL connection string from this systemd "+
		"environment file, the one the unit names, instead of the process environment")
	asJSON := fs.Bool("json", false, "print the answer as JSON (the only form)")

	if err := parse(fs, args); err != nil {
		return err
	}

	if !*asJSON {
		return errors.New("registration answers as JSON; pass --json")
	}

	bad := func(why string) error {
		return answerEndpointRefusal(endpointRefuse(endpointReasonCombination, why,
			"rollout registration --node N --incarnation I [--wait D] [--environment-file PATH] --json", ""))
	}

	switch {
	case *node == "":
		return bad("--node names the node, and it is empty")
	case !hex32.MatchString(*incarnation):
		return bad("--incarnation is not 32 hex characters")
	case *wait <= 0:
		return bad("--wait must be positive")
	}

	if err := config.ValidateNodeName("node", *node); err != nil {
		return bad("--node: " + err.Error())
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}

	if cfg.Server == nil {
		return errors.New("a registration is a property of the control plane, and this config has no server section")
	}

	// THE RE-EXECUTION AS THE LEDGER'S OWNER, the child's exit preserved: an
	// answer of timeout or refused is the child's to give.
	done, code, err := runAsLedgerOwnerCode(ctx, cfg, append([]string{"rollout", "registration"}, args...))
	if err != nil {
		return err
	}

	if done {
		if code != 0 {
			return &exitError{code: code}
		}

		return nil
	}

	dsn, err := ledgerDSNFrom(cfg, *environmentFile)
	if err != nil {
		return err
	}

	db, err := openStateInspect(ctx, cfg, dsn)
	if err != nil {
		return fmt.Errorf("server state: %w", err)
	}

	defer func() { _ = db.Close() }()

	answer, timeout, r := awaitRegistration(ctx, rollout.New(db), cfg, *node, *incarnation, *wait)
	if r != nil {
		return answerEndpointRefusal(r)
	}

	if timeout != nil {
		return answerJSON(timeout, exitUnknown, "the ledger did not show "+*node+" registered as incarnation "+
			*incarnation+" within "+wait.String())
	}

	return answerObject(answer)
}

// awaitRegistration polls until the node's row carries the incarnation and
// is live, the ledger refuses, or the wait elapses.
func awaitRegistration(ctx context.Context, store *rollout.Store, cfg *config.Config, node, incarnation string,
	wait time.Duration,
) (*registrationAnswer, *registrationTimeout, *endpointRefusal) {
	deadline := time.Now().Add(wait)

	var last *registrationRow

	for {
		// PEEKED, NEVER MINTED, AT EVERY POLL: the identity file is what the
		// snapshot's binding is compared with, and a host whose identity
		// moved under the polls is refused at the poll that sees it. An
		// identity that is ABSENT proves nothing, so it confirms nothing.
		identity, found, err := state.PeekDeploymentID(cfg.Server.IdentityDir)
		if err != nil {
			return nil, nil, endpointUnknown(endpointReasonUnexamined, "read the deployment identity: "+err.Error(), "", "")
		}

		if !found {
			return nil, nil, endpointRefuse(endpointReasonTrust, "this host has no deployment identity in "+
				cfg.Server.IdentityDir+", so no ledger binding can be proved to be this host's", "", "")
		}

		snapshot, err := registrationPoll(ctx, store)
		if err != nil {
			return nil, nil, endpointUnknown(endpointReasonUnexamined, "read the ledger: "+err.Error(), "", "")
		}

		if snapshot.Binding == "" {
			return nil, nil, endpointRefuse(endpointReasonUnbound, "this ledger is bound to no deployment, so no registration "+
				"in it is this deployment's", "", "")
		}

		if snapshot.Binding != identity {
			return nil, nil, endpointRefuse(endpointReasonTrust, fmt.Sprintf("%v: this ledger is bound to deployment %s and "+
				"this host's identity directory says %s", state.ErrForeignLedger, snapshot.Binding, identity), "", "")
		}

		dep := registrationDep{Bound: true, ID: snapshot.Binding}
		last = nil

		for i := range snapshot.Registrations {
			row := &snapshot.Registrations[i]
			if row.Name != node {
				continue
			}

			last = &registrationRow{Incarnation: row.Incarnation, Epoch: row.Epoch, Live: row.Live}

			if row.Incarnation == incarnation && row.Live {
				return &registrationAnswer{Schema: endpointSchema, Outcome: outcomeConfirmed, Node: node,
					Incarnation: incarnation, Epoch: row.Epoch, Live: true, Deployment: dep}, nil, nil
			}
		}

		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, &registrationTimeout{Schema: endpointSchema, Outcome: outcomeTimeout, Node: node,
				Incarnation: incarnation, Deployment: dep, Last: last}, nil
		}

		select {
		case <-ctx.Done():
		case <-time.After(2 * endpointPoll):
		}
	}
}

// runAsLedgerOwnerCode is runAsLedgerOwner with the child's exit status
// preserved for the caller to exit with, for a command whose non-zero exits
// are answers (2 a refusal, 3 could-not-tell) and not failures.
func runAsLedgerOwnerCode(ctx context.Context, cfg *config.Config, args []string) (bool, int, error) {
	done, err := runAsLedgerOwner(ctx, cfg, args)

	var coded *ledgerChildExit
	if errors.As(err, &coded) {
		return true, coded.code, nil
	}

	return done, 0, err
}
