package initconfig

import (
	"strings"
	"testing"
)

// A MINTED FINGERPRINT HAS THE ONE SHAPE THE CHECK ACCEPTS, and two mints
// differ: the runbook prints it, and a repeated value would bind two roots to
// each other's outputs.
func TestNewHybridGenerationIsAFingerprint(t *testing.T) {
	t.Parallel()

	a, err := NewHybridGeneration()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	b, err := NewHybridGeneration()
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	for _, g := range []string{a, b} {
		if err := CheckHybridGeneration(g); err != nil {
			t.Errorf("a minted fingerprint %q fails its own check: %v", g, err)
		}
	}
	if a == b {
		t.Errorf("two mints returned the same fingerprint %q", a)
	}
}

// THE CHECK REFUSES ANYTHING BUT THE PRINTED SHAPE, padding included, because
// the value is compared byte for byte with what the root declares.
func TestCheckHybridGenerationRefusesOtherShapes(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{
		"", "0123456789abcdef", "0123-4567-89ab-cdeF", " 0123-4567-89ab-cdef",
		"0123-4567-89ab-cdef ", "0123-4567-89ab", "0123-4567-89ab-cdef-0000", "g123-4567-89ab-cdef",
	} {
		if err := CheckHybridGeneration(bad); err == nil {
			t.Errorf("%q was accepted as a fingerprint", bad)
		}
	}

	if err := CheckHybridGeneration(testGeneration); err != nil {
		t.Errorf("the printed shape was refused: %v", err)
	}
}

// THE ROOT DECLARES THE FINGERPRINT IT WAS RENDERED WITH AND THE ACCOUNT FROM
// THE APPLY'S OWN CALLER IDENTITY, so the outputs carry both facts a later
// render compares.
func TestGenerateHybridRootDeclaresItsGenerationAndAccount(t *testing.T) {
	t.Parallel()

	files, _ := mustGenerateHybrid(t, hybridParams())
	root := files[HybridTerraformFile]

	for _, want := range []string{
		"output \"" + HybridOutputGeneration + "\" {",
		"value       = \"" + testGeneration + "\"",
		"output \"" + HybridOutputAccountID + "\" {",
		"value       = data.aws_caller_identity.this.account_id",
	} {
		if !strings.Contains(root, want) {
			t.Errorf("terraform/main.tf does not carry %q", want)
		}
	}
}

// A GENERATION IS REQUIRED, and only in the printed shape: a render without
// one would write a root whose outputs bind to nothing.
func TestGenerateHybridRefusesAMissingGeneration(t *testing.T) {
	t.Parallel()

	for _, g := range []string{"", "0123"} {
		p := hybridParams()
		p.Generation = g

		if _, _, err := GenerateHybrid(p); err == nil || !strings.Contains(err.Error(), "--generation") {
			t.Errorf("generation %q was not refused by its flag: %v", g, err)
		}
	}
}

// OUTPUTS FROM A ROOT THAT PREDATES THE FINGERPRINT ARE REFUSED WITH THE WAY
// TO A ROOT THAT HAS ONE, and an account id that is not one is refused rather
// than compared.
func TestParseTerraformOutputDemandsTheBinding(t *testing.T) {
	t.Parallel()

	full := `{
  "control_plane_private_ip": {"value": "10.60.0.10"},
  "ledger_volume_id": {"value": "vol-0abc"},
  "subnet_id": {"value": "subnet-0abc"},
  "runner_security_group_id": {"value": "sg-trusted"},
  "ami_payload_bucket": {"value": "acme-ci-ami-payloads-1"},
  "name": {"value": "acme-ci"},
  "region": {"value": "us-west-2"},
  "generation": {"value": "0123-4567-89ab-cdef"},
  "account_id": {"value": "123456789012"}
}`
	if _, err := ParseTerraformOutput([]byte(full), HybridNeeds{}); err != nil {
		t.Fatalf("a complete output: %v", err)
	}

	for _, name := range []string{HybridOutputGeneration, HybridOutputAccountID} {
		old := strings.Replace(full, `"`+name+`"`, `"renamed"`, 1)
		_, err := ParseTerraformOutput([]byte(old), HybridNeeds{})
		if err == nil {
			t.Errorf("outputs with no %q were accepted", name)

			continue
		}
		for _, want := range []string{name, "scratch --out", "terraform plan", "only those two outputs", "--generation"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal of a missing %q does not say %q: %v", name, want, err)
			}
		}
	}

	notAnAccount := strings.Replace(full, `"123456789012"`, `"12345"`, 1)
	if _, err := ParseTerraformOutput([]byte(notAnAccount), HybridNeeds{}); err == nil ||
		!strings.Contains(err.Error(), HybridOutputAccountID) {
		t.Errorf("an account id that is not twelve digits was not refused by name: %v", err)
	}
}
