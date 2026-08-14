package imagesource

import (
	"strings"
	"testing"
)

func TestParseSourceAcceptsHTTPS(t *testing.T) {
	s, err := ParseSource("https://example.test/billet/releases/latest/download")
	if err != nil {
		t.Fatalf("a plain https source was refused: %v", err)
	}

	if got := s.ManifestURL(); got != "https://example.test/billet/releases/latest/download/manifest.json" {
		t.Errorf("manifest url = %q", got)
	}
}

func TestParseSourceTrimsATrailingSlashSoURLsDoNotDoubleUp(t *testing.T) {
	s, err := ParseSource("https://example.test/download/")
	if err != nil {
		t.Fatalf("refused: %v", err)
	}

	if strings.Contains(s.ManifestURL(), "//manifest") {
		t.Errorf("a trailing slash produced %q", s.ManifestURL())
	}
}

// PLAINTEXT IS REFUSED EVEN THOUGH CONTENT IS VERIFIED. The digest check does
// not stop somebody on the path from pinning a node to an older-but-genuine
// manifest forever, which passes every check here and walks the fleet into
// github's thirty-day expiry.
func TestParseSourceRefusesPlaintextAndOtherSchemes(t *testing.T) {
	for _, raw := range []string{
		"http://example.test/download",
		"ftp://example.test/download",
		"file:///tmp/images",
		"example.test/download",
		"",
		"   ",
		"https://",
	} {
		if _, err := ParseSource(raw); err == nil {
			t.Errorf("%q was accepted as an image source", raw)
		}
	}
}

func TestParseSourceRefusesAQueryOrFragment(t *testing.T) {
	for _, raw := range []string{
		"https://example.test/download?ref=main",
		"https://example.test/download#latest",
	} {
		if _, err := ParseSource(raw); err == nil {
			t.Errorf("%q was accepted, and asset names are appended to it as path", raw)
		}
	}
}

// The name has been validated by the time it reaches AssetURL, but AssetURL is
// exported and a future caller could reach it another way.
func TestAssetURLRefusesANameThatIsNotAPlainFile(t *testing.T) {
	s, err := ParseSource("https://example.test/download")
	if err != nil {
		t.Fatalf("refused: %v", err)
	}

	for _, name := range []string{"../manifest.json", "a/b", "", "/etc/passwd"} {
		if _, err := s.AssetURL(name); err == nil {
			t.Errorf("%q was turned into an asset url", name)
		}
	}

	got, err := s.AssetURL("rootfs.img.zst")
	if err != nil {
		t.Fatalf("a plain name was refused: %v", err)
	}

	if got != "https://example.test/download/rootfs.img.zst" {
		t.Errorf("asset url = %q", got)
	}
}

// THE PROJECT'S NAME IS WRITTEN ONCE. This is the test that fails when somebody
// hardcodes it a second time, which is the shape of a half-landed rename: the
// binary keeps pulling from the old account while the signature check names the
// new one.
func TestDefaultBaseURLIsDerivedFromTheOneRepoConstant(t *testing.T) {
	if !strings.Contains(DefaultBaseURL, DefaultRepo) {
		t.Fatalf("DefaultBaseURL %q does not derive from DefaultRepo %q",
			DefaultBaseURL, DefaultRepo)
	}

	if _, err := ParseSource(DefaultBaseURL); err != nil {
		t.Fatalf("the built-in default is not a valid source: %v", err)
	}
}

// `/releases/latest/` RESOLVES ACROSS THE WHOLE REPOSITORY, and this repository
// also publishes billet's own binaries. Pointing the image channel at it would
// have a binary release silently take the channel over -- and not as a 404: the
// node fetches a manifest that is simply absent and reports a source with no
// images, on a source that has them.
func TestDefaultBaseURLDoesNotUseTheRepoWideLatestAlias(t *testing.T) {
	if strings.Contains(DefaultBaseURL, "/releases/latest/") {
		t.Fatalf("DefaultBaseURL %q resolves across every release in the repository, "+
			"including billet's own binary releases", DefaultBaseURL)
	}

	if !strings.Contains(DefaultBaseURL, LatestTag) {
		t.Errorf("DefaultBaseURL %q does not name the guest-image tag %q",
			DefaultBaseURL, LatestTag)
	}
}

// url.Parse REPORTS AN EMPTY RawQuery FOR "host/p?" AND AN EMPTY Fragment FOR
// "host/p#", so checking the parsed fields let both through -- and since asset
// names are appended to the RAW string, the result fetched the directory with a
// query rather than the file. Trimming trailing slashes made it worse: "p#/"
// became "p#", which then passed.
func TestParseSourceRefusesEmptyQueryAndFragmentDelimiters(t *testing.T) {
	for _, raw := range []string{
		"https://example.test/download?",
		"https://example.test/download#",
		"https://example.test/download#/",
		"https://example.test/download?/",
		"https://example.test/dow?nload",
	} {
		if _, err := ParseSource(raw); err == nil {
			t.Errorf("%q was accepted; appending an asset name to it does not produce a path", raw)
		}
	}
}

// CREDENTIALS WOULD RIDE IN EVERY ASSET URL and therefore into every error
// message and log line naming one.
func TestParseSourceRefusesCredentialsInTheURL(t *testing.T) {
	for _, raw := range []string{
		"https://user:secret@example.test/download",
		"https://user@example.test/download",
	} {
		_, err := ParseSource(raw)
		if err == nil {
			t.Fatalf("%q was accepted, and the credentials would be repeated in every asset url", raw)
		}

		if strings.Contains(err.Error(), "secret") {
			t.Errorf("the refusal for %q repeats the credential: %v", raw, err)
		}
	}
}

// "https://:443/path" PARSES WITH A NON-EMPTY Host OF ":443" and no hostname,
// which is not something to send a request to.
func TestParseSourceRequiresARealHostname(t *testing.T) {
	for _, raw := range []string{
		"https://:443/download",
		"https:///download",
	} {
		if _, err := ParseSource(raw); err == nil {
			t.Errorf("%q was accepted as a source", raw)
		}
	}
}
