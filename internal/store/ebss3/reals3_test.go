package ebss3

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/awscreds"
	"github.com/junioryono/billet/internal/config"
	storecontract "github.com/junioryono/billet/internal/store"
)

// TestRealArchiveStore proves the object half against real S3, with billet's own
// signer, under whatever credential the environment holds: run it under the node's
// role to prove the policy too. A fake proves the shape of a request, never the values
// a real service refuses.
//
// Opt-in, and removes everything it wrote:
//
//	BILLET_LIVE_EBSS3_BUCKET=<bucket> BILLET_LIVE_EBSS3_REGION=<region> \
//	BILLET_LIVE_EBSS3_ZONE=<zone> BILLET_LIVE_EBSS3_PREFIX=<prefix> go test -run TestRealArchiveStore
func TestRealArchiveStore(t *testing.T) {
	bucket := os.Getenv("BILLET_LIVE_EBSS3_BUCKET")
	if bucket == "" {
		t.Skip("BILLET_LIVE_EBSS3_BUCKET is not set; this test talks to real S3")
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	run := hex.EncodeToString(nonce[:])
	prefix := os.Getenv("BILLET_LIVE_EBSS3_PREFIX")
	if prefix == "" {
		prefix = "billet-cache"
	}
	store, err := New(config.EBSS3Config{
		Region: os.Getenv("BILLET_LIVE_EBSS3_REGION"), AvailabilityZone: os.Getenv("BILLET_LIVE_EBSS3_ZONE"),
		Bucket: bucket, Prefix: prefix,
	}, "live-test/"+run, awscreds.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := t.Context()
	defer func() {
		if _, err := store.purgeObjects(ctx); err != nil {
			t.Errorf("clean up what the test wrote: %v", err)
		}
	}()

	// THE SHAPES A FAKE ACCEPTS AND S3 MAY NOT.
	first := make([]byte, partMinimum+17)
	if _, err := rand.Read(first); err != nil {
		t.Fatal(err)
	}
	second := []byte("the last block may be any size")
	for name, body := range map[string][]byte{"block-1": first, "block-2": second} {
		if err := store.StageArchive(ctx, "up1", name, io.MultiReader(bytes.NewReader(body)), int64(len(body))); err != nil {
			t.Fatalf("StageArchive(%s): %v", name, err)
		}
	}
	key := "ns/node modules/100%-é+?#/end"
	size := int64(len(first) + len(second))

	if _, err := store.CommitArchive(ctx, key, "up1", []string{"block-1", "block-2"}, size+1); !errors.Is(err, storecontract.ErrArchiveSize) {
		t.Fatalf("a commit of the wrong size = %v, want ErrArchiveSize", err)
	}
	published, err := store.CommitArchive(ctx, key, "up1", []string{"block-1", "block-2"}, size)
	if err != nil || !published {
		t.Fatalf("CommitArchive = %v, %v", published, err)
	}

	// A SECOND COMMIT OF THE SAME UPLOAD ASKS THE OBJECT, whose metadata says whose
	// it is, and its blocks are gone by now.
	published, err = store.CommitArchive(ctx, key, "up1", []string{"block-1", "block-2"}, size)
	if err != nil || !published {
		t.Fatalf("the retried commit = %v, %v", published, err)
	}

	// THE CONDITION ON THE COMPLETION IS WHAT MAKES THE FIRST WRITER WIN: a second
	// upload past the check at the top reaches it.
	if err := store.StageArchive(ctx, "up2", "only", strings.NewReader("second writer"), 13); err != nil {
		t.Fatalf("StageArchive(up2): %v", err)
	}
	name, err := store.archiveObject(key)
	if err != nil {
		t.Fatal(err)
	}
	uploadID, err := store.bulk.CreateMultipart(ctx, name, "up2")
	if err != nil {
		t.Fatalf("CreateMultipart: %v", err)
	}
	part, err := store.bulk.CopyPart(ctx, name, uploadID, 1, store.stageObject("up2", "only"))
	if err != nil {
		t.Fatalf("CopyPart: %v", err)
	}
	err = store.bulk.CompleteMultipart(ctx, name, uploadID, []completedPart{part})
	if !errors.Is(err, errObjectConflict) {
		t.Fatalf("a completion over a published archive = %v, want errObjectConflict", err)
	}
	if err := store.bulk.AbortMultipart(ctx, name, uploadID); err != nil {
		t.Fatalf("AbortMultipart: %v", err)
	}
	if err := store.bulk.AbortMultipart(ctx, name, uploadID); err != nil {
		t.Fatalf("a second abort: %v", err)
	}
	if err := store.AbortArchive(ctx, "up2"); err != nil {
		t.Fatalf("AbortArchive: %v", err)
	}

	archive, err := store.MatchArchive(ctx, "ns/absent", []string{"ns/node modules/"})
	if err != nil {
		t.Fatalf("MatchArchive: %v", err)
	}
	if archive.Key != key || archive.Size != size {
		t.Fatalf("matched %+v", archive)
	}
	reader, err := store.ReadArchive(ctx, archive, partMinimum, 17+int64(len(second)))
	if err != nil {
		t.Fatalf("ReadArchive: %v", err)
	}
	got, err := io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatalf("read the range: %v", err)
	}
	if want := append(append([]byte{}, first[partMinimum:]...), second...); !bytes.Equal(got, want) {
		t.Fatalf("the range holds %d bytes that are not what was staged", len(got))
	}

	// BLOCKS BELOW THE MINIMUM, which only the rejoin can publish.
	for _, name := range []string{"a", "b", "c"} {
		if err := store.StageArchive(ctx, "up3", name, strings.NewReader(name+"-small"), 7); err != nil {
			t.Fatalf("StageArchive(up3/%s): %v", name, err)
		}
	}
	published, err = store.CommitArchive(ctx, "ns/small", "up3", []string{"c", "a", "b"}, 21)
	if err != nil || !published {
		t.Fatalf("the rejoined commit = %v, %v", published, err)
	}
	small, err := store.MatchArchive(ctx, "ns/small", nil)
	if err != nil {
		t.Fatal(err)
	}
	reader, err = store.ReadArchive(ctx, small, 0, small.Size)
	if err != nil {
		t.Fatal(err)
	}
	got, err = io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || string(got) != "c-smalla-smallb-small" {
		t.Fatalf("the rejoined archive = %q, %v", got, err)
	}

	// THE CONTENT-ADDRESSED HALF.
	ref := storecontract.BlobRef{Table: storecontract.BlobTableCAS, Digest: strings.Repeat("a", 64)}
	for _, body := range []string{"entry", "other"} {
		if err := store.PutBlob(ctx, "cas-key", ref, io.MultiReader(strings.NewReader(body)), 5); err != nil {
			t.Fatalf("PutBlob: %v", err)
		}
	}
	blob, err := store.ReadBlob(ctx, "cas-key", ref)
	if err != nil {
		t.Fatalf("ReadBlob: %v", err)
	}
	got, err = io.ReadAll(blob)
	if closeErr := blob.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || string(got) != "entry" {
		t.Fatalf("the entry = %q, %v", got, err)
	}
	absent := storecontract.BlobRef{Table: storecontract.BlobTableCAS, Digest: strings.Repeat("b", 64)}
	if _, err := store.ReadBlob(ctx, "cas-key", absent); !errors.Is(err, storecontract.ErrMiss) {
		t.Fatalf("an absent entry = %v, want ErrMiss", err)
	}
	refs, err := store.ListBlobs(ctx, "cas-key")
	if err != nil {
		t.Fatalf("ListBlobs: %v", err)
	}
	if _, ok := refs[ref]; !ok || len(refs) != 1 {
		t.Fatalf("ListBlobs = %v", refs)
	}
	if err := store.RecordBlobUse(ctx, "cas-key", "job", []storecontract.BlobRef{ref}); err != nil {
		t.Fatalf("RecordBlobUse: %v", err)
	}

	// EVICTION'S OWN CALLS, against what the role holds, with nothing old enough to go.
	if err := store.evictObjects(ctx, 7*24*time.Hour); err != nil {
		t.Fatalf("evictObjects: %v", err)
	}
	if refs, err := store.ListBlobs(ctx, "cas-key"); err != nil || len(refs) != 1 {
		t.Fatalf("eviction took a recent entry: %v, %v", refs, err)
	}
	if exists, err := store.ArchiveExists(ctx, key); err != nil || !exists {
		t.Fatalf("eviction took a recent archive: %v, %v", exists, err)
	}
}
