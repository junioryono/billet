package imagesource

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"testing"
)

func defaultPolicy(t *testing.T) Policy {
	t.Helper()

	src := DefaultSource()

	p, err := PolicyFor(src, "", "", false)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	return p
}

// A POLICY THAT DOES NOT REQUIRE A SIGNATURE MUST NOT VERIFY ONE, and must not
// error either. This is the air-gapped path, and turning it into a failure would
// make the explicit waiver useless.
func TestVerifySignatureSkipsWhenThePolicyDoesNotRequireIt(t *testing.T) {
	if err := VerifySignature([]byte("anything"), nil, Policy{Required: false}); err != nil {
		t.Fatalf("a policy that does not require verification still failed: %v", err)
	}
}

// A REQUIRED POLICY WITH NO BUNDLE IS A FAILURE, not a skip. This is the case
// where an image is published without a signature, or the bundle asset is missing
// from the release -- and treating an absent signature as "nothing to check" is
// the fail-open this whole path exists to prevent.
func TestVerifySignatureRefusesAMissingBundleWhenRequired(t *testing.T) {
	err := VerifySignature([]byte("a manifest"), nil, defaultPolicy(t))
	if err == nil {
		t.Fatal("a required signature was satisfied by no bundle at all")
	}

	// ASSERTED ON THE ABSENCE, NOT MERELY ON AN ERROR.
	//
	// The first version of this checked only that the message mentioned
	// "signature", which the PARSE failure also does -- so removing the
	// absent-bundle check entirely left the test passing, because a nil bundle
	// falls through to the parser and errors there for an unrelated reason. A
	// missing signature and a corrupt one are different situations and an operator
	// needs to be told which.
	if !strings.Contains(err.Error(), "none was published") {
		t.Errorf("the refusal reads as a malformed signature rather than an absent one, "+
			"so an operator is sent after the wrong thing: %v", err)
	}
}

// THE LEGACY COSIGN BUNDLE IS REFUSED, WITH A MESSAGE THAT SAYS WHY.
//
// This is a real bundle from this project's first published release: cosign's old
// shape, base64Signature / cert / rekorBundle. The library that verifies this
// parses only the protobuf Sigstore bundle, so the signature is genuine and
// unreadable at the same time -- and an operator hitting this needs to be told the
// image must be republished, not left reading a parse error about a field name.
func TestVerifySignatureRefusesTheLegacyBundleFormatWithAUsefulMessage(t *testing.T) {
	legacy, err := os.ReadFile("testdata/legacy-bundle.json")
	if err != nil {
		t.Skipf("no legacy fixture: %v", err)
	}

	err = VerifySignature([]byte("a manifest"), legacy, defaultPolicy(t))
	if err == nil {
		t.Fatal("the legacy cosign bundle format was accepted")
	}

	lower := strings.ToLower(err.Error())

	if !strings.Contains(lower, "republish") && !strings.Contains(lower, "format") {
		t.Errorf("the refusal does not tell the operator what to do about it: %v", err)
	}
}

func TestVerifySignatureRefusesAMalformedBundle(t *testing.T) {
	for _, bad := range [][]byte{
		[]byte("{"),
		[]byte("not json at all"),
		[]byte("{}"),
		[]byte("[]"),
	} {
		if err := VerifySignature([]byte("a manifest"), bad, defaultPolicy(t)); err == nil {
			t.Errorf("%q was accepted as a signature bundle", bad)
		}
	}
}

// THE TRUST ROOT IS EMBEDDED AND USABLE. Without it a node that may be air gapped
// would have to reach sigstore's TUF repository to verify anything, which is
// exactly what it cannot do.
func TestTheEmbeddedTrustRootLoads(t *testing.T) {
	tr, err := TrustedRoot()
	if err != nil {
		t.Fatalf("the embedded trust root does not load, so nothing can be verified "+
			"without network access: %v", err)
	}

	if tr == nil {
		t.Fatal("the embedded trust root is nil")
	}
}

// A REAL SIGNATURE, OVER A REAL MANIFEST, FROM THE REAL WORKFLOW.
//
// Every other test here asserts that verification REFUSES something, and a
// verifier that refuses everything passes all of them. This is the one that proves
// the policy accepts what it is supposed to: the manifest and bundle from a
// genuine publish, checked against the built-in identity with nothing relaxed.
//
// The fixture does not expire. A Fulcio certificate lives about ten minutes, but
// verification judges it as-of the timestamp in the transparency log entry, which
// is what makes an old signature over unchanged bytes verifiable indefinitely.
func TestARealSignatureFromThisProjectVerifies(t *testing.T) {
	manifest, err := os.ReadFile("testdata/signed-manifest.json")
	if err != nil {
		t.Skipf("no signed fixture: %v", err)
	}

	bundle, err := os.ReadFile("testdata/signed-manifest.sigstore.json")
	if err != nil {
		t.Skipf("no bundle fixture: %v", err)
	}

	if err := VerifySignature(manifest, bundle, defaultPolicy(t)); err != nil {
		t.Fatalf("a genuine signature from this project's publishing workflow was "+
			"refused by the built-in policy, so no node could pull anything: %v", err)
	}
}

// AND THE SAME SIGNATURE OVER CHANGED BYTES IS REFUSED. Without this the test
// above proves only that the code runs.
func TestARealSignatureDoesNotCoverAChangedManifest(t *testing.T) {
	manifest, err := os.ReadFile("testdata/signed-manifest.json")
	if err != nil {
		t.Skipf("no signed fixture: %v", err)
	}

	bundle, err := os.ReadFile("testdata/signed-manifest.sigstore.json")
	if err != nil {
		t.Skipf("no bundle fixture: %v", err)
	}

	// One field, of the kind an attacker would change: the architecture a node
	// checks before deciding the image is for it.
	tampered := []byte(strings.Replace(string(manifest), `"x86_64"`, `"aarch64"`, 1))

	if bytes.Equal(tampered, manifest) {
		t.Fatal("could not tamper with the fixture")
	}

	if err := VerifySignature(tampered, bundle, defaultPolicy(t)); err == nil {
		t.Fatal("a genuine signature covered a manifest that had been changed under it")
	}
}

// A SIGNATURE FROM ANOTHER WORKFLOW IN THIS SAME REPOSITORY IS REFUSED, which is
// the attack the identity pins the workflow and ref against: opening a pull
// request is a far lower bar than compromising the release process.
func TestARealSignatureDoesNotSatisfyAnotherWorkflowsPolicy(t *testing.T) {
	manifest, err := os.ReadFile("testdata/signed-manifest.json")
	if err != nil {
		t.Skipf("no signed fixture: %v", err)
	}

	bundle, err := os.ReadFile("testdata/signed-manifest.sigstore.json")
	if err != nil {
		t.Skipf("no bundle fixture: %v", err)
	}

	src := DefaultSource()

	other, err := PolicyFor(src,
		`^https://github\.com/`+regexp.QuoteMeta(DefaultRepo)+`/\.github/workflows/ci\.yml@refs/heads/main$`,
		GitHubOIDCIssuer, false)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	if err := VerifySignature(manifest, bundle, other); err == nil {
		t.Fatal("a signature from the guest-image workflow satisfied a policy naming a " +
			"different workflow in the same repository")
	}
}
