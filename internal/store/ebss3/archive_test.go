package ebss3

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	storecontract "github.com/junioryono/billet/internal/store"
)

// fakeBulk is S3 as far as whole objects go, sharing fakeObjects' deletes through
// the store's objectAPI being this same value in these tests.
type fakeBulk struct {
	mu      sync.Mutex
	now     *time.Time
	objects map[string]fakeBulkObject
	uploads map[string]*fakeBulkUpload
	nextID  int
	// loseCompletion lands the next completion and answers as if it did not.
	loseCompletion bool
	// failCopy fails every part copy.
	failCopy error
	// failRange fails the next ranged read, once.
	failRange error
	// partSizes records every part size a completion assembled.
	partSizes []int
}

type fakeBulkObject struct {
	body     []byte
	modified time.Time
	upload   string
}

type fakeBulkUpload struct {
	key       string
	upload    string
	parts     map[int][]byte
	initiated time.Time
}

func newFakeBulk(now *time.Time) *fakeBulk {
	return &fakeBulk{
		now: now, objects: map[string]fakeBulkObject{}, uploads: map[string]*fakeBulkUpload{},
	}
}

func (f *fakeBulk) PutStream(
	_ context.Context, key string, body io.Reader, size int64, ifAbsent bool,
) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("body of %d bytes declared as %d", len(data), size)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.objects[key]; exists && ifAbsent {
		return errObjectConflict
	}
	f.objects[key] = fakeBulkObject{body: data, modified: *f.now}

	return nil
}

func (f *fakeBulk) GetRange(
	_ context.Context, key string, start, length int64,
) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failRange != nil {
		err := f.failRange
		f.failRange = nil

		return nil, err
	}
	object, ok := f.objects[key]
	if !ok {
		return nil, errObjectAbsent
	}

	return io.NopCloser(bytes.NewReader(object.body[start : start+length])), nil
}

func (f *fakeBulk) GetAll(_ context.Context, key string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[key]
	if !ok {
		return nil, errObjectAbsent
	}

	return io.NopCloser(bytes.NewReader(object.body)), nil
}

func (f *fakeBulk) Head(_ context.Context, key string) (objectInfo, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[key]
	if !ok {
		return objectInfo{}, "", errObjectAbsent
	}

	return objectInfo{Key: key, Size: int64(len(object.body)), Modified: object.modified}, object.upload, nil
}

func (f *fakeBulk) ListObjects(_ context.Context, prefix string) ([]objectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []objectInfo
	for key, object := range f.objects {
		if strings.HasPrefix(key, prefix) {
			out = append(out, objectInfo{Key: key, Size: int64(len(object.body)), Modified: object.modified})
		}
	}
	slices.SortFunc(out, func(a, b objectInfo) int { return strings.Compare(a.Key, b.Key) })

	return out, nil
}

func (f *fakeBulk) CreateMultipart(_ context.Context, key, upload string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	id := fmt.Sprintf("mpu-%d", f.nextID)
	f.uploads[id] = &fakeBulkUpload{key: key, upload: upload, parts: map[int][]byte{}, initiated: *f.now}

	return id, nil
}

func (f *fakeBulk) CopyPart(
	_ context.Context, key, uploadID string, number int, source string,
) (completedPart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failCopy != nil {
		return completedPart{}, f.failCopy
	}
	object, ok := f.objects[source]
	if !ok {
		return completedPart{}, errObjectAbsent
	}
	f.uploads[uploadID].parts[number] = object.body

	return completedPart{Number: number, ETag: fmt.Sprintf("etag-%d", number)}, nil
}

func (f *fakeBulk) UploadPart(
	_ context.Context, key, uploadID string, number int, body []byte,
) (completedPart, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.uploads[uploadID].parts[number] = slices.Clone(body)

	return completedPart{Number: number, ETag: fmt.Sprintf("etag-%d", number)}, nil
}

func (f *fakeBulk) CompleteMultipart(
	_ context.Context, key, uploadID string, parts []completedPart,
) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	upload := f.uploads[uploadID]
	if _, exists := f.objects[key]; exists {
		return errObjectConflict
	}
	var body []byte
	for i, part := range parts {
		chunk := upload.parts[part.Number]
		// S3'S OWN RULE: every part but the last is at least the minimum.
		if i != len(parts)-1 && len(chunk) < partMinimum {
			return fmt.Errorf("EntityTooSmall: part %d is %d bytes", part.Number, len(chunk))
		}
		f.partSizes = append(f.partSizes, len(chunk))
		body = append(body, chunk...)
	}
	f.objects[key] = fakeBulkObject{body: body, modified: *f.now, upload: upload.upload}
	delete(f.uploads, uploadID)
	if f.loseCompletion {
		f.loseCompletion = false

		return errObjectAmbiguous
	}

	return nil
}

func (f *fakeBulk) AbortMultipart(_ context.Context, _, uploadID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.uploads, uploadID)

	return nil
}

func (f *fakeBulk) ListMultipart(_ context.Context, prefix string) ([]multipartInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []multipartInfo
	for id, upload := range f.uploads {
		if strings.HasPrefix(upload.key, prefix) {
			out = append(out, multipartInfo{Key: upload.key, UploadID: id, Initiated: upload.initiated})
		}
	}

	return out, nil
}

// The state half's calls, so one fake is both halves of S3 here.
func (f *fakeBulk) Get(_ context.Context, key string) ([]byte, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.objects[key]
	if !ok {
		return nil, "", false, nil
	}

	return object.body, "etag", true, nil
}

func (f *fakeBulk) Put(_ context.Context, key string, body []byte, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.objects[key] = fakeBulkObject{body: body, modified: *f.now}

	return "etag", nil
}

func (f *fakeBulk) List(_ context.Context, prefix string) ([]string, error) {
	objects, err := f.ListObjects(context.Background(), prefix)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(objects))
	for _, object := range objects {
		keys = append(keys, object.Key)
	}

	return keys, nil
}

func (f *fakeBulk) Delete(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.objects, key)

	return nil
}

func archiveStore(now *time.Time) (*Store, *fakeBulk) {
	bulk := newFakeBulk(now)
	store := newStore(config.EBSS3Config{
		Region: "us-west-2", AvailabilityZone: "us-west-2a", Bucket: "bucket", Prefix: "billet-cache",
	}, "deployment/site", newFakeBlocks(), bulk, withBulk(bulk), withNow(func() time.Time { return *now }))

	return store, bulk
}

func stage(t *testing.T, store *Store, upload, block string, body []byte) {
	t.Helper()
	if err := store.StageArchive(t.Context(), upload, block, bytes.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("StageArchive(%q): %v", block, err)
	}
}

func readWhole(t *testing.T, store *Store, archive storecontract.Archive) []byte {
	t.Helper()
	reader, err := store.ReadArchive(t.Context(), archive, 0, archive.Size)
	if err != nil {
		t.Fatalf("ReadArchive: %v", err)
	}
	defer reader.Close()
	body, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read the archive: %v", err)
	}

	return body
}

func TestAnArchiveIsItsBlocksInTheClientsOrder(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	first := bytes.Repeat([]byte("a"), partMinimum)
	second := bytes.Repeat([]byte("b"), partMinimum)
	last := []byte("tail")

	// STAGED OUT OF ORDER, as eight workers do. The list is what says the order.
	stage(t, store, "up1", "block-2", second)
	stage(t, store, "up1", "block-3", last)
	stage(t, store, "up1", "block-1", first)

	published, err := store.CommitArchive(t.Context(), "ns/key", "up1",
		[]string{"block-1", "block-2", "block-3"}, int64(len(first)+len(second)+len(last)))
	if err != nil || !published {
		t.Fatalf("CommitArchive = %v, %v", published, err)
	}
	got, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatalf("MatchArchive: %v", err)
	}
	want := slices.Concat(first, second, last)
	if !bytes.Equal(readWhole(t, store, got), want) {
		t.Fatal("the archive's bytes are not its blocks in the listed order")
	}
}

func TestACommitReleasesItsStagedBlocks(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "up1", "b", []byte("body"))
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 4); err != nil {
		t.Fatalf("CommitArchive: %v", err)
	}
	staged, err := bulk.ListObjects(t.Context(), store.stagePrefix("up1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staged) != 0 {
		t.Fatalf("%d staged blocks outlived the commit", len(staged))
	}
}

func TestTheFirstWriterOfAnArchiveWins(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	stage(t, store, "winner", "b", []byte("winner's"))
	stage(t, store, "loser", "b", []byte("losers!!"))

	published, err := store.CommitArchive(t.Context(), "ns/key", "winner", []string{"b"}, 8)
	if err != nil || !published {
		t.Fatalf("the first commit = %v, %v", published, err)
	}
	published, err = store.CommitArchive(t.Context(), "ns/key", "loser", []string{"b"}, 8)
	if err != nil {
		t.Fatalf("the second commit: %v", err)
	}
	if published {
		t.Fatal("the second writer was told it published")
	}
	got, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if body := readWhole(t, store, got); string(body) != "winner's" {
		t.Fatalf("the key holds %q", body)
	}
}

// A commit whose answer was lost may have landed. The object says whose it is.
func TestACommitWhoseAnswerWasLostAsksTheObject(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "up1", "b", []byte("body"))
	bulk.loseCompletion = true

	published, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 4)
	if err != nil {
		t.Fatalf("CommitArchive: %v", err)
	}
	if !published {
		t.Fatal("a commit that landed and lost its answer was reported as another's")
	}

	// AND A RETRY FROM A RESTART, its blocks gone, says the same.
	published, err = store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 4)
	if err != nil || !published {
		t.Fatalf("the retried commit = %v, %v", published, err)
	}
}

func TestACommitRefusesBlocksThatDoNotAddUp(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		blocks []string
		size   int64
	}{
		{"a declared size that differs", []string{"b"}, 5},
		{"a block never staged", []string{"b", "missing"}, 4},
		{"no bytes at all", []string{"b"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, bulk := archiveStore(&now)
			stage(t, store, "up1", "b", []byte("body"))

			published, err := store.CommitArchive(t.Context(), "ns/key", "up1", tc.blocks, tc.size)
			if !errors.Is(err, storecontract.ErrArchiveSize) {
				t.Fatalf("CommitArchive = %v, %v; want ErrArchiveSize", published, err)
			}
			exists, err := store.ArchiveExists(t.Context(), "ns/key")
			if err != nil || exists {
				t.Fatalf("ArchiveExists = %v, %v; nothing should be published", exists, err)
			}
			if len(bulk.uploads) != 0 {
				t.Fatal("a refused commit left a multipart upload billing")
			}
		})
	}
}

// S3 refuses a part below its minimum anywhere but last, which a block-for-part copy
// would run straight into: BuildKit's blocks are small.
func TestSmallBlocksAreRejoinedRatherThanRefusedByS3(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	var blocks []string
	var want []byte
	for i := range 20 {
		block := bytes.Repeat([]byte{byte('a' + i)}, 1<<20)
		name := fmt.Sprintf("block-%02d", i)
		stage(t, store, "up1", name, block)
		blocks = append(blocks, name)
		want = append(want, block...)
	}

	published, err := store.CommitArchive(t.Context(), "ns/key", "up1", blocks, int64(len(want)))
	if err != nil || !published {
		t.Fatalf("CommitArchive = %v, %v", published, err)
	}
	got, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readWhole(t, store, got), want) {
		t.Fatal("rejoined bytes are not the blocks in order")
	}
	if len(bulk.partSizes) == 0 {
		t.Fatal("no part was assembled")
	}
}

func TestAFailedAssemblyAbortsItsUpload(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "up1", "b", []byte("body"))
	bulk.failCopy = errors.New("copy refused")

	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 4); err == nil ||
		!strings.Contains(err.Error(), "copy refused") {
		t.Fatalf("CommitArchive = %v, want the copy's refusal", err)
	}
	if len(bulk.uploads) != 0 {
		t.Fatal("a failed assembly left a multipart upload billing")
	}
	exists, err := store.ArchiveExists(t.Context(), "ns/key")
	if err != nil || exists {
		t.Fatalf("ArchiveExists = %v, %v", exists, err)
	}
}

func TestAnArchiveMatchesTheExactKeyThenTheNewestUnderTheFirstPrefix(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	commit := func(key, body string) {
		t.Helper()
		upload := "up-" + keyDigest(key)
		stage(t, store, upload, "b", []byte(body))
		if _, err := store.CommitArchive(t.Context(), key, upload, []string{"b"}, int64(len(body))); err != nil {
			t.Fatalf("commit %q: %v", key, err)
		}
	}
	commit("ns/linux-old", "old")
	now = now.Add(time.Hour)
	commit("ns/linux-new", "new")
	commit("ns/darwin-1", "mac")
	commit("ns/exact", "exact")

	for _, tc := range []struct {
		name     string
		exact    string
		prefixes []string
		want     string
	}{
		{"the exact key beats every prefix", "ns/exact", []string{"ns/linux-"}, "ns/exact"},
		{"the newest under a prefix", "ns/none", []string{"ns/linux-"}, "ns/linux-new"},
		{"the first prefix with a match", "ns/none", []string{"ns/windows-", "ns/darwin-", "ns/linux-"}, "ns/darwin-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := store.MatchArchive(t.Context(), tc.exact, tc.prefixes)
			if err != nil {
				t.Fatalf("MatchArchive: %v", err)
			}
			if got.Key != tc.want {
				t.Fatalf("matched %q, want %q", got.Key, tc.want)
			}
		})
	}

	_, err := store.MatchArchive(t.Context(), "ns/none", []string{"ns/nothing-"})
	if !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("a key nothing matches = %v, want ErrMiss", err)
	}
}

// A cache key is the client's: slashes, spaces, percent signs and bytes that are not
// UTF-8 all come back as they went in, and a restore prefix of one is still a prefix.
func TestACacheKeySurvivesBeingAnObjectName(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	key := "ns/node modules/100%-é\xff+?#/end"
	stage(t, store, "up1", "b", []byte("body"))
	if _, err := store.CommitArchive(t.Context(), key, "up1", []string{"b"}, 4); err != nil {
		t.Fatalf("CommitArchive: %v", err)
	}
	got, err := store.MatchArchive(t.Context(), "ns/absent", []string{"ns/node modules/100%-é\xff"})
	if err != nil {
		t.Fatalf("MatchArchive: %v", err)
	}
	if got.Key != key {
		t.Fatalf("the key came back as %q", got.Key)
	}
}

func TestAKeyTooLongForAnObjectNameIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	key := "ns/" + strings.Repeat("\xff", 500)
	if _, err := store.ArchiveExists(t.Context(), key); err == nil ||
		!strings.Contains(err.Error(), "too long") {
		t.Fatalf("ArchiveExists = %v, want it refused as too long", err)
	}
}

func TestARangeOfAnArchiveIsThatRange(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	// Past several chunks, so what is read crosses what was fetched at once.
	size := 3*readChunk + 17
	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i % 251)
	}
	stage(t, store, "up1", "b", body)
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, int64(size)); err != nil {
		t.Fatal(err)
	}
	archive, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ start, length int64 }{
		{0, int64(size)},
		{readChunk - 3, 7},
		{int64(size) - 1, 1},
		{5, 2*readChunk + 1},
	} {
		reader, err := store.ReadArchive(t.Context(), archive, tc.start, tc.length)
		if err != nil {
			t.Fatalf("ReadArchive(%d, %d): %v", tc.start, tc.length, err)
		}
		got, err := io.ReadAll(reader)
		if closeErr := reader.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatalf("read (%d, %d): %v", tc.start, tc.length, err)
		}
		if !bytes.Equal(got, body[tc.start:tc.start+tc.length]) {
			t.Fatalf("range (%d, %d) came back wrong", tc.start, tc.length)
		}
	}
	if _, err := store.ReadArchive(t.Context(), archive, int64(size), 1); err == nil {
		t.Fatal("a range past the archive's end was accepted")
	}
}

func TestAFailedChunkEndsTheReadRatherThanSkippingIt(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "up1", "b", make([]byte, 2*readChunk))
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 2*readChunk); err != nil {
		t.Fatal(err)
	}
	archive, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	// errObjectAbsent IS NOT RETRIED, so one failure is the whole answer.
	bulk.failRange = errObjectAbsent
	reader, err := store.ReadArchive(t.Context(), archive, 0, archive.Size)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, err := io.ReadAll(reader)
	if !errors.Is(err, errObjectAbsent) {
		t.Fatalf("read %d bytes and %v; want the failure, not a short read", len(got), err)
	}
}

func TestClosingAReadStopsItsFetching(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	size := int64(16 * readChunk)
	stage(t, store, "up1", "b", make([]byte, size))
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, size); err != nil {
		t.Fatal(err)
	}
	archive, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := store.ReadArchive(t.Context(), archive, 0, size)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	// Close JOINS every goroutine the read started, so returning at all is the proof
	// nothing is left blocked on a window nobody will drain.
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestEvictionTakesAnArchiveNobodyHasLookedUp(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	commit := func(key string) {
		t.Helper()
		upload := "up-" + keyDigest(key)
		stage(t, store, upload, "b", []byte("body"))
		if _, err := store.CommitArchive(t.Context(), key, upload, []string{"b"}, 4); err != nil {
			t.Fatal(err)
		}
	}
	commit("ns/unused")
	commit("ns/used")
	now = now.Add(5 * 24 * time.Hour)
	if _, err := store.MatchArchive(t.Context(), "ns/used", nil); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * 24 * time.Hour)

	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	for key, want := range map[string]bool{"ns/unused": false, "ns/used": true} {
		exists, err := store.ArchiveExists(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		if exists != want {
			t.Errorf("after eviction, %s exists = %v, want %v", key, exists, want)
		}
	}
}

func TestEvictionTakesAbandonedUploadsOnlyOnceNoJobCanBeFinishingThem(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "abandoned", "b", []byte("body"))
	if _, err := bulk.CreateMultipart(t.Context(), store.ownerPrefix()+"archives/x", "abandoned"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(staleUpload - time.Hour)
	stage(t, store, "live", "b", []byte("body"))
	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if staged, _ := bulk.ListObjects(t.Context(), store.stagePrefix("abandoned")); len(staged) != 1 {
		t.Fatal("a block still inside the window was taken")
	}
	if len(bulk.uploads) != 1 {
		t.Fatal("an upload still inside the window was aborted")
	}

	now = now.Add(2 * time.Hour)
	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if staged, _ := bulk.ListObjects(t.Context(), store.stagePrefix("abandoned")); len(staged) != 0 {
		t.Fatal("an abandoned block outlived its window")
	}
	if len(bulk.uploads) != 0 {
		t.Fatal("an abandoned multipart upload outlived its window")
	}
	if staged, _ := bulk.ListObjects(t.Context(), store.stagePrefix("live")); len(staged) != 1 {
		t.Fatal("a recent upload's block was taken with the old one")
	}
}

func blobRef(table storecontract.BlobTable, digest byte) storecontract.BlobRef {
	return storecontract.BlobRef{Table: table, Digest: strings.Repeat(string(digest), 64)}
}

func TestAContentAddressedEntryIsWrittenOnce(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	ref := blobRef(storecontract.BlobTableAC, 'a')
	put := func(body string) {
		t.Helper()
		if err := store.PutBlob(t.Context(), "key", ref, strings.NewReader(body), int64(len(body))); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
	}
	put("first")
	// NOT AN ERROR: of two action results either is a valid answer, and the second
	// job's publication has nothing to be told.
	put("second")

	body, err := store.ReadBlob(t.Context(), "key", ref)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first" {
		t.Fatalf("the entry holds %q", got)
	}
	refs, err := store.ListBlobs(t.Context(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refs[ref]; !ok || len(refs) != 1 {
		t.Fatalf("ListBlobs = %v", refs)
	}
	if _, err := store.ReadBlob(t.Context(), "key", blobRef(storecontract.BlobTableCAS, 'b')); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("an absent entry = %v, want ErrMiss", err)
	}
	if refs, err := store.ListBlobs(t.Context(), "another key"); err != nil || len(refs) != 0 {
		t.Fatalf("another key's listing = %v, %v", refs, err)
	}
}

func TestEvictionSweepsOnlyEntriesNoRecentJobUsed(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	used, unused := blobRef(storecontract.BlobTableCAS, 'a'), blobRef(storecontract.BlobTableCAS, 'b')
	otherKey := blobRef(storecontract.BlobTableCAS, 'c')
	for _, write := range []struct {
		key string
		ref storecontract.BlobRef
	}{{"key", used}, {"key", unused}, {"other", otherKey}} {
		if err := store.PutBlob(t.Context(), write.key, write.ref, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(5 * 24 * time.Hour)
	if err := store.RecordBlobUse(t.Context(), "key", "job-1", []storecontract.BlobRef{used}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * 24 * time.Hour)

	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	refs, err := store.ListBlobs(t.Context(), "key")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refs[used]; !ok {
		t.Error("an entry a job used within the retention was evicted")
	}
	if _, ok := refs[unused]; ok {
		t.Error("an entry nothing used was kept")
	}
	// A USE OF ONE KEY KEEPS NOTHING OF ANOTHER'S: the same digest under another key
	// is another scope's entry.
	if refs, err := store.ListBlobs(t.Context(), "other"); err != nil || len(refs) != 0 {
		t.Errorf("another key's unused entry = %v, %v", refs, err)
	}

	// A RECORD PAST THE RETENTION KEEPS NOTHING, and is itself taken.
	now = now.Add(5 * 24 * time.Hour)
	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if refs, err := store.ListBlobs(t.Context(), "key"); err != nil || len(refs) != 0 {
		t.Errorf("after the record expired, ListBlobs = %v, %v", refs, err)
	}
	if uses, _ := bulk.ListObjects(t.Context(), store.ownerPrefix()+"blob-uses/"); len(uses) != 0 {
		t.Errorf("%d expired use records were kept", len(uses))
	}
}

// What a record names cannot be told from what nobody uses if it cannot be read, and
// taking what a job uses is the one way the sweep can do harm.
func TestAUseRecordThatCannotBeReadStopsTheSweep(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	ref := blobRef(storecontract.BlobTableCAS, 'a')
	if err := store.PutBlob(t.Context(), "key", ref, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(8 * 24 * time.Hour)
	corrupt := []byte("not gzip")
	if err := bulk.PutStream(t.Context(), store.blobUseObject("key", "job"), bytes.NewReader(corrupt),
		int64(len(corrupt)), false); err != nil {
		t.Fatal(err)
	}

	if err := store.Evict(t.Context(), 7*24*time.Hour); err == nil ||
		!strings.Contains(err.Error(), "use record") {
		t.Fatalf("Evict = %v, want it to report the record", err)
	}
	if refs, err := store.ListBlobs(t.Context(), "key"); err != nil || len(refs) != 1 {
		t.Fatalf("the sweep took an entry it could not judge: %v, %v", refs, err)
	}
}

func TestPurgeRemovesTheObjectHalfAndLeavesNoUploadBilling(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "up1", "b", []byte("body"))
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 4); err != nil {
		t.Fatal(err)
	}
	if err := store.PutBlob(t.Context(), "key", blobRef(storecontract.BlobTableCAS, 'a'),
		strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	stage(t, store, "abandoned", "b", []byte("body"))
	if _, err := bulk.CreateMultipart(t.Context(), store.ownerPrefix()+"archives/x", "abandoned"); err != nil {
		t.Fatal(err)
	}
	// A state object, so the purge is held to removing that too.
	if _, _, err := store.AcquireWriter(t.Context(), "state-key", "holder", time.Hour); err != nil {
		t.Fatal(err)
	}

	report, err := store.Purge(t.Context())
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if report.CacheObjects == 0 {
		t.Fatal("Purge reports no cache objects")
	}
	if left, _ := bulk.ListObjects(t.Context(), store.ownerPrefix()); len(left) != 0 {
		t.Fatalf("Purge left %d objects: %v", len(left), left)
	}
	if len(bulk.uploads) != 0 {
		t.Fatal("Purge left a multipart upload billing")
	}
}

func TestAnotherOwnersObjectsAreNotInThisOnesNames(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	other := newStore(store.cfg, "another/site", newFakeBlocks(), bulk, withBulk(bulk),
		withNow(func() time.Time { return now }))
	stage(t, other, "up1", "b", []byte("body"))
	if _, err := other.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 4); err != nil {
		t.Fatal(err)
	}
	exists, err := store.ArchiveExists(t.Context(), "ns/key")
	if err != nil || exists {
		t.Fatalf("one owner sees another's archive: %v, %v", exists, err)
	}
	if _, err := store.Purge(t.Context()); err != nil {
		t.Fatal(err)
	}
	exists, err = other.ArchiveExists(t.Context(), "ns/key")
	if err != nil || !exists {
		t.Fatalf("one owner's purge reached another's archive: %v, %v", exists, err)
	}
}
