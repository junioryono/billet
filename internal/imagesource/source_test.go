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
func TestDefaultChannelURLIsDerivedFromTheOneRepoConstant(t *testing.T) {
	if !strings.Contains(DefaultChannelURL, DefaultRepo) {
		t.Fatalf("DefaultChannelURL %q does not derive from DefaultRepo %q",
			DefaultChannelURL, DefaultRepo)
	}
	if !strings.Contains(DefaultChannelBundleURL, DefaultRepo) {
		t.Fatalf("DefaultChannelBundleURL %q does not derive from DefaultRepo %q",
			DefaultChannelBundleURL, DefaultRepo)
	}
	if !DefaultSource().IsDefault() {
		t.Fatal("the built-in image source is not marked as the signed default channel")
	}
	for name, raw := range map[string]string{
		"channel": DefaultChannelURL,
		"bundle":  DefaultChannelBundleURL,
	} {
		if !strings.HasPrefix(raw, "https://raw.githubusercontent.com/") || strings.Contains(raw, "api.github.com") {
			t.Fatalf("default %s %q spends the shared anonymous REST budget", name, raw)
		}
	}
}

// The default cannot use either repository-wide latest or a fixed rolling Release:
// the former mixes binary and guest releases, while immutability freezes the
// latter as soon as it is published. A one-file branch is the moving pointer.
func TestDefaultChannelDoesNotUseAReleaseAlias(t *testing.T) {
	for _, forbidden := range []string{"/releases/latest", "guest-latest"} {
		if strings.Contains(DefaultChannelURL, forbidden) {
			t.Fatalf("DefaultChannelURL %q contains forbidden rolling alias %q",
				DefaultChannelURL, forbidden)
		}
	}
}

func TestOnlyDatedOfficialReleaseURLsGetTheDefaultTrustPolicy(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		trusted bool
	}{
		{raw: defaultDownloadPrefix + "guest-20260819-031238", trusted: true},
		{raw: defaultDownloadPrefix + "guest-latest"},
		{raw: defaultDownloadPrefix + "v0.2.0"},
		{raw: "https://github.com/attacker/billet/releases/download/guest-20260819-031238"},
		{raw: defaultDownloadPrefix + "guest-20260819-031238-extra"},
	} {
		src, err := ParseSource(tc.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.raw, err)
		}
		if src.IsDefault() != tc.trusted {
			t.Errorf("IsDefault(%q) = %t; want %t", tc.raw, src.IsDefault(), tc.trusted)
		}
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
