package initconfig

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ParseTerraformOutput reads `terraform output -json` into the facts a hybrid
// render consumes.
//
// EVERY CONSUMED OUTPUT IS REQUIRED, BY NAME. A missing one is not a fact
// billet can leave blank: an empty subnet loads and then launches nothing, and
// an empty ledger volume id makes the role skip the fail-closed mount and start
// the controller on the root disk. The untrusted group is demanded only for an
// untrusted generation, because a trusted root declares no such output.
func ParseTerraformOutput(raw []byte, untrusted bool) (HybridFacts, error) {
	var doc map[string]struct {
		Value any `json:"value"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return HybridFacts{}, fmt.Errorf("--terraform-output: not the JSON `terraform output -json` "+
			"writes: %w", err)
	}

	read := func(name string) (string, error) {
		entry, ok := doc[name]
		if !ok {
			return "", fmt.Errorf("--terraform-output: no output named %q; the generated "+
				"terraform/main.tf declares it, so this file was not produced by "+
				"`terraform output -json` against that root after an apply", name)
		}

		s, ok := entry.Value.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return "", fmt.Errorf("--terraform-output: output %q is empty or not a string; the "+
				"apply has not produced it yet", name)
		}

		return strings.TrimSpace(s), nil
	}

	var (
		f   HybridFacts
		err error
	)

	for name, dst := range map[string]*string{
		HybridOutputControlPlanePrivateIP: &f.ControlPlanePrivateIP,
		HybridOutputLedgerVolumeID:        &f.LedgerVolumeID,
		HybridOutputSubnetID:              &f.SubnetID,
		HybridOutputRunnerSecurityGroup:   &f.RunnerSecurityGroupID,
		HybridOutputAMIPayloadBucket:      &f.AMIPayloadBucket,
	} {
		if *dst, err = read(name); err != nil {
			return HybridFacts{}, err
		}
	}

	if untrusted {
		if f.UntrustedRunnerSecurityGroupID, err = read(HybridOutputUntrustedRunnerSG); err != nil {
			return HybridFacts{}, err
		}
	}

	return f, nil
}
