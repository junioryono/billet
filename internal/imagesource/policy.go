package imagesource

import (
	"fmt"
	"regexp"
	"strings"
)

// GitHubOIDCIssuer is the issuer a GitHub Actions workflow's certificate carries.
const GitHubOIDCIssuer = "https://token.actions.githubusercontent.com"

// PublishWorkflow is the workflow allowed to sign billet's images.
//
// NAMED, NOT JUST THE REPOSITORY. A certificate's SAN identifies the WORKFLOW that
// requested it, so pinning only the repository would accept a signature from any
// other workflow in it -- including one added by a pull request, which is a far
// lower bar to clear than compromising the release process.
const PublishWorkflow = ".github/workflows/guest-image.yml"

// DefaultSigningIdentity is the certificate SAN a manifest from billet must carry.
//
// DERIVED FROM THE ONE CONSTANT NAMING THIS PROJECT, like the fetch URL, because a
// project that moves between accounts and forgets one of them gets a signature
// mismatch that reads exactly like an attack.
//
// The ref is deliberately open: images are published from main today, and pinning
// the branch here would make a release cut from anywhere else fail verification
// rather than fail review.
var DefaultSigningIdentity = fmt.Sprintf(
	`^https://github\.com/%s/%s@refs/.+$`,
	regexp.QuoteMeta(DefaultRepo), regexp.QuoteMeta(PublishWorkflow))

// Policy says what a source demands before its manifest may be trusted.
type Policy struct {
	// Required is whether the manifest's signature must verify.
	Required bool
	// Identity is the certificate SAN pattern a valid signature must carry.
	Identity string
	// Issuer is the OIDC issuer that certificate must come from.
	Issuer string
}

// PolicyFor decides what verification a source demands.
//
// THE MANIFEST IS THE ONLY THING THAT MAKES THE DIGESTS MEAN ANYTHING. Every asset
// is checked against a digest the manifest names, so a manifest an attacker serves
// names digests of bytes the attacker chose and every check passes. The signature
// is what binds the manifest to the workflow that produced it, and without it the
// rest of the verification is a checksum against itself.
//
// SO A MISSING POLICY IS AN ERROR RATHER THAN A SKIP. The tempting shape --
// "verify if an identity is configured" -- fails open exactly when an operator
// points at their own mirror and configures nothing, which is the common case and
// the one where the guarantee silently disappears.
func PolicyFor(src Source, identity, issuer string, skip bool) (Policy, error) {
	identity = strings.TrimSpace(identity)
	issuer = strings.TrimSpace(issuer)

	// EXPLICIT, AND IT WINS. An air-gapped deployment distributing its own images
	// has a real reason; the point is that it is an act somebody performed rather
	// than what happens when nothing is set.
	if skip {
		return Policy{Required: false}, nil
	}

	if identity == "" && issuer == "" {
		if src.BaseURL == DefaultBaseURL {
			return Policy{
				Required: true,
				Identity: DefaultSigningIdentity,
				Issuer:   GitHubOIDCIssuer,
			}, nil
		}

		return Policy{}, fmt.Errorf("imagesource: %s is not billet's own image source, so "+
			"billet's signing identity cannot vouch for what it serves, and nothing else has "+
			"been configured to. Set images.signing_identity and images.signing_issuer to the "+
			"workflow that signs your images, or pass --skip-signature-verification if this "+
			"source is trusted by other means. Refusing to import a manifest nothing has "+
			"vouched for, because every digest it names is its own", src.BaseURL)
	}

	// HALF A POLICY IS NOT A POLICY. A SAN says who the certificate is FOR; the
	// issuer says who vouched for that. Without the issuer, any authority able to
	// mint a certificate carrying that name satisfies the check.
	if identity == "" {
		return Policy{}, fmt.Errorf("imagesource: an issuer is configured with no identity, " +
			"so any workflow that issuer signs for would be accepted")
	}

	if issuer == "" {
		return Policy{}, fmt.Errorf("imagesource: a signing identity is configured with no " +
			"issuer, so any authority able to mint a certificate carrying that name would " +
			"satisfy it")
	}

	// COMPILED HERE, so an unusable pattern is a configuration error rather than
	// something discovered when a pull fails to match it at three in the morning.
	if _, err := regexp.Compile(identity); err != nil {
		return Policy{}, fmt.Errorf("imagesource: the configured signing identity is not a "+
			"usable pattern: %w", err)
	}

	return Policy{Required: true, Identity: identity, Issuer: issuer}, nil
}
