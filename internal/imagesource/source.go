package imagesource

import (
	"fmt"
	"net/url"
	"regexp"
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

// DefaultChannelURL discovers the current immutable guest-image release.
//
// RELEASE IMMUTABILITY MAKES A MOVING RELEASE IMPOSSIBLE. A published release's
// tag and assets cannot be replaced, so the old guest-latest release froze on its
// first generation. The guest-channel branch carries a signed, expiring pointer;
// the manifest and large assets still come from an immutable dated release, and
// every byte is bound to the signed manifest afterward. raw.githubusercontent.com
// avoids the API's per-address anonymous rate limit for fleets behind one NAT.
const DefaultChannelURL = "https://raw.githubusercontent.com/" + DefaultRepo + "/guest-channel/current.json"

// DefaultChannelBundleURL authenticates DefaultChannelURL.
const DefaultChannelBundleURL = "https://raw.githubusercontent.com/" + DefaultRepo + "/guest-channel/current.sigstore.json"

const defaultDownloadPrefix = "https://github.com/" + DefaultRepo + "/releases/download/"

var guestReleasePattern = regexp.MustCompile(`^guest-\d{8}-\d{6}$`)

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
	// trailing slash. It is populated by Client.Resolve for the default source.
	BaseURL string

	channelURL       string
	channelBundleURL string
	channelPolicy    *Policy
	official         bool
}

// DefaultSource discovers the current immutable guest-image release published by
// this project.
func DefaultSource() Source {
	return Source{
		channelURL:       DefaultChannelURL,
		channelBundleURL: DefaultChannelBundleURL,
		official:         true,
	}
}

// IsDefault reports whether this is billet's signed first-party image channel.
func (s Source) IsDefault() bool {
	return s.official
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

	return Source{
		BaseURL:  trimmed,
		official: isOfficialDatedSource(trimmed),
	}, nil
}

func isOfficialDatedSource(raw string) bool {
	tag, ok := strings.CutPrefix(raw, defaultDownloadPrefix)

	return ok && guestReleasePattern.MatchString(tag)
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
