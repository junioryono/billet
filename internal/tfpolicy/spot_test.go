package tfpolicy

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider/ec2"
)

// The terraform module grants the spot interruption actions from the committed
// policy/spot-actions.json (read with jsondecode(file()) in HCL), so this pins
// those bytes to billet's own source of truth the same way the node-policy
// renderings are pinned: if ec2.SpotIAMActions ever changes, the committed file
// must change with it. A byte comparison, not a set comparison — the file is
// what the module ships, so its exact content is the contract.
//
// The module's plan tests read the same committed file (tests/fleet.tftest.hcl
// derives its expected actions from it), so a regeneration here flows through
// both pins without a literal to update by hand.
func TestTerraformSpotGrantMatchesGenerator(t *testing.T) {
	const path = "../../terraform/modules/billet/modules/fleet-ec2/policy/spot-actions.json"

	got, err := json.MarshalIndent(ec2.SpotIAMActions(), "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	if os.Getenv("UPDATE_TF_POLICY") == "1" {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (regenerate with UPDATE_TF_POLICY=1)", path, err)
	}

	if !bytes.Equal(got, want) {
		t.Errorf("terraform-aws-billet's spot-actions.json no longer matches "+
			"ec2.SpotIAMActions(). If intended, regenerate with UPDATE_TF_POLICY=1 "+
			"and review the diff.\nGenerator:\n%s\nFile:\n%s", got, want)
	}
}

// A pinned file the module does not read pins nothing: the grant could regress
// to a hard-coded action list (or disappear) and the byte comparison above
// would stay green. A substring, not an HCL parse, so a formatting change
// cannot silently break the check — the consuming expression either appears in
// iam.tf or it does not.
func TestTerraformSpotGrantConsumesTheCommittedFile(t *testing.T) {
	const iamPath = "../../terraform/modules/billet/modules/fleet-ec2/iam.tf"

	raw, err := os.ReadFile(iamPath)
	if err != nil {
		t.Fatalf("read %s: %v", iamPath, err)
	}

	const consume = `jsondecode(file("${path.module}/policy/spot-actions.json"))`
	if !strings.Contains(string(raw), consume) {
		t.Errorf("%s no longer reads policy/spot-actions.json (expected the expression %s): "+
			"the committed actions file pins nothing unless the module consumes it", iamPath, consume)
	}
}
