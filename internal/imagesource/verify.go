package imagesource

import (
	"bytes"
	_ "embed"
	"fmt"
	"strings"
	"sync"

	"github.com/sigstore/sigstore-go/pkg/bundle"
	"github.com/sigstore/sigstore-go/pkg/fulcio/certificate"
	"github.com/sigstore/sigstore-go/pkg/root"
	"github.com/sigstore/sigstore-go/pkg/verify"
)

// trustedRootJSON is sigstore's trust root, vendored into the binary.
//
// EMBEDDED BECAUSE A NODE MAY BE AIR GAPPED, and the library's own documentation
// is blunt about the alternative: its TUF client refreshes whenever its cache
// expires, so relying on TUF at verification time means reaching
// tuf-repo-cdn.sigstore.dev from a machine that by assumption cannot. The library
// embeds only the TUF BOOTSTRAP root, not the trust root itself, so this file has
// to travel with billet.
//
// REFRESHED ON BILLET'S OWN RELEASE CADENCE. Sigstore's public-good roots rotate
// on the order of years, so a release-cadence refresh is the right granularity --
// and a node silently reaching the internet to get one is not.
//
//go:embed trustedroot/trusted_root.json
var trustedRootJSON []byte

var (
	trustedRootOnce sync.Once
	trustedRoot     *root.TrustedRoot
	trustedRootErr  error
)

// TrustedRoot is the embedded sigstore trust root.
//
// PARSED ONCE. It is a few kilobytes of JSON and parsing it per verification would
// be wasteful, but the real reason is that a pull verifies two things and both
// should be judged against the same root rather than two parses that could in
// principle disagree.
func TrustedRoot() (*root.TrustedRoot, error) {
	trustedRootOnce.Do(func() {
		trustedRoot, trustedRootErr = root.NewTrustedRootFromJSON(trustedRootJSON)
		if trustedRootErr != nil {
			trustedRootErr = fmt.Errorf("imagesource: the embedded sigstore trust root could "+
				"not be parsed, so nothing can be verified: %w", trustedRootErr)
		}
	})

	return trustedRoot, trustedRootErr
}

// VerifySignature checks a manifest against its signature bundle under a policy.
//
// THIS IS WHAT MAKES EVERY OTHER CHECK MEAN ANYTHING. Each asset is verified
// against a digest the MANIFEST names, so a manifest an attacker serves names
// digests of bytes the attacker chose and every one of those checks passes. The
// signature is the only thing binding the manifest to the workflow that produced
// it; without it the rest is a checksum against itself.
func VerifySignature(manifest, bundleJSON []byte, p Policy) error {
	// NOT AN ERROR. This is the air-gapped path, where an operator has said
	// explicitly that the source is trusted by other means, and turning that into a
	// failure would make the waiver useless.
	if !p.Required {
		return nil
	}

	if len(bytes.TrimSpace(bundleJSON)) == 0 {
		return fmt.Errorf("imagesource: this source requires a signature and none was " +
			"published alongside the manifest. An absent signature is not an unsigned " +
			"image that happens to be fine -- it is a manifest nothing has vouched for, " +
			"and every digest it names is its own")
	}

	tr, err := TrustedRoot()
	if err != nil {
		return err
	}

	var b bundle.Bundle
	if err := b.UnmarshalJSON(bundleJSON); err != nil {
		// THE LEGACY FORMAT IS THE LIKELY CAUSE AND DESERVES ITS OWN SENTENCE. An
		// image published before the workflow switched carries cosign's old shape,
		// which this library does not parse -- so the signature is genuine and
		// unreadable at once, and the operator needs to be told to republish rather
		// than left reading a message about an unrecognised field.
		if looksLegacy(bundleJSON) {
			return fmt.Errorf("imagesource: this image is signed with cosign's legacy bundle "+
				"format, which cannot be verified. The signature is real; nothing can read "+
				"it. Republish the image with a workflow that passes --new-bundle-format "+
				"(the legacy format also carries no inclusion proof, so it could not be "+
				"verified offline either): %w", err)
		}

		return fmt.Errorf("imagesource: the signature bundle could not be read: %w", err)
	}

	identity, err := verify.NewShortCertificateIdentity(p.Issuer, "", "", p.Identity)
	if err != nil {
		return fmt.Errorf("imagesource: the signing policy is not usable: %w", err)
	}

	verifier, err := verify.NewVerifier(tr,
		// EACH OF THESE IS A SEPARATE CLAIM AND ALL THREE ARE REQUIRED.
		//
		// The certificate transparency log proves the signing certificate was
		// published rather than minted quietly. The transparency log proves the
		// signature itself was. The observer timestamp is what makes a Fulcio
		// certificate verifiable at all: they live about ten minutes, so without a
		// trusted time to check validity AT, every signature older than that would
		// fail.
		verify.WithSignedCertificateTimestamps(1),
		verify.WithTransparencyLog(1),
		verify.WithObserverTimestamps(1),
	)
	if err != nil {
		return fmt.Errorf("imagesource: could not build a verifier: %w", err)
	}

	if _, err := verifier.Verify(&b, verify.NewPolicy(
		verify.WithArtifact(bytes.NewReader(manifest)),
		verify.WithCertificateIdentity(identity),
	)); err != nil {
		return fmt.Errorf("imagesource: the manifest's signature does not satisfy this "+
			"source's policy (identity %s, issuer %s): %w", p.Identity, p.Issuer, err)
	}

	return nil
}

// looksLegacy reports whether a bundle is cosign's pre-protobuf shape.
//
// MATCHED ON ITS DISTINCTIVE FIELD NAMES rather than by trying to parse it: the
// point is only to produce a better error, so a false negative costs nothing and a
// dependency on the old format's parser would cost a dependency.
func looksLegacy(bundleJSON []byte) bool {
	head := bundleJSON
	if len(head) > 4096 {
		head = head[:4096]
	}

	text := string(head)

	return strings.Contains(text, `"base64Signature"`) || strings.Contains(text, `"rekorBundle"`)
}

// certificateExtensions is kept as documentation of what a stricter policy would
// pin, and as the place to put it when the identity becomes configurable per
// deployment.
//
// NOT ENFORCED YET, DELIBERATELY. Pinning SourceRepositoryURI, RunnerEnvironment
// and BuildConfigURI is strictly better than the SAN alone -- the last of those is
// the one that closes reusable-workflow confusion -- but every value has to be
// known to be correct before it can be required, and requiring a wrong one refuses
// every genuine image. The SAN already encodes repository, workflow and ref, which
// is the primary binding.
var _ = certificate.Extensions{}
