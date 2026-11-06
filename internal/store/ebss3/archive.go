package ebss3

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

const (
	// partMinimum is the smallest part S3 takes, except the last of an upload.
	partMinimum = 5 << 20
	// partBuffer is what a part is built up to when blocks are too small to copy
	// as they are, and so the most one commit holds in memory.
	partBuffer = 64 << 20
	// partCopies is how many parts one commit copies at once.
	partCopies = 8
	// maxStageBlock is the largest block a client may name, Azure's own limit on a
	// block, which is below S3's five gibibytes on a part.
	maxStageBlock = 4000 << 20
	// maxObjectKey is S3's limit on a key.
	maxObjectKey = 1024

	// readChunk and readWindow shape reading one archive: S3 gave one stream about
	// 90 MiB/s and four 100 to 380 (2026-11-04, c7i.2xlarge through a gateway
	// endpoint), and the client asks on one connection, so the node fetches ahead.
	readChunk  = 8 << 20
	readWindow = 4
	// readAheadReaders is how many reads at once fetch ahead, each holding up to
	// readWindow chunks plus one. A node takes 128 connections, and a job can open as
	// many reads of a signed URL as it likes and stop reading them, so without this a
	// node a size that suits an orchestrator could be run out of memory by one job. A
	// read past this is one plain stream, which holds nothing, and which is slower and
	// does not try a broken body again: that restore is a miss.
	readAheadReaders = 16
	// rejoinsAtOnce is how many commits at once read small blocks back, each holding a
	// partBuffer, because a job can finalize as many saves as it has reserved. The rest
	// wait, as a commit's client already does.
	rejoinsAtOnce = 4

	// staleUpload is how old a staged block or an unfinished multipart upload must
	// be before eviction takes it: longer than any job, or its finalization could
	// find its blocks gone.
	staleUpload = 48 * time.Hour

	// blobUseLimit bounds reading one use record, a list of digests.
	blobUseLimit = 64 << 20
	// blobUseEntries is how many entries one record names, at under 80 bytes each, so
	// a record billet wrote is never past blobUseLimit.
	blobUseEntries = 500_000
)

// errNoBulk is a store built with no way to reach whole objects, which only a test
// of the state half does.
var errNoBulk = errors.New("ebs-s3: this store cannot reach whole objects")

var (
	_ storecontract.ArchiveStore = (*Store)(nil)
	_ storecontract.BlobStore    = (*Store)(nil)
)

func (s Store) ownerPrefix() string {
	return strings.TrimSuffix(s.statePrefix(), "state/")
}

func keyDigest(key string) string {
	digest := sha256.Sum256([]byte(key))

	return hex.EncodeToString(digest[:])
}

// archiveName is a cache key as an object name. PER BYTE AND SELF-DELIMITING, so a
// restore prefix of a key is a prefix of its name, which is what lets S3's own
// listing answer a restore key.
func archiveName(key string) string {
	var b strings.Builder
	for _, c := range []byte(key) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '/':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}

	return b.String()
}

func (s Store) archiveObject(key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	name := s.ownerPrefix() + "archives/" + archiveName(key)
	if len(name) > maxObjectKey {
		return "", fmt.Errorf("ebs-s3: cache key %q is too long to be an object name", key)
	}

	return name, nil
}

func (s Store) archiveUsedObject(key string) string {
	return s.ownerPrefix() + "archive-used/" + keyDigest(key)
}

func (s Store) stagePrefix(upload string) string {
	return s.ownerPrefix() + "stage/" + upload + "/"
}

func (s Store) stageObject(upload, block string) string {
	return s.stagePrefix(upload) + keyDigest(block)
}

func (s Store) blobPrefix(key string) string {
	return s.ownerPrefix() + "blobs/" + keyDigest(key) + "/"
}

func (s Store) blobObject(key string, ref storecontract.BlobRef) string {
	return s.blobPrefix(key) + string(ref.Table) + "/" + ref.Digest
}

func (s Store) blobUseObject(key, id string) string {
	return s.ownerPrefix() + "blob-uses/" + keyDigest(key) + "/" + keyDigest(id) + ".gz"
}

func checkUpload(upload string) error {
	if upload == "" || strings.ContainsAny(upload, "/\x00") {
		return errors.New("ebs-s3: an upload needs a name of its own")
	}

	return nil
}

// listedArchive is the object at exactly this name, found by listing because S3
// answers a listing with an error code a HEAD has no body to carry, and a HEAD of an
// absent object is a 403 under the prefix-conditioned grant the node holds.
func (s Store) listedArchive(ctx context.Context, name string) (objectInfo, bool, error) {
	objects, err := s.bulk.ListObjects(ctx, name)
	if err != nil {
		return objectInfo{}, false, err
	}
	for _, object := range objects {
		if object.Key == name {
			return object, true, nil
		}
	}

	return objectInfo{}, false, nil
}

// ArchiveExists reports whether a key is published.
func (s Store) ArchiveExists(ctx context.Context, key string) (bool, error) {
	if s.bulk == nil {
		return false, errNoBulk
	}
	name, err := s.archiveObject(key)
	if err != nil {
		return false, err
	}
	_, found, err := s.listedArchive(ctx, name)

	return found, err
}

// MatchArchive finds the exact key, or the newest archive under the first restore
// prefix that has one, and marks what it found used.
//
// MARKED AT LOOKUP, BECAUSE A READ LEAVES NO TRACE ON AN OBJECT, and a marker older
// than the retention is what eviction takes an archive on. The retention is days and
// a job is hours, so a download in progress cannot lose its archive, and no lease is
// needed to say so. A marker that cannot be written costs the archive at most one
// early eviction, so it fails nothing.
func (s Store) MatchArchive(
	ctx context.Context, exact string, restorePrefixes []string,
) (storecontract.Archive, error) {
	if s.bulk == nil {
		return storecontract.Archive{}, errNoBulk
	}
	name, err := s.archiveObject(exact)
	if err != nil {
		return storecontract.Archive{}, err
	}
	object, found, err := s.listedArchive(ctx, name)
	if err != nil {
		return storecontract.Archive{}, err
	}
	matched := exact
	for _, prefix := range restorePrefixes {
		if found {
			break
		}
		named, err := s.archiveObject(prefix)
		if err != nil {
			return storecontract.Archive{}, err
		}
		objects, err := s.bulk.ListObjects(ctx, named)
		if err != nil {
			return storecontract.Archive{}, err
		}
		// THE NEWEST, AND THE SMALLEST NAME ON A TIE, as Match decides between
		// generations, so one lookup answers the same way twice.
		for _, candidate := range objects {
			if !found || candidate.Modified.After(object.Modified) ||
				candidate.Modified.Equal(object.Modified) && candidate.Key < object.Key {
				object, found = candidate, true
			}
		}
		if found {
			matched, err = s.archiveKeyOf(prefix, named, object.Key)
			if err != nil {
				return storecontract.Archive{}, err
			}
		}
	}
	if !found {
		return storecontract.Archive{}, fmt.Errorf("%w: site has no archive matching cache %q",
			storecontract.ErrMiss, exact)
	}
	if err := s.bulk.PutStream(ctx, s.archiveUsedObject(matched), http.NoBody, 0, false); err != nil {
		slog.Default().Warn("ebs-s3: could not mark an archive used; it may be evicted early",
			"key", matched, "error", err)
	}

	return storecontract.Archive{Key: matched, Size: object.Size, Modified: object.Modified, ETag: object.ETag}, nil
}

// archiveKeyOf is the cache key an object under a restore prefix holds.
func (s Store) archiveKeyOf(prefix, namedPrefix, object string) (string, error) {
	rest, ok := strings.CutPrefix(object, namedPrefix)
	if !ok {
		return "", fmt.Errorf("ebs-s3: listing under %q returned %q", namedPrefix, object)
	}
	decoded, err := unescapeArchiveName(rest)
	if err != nil {
		return "", fmt.Errorf("ebs-s3: object %q is not a cache key: %w", object, err)
	}

	return prefix + decoded, nil
}

// unescapeArchiveName undoes archiveName.
func unescapeArchiveName(name string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] != '%' {
			b.WriteByte(name[i])

			continue
		}
		if i+2 >= len(name) {
			return "", errors.New("a truncated escape")
		}
		value, err := strconv.ParseUint(name[i+1:i+3], 16, 8)
		if err != nil {
			return "", errors.New("an escape that is not hex")
		}
		b.WriteByte(byte(value))
		i += 2
	}

	return b.String(), nil
}

// StageArchive stores one block of an upload.
func (s Store) StageArchive(
	ctx context.Context, upload, block string, body io.Reader, size int64,
) error {
	if s.bulk == nil {
		return errNoBulk
	}
	if err := checkUpload(upload); err != nil {
		return err
	}
	if size < 0 || size > maxStageBlock {
		return fmt.Errorf("ebs-s3: a block of %d bytes is outside what a block may be", size)
	}

	return s.bulk.PutStream(ctx, s.stageObject(upload, block), body, size, false)
}

// CommitArchive publishes an upload's blocks as key.
func (s Store) CommitArchive(
	ctx context.Context, key, upload string, blocks []string, size int64,
) (bool, error) {
	if s.bulk == nil {
		return false, errNoBulk
	}
	if err := checkUpload(upload); err != nil {
		return false, err
	}
	name, err := s.archiveObject(key)
	if err != nil {
		return false, err
	}

	// ASKED FIRST, because a commit is retried after a lost answer, and by then this
	// upload's own blocks may be gone: the archive at the key is what says.
	if ours, found, err := s.archiveOwnedBy(ctx, name, upload); err != nil {
		return false, err
	} else if found {
		if err := s.AbortArchive(ctx, upload); err != nil {
			slog.Default().Warn("ebs-s3: could not release a committed upload's blocks; eviction will",
				"upload", upload, "error", err)
		}

		return ours, nil
	}

	if size <= 0 {
		return false, fmt.Errorf("%w: an archive of %d bytes", storecontract.ErrArchiveSize, size)
	}
	objects, err := s.bulk.ListObjects(ctx, s.stagePrefix(upload))
	if err != nil {
		return false, err
	}
	sizes := make(map[string]int64, len(objects))
	for _, object := range objects {
		sizes[object.Key] = object.Size
	}
	sources := make([]objectInfo, 0, len(blocks))
	var total int64
	for _, block := range blocks {
		source := s.stageObject(upload, block)
		length, ok := sizes[source]
		if !ok {
			return false, fmt.Errorf("%w: block %q was never staged", storecontract.ErrArchiveSize, block)
		}
		sources = append(sources, objectInfo{Key: source, Size: length})
		total += length
	}
	if total != size {
		return false, fmt.Errorf("%w: the blocks total %d bytes and the client declared %d",
			storecontract.ErrArchiveSize, total, size)
	}

	uploadID, err := s.bulk.CreateMultipart(ctx, name, upload)
	if err != nil {
		return false, err
	}
	abandon := func(cause error) error {
		// DETACHED: the commit may have failed because its context ended, and an
		// upload left behind bills until eviction finds it.
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()

		return errors.Join(cause, s.bulk.AbortMultipart(abortCtx, name, uploadID))
	}
	parts, err := s.assemble(ctx, name, uploadID, sources)
	if err != nil {
		return false, abandon(err)
	}
	err = s.bulk.CompleteMultipart(ctx, name, uploadID, parts)
	switch {
	case err == nil:
	case errors.Is(err, errObjectConflict), errors.Is(err, errObjectAmbiguous):
		// EITHER ANOTHER UPLOAD WON OR THIS ONE'S ANSWER WAS LOST, and the object
		// says which.
		ours, found, askErr := s.archiveOwnedBy(ctx, name, upload)
		if askErr != nil {
			return false, abandon(errors.Join(err, askErr))
		}
		if !found {
			return false, abandon(err)
		}
		// WHOSEVER IT IS, THIS UPLOAD IS OVER: one retried finalization beside another
		// holds an upload of its own, which bills until eviction finds it, and aborting
		// one that landed answers that it is gone.
		if abortErr := abandon(nil); abortErr != nil {
			slog.Default().Warn("ebs-s3: could not abort a commit that did not land",
				"upload", upload, "error", abortErr)
		}
		if releaseErr := s.AbortArchive(ctx, upload); releaseErr != nil {
			slog.Default().Warn("ebs-s3: could not release a committed upload's blocks; eviction will",
				"upload", upload, "error", releaseErr)
		}

		return ours, nil
	default:
		return false, abandon(err)
	}
	if err := s.AbortArchive(ctx, upload); err != nil {
		slog.Default().Warn("ebs-s3: could not release a committed upload's blocks; eviction will",
			"upload", upload, "error", err)
	}

	return true, nil
}

// archiveOwnedBy answers whether upload committed the archive at name, then whether
// an archive is there at all.
func (s Store) archiveOwnedBy(ctx context.Context, name, upload string) (bool, bool, error) {
	_, found, err := s.listedArchive(ctx, name)
	if err != nil || !found {
		return false, false, err
	}
	_, owner, err := s.bulk.Head(ctx, name)
	if err != nil {
		return false, false, err
	}

	return owner == upload, true, nil
}

// assemble builds an upload's parts from its staged blocks, in the client's order.
//
// COPIED INSIDE S3 WHERE EVERY BLOCK MAY BE A PART, so an archive of any size moves
// no bytes through the node. A block below S3's part minimum anywhere but last cannot
// be one, so such an upload is read back and rejoined through a bounded buffer: slower,
// and the one path that costs the node memory and transfer.
func (s Store) assemble(
	ctx context.Context, name, uploadID string, sources []objectInfo,
) ([]completedPart, error) {
	for i, source := range sources {
		if source.Size < partMinimum && i != len(sources)-1 {
			return s.rejoin(ctx, name, uploadID, sources)
		}
	}

	parts := make([]completedPart, len(sources))
	failures := make([]error, len(sources))
	slots := make(chan struct{}, partCopies)
	var wg sync.WaitGroup
	for i, source := range sources {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			parts[i], failures[i] = s.bulk.CopyPart(ctx, name, uploadID, i+1, source.Key)
		}()
	}
	wg.Wait()

	if err := errors.Join(failures...); err != nil {
		return nil, err
	}

	return parts, nil
}

// rejoin reads blocks back and sends them on as parts of partBuffer bytes.
func (s Store) rejoin(
	ctx context.Context, name, uploadID string, sources []objectInfo,
) ([]completedPart, error) {
	select {
	case s.rejoining <- struct{}{}:
		defer func() { <-s.rejoining }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	var parts []completedPart
	buffer := make([]byte, 0, partBuffer)
	flush := func() error {
		part, err := s.bulk.UploadPart(ctx, name, uploadID, len(parts)+1, buffer)
		if err != nil {
			return err
		}
		parts = append(parts, part)
		buffer = buffer[:0]

		return nil
	}
	for _, source := range sources {
		body, err := s.bulk.GetAll(ctx, source.Key)
		if err != nil {
			return nil, err
		}
		var read int64
		for {
			count, readErr := io.ReadFull(body, buffer[len(buffer):cap(buffer)])
			buffer = buffer[:len(buffer)+count]
			read += int64(count)
			if len(buffer) == cap(buffer) {
				if err := flush(); err != nil {
					_ = body.Close()

					return nil, err
				}
			}
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				break
			}
			if readErr != nil {
				_ = body.Close()

				return nil, readErr
			}
		}
		if err := body.Close(); err != nil {
			return nil, err
		}
		// io.ErrUnexpectedEOF ENDS A BLOCK SHORTER THAN THE BUFFER AND A BODY CUT SHORT
		// ALIKE, so the length is what tells them apart. A short archive published here
		// would stay: the first writer holds the key, and each failed restore refreshes
		// its marker.
		if read != source.Size {
			return nil, fmt.Errorf("ebs-s3: a staged block read back %d bytes of %d", read, source.Size)
		}
	}
	if len(buffer) > 0 {
		if err := flush(); err != nil {
			return nil, err
		}
	}

	return parts, nil
}

// AbortArchive deletes what an upload staged.
func (s Store) AbortArchive(ctx context.Context, upload string) error {
	if s.bulk == nil {
		return errNoBulk
	}
	if err := checkUpload(upload); err != nil {
		return err
	}
	objects, err := s.bulk.ListObjects(ctx, s.stagePrefix(upload))
	if err != nil {
		return err
	}
	var failures []error
	for _, object := range objects {
		failures = append(failures, s.objects.Delete(ctx, object.Key))
	}

	return errors.Join(failures...)
}

// ReadArchive reads length bytes of an archive from start, fetching ahead.
func (s Store) ReadArchive(
	ctx context.Context, archive storecontract.Archive, start, length int64,
) (io.ReadCloser, error) {
	if s.bulk == nil {
		return nil, errNoBulk
	}
	name, err := s.archiveObject(archive.Key)
	if err != nil {
		return nil, err
	}
	if start < 0 || length < 0 || start+length > archive.Size {
		return nil, fmt.Errorf("ebs-s3: %d bytes from %d is outside an archive of %d",
			length, start, archive.Size)
	}
	if length == 0 {
		return io.NopCloser(strings.NewReader("")), nil
	}
	select {
	case s.readAhead <- struct{}{}:
		return newRangeReader(ctx, s.bulk, name, archive.ETag, start, length, func() { <-s.readAhead }), nil
	default:
		return s.bulk.GetRange(ctx, name, archive.ETag, start, length)
	}
}

// rangeReader reads an object in order while fetching several chunks of it at once.
// It holds at most readWindow chunks plus the one being read.
type rangeReader struct {
	cancel  context.CancelFunc
	results chan chan rangeChunk
	current []byte
	err     error
	wg      sync.WaitGroup
	release func()
	closing sync.Once
}

type rangeChunk struct {
	body []byte
	err  error
}

func newRangeReader(
	ctx context.Context, bulk bulkAPI, name, etag string, start, length int64, release func(),
) *rangeReader {
	ctx, cancel := context.WithCancel(ctx)
	r := &rangeReader{cancel: cancel, results: make(chan chan rangeChunk, readWindow), release: release}
	r.wg.Go(func() {
		defer close(r.results)
		for offset := start; offset < start+length; offset += readChunk {
			size := min(readChunk, start+length-offset)
			done := make(chan rangeChunk, 1)
			select {
			case r.results <- done:
			case <-ctx.Done():
				return
			}
			r.wg.Go(func() { done <- fetchChunk(ctx, bulk, name, etag, offset, size) })
		}
	})

	return r
}

// fetchChunk reads one chunk whole, trying again if the body breaks partway: a broken
// body is the transfer's failure and a fresh range costs one request.
func fetchChunk(ctx context.Context, bulk bulkAPI, name, etag string, offset, size int64) rangeChunk {
	buffer := make([]byte, size)
	_, err := idempotent(ctx, func() (struct{}, error) {
		body, err := bulk.GetRange(ctx, name, etag, offset, size)
		if err != nil {
			return struct{}{}, err
		}
		defer body.Close()
		_, err = io.ReadFull(body, buffer)

		return struct{}{}, err
	})

	return rangeChunk{body: buffer, err: err}
}

func (r *rangeReader) Read(p []byte) (int, error) {
	for len(r.current) == 0 {
		if r.err != nil {
			return 0, r.err
		}
		done, ok := <-r.results
		if !ok {
			r.err = io.EOF

			return 0, io.EOF
		}
		chunk := <-done
		if chunk.err != nil {
			r.err = chunk.err

			return 0, r.err
		}
		r.current = chunk.body
	}
	count := copy(p, r.current)
	r.current = r.current[count:]

	return count, nil
}

// Close ends the fetching and waits for it.
func (r *rangeReader) Close() error {
	r.closing.Do(func() {
		r.cancel()
		r.wg.Wait()
		r.release()
	})

	return nil
}

// ListBlobs is every entry a key holds, with its size.
func (s Store) ListBlobs(ctx context.Context, key string) (map[storecontract.BlobRef]int64, error) {
	if s.bulk == nil {
		return nil, errNoBulk
	}
	prefix := s.blobPrefix(key)
	objects, err := s.bulk.ListObjects(ctx, prefix)
	if err != nil {
		return nil, err
	}
	refs := make(map[storecontract.BlobRef]int64, len(objects))
	for _, object := range objects {
		if ref, ok := parseBlobRef(strings.TrimPrefix(object.Key, prefix)); ok {
			refs[ref] = object.Size
		}
	}

	return refs, nil
}

func parseBlobRef(rest string) (storecontract.BlobRef, bool) {
	table, digest, ok := strings.Cut(rest, "/")
	if !ok || digest == "" ||
		table != string(storecontract.BlobTableAC) && table != string(storecontract.BlobTableCAS) {
		return storecontract.BlobRef{}, false
	}

	return storecontract.BlobRef{Table: storecontract.BlobTable(table), Digest: digest}, true
}

// ReadBlob reads one entry.
func (s Store) ReadBlob(
	ctx context.Context, key string, ref storecontract.BlobRef,
) (io.ReadCloser, error) {
	if s.bulk == nil {
		return nil, errNoBulk
	}
	body, err := s.bulk.GetAll(ctx, s.blobObject(key, ref))
	if errors.Is(err, errObjectAbsent) {
		return nil, fmt.Errorf("%w: cache %q has no %s %s", storecontract.ErrMiss, key, ref.Table, ref.Digest)
	}

	return body, err
}

// PutBlob stores one entry unless it is there already: an object's name is its
// content, and of two action results either is a valid answer.
func (s Store) PutBlob(
	ctx context.Context, key string, ref storecontract.BlobRef, body io.Reader, size int64,
) error {
	if s.bulk == nil {
		return errNoBulk
	}
	err := s.bulk.PutStream(ctx, s.blobObject(key, ref), body, size, true)
	if errors.Is(err, errObjectConflict) {
		return nil
	}

	return err
}

// RecordBlobUse writes the entries one job used, compressed, in objects of at most
// blobUseEntries, so no record reaches the limit a reader treats as a failure.
func (s Store) RecordBlobUse(
	ctx context.Context, key, id string, used []storecontract.BlobRef,
) error {
	if s.bulk == nil {
		return errNoBulk
	}
	piece := 0
	for start := 0; start < len(used) || piece == 0; start += blobUseEntries {
		name := id
		if piece > 0 {
			name = id + "#" + strconv.Itoa(piece)
		}
		if err := s.recordBlobUses(ctx, key, name, used[start:min(start+blobUseEntries, len(used))]); err != nil {
			return err
		}
		piece++
	}

	return nil
}

func (s Store) recordBlobUses(
	ctx context.Context, key, id string, used []storecontract.BlobRef,
) error {
	var encoded strings.Builder
	writer := gzip.NewWriter(&encoded)
	lines := bufio.NewWriter(writer)
	for _, ref := range used {
		if _, err := lines.WriteString(string(ref.Table) + "/" + ref.Digest + "\n"); err != nil {
			return fmt.Errorf("ebs-s3: encode a use record: %w", err)
		}
	}
	if err := errors.Join(lines.Flush(), writer.Close()); err != nil {
		return fmt.Errorf("ebs-s3: encode a use record: %w", err)
	}
	body := encoded.String()

	return s.bulk.PutStream(ctx, s.blobUseObject(key, id), strings.NewReader(body), int64(len(body)), false)
}

// evictObjects takes what the object half of the store no longer needs. Called at
// the end of Evict, whose failures it joins.
func (s Store) evictObjects(ctx context.Context, olderThan time.Duration) error {
	if s.bulk == nil {
		return nil
	}

	return errors.Join(
		s.evictArchives(ctx, olderThan),
		s.evictBlobs(ctx, olderThan),
		s.evictUploads(ctx),
	)
}

// evictArchives takes an archive neither written nor looked up within the retention.
func (s Store) evictArchives(ctx context.Context, olderThan time.Duration) error {
	prefix := s.ownerPrefix() + "archives/"
	archives, err := s.bulk.ListObjects(ctx, prefix)
	if err != nil {
		return err
	}
	if len(archives) == 0 {
		return nil
	}
	usedPrefix := s.ownerPrefix() + "archive-used/"
	markers, err := s.bulk.ListObjects(ctx, usedPrefix)
	if err != nil {
		return err
	}
	used := make(map[string]time.Time, len(markers))
	for _, marker := range markers {
		used[strings.TrimPrefix(marker.Key, usedPrefix)] = marker.Modified
	}
	now := s.now()
	var failures []error
	for _, archive := range archives {
		key, err := unescapeArchiveName(strings.TrimPrefix(archive.Key, prefix))
		if err != nil {
			failures = append(failures, fmt.Errorf("ebs-s3: object %q is not a cache key: %w", archive.Key, err))

			continue
		}
		digest := keyDigest(key)
		last := archive.Modified
		if marked, ok := used[digest]; ok && marked.After(last) {
			last = marked
		}
		if now.Sub(last) < olderThan {
			continue
		}
		// THE ARCHIVE FIRST: a marker left behind by a failure here costs nothing,
		// and an archive with no marker reads as older than it is.
		if err := s.objects.Delete(ctx, archive.Key); err != nil {
			failures = append(failures, err)

			continue
		}
		if _, ok := used[digest]; ok {
			failures = append(failures, s.objects.Delete(ctx, usedPrefix+digest))
		}
	}

	return errors.Join(failures...)
}

// evictBlobs takes an entry older than the retention that no job within it recorded
// using. A mark and a sweep, because a read leaves no trace on an object and a write
// per read would cost more than the object is worth.
func (s Store) evictBlobs(ctx context.Context, olderThan time.Duration) error {
	usePrefix := s.ownerPrefix() + "blob-uses/"
	uses, err := s.bulk.ListObjects(ctx, usePrefix)
	if err != nil {
		return err
	}
	now := s.now()
	live := make(map[string]struct{})
	var expired []string
	for _, use := range uses {
		keyPart, _, ok := strings.Cut(strings.TrimPrefix(use.Key, usePrefix), "/")
		if !ok {
			continue
		}
		if now.Sub(use.Modified) >= olderThan {
			expired = append(expired, use.Key)

			continue
		}
		// A RECORD THAT CANNOT BE READ STOPS THE SWEEP: what it names cannot be told
		// from what nobody uses, and taking it is the one way this pass can do harm.
		if err := s.markBlobUse(ctx, use.Key, keyPart, live); err != nil {
			return fmt.Errorf("ebs-s3: read a use record: %w", err)
		}
	}

	blobPrefix := s.ownerPrefix() + "blobs/"
	blobs, err := s.bulk.ListObjects(ctx, blobPrefix)
	if err != nil {
		return err
	}
	var failures []error
	for _, blob := range blobs {
		if now.Sub(blob.Modified) < olderThan {
			continue
		}
		if _, ok := live[strings.TrimPrefix(blob.Key, blobPrefix)]; ok {
			continue
		}
		failures = append(failures, s.objects.Delete(ctx, blob.Key))
	}
	for _, record := range expired {
		failures = append(failures, s.objects.Delete(ctx, record))
	}

	return errors.Join(failures...)
}

func (s Store) markBlobUse(ctx context.Context, object, keyPart string, live map[string]struct{}) error {
	body, err := s.bulk.GetAll(ctx, object)
	if errors.Is(err, errObjectAbsent) {
		return nil
	}
	if err != nil {
		return err
	}
	defer body.Close()
	reader, err := gzip.NewReader(body)
	if err != nil {
		return err
	}
	limited := &io.LimitedReader{R: reader, N: blobUseLimit}
	lines := bufio.NewScanner(limited)
	for lines.Scan() {
		live[keyPart+"/"+lines.Text()] = struct{}{}
	}
	if err := lines.Err(); err != nil {
		return err
	}
	// NOT THE END OF THE RECORD, ONLY OF WHAT WAS READ, and what it did not reach would
	// read as unused.
	if limited.N <= 0 {
		return fmt.Errorf("a use record is larger than %d bytes", blobUseLimit)
	}

	return nil
}

// evictUploads takes staged blocks and unfinished multipart uploads no job can still
// be finishing.
func (s Store) evictUploads(ctx context.Context) error {
	now := s.now()
	staged, err := s.bulk.ListObjects(ctx, s.ownerPrefix()+"stage/")
	if err != nil {
		return err
	}
	var failures []error
	for _, block := range staged {
		if now.Sub(block.Modified) >= staleUpload {
			failures = append(failures, s.objects.Delete(ctx, block.Key))
		}
	}
	uploads, err := s.bulk.ListMultipart(ctx, s.ownerPrefix())
	if err != nil {
		return errors.Join(append(failures, err)...)
	}
	for _, upload := range uploads {
		if now.Sub(upload.Initiated) >= staleUpload {
			failures = append(failures, s.bulk.AbortMultipart(ctx, upload.Key, upload.UploadID))
		}
	}

	return errors.Join(failures...)
}

// purgeObjects removes every whole object and unfinished upload this owner holds,
// leaving the state index to the caller's last step.
func (s Store) purgeObjects(ctx context.Context) (int, error) {
	if s.bulk == nil {
		return 0, nil
	}
	objects, err := s.bulk.ListObjects(ctx, s.ownerPrefix())
	if err != nil {
		return 0, err
	}
	count := 0
	for _, object := range objects {
		if strings.HasPrefix(object.Key, s.statePrefix()) {
			continue
		}
		if err := s.objects.Delete(ctx, object.Key); err != nil {
			return count, err
		}
		count++
	}
	uploads, err := s.bulk.ListMultipart(ctx, s.ownerPrefix())
	if err != nil {
		return count, err
	}
	for _, upload := range uploads {
		if err := s.bulk.AbortMultipart(ctx, upload.Key, upload.UploadID); err != nil {
			return count, err
		}
		count++
	}

	return count, nil
}
