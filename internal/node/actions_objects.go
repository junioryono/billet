package node

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"

	storecontract "github.com/junioryono/billet/internal/store"
)

// THE ACTIONS CACHE ON A STORE THAT KEEPS WHOLE OBJECTS (ADR-023). The three calls,
// the signed URLs, the kill switch and the fail-open rule are the same as on a store
// of volumes; what differs is only where an archive's bytes are. Nothing is mounted,
// so a block goes straight to the store, a commit assembles them inside it, and a
// download reads from it through this listener.

// actionsObjectArchives is how many archives one job may hold at once on such a
// store. The limit on a store of volumes is what a volume per archive costs; here an
// archive costs one entry of the session's record, and BuildKit's `mode=max` writes
// one per layer.
const actionsObjectArchives = 512

// archiveStore is the site's store, if it keeps whole objects.
func (s *CacheService) archiveStore() storecontract.ArchiveStore {
	store, ok := s.store.(storecontract.ArchiveStore)
	if !ok {
		return nil
	}

	return store
}

// actionsArchiveCeiling is how many archives one job may hold at once.
func (s *CacheService) actionsArchiveCeiling() int {
	if s.archiveStore() != nil {
		return actionsObjectArchives
	}

	return actionsArchiveCount
}

func (a *actionsArchive) objectBacked() bool { return a.Upload != "" || a.Object != nil }

// createObjectArchive reserves an upload. Called with session.mu held. The store's
// conditional commit is what decides a race between two jobs; this only spares a job
// that would lose it the upload.
func (s *CacheService) createObjectArchive(
	ctx context.Context,
	store storecontract.ArchiveStore,
	session *cacheSession,
	storeKey, cacheKey, version, origin string,
) (*http.Response, error) {
	exists, err := store.ArchiveExists(ctx, storeKey)
	if err != nil {
		return nil, err
	}
	if exists {
		return actionsJSONResponse(map[string]any{
			"ok": false, "message": "a cache entry already exists for this key and version",
		})
	}
	id, err := randomCacheToken()
	if err != nil {
		return nil, err
	}
	signature, err := randomCacheToken()
	if err != nil {
		return nil, err
	}
	archive := &actionsArchive{
		ID: id, Signature: signature, Mode: actionsModeUpload, CacheKey: cacheKey,
		StoreKey: storeKey, Version: version, Upload: id,
	}
	session.actions[id] = archive
	if err := s.persistSession(session); err != nil {
		delete(session.actions, id)

		return nil, err
	}

	return actionsJSONResponse(map[string]any{
		"ok": true, "signed_upload_url": s.actionsSignedURL(archive, origin),
	})
}

// objectBlockRefused is the answer to a block the store cannot take, or nil. Azure's
// client always says how long a block is, and S3 will not take one that does not, so
// an upload that does not is refused rather than spooled.
func objectBlockRefused(req *http.Request) *http.Response {
	if req.ContentLength < 0 {
		return actionsBlobError(http.StatusLengthRequired, "a block must say how long it is")
	}
	if req.ContentLength > actionsArchiveLimit {
		return actionsBlobError(http.StatusRequestEntityTooLarge, "actions cache archive exceeds the site limit")
	}

	return nil
}

// stageObjectBlock sends one block of a guest's upload to the store as it arrives.
func (s *CacheService) stageObjectBlock(
	req *http.Request, archive *actionsArchive, block string, body io.Reader,
) error {
	return s.archiveStore().StageArchive(req.Context(), archive.Upload, block, body, req.ContentLength)
}

// finalizeObjectArchive turns a committed upload into a receipt and publishes it.
// Called with session.mu and the archive held.
func (s *CacheService) finalizeObjectArchive(
	ctx context.Context, session *cacheSession, archive *actionsArchive, size int64,
) (*http.Response, error) {
	blocks := archive.committed.Load()
	if blocks == nil {
		// HELD IN MEMORY, between the block list and the finalization a moment after
		// it, because the list arrives under the archive's lock and the session's
		// record is written under the session's. A restart in that moment fails this
		// one save, which the client reports as a warning.
		return nil, errors.New("this archive's block list was never committed in this process")
	}
	digest := sha256.Sum256([]byte(archive.StoreKey + "\x00" + archive.Upload))
	entryNumber := binary.BigEndian.Uint64(digest[:8]) & math.MaxInt64
	if entryNumber == 0 {
		entryNumber = 1
	}
	receipt := &actionsReceipt{
		ID: actionsReceiptID(archive.CacheKey, archive.Version), CacheKey: archive.CacheKey,
		StoreKey: archive.StoreKey, Version: archive.Version, Upload: archive.Upload, Blocks: *blocks,
		EntryID: strconv.FormatUint(entryNumber, 10), Size: size,
	}
	delete(session.actions, archive.ID)
	session.receipts[receipt.ID] = receipt
	if err := s.persistSession(session); err != nil {
		delete(session.receipts, receipt.ID)
		session.actions[archive.ID] = archive

		return nil, err
	}

	return s.publishObjectReceipt(ctx, session, receipt)
}

// publishObjectReceipt commits a receipt's upload. Called with session.mu held, and
// returns with it held. The receipt is durable before the commit and complete before
// success is returned, as a volume's is, and a commit asks first whether its own archive
// is already there.
//
// ONE COMMIT OF A RECEIPT AT A TIME, because the first to land deletes the blocks a second
// is about to list, and the second would then report that nothing was published, over an
// answer that said it was. A second finalization waits and reads the first's answer.
// session.mu is never held while waiting for this lock, so nothing can wait on it in turn.
func (s *CacheService) publishObjectReceipt(
	ctx context.Context, session *cacheSession, receipt *actionsReceipt,
) (*http.Response, error) {
	if receipt.commit == nil {
		receipt.commit = new(sync.Mutex)
	}
	commit := receipt.commit
	session.mu.Unlock()
	commit.Lock()
	defer commit.Unlock()
	session.mu.Lock()
	if receipt.Complete {
		return actionsReceiptResponse(receipt)
	}

	published, err := s.commitReceipt(ctx, session, receipt)
	if err != nil {
		return nil, err
	}
	receipt.Complete, receipt.OK = true, published
	// A SESSION FINISHED MEANWHILE HAS NO RECORD, and its client still gets its answer.
	if !session.finished {
		if err := s.persistSession(session); err != nil {
			return nil, err
		}
	}

	return actionsReceiptResponse(receipt)
}

// commitReceipt runs the store's commit outside session.mu, which it holds on return
// whatever happens: small blocks are read back through the node, which takes minutes, and
// the rest of the job's cache calls and every later session's renewal would queue behind
// it.
func (s *CacheService) commitReceipt(
	ctx context.Context, session *cacheSession, receipt *actionsReceipt,
) (bool, error) {
	session.mu.Unlock()
	defer session.mu.Lock()

	published, err := s.archiveStore().CommitArchive(ctx, receipt.StoreKey, receipt.Upload,
		receipt.Blocks, receipt.Size)
	if errors.Is(err, storecontract.ErrArchiveSize) {
		// THE CLIENT'S OWN ACCOUNT OF THE UPLOAD DOES NOT ADD UP, which a retry will
		// not change. Nothing was published, and it is told so.
		s.log.Warn("an Actions cache save did not match what it uploaded; nothing was published",
			"instance", session.instance, "key", receipt.CacheKey, "error", err)
		s.releaseStagedBlocks(ctx, session, receipt.Upload)

		return false, nil
	}

	return published, err
}

// releaseStagedBlocks deletes what an upload staged that will never be committed. A
// failure costs a day or two of storage until eviction takes them, which is no reason
// to keep a finished job's session open.
func (s *CacheService) releaseStagedBlocks(ctx context.Context, session *cacheSession, upload string) {
	if err := s.archiveStore().AbortArchive(ctx, upload); err != nil {
		s.log.Warn("could not release an Actions cache save's staged blocks; eviction will",
			"instance", session.instance, "error", err)
	}
}

// findObjectArchive answers a lookup from the store. Called with session.mu held.
func (s *CacheService) findObjectArchive(
	ctx context.Context,
	store storecontract.ArchiveStore,
	session *cacheSession,
	keys actionsKeys,
	key string,
	restoreKeys []string,
	version, origin string,
) (*http.Response, error) {
	// THE FIRST SCOPE THAT HAS A MATCH WINS, exact key before restore keys within
	// it, which is GitHub's own order: a job's own ref, its base, the default.
	var (
		prefix string
		found  storecontract.Archive
	)
	for _, candidate := range keys.read(version) {
		restore := make([]string, len(restoreKeys))
		for index, restoreKey := range restoreKeys {
			restore[index] = candidate + restoreKey
		}
		archive, err := store.MatchArchive(ctx, candidate+key, restore)
		if errors.Is(err, storecontract.ErrMiss) {
			continue
		}
		if err != nil {
			return nil, err
		}
		prefix, found = candidate, archive

		break
	}
	if found.Key == "" {
		return actionsJSONResponse(map[string]any{"ok": false})
	}
	matchedKey, ok := strings.CutPrefix(found.Key, prefix)
	if !ok || !validActionsCacheField(matchedKey) {
		return nil, errors.New("site cache returned a key outside the Actions namespace")
	}
	id, err := randomCacheToken()
	if err != nil {
		return nil, err
	}
	signature, err := randomCacheToken()
	if err != nil {
		return nil, err
	}
	archive := &actionsArchive{
		ID: id, Signature: signature, Mode: actionsModeDownload, CacheKey: matchedKey,
		StoreKey: found.Key, Version: version, Object: &found,
	}
	session.actions[id] = archive
	if err := s.persistSession(session); err != nil {
		delete(session.actions, id)

		return nil, err
	}

	return actionsJSONResponse(map[string]any{
		"ok": true, "signed_download_url": s.actionsSignedURL(archive, origin), "matched_key": matchedKey,
	})
}

// downloadObjectArchive serves an archive from the store, with the ranges the
// official client asks for. The size came from the store when the lookup matched, and
// an archive never changes, so nothing here asks again before the bytes.
func (s *CacheService) downloadObjectArchive(
	req *http.Request, session *cacheSession, archive *actionsArchive,
) (*http.Response, error) {
	object := *archive.Object
	start, length, partial, refused := actionsRequestedRange(req, object.Size)
	if refused != nil {
		return refused, nil
	}
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
	}
	response := actionsBlobResponse(status)
	response.Header.Set("Accept-Ranges", "bytes")
	response.Header.Set("Content-Type", "application/octet-stream")
	response.Header.Set("Content-Length", strconv.FormatInt(length, 10))
	response.Header.Set("ETag", `"billet-`+archive.ID+`"`)
	response.Header.Set("Last-Modified", object.Modified.UTC().Format(http.TimeFormat))
	if partial {
		response.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d",
			start, start+length-1, object.Size))
	}
	if req.Method == http.MethodHead {
		return response, nil
	}
	body, err := s.archiveStore().ReadArchive(req.Context(), object, start, length)
	if err != nil {
		return nil, err
	}
	response.Body = &limitedFile{
		Reader: io.LimitReader(body, length), file: body,
		service: s, session: session, archive: archive.ID,
	}

	return response, nil
}

// actionsBlockNames checks the block ids a client committed.
func actionsBlockNames(blockIDs []string) ([]string, error) {
	if len(blockIDs) == 0 || len(blockIDs) > 10000 {
		return nil, errors.New("azure block list is empty or too large")
	}
	for _, blockID := range blockIDs {
		if blockID == "" || len(blockID) > 1024 {
			return nil, errors.New("azure block list contains an invalid id")
		}
	}

	return blockIDs, nil
}

func (r *actionsReceipt) validObject() error {
	rawID, idErr := hex.DecodeString(r.ID)
	entryID, err := strconv.ParseInt(r.EntryID, 10, 64)
	if idErr != nil || len(rawID) != 32 || r.ID != actionsReceiptID(r.CacheKey, r.Version) ||
		!validActionsCacheField(r.CacheKey) || !validActionsCacheField(r.Version) ||
		strings.TrimSpace(r.StoreKey) == "" || len(r.Blocks) == 0 || err != nil || entryID <= 0 ||
		r.Size < 0 || r.Size > actionsArchiveLimit || r.OK && !r.Complete {
		return errors.New("invalid finalization receipt")
	}

	return nil
}
