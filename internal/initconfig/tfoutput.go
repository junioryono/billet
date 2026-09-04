package initconfig

import (
	"encoding/json"
	"fmt"
	"strings"
)

// HybridNeeds says which optional facts a generation depends on, so
// ParseTerraformOutput demands exactly what this deployment's shape consumes
// and nothing it does not.
//
// A STRUCT RATHER THAN BOOLEANS IN A ROW, because two bare bools at a call site
// are two chances to swap them, and swapping these means demanding an output
// the root does not declare while accepting one it does.
type HybridNeeds struct {
	// Untrusted demands the untrusted runner group, which only an untrusted
	// generation's root declares.
	Untrusted bool
	// Cache demands the three cache facts, which only a --cache root declares.
	Cache bool
}

// ParseTerraformOutput reads `terraform output -json` into the facts a hybrid
// render consumes.
//
// EVERY CONSUMED OUTPUT IS REQUIRED, BY NAME. A missing one is not a fact
// billet can leave blank: an empty subnet loads and then launches nothing, and
// an empty ledger volume id makes the role skip the fail-closed mount and start
// the controller on the root disk. The optional ones are demanded only where
// the generation actually reads them, because a root that never declared an
// output cannot be faulted for not producing it.
func ParseTerraformOutput(raw []byte, need HybridNeeds) (HybridFacts, error) {
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
		HybridOutputRegion:                &f.Region,
	} {
		if *dst, err = read(name); err != nil {
			return HybridFacts{}, err
		}
	}

	if need.Untrusted {
		if f.UntrustedRunnerSecurityGroupID, err = read(HybridOutputUntrustedRunnerSG); err != nil {
			return HybridFacts{}, err
		}
	}

	if need.Cache {
		for name, dst := range map[string]*string{
			HybridOutputCacheBucket:      &f.CacheBucket,
			HybridOutputCachePrefix:      &f.CachePrefix,
			HybridOutputAvailabilityZone: &f.AvailabilityZone,
		} {
			if *dst, err = read(name); err != nil {
				return HybridFacts{}, err
			}
		}
	}

	return f, nil
}
