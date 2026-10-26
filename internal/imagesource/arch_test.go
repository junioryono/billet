package imagesource

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// manifestOf is a valid manifest for machine, as its JSON.
func manifestOf(t *testing.T, machine string) []byte {
	t.Helper()

	m := validManifest()
	m.Arch = machine

	body, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	return body
}

// requests is the paths a source was asked for, in order.
type requests struct {
	mu    sync.Mutex
	paths []string
}

func (r *requests) record(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.paths = append(r.paths, path)
}

func (r *requests) all() []string {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([]string(nil), r.paths...)
}

// serveMachine is serve, recording what was asked for, with the source marked
// as billet's own or an operator's.
func serveMachine(t *testing.T, bodies map[string][]byte, official bool, machine string) (*Client, *requests) {
	t.Helper()

	asked := &requests{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.record(r.URL.Path)

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

	return &Client{HTTP: srv.Client(), Source: Source{BaseURL: srv.URL, official: official},
		Machine: machine}, asked
}

func TestEachMachineHasItsOwnManifestAndX86KeepsTheOldNames(t *testing.T) {
	t.Parallel()

	for machine, want := range map[string][2]string{
		"":        {ManifestName, BundleName},
		"x86_64":  {ManifestName, BundleName},
		"aarch64": {"manifest-aarch64.json", "manifest-aarch64.sigstore.json"},
	} {
		manifest, bundle := ManifestNamesFor(machine)
		if manifest != want[0] || bundle != want[1] {
			t.Errorf("ManifestNamesFor(%q) = %s, %s, want %s, %s", machine, manifest, bundle, want[0], want[1])
		}
	}
}

// AN AARCH64 READER READS THE MANIFEST NAMED FOR IT, and an x86_64 one reads
// manifest.json exactly as every reader before the split did.
func TestAReaderFetchesTheManifestNamedForItsMachine(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ machine, path string }{
		{"aarch64", "/manifest-aarch64.json"},
		{"x86_64", "/manifest.json"},
	} {
		c, asked := serveMachine(t, map[string][]byte{
			"manifest-aarch64.json": manifestOf(t, "aarch64"),
			ManifestName:            manifestOf(t, "x86_64"),
		}, true, tc.machine)

		m, err := c.Manifest(t.Context(), Policy{})
		if err != nil || m.Arch != tc.machine {
			t.Fatalf("%s: Manifest = %+v, %v, want its own", tc.machine, m, err)
		}

		if got := asked.all(); len(got) != 1 || got[0] != tc.path {
			t.Errorf("%s asked for %v, want only %s", tc.machine, got, tc.path)
		}
	}
}

// A CHANNEL THAT LISTS ITS RELEASE'S ARCHITECTURES AND LEAVES THIS ONE OUT IS FINAL,
// and nothing is fetched for it.
func TestAChannelListingNoImageForThisMachineIsFinal(t *testing.T) {
	t.Parallel()

	c, asked := serveMachine(t, map[string][]byte{ManifestName: manifestOf(t, "x86_64")}, true, "aarch64")
	c.channelArchitectures, c.channelSaid = []string{"x86_64"}, true

	if _, err := c.Manifest(t.Context(), Policy{}); !errors.Is(err, ErrNoImageForMachine) {
		t.Fatalf("Manifest = %v, want ErrNoImageForMachine", err)
	}

	if got := asked.all(); len(got) != 0 {
		t.Errorf("a release the channel says carries no image for this machine was asked for %v", got)
	}

	// AND ONE THAT LISTS IT IS READ.
	c, _ = serveMachine(t, map[string][]byte{"manifest-aarch64.json": manifestOf(t, "aarch64")}, true, "aarch64")
	c.channelArchitectures, c.channelSaid = []string{"x86_64", "aarch64"}, true

	if _, err := c.Manifest(t.Context(), Policy{}); err != nil {
		t.Fatalf("a release the channel lists for this machine was refused: %v", err)
	}
}

// BILLET'S OWN RELEASE THAT SAYS NOTHING AND PUBLISHES NO MANIFEST FOR THIS MACHINE
// IS COULD-NOT-TELL, never absence: a 404 can be temporary or cached.
func TestAnUnlistedMissingManifestOfBilletsOwnIsCouldNotTell(t *testing.T) {
	t.Parallel()

	c, asked := serveMachine(t, map[string][]byte{ManifestName: manifestOf(t, "x86_64")}, true, "aarch64")

	_, err := c.Manifest(t.Context(), Policy{})
	if err == nil || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNoImageForMachine) ||
		!strings.Contains(err.Error(), "could not tell") {
		t.Fatalf("Manifest = %v, want could-not-tell, neither absence nor a final no", err)
	}

	if got := asked.all(); len(got) != 1 || got[0] != "/manifest-aarch64.json" {
		t.Errorf("billet's own release was asked for %v, want only its aarch64 manifest", got)
	}
}

// AN OPERATOR'S OWN SOURCE WITH NO MANIFEST FOR THIS MACHINE IS READ FROM
// manifest.json, which Usable then judges: a mirror publishing a single arm64
// manifest.json keeps working.
func TestAnOperatorsSourceFallsBackToTheManifestItPublishes(t *testing.T) {
	t.Parallel()

	c, asked := serveMachine(t, map[string][]byte{ManifestName: manifestOf(t, "aarch64")}, false, "aarch64")

	m, err := c.Manifest(t.Context(), Policy{})
	if err != nil || m.Arch != "aarch64" {
		t.Fatalf("Manifest = %+v, %v, want the mirror's manifest.json", m, err)
	}

	if got := asked.all(); len(got) != 2 || got[0] != "/manifest-aarch64.json" || got[1] != "/manifest.json" {
		t.Errorf("the mirror was asked for %v, want its aarch64 manifest and then manifest.json", got)
	}
}

// AND THE SIGNATURE THAT IS FETCHED IS THE ONE BESIDE THE MANIFEST THAT WAS READ.
func TestTheBundleFetchedIsTheOneBesideTheManifestRead(t *testing.T) {
	t.Parallel()

	c, asked := serveMachine(t, map[string][]byte{"manifest-aarch64.json": manifestOf(t, "aarch64")}, true, "aarch64")

	if _, err := c.Manifest(t.Context(), Policy{Required: true, Identity: "x", Issuer: "y"}); err == nil {
		t.Fatal("a manifest whose required signature is absent was accepted")
	}

	if got := asked.all(); len(got) != 2 || got[1] != "/manifest-aarch64.sigstore.json" {
		t.Errorf("asked for %v, want the aarch64 manifest and then its own bundle", got)
	}
}

// THE CHANNEL'S ARCHITECTURES ARE READ WHEN IT HAS THEM, and a pointer published
// before the member existed says nothing; a list this reader cannot take at its
// word refuses the channel rather than telling every node there is nothing.
func TestResolveReadsTheChannelsArchitectures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		extra   map[string]any
		said    bool
		list    []string
		refused bool
	}{
		{name: "listed", extra: map[string]any{"architectures": []string{"x86_64", "aarch64"}}, said: true,
			list: []string{"x86_64", "aarch64"}},
		{name: "x86_64 alone", extra: map[string]any{"architectures": []string{"x86_64"}}, said: true,
			list: []string{"x86_64"}},
		{name: "unsaid", extra: map[string]any{}},
		{name: "an empty list", extra: map[string]any{"architectures": []string{}}, refused: true},
		{name: "billet's spelling", extra: map[string]any{"architectures": []string{"x64", "arm64"}}, refused: true},
		{name: "manifest names", extra: map[string]any{"architectures": []string{"manifest.json"}}, refused: true},
		{name: "no x86_64", extra: map[string]any{"architectures": []string{"aarch64"}}, refused: true},
		{name: "x86_64 and a misspelling", extra: map[string]any{"architectures": []string{"x86_64", "arm64"}}, refused: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			now := time.Now().UTC()
			pointer := map[string]any{
				"schema": channelSchema, "tag": "guest-20260821-000000",
				"published_at": now.Add(-time.Hour), "expires_at": now.Add(9 * 24 * time.Hour),
				"release_immutable": true,
			}
			for k, v := range tc.extra {
				pointer[k] = v
			}

			body, err := json.Marshal(pointer)
			if err != nil {
				t.Fatalf("marshal channel: %v", err)
			}

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if _, err := w.Write(body); err != nil {
					t.Errorf("write channel: %v", err)
				}
			}))
			t.Cleanup(srv.Close)

			c := &Client{HTTP: srv.Client(), Source: Source{channelURL: srv.URL + "/current.json",
				channelPolicy: &Policy{}, official: true}}

			err = c.Resolve(t.Context())
			if tc.refused {
				if err == nil || !strings.Contains(err.Error(), "lists architectures") {
					t.Fatalf("Resolve = %v, want the channel refused for its list", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}

			if c.channelSaid != tc.said || len(c.channelArchitectures) != len(tc.list) {
				t.Fatalf("read said=%v %v, want said=%v %v", c.channelSaid, c.channelArchitectures, tc.said, tc.list)
			}

			for i := range tc.list {
				if c.channelArchitectures[i] != tc.list[i] {
					t.Fatalf("read %v, want %v", c.channelArchitectures, tc.list)
				}
			}
		})
	}
}

// AND SO IS ITS X86_64 MANIFEST, OR A BUNDLE, NOT FOUND: billet's own release is
// immutable and signed, so a 404 from it is never what it publishes.
func TestAnyNotFoundFromBilletsOwnReleaseIsCouldNotTell(t *testing.T) {
	t.Parallel()

	x64, _ := serveMachine(t, map[string][]byte{}, true, "x86_64")
	if _, err := x64.Manifest(t.Context(), Policy{}); err == nil || errors.Is(err, ErrNotFound) ||
		!strings.Contains(err.Error(), "could not tell") {
		t.Errorf("an x86_64 manifest not found on billet's release = %v, want could-not-tell", err)
	}

	unsigned, _ := serveMachine(t, map[string][]byte{"manifest-aarch64.json": manifestOf(t, "aarch64")}, true, "aarch64")
	if _, err := unsigned.Manifest(t.Context(), Policy{Required: true, Identity: "x", Issuer: "y"}); err == nil ||
		errors.Is(err, ErrNotFound) || !strings.Contains(err.Error(), "could not tell") {
		t.Errorf("a bundle not found on billet's release = %v, want could-not-tell", err)
	}

	// WHILE AN OPERATOR'S SOURCE THAT PUBLISHES NOTHING SAYS SO.
	mirror, _ := serveMachine(t, map[string][]byte{}, false, "x86_64")
	if _, err := mirror.Manifest(t.Context(), Policy{}); !errors.Is(err, ErrNotFound) {
		t.Errorf("an operator's source publishing nothing = %v, want ErrNotFound", err)
	}
}

// AN OPERATOR'S SOURCE THAT REFUSES A KEY IT DOES NOT HOLD, as S3 does to a reader
// that may not list the bucket, falls back too; one that fails otherwise does not,
// and a fallback that also fails names both attempts.
func TestAnOperatorsSourceFallsBackOnAForbiddenAndNothingElse(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		status   int
		fallback bool
	}{
		{"403", http.StatusForbidden, true},
		{"500", http.StatusInternalServerError, false},
	} {
		asked := &requests{}
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked.record(r.URL.Path)

			if r.URL.Path == "/manifest.json" {
				if _, err := w.Write(manifestOf(t, "aarch64")); err != nil {
					t.Errorf("write: %v", err)
				}

				return
			}

			w.WriteHeader(tc.status)
		}))
		t.Cleanup(srv.Close)

		c := &Client{HTTP: srv.Client(), Source: Source{BaseURL: srv.URL}, Machine: "aarch64"}

		_, err := c.Manifest(t.Context(), Policy{})
		if tc.fallback && err != nil {
			t.Errorf("%s: a mirror refusing the aarch64 manifest did not fall back: %v", tc.name, err)
		}

		if !tc.fallback && (err == nil || len(asked.all()) != 1) {
			t.Errorf("%s: Manifest = %v after asking %v, want the failure and no fallback", tc.name, err, asked.all())
		}
	}

	c, _ := serveMachine(t, map[string][]byte{}, false, "aarch64")
	if _, err := c.Manifest(t.Context(), Policy{}); err == nil ||
		!strings.Contains(err.Error(), "manifest.json") || !strings.Contains(err.Error(), "manifest-aarch64.json") {
		t.Errorf("a fallback that also failed = %v, want both attempts named", err)
	}
}

// AND THE BUNDLE A FALLBACK FETCHES IS manifest.json'S.
func TestAFallbackFetchesTheBundleBesideManifestJSON(t *testing.T) {
	t.Parallel()

	c, asked := serveMachine(t, map[string][]byte{ManifestName: manifestOf(t, "aarch64")}, false, "aarch64")

	if _, err := c.Manifest(t.Context(), Policy{Required: true, Identity: "x", Issuer: "y"}); err == nil {
		t.Fatal("a manifest whose required signature is absent was accepted")
	}

	if got := asked.all(); len(got) != 3 || got[2] != "/manifest.sigstore.json" {
		t.Errorf("asked for %v, want the aarch64 manifest, manifest.json, and then its bundle", got)
	}
}

// A CLIENT NAMING NO MACHINE IS AN X86_64 ONE, which a channel listing x86_64 serves.
func TestAClientNamingNoMachineIsX86(t *testing.T) {
	t.Parallel()

	c, asked := serveMachine(t, map[string][]byte{ManifestName: manifestOf(t, "x86_64")}, true, "")
	c.channelArchitectures, c.channelSaid = []string{"x86_64"}, true

	if _, err := c.Manifest(t.Context(), Policy{}); err != nil {
		t.Fatalf("an x86_64 client was refused by a channel listing x86_64: %v", err)
	}

	if got := asked.all(); len(got) != 1 || got[0] != "/manifest.json" {
		t.Errorf("asked for %v, want manifest.json", got)
	}
}
