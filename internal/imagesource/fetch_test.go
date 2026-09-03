package imagesource

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)

	return hex.EncodeToString(sum[:])
}

// serve stands up a source whose paths map to the given bodies.
func serve(t *testing.T, bodies map[string][]byte) *Client {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[strings.TrimPrefix(r.URL.Path, "/")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)

			return
		}

		if _, err := w.Write(body); err != nil {
			t.Errorf("write: %v", err)
		}
	}))

	t.Cleanup(srv.Close)

	// The server is http, and ParseSource refuses that by design, so the source
	// is constructed directly. That is the seam a test may use and production
	// code may not: every non-test path goes through ParseSource.
	return &Client{HTTP: srv.Client(), Source: Source{BaseURL: srv.URL}}
}

func TestDownloadPlacesAnAssetThatMatchesItsDigest(t *testing.T) {
	payload := []byte("a root filesystem, in spirit")

	c := serve(t, map[string][]byte{"rootfs.img": payload})

	dir := t.TempDir()

	path, err := c.Download(t.Context(), Asset{
		Name:   "rootfs.img",
		SHA256: digestOf(payload),
		Size:   int64(len(payload)),
	}, dir)
	if err != nil {
		t.Fatalf("a matching asset was refused: %v", err)
	}

	if path != filepath.Join(dir, "rootfs.img") {
		t.Errorf("landed at %q", path)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !bytes.Equal(got, payload) {
		t.Errorf("content = %q", got)
	}
}

// THE LOAD-BEARING TEST. A verifier that runs after the rename has already
// published unverified bytes under a trusted name. This asserts the file never
// appears under its real name at all -- not that an error came back, which a
// verify-after-rename implementation would also satisfy.
func TestDownloadNeverPlacesBytesThatFailTheirDigest(t *testing.T) {
	payload := []byte("substituted content")

	c := serve(t, map[string][]byte{"rootfs.img": payload})

	dir := t.TempDir()

	_, err := c.Download(t.Context(), Asset{
		Name:   "rootfs.img",
		SHA256: digestOf([]byte("what was actually published")),
		Size:   int64(len(payload)),
	}, dir)
	if err == nil {
		t.Fatal("content that does not match the published digest was accepted")
	}

	if _, statErr := os.Stat(filepath.Join(dir, "rootfs.img")); !os.IsNotExist(statErr) {
		t.Fatalf("rootfs.img exists after a failed digest check (stat: %v); unverified bytes "+
			"reached the name a caller would trust", statErr)
	}

	assertDirEmpty(t, dir)
}

// A STAGED FILE THAT OUTLIVES ITS FAILURE is several hundred megabytes nothing
// will collect, on a node that retries weekly.
func assertDirEmpty(t *testing.T, dir string) {
	t.Helper()

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}

	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}

		t.Fatalf("the staging directory still holds %v after a failure", names)
	}
}

func TestDownloadRefusesABodyShorterThanPublished(t *testing.T) {
	payload := []byte("truncated")

	c := serve(t, map[string][]byte{"rootfs.img": payload})

	dir := t.TempDir()

	_, err := c.Download(t.Context(), Asset{
		Name:   "rootfs.img",
		SHA256: digestOf(payload),
		Size:   int64(len(payload)) + 100,
	}, dir)
	if err == nil {
		t.Fatal("a body shorter than the published size was accepted")
	}

	assertDirEmpty(t, dir)
}

// A LONGER BODY MUST NOT BE TRUNCATED TO THE PROMISED LENGTH, which would let a
// digest match a prefix of something larger.
func TestDownloadRefusesABodyLongerThanPublished(t *testing.T) {
	prefix := []byte("the published bytes")
	payload := append(append([]byte{}, prefix...), []byte(" plus a tail nobody signed")...)

	c := serve(t, map[string][]byte{"rootfs.img": payload})

	dir := t.TempDir()

	_, err := c.Download(t.Context(), Asset{
		Name:   "rootfs.img",
		SHA256: digestOf(prefix),
		Size:   int64(len(prefix)),
	}, dir)
	if err == nil {
		t.Fatal("a body longer than published was truncated to the promised length and accepted")
	}

	assertDirEmpty(t, dir)
}

func TestDownloadReportsAMissingAssetDistinctly(t *testing.T) {
	c := serve(t, map[string][]byte{})

	_, err := c.Download(t.Context(), Asset{
		Name:   "rootfs.img",
		SHA256: strings.Repeat("a", 64),
		Size:   10,
	}, t.TempDir())

	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("a missing asset reported %v, which callers cannot separate from a "+
			"network failure", err)
	}
}

func TestDownloadRefusesAnAssetTheManifestCouldNotHaveCarried(t *testing.T) {
	c := serve(t, map[string][]byte{})

	// Download re-validates rather than trusting its caller, because it is
	// exported and the name reaches both a url and the filesystem.
	_, err := c.Download(t.Context(), Asset{
		Name:   "../escape",
		SHA256: strings.Repeat("a", 64),
		Size:   10,
	}, t.TempDir())
	if err == nil {
		t.Fatal("an asset name that is not a plain file was fetched")
	}
}

func TestManifestFetchValidates(t *testing.T) {
	good, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	c := serve(t, map[string][]byte{ManifestName: good})

	m, err := c.Manifest(t.Context(), Policy{})
	if err != nil {
		t.Fatalf("a valid manifest was refused: %v", err)
	}

	if m.RunnerVersion != "2.336.0" {
		t.Errorf("runner version = %q", m.RunnerVersion)
	}
}

func TestManifestFetchRefusesAnOversizeDocument(t *testing.T) {
	c := serve(t, map[string][]byte{ManifestName: make([]byte, MaxManifestBytes+1)})

	if _, err := c.Manifest(t.Context(), Policy{}); err == nil {
		t.Fatal("an oversize manifest was read whole")
	}
}

func TestManifestFetchReportsAbsenceDistinctly(t *testing.T) {
	c := serve(t, map[string][]byte{})

	if _, err := c.Manifest(t.Context(), Policy{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a source with no manifest reported %v", err)
	}
}

func TestFetchHonoursContextCancellation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))

	t.Cleanup(srv.Close)

	c := &Client{HTTP: srv.Client(), Source: Source{BaseURL: srv.URL}}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	if _, err := c.Manifest(ctx, Policy{}); err == nil {
		t.Fatal("a hung source did not surface as an error")
	}
}

func TestResolveDefaultSourceUsesTheDatedGuestReleaseNamedByTheChannel(t *testing.T) {
	now := time.Now().UTC()
	body, err := json.Marshal(map[string]any{
		"schema":            channelSchema,
		"tag":               "guest-20260821-000000",
		"published_at":      now.Add(-time.Hour),
		"expires_at":        now.Add(9 * 24 * time.Hour),
		"release_immutable": true,
	})
	if err != nil {
		t.Fatalf("marshal channel: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/current.json" {
			t.Errorf("channel path = %q", r.URL.Path)
		}
		if _, err := w.Write(body); err != nil {
			t.Errorf("write channel: %v", err)
		}
	}))
	t.Cleanup(srv.Close)

	client := &Client{
		HTTP: srv.Client(),
		Source: Source{
			channelURL:    srv.URL + "/current.json",
			channelPolicy: &Policy{Required: false},
			official:      true,
		},
	}
	if err := client.Resolve(t.Context()); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got, want := client.Source.BaseURL, defaultDownloadPrefix+"guest-20260821-000000"; got != want {
		t.Fatalf("resolved source = %q; want %q", got, want)
	}
	if !client.Source.IsDefault() {
		t.Fatal("resolved first-party source lost its default trust identity")
	}
}

func TestResolveDefaultSourceRefusesInvalidChannels(t *testing.T) {
	now := time.Now().UTC()
	t.Run("malformed json", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if _, err := io.WriteString(w, `{not json`); err != nil {
				t.Errorf("write malformed channel: %v", err)
			}
		}))
		t.Cleanup(srv.Close)

		client := &Client{HTTP: srv.Client(), Source: Source{
			channelURL:    srv.URL,
			channelPolicy: &Policy{Required: false},
			official:      true,
		}}
		if err := client.Resolve(t.Context()); err == nil {
			t.Fatal("malformed channel was accepted")
		}
	})

	for _, fields := range []map[string]any{
		{"schema": channelSchema, "tag": "guest-latest", "published_at": now.Add(-time.Hour), "expires_at": now.Add(time.Hour), "release_immutable": true},
		{"schema": channelSchema, "tag": "v0.3.0", "published_at": now.Add(-time.Hour), "expires_at": now.Add(time.Hour), "release_immutable": true},
		{"schema": channelSchema, "tag": "guest-20260821-000000/manifest.json", "published_at": now.Add(-time.Hour), "expires_at": now.Add(time.Hour), "release_immutable": true},
		{"schema": channelSchema + 1, "tag": "guest-20260821-000000", "published_at": now.Add(-time.Hour), "expires_at": now.Add(time.Hour), "release_immutable": true},
		{"schema": channelSchema, "tag": "guest-20260821-000000", "published_at": now.Add(-time.Hour), "expires_at": now.Add(time.Hour), "release_immutable": false},
		{"schema": channelSchema, "tag": "guest-20260821-000000", "published_at": now.Add(-11 * 24 * time.Hour), "expires_at": now.Add(-time.Hour), "release_immutable": true},
		{"schema": channelSchema, "tag": "guest-20260821-000000", "published_at": now.Add(-time.Hour), "expires_at": now.Add(maxChannelLifetime + time.Hour), "release_immutable": true},
		{"schema": channelSchema, "tag": "guest-20260821-000000", "published_at": now.Add(channelClockSkew + time.Minute), "expires_at": now.Add(24 * time.Hour), "release_immutable": true},
	} {
		body, err := json.Marshal(fields)
		if err != nil {
			t.Fatalf("marshal invalid channel: %v", err)
		}
		t.Run(string(body), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := w.Write(body); err != nil {
					t.Errorf("write channel: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			client := &Client{HTTP: srv.Client(), Source: Source{
				channelURL:    srv.URL,
				channelPolicy: &Policy{Required: false},
				official:      true,
			}}
			if err := client.Resolve(t.Context()); err == nil {
				t.Fatal("invalid channel was accepted")
			}
		})
	}
}

func TestResolveDefaultSourceRequiresAnAuthenticChannel(t *testing.T) {
	now := time.Now().UTC()
	body, err := json.Marshal(map[string]any{
		"schema":            channelSchema,
		"tag":               "guest-20260821-000000",
		"published_at":      now.Add(-time.Hour),
		"expires_at":        now.Add(time.Hour),
		"release_immutable": true,
	})
	if err != nil {
		t.Fatalf("marshal channel: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/current.json" {
			if _, err := w.Write(body); err != nil {
				t.Errorf("write channel: %v", err)
			}

			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	client := &Client{HTTP: srv.Client(), Source: Source{
		channelURL:       srv.URL + "/current.json",
		channelBundleURL: srv.URL + "/current.sigstore.json",
		official:         true,
	}}
	if err := client.Resolve(t.Context()); err == nil || !strings.Contains(err.Error(), "authenticate") {
		t.Fatalf("unsigned channel returned %v", err)
	}
}

func TestResolveDefaultSourceRefetchesAMixedChannelPair(t *testing.T) {
	now := time.Now().UTC()
	channel := func(tag string) []byte {
		body, err := json.Marshal(map[string]any{
			"schema":            channelSchema,
			"tag":               tag,
			"published_at":      now.Add(-time.Hour),
			"expires_at":        now.Add(time.Hour),
			"release_immutable": true,
		})
		if err != nil {
			t.Fatalf("marshal channel: %v", err)
		}

		return body
	}
	oldChannel := channel("guest-20260820-000000")
	newChannel := channel("guest-20260821-000000")
	channelRequests, bundleRequests := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/current.json":
			channelRequests++
			body := newChannel
			if channelRequests == 1 {
				body = oldChannel
			}
			if _, err := w.Write(body); err != nil {
				t.Errorf("write channel: %v", err)
			}
		case "/current.sigstore.json":
			bundleRequests++
			if _, err := w.Write(newChannel); err != nil {
				t.Errorf("write bundle: %v", err)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	client := &Client{
		HTTP: srv.Client(),
		Source: Source{
			channelURL:       srv.URL + "/current.json",
			channelBundleURL: srv.URL + "/current.sigstore.json",
			channelPolicy:    &Policy{Required: true},
			official:         true,
		},
		verifyChannel: func(body, signature []byte, _ Policy) error {
			if !bytes.Equal(body, signature) {
				return errors.New("signature belongs to another channel generation")
			}

			return nil
		},
	}
	if err := client.Resolve(t.Context()); err != nil {
		t.Fatalf("resolve mixed pair: %v", err)
	}
	if got, want := client.Source.BaseURL, defaultDownloadPrefix+"guest-20260821-000000"; got != want {
		t.Fatalf("resolved source = %q; want %q", got, want)
	}
	if channelRequests != channelPairAttempts || bundleRequests != channelPairAttempts {
		t.Fatalf("channel requests = %d and bundle requests = %d; want %d each",
			channelRequests, bundleRequests, channelPairAttempts)
	}
}

func TestResolveDefaultSourceSurfacesChannelFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(srv.Close)

	client := &Client{HTTP: srv.Client(), Source: Source{
		channelURL:    srv.URL,
		channelPolicy: &Policy{Required: false},
		official:      true,
	}}
	if err := client.Resolve(t.Context()); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("channel failure returned %v", err)
	}
}

func TestFetchSurfacesAServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))

	t.Cleanup(srv.Close)

	c := &Client{HTTP: srv.Client(), Source: Source{BaseURL: srv.URL}}

	_, err := c.Manifest(t.Context(), Policy{})
	if err == nil {
		t.Fatal("a 500 was treated as a manifest")
	}

	if errors.Is(err, ErrNotFound) {
		t.Error("a 500 was reported as an absent artifact, which callers treat as normal")
	}
}

// A REQUIRED SIGNATURE IS FETCHED AND ENFORCED BY THE FETCH PATH ITSELF.
//
// This is the wiring that matters: Manifest takes a policy precisely so a caller
// cannot obtain a manifest without having said what would make it trustworthy. A
// source that serves a manifest and no bundle must not produce one.
func TestManifestRefusesWhenTheRequiredSignatureIsNotPublished(t *testing.T) {
	good, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// The manifest is served; the bundle is not.
	c := serve(t, map[string][]byte{ManifestName: good})

	src := DefaultSource()

	policy, err := PolicyFor(src, "", "", false)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	if _, err := c.Manifest(t.Context(), policy); err == nil {
		t.Fatal("a manifest with no published signature was returned under a policy that " +
			"requires one; every digest it names is its own")
	}
}

// AND THE SIGNATURE IS CHECKED OVER THE BYTES THAT ARRIVED. A manifest that parses
// is not the question; a manifest somebody vouched for is.
func TestManifestRefusesAManifestWhoseSignatureDoesNotVerify(t *testing.T) {
	good, err := json.Marshal(validManifest())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	legacy, err := os.ReadFile("testdata/legacy-bundle.json")
	if err != nil {
		t.Skipf("no legacy fixture: %v", err)
	}

	c := serve(t, map[string][]byte{ManifestName: good, BundleName: legacy})

	src := DefaultSource()

	policy, err := PolicyFor(src, "", "", false)
	if err != nil {
		t.Fatalf("policy: %v", err)
	}

	_, err = c.Manifest(t.Context(), policy)
	if err == nil {
		t.Fatal("a manifest whose signature cannot be verified was returned")
	}

	// AND THE MANIFEST WAS NOT PARSED INTO SOMETHING USABLE FIRST. Order matters:
	// verifying after parsing would mean the program had already acted on fields
	// from a document nothing vouched for.
	if !strings.Contains(strings.ToLower(err.Error()), "signature") &&
		!strings.Contains(strings.ToLower(err.Error()), "bundle") {
		t.Errorf("the failure is not about the signature, so something else rejected it "+
			"first: %v", err)
	}
}
