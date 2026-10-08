package app

import (
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/wirecert"
)

// ec2Authorize dry-runs the launch a job needs, to prove the role may RunInstances
// — the one thing the read-only describes cannot. A DryRun has no side effect (AWS
// validates and checks IAM, then refuses and starts nothing), which is why this is
// opt-in behind --authorize rather than run by default: it is the only probe here
// that asks a write-shaped question, and an operator should choose to. Teardown is
// NOT dry-run here: a DryRun TerminateInstances validates the instance id before
// the permission verdict (measured), so ec2:TerminateInstances cannot be proved
// without a real instance and stays advisory.
// authorizeOwner resolves the deployment identity the dry-run must tag as, the way
// the node runtime does (nodeDeploymentID): the certificate outranks the config,
// because the certificate is what the control plane actually checks. It PEEKS only
// — a diagnostic must never mint an identity — so an unenrolled, never-started
// deployment returns "" and the caller skips the probe rather than tagging a
// value a per-deployment policy would reject.
func AuthorizeOwner(cfg *config.Config, bundle *wirecert.Bundle) (string, error) {
	if bundle != nil {
		return bundle.Deployment()
	}

	for _, dir := range cfg.DeploymentStateDirs() {
		id, found, err := state.PeekDeploymentID(dir)
		if err != nil {
			return "", err
		}
		if found {
			return id, nil
		}
	}

	return "", nil
}
