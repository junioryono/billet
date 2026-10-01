package node

import (
	"context"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

const heldVolume = "billet-cache/cache-v-1790000000-0123456789abcdef01234567"

// namedVolumeStore hands out a volume under a name the ceph store would give.
type namedVolumeStore struct{ *fakeCacheStore }

func (s namedVolumeStore) Create(ctx context.Context, key string, size int64) (storecontract.Volume, error) {
	volume, err := s.fakeCacheStore.Create(ctx, key, size)
	volume.Handle = heldVolume

	return volume, err
}

// THE RECORDS ANOTHER PROCESS READS ARE THE ONES THE NODE WROTE. A volume a live
// session attached is found in what the service persisted, and a name no
// session holds is not.
func TestCacheSessionRecordsNameTheVolumesTheNodeHolds(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	service, err := NewCacheService("http://cache.billet.test:7718", "test-deployment", dir,
		namedVolumeStore{&fakeCacheStore{}}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	credentials, err := service.Prepare("billet-one", provider.TrustTrusted)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	attached := cacheRequest(t, service, credentials.Token, "/v1/volumes", map[string]any{
		"key": "acme/api/npm", "size_bytes": int64(10 << 30),
	})
	if attached.Code != http.StatusCreated {
		t.Fatalf("attach status = %d: %s", attached.Code, attached.Body.String())
	}

	records, err := ReadCacheSessionRecords(dir)
	if err != nil {
		t.Fatalf("ReadCacheSessionRecords: %v", err)
	}

	if !records.Mentions("cache-v-1790000000-0123456789abcdef01234567") {
		t.Error("the volume a live session holds is not named by its records")
	}

	if records.Mentions("cache-v-1790000000-fedcba9876543210fedcba98") {
		t.Error("a volume no session holds is named by the records")
	}
}

// A RECORD THAT CANNOT BE READ IS NOT AN EMPTY ONE. A link where a record should
// be refuses the read, and a directory that is not there says so rather than
// answering that no session holds anything.
func TestCacheSessionRecordsRefuseWhatTheyCannotRead(t *testing.T) {
	t.Parallel()

	missing := t.TempDir()
	if _, err := ReadCacheSessionRecords(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a missing custody directory read as %v, want fs.ErrNotExist", err)
	}

	dir := t.TempDir()
	sessions := filepath.Join(dir, cacheSessionDirectory)
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}

	target := filepath.Join(dir, "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"volume":"`+heldVolume+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(sessions, "linked.json")); err != nil {
		t.Fatal(err)
	}

	if _, err := ReadCacheSessionRecords(dir); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("a linked record read as %v, want a refusal", err)
	}
}
