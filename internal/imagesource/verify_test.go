package imagesource

import (
	"os"
	"strings"
	"testing"
)

func defaultPolicy(t *testing.T) Policy {
	t.Helper()

	src, err := ParseSource(DefaultBaseURL)
	if err != nil {
		t.Fatalf("source: %v", err)
	}

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
