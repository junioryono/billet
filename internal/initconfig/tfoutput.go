package initconfig

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// awsAccountPattern is the one shape an account id has; anything else is an
// output an apply did not write, and comparing it would compare garbage.
var awsAccountPattern = regexp.MustCompile(`^\d{12}$`)

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

	// THE TWO A ROOT FROM BEFORE THE FINGERPRINT DOES NOT DECLARE, refused with
	// the way to a root that does. The way is a merge, never "re-render and
	// apply": a root the operator owns gets the fresh render beside it at
	// main.tf.new, where an apply never reads it, and a newer billet's render
	// also moves the module's release pin, which is not only an output.
	for name, dst := range map[string]*string{
		HybridOutputGeneration: &f.Generation,
		HybridOutputAccountID:  &f.AccountID,
	} {
		if _, ok := doc[name]; !ok {
			return HybridFacts{}, fmt.Errorf("--terraform-output: no output named %q, so this root "+
				"was rendered before billet bound its outputs to the generation that wrote them. "+
				"Run the plan render again into a scratch --out with the same flags, copy its "+
				"%q and %q output blocks into your terraform/main.tf and nothing else, run "+
				"`terraform plan` and check it adds only those two outputs, apply it, save "+
				"`terraform output -json` again, and pass --generation with the value you copied. A root "+
				"that still begins with billet's marker is replaced whole by the next render; remove the "+
				"marker from one you want kept",
				name, HybridOutputGeneration, HybridOutputAccountID)
		}
		if *dst, err = read(name); err != nil {
			return HybridFacts{}, err
		}
	}

	if !awsAccountPattern.MatchString(f.AccountID) {
		return HybridFacts{}, fmt.Errorf("--terraform-output: output %q is %q, which is not a "+
			"twelve-digit AWS account id", HybridOutputAccountID, f.AccountID)
	}

	for name, dst := range map[string]*string{
		HybridOutputControlPlanePrivateIP: &f.ControlPlanePrivateIP,
		HybridOutputLedgerVolumeID:        &f.LedgerVolumeID,
		HybridOutputSubnetID:              &f.SubnetID,
		HybridOutputRunnerSecurityGroup:   &f.RunnerSecurityGroupID,
		HybridOutputAMIPayloadBucket:      &f.AMIPayloadBucket,
		HybridOutputName:                  &f.Name,
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
