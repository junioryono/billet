package ebss3

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	// dropCompletion answers the next completion as if it were lost, and does not land it.
	dropCompletion bool
	// rival lands at the key just before the next completion is judged: another
	// upload that got past the check a commit opens with.
	rival *fakeBulkObject
	// failCopy fails every part copy.
	failCopy error
	// onCopy runs as a part copy starts, outside the lock.
	onCopy func()
	// reverseListing answers listings last name first, which S3 does not promise never to.
	reverseListing bool
	// truncateGets cuts every whole-object read short, as a dropped connection does.
	truncateGets bool
	// failStateList fails the state half's listing, and so the volume half of eviction.
	failStateList error
	// holdRanges blocks every ranged read past the first chunk until its context ends,
	// announcing each on the channel.
	holdRanges chan struct{}
	// rangesStarted and rangesReturned count the held reads.
	rangesStarted, rangesReturned atomic.Int32
	// failRange fails the next ranged read, once.
	failRange error
	// partSizes records every part size a completion assembled.
	partSizes []int
}

type fakeBulkObject struct {
	body     []byte
	modified time.Time
	upload   string
	etag     string
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
	f.objects[key] = fakeBulkObject{body: data, modified: *f.now, etag: f.nextETag()}

	return nil
}

func (f *fakeBulk) nextETag() string {
	f.nextID++

	return fmt.Sprintf(`"etag-%d"`, f.nextID)
}

func (f *fakeBulk) GetRange(
	ctx context.Context, key, etag string, start, length int64,
) (io.ReadCloser, error) {
	if f.holdRanges != nil && start >= readChunk {
		f.rangesStarted.Add(1)
		defer f.rangesReturned.Add(1)
		select {
		case f.holdRanges <- struct{}{}:
		default:
		}
		<-ctx.Done()

		return nil, ctx.Err()
	}
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
	if etag != "" && object.etag != etag {
		return nil, errObjectConflict
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
	if f.truncateGets {
		return io.NopCloser(io.MultiReader(bytes.NewReader(object.body[:len(object.body)/2]),
			readerOf{io.ErrUnexpectedEOF})), nil
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
			out = append(out, objectInfo{
				Key: key, Size: int64(len(object.body)), Modified: object.modified, ETag: object.etag,
			})
		}
	}
	slices.SortFunc(out, func(a, b objectInfo) int { return strings.Compare(a.Key, b.Key) })
	if f.reverseListing {
		slices.Reverse(out)
	}

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
	if f.onCopy != nil {
		f.onCopy()
	}
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
	if f.dropCompletion {
		f.dropCompletion = false

		return errObjectAmbiguous
	}
	if f.rival != nil {
		f.objects[key] = *f.rival
		f.rival = nil
	}
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
	f.objects[key] = fakeBulkObject{body: body, modified: *f.now, upload: upload.upload, etag: f.nextETag()}
	delete(f.uploads, uploadID)
	if f.loseCompletion {
		f.loseCompletion = false

		return errObjectAmbiguous
	}

	return nil
}

func (f *fakeBulk) AbortMultipart(ctx context.Context, _, uploadID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
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
	f.objects[key] = fakeBulkObject{body: body, modified: *f.now, etag: f.nextETag()}

	return "etag", nil
}

func (f *fakeBulk) List(ctx context.Context, prefix string) ([]string, error) {
	if f.failStateList != nil {
		return nil, f.failStateList
	}
	objects, err := f.ListObjects(ctx, prefix)
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

// readerOf is a reader that only fails.
type readerOf struct{ err error }

func (r readerOf) Read([]byte) (int, error) { return 0, r.err }

// listed is what S3 holds under a prefix.
func listed(t *testing.T, bulk *fakeBulk, prefix string) []objectInfo {
	t.Helper()
	objects, err := bulk.ListObjects(t.Context(), prefix)
	if err != nil {
		t.Fatalf("list %q: %v", prefix, err)
	}

	return objects
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

// TWO COMMITS RACING: both found the key empty, and the condition on the completion is
// what decides. The loser is told so, and leaves nothing billing.
func TestACommitThatLosesTheRaceAtCompletionIsNotPublished(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "loser", "b", []byte("losers!!"))
	bulk.rival = &fakeBulkObject{body: []byte("winner's"), modified: now, upload: "winner"}

	published, err := store.CommitArchive(t.Context(), "ns/key", "loser", []string{"b"}, 8)
	if err != nil {
		t.Fatalf("the losing commit: %v", err)
	}
	if published {
		t.Fatal("the loser was told it published")
	}
	if len(bulk.uploads) != 0 {
		t.Fatal("the losing commit left a multipart upload billing")
	}
	if staged := listed(t, bulk, store.stagePrefix("loser")); len(staged) != 0 {
		t.Fatalf("the losing commit kept %d staged blocks", len(staged))
	}
	got, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if body := readWhole(t, store, got); string(body) != "winner's" {
		t.Fatalf("the key holds %q", body)
	}
}

// AN ANSWER LOST AND NOTHING LANDED: the upload is abandoned, and the blocks stay for
// the client's own retry, because could-not-tell is not "published".
func TestACommitWhoseAnswerWasLostAndDidNotLandFails(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "up1", "b", []byte("body"))
	bulk.dropCompletion = true

	published, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 4)
	if !errors.Is(err, errObjectAmbiguous) || published {
		t.Fatalf("CommitArchive = %v, %v; want the ambiguity reported", published, err)
	}
	if len(bulk.uploads) != 0 {
		t.Fatal("the commit left a multipart upload billing")
	}
	if staged := listed(t, bulk, store.stagePrefix("up1")); len(staged) != 1 {
		t.Fatalf("%d staged blocks remain; the retry needs its one", len(staged))
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
		{"no bytes at all", nil, 0},
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
	// PAST ONE BUFFER, so a part is sent from the middle of a block.
	for i := range 70 {
		block := bytes.Repeat([]byte{byte(i)}, 1<<20)
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
	if len(bulk.partSizes) != 2 || bulk.partSizes[0] != partBuffer {
		t.Fatalf("parts of %v bytes, want one of the buffer's %d and the rest", bulk.partSizes, partBuffer)
	}
}

// A BODY CUT SHORT READS LIKE A BLOCK THAT ENDED. Published, a short archive stays: the
// first writer holds the key, every restore fails to extract, and each one refreshes
// its marker so eviction never takes it.
func TestARejoinThatReadsABlockShortPublishesNothing(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	for _, block := range []string{"a", "b"} {
		stage(t, store, "up1", block, bytes.Repeat([]byte(block), 1<<20))
	}
	bulk.truncateGets = true

	published, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"a", "b"}, 2<<20)
	if err == nil || published || !strings.Contains(err.Error(), "read back") {
		t.Fatalf("CommitArchive = %v, %v; want the short read reported", published, err)
	}
	exists, err := store.ArchiveExists(t.Context(), "ns/key")
	if err != nil || exists {
		t.Fatalf("ArchiveExists = %v, %v; a truncated archive was published", exists, err)
	}
	if len(bulk.uploads) != 0 {
		t.Fatal("the failed commit left a multipart upload billing")
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

// THE COMMIT'S OWN CONTEXT MAY BE WHY IT FAILED, and an upload left behind bills until
// eviction finds it, so the abort does not ride on that context.
func TestACancelledCommitStillAbortsItsUpload(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "up1", "b", []byte("body"))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bulk.onCopy = cancel
	bulk.failCopy = context.Canceled

	if _, err := store.CommitArchive(ctx, "ns/key", "up1", []string{"b"}, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("CommitArchive = %v, want the cancellation", err)
	}
	if len(bulk.uploads) != 0 {
		t.Fatal("a cancelled commit left a multipart upload billing")
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
	// NAMED TO SORT LAST, so only the date can choose it.
	commit("ns/linux-z-new", "new")
	commit("ns/darwin-1", "mac")
	commit("ns/exact", "exact")

	for _, tc := range []struct {
		name     string
		exact    string
		prefixes []string
		want     string
	}{
		{"the exact key beats every prefix", "ns/exact", []string{"ns/linux-"}, "ns/exact"},
		{"the newest under a prefix", "ns/none", []string{"ns/linux-"}, "ns/linux-z-new"},
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
	store, bulk := archiveStore(&now)
	key := "ns/node modules/100%-é\xff+?#/end"
	stage(t, store, "up1", "b", []byte("body"))
	if _, err := store.CommitArchive(t.Context(), key, "up1", []string{"b"}, 4); err != nil {
		t.Fatalf("CommitArchive: %v", err)
	}
	// THE RESTORE PREFIX STOPS BEFORE THE ESCAPES, so they come back through the name.
	got, err := store.MatchArchive(t.Context(), "ns/absent", []string{"ns/node"})
	if err != nil {
		t.Fatalf("MatchArchive: %v", err)
	}
	if got.Key != key {
		t.Fatalf("the key came back as %q", got.Key)
	}
	archives := listed(t, bulk, store.ownerPrefix()+"archives/")
	if len(archives) != 1 {
		t.Fatalf("archives = %v", archives)
	}
	for _, c := range []byte(archives[0].Key) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '/', c == '%':
		default:
			t.Fatalf("the object is named %q; S3 and its signer each read %q their own way", archives[0].Key, c)
		}
	}
}

// ONE LOOKUP ANSWERS THE SAME WAY TWICE: of two as new, the smallest name, whatever
// order the listing came in.
func TestAnArchiveMatchedOnATieIsTheSmallestName(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	for _, key := range []string{"ns/tie-a", "ns/tie-b"} {
		upload := "up-" + keyDigest(key)
		stage(t, store, upload, "b", []byte("body"))
		if _, err := store.CommitArchive(t.Context(), key, upload, []string{"b"}, 4); err != nil {
			t.Fatalf("commit %q: %v", key, err)
		}
	}
	bulk.reverseListing = true
	got, err := store.MatchArchive(t.Context(), "ns/none", []string{"ns/tie-"})
	if err != nil {
		t.Fatalf("MatchArchive: %v", err)
	}
	if got.Key != "ns/tie-a" {
		t.Fatalf("matched %q", got.Key)
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
	if _, err := store.ReadArchive(t.Context(), archive, int64(size), 1); err == nil ||
		!strings.Contains(err.Error(), "outside an archive") {
		t.Fatalf("a range past the archive's end = %v", err)
	}
}

// A KEY EVICTED AND SAVED AGAIN MID-DOWNLOAD IS ANOTHER ARCHIVE. A read fails rather than
// hand the client the first half of one and the second half of the other.
func TestAReadNeverMixesTwoArchivesOfOneKey(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "up1", "b", []byte("first"))
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 5); err != nil {
		t.Fatal(err)
	}
	archive, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if archive.ETag == "" {
		t.Fatal("the match names no object")
	}
	name, err := store.archiveObject("ns/key")
	if err != nil {
		t.Fatal(err)
	}
	if err := bulk.Delete(t.Context(), name); err != nil {
		t.Fatal(err)
	}
	stage(t, store, "up2", "b", []byte("other"))
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up2", []string{"b"}, 5); err != nil {
		t.Fatal(err)
	}

	reader, err := store.ReadArchive(t.Context(), archive, 0, archive.Size)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if got, err := io.ReadAll(reader); !errors.Is(err, errObjectConflict) {
		t.Fatalf("read %q and %v; want the object's change reported", got, err)
	}
}

// A NODE TAKES 128 CONNECTIONS, AND A JOB CAN OPEN EVERY ONE AS A READ OF A SIGNED URL AND
// STOP READING: fetching ahead on each would hold 5 GiB on a node of 4. Past the bound a
// read is a plain stream, which holds nothing and returns the same bytes.
func TestOnlySoManyReadsFetchAhead(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, _ := archiveStore(&now)
	body := []byte("the archive's bytes")
	stage(t, store, "up1", "b", body)
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, int64(len(body))); err != nil {
		t.Fatal(err)
	}
	archive, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	readers := make([]io.ReadCloser, 0, readAheadReaders)
	for range readAheadReaders {
		reader, err := store.ReadArchive(t.Context(), archive, 0, archive.Size)
		if err != nil {
			t.Fatal(err)
		}
		if _, ahead := reader.(*rangeReader); !ahead {
			t.Fatalf("read %d did not fetch ahead", len(readers))
		}
		readers = append(readers, reader)
	}

	past, err := store.ReadArchive(t.Context(), archive, 0, archive.Size)
	if err != nil {
		t.Fatal(err)
	}
	if _, ahead := past.(*rangeReader); ahead {
		t.Fatal("a read past the bound fetched ahead")
	}
	got, err := io.ReadAll(past)
	if closeErr := past.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("the plain stream = %q, %v", got, err)
	}

	// A SLOT COMES BACK ONCE, however often its read is closed.
	for range 2 {
		if err := readers[0].Close(); err != nil {
			t.Fatal(err)
		}
	}
	if held := len(store.readAhead); held != readAheadReaders-1 {
		t.Fatalf("%d slots held after one read closed twice", held)
	}
	for _, reader := range readers[1:] {
		if err := reader.Close(); err != nil {
			t.Fatal(err)
		}
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
	store, bulk := archiveStore(&now)
	size := int64(16 * readChunk)
	stage(t, store, "up1", "b", make([]byte, size))
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, size); err != nil {
		t.Fatal(err)
	}
	archive, err := store.MatchArchive(t.Context(), "ns/key", nil)
	if err != nil {
		t.Fatal(err)
	}
	bulk.holdRanges = make(chan struct{}, 1)
	reader, err := store.ReadArchive(t.Context(), archive, 0, size)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader.Read(make([]byte, 16)); err != nil {
		t.Fatal(err)
	}
	guard := time.NewTimer(30 * time.Second)
	defer guard.Stop()
	select {
	case <-bulk.holdRanges:
	case <-guard.C:
		t.Fatal("no fetch ever went ahead of the reader")
	}

	closed := make(chan error, 1)
	go func() { closed <- reader.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-guard.C:
		t.Fatal("Close did not end the fetches in flight")
	}
	// Close JOINS WHAT THE READ STARTED: every fetch it let begin has returned.
	if started, returned := bulk.rangesStarted.Load(), bulk.rangesReturned.Load(); started == 0 || started != returned {
		t.Fatalf("%d fetches started and %d returned by the time Close did", started, returned)
	}
}

func TestEvictionTakesAnArchiveNobodyHasLookedUp(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
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
	commit("ns/found by prefix-1")
	commit("ns/used with a space")
	now = now.Add(5 * 24 * time.Hour)
	if _, err := store.MatchArchive(t.Context(), "ns/used", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := store.MatchArchive(t.Context(), "ns/used with a space", nil); err != nil {
		t.Fatal(err)
	}
	// THE ARCHIVE A RESTORE KEY FOUND IS THE ONE IN USE, not the key that was asked for.
	if _, err := store.MatchArchive(t.Context(), "ns/absent", []string{"ns/found by prefix-"}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(3 * 24 * time.Hour)

	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	for key, want := range map[string]bool{
		"ns/unused": false, "ns/used": true, "ns/found by prefix-1": true, "ns/used with a space": true,
	} {
		exists, err := store.ArchiveExists(t.Context(), key)
		if err != nil {
			t.Fatal(err)
		}
		if exists != want {
			t.Errorf("after eviction, %s exists = %v, want %v", key, exists, want)
		}
	}
	if markers := listed(t, bulk, store.ownerPrefix()+"archive-used/"); len(markers) != 3 {
		t.Errorf("%d markers survive, want the three used archives': %v", len(markers), markers)
	}

	// THE MARKER GOES WITH ITS ARCHIVE, or the prefix fills with records of nothing.
	now = now.Add(8 * 24 * time.Hour)
	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if left := listed(t, bulk, store.ownerPrefix()+"archives/"); len(left) != 0 {
		t.Errorf("archives nobody looked up survive: %v", left)
	}
	if markers := listed(t, bulk, store.ownerPrefix()+"archive-used/"); len(markers) != 0 {
		t.Errorf("markers outlived their archives: %v", markers)
	}
}

// ONE SNAPSHOT THAT REFUSES DELETION MUST NOT LEAVE EVERY ARCHIVE AND UPLOAD BILLING.
func TestEvictionTakesObjectsWhateverTheVolumeHalfDid(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	stage(t, store, "up1", "b", []byte("body"))
	if _, err := store.CommitArchive(t.Context(), "ns/key", "up1", []string{"b"}, 4); err != nil {
		t.Fatal(err)
	}
	now = now.Add(8 * 24 * time.Hour)
	stateless := errors.New("the state index cannot be listed")
	bulk.failStateList = stateless

	if err := store.Evict(t.Context(), 7*24*time.Hour); !errors.Is(err, stateless) {
		t.Fatalf("Evict = %v, want the volume half's failure reported", err)
	}
	if left := listed(t, bulk, store.ownerPrefix()+"archives/"); len(left) != 0 {
		t.Fatalf("the object half did not run: %v", left)
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
	if staged := listed(t, bulk, store.stagePrefix("abandoned")); len(staged) != 1 {
		t.Fatal("a block still inside the window was taken")
	}
	if len(bulk.uploads) != 1 {
		t.Fatal("an upload still inside the window was aborted")
	}

	now = now.Add(2 * time.Hour)
	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if staged := listed(t, bulk, store.stagePrefix("abandoned")); len(staged) != 0 {
		t.Fatal("an abandoned block outlived its window")
	}
	if len(bulk.uploads) != 0 {
		t.Fatal("an abandoned multipart upload outlived its window")
	}
	if staged := listed(t, bulk, store.stagePrefix("live")); len(staged) != 1 {
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
	// THE DIGEST THE RECORD NAMES, under another key.
	otherKey := blobRef(storecontract.BlobTableCAS, 'a')
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
	// WRITTEN INSIDE THE RETENTION AND USED BY NOTHING YET, as a job's fresh output is.
	recent := blobRef(storecontract.BlobTableCAS, 'd')
	if err := store.PutBlob(t.Context(), "key", recent, strings.NewReader("x"), 1); err != nil {
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
	if _, ok := refs[recent]; !ok {
		t.Error("an entry written within the retention was evicted")
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
	if uses := listed(t, bulk, store.ownerPrefix()+"blob-uses/"); len(uses) != 0 {
		t.Errorf("%d expired use records were kept", len(uses))
	}
}

// What a record names cannot be told from what nobody uses if it cannot be read, and
// taking what a job uses is the one way the sweep can do harm.
// ONE RECORD PER JOB: a second job's record must not replace the first's, or what only
// the first used reads as unused.
func TestEachJobKeepsItsOwnUseRecord(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	first, second := blobRef(storecontract.BlobTableCAS, 'a'), blobRef(storecontract.BlobTableCAS, 'b')
	for _, ref := range []storecontract.BlobRef{first, second} {
		if err := store.PutBlob(t.Context(), "key", ref, strings.NewReader("x"), 1); err != nil {
			t.Fatal(err)
		}
	}
	now = now.Add(5 * 24 * time.Hour)
	if err := store.RecordBlobUse(t.Context(), "key", "job-1", []storecontract.BlobRef{first}); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordBlobUse(t.Context(), "key", "job-2", []storecontract.BlobRef{second}); err != nil {
		t.Fatal(err)
	}
	if uses := listed(t, bulk, store.ownerPrefix()+"blob-uses/"); len(uses) != 2 {
		t.Fatalf("%d use records, want one per job", len(uses))
	}
	now = now.Add(3 * 24 * time.Hour)
	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatal(err)
	}
	if refs, err := store.ListBlobs(t.Context(), "key"); err != nil || len(refs) != 2 {
		t.Fatalf("what each job used = %v, %v", refs, err)
	}
}

// NOT THE END OF THE RECORD, ONLY OF WHAT WAS READ: what it did not reach would read as
// unused, and a record past the bound stops the sweep as one that cannot be read does.
func TestAUseRecordPastTheBoundStopsTheSweep(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	ref := blobRef(storecontract.BlobTableCAS, 'a')
	if err := store.PutBlob(t.Context(), "key", ref, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(8 * 24 * time.Hour)

	var encoded bytes.Buffer
	writer := gzip.NewWriter(&encoded)
	line := []byte(string(storecontract.BlobTableCAS) + "/" + strings.Repeat("b", 64) + "\n")
	for written := 0; written <= blobUseLimit; written += len(line) {
		if _, err := writer.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := bulk.PutStream(t.Context(), store.blobUseObject("key", "job"), bytes.NewReader(encoded.Bytes()),
		int64(encoded.Len()), false); err != nil {
		t.Fatal(err)
	}

	if err := store.Evict(t.Context(), 7*24*time.Hour); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("Evict = %v, want the record's size reported", err)
	}
	if refs, err := store.ListBlobs(t.Context(), "key"); err != nil || len(refs) != 1 {
		t.Fatalf("the sweep took an entry a record may name: %v, %v", refs, err)
	}
}

// SO BILLET ITSELF NEVER WRITES ONE: a job past one piece's entries gets several, and
// what the last of them names is kept like the rest.
func TestAJobThatUsedALotKeepsAllOfIt(t *testing.T) {
	now := time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC)
	store, bulk := archiveStore(&now)
	used := make([]storecontract.BlobRef, blobUseEntries+1)
	for i := range used {
		used[i] = storecontract.BlobRef{Table: storecontract.BlobTableCAS, Digest: fmt.Sprintf("%064x", i)}
	}
	last := used[len(used)-1]
	if err := store.PutBlob(t.Context(), "key", last, strings.NewReader("x"), 1); err != nil {
		t.Fatal(err)
	}
	now = now.Add(5 * 24 * time.Hour)
	if err := store.RecordBlobUse(t.Context(), "key", "job", used); err != nil {
		t.Fatalf("RecordBlobUse: %v", err)
	}
	if uses := listed(t, bulk, store.ownerPrefix()+"blob-uses/"); len(uses) != 2 {
		t.Fatalf("%d records for one job past a piece's entries, want 2", len(uses))
	}
	now = now.Add(3 * 24 * time.Hour)
	if err := store.Evict(t.Context(), 7*24*time.Hour); err != nil {
		t.Fatalf("Evict: %v", err)
	}
	if refs, err := store.ListBlobs(t.Context(), "key"); err != nil || len(refs) != 1 {
		t.Fatalf("what the second piece names = %v, %v", refs, err)
	}
}

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
	// THE ARCHIVE, THE ENTRY, THE ABANDONED BLOCK AND THE UPLOAD, and not the state
	// object, which the state half's own count covers.
	if report.CacheObjects != 4 {
		t.Fatalf("Purge reports %d cache objects, want 4", report.CacheObjects)
	}
	if left := listed(t, bulk, store.ownerPrefix()); len(left) != 0 {
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
