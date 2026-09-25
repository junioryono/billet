package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node/reapi"
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
	// targets records every mount point, and failSuffix fails a mount whose
	// target ends with it.
	targets    []string
	failSuffix string
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
	d.mu.Lock()
	d.targets = append(d.targets, target)
	fail := d.failSuffix != "" && strings.HasSuffix(target, d.failSuffix)
	d.mu.Unlock()
	if fail {
		return errors.New("the mount failed")
	}
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

			if storage.published != 1 || storage.snapshots != 1 || storage.current != "next" {
				t.Fatalf("snapshots/publications = %d/%d and current %q, want 1/1 and the candidate",
					storage.snapshots, storage.published, storage.current)
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

// A gRPC CALL REACHES THE REMOTE EXECUTION API ONLY AUTHENTICATED, carrying its
// session to the bazel volume; a tier without the bazel cache is refused there.
func TestAGRPCCallReachesTheBazelVolumeOfItsOwnSession(t *testing.T) {
	t.Parallel()

	legacy := config.Tier{}.EffectiveCache()
	for name, tc := range map[string]struct {
		spec    *config.CacheSpec
		bearer  bool
		reached bool
		off     bool
	}{
		"the bazel cache":      {spec: goBazelCache(), bearer: true, reached: true},
		"no bearer":            {spec: goBazelCache()},
		"a tier without bazel": {spec: &legacy, bearer: true, reached: true, off: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			service, _, token, _ := casService(t, provider.TrustTrusted, tc.spec, &fakeCacheStore{})
			var reached bool
			var opened error
			service.remoteAPI = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				volume, err := service.openRemoteAPIVolume(r.Context())
				opened = err
				if err != nil {
					return
				}
				defer volume.Close()
				body := "a bazel blob"
				if err := volume.Put(r.Context(), reapi.TableCAS, digestOf(body), strings.NewReader(body)); err != nil {
					opened = err

					return
				}
				file, err := volume.Open(reapi.TableCAS, digestOf(body))
				if err != nil {
					opened = err

					return
				}
				_ = file.Close()
			})

			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost,
				"/build.bazel.remote.execution.v2.ContentAddressableStorage/FindMissingBlobs", nil)
			req.ProtoMajor, req.ProtoMinor = 2, 0
			req.Header.Set("Content-Type", "application/grpc")
			if tc.bearer {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			response := httptest.NewRecorder()
			service.ServeHTTP(response, req)

			if reached != tc.reached {
				t.Fatalf("the remote API was reached = %v, want %v (status %d)", reached, tc.reached,
					response.Code)
			}
			if !tc.reached {
				if response.Code != http.StatusUnauthorized {
					t.Errorf("an unauthenticated call answered %d", response.Code)
				}

				return
			}
			if tc.off != errors.Is(opened, reapi.ErrOff) || (!tc.off && opened != nil) {
				t.Errorf("opening the volume = %v, want off=%v", opened, tc.off)
			}
		})
	}
}

// WHAT EACH BUILD CACHE DID IS REPORTED WHEN THE SESSION ENDS: warm for one
// that served something it already held, cold for one used and found empty,
// unused for one the tier enabled and the guest never asked for, and nothing
// for one the tier does not enable.
func TestTheBuildCachesOutcomesAreReportedWhenTheSessionEnds(t *testing.T) {
	t.Parallel()

	enabled, disabled := true, false
	spec := config.Tier{Cache: &config.TierCache{
		Go:          &config.GoCache{Enabled: &enabled},
		Bazel:       &config.CacheToggle{Enabled: &enabled},
		Git:         &config.CacheToggle{Enabled: &enabled},
		StickyDisks: &config.CacheToggle{Enabled: &disabled},
	}}.EffectiveCache()
	service, _, token, instance := casService(t, provider.TrustTrusted, &spec, &fakeCacheStore{})
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)

	if code := putObject(t, service, token, "go object"); code != http.StatusOK {
		t.Fatalf("PUT go = %d", code)
	}
	if code := casRequest(t, service, token, http.MethodGet, "/v1/cas/go/cas/"+digestOf("go object"), "").Code; code != http.StatusOK {
		t.Fatalf("GET go = %d", code)
	}
	if code := casRequest(t, service, token, http.MethodGet, "/v1/cas/bazel/cas/"+digestOf("absent"), "").Code; code != http.StatusNotFound {
		t.Fatalf("GET bazel = %d", code)
	}
	if err := service.Close(t.Context(), instance); err != nil {
		t.Fatalf("Close: %v", err)
	}

	calls := observer.recorded()
	if len(calls) == 0 {
		t.Fatal("nothing was reported")
	}
	got := calls[len(calls)-1].obs.BuildCaches
	want := alloc.BuildCaches{Go: alloc.BuildCacheWarm, Bazel: alloc.BuildCacheCold,
		Git: alloc.BuildCacheUnused}
	if got != want {
		t.Fatalf("build caches reported %+v, want %+v", got, want)
	}
}

// A STICKY DISK IS REPORTED COLD when its first attach found no generation, and
// WARM when it cloned one.
func TestAStickyDiskIsReportedColdOrWarm(t *testing.T) {
	t.Parallel()

	for want, current := range map[alloc.BuildCache]string{
		alloc.BuildCacheCold: "", alloc.BuildCacheWarm: "baseline",
	} {
		legacy := config.Tier{}.EffectiveCache()
		service, _, token, instance := casService(t, provider.TrustTrusted, &legacy,
			&fakeCacheStore{current: current})
		observer := &recordingObserver{}
		service.SetCacheObserver(observer)
		if got := cacheRequest(t, service, token, "/v1/volumes",
			map[string]any{"key": "npm", "size_bytes": int64(1 << 30)}); got.Code != http.StatusCreated {
			t.Fatalf("attach = %d %s", got.Code, got.Body.String())
		}
		if err := service.Close(t.Context(), instance); err != nil {
			t.Fatalf("Close: %v", err)
		}
		calls := observer.recorded()
		if got := calls[len(calls)-1].obs.Sticky; got != want {
			t.Errorf("a sticky disk over generation %q was reported %q, want %q", current, got, want)
		}
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

// ledgerPolicy answers the way the ledger does: it refuses a lookup that names
// no repository, and blocks while blocked is set.
type ledgerPolicy struct{ blocked atomic.Bool }

func (l *ledgerPolicy) CacheAllowed(_ context.Context, _ config.CacheKind, owner, repository string) (bool, error) {
	if owner == "" || repository == "" {
		return false, errors.New("cache policy lookup needs an owner and repository")
	}

	return !l.blocked.Load(), nil
}

func putObject(t *testing.T, service *CacheService, token, body string) int {
	t.Helper()

	return casRequest(t, service, token, http.MethodPut, "/v1/cas/go/cas/"+digestOf(body), body).Code
}

func finish(t *testing.T, service *CacheService, instance string, authority server.CacheAuthority) {
	t.Helper()

	if err := service.SettleCompleted(t.Context(), instance, true, authority); err != nil {
		t.Fatalf("SettleCompleted: %v", err)
	}
	if err := service.Close(t.Context(), instance); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// THE BEARER NEVER NAMES A MOUNT POINT, which every process on the host can
// read from the mount table; a session an older binary recorded keeps the
// token it mounted under, so its mounts are still found.
func TestNoMountPointNamesTheBearer(t *testing.T) {
	t.Parallel()

	service, volumes, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), &fakeCacheStore{})
	if code := putObject(t, service, token, "an object"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if len(volumes.targets) == 0 {
		t.Fatal("nothing was mounted")
	}
	for _, target := range volumes.targets {
		if strings.Contains(target, token) {
			t.Fatalf("mount point %s names the bearer", target)
		}
	}

	session := service.sessionOf(instance)
	record := filepath.Join(service.stateDir, token+".json")
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(raw), `"path_id":"`+session.pathID+`",`, "", 1)
	if legacy == string(raw) {
		t.Fatal("the session record carries no path_id to remove")
	}
	if err := os.WriteFile(record, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := NewCacheService("http://172.20.0.1:7718", "test-deployment", service.rootState,
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	if got := reloaded.sessionOf(instance).pathID; got != token {
		t.Fatalf("a record without a path id mounts under %q, want its token", got)
	}
}

// A WRITE FINISHING WHILE THE CACHE LOOP RELEASES THE VOLUME NEVER DEADLOCKS:
// the transfer does not wait for the session lock the loop holds, and the loop
// gives up on a volume still in use and comes back for it.
func TestAWriteDuringReleaseDoesNotDeadlock(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, _, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	if code := putObject(t, service, token, "first"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	session := service.sessionOf(instance)
	handle, err := service.openCASHandle(t.Context(), session, config.CacheGo)
	if err != nil {
		t.Fatalf("openCASHandle: %v", err)
	}
	finish(t, service, instance, server.CacheAuthority{})

	// THE LOOP HOLDS THE SESSION LOCK WHILE IT WAITS FOR THE VOLUME, so a
	// transfer holding the volume must finish without it.
	session.mu.Lock()
	stored := make(chan error, 1)
	go func() {
		stored <- handle.Put(t.Context(), reapi.TableCAS, digestOf("second"), strings.NewReader("second"))
	}()
	select {
	case err := <-stored:
		session.mu.Unlock()
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
	case <-time.After(5 * time.Second):
		session.mu.Unlock()
		t.Fatal("a transfer waited for the session lock while holding the volume")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if err := service.RetryClosed(ctx); err == nil {
		t.Fatal("the loop released a volume a transfer still holds")
	}
	handle.Close()

	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	if storage.published != 1 {
		t.Fatalf("published %d, want the volume released and published once", storage.published)
	}
}

// DISABLING A CACHE STOPS A RUNNING JOB'S USE OF IT, not only the next job's.
func TestTheKillSwitchStopsACacheAlreadyInUse(t *testing.T) {
	t.Parallel()

	spec := goBazelCache()
	spec.Owner, spec.Repository = "acme", "api"
	service, _, token, _ := casService(t, provider.TrustTrusted, spec, &fakeCacheStore{})
	policy := &ledgerPolicy{}
	service.SetCachePolicy(policy)
	now := time.Now()
	service.now = func() time.Time { return now }

	if code := putObject(t, service, token, "allowed"); code != http.StatusOK {
		t.Fatalf("PUT before the block = %d", code)
	}
	policy.blocked.Store(true)
	if code := putObject(t, service, token, "within the age"); code != http.StatusOK {
		t.Fatalf("PUT within %s of the last answer = %d", casPolicyAge, code)
	}
	now = now.Add(casPolicyAge)
	if code := putObject(t, service, token, "after the block"); code != http.StatusForbidden {
		t.Fatalf("PUT after the block = %d, want 403", code)
	}
}

// A BLOCK SET WHILE A PUBLICATION IS UNDER WAY STOPS IT before the pointer
// moves: the kill switch is asked again after the snapshot, not only before.
func TestABlockDuringPublicationStopsIt(t *testing.T) {
	t.Parallel()

	spec := goBazelCache()
	spec.Owner, spec.Repository = "acme", "api"
	policy := &ledgerPolicy{}
	storage := &fakeCacheStore{snapshotHook: func() { policy.blocked.Store(true) }}
	service, _, token, instance := casService(t, provider.TrustTrusted, spec, storage)
	service.SetCachePolicy(policy)
	if code := putObject(t, service, token, "an object"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, instance, server.CacheAuthority{})
	_ = service.RetryClosed(t.Context())

	if storage.snapshots != 1 || storage.published != 0 {
		t.Fatalf("snapshots/publications = %d/%d, want the snapshot taken and nothing published",
			storage.snapshots, storage.published)
	}
}

// A TRUSTED-ONLY TIER WITH NO CACHE SCOPE PUBLISHES under the ledger's policy,
// which cannot answer for a session that names no repository.
func TestAnUnscopedTrustedCachePublishesUnderTheLedgersPolicy(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, _, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	service.SetCachePolicy(&ledgerPolicy{})
	if code := putObject(t, service, token, "an object"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	finish(t, service, instance, server.CacheAuthority{})
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	if storage.current != "next" {
		t.Fatalf("current = %q, want the job's objects published", storage.current)
	}
}

// A DISCARD THAT FAILS KEEPS THE VOLUME'S RECORD, so the next pass discards it
// rather than leave it mapped and forgotten.
func TestAFailedDiscardIsRetried(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{discardFailures: 1}
	service, _, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	if code := putObject(t, service, token, "unpublished"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if err := service.Close(t.Context(), instance); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = service.RetryClosed(t.Context())
	session := service.sessionOf(instance)
	if session == nil || len(session.hosts) != 1 {
		t.Fatal("a failed discard dropped the volume's record")
	}
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	if storage.discarded != 2 {
		t.Fatalf("discard attempts = %d, want a retry after the failure", storage.discarded)
	}
}

// A MERGE THAT FAILS DISCARDS ITS CLONE OF THE NEWEST GENERATION as well as the
// job's own, because the clone was recorded before anything used it.
func TestAFailedMergeDiscardsItsClone(t *testing.T) {
	t.Parallel()

	storage := &fakeCacheStore{}
	service, volumes, token, instance := casService(t, provider.TrustTrusted, goBazelCache(), storage)
	if code := putObject(t, service, token, "ours"); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	storage.current = "theirs"
	volumes.failSuffix = ".merge"
	finish(t, service, instance, server.CacheAuthority{})
	if err := service.RetryClosed(t.Context()); err != nil {
		t.Fatalf("RetryClosed: %v", err)
	}
	if storage.published != 0 || storage.current != "theirs" {
		t.Fatalf("a failed merge published: %d, current %q", storage.published, storage.current)
	}
	if storage.discarded != 2 {
		t.Fatalf("discards = %d, want the merge clone and the job's own", storage.discarded)
	}
}
