package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node/reapi"
	storecontract "github.com/junioryono/billet/internal/store"
)

// THE GO AND BAZEL CACHES ON A STORE THAT KEEPS WHOLE OBJECTS (ADR-023). A session's
// volume is a directory on the node's own disk, which is what its writes land in and
// what publication uploads from. An entry the session did not write comes from the
// store through a read cache on the same disk, shared by the sessions of one key, so a
// build's thousands of reads do not each cost a request and a job's unpublished
// writes reach no other job.
const (
	// casCacheDirectory holds the read cache, under the node's state directory.
	casCacheDirectory = "cas-cache"
	// casCacheFill is how full the disk may get before the reaper makes room. Below
	// casFullFraction, so a job's own writes are not refused for what the node merely
	// kept for later.
	casCacheFill = 0.7
	// casPublishWorkers is how many entries a publication uploads at once.
	casPublishWorkers = 16
	// casIndexRetry is how soon a listing that failed is tried again; a build asks by
	// the thousand, and each would otherwise be a listing.
	casIndexRetry = time.Minute
)

// blobIndex is what a key held when a session asked. It is not asked again, because
// what another job publishes meanwhile only costs this one a miss.
type blobIndex struct {
	mu    sync.Mutex
	refs  map[storecontract.BlobRef]struct{}
	retry time.Time
}

// blobUses is what a session read through the store, for the record that keeps those
// entries from eviction. In memory: a session recovered by another process records
// only what it read since, which costs at most an early eviction.
type blobUses struct {
	mu   sync.Mutex
	refs map[storecontract.BlobRef]struct{}
}

func (u *blobUses) add(ref storecontract.BlobRef) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.refs == nil {
		u.refs = make(map[storecontract.BlobRef]struct{})
	}
	u.refs[ref] = struct{}{}
}

// blobStore is the site's store, if it keeps a content-addressed cache as objects.
func (s *CacheService) blobStore() storecontract.BlobStore {
	store, _ := s.store.(storecontract.BlobStore)

	return store
}

// attachBlobVolume is casVolume on a store of objects. Called with session.mu held.
func (s *CacheService) attachBlobVolume(
	session *cacheSession, kind config.CacheKind, key string,
) (*hostVolume, error) {
	root := s.casMountPath(session, kind)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("prepare the %s cache directory: %w", kind, err)
	}
	hv := &hostVolume{
		Kind: kind, Volume: storecontract.Volume{Key: key}, Objects: true, Mounted: true,
		allowedAt: s.now(),
	}
	session.hosts[kind] = hv
	if err := s.persistSession(session); err != nil {
		delete(session.hosts, kind)

		return nil, errors.Join(err, os.RemoveAll(root))
	}

	return hv, nil
}

// blobIndexOf is what the store holds for a volume's key, listed on first use and
// outside session.mu: a listing of a large cache takes seconds. An error is
// could-not-tell, which is a miss for the one read that asked.
func (s *CacheService) blobIndexOf(
	ctx context.Context, hv *hostVolume,
) (map[storecontract.BlobRef]struct{}, error) {
	hv.index.mu.Lock()
	defer hv.index.mu.Unlock()
	if hv.index.refs != nil {
		return hv.index.refs, nil
	}
	if s.now().Before(hv.index.retry) {
		return nil, errCASBackoff
	}
	refs, err := s.blobStore().ListBlobs(ctx, hv.Volume.Key)
	if err != nil {
		hv.index.retry = s.now().Add(casIndexRetry)

		return nil, err
	}
	hv.index.refs = refs

	return refs, nil
}

// casCachePath is where the read cache keeps one entry of a key. The key's digest,
// so nothing a key holds can name a path, and the digest was validated by casHandle.
func (s *CacheService) casCachePath(key string, table reapi.Table, digest string) string {
	sum := sha256.Sum256([]byte(key))

	return filepath.Join(s.rootState, casCacheDirectory, hex.EncodeToString(sum[:]),
		string(table), digest[:2], digest)
}

// openBlob answers a read the session's own directory cannot, from the read cache
// or the store. An entry neither holds, and every failure, is a miss: a cache that
// cannot be read must never fail a build.
func (h *casHandle) openBlob(table reapi.Table, digest string) (*os.File, error) {
	refs, err := h.s.blobIndexOf(h.ctx, h.hv)
	if err != nil {
		if !errors.Is(err, errCASBackoff) {
			h.s.log.Warn("could not list a content-addressed cache; its reads are misses for now",
				"instance", h.session.instance, "kind", h.hv.Kind, "error", err)
		}

		return nil, fs.ErrNotExist
	}
	ref := storecontract.BlobRef{Table: storecontract.BlobTable(table), Digest: digest}
	if _, ok := refs[ref]; !ok {
		return nil, fs.ErrNotExist
	}

	path := h.s.casCachePath(h.hv.Volume.Key, table, digest)
	file, err := openRegular(path)
	if err != nil {
		if err := h.fillCache(table, ref, path); err != nil {
			if !errors.Is(err, storecontract.ErrMiss) {
				h.s.log.Warn("could not fetch a content-addressed cache entry; it is a miss",
					"instance", h.session.instance, "kind", h.hv.Kind, "error", err)
			}

			return nil, fs.ErrNotExist
		}
		if file, err = openRegular(path); err != nil {
			return nil, fs.ErrNotExist
		}
	} else if err := os.Chtimes(path, time.Time{}, h.s.now()); err != nil {
		// THE REAPER'S AGE IS THIS TIME, so one that cannot be set costs an early
		// reap and no more.
		h.s.log.Debug("could not mark a cached entry used", "error", err)
	}
	h.hv.hit.Store(true)
	h.hv.reads.add(ref)

	return file, nil
}

// fillCache fetches one entry into the read cache, whole or not at all.
func (h *casHandle) fillCache(table reapi.Table, ref storecontract.BlobRef, path string) error {
	body, err := h.s.blobStore().ReadBlob(h.ctx, h.hv.Volume.Key, ref)
	if err != nil {
		return err
	}
	defer body.Close()

	limit := int64(casResultLimit)
	if table == reapi.TableCAS {
		limit = h.limit
	}

	// MADE HERE, because the fill asks how full this directory's disk is before it
	// writes, and a directory that is not there reads as a full disk.
	root := filepath.Join(h.s.rootState, casCacheDirectory)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("prepare the read cache: %w", err)
	}

	return h.s.storeCASObject(h.ctx, body, path, string(table), ref.Digest, limit, root)
}

// openRegular opens a file without following a link, and only if it is regular.
func openRegular(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.Join(fs.ErrNotExist, err, file.Close())
	}

	return file, nil
}

// releaseBlobVolume is releaseCASVolume on a store of objects: it uploads what a
// completed job wrote and was authorised to publish, records what the job used, and
// removes the session's directory. Called with session.mu held, from cleanupSession.
//
// RETRIED RATHER THAN ABANDONED WITHIN ITS WINDOW, unlike a volume's merge: an upload
// that stopped partway leaves only entries that are whole and correct, so doing it
// again is safe at any point, a crash's restart among them.
func (s *CacheService) releaseBlobVolume(
	ctx context.Context, session *cacheSession, hv *hostVolume,
) (bool, error) {
	if !lockWithin(ctx, &hv.io, casReleaseWait) {
		return false, errors.New("a transfer is still using the volume")
	}
	defer hv.io.Unlock()

	root := s.casMountPath(session, hv.Kind)
	done := func() (bool, error) {
		if err := os.RemoveAll(root); err != nil {
			return false, fmt.Errorf("remove the %s cache directory: %w", hv.Kind, err)
		}
		hv.Mounted = false

		return true, nil
	}

	journal := hv.Journal
	if journal == nil {
		journal = &publishJournal{}
		hv.Journal = journal
	}
	written := s.writtenBlobs(root)
	if hv.Intent != nil && hv.Dirty && journal.Phase == "" {
		abandon := func(reason string) {
			journal.Phase, journal.Reason = phaseAbandoned, reason
			s.log.Info("a cache publication was abandoned; the writes are discarded",
				"instance", session.instance, "kind", hv.Kind, "reason", reason)
		}
		deadline := hv.Intent.At.Add(publishWindow)
		switch {
		case !s.now().Before(deadline):
			abandon("the publication did not complete within its window")
		case !s.sessionKindAllowed(ctx, session, hv.Kind):
			abandon("the cache is disabled for this repository")
		default:
			publishCtx, cancel := context.WithDeadline(ctx, deadline)
			err := s.publishBlobs(publishCtx, hv, root, written)
			cancel()
			if err != nil {
				return false, fmt.Errorf("upload the %s cache: %w", hv.Kind, err)
			}
			journal.Phase = phasePublished
			s.log.Info("published a content-addressed cache a job wrote", "instance", session.instance,
				"kind", hv.Kind, "entries", len(written))
		}
		if err := s.persistSession(session); err != nil {
			return false, err
		}
	}

	// A RECORD THAT CANNOT BE WRITTEN COSTS AN EARLY EVICTION, and a discarded job's
	// reads still count: what it read is what the next job will want.
	used := s.usedBlobs(hv, written, journal.Phase == phasePublished)
	if len(used) > 0 {
		if err := s.blobStore().RecordBlobUse(ctx, hv.Volume.Key,
			session.pathID+"/"+string(hv.Kind), used); err != nil {
			s.log.Warn("could not record what a job used of a content-addressed cache; "+
				"it may be evicted early", "instance", session.instance, "kind", hv.Kind, "error", err)
		}
	}

	return done()
}

// writtenBlobs is every entry a session's directory holds, which is what it wrote: a
// read of the store lands in the read cache. A directory that cannot be read is
// reported as what could be walked, and the upload is of that.
func (s *CacheService) writtenBlobs(root string) []storecontract.BlobRef {
	var refs []storecontract.BlobRef
	for _, table := range []reapi.Table{reapi.TableAC, reapi.TableCAS} {
		base := filepath.Join(root, string(table))
		err := filepath.WalkDir(base, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && path == base {
					return fs.SkipDir
				}

				return err
			}
			if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") || !validDigest(entry.Name()) {
				return nil
			}
			refs = append(refs, storecontract.BlobRef{Table: storecontract.BlobTable(table), Digest: entry.Name()})

			return nil
		})
		if err != nil && !errors.Is(err, fs.SkipDir) {
			s.log.Warn("could not read all of a content-addressed cache's writes", "path", base, "error", err)
		}
	}
	slices.SortFunc(refs, func(a, b storecontract.BlobRef) int {
		if a.Table != b.Table {
			return strings.Compare(string(a.Table), string(b.Table))
		}

		return strings.Compare(a.Digest, b.Digest)
	})

	return refs
}

// usedBlobs is what a job's record names: what it read, and what it wrote if it was
// published.
func (s *CacheService) usedBlobs(
	hv *hostVolume, written []storecontract.BlobRef, published bool,
) []storecontract.BlobRef {
	hv.reads.mu.Lock()
	defer hv.reads.mu.Unlock()
	used := make([]storecontract.BlobRef, 0, len(hv.reads.refs)+len(written))
	for ref := range hv.reads.refs {
		used = append(used, ref)
	}
	if published {
		used = append(used, written...)
	}

	return used
}

// publishBlobs uploads a session's writes, unless the store's first writer has them.
func (s *CacheService) publishBlobs(
	ctx context.Context, hv *hostVolume, root string, written []storecontract.BlobRef,
) error {
	store := s.blobStore()
	work := make(chan storecontract.BlobRef)
	failed := atomic.Pointer[error]{}
	var wg sync.WaitGroup
	for range casPublishWorkers {
		wg.Go(func() {
			for ref := range work {
				if failed.Load() != nil {
					continue
				}
				if err := putBlobFile(ctx, store, hv.Volume.Key, ref, root); err != nil {
					failed.CompareAndSwap(nil, &err)
				}
			}
		})
	}
	for _, ref := range written {
		work <- ref
	}
	close(work)
	wg.Wait()
	if err := failed.Load(); err != nil {
		return *err
	}

	return nil
}

func putBlobFile(
	ctx context.Context, store storecontract.BlobStore, key string, ref storecontract.BlobRef, root string,
) error {
	file, err := openRegular(filepath.Join(root, string(ref.Table), ref.Digest[:2], ref.Digest))
	if err != nil {
		return err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return err
	}

	return store.PutBlob(ctx, key, ref, io.LimitReader(file, info.Size()), info.Size())
}

// ReapCASCache makes room on the node's disk by removing the read cache's least
// recently used entries, once it is fuller than casCacheFill. A removed entry is
// fetched again when it is next read.
func (s *CacheService) ReapCASCache(ctx context.Context) error {
	root := filepath.Join(s.rootState, casCacheDirectory)
	if _, err := os.Stat(root); errors.Is(err, fs.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return fmt.Errorf("node: read the disk under the read cache: %w", err)
	}
	total := float64(stat.Blocks)
	if total == 0 {
		return nil
	}
	over := (total-float64(stat.Bavail))/total - s.casCacheFill
	if over <= 0 {
		return nil
	}

	return reapCASEntries(ctx, root, int64(over*total*float64(stat.Bsize)))
}

// reapCASEntries removes the read cache's least recently used entries until toFree
// bytes are gone.
func reapCASEntries(ctx context.Context, root string, toFree int64) error {
	type entry struct {
		path string
		used time.Time
		size int64
	}
	var entries []entry
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		entries = append(entries, entry{path: path, used: info.ModTime(), size: info.Size()})

		return nil
	})
	if err != nil {
		return fmt.Errorf("node: walk the read cache: %w", err)
	}
	slices.SortFunc(entries, func(a, b entry) int { return a.used.Compare(b.used) })
	for _, candidate := range entries {
		if toFree <= 0 || ctx.Err() != nil {
			break
		}
		if err := os.Remove(candidate.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("node: remove a cached entry: %w", err)
		}
		toFree -= candidate.size
	}

	return ctx.Err()
}
