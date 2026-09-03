package releasesource

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// releaseServer serves a channel, a manifest and an artifact the way the release
// does, so a test can corrupt exactly one of them.
type releaseServer struct {
	channel  []byte
	manifest []byte
	artifact []byte
	tag      string
	channels map[string][]byte
}

func (s *releaseServer) handler(t *testing.T) http.Handler {
	t.Helper()

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".sigstore.json"):
			// A SIGNATURE IS SERVED SO THE FETCH PATH IS EXERCISED. What it
			// contains is the verifier's business, and these tests replace the
			// verifier: minting a real Fulcio certificate needs the network and
			// proves sigstore works rather than that billet calls it.
			write(t, w, []byte(`{"stub":true}`))
		case strings.HasSuffix(r.URL.Path, "/"+ManifestName):
			write(t, w, s.manifest)
		case strings.Contains(r.URL.Path, "/release-channel/"):
			name := strings.TrimSuffix(filepath.Base(r.URL.Path), ".json")
			if body, ok := s.channels[name]; ok {
				write(t, w, body)

				return
			}

			write(t, w, s.channel)
		case strings.Contains(r.URL.Path, "/releases/download/"):
			write(t, w, s.artifact)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// newReleaseServer stands up a consistent release: the channel names the
// manifest's digest, and the manifest names the artifact's.
func newReleaseServer(t *testing.T) (*releaseServer, *Client) {
	t.Helper()

	artifact := []byte("this is a billet archive")
	sum := sha256.Sum256(artifact)

	m := good()
	m.Artifacts[0].Size = int64(len(artifact))
	m.Artifacts[0].SHA256 = hex.EncodeToString(sum[:])

	manifest := mustJSON(t, m)
	manifestSum := sha256.Sum256(manifest)

	statement, err := NewChannelStatement(ChannelStable, m.Version,
		hex.EncodeToString(manifestSum[:]), channelNow)
	if err != nil {
		t.Fatalf("NewChannelStatement: %v", err)
	}

	s := &releaseServer{
		channel:  render(t, statement),
		manifest: manifest,
		artifact: artifact,
		tag:      m.Version,
		channels: map[string][]byte{},
	}

	srv := httptest.NewServer(s.handler(t))
	t.Cleanup(srv.Close)

	c := &Client{
		HTTP: srv.Client(),
		Repo: "junioryono/billet",
		Now:  func() time.Time { return channelNow },
		// NO SIGNATURE ARITHMETIC HERE. These tests are about the refusals AROUND
		// the signature — the digest, the tag, the size — and a stub verifier is
		// what lets each of them fail for its own reason.
		verify: func([]byte, []byte, Policy) error { return nil },
	}

	// Point every URL builder at the test server by rewriting the transport's
	// destination rather than the URLs, so the production URL shapes stay under
	// test.
	c.HTTP = &http.Client{Transport: rewriteHost{to: srv.URL, inner: srv.Client().Transport}}

	return s, c
}

// rewriteHost sends every request to the test server while leaving the path
// alone, so the URL builders are exercised exactly as they ship.
type rewriteHost struct {
	to    string
	inner http.RoundTripper
}

func (r rewriteHost) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())

	target, err := http.NewRequestWithContext(req.Context(), req.Method,
		r.to+req.URL.Path, http.NoBody)
	if err != nil {
		return nil, err
	}

	clone.URL = target.URL
	clone.Host = target.Host

	inner := r.inner
	if inner == nil {
		inner = http.DefaultTransport
	}

	return inner.RoundTrip(clone)
}

func requiredPolicy() Policy {
	return Policy{Required: true, Identity: DefaultSigningIdentity, Issuer: "https://example"}
}

func TestAConsistentReleaseResolvesAndDownloads(t *testing.T) {
	s, c := newReleaseServer(t)

	statement, err := c.Resolve(t.Context(), ChannelStable, requiredPolicy())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	if statement.Tag != s.tag {
		t.Fatalf("the channel resolved to %q, want %q", statement.Tag, s.tag)
	}

	m, digest, err := c.Manifest(t.Context(), statement.Tag, statement.ManifestSHA256,
		requiredPolicy())
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}

	// THE DIGEST IS WHAT A ROLLOUT PERSISTS, so it has to come back from the
	// fetch rather than being recomputed from a re-serialised manifest — which
	// would hash a document this program produced rather than the published one.
	if digest != statement.ManifestSHA256 {
		t.Errorf("the fetch reported digest %s, want the channel's %s",
			digest, statement.ManifestSHA256)
	}

	a, err := m.Select("linux", "amd64", KindArchive)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}

	dir := t.TempDir()

	path, err := c.Download(t.Context(), m.Version, a, dir)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the downloaded artifact: %v", err)
	}

	if !bytes.Equal(got, s.artifact) {
		t.Errorf("the downloaded bytes are %q", got)
	}
}

// THE CHANNEL'S DIGEST IS CHECKED AGAINST THE MANIFEST'S BYTES.
//
// A tag names a release; a digest names its contents. GitHub release immutability
// should make the two equivalent, which is a reason to check rather than a reason
// not to: an assumption billet can verify for free is one it should verify.
func TestAManifestThatDoesNotMatchTheChannelDigestIsRefused(t *testing.T) {
	s, c := newReleaseServer(t)

	s.manifest = append(s.manifest, ' ')

	_, _, err := c.Manifest(t.Context(), s.tag, strings.Repeat("d", 64), requiredPolicy())
	if err == nil {
		t.Fatal("a manifest that does not match the channel's digest was accepted")
	}

	if !strings.Contains(err.Error(), "vouched for") {
		t.Errorf("the refusal does not name what the channel vouched for: %v", err)
	}
}

// A MANIFEST MUST AGREE WITH ITS OWN TAG.
//
// Without this, a correctly signed manifest for one release served at another
// release's address is accepted whole — every signature valid, every digest
// correct, and the wrong version installed under the tag an operator asked for.
func TestAManifestServedUnderAnotherTagIsRefused(t *testing.T) {
	s, c := newReleaseServer(t)

	_, _, err := c.Manifest(t.Context(), "v0.9.9", "", requiredPolicy())
	if err == nil {
		t.Fatal("a manifest served under a tag it does not name was accepted")
	}

	if !strings.Contains(err.Error(), "does not agree with its own tag") {
		t.Errorf("the refusal does not name the disagreement: %v", err)
	}

	_ = s
}

// A SIGNATURE THAT DOES NOT VERIFY STOPS THE RESOLUTION, after exactly one
// refetch. The pair can legitimately disagree for a moment while the branch
// advances; retrying forever would turn a bad signature into a hang.
func TestAnInauthenticChannelIsRefusedAfterOneRefetch(t *testing.T) {
	_, c := newReleaseServer(t)

	var attempts int

	c.verify = func([]byte, []byte, Policy) error {
		attempts++

		return errors.New("signature does not verify")
	}

	if _, err := c.Resolve(t.Context(), ChannelStable, requiredPolicy()); err == nil {
		t.Fatal("an inauthentic channel resolved")
	}

	if attempts != channelPairAttempts {
		t.Errorf("the channel was verified %d times, want %d", attempts, channelPairAttempts)
	}
}

// AND A PAIR THAT AGREES ON THE SECOND FETCH IS ACCEPTED, which is the CDN case
// the retry exists for.
func TestAChannelPairThatSettlesOnTheRefetchIsAccepted(t *testing.T) {
	_, c := newReleaseServer(t)

	var attempts int

	c.verify = func([]byte, []byte, Policy) error {
		attempts++
		if attempts == 1 {
			return errors.New("a stale CDN generation")
		}

		return nil
	}

	if _, err := c.Resolve(t.Context(), ChannelStable, requiredPolicy()); err != nil {
		t.Fatalf("a pair that settled on the refetch was refused: %v", err)
	}
}

// A DOWNLOAD WHOSE BYTES DO NOT MATCH THE MANIFEST LEAVES NOTHING BEHIND.
//
// A reader that finds the final name must find a verified file or nothing; an
// artifact written under its real name and only then rejected is indistinguishable
// from a complete one to whatever looks next.
func TestADownloadThatFailsVerificationPublishesNothing(t *testing.T) {
	s, c := newReleaseServer(t)

	m, err := ParseManifest(s.manifest)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	a, err := m.Select("linux", "amd64", KindArchive)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}

	// The server now serves different bytes of the same length.
	s.artifact = []byte(strings.Repeat("x", len(s.artifact)))

	dir := t.TempDir()

	if _, err := c.Download(t.Context(), m.Version, a, dir); err == nil {
		t.Fatal("an artifact that does not match its digest was published")
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the staging directory: %v", err)
	}

	for _, entry := range entries {
		if entry.Name() == a.Name {
			t.Errorf("a failed download left %s behind under its real name", a.Name)
		}
	}
}

// A RESPONSE LONGER THAN THE MANIFEST PROMISED IS A MISMATCH, caught by the
// bound rather than by the digest — which is only checkable after the last byte
// and so cannot stop a response that never ends.
func TestADownloadLongerThanItsDeclaredSizeIsRefused(t *testing.T) {
	s, c := newReleaseServer(t)

	m, err := ParseManifest(s.manifest)
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}

	a, err := m.Select("linux", "amd64", KindArchive)
	if err != nil {
		t.Fatalf("Select: %v", err)
	}

	s.artifact = append(s.artifact, []byte("and more")...)

	if _, err := c.Download(t.Context(), m.Version, a, t.TempDir()); err == nil {
		t.Fatal("an artifact longer than its declared size was accepted")
	}
}

// A CHANNEL BILLET DOES NOT PUBLISH IS REFUSED BEFORE ANY REQUEST IS MADE.
func TestResolvingAnUnknownChannelMakesNoRequest(t *testing.T) {
	_, c := newReleaseServer(t)

	c.HTTP = &http.Client{Transport: refusingTransport{t: t}}

	if _, err := c.Resolve(t.Context(), "nightly", requiredPolicy()); err == nil {
		t.Fatal("an unknown channel resolved")
	}
}

type refusingTransport struct{ t *testing.T }

func (r refusingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("a request was made for a channel billet does not publish")

	return nil, errors.New("no request should have been made")
}

// write serves a fixture body and fails the test if it cannot.
//
// NOT `_, _ = w.Write(...)`. A discarded error there is the shape errcheck is
// enabled on tests to catch: a fixture that silently served nothing would make
// every assertion downstream fail for a reason nobody could see.
func write(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()

	if _, err := w.Write(body); err != nil {
		t.Errorf("serve a fixture body: %v", err)
	}
}
