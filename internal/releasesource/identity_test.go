package releasesource

import (
	"regexp"
	"testing"
)

// THE POLICY MUST NAME THE IDENTITY THE RELEASE WORKFLOW ACTUALLY SIGNS WITH.
//
// cut-release.yml runs on main and calls release.yml, so the certificate's SAN
// carries `refs/heads/main`; a hotfix tag pushed by hand runs release.yml under
// `refs/tags/vX.Y.Z`. The first version of the pattern accepted a release branch
// or a tag on the belief that release.yml runs against the tag, and every binary
// shipped with it refused every manifest billet published. This pins the SAN
// measured on v0.6.0's bundle, so the policy cannot drift from the pipeline
// again without a test naming the exact string that stopped matching.
func TestTheSigningIdentityAcceptsWhatTheReleaseWorkflowSignsWith(t *testing.T) {
	t.Parallel()

	// THE POLICY THE COMMANDS USE, not the constant behind it: a test on the
	// constant stays green if PolicyForRelease stops handing it out.
	policy, err := PolicyForRelease(false)
	if err != nil {
		t.Fatalf("PolicyForRelease: %v", err)
	}

	if !policy.Required {
		t.Fatal("the default release policy does not require a signature")
	}

	if policy.Issuer != "https://token.actions.githubusercontent.com" {
		t.Fatalf("the default release policy trusts issuer %q, want GitHub Actions", policy.Issuer)
	}

	identity := regexp.MustCompile(policy.Identity)

	for _, san := range []string{
		// The cut button: what v0.6.0's release-manifest.sigstore.json carries.
		"https://github.com/junioryono/billet/.github/workflows/release.yml@refs/heads/main",
		// A hotfix tag pushed by hand.
		"https://github.com/junioryono/billet/.github/workflows/release.yml@refs/tags/v0.6.1",
	} {
		if !identity.MatchString(san) {
			t.Errorf("the policy refuses %q, which the release workflow signs with", san)
		}
	}

	for _, san := range []string{
		// The low bar: a workflow a pull request introduced, or a branch.
		"https://github.com/junioryono/billet/.github/workflows/release.yml@refs/pull/12/head",
		"https://github.com/junioryono/billet/.github/workflows/release.yml@refs/heads/junior-fix",
		// Another workflow in the repository, on main.
		"https://github.com/junioryono/billet/.github/workflows/ci.yml@refs/heads/main",
		// Another repository.
		"https://github.com/someone/billet/.github/workflows/release.yml@refs/heads/main",
		// A prefix or suffix around the exact SAN, and a path below main.
		"https://github.com/junioryono/billet/.github/workflows/release.yml@refs/heads/main2",
		"https://github.com/junioryono/billet/.github/workflows/release.yml@refs/heads/main/x",
		"xhttps://github.com/junioryono/billet/.github/workflows/release.yml@refs/heads/main",
		// A release branch: release.yml has no branch trigger, so nothing signs there.
		"https://github.com/junioryono/billet/.github/workflows/release.yml@refs/heads/release/v0.6",
	} {
		if identity.MatchString(san) {
			t.Errorf("the policy accepts %q, which nothing billet publishes is signed with", san)
		}
	}
}
