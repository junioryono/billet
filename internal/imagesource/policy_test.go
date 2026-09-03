package imagesource

import (
	"regexp"
	"strings"
	"testing"
)

// THE DEFAULT SOURCE DEMANDS A SIGNATURE, and the identity is not something the
// operator has to know: it is billet's own workflow, derived from the one constant
// naming this project.
func TestPolicyForTheDefaultSourceRequiresBilletsOwnIdentity(t *testing.T) {
	src := DefaultSource()

	p, err := PolicyFor(src, "", "", false)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	if !p.Required {
		t.Fatal("the default source does not require a signature; a manifest served by " +
			"anyone would then be trusted, and the digests it names are its own")
	}

	// ASSERTED BY WHAT IT MATCHES, NOT BY HOW IT IS SPELLED. The first version of
	// this checked for the substring "guest-image.yml" and failed against a correct
	// pattern, because QuoteMeta had escaped the dot. Testing the spelling of a
	// regex tests the wrong thing anyway: what matters is which certificates it
	// accepts.
	pattern, err := regexp.Compile(p.Identity)
	if err != nil {
		t.Fatalf("the built-in identity is not a usable pattern: %v", err)
	}

	// The SAN a real signature from this project carries -- taken from the
	// certificate of the first published release.
	published := "https://github.com/junioryono/billet/.github/workflows/" +
		"guest-image.yml@refs/heads/main"

	if !pattern.MatchString(published) {
		t.Errorf("the built-in identity %q does not match a real signature from this "+
			"project's publishing workflow (%q)", p.Identity, published)
	}

	// AND THE ONES IT MUST REJECT. A workflow added by a pull request is a far
	// lower bar to clear than compromising the release process, so pinning only the
	// repository is not enough.
	for _, wrong := range []string{
		"https://github.com/junioryono/billet/.github/workflows/ci.yml@refs/heads/main",
		"https://github.com/junioryono/billet/.github/workflows/attacker.yml@refs/pull/1/head",
		// THE ONE THAT SLIPPED THROUGH. The publishing workflow's own name, signed
		// from a PULL REQUEST ref. A contributor who opens a pr modifying
		// guest-image.yml gets a certificate with exactly this SAN, and a pattern
		// ending `@refs/.+` accepts it -- which is a far lower bar to clear than
		// compromising the release process.
		"https://github.com/junioryono/billet/.github/workflows/guest-image.yml@refs/pull/1/head",
		"https://github.com/junioryono/billet/.github/workflows/guest-image.yml@refs/heads/attacker",
		"https://github.com/junioryono/billet/.github/workflows/guest-image.yml@refs/tags/v1",
		"https://github.com/someoneelse/billet/.github/workflows/guest-image.yml@refs/heads/main",
		"https://github.com/junioryono/billet-evil/.github/workflows/guest-image.yml@refs/heads/main",
		"https://evil.test/junioryono/billet/.github/workflows/guest-image.yml@refs/heads/main",
	} {
		if pattern.MatchString(wrong) {
			t.Errorf("the built-in identity accepts %q, which is not this project's "+
				"publishing workflow", wrong)
		}
	}

	if p.Issuer != GitHubOIDCIssuer {
		t.Errorf("issuer = %q, want the github actions oidc issuer", p.Issuer)
	}
}

// A CUSTOM SOURCE WITH NO IDENTITY AND NO EXPLICIT WAIVER IS AN ERROR, NOT A
// SILENT DOWNGRADE.
//
// This is the fail-open that would otherwise hide here: an operator points at an
// internal mirror, billet's identity cannot match it, and the easy implementation
// quietly stops verifying. The manifest is then whatever the mirror says, and its
// digests are its own -- which is the entire attack this exists to prevent.
func TestPolicyForACustomSourceRefusesToDowngradeSilently(t *testing.T) {
	src, err := ParseSource("https://mirror.internal/billet")
	if err != nil {
		t.Fatalf("source: %v", err)
	}

	_, err = PolicyFor(src, "", "", false)
	if err == nil {
		t.Fatal("a custom source with no configured identity was accepted; verification " +
			"would silently not happen")
	}

	// AND THE MESSAGE HAS TO SAY WHAT TO DO. An operator who has just pointed at
	// their own mirror needs both options named.
	for _, want := range []string{"identity", "skip"} {
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

func TestPolicyForACustomSourceWithAnIdentityRequiresIt(t *testing.T) {
	src, err := ParseSource("https://mirror.internal/billet")
	if err != nil {
		t.Fatalf("source: %v", err)
	}

	p, err := PolicyFor(src, "https://github.com/acme/images/.*", "https://token.example", false)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	if !p.Required {
		t.Fatal("a configured identity did not require verification")
	}

	if p.Identity != "https://github.com/acme/images/.*" {
		t.Errorf("identity = %q", p.Identity)
	}

	if p.Issuer != "https://token.example" {
		t.Errorf("issuer = %q", p.Issuer)
	}
}

// SKIPPING IS ALLOWED AND MUST BE DELIBERATE. An air-gapped deployment with its own
// distribution has a real reason; the point is that it is an explicit act rather
// than what happens when nothing is configured.
func TestPolicyForAnExplicitSkipDoesNotRequireVerification(t *testing.T) {
	src, err := ParseSource("https://mirror.internal/billet")
	if err != nil {
		t.Fatalf("source: %v", err)
	}

	p, err := PolicyFor(src, "", "", true)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	if p.Required {
		t.Error("an explicit skip still required verification")
	}
}

// AN IDENTITY THAT IS NOT A USABLE PATTERN IS REFUSED AT CONFIGURATION TIME, not
// discovered when a pull fails to match it.
func TestPolicyForRefusesAnIdentityThatIsNotAPattern(t *testing.T) {
	src, err := ParseSource("https://mirror.internal/billet")
	if err != nil {
		t.Fatalf("source: %v", err)
	}

	if _, err := PolicyFor(src, "([unclosed", "https://token.example", false); err == nil {
		t.Error("an unparseable identity pattern was accepted")
	}
}

// AN IDENTITY WITHOUT AN ISSUER IS HALF A POLICY. The SAN alone does not say who
// vouched for it, and any issuer able to mint a certificate with that name would
// satisfy it.
func TestPolicyForRequiresAnIssuerAlongsideAnIdentity(t *testing.T) {
	src, err := ParseSource("https://mirror.internal/billet")
	if err != nil {
		t.Fatalf("source: %v", err)
	}

	if _, err := PolicyFor(src, "https://github.com/acme/images/.*", "", false); err == nil {
		t.Error("an identity with no issuer was accepted; any issuer able to mint that " +
			"name would satisfy the policy")
	}
}
