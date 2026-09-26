package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/node/reapi"
	storecontract "github.com/junioryono/billet/internal/store"
)

// A content-addressed cache volume serves the Go build cache and the Bazel and
// Buck2 remote caches: a site-store clone the node mounts on the host, holding
// objects named by their SHA-256 (`cas/`) and action results named by the
// action's digest (`ac/`). The guest never mounts it; it reads and writes over
// the cache listener, so every byte in it was written by the node.
const (
	casPathPrefix  = "/v1/cas/"
	casRequestLife = 10 * time.Minute
	// casResultLimit bounds one action result, which is a small record (a Go
	// output reference, a Bazel ActionResult).
	casResultLimit = 1 << 20
	// casFullFraction is how full a volume may get before writes stop being
	// cached: a job whose cache is full still builds, and publishes nothing new.
	casFullFraction = 0.9
	// casConcurrency bounds one session's concurrent cache transfers, which a
	// build issues many of at once.
	casConcurrency = 16
	// casPolicyAge is how long a kill switch's answer stands for a session's
	// transfers, which are too many to ask the control plane about each.
	casPolicyAge = 30 * time.Second
	// casReleaseWait bounds how long the cache loop waits for a closed
	// session's transfers to end before it tries again on a later pass.
	casReleaseWait = 5 * time.Second
)

// casKinds are the caches served from a content-addressed volume.
var casKinds = map[config.CacheKind]bool{config.CacheGo: true, config.CacheBazel: true}

// hostVolume is a clone the node mounts to serve one guest's content-addressed
// cache. Durable with the session, so a restart can finish what it began.
type hostVolume struct {
	Kind    config.CacheKind     `json:"kind"`
	Volume  storecontract.Volume `json:"volume"`
	Mounted bool                 `json:"mounted,omitempty"`
	// Dirty says something was written, so there is something to publish.
	Dirty   bool            `json:"dirty,omitempty"`
	Intent  *publishIntent  `json:"intent,omitempty"`
	Journal *publishJournal `json:"journal,omitempty"`
	// Merge is the clone of the newest generation a merge publication is
	// filling, recorded before it is mounted so a crash cannot lose it.
	Merge *storecontract.Volume `json:"merge,omitempty"`
	// Supersedes is the full generation this job found and started empty in
	// place of. Its volume replaces that generation and no other: anything
	// published since, another fresh start's included, is merged into.
	Supersedes string `json:"supersedes,omitempty"`

	// io is held for reading by every transfer and for writing by whatever
	// unmounts the volume, so no transfer runs on a volume being taken away.
	// THE ORDER IS session.mu, THEN io: a transfer holding io never waits for
	// session.mu, which is why a write is marked Dirty when it is admitted.
	io sync.RWMutex
	// allowedAt is when the kill switch last allowed this cache, under
	// session.mu; a transfer asks again once it is older than casPolicyAge.
	allowedAt time.Time
	// used and hit say a transfer was admitted and an object was found, for
	// what the session reports the cache did; atomics, for the lock order.
	used atomic.Bool
	hit  atomic.Bool
}

func (s *CacheService) casMountPath(session *cacheSession, kind config.CacheKind) string {
	return filepath.Join(s.rootState, "cas-volumes", session.pathID, string(kind))
}

// sessionPathID is a name for a session's mount points that reveals nothing of
// its bearer.
func sessionPathID(token string) string {
	sum := sha256.Sum256([]byte("billet cache session path\x00" + token))

	return hex.EncodeToString(sum[:16])
}

// casVolume is the session's mounted volume of one kind, attaching it on first
// use. Called with session.mu held.
func (s *CacheService) casVolume(
	ctx context.Context, session *cacheSession, kind config.CacheKind,
) (*hostVolume, error) {
	if hv := session.hosts[kind]; hv != nil {
		if !hv.Mounted {
			return nil, errors.New("the cache volume is not mounted")
		}

		return hv, nil
	}
	setting := sessionSetting(session, kind)
	if !setting.Enabled || !s.sessionKindAllowed(ctx, session, kind) {
		return nil, errCacheOff
	}

	ceiling := volumeCeiling(setting)
	key := s.cacheKeyFor(session, kind, "", "")
	volume, cold, err := s.cloneWithin(ctx, key, ceiling)
	if cold {
		volume, err = s.store.Create(ctx, key, ceiling)
	}
	if err != nil {
		return nil, err
	}

	// DURABLE BEFORE IT IS MOUNTED, so a crash between the two leaves a record
	// restart cleanup can find.
	hv := &hostVolume{Kind: kind, Volume: volume, allowedAt: s.now()}
	session.hosts[kind] = hv
	if err := s.persistSession(session); err != nil {
		delete(session.hosts, kind)

		return nil, errors.Join(err, s.store.Discard(ctx, volume))
	}

	mount := s.actionIO.MountWritable
	if cold {
		mount = s.actionIO.MountNew
	}
	if err := mount(ctx, volume.Device, s.casMountPath(session, kind)); err != nil {
		return nil, fmt.Errorf("mount the %s cache: %w", kind, err)
	}
	hv.Mounted = true
	if err := s.persistSession(session); err != nil {
		return nil, err
	}

	// A FULL CACHE STARTS OVER. Merge publication only ever adds, so a
	// generation past its fill line would refuse every write and every merge
	// from then on; a job that finds one starts empty, and publishes an empty
	// start with what it wrote in place of the full generation.
	if full, err := s.casFull(s.casMountPath(session, kind)); err == nil && full && !cold {
		if err := s.startFresh(ctx, session, hv, key, ceiling); err != nil {
			return nil, err
		}
	}

	return hv, nil
}

// startFresh replaces a mounted clone of a full generation with a new empty
// volume, recorded before each step so a crash leaves nothing unaccounted for.
func (s *CacheService) startFresh(
	ctx context.Context, session *cacheSession, hv *hostVolume, key string, ceiling int64,
) error {
	path := s.casMountPath(session, hv.Kind)
	if err := s.actionIO.Unmount(ctx, path); err != nil {
		return fmt.Errorf("unmount the full %s cache: %w", hv.Kind, err)
	}
	hv.Mounted = false
	if err := s.persistSession(session); err != nil {
		return err
	}
	if err := s.store.Discard(ctx, hv.Volume); err != nil {
		return fmt.Errorf("discard the full %s cache: %w", hv.Kind, err)
	}
	fresh, err := s.store.Create(ctx, key, ceiling)
	if err != nil {
		return err
	}
	hv.Supersedes, hv.Volume = hv.Volume.Generation, fresh
	if err := s.persistSession(session); err != nil {
		return errors.Join(err, s.store.Discard(ctx, fresh))
	}
	if err := s.actionIO.MountNew(ctx, fresh.Device, path); err != nil {
		return fmt.Errorf("mount the fresh %s cache: %w", hv.Kind, err)
	}
	hv.Mounted = true

	return s.persistSession(session)
}

var errCacheOff = errors.New("this cache is off for this job")

// serveCAS answers one content-addressed cache request:
// `GET|HEAD|PUT /v1/cas/{kind}/{ac|cas}/{sha256}`.
//
// A cas object is refused unless its bytes hash to its name, so nothing a
// guest sends can sit under a digest it does not have. An ac record is the
// guest's claim about an action, trusted exactly as far as the job is: it lands
// in the job's own clone and is published only where the job's writes are.
func (s *CacheService) serveCAS(w http.ResponseWriter, r *http.Request, session *cacheSession) {
	kind, table, digest, ok := parseCASPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)

		return
	}
	ctx, cancel := extendTransfer(w, r)
	defer cancel()

	handle, err := s.openCASHandle(ctx, session, kind, r.Method == http.MethodPut)
	switch {
	case errors.Is(err, reapi.ErrBusy):
		http.Error(w, err.Error(), http.StatusTooManyRequests)

		return
	case errors.Is(err, reapi.ErrEnded):
		http.Error(w, err.Error(), http.StatusGone)

		return
	case errors.Is(err, reapi.ErrOff):
		http.Error(w, err.Error(), http.StatusForbidden)

		return
	case err != nil:
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}
	defer handle.Close()

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		file, err := handle.Open(reapi.Table(table), digest)
		if err != nil {
			http.NotFound(w, r)

			return
		}
		defer file.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, file)
	case http.MethodPut:
		if err := handle.Put(ctx, reapi.Table(table), digest, r.Body); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, reapi.ErrFull) {
				status = http.StatusInsufficientStorage
			}
			http.Error(w, err.Error(), status)

			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// extendTransfer lets one cache transfer outlive the listener's read timeout,
// which is sized for the small requests the rest of the API makes.
func extendTransfer(w http.ResponseWriter, r *http.Request) (context.Context, context.CancelFunc) {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(casRequestLife))
	_ = controller.SetWriteDeadline(time.Now().Add(casRequestLife))

	return context.WithTimeout(r.Context(), casRequestLife)
}

// casHandle is one admitted transfer's hold on a session's mounted volume: a
// slot of the session's transfer bound, and the volume's io lock for reading,
// so the volume is not unmounted under it. It is what both the HTTP routes and
// the Remote Execution API read and write through.
type casHandle struct {
	s       *CacheService
	session *cacheSession
	hv      *hostVolume
	root    string
	limit   int64
	release func()
}

var _ reapi.Volume = (*casHandle)(nil)

// openCASHandle admits a transfer and attaches the session's volume of kind on
// first use. The refusals are reapi's, so each protocol can say them its way.
//
// A WRITE IS RECORDED BEFORE IT IS ADMITTED, durably and under the session's
// lock, so a restart between the write and the job's completion still knows
// the volume holds something to publish; a transfer never takes that lock once
// it holds the volume.
func (s *CacheService) openCASHandle(
	ctx context.Context, session *cacheSession, kind config.CacheKind, write bool,
) (*casHandle, error) {
	select {
	case session.casAdmit <- struct{}{}:
	default:
		return nil, reapi.ErrBusy
	}
	admitted := func() { <-session.casAdmit }

	if err := lockCacheSession(ctx, session); err != nil {
		admitted()

		return nil, err
	}
	if session.closed {
		session.mu.Unlock()
		admitted()

		return nil, reapi.ErrEnded
	}
	hv, err := s.casVolume(ctx, session, kind)
	enabled := sessionSetting(session, kind).Enabled
	if err == nil && s.now().Sub(hv.allowedAt) >= casPolicyAge {
		// THE KILL SWITCH IS ASKED AGAIN WHILE THE VOLUME IS IN USE, not only
		// when it is attached, so disabling a cache stops a running job's use of
		// it within casPolicyAge.
		if !s.sessionKindAllowed(ctx, session, kind) {
			err = errCacheOff
		} else {
			hv.allowedAt = s.now()
		}
	}
	if err == nil && write && !hv.Dirty {
		hv.Dirty = true
		if persistErr := s.persistSession(session); persistErr != nil {
			hv.Dirty = false
			err = persistErr
		}
	}
	switch {
	case errors.Is(err, errCacheOff) && enabled:
		noteBuildCache(session, kind, alloc.BuildCacheDisabled)
	case err != nil && !errors.Is(err, errCacheOff):
		noteBuildCache(session, kind, alloc.BuildCacheUnavailable)
	}
	session.mu.Unlock()
	if errors.Is(err, errCacheOff) {
		admitted()

		return nil, reapi.ErrOff
	}
	if err != nil {
		admitted()
		s.log.Warn("a content-addressed cache is unavailable; the build continues uncached",
			"instance", session.instance, "kind", kind, "error", err)

		return nil, err
	}

	hv.io.RLock()
	if !hv.Mounted {
		hv.io.RUnlock()
		admitted()

		return nil, reapi.ErrEnded
	}
	hv.used.Store(true)

	return &casHandle{
		s: s, session: session, hv: hv, root: s.casMountPath(session, kind),
		limit: volumeCeiling(sessionSetting(session, kind)),
		release: func() {
			hv.io.RUnlock()
			admitted()
		},
	}, nil
}

func (h *casHandle) path(table reapi.Table, digest string) (string, error) {
	if (table != reapi.TableAC && table != reapi.TableCAS) || !validDigest(digest) {
		return "", fs.ErrNotExist
	}

	return filepath.Join(h.root, string(table), digest[:2], digest), nil
}

// Open returns a stored object, opened without following a link and only if it
// is a regular file, though nothing but the node writes the volume.
func (h *casHandle) Open(table reapi.Table, digest string) (*os.File, error) {
	path, err := h.path(table, digest)
	if err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fs.ErrNotExist
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()

		return nil, fs.ErrNotExist
	}
	h.hv.hit.Store(true)

	return file, nil
}

// Put stores an object and marks the volume written.
func (h *casHandle) Put(ctx context.Context, table reapi.Table, digest string, body io.Reader) error {
	path, err := h.path(table, digest)
	if err != nil {
		return reapi.ErrMismatch
	}
	limit := int64(casResultLimit)
	if table == reapi.TableCAS {
		limit = h.limit
	}
	return h.s.storeCASObject(ctx, body, path, string(table), digest, limit, h.root)
}

func (h *casHandle) Close() { h.release() }

func parseCASPath(path string) (config.CacheKind, string, string, bool) {
	rest, ok := strings.CutPrefix(path, casPathPrefix)
	if !ok {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 || !casKinds[config.CacheKind(parts[0])] ||
		(parts[1] != "ac" && parts[1] != "cas") || !validDigest(parts[2]) {
		return "", "", "", false
	}

	return config.CacheKind(parts[0]), parts[1], parts[2], true
}

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)

	return err == nil && strings.ToLower(value) == value
}

// storeCASObject writes one object through a temporary file and a rename, so a
// reader sees a whole object or none, and refuses content that does not hash
// to its name.
func (s *CacheService) storeCASObject(
	ctx context.Context, body io.Reader, path, table, digest string, limit int64, root string,
) error {
	if full, err := s.casFull(root); err != nil || full {
		return reapi.ErrFull
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("prepare the cache directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".put-")
	if err != nil {
		return fmt.Errorf("stage the object: %w", err)
	}
	defer os.Remove(temporary.Name())

	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(temporary, hash),
		io.LimitReader(contextReader{ctx: ctx, r: body}, limit+1))
	closeErr := temporary.Close()
	switch {
	case copyErr != nil:
		return fmt.Errorf("read the object: %w", copyErr)
	case closeErr != nil:
		return fmt.Errorf("stage the object: %w", closeErr)
	case written > limit:
		return fmt.Errorf("the object is larger than %d bytes", limit)
	case table == "cas" && hex.EncodeToString(hash.Sum(nil)) != digest:
		return reapi.ErrMismatch
	}

	return os.Rename(temporary.Name(), path)
}

// casFull reports whether a volume has passed the fraction it may fill.
func (s *CacheService) casFull(root string) (bool, error) { return filledAbove(root, s.casFill) }

// filledAbove reports whether root's filesystem is more than fraction full.
func filledAbove(root string, fraction float64) (bool, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return false, err
	}
	total := float64(stat.Blocks)
	if total == 0 {
		return false, nil
	}

	return (total-float64(stat.Bavail))/total > fraction, nil
}

// contextReader stops a copy when its context ends.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}

	return c.r.Read(p)
}

// releaseCASVolume unmounts a closed session's volume and publishes it when
// the job's completion authorised it, merging into the newest generation so a
// concurrent job's additions survive. Called with session.mu held, from
// cleanupSession; done means the volume needs nothing more.
func (s *CacheService) releaseCASVolume(
	ctx context.Context, session *cacheSession, hv *hostVolume,
) (done bool, err error) {
	// A TRANSFER STILL RUNNING HOLDS io, and the cache loop serves every
	// session, so it waits a little and comes back rather than wait out a
	// transfer's whole deadline.
	if !lockWithin(ctx, &hv.io, casReleaseWait) {
		return false, errors.New("a transfer is still using the volume")
	}
	defer hv.io.Unlock()

	if hv.Mounted {
		if err := s.actionIO.Unmount(ctx, s.casMountPath(session, hv.Kind)); err != nil {
			return false, fmt.Errorf("unmount the %s cache: %w", hv.Kind, err)
		}
		hv.Mounted = false
		if err := s.persistSession(session); err != nil {
			return false, err
		}
	}

	if hv.Intent == nil || !hv.Dirty {
		return s.discardCASVolumes(ctx, session, hv, true)
	}

	journal := hv.Journal
	if journal == nil {
		journal = &publishJournal{}
		hv.Journal = journal
	}
	abandon := func(reason string) (bool, error) {
		journal.Phase, journal.Reason = phaseAbandoned, reason
		s.log.Info("a cache publication was abandoned; the writes are discarded",
			"instance", session.instance, "kind", hv.Kind, "reason", reason)
		if err := s.persistSession(session); err != nil {
			return false, err
		}

		return s.discardCASVolumes(ctx, session, hv, true)
	}
	switch journal.Phase {
	case phasePublished:
		// A SNAPSHOT TOOK THE SESSION'S OWN CLONE, or a merge left it to discard.
		if journal.Consumed {
			return s.discardCASVolumes(ctx, session, hv, false)
		}

		return s.discardCASVolumes(ctx, session, hv, true)
	case phaseAbandoned:
		return s.discardCASVolumes(ctx, session, hv, true)
	case "":
	default:
		return abandon("a crash interrupted the merge, so its state cannot be told")
	}

	deadline := hv.Intent.At.Add(publishWindow)
	if !s.now().Before(deadline) {
		return abandon("the publication did not complete within its window")
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if !s.sessionKindAllowed(ctx, session, hv.Kind) {
		return abandon("the cache is disabled for this repository")
	}

	journal.Phase = phaseSnapshotting
	if err := s.persistSession(session); err != nil {
		return false, err
	}
	merged, consumed, err := s.mergeCASVolume(ctx, session, hv)
	if err != nil {
		return abandon("the merge failed: " + err.Error())
	}
	journal.Phase, journal.Consumed = phasePublished, consumed
	s.log.Info("published a content-addressed cache a job wrote", "instance", session.instance,
		"kind", hv.Kind, "generation", merged)
	if err := s.persistSession(session); err != nil {
		return false, err
	}
	if consumed {
		return true, nil
	}

	return s.discardCASVolumes(ctx, session, hv, true)
}

// discardCASVolumes lets go of everything a host volume holds: the merge's
// mount points, the merge clone, and the session's own clone unless a snapshot
// took it (own false). Done only when every step succeeded, so a failed one
// keeps the record for the next pass.
func (s *CacheService) discardCASVolumes(
	ctx context.Context, session *cacheSession, hv *hostVolume, own bool,
) (bool, error) {
	base := s.casMountPath(session, hv.Kind)
	err := errors.Join(s.actionIO.Unmount(ctx, base+".source"), s.actionIO.Unmount(ctx, base+".merge"))
	if err == nil && hv.Merge != nil {
		err = s.store.Discard(ctx, *hv.Merge)
	}
	if err == nil && own {
		err = s.store.Discard(ctx, hv.Volume)
	}

	return err == nil, err
}

// lockWithin takes l for writing, giving up after wait or when ctx ends.
func lockWithin(ctx context.Context, l *sync.RWMutex, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for {
		if l.TryLock() {
			return true
		}
		if ctx.Err() != nil || time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// mergeCASVolume publishes a session's objects into the newest generation of
// its key, under the key's writer.
//
// WHEN NOTHING HAS BEEN PUBLISHED SINCE THE SESSION CLONED, the session's own
// volume is the newest state and is snapshotted as it is. Otherwise the newest
// generation is cloned, the session's objects it lacks are copied in (an
// object's name is its content, so one already there is the same object; an
// action result already there is kept, because either is a valid answer), and
// that clone is published. Disjoint writes of concurrent jobs therefore both
// survive, which a whole-volume last-write-wins would lose.
//
// consumed says the session's own clone became the published generation, so
// there is nothing of it left to discard. Every failure leaves what it made to
// the caller's discard: the merge clone is recorded before it is mounted.
func (s *CacheService) mergeCASVolume(
	ctx context.Context, session *cacheSession, hv *hostVolume,
) (generation string, consumed bool, err error) {
	key := hv.Volume.Key
	lease, fence, err := s.store.AcquireWriter(ctx, key, session.instance+"/"+string(hv.Kind),
		cacheWriterTTL)
	if err != nil {
		return "", false, fmt.Errorf("acquire writer: %w", err)
	}
	release := func(err error) error { return errors.Join(err, s.releaseWriter(ctx, lease, fence)) }
	// THE KILL SWITCH IS ASKED AGAIN IMMEDIATELY BEFORE THE POINTER MOVES, after
	// the writer's wait and the copy, which can both be long.
	publish := func(current string, candidate storecontract.Candidate) error {
		if !s.sessionKindAllowed(ctx, session, hv.Kind) {
			return release(errors.New("the cache was disabled before its publication"))
		}
		if err := s.store.PublishCAS(ctx, key, current, candidate, lease, fence); err != nil {
			return release(fmt.Errorf("publish: %w", err))
		}

		return nil
	}

	current, currentErr := s.store.Current(ctx, key)
	if currentErr != nil && !errors.Is(currentErr, storecontract.ErrMiss) {
		return "", false, release(currentErr)
	}
	if current == hv.Volume.Generation || (hv.Supersedes != "" && current == hv.Supersedes) {
		candidate, err := s.store.Snapshot(ctx, hv.Volume)
		if err != nil {
			return "", false, release(fmt.Errorf("snapshot: %w", err))
		}
		if err := publish(current, candidate); err != nil {
			return "", false, err
		}

		return candidate.Generation, true, nil
	}

	latest, err := s.store.Clone(ctx, key, current)
	if err != nil {
		return "", false, release(fmt.Errorf("clone the newest generation: %w", err))
	}
	hv.Merge = &latest
	if err := s.persistSession(session); err != nil {
		return "", false, release(errors.Join(err, s.store.Discard(ctx, latest)))
	}
	source := s.casMountPath(session, hv.Kind) + ".source"
	target := s.casMountPath(session, hv.Kind) + ".merge"
	if err := s.actionIO.MountReadOnly(ctx, hv.Volume.Device, source); err != nil {
		return "", false, release(err)
	}
	if err := s.actionIO.MountWritable(ctx, latest.Device, target); err != nil {
		return "", false, release(err)
	}
	if err := copyMissingObjects(ctx, source, target); err != nil {
		return "", false, release(err)
	}
	if err := errors.Join(s.actionIO.Unmount(ctx, source), s.actionIO.Unmount(ctx, target)); err != nil {
		return "", false, release(err)
	}
	candidate, err := s.store.Snapshot(ctx, latest)
	if err != nil {
		return "", false, release(fmt.Errorf("snapshot: %w", err))
	}
	if err := publish(current, candidate); err != nil {
		return "", false, err
	}
	// THE MERGE CLONE BECAME THE GENERATION; the session's own clone is what is
	// left to discard.
	hv.Merge = nil

	return candidate.Generation, false, nil
}

// copyMissingObjects copies every object and action result in source that
// target lacks. Only regular files under the two tables are copied.
func copyMissingObjects(ctx context.Context, source, target string) error {
	for _, table := range []string{"ac", "cas"} {
		root := filepath.Join(source, table)
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) && path == root {
					return fs.SkipDir
				}

				return err
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !entry.Type().IsRegular() || strings.HasPrefix(entry.Name(), ".") {
				return nil
			}
			relative, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			destination := filepath.Join(target, relative)
			if _, err := os.Lstat(destination); err == nil {
				return nil
			}

			return copyFile(path, destination)
		})
		if err != nil && !errors.Is(err, fs.SkipDir) {
			return fmt.Errorf("merge %s: %w", table, err)
		}
	}

	return nil
}

func copyFile(from, to string) error {
	if err := os.MkdirAll(filepath.Dir(to), 0o700); err != nil {
		return err
	}
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	temporary, err := os.CreateTemp(filepath.Dir(to), ".merge-")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	_, copyErr := io.Copy(temporary, in)
	if err := errors.Join(copyErr, temporary.Close()); err != nil {
		return err
	}

	return os.Rename(temporary.Name(), to)
}

// remoteAPISession carries an authenticated session from the listener to the
// Remote Execution API's handlers, which gRPC runs on the request's context.
type remoteAPISession struct{}

func (s *CacheService) serveRemoteAPI(w http.ResponseWriter, r *http.Request, session *cacheSession) {
	ctx, cancel := extendTransfer(w, r)
	defer cancel()
	s.remoteAPI.ServeHTTP(w, r.WithContext(context.WithValue(ctx, remoteAPISession{}, session)))
}

// openRemoteAPIVolume is the Bazel volume of the call's session: the Remote
// Execution API serves the bazel cache only, whichever client speaks it.
func (s *CacheService) openRemoteAPIVolume(ctx context.Context, write bool) (reapi.Volume, error) {
	session, ok := ctx.Value(remoteAPISession{}).(*cacheSession)
	if !ok || session == nil {
		return nil, reapi.ErrEnded
	}
	handle, err := s.openCASHandle(ctx, session, config.CacheBazel, write)
	if err != nil {
		return nil, err
	}

	return handle, nil
}
