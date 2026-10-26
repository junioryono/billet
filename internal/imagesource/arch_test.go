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
// before the member existed says nothing, which is not an empty list.
func TestResolveReadsTheChannelsArchitectures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		extra map[string]any
		said  bool
		list  []string
	}{
		{name: "listed", extra: map[string]any{"architectures": []string{"x86_64", "aarch64"}}, said: true,
			list: []string{"x86_64", "aarch64"}},
		{name: "an empty list", extra: map[string]any{"architectures": []string{}}, said: true, list: []string{}},
		{name: "unsaid", extra: map[string]any{}},
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
			if err := c.Resolve(t.Context()); err != nil {
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
