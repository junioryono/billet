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
	// closedSessionUploads is how many closed sessions a pass finishes at once on a store
	// of objects. An upload holds its session for as long as it takes and its window
	// counts from the job's completion, so one job's upload must not spend the next's.
	closedSessionUploads = 8
	// closedSessionAdmit is how long a pass goes on taking sessions that close while it
	// runs. It has to return, because renewal runs after it on the same loop: this plus
	// a publication's window is far inside the cacheRenewWithin a lease has left when it
	// comes due.
	closedSessionAdmit = time.Hour
	// casIndexRetry is how soon a listing that failed is tried again; a build asks by
	// the thousand, and each would otherwise be a listing.
	casIndexRetry = time.Minute
)

// errPublishDisabled is the kill switch turned on partway through an upload.
var errPublishDisabled = errors.New("the cache was disabled during its publication")

// blobIndex is what a key held when a session asked. It is not asked again, because
// what another job publishes meanwhile only costs this one a miss.
type blobIndex struct {
	mu    sync.Mutex
	refs  map[storecontract.BlobRef]int64
	retry time.Time
}

// blobUses is what a session read through the store, for the record that keeps those
// entries from eviction. In memory: a session recovered by another process records
// only what it read since, which costs at most an early eviction.
type blobUses struct {
	mu   sync.Mutex
	refs map[storecontract.BlobRef]struct{}
}

// forget takes an entry a read proved gone out of the listing.
func (i *blobIndex) forget(ref storecontract.BlobRef) {
	i.mu.Lock()
	defer i.mu.Unlock()
	delete(i.refs, ref)
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
	store, ok := s.store.(storecontract.BlobStore)
	if !ok {
		return nil
	}

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

// listedSize is what the store's listing says of one entry of a volume's key, listed on
// first use and outside session.mu: a listing of a large cache takes seconds. An error
// is could-not-tell, which is a miss for the one read that asked.
//
// THE LISTING DOES NOT LEAVE THE LOCK, because a read that finds an entry gone takes it
// out, and transfers run side by side: a map read beside that write ends the process.
func (s *CacheService) listedSize(
	ctx context.Context, hv *hostVolume, ref storecontract.BlobRef,
) (int64, bool, error) {
	hv.index.mu.Lock()
	defer hv.index.mu.Unlock()
	if hv.index.refs == nil {
		if s.now().Before(hv.index.retry) {
			return 0, false, errCASBackoff
		}
		refs, err := s.blobStore().ListBlobs(ctx, hv.Volume.Key)
		if err != nil {
			// NOT FOR A CLIENT THAT HUNG UP, which says nothing of the store, and would cost
			// the rest of its session a minute of misses.
			if ctx.Err() == nil {
				hv.index.retry = s.now().Add(casIndexRetry)
			}

			return 0, false, err
		}
		hv.index.refs = refs
	}
	size, ok := hv.index.refs[ref]

	return size, ok, nil
}

// casCachePath is where the read cache keeps one entry of a key. The key's digest,
// so nothing a key holds can name a path, and the digest was validated by casHandle.
func (s *CacheService) casCachePath(key string, table reapi.Table, digest string) string {
	sum := sha256.Sum256([]byte(key))

	return filepath.Join(s.rootState, casCacheDirectory, hex.EncodeToString(sum[:]),
		string(table), digest[:2], digest)
}

// listedBlob is what the key's listing says of one entry. Not being listed and not
// being able to list are both a miss: a cache that cannot be read must never fail a
// build.
func (h *casHandle) listedBlob(table reapi.Table, digest string) (storecontract.BlobRef, int64, bool) {
	ref := storecontract.BlobRef{Table: storecontract.BlobTable(table), Digest: digest}
	size, ok, err := h.s.listedSize(h.ctx, h.hv, ref)
	if err != nil {
		if !errors.Is(err, errCASBackoff) {
			h.s.log.Warn("could not list a content-addressed cache; its reads are misses for now",
				"instance", h.session.instance, "kind", h.hv.Kind, "error", err)
		}

		return ref, 0, false
	}

	return ref, size, ok
}

// blobSize answers Size from the listing, for an entry not in the session's directory.
//
// A YES IS A PROMISE THE ENTRY CAN BE READ, because a build that is told an action's output
// is there may go on without it, and only finds out when it asks. So an entry this session
// could not read is not there: one past its limit, and one a full disk would not let the
// read cache fill. An eviction after the listing is the one thing this cannot see, and a
// later job's listing corrects it.
func (h *casHandle) blobSize(table reapi.Table, digest string) (int64, error) {
	ref, size, ok := h.listedBlob(table, digest)
	if !ok || size > h.readLimit(table) {
		return 0, fs.ErrNotExist
	}
	if _, err := os.Lstat(h.s.casCachePath(h.hv.Volume.Key, table, digest)); err != nil {
		if h.s.readCacheFull() != nil {
			return 0, fs.ErrNotExist
		}
	}
	h.hv.hit.Store(true)
	h.hv.reads.add(ref)

	return size, nil
}

// openBlob answers a read the session's own directory cannot, from the read cache
// or the store. An entry neither holds, and every failure, is a miss.
func (h *casHandle) openBlob(table reapi.Table, digest string) (*os.File, error) {
	ref, _, ok := h.listedBlob(table, digest)
	if !ok {
		return nil, fs.ErrNotExist
	}

	path := h.s.casCachePath(h.hv.Volume.Key, table, digest)
	file, err := openRegular(path)
	if err != nil {
		if err := h.fillCache(table, ref, path); err != nil {
			// WHAT THE LISTING SAID IS DISPROVED, so no one is told it is there again.
			if errors.Is(err, storecontract.ErrMiss) {
				h.hv.index.forget(ref)
			}
			// A FULL DISK IS NOT WORTH A LINE PER READ, and the reaper is on its way.
			if !errors.Is(err, storecontract.ErrMiss) && !errors.Is(err, reapi.ErrFull) {
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
	// ASKED BEFORE THE REQUEST, which would otherwise be paid for and thrown away.
	if err := h.s.readCacheFull(); err != nil {
		return err
	}

	body, err := h.s.blobStore().ReadBlob(h.ctx, h.hv.Volume.Key, ref)
	if err != nil {
		return err
	}
	defer body.Close()

	return h.s.storeCASObject(h.ctx, body, path, string(table), ref.Digest, h.readLimit(table),
		filepath.Join(h.s.rootState, casCacheDirectory))
}

// readLimit is the largest entry of table this session may read or write.
func (h *casHandle) readLimit(table reapi.Table) int64 {
	if table == reapi.TableCAS {
		return h.limit
	}

	return casResultLimit
}

// readCacheFull is reapi.ErrFull when the disk under the read cache will not take another
// entry. It makes the directory, because one that is not there reads as a full disk.
func (s *CacheService) readCacheFull() error {
	root := filepath.Join(s.rootState, casCacheDirectory)
	//nolint:gosec // G703: the node's configured state directory and a constant, nothing of the request's
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("prepare the read cache: %w", err)
	}
	if full, err := s.casFull(root); err != nil || full {
		return reapi.ErrFull
	}

	return nil
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
		inWindow := s.now().Before(deadline)
		policy := policyUnknown
		if inWindow {
			policy = s.sessionKindPolicy(ctx, session, hv.Kind)
		}
		switch {
		case !inWindow:
			abandon("the publication did not complete within its window")
		case policy == policyRefused:
			abandon("the cache is disabled for this repository")
		case policy != policyAllowed:
			// COULD-NOT-TELL IS NOT A REFUSAL, and nothing is lost by asking again on
			// the next pass, inside the window.
			return false, fmt.Errorf("the %s cache's kill switch could not be read", hv.Kind)
		default:
			publishCtx, cancel := context.WithDeadline(ctx, deadline)
			err := s.publishBlobs(publishCtx, session, hv, root, written)
			cancel()
			switch {
			case errors.Is(err, errPublishDisabled):
				abandon("the cache is disabled for this repository")
			case err != nil:
				// SAID HERE, because a pass keeps only each session's last word, and an
				// upload that fails until its window ends has none but the abandonment.
				s.log.Warn("could not yet upload a cache a completed job wrote; will retry",
					"instance", session.instance, "kind", hv.Kind, "error", err)

				return false, fmt.Errorf("upload the %s cache: %w", hv.Kind, err)
			default:
				journal.Phase = phasePublished
				s.log.Info("published a content-addressed cache a job wrote", "instance", session.instance,
					"kind", hv.Kind, "entries", len(written))
			}
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
	if journal.Phase == phasePublished {
		s.keepPublished(hv, root, written)
	}

	return done()
}

// keepPublished links what a published job wrote into the read cache, which is on the
// same disk, so the next job of the key on this node reads it there rather than from
// the store: a warm build asks for thousands of entries, and each one fetched is a
// request (114 s against 45 s cold for `go build std` on EC2, 2026-11-05).
//
// ONLY ONCE IT IS PUBLISHED, so what reaches the next job is what the store holds or
// another answer the store would equally give: an output is named by its bytes, and of
// two action results either is valid. What cannot be moved is fetched as before.
func (s *CacheService) keepPublished(hv *hostVolume, root string, written []storecontract.BlobRef) {
	kept := 0
	for _, ref := range written {
		if s.readCacheFull() != nil {
			break
		}
		target := s.casCachePath(hv.Volume.Key, reapi.Table(ref.Table), ref.Digest)
		if _, err := os.Lstat(target); err == nil {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			break
		}
		// LINKED, NOT MOVED: the session's directory keeps it until the directory goes,
		// so a release retried after a failure here counts the same writes in its record.
		if err := os.Link(filepath.Join(root, string(ref.Table), ref.Digest[:2], ref.Digest), target); err != nil {
			continue
		}
		kept++
	}
	if kept > 0 {
		s.log.Debug("kept a published job's entries on this node", "kind", hv.Kind, "entries", kept)
	}
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

// writtenBytes is what a session's directory holds, which is what its job has spent
// of its allowance. What cannot be read counts as nothing: the worst of that is a job
// writing a little over its size after a restart.
func (s *CacheService) writtenBytes(root string) int64 {
	var total int64
	err := filepath.WalkDir(root, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// NOT A DOTTED NAME, which a crash left and no upload will take: a whole object
		// never installed must not spend what the job has left.
		if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		total += info.Size()

		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Warn("could not count all of what a job has written", "path", root, "error", err)
	}

	return total
}

// publishBlobs uploads a session's writes, unless the store's first writer has them.
//
// THE KILL SWITCH IS ASKED AGAIN WHILE IT RUNS, as it is while a volume is in use: an
// upload lasts as long as the job's output is large, and disabling a cache has to stop
// it within casPolicyAge, including one upload that takes that long by itself. Only a
// refusal stops it, because one that could not be read mid-upload is not one, and a blip
// must not discard what the job wrote.
func (s *CacheService) publishBlobs(
	parent context.Context, session *cacheSession, hv *hostVolume, root string, written []storecontract.BlobRef,
) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)

	uploaded := make(chan struct{})
	var asking sync.WaitGroup
	asking.Go(func() {
		ticker := time.NewTicker(s.casPolicyEvery)
		defer ticker.Stop()
		for {
			select {
			case <-uploaded:
				return
			case <-ticker.C:
				if s.sessionKindPolicy(ctx, session, hv.Kind) == policyRefused {
					cancel(errPublishDisabled)

					return
				}
			}
		}
	})

	store := s.blobStore()
	work := make(chan storecontract.BlobRef)
	failed := atomic.Pointer[error]{}
	var wg sync.WaitGroup
	for range casPublishWorkers {
		wg.Go(func() {
			for ref := range work {
				if failed.Load() != nil || ctx.Err() != nil {
					continue
				}
				if err := putBlobFile(ctx, store, hv.Volume.Key, ref, root); err != nil {
					failed.CompareAndSwap(nil, &err)
				}
			}
		})
	}
feed:
	for _, ref := range written {
		select {
		case work <- ref:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	close(uploaded)
	asking.Wait()

	if errors.Is(context.Cause(ctx), errPublishDisabled) {
		return errPublishDisabled
	}
	if err := failed.Load(); err != nil {
		return *err
	}

	return parent.Err()
}

// retryClosedSideBySide is RetryClosed on a store of objects: up to closedSessionUploads
// sessions at once, and while any runs it goes on taking those that close and trying
// again those not finished. A pass over the ones closed when it began would leave a job
// that ended a minute later waiting out the longest upload ahead of it, with its window
// counting, and one whose upload failed once waiting past that window for its next try.
func (s *CacheService) retryClosedSideBySide(ctx context.Context) error {
	type outcome struct {
		session *cacheSession
		err     error
	}
	var (
		queue   []*cacheSession
		running int
		missed  bool
	)
	failures := make(map[*cacheSession]error)
	active := make(map[*cacheSession]bool)
	admit := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, session := range s.byToken {
			if session.closing.Load() && !active[session] {
				active[session] = true
				queue = append(queue, session)
			}
		}
	}
	admit()

	done := make(chan outcome)
	closed := s.closed
	ticker := time.NewTicker(s.closedSessionRetry)
	defer ticker.Stop()
	retry := ticker.C
	admitUntil := s.now().Add(closedSessionAdmit)
	for {
		for running < closedSessionUploads && len(queue) > 0 && ctx.Err() == nil {
			session := queue[0]
			queue = queue[1:]
			running++
			go func() { done <- outcome{session, s.cleanupSession(ctx, session, false, true)} }()
		}
		if running == 0 {
			break
		}
		select {
		case finished := <-done:
			running--
			delete(active, finished.session)
			delete(failures, finished.session)
			if finished.err != nil {
				failures[finished.session] = finished.err
			}
		case <-retry:
			if s.now().Before(admitUntil) {
				admit()
			} else {
				retry = nil
			}
		case <-closed:
			if s.now().Before(admitUntil) {
				admit()
			} else {
				// LEFT FOR THE NEXT PASS, which this signal is what wakes.
				closed, missed = nil, true
			}
		}
	}
	if missed {
		s.signalClosed()
	}

	// THE LAST WORD ON EACH: one that failed and then finished has nothing to report.
	joined := make([]error, 0, len(failures))
	for _, err := range failures {
		joined = append(joined, err)
	}

	return errors.Join(joined...)
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
		// NOT WHAT A FILL IS STILL WRITING, whose rename would then fail. One older than
		// any transfer can last is what a crash left, which nothing else will take.
		if strings.HasPrefix(d.Name(), ".") && time.Since(info.ModTime()) < casRequestLife {
			return nil
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
