package imagesource

import (
	"fmt"
	"net/url"
	"strings"
)

// DefaultRepo is the project's own home, and the ONE place its name is written.
//
// EVERY OTHER REFERENCE IS DERIVED. This project expects to move between
// accounts, and a name spread across a fetch URL, a signing identity and a
// documentation link is a rename that half-lands: the binary keeps pulling from
// the old place while the signature check names the new one, and the failure
// arrives as a signature mismatch that reads like an attack.
//
// It is also only a DEFAULT. The pull path takes a source from configuration,
// because a deployment that mirrors artifacts internally — or is not on the
// public internet at all — must not have to patch a constant to do it. Retro-
// fitting that is the specific thing that hurt other projects who baked one
// registry in and then needed a second.
const DefaultRepo = "junioryono/billet"

// LatestTag is the release whose assets are replaced by each guest-image build.
//
// A FIXED TAG, NOT `/releases/latest/`. GitHub resolves `latest` across the WHOLE
// repository, and this repository also publishes billet's own binaries — so
// `/releases/latest/download/manifest.json` would resolve to whichever release
// was cut most recently, and a binary release would silently take over the image
// channel. The failure is not a 404 either: a node would fetch a manifest that
// is simply absent and report a source with no images, on a source that has
// them.
//
// Dated releases are still published for history and for pinning; this tag is
// the moving pointer, which is what an unattended puller needs.
const LatestTag = "guest-latest"

// DefaultBaseURL is where this build looks for published guest images.
//
// A DOWNLOAD URL RATHER THAN THE API. It redirects straight to the asset with no
// token and no api.github.com call. That matters because unauthenticated GitHub
// API requests are limited to sixty an hour PER ORIGINATING ADDRESS, so
// deployments sharing an egress address would spend each other's budget merely
// asking what the current version is. Asset downloads are not on that limiter.
const DefaultBaseURL = "https://github.com/" + DefaultRepo + "/releases/download/" + LatestTag

// ManifestName is the index document within a release.
const ManifestName = "manifest.json"

// BundleName is the signature over the manifest.
//
// A SIGSTORE BUNDLE, NOT COSIGN'S LEGACY SHAPE. The two are different documents:
// the legacy one carries base64Signature, cert and rekorBundle, and the library
// that verifies this on a node parses only the protobuf bundle -- it rejects the
// other outright. The first release published the legacy form, so nothing could
// have verified it even though the signature was real. The extension is part of
// how they are told apart.
//
// The new format also carries the transparency-log inclusion proof, which is what
// makes verification possible without reaching Rekor -- and a node that may be air
// gapped cannot reach Rekor.
const BundleName = "manifest.sigstore.json"

// Source names where images are fetched from.
type Source struct {
	// BaseURL is the directory the manifest and its assets sit in, without a
	// trailing slash.
	BaseURL string
}

// ParseSource validates a configured base URL.
//
// HTTPS IS REQUIRED EVEN THOUGH THE CONTENT IS VERIFIED. Digest and signature
// checks make plaintext transport survivable, not harmless: an observer still
// learns which version a deployment runs, and — more usefully to them — an
// active party can hold a node on an old-but-genuine manifest indefinitely,
// which passes every check this makes and quietly walks the fleet into the
// thirty-day expiry. Transport security is what makes that expensive.
func ParseSource(raw string) (Source, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return Source{}, fmt.Errorf("imagesource: no image source configured")
	}

	// THE DELIMITERS ARE REFUSED BEFORE PARSING, not after.
	//
	// url.Parse reports an EMPTY RawQuery for "https://host/p?" and an EMPTY
	// Fragment for "https://host/p#", so checking the parsed fields let both
	// through -- and because the raw string is what asset names are appended to,
	// the result was "https://host/p?/manifest.json", which fetches /p with a
	// query rather than the manifest. Trimming trailing slashes made it worse:
	// "https://host/p#/" became "https://host/p#", which then passed.
	//
	// Neither character can appear in a legitimate base url here, so they are
	// refused by inspecting the string itself, where an empty delimiter is still
	// a delimiter.
	if strings.ContainsAny(trimmed, "?#") {
		return Source{}, fmt.Errorf("imagesource: the image source %q carries a query or a "+
			"fragment delimiter, and asset names are appended to it as path", raw)
	}

	trimmed = strings.TrimRight(trimmed, "/")

	u, err := url.Parse(trimmed)
	if err != nil {
		return Source{}, fmt.Errorf("imagesource: %q is not a url: %w", raw, err)
	}

	if u.Scheme != "https" {
		return Source{}, fmt.Errorf("imagesource: the image source is %q; it must be https, so "+
			"that nobody on the path can choose which version this deployment sees", raw)
	}

	// Hostname(), NOT Host. "https://:443/path" parses with a non-empty Host of
	// ":443" and no hostname at all, which is not something to send a request to.
	if u.Hostname() == "" {
		return Source{}, fmt.Errorf("imagesource: the image source %q names no host", raw)
	}

	// CREDENTIALS ARE REFUSED RATHER THAN CARRIED. A userinfo section would ride
	// in every asset url this builds, and those urls are printed in error
	// messages and logs -- so a password configured once here would be copied
	// wherever a download failed. Artifact fetching is anonymous by design; a
	// source that needs credentials wants a different mechanism, not this one.
	if u.User != nil {
		return Source{}, fmt.Errorf("imagesource: the image source carries credentials in its " +
			"url; they would be repeated in every asset url and in any error naming one")
	}

	return Source{BaseURL: trimmed}, nil
}

// AssetURL is where a named file within the release lives.
//
// The name has already been through Asset.validate, which is what makes simple
// concatenation safe: it holds no separator, so it cannot climb out of the
// release it belongs to.
func (s Source) AssetURL(name string) (string, error) {
	if !namePattern.MatchString(name) {
		return "", fmt.Errorf("imagesource: %q is not a plain file name", name)
	}

	return s.BaseURL + "/" + name, nil
}

// ManifestURL is where the index for the current release lives.
func (s Source) ManifestURL() string {
	return s.BaseURL + "/" + ManifestName
}
