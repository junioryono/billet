package ceph

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"time"

	storecontract "github.com/junioryono/billet/internal/store"
)

const (
	cacheIndexName = ".cache-index"
	// CacheLockStaleAfter bounds recovery from a cache-index holder that dies.
	// Ten minutes spans twenty heartbeat intervals and is far past the ordinary
	// metadata critical sections. A holder older than this is still never broken
	// while its heartbeat moves, including a longer eviction pass.
	CacheLockStaleAfter  = 10 * time.Minute
	cacheLockRetryDelay  = 250 * time.Millisecond
	cacheLockRecoveryGap = 30 * time.Second
	cacheLockWaitLimit   = CacheLockStaleAfter + HeartbeatObservation + cacheLockRecoveryGap
	cacheVolumeTTL       = 7 * time.Hour
	cacheMetaPrefix      = "billet.cache."
	filesystemProbeLimit = 8 << 10
	maxCacheCloneDepth   = 8
	cacheCompactionLimit = 10 * time.Minute
)

var _ storecontract.Store = (*Client)(nil)

type filesystemVerifier func(context.Context, string) (storecontract.Filesystem, error)

func withFilesystemVerifier(verify filesystemVerifier) Option {
	return func(c *Client) {
		if verify != nil {
			c.verify = verify
		}
	}
}

type cachePointer struct {
	Key            string                     `json:"key,omitempty"`
	Generation     string                     `json:"generation"`
	Handle         string                     `json:"handle"`
	UsedAt         time.Time                  `json:"used_at"`
	PublishedAt    time.Time                  `json:"published_at,omitempty"`
	RetentionHours int                        `json:"retention_hours,omitempty"`
	WriterID       string                     `json:"writer_id,omitempty"`
	Fence          storecontract.FencingToken `json:"fence,omitempty"`
	Previous       string                     `json:"previous,omitempty"`
}

// Match finds the exact key or the newest pointer under the first restore prefix.
func (c *Client) Match(
	ctx context.Context,
	exact string,
	restorePrefixes []string,
) (string, string, error) {
	if err := checkCacheKey(exact); err != nil {
		return "", "", err
	}
	for _, prefix := range restorePrefixes {
		if err := checkCacheKey(prefix); err != nil {
			return "", "", err
		}
	}

	var matchedKey, generation string
	err := c.withCacheLock(ctx, time.Now(), func(time.Time) error {
		var exactPointer cachePointer
		ok, err := c.readJSON(ctx, pointerKey(exact), &exactPointer)
		if err != nil {
			return err
		}
		if ok {
			if exactPointer.Generation == "" ||
				(exactPointer.Key != "" && exactPointer.Key != exact) {
				return fmt.Errorf("ceph: cache %q has an invalid pointer identity", exact)
			}
			matchedKey, generation = exact, exactPointer.Generation

			return nil
		}

		metadata, err := c.cacheIndexMetadata(ctx)
		if err != nil {
			return err
		}
		for _, prefix := range restorePrefixes {
			var newest time.Time
			for key, value := range metadata {
				if !strings.HasPrefix(key, cacheMetaPrefix+"pointer.") {
					continue
				}
				var pointer cachePointer
				if json.Unmarshal([]byte(value), &pointer) != nil ||
					pointer.Key == "" || pointer.Generation == "" ||
					!strings.HasPrefix(pointer.Key, prefix) {
					continue
				}
				published := pointer.PublishedAt
				if published.IsZero() {
					published = pointer.UsedAt
				}
				if matchedKey == "" || published.After(newest) ||
					(published.Equal(newest) && pointer.Key < matchedKey) {
					matchedKey, generation, newest = pointer.Key, pointer.Generation, published
				}
			}
			if matchedKey != "" {
				return nil
			}
		}

		return fmt.Errorf("%w: site has no matching generation for cache %q",
			storecontract.ErrMiss, exact)
	})

	return matchedKey, generation, err
}

type cacheWriter struct {
	Lease  storecontract.WriterLease  `json:"lease"`
	Fence  storecontract.FencingToken `json:"fence"`
	Holder string                     `json:"holder"`
}

type cacheActive struct {
	Key        string    `json:"key"`
	Handle     string    `json:"handle"`
	Generation string    `json:"generation"`
	Expires    time.Time `json:"expires"`
}

func cacheDigest(key string) string {
	digest := sha256.Sum256([]byte(key))

	return hex.EncodeToString(digest[:])
}

func pointerKey(key string) string { return cacheMetaPrefix + "pointer." + cacheDigest(key) }
func writerKey(key string) string  { return cacheMetaPrefix + "writer." + cacheDigest(key) }
func fenceKey(key string) string   { return cacheMetaPrefix + "fence." + cacheDigest(key) }
func activeKey(id string) string   { return cacheMetaPrefix + "active." + id }
func generationKey(key, generation string) string {
	return cacheMetaPrefix + "generation." + cacheDigest(key) + "." + cacheDigest(generation)
}

func (c *Client) cacheIndex() string { return c.cfg.CachePool + "/" + cacheIndexName }

// Current reports the generation named by the cache pointer.
func (c *Client) Current(ctx context.Context, key string) (string, error) {
	if err := checkCacheKey(key); err != nil {
		return "", err
	}

	var pointer cachePointer
	err := c.withCacheLock(ctx, time.Now(), func(time.Time) error {
		ok, err := c.readJSON(ctx, pointerKey(key), &pointer)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("%w: site has no generation for cache %q", storecontract.ErrMiss, key)
		}

		return nil
	})

	return pointer.Generation, err
}

func cacheName(prefix string, now time.Time) (string, error) {
	var nonce [12]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("ceph: make a cache volume identity: %w", err)
	}

	return fmt.Sprintf("cache-%s-%d-%s", prefix, now.UTC().Unix(), hex.EncodeToString(nonce[:])), nil
}

func checkCacheKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("ceph: a cache volume needs a key")
	}

	if strings.TrimSpace(key) != key || strings.ContainsRune(key, 0) {
		return errors.New("ceph: a cache key cannot have surrounding whitespace or a NUL byte")
	}

	return nil
}

func (c *Client) withCacheLock(
	ctx context.Context,
	now time.Time,
	fn func(time.Time) error,
) error {
	lockCtx, cancelLock := context.WithTimeout(ctx, cacheLockWaitLimit)
	defer cancelLock()

	started := time.Now()
	elapsed := func() time.Duration {
		if c.cacheLockElapsed != nil {
			return c.cacheLockElapsed()
		}

		return time.Since(started)
	}
	attemptAt := now
	var lock *PublishLock
	for {
		cookie, err := publishCookie(attemptAt)
		if err != nil {
			return err
		}
		lock, err = c.takeLock(lockCtx, c.cacheIndex(), cookie, attemptAt, CacheLockStaleAfter)
		if err == nil {
			break
		}
		if !errors.Is(err, errLockContended) {
			return fmt.Errorf("ceph: take the cache index lock: %w", err)
		}

		delay := cacheLockRetryDelay
		if c.cacheLockRetry > 0 {
			delay = c.cacheLockRetry
		}
		timer := time.NewTimer(delay)
		select {
		case <-lockCtx.Done():
			timer.Stop()

			return fmt.Errorf("ceph: wait for the cache index lock: %w", lockCtx.Err())
		case <-timer.C:
		}
		attemptAt = now.Add(elapsed())
	}

	lockedAt := now.Add(elapsed())
	workErr := fn(lockedAt)
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
	defer cancel()

	return errors.Join(workErr, lock.Release(releaseCtx))
}

func (c *Client) metaGet(
	ctx context.Context,
	image, key string,
) (string, bool, error) {
	out, err := c.rbdCmd(ctx, false, "image-meta", "get", image, key)
	if err != nil {
		if isNoSuchFile(err) {
			return "", false, nil
		}

		return "", false, fmt.Errorf("ceph: read cache metadata %s: %w", key, err)
	}

	return strings.TrimSpace(string(out)), true, nil
}

func (c *Client) metaSet(ctx context.Context, image, key, value string) error {
	if _, err := c.rbdCmd(ctx, false, "image-meta", "set", image, key, value); err != nil {
		return fmt.Errorf("ceph: write cache metadata %s: %w", key, err)
	}

	return nil
}

func (c *Client) metaRemove(ctx context.Context, image, key string) error {
	if _, err := c.rbdCmd(ctx, false, "image-meta", "remove", image, key); err != nil &&
		!isNoSuchFile(err) {
		return fmt.Errorf("ceph: remove cache metadata %s: %w", key, err)
	}

	return nil
}

func (c *Client) readJSON(ctx context.Context, key string, into any) (bool, error) {
	value, ok, err := c.metaGet(ctx, c.cacheIndex(), key)
	if err != nil || !ok {
		return ok, err
	}

	if err := json.Unmarshal([]byte(value), into); err != nil {
		return false, fmt.Errorf("ceph: cache metadata %s is not valid json", key)
	}

	return true, nil
}

func (c *Client) writeJSON(ctx context.Context, key string, value any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("ceph: encode cache metadata %s: %w", key, err)
	}

	return c.metaSet(ctx, c.cacheIndex(), key, string(encoded))
}

// Create maps a new unformatted cache volume.
func (c *Client) Create(
	ctx context.Context,
	key string,
	sizeBytes int64,
) (storecontract.Volume, error) {
	if err := checkCacheKey(key); err != nil {
		return storecontract.Volume{}, err
	}

	if sizeBytes <= 0 {
		return storecontract.Volume{}, fmt.Errorf("ceph: cache %q asks for %d bytes", key, sizeBytes)
	}

	name, err := cacheName("v", time.Now())
	if err != nil {
		return storecontract.Volume{}, err
	}

	mebibytes := (sizeBytes + (1 << 20) - 1) / (1 << 20)
	if mebibytes <= 0 {
		return storecontract.Volume{}, fmt.Errorf("ceph: cache %q has an unrepresentable size", key)
	}

	handle := c.cfg.CachePool + "/" + name
	if _, err := c.rbdCmd(ctx, false, "create", handle, "--size", strconv.FormatInt(mebibytes, 10)+"M",
		"--image-feature", "layering"); err != nil {
		return storecontract.Volume{}, fmt.Errorf("ceph: create cache volume for %q: %w", key, err)
	}

	device, err := c.mapCache(ctx, handle)
	if err != nil {
		return storecontract.Volume{}, errors.Join(err, c.removeCacheImage(ctx, handle))
	}

	return storecontract.Volume{Key: key, Handle: handle, Device: device}, nil
}

func (c *Client) mapCache(ctx context.Context, handle string) (string, error) {
	out, err := c.rbdCmd(ctx, false, "device", "map", handle)
	if err != nil {
		return "", fmt.Errorf("ceph: map cache volume %s: %w", handle, err)
	}

	device := strings.TrimSpace(string(out))
	if !strings.HasPrefix(device, "/dev/rbd") {
		return "", fmt.Errorf("ceph: %s answered %s when asked to map a cache volume", c.bin,
			bounded(device))
	}

	return device, nil
}

// AcquireWriter issues a short-lived cache writer lease and a newer fencing token.
func (c *Client) AcquireWriter(
	ctx context.Context,
	key, holder string,
	ttl time.Duration,
) (storecontract.WriterLease, storecontract.FencingToken, error) {
	return c.acquireWriterAt(ctx, key, holder, ttl, time.Now())
}

func (c *Client) acquireWriterAt(
	ctx context.Context,
	key, holder string,
	ttl time.Duration,
	now time.Time,
) (storecontract.WriterLease, storecontract.FencingToken, error) {
	if err := checkCacheKey(key); err != nil {
		return storecontract.WriterLease{}, 0, err
	}

	if strings.TrimSpace(holder) == "" || ttl <= 0 {
		return storecontract.WriterLease{}, 0, errors.New("ceph: a cache writer needs a holder and a positive lifetime")
	}

	var issued cacheWriter
	err := c.withCacheLock(ctx, now, func(lockedAt time.Time) error {
		var current cacheWriter
		if ok, err := c.readJSON(ctx, writerKey(key), &current); err != nil {
			return err
		} else if ok && lockedAt.Before(current.Lease.Expires) {
			return fmt.Errorf("%w: cache %q already has a live writer", storecontract.ErrConflict, key)
		}

		value, ok, err := c.metaGet(ctx, c.cacheIndex(), fenceKey(key))
		if err != nil {
			return err
		}

		var fence uint64
		if ok {
			fence, err = strconv.ParseUint(value, 10, 64)
			if err != nil {
				return fmt.Errorf("ceph: cache %q has an invalid fencing token", key)
			}
		}

		if fence == math.MaxUint64 {
			return fmt.Errorf("ceph: cache %q exhausted its fencing tokens", key)
		}

		id, err := cacheName("w", lockedAt)
		if err != nil {
			return err
		}

		issued = cacheWriter{
			Lease:  storecontract.WriterLease{Key: key, ID: id, Expires: lockedAt.Add(ttl)},
			Fence:  storecontract.FencingToken(fence + 1),
			Holder: holder,
		}

		if err := c.metaSet(ctx, c.cacheIndex(), fenceKey(key),
			strconv.FormatUint(uint64(issued.Fence), 10)); err != nil {
			return err
		}

		return c.writeJSON(ctx, writerKey(key), issued)
	})

	return issued.Lease, issued.Fence, err
}

// Snapshot verifies a quiesced volume and turns it into an immutable candidate.
func (c *Client) Snapshot(
	ctx context.Context,
	volume storecontract.Volume,
) (storecontract.Candidate, error) {
	return c.snapshotAt(ctx, volume, time.Now())
}

func (c *Client) snapshotAt(
	ctx context.Context,
	volume storecontract.Volume,
	now time.Time,
) (storecontract.Candidate, error) {
	if err := checkCacheKey(volume.Key); err != nil {
		return storecontract.Candidate{}, err
	}

	if !strings.HasPrefix(volume.Handle, c.cfg.CachePool+"/cache-v-") || volume.Device == "" {
		return storecontract.Candidate{}, errors.New("ceph: a candidate must come from a mapped cache volume")
	}

	filesystem, err := c.verify(ctx, volume.Device)
	if err != nil {
		return storecontract.Candidate{}, fmt.Errorf("ceph: verify cache %q before publication: %w",
			volume.Key, err)
	}

	if err := filesystem.Valid(); err != nil {
		return storecontract.Candidate{}, err
	}

	depth, err := c.nextCacheCloneDepth(ctx, volume)
	if err != nil {
		return storecontract.Candidate{}, err
	}
	if err := c.withCacheLock(ctx, now, func(lockedAt time.Time) error {
		now = lockedAt

		return c.metaSet(ctx, volume.Handle, cacheMetaPrefix+"used_at",
			now.UTC().Format(time.RFC3339Nano))
	}); err != nil {
		return storecontract.Candidate{}, err
	}
	if err := c.unmapDevice(ctx, volume.Device, volume.Handle); err != nil {
		return storecontract.Candidate{}, err
	}

	generationName, err := cacheName("s", now)
	if err != nil {
		return storecontract.Candidate{}, c.cleanupSnapshotFailure(ctx, volume, "", "", err)
	}

	_, generation, _ := strings.Cut(generationName, "cache-s-")
	stage := volume.Handle + "@" + generation
	if _, err := c.rbdCmd(ctx, false, "snap", "create", stage); err != nil {
		return storecontract.Candidate{}, c.cleanupSnapshotFailure(ctx, volume, stage, "",
			fmt.Errorf("ceph: snapshot cache %q: %w", volume.Key, err))
	}

	candidateName, err := cacheName("g", now)
	if err != nil {
		return storecontract.Candidate{}, c.cleanupSnapshotFailure(ctx, volume, stage, "", err)
	}

	handle := c.cfg.CachePool + "/" + candidateName
	if depth >= maxCacheCloneDepth {
		if err := c.copyCacheCandidate(ctx, stage, handle, generation); err != nil {
			return storecontract.Candidate{}, c.cleanupSnapshotFailure(
				ctx, volume, stage, handle, err,
			)
		}
		depth = 0
	} else {
		if _, err := c.rbdCmd(ctx, false, "clone", stage, handle); err != nil {
			return storecontract.Candidate{}, c.cleanupSnapshotFailure(ctx, volume, stage, handle,
				fmt.Errorf("ceph: clone immutable candidate for %q: %w", volume.Key, err))
		}

		if _, err := c.rbdCmd(ctx, false, "snap", "create", handle+"@"+generation); err != nil {
			return storecontract.Candidate{}, c.cleanupSnapshotFailure(ctx, volume, stage, handle,
				fmt.Errorf("ceph: freeze immutable candidate for %q: %w", volume.Key, err))
		}
	}

	for key, value := range map[string]string{
		cacheMetaPrefix + "key":        cacheDigest(volume.Key),
		cacheMetaPrefix + "generation": generation,
		cacheMetaPrefix + "lineage":    strconv.Itoa(depth),
		cacheMetaPrefix + "used_at":    now.UTC().Format(time.RFC3339Nano),
	} {
		if err := c.metaSet(ctx, handle, key, value); err != nil {
			return storecontract.Candidate{}, c.cleanupSnapshotFailure(ctx, volume, stage, handle, err)
		}
	}

	if _, err := c.rbdCmd(ctx, false, "snap", "rm", stage); err != nil {
		return storecontract.Candidate{}, c.cleanupSnapshotFailure(ctx, volume, stage, handle,
			fmt.Errorf("ceph: remove cache staging snapshot: %w", err))
	}

	if err := c.retireCacheImage(ctx, volume.Handle); err != nil {
		return storecontract.Candidate{}, c.cleanupSnapshotFailure(ctx, volume, "", handle, err)
	}
	if volume.Lease.ID != "" {
		if err := c.withCacheLock(ctx, now, func(lockedAt time.Time) error {
			var pointer cachePointer
			ok, err := c.readJSON(ctx, pointerKey(volume.Key), &pointer)
			if err != nil {
				return err
			}
			// REFRESH BEFORE RELEASING THE ACTIVE LEASE. Eviction takes this same
			// lock, so the expected pointer remains protected through the bounded
			// Snapshot-to-PublishCAS handoff even after a long-running job.
			if ok && pointer.Generation == volume.Generation {
				pointer.UsedAt = lockedAt.UTC()
				if pointer.RetentionHours == 0 {
					pointer.RetentionHours = retentionHours(volume.Key)
				}
				if err := c.writeJSON(ctx, pointerKey(volume.Key), pointer); err != nil {
					return err
				}
			}

			return c.metaRemove(ctx, c.cacheIndex(), activeKey(volume.Lease.ID))
		}); err != nil {
			return storecontract.Candidate{}, c.cleanupSnapshotFailure(ctx, volume, "", handle, err)
		}
	}

	return storecontract.Candidate{
		Key: volume.Key, Generation: generation, Handle: handle, Filesystem: filesystem,
	}, nil
}

func (c *Client) nextCacheCloneDepth(ctx context.Context, volume storecontract.Volume) (int, error) {
	if volume.Generation == "" {
		return 1, nil
	}

	depth := maxCacheCloneDepth
	err := c.withCacheLock(ctx, time.Now(), func(time.Time) error {
		value, found, err := c.metaGet(ctx, volume.Handle, cacheMetaPrefix+"lineage")
		if err != nil {
			return err
		}
		if !found {
			// A writable clone made before lineage was recorded is compacted rather
			// than assigned a guessed depth.
			return nil
		}

		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 || parsed > maxCacheCloneDepth {
			return fmt.Errorf("ceph: cache %q generation %q has invalid lineage depth %q",
				volume.Key, volume.Generation, bounded(value))
		}
		depth = parsed + 1

		return nil
	})

	return depth, err
}

func (c *Client) copyCacheCandidate(
	ctx context.Context,
	source, destination, generation string,
) error {
	// COPY DIRECTLY FROM THE VERIFIED STAGING SNAPSHOT. Making an intermediate
	// clone first would cross the depth limit before the copy had a chance to
	// compact it. The published pointer remains unchanged throughout.
	if err := c.copyCacheImage(ctx, source, destination); err != nil {
		return err
	}
	if _, err := c.rbdCmd(ctx, false, "snap", "create", destination+"@"+generation); err != nil {
		return fmt.Errorf("ceph: freeze compacted cache candidate: %w", err)
	}

	return nil
}

func (c *Client) copyCacheImage(ctx context.Context, source, destination string) error {
	copyCtx, cancelCopy := context.WithTimeout(ctx, cacheCompactionLimit)
	defer cancelCopy()
	if _, err := c.run(copyCtx, c.bin, append(c.identity(), "cp", source, destination)); err != nil {
		return fmt.Errorf("ceph: compact cache lineage: %w", err)
	}

	return nil
}

func (c *Client) cleanupSnapshotFailure(
	ctx context.Context,
	volume storecontract.Volume,
	stage, candidate string,
	primary error,
) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
	defer cancel()

	var failures []error
	if candidate != "" {
		if _, err := c.rbdCmd(cleanupCtx, false, "snap", "purge", candidate); err != nil &&
			!isNoSuchFile(err) {
			failures = append(failures, err)
		}
		if err := c.removeCacheImage(cleanupCtx, candidate); err != nil {
			failures = append(failures, err)
		}
	}
	if stage != "" {
		if _, err := c.rbdCmd(cleanupCtx, false, "snap", "rm", stage); err != nil &&
			!isNoSuchFile(err) {
			failures = append(failures, err)
		}
	}
	if err := c.removeCacheImage(cleanupCtx, volume.Handle); err != nil {
		failures = append(failures, err)
	}
	if volume.Lease.ID != "" {
		if err := c.withCacheLock(cleanupCtx, time.Now(), func(time.Time) error {
			return c.metaRemove(cleanupCtx, c.cacheIndex(), activeKey(volume.Lease.ID))
		}); err != nil {
			failures = append(failures, err)
		}
	}

	if cleanupErr := errors.Join(failures...); cleanupErr != nil {
		return errors.Join(primary, fmt.Errorf("ceph: clean up failed cache snapshot: %w", cleanupErr))
	}

	return primary
}

// PublishCAS atomically advances one cache pointer under the cluster-wide index lock.
func (c *Client) PublishCAS(
	ctx context.Context,
	key, expected string,
	candidate storecontract.Candidate,
	lease storecontract.WriterLease,
	fence storecontract.FencingToken,
) error {
	return c.publishCASAt(ctx, key, expected, candidate, lease, fence, time.Now())
}

func (c *Client) publishCASAt(
	ctx context.Context,
	key, expected string,
	candidate storecontract.Candidate,
	lease storecontract.WriterLease,
	fence storecontract.FencingToken,
	now time.Time,
) error {
	if err := checkCacheKey(key); err != nil {
		return err
	}

	if candidate.Key != key || candidate.Generation == "" ||
		!strings.HasPrefix(candidate.Handle, c.cfg.CachePool+"/cache-g-") {
		return errors.New("ceph: candidate does not belong to this cache key and pool")
	}

	if err := candidate.Filesystem.Valid(); err != nil {
		return err
	}

	if fence == 0 {
		return fmt.Errorf("%w: fencing token zero authorises nothing", storecontract.ErrConflict)
	}

	return c.withCacheLock(ctx, now, func(lockedAt time.Time) error {
		var current cachePointer
		pointerExists, err := c.readJSON(ctx, pointerKey(key), &current)
		if err != nil {
			return err
		}
		if pointerExists && current.Generation == candidate.Generation &&
			current.Handle == candidate.Handle && current.WriterID == lease.ID &&
			current.Fence == fence && current.Previous == expected {
			return c.metaRemove(ctx, c.cacheIndex(), writerKey(key))
		}
		if err := lease.ValidAt(key, lockedAt); err != nil {
			return fmt.Errorf("%w: %w", storecontract.ErrConflict, err)
		}

		var currentWriter cacheWriter
		ok, err := c.readJSON(ctx, writerKey(key), &currentWriter)
		if err != nil {
			return err
		}

		if !ok || currentWriter.Lease.ID != lease.ID ||
			!currentWriter.Lease.Expires.Equal(lease.Expires) || currentWriter.Fence != fence {
			return fmt.Errorf("%w: cache %q has a newer writer", storecontract.ErrConflict, key)
		}

		ok, err = c.readJSON(ctx, pointerKey(key), &current)
		if err != nil {
			return err
		}

		actual := ""
		if ok {
			actual = current.Generation
		}

		if actual != expected {
			return fmt.Errorf("%w: cache %q is at generation %q, not %q",
				storecontract.ErrConflict, key, actual, expected)
		}

		if _, err := c.rbdCmd(ctx, true, "info", candidate.Handle+"@"+candidate.Generation); err != nil {
			return fmt.Errorf("ceph: immutable candidate for cache %q is not present: %w", key, err)
		}

		pointer := cachePointer{
			Key:            key,
			Generation:     candidate.Generation,
			Handle:         candidate.Handle,
			UsedAt:         lockedAt.UTC(),
			PublishedAt:    lockedAt.UTC(),
			RetentionHours: retentionHours(key),
			WriterID:       lease.ID,
			Fence:          fence,
			Previous:       expected,
		}
		// THE GENERATION RECORD FIRST, THE POINTER LAST. A failure before the
		// pointer is an orphan candidate GC can remove; a pointer written first and
		// a record that then failed would return an error after changing the answer
		// readers see, so the caller could retry against an expectation that was no
		// longer true.
		if err := c.writeJSON(ctx, generationKey(key, candidate.Generation), pointer); err != nil {
			return err
		}

		if err := c.writeJSON(ctx, pointerKey(key), pointer); err != nil {
			return err
		}

		return c.metaRemove(ctx, c.cacheIndex(), writerKey(key))
	})
}

// Clone maps a writable clone of a current or explicitly named generation.
func (c *Client) Clone(
	ctx context.Context,
	key, generation string,
) (storecontract.Volume, error) {
	if err := checkCacheKey(key); err != nil {
		return storecontract.Volume{}, err
	}

	now := time.Now()
	var pointer cachePointer
	var active cacheActive
	var leaseID string
	cloneDepth := 0
	copySource := false

	err := c.withCacheLock(ctx, now, func(lockedAt time.Time) error {
		now = lockedAt

		metadataKey := pointerKey(key)
		if generation != "" {
			metadataKey = generationKey(key, generation)
		}

		ok, err := c.readJSON(ctx, metadataKey, &pointer)
		if err != nil {
			return err
		}

		if !ok {
			return fmt.Errorf("%w: site has no generation for cache %q", storecontract.ErrMiss, key)
		}

		value, found, err := c.metaGet(ctx, pointer.Handle, cacheMetaPrefix+"lineage")
		if err != nil {
			return err
		}
		if !found {
			// A generation from before lineage tracking may already be at Ceph's
			// hard clone-depth limit. Copy it before making another child.
			copySource = true
		} else {
			parsed, parseErr := strconv.Atoi(value)
			if parseErr != nil || parsed < 0 || parsed > maxCacheCloneDepth {
				return fmt.Errorf("ceph: cache %q generation %q has invalid lineage depth %q",
					key, pointer.Generation, bounded(value))
			}
			copySource = parsed >= maxCacheCloneDepth
			cloneDepth = parsed + 1
		}

		pointer.UsedAt = now.UTC()
		if pointer.RetentionHours == 0 {
			pointer.RetentionHours = retentionHours(key)
		}
		if err := c.writeJSON(ctx, metadataKey, pointer); err != nil {
			return err
		}
		if generation == "" {
			if err := c.writeJSON(ctx, generationKey(key, pointer.Generation), pointer); err != nil {
				return err
			}
		}

		leaseID, err = cacheName("a", now)
		if err != nil {
			return err
		}

		active = cacheActive{
			Key: key, Handle: pointer.Handle, Generation: pointer.Generation,
			Expires: now.Add(cacheVolumeTTL),
		}
		if err := c.writeJSON(ctx, activeKey(leaseID), active); err != nil {
			return err
		}

		return c.metaSet(ctx, pointer.Handle, cacheMetaPrefix+"used_at",
			now.UTC().Format(time.RFC3339Nano))
	})
	if err != nil {
		return storecontract.Volume{}, err
	}

	cloneName, err := cacheName("v", now)
	if err != nil {
		return storecontract.Volume{}, err
	}

	handle := c.cfg.CachePool + "/" + cloneName
	source := pointer.Handle + "@" + pointer.Generation
	if copySource {
		err = c.copyCacheImage(ctx, source, handle)
		cloneDepth = 0
	} else {
		_, err = c.rbdCmd(ctx, false, "clone", source, handle)
	}
	if err != nil {
		if isNoSuchFile(err) {
			return storecontract.Volume{}, c.cleanupCloneFailure(ctx, leaseID, handle,
				fmt.Errorf("%w: cache %q generation disappeared before clone",
					storecontract.ErrMiss, key))
		}

		return storecontract.Volume{}, c.cleanupCloneFailure(ctx, leaseID, handle,
			fmt.Errorf("ceph: materialize cache %q: %w", key, err))
	}
	if err := c.metaSet(ctx, handle, cacheMetaPrefix+"lineage", strconv.Itoa(cloneDepth)); err != nil {
		return storecontract.Volume{}, c.cleanupCloneFailure(ctx, leaseID, handle, err)
	}

	device, err := c.mapCache(ctx, handle)
	if err != nil {
		return storecontract.Volume{}, c.cleanupCloneFailure(ctx, leaseID, handle, err)
	}

	return storecontract.Volume{
		Key: key, Generation: pointer.Generation, Handle: handle, Device: device,
		Lease: storecontract.ActiveLease{ID: leaseID, Expires: active.Expires},
	}, nil
}

func (c *Client) cleanupCloneFailure(
	ctx context.Context,
	leaseID, handle string,
	primary error,
) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.wait)
	defer cancel()

	volume := storecontract.Volume{
		Handle: handle,
		Lease:  storecontract.ActiveLease{ID: leaseID},
	}
	cleanupErr := c.Discard(cleanupCtx, volume)
	if cleanupErr != nil {
		// A miss is safe to replace with a fresh volume only after cleanup
		// succeeded. Do not leave ErrMiss in the error chain when an orphan may
		// remain, or the attach path will hide this failure by creating again.
		return fmt.Errorf("ceph: clean up failed clone after %s: %w", primary.Error(), cleanupErr)
	}

	return primary
}

// RenewActive extends the protection of a mounted cache clone.
func (c *Client) RenewActive(
	ctx context.Context,
	volume storecontract.Volume,
	until time.Time,
) error {
	now := time.Now()
	if volume.Lease.ID == "" || !now.Before(until) {
		return errors.New("ceph: an active cache renewal needs an identity and a future expiry")
	}

	return c.withCacheLock(ctx, now, func(lockedAt time.Time) error {
		if !lockedAt.Before(until) {
			return errors.New("ceph: an active cache renewal needs an identity and a future expiry")
		}

		var active cacheActive
		ok, err := c.readJSON(ctx, activeKey(volume.Lease.ID), &active)
		if err != nil {
			return err
		}

		if !ok || active.Key != volume.Key || active.Generation != volume.Generation {
			return fmt.Errorf("%w: active cache lease no longer names this volume", storecontract.ErrConflict)
		}

		active.Expires = until.UTC()

		return c.writeJSON(ctx, activeKey(volume.Lease.ID), active)
	})
}

// Discard unmaps and removes a writable cache clone.
func (c *Client) Discard(ctx context.Context, volume storecontract.Volume) error {
	if volume.Handle == "" {
		return nil
	}

	name := strings.TrimPrefix(volume.Handle, c.cfg.CachePool+"/")
	if name == volume.Handle || !strings.HasPrefix(name, "cache-v-") {
		return errors.New("ceph: refusing to discard a cache volume outside the configured pool")
	}

	devices, err := c.mappedDevices(ctx, name)
	if err != nil {
		return err
	}

	for _, device := range devices {
		if err := c.unmapDevice(ctx, device, name); err != nil {
			return err
		}
	}

	if err := c.removeCacheImage(ctx, volume.Handle); err != nil {
		return err
	}

	if volume.Lease.ID != "" {
		return c.withCacheLock(ctx, time.Now(), func(time.Time) error {
			return c.metaRemove(ctx, c.cacheIndex(), activeKey(volume.Lease.ID))
		})
	}

	return nil
}

func (c *Client) removeCacheImage(ctx context.Context, handle string) error {
	if _, err := c.rbdCmd(ctx, false, "rm", handle); err != nil && !isNoSuchFile(err) {
		return fmt.Errorf("ceph: remove cache volume %s: %w", handle, err)
	}

	return nil
}

func (c *Client) retireCacheImage(ctx context.Context, handle string) error {
	if _, err := c.rbdCmd(ctx, false, "trash", "mv", handle); err != nil {
		return fmt.Errorf("ceph: retire cache volume %s: %w", handle, err)
	}

	return nil
}

// Evict removes old, unreferenced cache images under the same lock as publication.
func (c *Client) Evict(ctx context.Context, olderThan time.Duration) error {
	if olderThan <= 0 {
		return errors.New("ceph: cache eviction needs a positive inactivity age")
	}

	now := time.Now()

	return c.withCacheLock(ctx, now, func(now time.Time) error {
		metadata, err := c.cacheIndexMetadata(ctx)
		if err != nil {
			return err
		}

		protected := map[string]bool{}
		retention := map[string]time.Duration{}
		generationMetadata := map[string][]string{}
		for key, value := range metadata {
			if !strings.HasPrefix(key, cacheMetaPrefix+"active.") {
				continue
			}

			var active cacheActive
			if json.Unmarshal([]byte(value), &active) != nil || !now.Before(active.Expires) {
				if err := c.metaRemove(ctx, c.cacheIndex(), key); err != nil {
					return err
				}

				continue
			}

			protected[active.Handle] = true
		}
		for key, value := range metadata {
			switch {
			case strings.HasPrefix(key, cacheMetaPrefix+"pointer."):
				var pointer cachePointer
				if json.Unmarshal([]byte(value), &pointer) == nil && pointer.Handle != "" {
					age := retentionDuration(pointer, olderThan)
					retention[pointer.Handle] = age
					if protected[pointer.Handle] || now.Sub(pointer.UsedAt) < age {
						protected[pointer.Handle] = true
					} else if err := c.metaRemove(ctx, c.cacheIndex(), key); err != nil {
						return err
					}
				}
			case strings.HasPrefix(key, cacheMetaPrefix+"generation."):
				var generation cachePointer
				if json.Unmarshal([]byte(value), &generation) == nil && generation.Handle != "" {
					retention[generation.Handle] = retentionDuration(generation, olderThan)
					generationMetadata[generation.Handle] = append(generationMetadata[generation.Handle], key)
				}
			}
		}

		images, err := c.cacheImages(ctx)
		if err != nil {
			return err
		}
		present := make(map[string]bool, len(images))
		for _, name := range images {
			present[c.cfg.CachePool+"/"+name] = true
		}
		for handle, keys := range generationMetadata {
			if present[handle] {
				continue
			}
			for _, key := range keys {
				if err := c.metaRemove(ctx, c.cacheIndex(), key); err != nil {
					return err
				}
			}
		}

		removed := map[string]bool{}
		for {
			progress := false
			for _, name := range images {
				if removed[name] {
					continue
				}
				if !strings.HasPrefix(name, "cache-g-") && !strings.HasPrefix(name, "cache-v-") {
					continue
				}

				handle := c.cfg.CachePool + "/" + name
				if protected[handle] {
					continue
				}

				usedAt, ok := cacheTimeFromName(name)
				if value, found, readErr := c.metaGet(ctx, handle, cacheMetaPrefix+"used_at"); readErr != nil {
					return readErr
				} else if found {
					if parsed, parseErr := time.Parse(time.RFC3339Nano, value); parseErr == nil {
						usedAt, ok = parsed, true
					}
				}

				age := olderThan
				if specific := retention[handle]; specific > age {
					age = specific
				}
				if !ok || now.Sub(usedAt) < age {
					continue
				}

				mapped, err := c.mappedDevices(ctx, name)
				if err != nil {
					return err
				}

				if len(mapped) != 0 {
					continue
				}

				if _, err := c.rbdCmd(ctx, false, "snap", "purge", handle); err != nil &&
					!isNoSuchFile(err) {
					return fmt.Errorf("ceph: purge snapshots of expired cache %s: %w", handle, err)
				}

				if err := c.removeCacheImage(ctx, handle); err != nil {
					// A newer generation may still be a copy-on-write descendant. Keep
					// this generation's metadata so a later dependency pass can retry it
					// after the descendants and their retired writers are gone.
					if isImageNotEmpty(err) {
						continue
					}

					return err
				}
				removed[name] = true
				progress = true
				for _, key := range generationMetadata[handle] {
					if err := c.metaRemove(ctx, c.cacheIndex(), key); err != nil {
						return err
					}
				}
			}

			retiredProgress, err := c.purgeRetiredCacheImages(ctx)
			if err != nil {
				return err
			}
			if !progress && !retiredProgress {
				return nil
			}
		}
	})
}

type cacheTrashImage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func isImageNotEmpty(err error) bool {
	return exitedWith(err, 39)
}

func (c *Client) purgeRetiredCacheImages(ctx context.Context) (bool, error) {
	out, err := c.rbdCmd(ctx, true, "trash", "list", c.cfg.CachePool)
	if err != nil {
		return false, fmt.Errorf("ceph: list retired cache volumes: %w", err)
	}

	var images []cacheTrashImage
	if err := json.Unmarshal(out, &images); err != nil || images == nil {
		return false, fmt.Errorf("ceph: %s did not answer with a json trash list", c.bin)
	}

	removed := false
	for _, image := range images {
		if !strings.HasPrefix(image.Name, "cache-v-") {
			continue
		}
		if image.ID == "" || strings.ContainsAny(image.ID, "/@") ||
			strings.HasPrefix(image.ID, "-") || strings.TrimSpace(image.ID) != image.ID {
			return false, errors.New("ceph: the cache trash contains an unusable image identity")
		}

		handle := c.cfg.CachePool + "/" + image.ID
		if _, err := c.rbdCmd(ctx, false, "trash", "rm", handle); err != nil &&
			!isNoSuchFile(err) {
			// ENOTEMPTY (39) means a copy-on-write child still reads the retired
			// parent. `rbd trash rm` prints different prose than `rbd rm`, so the
			// errno is the stable part measured against both commands.
			if isImageNotEmpty(err) {
				continue
			}

			return false, fmt.Errorf("ceph: remove retired cache volume %s: %w", handle, err)
		}
		removed = true
	}

	return removed, nil
}

func retentionHours(key string) int {
	if strings.Contains(key, "/docker-images/") || strings.HasPrefix(key, "docker-images/") {
		return 8 * 24
	}

	return 7 * 24
}

func retentionDuration(pointer cachePointer, fallback time.Duration) time.Duration {
	if pointer.RetentionHours <= 0 {
		return fallback
	}

	return time.Duration(pointer.RetentionHours) * time.Hour
}

func (c *Client) cacheIndexMetadata(ctx context.Context) (map[string]string, error) {
	out, err := c.rbdCmd(ctx, false, "image-meta", "list", c.cacheIndex())
	if err != nil {
		return nil, fmt.Errorf("ceph: list the cache index: %w", err)
	}

	metadata := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			metadata[key] = strings.TrimSpace(value)
		}
	}

	return metadata, nil
}

func (c *Client) cacheImages(ctx context.Context) ([]string, error) {
	out, err := c.rbdCmd(ctx, true, "-p", c.cfg.CachePool, "ls")
	if err != nil {
		return nil, fmt.Errorf("ceph: list cache images: %w", err)
	}

	var names []string
	if err := json.Unmarshal(out, &names); err != nil || names == nil {
		return nil, fmt.Errorf("ceph: %s did not answer with a json cache image list", c.bin)
	}

	slices.Sort(names)

	return names, nil
}

func cacheTimeFromName(name string) (time.Time, bool) {
	parts := strings.Split(name, "-")
	if len(parts) < 4 {
		return time.Time{}, false
	}

	seconds, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil {
		return time.Time{}, false
	}

	return time.Unix(seconds, 0).UTC(), true
}

func verifyFilesystem(ctx context.Context, device string) (storecontract.Filesystem, error) {
	blkid, err := exec.LookPath("blkid")
	if err != nil {
		return storecontract.Filesystem{}, fmt.Errorf("blkid is required to identify a cache filesystem: %w", err)
	}

	e2fsck, err := exec.LookPath("e2fsck")
	if err != nil {
		return storecontract.Filesystem{}, fmt.Errorf("e2fsck is required to verify an ext4 cache: %w", err)
	}

	output := &tailWriter{limit: filesystemProbeLimit}
	cmd := exec.CommandContext(ctx, blkid, "-p", "-s", "TYPE", "-s", "UUID", "-o", "export", device)
	cmd.Stdout = output
	if err := cmd.Run(); err != nil {
		return storecontract.Filesystem{}, fmt.Errorf("identify %s: %w", device, err)
	}

	filesystem := storecontract.Filesystem{}
	for _, line := range strings.Split(output.String(), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		switch key {
		case "TYPE":
			filesystem.Type = value
		case "UUID":
			filesystem.UUID = value
		}
	}

	if filesystem.Type != "ext4" {
		return storecontract.Filesystem{}, fmt.Errorf("cache device %s contains %q, want ext4",
			device, filesystem.Type)
	}

	if err := exec.CommandContext(ctx, e2fsck, "-f", "-n", device).Run(); err != nil {
		return storecontract.Filesystem{}, fmt.Errorf("the ext4 filesystem on %s is not clean: %w",
			device, err)
	}

	filesystem.Clean = true

	return filesystem, filesystem.Valid()
}
