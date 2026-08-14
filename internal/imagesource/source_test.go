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
