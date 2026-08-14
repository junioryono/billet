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

// DefaultBaseURL is where this build looks for published guest images.
//
// `/releases/latest/download/<name>` RATHER THAN THE API. It redirects straight
// to the asset with no token and no api.github.com call. That matters because
// unauthenticated GitHub API requests are limited to sixty an hour PER
// ORIGINATING ADDRESS, so deployments sharing an egress address would spend each
// other's budget merely asking what the latest version is. Asset downloads are
// not on that limiter.
const DefaultBaseURL = "https://github.com/" + DefaultRepo + "/releases/latest/download"

// ManifestName is the index document within a release.
const ManifestName = "manifest.json"

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

	trimmed = strings.TrimRight(trimmed, "/")

	u, err := url.Parse(trimmed)
	if err != nil {
		return Source{}, fmt.Errorf("imagesource: %q is not a url: %w", raw, err)
	}

	if u.Scheme != "https" {
		return Source{}, fmt.Errorf("imagesource: the image source is %q; it must be https, so "+
			"that nobody on the path can choose which version this deployment sees", raw)
	}

	if u.Host == "" {
		return Source{}, fmt.Errorf("imagesource: the image source %q names no host", raw)
	}

	// A QUERY OR A FRAGMENT MEANS THE ASSET NAME WOULD BE APPENDED PAST IT.
	// Refused here rather than producing a URL that fetches the directory and
	// ignores the file.
	if u.RawQuery != "" || u.Fragment != "" {
		return Source{}, fmt.Errorf("imagesource: the image source %q carries a query or a "+
			"fragment, and asset names are appended to it as path", raw)
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
