package config

import (
	"strings"
	"testing"
)

// keylessServerWith is a config whose github block names no private key path,
// which is what a store-backed deployment writes.
func keylessServerWith(server string) string {
	return "github:\n  org: acme\n  app_id: 1\n  installation_id: 2\nserver:\n" + server +
		"  max_vcpu: 4\n  max_memory: 8GiB\n"
}

// SILENCE MEANS FILES, which is where every deployment before this key kept its
// identity material.
func TestAnAbsentIdentityBlockIsTheFileBackend(t *testing.T) {
	cfg, err := loadServer(t, serverWith("  state_dir: "+t.TempDir()+"\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Server.IdentityBackendKind(); got != IdentityFile {
		t.Errorf("identity backend = %q, want %q", got, IdentityFile)
	}
}

// THE STORE NEEDS A REGION AND A PATH, AND THERE IS NOTHING TO GUESS.
//
// The region is the signing region AND selects the endpoint; the prefix is what
// IAM is scoped by, and what keeps two deployments from reading each other's
// authority.
func TestTheIdentityStoreRequiresARegionAndAPrefix(t *testing.T) {
	_, err := loadServer(t, keylessServerWith(
		"  identity_dir: "+t.TempDir()+"\n  identity:\n    backend: aws-ssm\n    aws_ssm: {}\n"))
	if err == nil {
		t.Fatal("an identity store with no region and no prefix was accepted")
	}

	for _, want := range []string{"region", "prefix"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q; got: %v", want, err)
		}
	}
}

// A PARAMETER STORE PATH IS ABSOLUTE.
//
// A relative name is a legal parameter and a DIFFERENT one, so accepting it
// would silently give a deployment a store nobody's IAM policy covers.
func TestARelativeIdentityPrefixIsRefused(t *testing.T) {
	_, err := loadServer(t, keylessServerWith(
		"  identity_dir: "+t.TempDir()+"\n  identity:\n    backend: aws-ssm\n"+
			"    aws_ssm:\n      region: us-west-2\n      prefix: billet/prod\n"))
	if err == nil {
		t.Fatal("a relative Parameter Store prefix was accepted")
	}

	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("the refusal should say the path is absolute; got: %v", err)
	}
}

// AND THE STORE AND A KEY PATH ARE TWO SPELLINGS OF ONE VALUE.
//
// internal/config has made this mistake three times and it is silent every time.
// With the key in Parameter Store there is no path to read, and a config carrying
// both leaves an operator unable to tell which one the deployment uses — or
// updating the one nothing reads.
func TestAKeyPathBesideTheIdentityStoreIsRefused(t *testing.T) {
	_, err := loadServer(t, serverWith(
		"  identity_dir: "+t.TempDir()+"\n  identity:\n    backend: aws-ssm\n"+
			"    aws_ssm:\n      region: us-west-2\n      prefix: /billet/prod\n"))
	if err == nil {
		t.Fatal("a private_key_path was accepted beside the identity store")
	}

	if !strings.Contains(err.Error(), "private_key_path") {
		t.Errorf("the refusal should name the key that has to go; got: %v", err)
	}
}

// AND A STORE-BACKED CONFIG LOADS WITHOUT ONE.
//
// The other direction, and the one that would break every store-backed
// deployment if it were missing: github.private_key_path is required only while
// the key is a file.
func TestAStoreBackedConfigNeedsNoKeyPath(t *testing.T) {
	cfg, err := loadServer(t, keylessServerWith(
		"  identity_dir: "+t.TempDir()+"\n  identity:\n    backend: aws-ssm\n"+
			"    aws_ssm:\n      region: us-west-2\n      prefix: /billet/prod\n"))
	if err != nil {
		t.Fatalf("load: %v", err)
	}

	if got := cfg.Server.IdentityBackendKind(); got != IdentitySSM {
		t.Errorf("identity backend = %q, want %q", got, IdentitySSM)
	}

	if ssm := cfg.Server.IdentitySSM(); ssm == nil || ssm.Prefix != "/billet/prod" {
		t.Errorf("the Parameter Store block did not survive the load: %+v", ssm)
	}
}

// A BLOCK THE SELECTED BACKEND DOES NOT READ IS REFUSED RATHER THAN IGNORED.
//
// The rule `state:` already follows: silently ignoring a block produces a
// deployment that believes it configured something.
func TestASSMBlockUnderTheFileBackendIsRefused(t *testing.T) {
	_, err := loadServer(t, serverWith(
		"  identity_dir: "+t.TempDir()+"\n  identity:\n    backend: file\n"+
			"    aws_ssm:\n      region: us-west-2\n      prefix: /billet/prod\n"))
	if err == nil {
		t.Fatal("an aws_ssm block was accepted under the file backend")
	}
}

// AND AN UNKNOWN BACKEND IS REFUSED BY NAME.
func TestAnUnknownIdentityBackendIsRefused(t *testing.T) {
	_, err := loadServer(t, serverWith(
		"  identity_dir: "+t.TempDir()+"\n  identity:\n    backend: vault\n"))
	if err == nil {
		t.Fatal("an unknown identity backend was accepted")
	}

	for _, want := range []string{"vault", "file", "aws-ssm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q; got: %v", want, err)
		}
	}
}

// THE PEEK ANSWERS FROM A CONFIG THAT DOES NOT LOAD.
//
// `billet github-app create` runs against a config with no app_id and no
// installation_id — registering the App is what produces them — so Load refuses
// it, and the command still has to know before the browser flow whether the key
// it is about to receive belongs in a file or in a store.
func TestTheIdentityPeekAnswersFromAnIncompleteConfig(t *testing.T) {
	body := []byte("github:\n  org: acme\nserver:\n  identity:\n    backend: aws-ssm\n")

	if got := PeekIdentityBackend(body); got != IdentitySSM {
		t.Errorf("PeekIdentityBackend = %q, want %q", got, IdentitySSM)
	}

	// AND EVERY FAILURE ANSWERS `file`, which is the compatible direction: a
	// config this cannot read is one whose key goes where every config's key has
	// always gone, and the Load that follows produces the real diagnostic.
	for name, broken := range map[string][]byte{
		"not yaml":       []byte("\tthis: is: not: yaml\n"),
		"no server":      []byte("github:\n  org: acme\n"),
		"no identity":    []byte("server:\n  state_dir: /tmp/x\n"),
		"empty backend":  []byte("server:\n  identity:\n    backend: \"\"\n"),
		"nothing at all": nil,
	} {
		if got := PeekIdentityBackend(broken); got != IdentityFile {
			t.Errorf("PeekIdentityBackend(%s) = %q, want %q", name, got, IdentityFile)
		}
	}
}
