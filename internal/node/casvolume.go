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
	"time"

	"golang.org/x/sys/unix"

	"github.com/junioryono/billet/internal/config"
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

	// io is held for reading by every transfer and for writing by whatever
	// unmounts the volume, so no transfer runs on a volume being taken away.
	io sync.RWMutex
}

func (s *CacheService) casMountPath(session *cacheSession, kind config.CacheKind) string {
	return filepath.Join(s.rootState, "cas-volumes", session.token, string(kind))
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
	hv := &hostVolume{Kind: kind, Volume: volume}
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

	return hv, nil
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

	select {
	case session.casAdmit <- struct{}{}:
		defer func() { <-session.casAdmit }()
	default:
		http.Error(w, "too many concurrent cache transfers", http.StatusTooManyRequests)

		return
	}

	// A TRANSFER MAY OUTLIVE THE LISTENER'S READ TIMEOUT, which is sized for
	// the small requests the rest of the API makes.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(casRequestLife))
	_ = controller.SetWriteDeadline(time.Now().Add(casRequestLife))
	ctx, cancel := context.WithTimeout(r.Context(), casRequestLife)
	defer cancel()

	if err := lockCacheSession(ctx, session); err != nil {
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}
	if session.closed {
		session.mu.Unlock()
		http.Error(w, "cache session has ended", http.StatusGone)

		return
	}
	hv, err := s.casVolume(ctx, session, kind)
	session.mu.Unlock()
	if errors.Is(err, errCacheOff) {
		http.Error(w, err.Error(), http.StatusForbidden)

		return
	}
	if err != nil {
		s.log.Warn("a content-addressed cache is unavailable; the build continues uncached",
			"instance", session.instance, "kind", kind, "error", err)
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}

	hv.io.RLock()
	defer hv.io.RUnlock()
	if !hv.Mounted {
		http.Error(w, "cache session has ended", http.StatusGone)

		return
	}
	path := filepath.Join(s.casMountPath(session, kind), table, digest[:2], digest)

	switch r.Method {
	case http.MethodGet, http.MethodHead:
		serveCASObject(w, r, path)
	case http.MethodPut:
		limit := int64(casResultLimit)
		if table == "cas" {
			limit = volumeCeiling(sessionSetting(session, kind))
		}
		if err := s.storeCASObject(ctx, r.Body, path, table, digest, limit,
			s.casMountPath(session, kind)); err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, errCASFull) {
				status = http.StatusInsufficientStorage
			}
			http.Error(w, err.Error(), status)

			return
		}
		s.markCASDirty(session, hv)
		w.WriteHeader(http.StatusOK)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

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

// serveCASObject writes a stored object, opened without following a link,
// though nothing but the node writes the volume.
func serveCASObject(w http.ResponseWriter, r *http.Request, path string) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		http.NotFound(w, r)

		return
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		http.NotFound(w, r)

		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, "", time.Time{}, file)
}

var errCASFull = errors.New("the cache volume is full; this object is not cached")

// storeCASObject writes one object through a temporary file and a rename, so a
// reader sees a whole object or none, and refuses content that does not hash
// to its name.
func (s *CacheService) storeCASObject(
	ctx context.Context, body io.Reader, path, table, digest string, limit int64, root string,
) error {
	if full, err := casFull(root); err != nil || full {
		return errCASFull
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
		return errors.New("the object's content does not hash to its name")
	}

	return os.Rename(temporary.Name(), path)
}

// casFull reports whether a volume has passed the fraction it may fill.
func casFull(root string) (bool, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(root, &stat); err != nil {
		return false, err
	}
	total := float64(stat.Blocks)
	if total == 0 {
		return false, nil
	}

	return (total-float64(stat.Bavail))/total > casFullFraction, nil
}

func (s *CacheService) markCASDirty(session *cacheSession, hv *hostVolume) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if hv.Dirty || session.finished {
		return
	}
	hv.Dirty = true
	if err := s.persistSession(session); err != nil {
		s.log.Warn("could not record that a cache volume was written", "instance",
			session.instance, "kind", hv.Kind, "error", err)
	}
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
	hv.io.Lock()
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
		return true, s.store.Discard(ctx, hv.Volume)
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

		return true, errors.Join(s.persistSession(session), s.store.Discard(ctx, hv.Volume))
	}
	switch journal.Phase {
	case phasePublished:
		return true, nil
	case phaseAbandoned:
		return true, s.store.Discard(ctx, hv.Volume)
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
	if !s.kindAllowed(ctx, hv.Kind, session.cacheOwner(), session.cacheRepository()) {
		return abandon("the cache is disabled for this repository")
	}

	journal.Phase = phaseSnapshotting
	if err := s.persistSession(session); err != nil {
		return false, err
	}
	merged, err := s.mergeCASVolume(ctx, session, hv)
	if err != nil {
		return abandon("the merge failed: " + err.Error())
	}
	journal.Phase = phasePublished
	s.log.Info("published a content-addressed cache a job wrote", "instance", session.instance,
		"kind", hv.Kind, "generation", merged)

	return true, s.persistSession(session)
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
func (s *CacheService) mergeCASVolume(
	ctx context.Context, session *cacheSession, hv *hostVolume,
) (string, error) {
	key := hv.Volume.Key
	lease, fence, err := s.store.AcquireWriter(ctx, key, session.instance+"/"+string(hv.Kind),
		cacheWriterTTL)
	if err != nil {
		return "", fmt.Errorf("acquire writer: %w", err)
	}
	release := func(err error) error { return errors.Join(err, s.releaseWriter(ctx, lease, fence)) }

	current, currentErr := s.store.Current(ctx, key)
	if currentErr != nil && !errors.Is(currentErr, storecontract.ErrMiss) {
		return "", release(currentErr)
	}
	if current == hv.Volume.Generation {
		candidate, err := s.store.Snapshot(ctx, hv.Volume)
		if err != nil {
			return "", release(fmt.Errorf("snapshot: %w", err))
		}
		if err := s.store.PublishCAS(ctx, key, current, candidate, lease, fence); err != nil {
			return "", release(fmt.Errorf("publish: %w", err))
		}

		return candidate.Generation, nil
	}

	latest, err := s.store.Clone(ctx, key, current)
	if err != nil {
		return "", release(fmt.Errorf("clone the newest generation: %w", err))
	}
	source := s.casMountPath(session, hv.Kind) + ".source"
	target := s.casMountPath(session, hv.Kind) + ".merge"
	cleanup := func(err error) error {
		return errors.Join(err, s.actionIO.Unmount(ctx, source), s.actionIO.Unmount(ctx, target),
			s.store.Discard(ctx, latest))
	}
	if err := s.actionIO.MountReadOnly(ctx, hv.Volume.Device, source); err != nil {
		return "", release(cleanup(err))
	}
	if err := s.actionIO.MountWritable(ctx, latest.Device, target); err != nil {
		return "", release(cleanup(err))
	}
	if err := copyMissingObjects(ctx, source, target); err != nil {
		return "", release(cleanup(err))
	}
	if err := errors.Join(s.actionIO.Unmount(ctx, source), s.actionIO.Unmount(ctx, target)); err != nil {
		return "", release(cleanup(err))
	}
	candidate, err := s.store.Snapshot(ctx, latest)
	if err != nil {
		return "", release(cleanup(fmt.Errorf("snapshot: %w", err)))
	}
	if err := s.store.PublishCAS(ctx, key, current, candidate, lease, fence); err != nil {
		return "", release(fmt.Errorf("publish: %w", err))
	}

	return candidate.Generation, s.store.Discard(ctx, hv.Volume)
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
