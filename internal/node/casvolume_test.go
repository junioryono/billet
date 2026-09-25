package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/server"
)

// deviceVolumes is a mount manager that gives every device a directory of its
// own and mounts it as a link, so two volumes really are two filesystems.
type deviceVolumes struct {
	mu    sync.Mutex
	root  string
	dirs  map[string]string
	fresh map[string]bool
}

func newDeviceVolumes(t *testing.T) *deviceVolumes {
	return &deviceVolumes{root: t.TempDir(), dirs: map[string]string{}, fresh: map[string]bool{}}
}

func (d *deviceVolumes) dir(device string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	if dir, ok := d.dirs[device]; ok {
		return dir
	}
	dir := filepath.Join(d.root, strings.ReplaceAll(device, "/", "_"))
	_ = os.MkdirAll(dir, 0o700)
	d.dirs[device] = dir

	return dir
}

func (d *deviceVolumes) mount(device, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}

	return os.Symlink(d.dir(device), target)
}

func (d *deviceVolumes) MountNew(_ context.Context, device, target string) error {
	d.mu.Lock()
	d.fresh[device] = true
	d.mu.Unlock()

	return d.mount(device, target)
}

func (d *deviceVolumes) MountWritable(_ context.Context, device, target string) error {
	return d.mount(device, target)
}

func (d *deviceVolumes) MountReadOnly(_ context.Context, device, target string) error {
	return d.mount(device, target)
}

func (d *deviceVolumes) Trim(context.Context, string) error { return nil }

func (d *deviceVolumes) Unmount(_ context.Context, target string) error {
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return err
	}

	return nil
}

func digestOf(body string) string {
	sum := sha256.Sum256([]byte(body))

	return hex.EncodeToString(sum[:])
}

// casService is a cache service with one session whose tier enables the Go
// and Bazel caches.
func casService(
	t *testing.T, trust provider.TrustClass, spec *config.CacheSpec, storage *fakeCacheStore,
) (*CacheService, *deviceVolumes, string, string) {
	t.Helper()

	service, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", t.TempDir(),
		storage, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	volumes := newDeviceVolumes(t)
	service.actionIO = volumes
	instance := provider.InstanceName(scopedLease)
	credentials, err := service.PrepareScoped(instance, CacheSessionScope{
		Trust: trust, LeaseID: scopedLease, Epoch: 1, Cache: spec,
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}

	return service, volumes, credentials.Token, instance
}

func goBazelCache() *config.CacheSpec {
	enabled := true
	spec := config.Tier{Cache: &config.TierCache{
		Go:    &config.GoCache{Enabled: &enabled},
		Bazel: &config.CacheToggle{Enabled: &enabled},
	}}.EffectiveCache()

	return &spec
}

func casRequest(t *testing.T, service *CacheService, token, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), method, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.ServeHTTP(response, req)

	return response
}

// AN OBJECT IS STORED ONLY UNDER THE DIGEST OF ITS OWN CONTENT, and read back
// exactly; an action result is stored as given, for the job's own clone.
func TestTheCASStoresOnlyContentUnderItsOwnDigest(t *testing.T) {
	t.Parallel()

	service, _, token, _ := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	body := "compiled object"
	good := "/v1/cas/go/cas/" + digestOf(body)

	if got := casRequest(t, service, token, http.MethodPut, "/v1/cas/go/cas/"+digestOf("other"), body); got.Code != http.StatusBadRequest {
		t.Fatalf("a mislabelled object was stored: %d %s", got.Code, got.Body.String())
	}
	if got := casRequest(t, service, token, http.MethodGet, good, ""); got.Code != http.StatusNotFound {
		t.Fatalf("a missing object answered %d", got.Code)
	}
	if got := casRequest(t, service, token, http.MethodPut, good, body); got.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", got.Code, got.Body.String())
	}
	if got := casRequest(t, service, token, http.MethodGet, good, ""); got.Code != http.StatusOK ||
		got.Body.String() != body {
		t.Fatalf("GET = %d %q", got.Code, got.Body.String())
	}
	if got := casRequest(t, service, token, http.MethodPut, "/v1/cas/bazel/ac/"+digestOf("action"),
		"an action result"); got.Code != http.StatusOK {
		t.Fatalf("PUT ac = %d %s", got.Code, got.Body.String())
	}
	for _, path := range []string{"/v1/cas/npm/cas/" + digestOf(body), "/v1/cas/go/xx/" + digestOf(body),
		"/v1/cas/go/cas/" + strings.ToUpper(digestOf(body)), "/v1/cas/go/cas/../../x"} {
		if got := casRequest(t, service, token, http.MethodGet, path, ""); got.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, got.Code)
		}
	}
}

// A CACHE THE TIER DOES NOT ENABLE ATTACHES NOTHING.
func TestADisabledCASKindIsRefused(t *testing.T) {
	t.Parallel()

	legacy := config.Tier{}.EffectiveCache()
	storage := &fakeCacheStore{}
	service, _, token, _ := casService(t, provider.TrustTrusted, &legacy, storage)
	if got := casRequest(t, service, token, http.MethodGet, "/v1/cas/go/cas/"+digestOf("x"), ""); got.Code != http.StatusForbidden {
		t.Fatalf("a disabled Go cache answered %d", got.Code)
	}
	if len(storage.keys) != 0 {
		t.Errorf("a disabled cache touched storage: %v", storage.keys)
	}
}

// A TRUSTED JOB'S WRITES PUBLISH AS THEY ARE WHEN NOTHING MOVED, and, when
// another job published meanwhile, are merged into the newest generation, so
// both jobs' objects survive.
func TestACASVolumePublishesAndMergesIntoTheNewestGeneration(t *testing.T) {
	t.Parallel()

	for name, moved := range map[string]bool{"nothing moved": false, "another job published": true} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			storage := &fakeCacheStore{}
			service, volumes, token, instance := casService(t, provider.TrustTrusted,
				goBazelCache(), storage)
			ours := "our object"
			if got := casRequest(t, service, token, http.MethodPut, "/v1/cas/go/cas/"+digestOf(ours), ours); got.Code != http.StatusOK {
				t.Fatalf("PUT = %d %s", got.Code, got.Body.String())
			}
			if moved {
				storage.current = "theirs"
				theirs := volumes.dir("/dev/rbd8")
				dir := filepath.Join(theirs, "cas", digestOf("their object")[:2])
				if err := os.MkdirAll(dir, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, digestOf("their object")),
					[]byte("their object"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if err := service.SettleCompleted(t.Context(), instance, true, server.CacheAuthority{}); err != nil {
				t.Fatalf("SettleCompleted: %v", err)
			}
			if err := service.Close(t.Context(), instance); err != nil {
				t.Fatalf("Close: %v", err)
			}
			if err := service.RetryClosed(t.Context()); err != nil {
				t.Fatalf("RetryClosed: %v", err)
			}

			if storage.published != 1 || storage.snapshots != 1 {
				t.Fatalf("snapshots/publications = %d/%d, want 1/1", storage.snapshots, storage.published)
			}
			if !moved {
				return
			}
			merged := volumes.dir("/dev/rbd8")
			for _, body := range []string{ours, "their object"} {
				got, err := os.ReadFile(filepath.Join(merged, "cas", digestOf(body)[:2], digestOf(body)))
				if err != nil || !bytes.Equal(got, []byte(body)) {
					t.Errorf("the merged generation lacks %q: %v", body, err)
				}
			}
			if storage.discarded == 0 {
				t.Error("the job's own clone was not discarded after the merge")
			}
		})
	}
}

// AN UNAUTHORISED DEFAULT-BRANCH JOB'S WRITES ARE DISCARDED.
func TestAnUnauthorisedJobsCASWritesAreDiscarded(t *testing.T) {
	t.Parallel()

	spec := goBazelCache()
	spec.Publish, spec.Owner, spec.Repository = config.CachePublishDefaultBranch, "acme", "api"
	storage := &fakeCacheStore{}
	service, _, token, instance := casService(t, provider.TrustUntrusted, spec, storage)
	body := "pull request object"
	if got := casRequest(t, service, token, http.MethodPut, "/v1/cas/go/cas/"+digestOf(body), body); got.Code != http.StatusOK {
		t.Fatalf("PUT = %d", got.Code)
	}
	pr := publishingAuthority()
	pr.PublishDefault = false
	if err := service.SettleCompleted(t.Context(), instance, true, pr); err != nil {
		t.Fatalf("SettleCompleted: %v", err)
	}
	if err := service.Close(t.Context(), instance); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = service.RetryClosed(t.Context())

	if storage.published != 0 || storage.discarded != 1 {
		t.Fatalf("published %d, discarded %d; want the clone discarded", storage.published,
			storage.discarded)
	}
}
