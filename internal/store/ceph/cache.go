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
	"regexp"
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

	// A cache lookup is lock-free. The exclusive cache-index lock exists to
	// serialize WRITERS; a reader that took it would inherit the writer's
	// 12-minute stale-lock recovery bound and stall every job's every cache
	// step behind a crashed holder, which breaks the "fail open to a miss,
	// never hang a job" contract. The commit ordering makes the lock-free read
	// safe: a generation record is written before the pointer that names it and
	// a pointer is set in one atomic metadata write, so the exact-key read below
	// and the single-shot `image-meta list` the prefix scan reads only ever
	// observe a complete pointer to a finished generation, never a pointer to
	// unfinished bookkeeping.
	var exactPointer cachePointer
	ok, err := c.readJSON(ctx, pointerKey(exact), &exactPointer)
	if err != nil {
		return "", "", err
	}
	if ok {
		if exactPointer.Generation == "" ||
			(exactPointer.Key != "" && exactPointer.Key != exact) {
			return "", "", fmt.Errorf("ceph: cache %q has an invalid pointer identity", exact)
		}

		return exact, exactPointer.Generation, nil
	}

	metadata, err := c.cacheIndexMetadata(ctx)
	if err != nil {
		// A cold site has no .cache-index image yet — the writer path creates it
		// under the lock, and a lock-free reader must not. An absent index has no
		// matching generation, so its ENOENT is this lookup's miss, which is also
		// the "fail open to a miss on any error" contract. Only this ENOENT is
		// softened; every other error (I/O, permission, timeout) still surfaces.
		// A destroyed cache POOL raises the same ENOENT and so also reads as a
		// miss here — correct per the contract, and never hidden: it fails every
		// cache WRITE, fails Evict (which reads the index strictly), and fails
		// `billet check`. rbd's error strings cannot tell an absent pool from an
		// absent image, so a read-path probe could not reliably distinguish them.
		if isNoSuchFile(err) {
			return "", "", fmt.Errorf("%w: site has no matching generation for cache %q",
				storecontract.ErrMiss, exact)
		}

		return "", "", err
	}
	for _, prefix := range restorePrefixes {
		var newest time.Time
		var candidateKey string
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
			if candidateKey == "" || published.After(newest) ||
				(published.Equal(newest) && pointer.Key < candidateKey) {
				candidateKey, newest = pointer.Key, published
			}
		}
		if candidateKey == "" {
			continue
		}

		// The metadata list is a torn multi-page read — rbd fetches image-meta in
		// pages, so a candidate captured on an early page may already have been
		// reaped (pointer removed, then image and generation record deleted)
		// before the list returned. Confirm the chosen key with one atomic
		// point-read and return its current generation, which refuses the one
		// dangerous outcome — a hit naming a generation that no longer exists.
		var confirm cachePointer
		ok, err := c.readJSON(ctx, pointerKey(candidateKey), &confirm)
		if err != nil {
			return "", "", err
		}
		if ok && confirm.Generation != "" && (confirm.Key == "" || confirm.Key == candidateKey) {
			return candidateKey, confirm.Generation, nil
		}

		// This prefix DID match a pointer in the listed view; it was just reaped
		// before the confirm. Do not fall to a lower-priority prefix and return a
		// less-specific cache the atomic read would never have chosen — miss, the
		// tolerated fail-open direction, keeping first-matching-prefix intact.
		return "", "", fmt.Errorf("%w: site has no matching generation for cache %q",
			storecontract.ErrMiss, exact)
	}

	return "", "", fmt.Errorf("%w: site has no matching generation for cache %q",
		storecontract.ErrMiss, exact)
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

	// Lock-free, for the reason given on Match: the pointer is written in one
	// atomic metadata write after the generation it names exists, so a reader
	// never has to wait out a writer's stale-lock recovery bound to observe a
	// complete pointer.
	var pointer cachePointer
	ok, err := c.readJSON(ctx, pointerKey(key), &pointer)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: site has no generation for cache %q", storecontract.ErrMiss, key)
	}

	return pointer.Generation, nil
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
		return storecontract.Volume{}, errors.Join(err, c.discardCacheVolume(ctx, handle))
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
			return &storecontract.WriterHeldError{
				Key: key, Holder: current.Holder, Expires: current.Lease.Expires,
			}
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

// ReleaseWriter removes exactly the recorded writer it is handed, under the
// cache index lock so it cannot interleave with an acquisition.
func (c *Client) ReleaseWriter(
	ctx context.Context,
	lease storecontract.WriterLease,
	fence storecontract.FencingToken,
) error {
	return c.releaseWriterAt(ctx, lease, fence, time.Now())
}

func (c *Client) releaseWriterAt(
	ctx context.Context,
	lease storecontract.WriterLease,
	fence storecontract.FencingToken,
	now time.Time,
) error {
	if err := checkCacheKey(lease.Key); err != nil {
		return err
	}
	if strings.TrimSpace(lease.ID) == "" || fence == 0 {
		return errors.New("ceph: a writer release needs the lease and fence that were issued")
	}

	return c.withCacheLock(ctx, now, func(time.Time) error {
		var current cacheWriter
		ok, err := c.readJSON(ctx, writerKey(lease.Key), &current)
		if err != nil {
			return err
		}
		// The same triple PublishCAS checks, so a newer writer's record is never
		// cleared; a record that is not ours leaves the key already free of this
		// lease. The fence key is deliberately not touched.
		if !ok || current.Lease.ID != lease.ID ||
			!current.Lease.Expires.Equal(lease.Expires) || current.Fence != fence {
			return nil
		}

		return c.metaRemove(ctx, c.cacheIndex(), writerKey(lease.Key))
	})
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

	if !c.isCacheVolume(volume.Handle) || volume.Device == "" {
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
	if err := c.discardCacheVolume(cleanupCtx, volume.Handle); err != nil {
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

// SizeOf reports a cache clone's provisioned size, as rbd describes it.
func (c *Client) SizeOf(ctx context.Context, volume storecontract.Volume) (int64, error) {
	name := strings.TrimPrefix(volume.Handle, c.cfg.CachePool+"/")
	if name == volume.Handle || !strings.HasPrefix(name, "cache-v-") {
		return 0, errors.New("ceph: refusing to inspect a cache volume outside the configured pool")
	}

	out, err := c.rbdCmd(ctx, true, "info", volume.Handle)
	if err != nil {
		return 0, fmt.Errorf("ceph: inspect cache clone %s: %w", volume.Handle, err)
	}
	var info struct {
		Size int64 `json:"size"`
	}
	if err := json.Unmarshal(out, &info); err != nil || info.Size <= 0 {
		return 0, fmt.Errorf("ceph: %s did not describe %s with a positive size", c.bin, volume.Handle)
	}

	return info.Size, nil
}

// Discard unmaps a writable cache clone and moves it to the cache pool's trash,
// where PurgeTrash deletes it.
func (c *Client) Discard(ctx context.Context, volume storecontract.Volume) error {
	if volume.Handle == "" {
		return nil
	}

	if !c.isCacheVolume(volume.Handle) {
		return fmt.Errorf("ceph: refusing to discard %s: it is not a writable cache volume billet "+
			"named in the configured pool", bounded(volume.Handle))
	}

	if err := c.discardCacheVolume(ctx, volume.Handle); err != nil {
		return err
	}

	if volume.Lease.ID != "" {
		return c.withCacheLock(ctx, time.Now(), func(time.Time) error {
			return c.metaRemove(ctx, c.cacheIndex(), activeKey(volume.Lease.ID))
		})
	}

	return nil
}

// removeCacheImage deletes a cache image with `rbd rm`, which refuses a parent a
// copy-on-write child still reads, or an image still open, before it deletes
// anything. A generation never goes to the trash, where neither is refused.
func (c *Client) removeCacheImage(ctx context.Context, handle string) error {
	if _, err := c.rbdCmd(ctx, false, "rm", handle); err != nil && !isNoSuchFile(err) {
		return fmt.Errorf("ceph: remove cache image %s: %w", handle, err)
	}

	return nil
}

// discardCacheVolume moves a writable cache volume to the cache pool's trash,
// treating an absent one as success. It refuses every other image.
//
// INTO THE TRASH, NOT `rbd rm`, for the reason removeClone gives: a remove
// deletes every data object before it returns, and under the command's bound it
// was killed partway for most jobs, leaving about 1,260 half-removed volumes in
// the reference deployment's cache pool (#292). PurgeTrash deletes the data off
// the command path.
func (c *Client) discardCacheVolume(ctx context.Context, handle string) error {
	if !c.isCacheVolume(handle) {
		return fmt.Errorf("ceph: refusing to move %s to the trash: only a writable cache volume "+
			"billet named goes there", bounded(handle))
	}

	// UNMAPPED HERE, ON EVERY PATH THAT DISCARDS. `rbd trash mv` ignores watchers,
	// and a volume this host still maps stays in the trash for good: every purge
	// answers EBUSY. A map that timed out is the case that shipped it: on 2026-10-03
	// `rbd device map` passed its bound, the kernel finished the mapping after the
	// failed create had already trashed the volume, and the purge failed on it until
	// the device was unmapped by hand. A device something still holds refuses the
	// unmap, and the volume then stays listed for the next discard to retry.
	name := strings.TrimPrefix(handle, c.cfg.CachePool+"/")

	devices, err := c.mappedDevices(ctx, name)
	if err != nil {
		return fmt.Errorf("ceph: could not tell whether cache volume %s is mapped here, so it stays "+
			"listed: %w", handle, err)
	}

	for _, device := range devices {
		if err := c.unmapDevice(ctx, device, name); err != nil {
			return err
		}
	}

	// SNAPSHOTS FIRST, AND A VOLUME THAT KEEPS ONE STAYS LISTED. A failed Snapshot
	// can leave its staging snapshot behind, and in the trash that snapshot makes
	// every `trash rm` answer ENOTEMPTY, which the purge takes for a live child and
	// waits on forever; listed, the next discard purges it.
	if _, err := c.rbdCmd(ctx, false, "snap", "purge", handle); err != nil && !isNoSuchFile(err) {
		return fmt.Errorf("ceph: purge the snapshots of cache volume %s before the trash: %w",
			handle, err)
	}

	if _, err := c.rbdCmd(ctx, false, "trash", "mv", handle); err != nil && !isNoSuchFile(err) {
		return fmt.Errorf("ceph: move cache volume %s to the trash: %w", handle, err)
	}

	return nil
}

// billetCacheImage matches exactly the names cacheName gives the images billet
// creates in the cache pool: a writable volume (v) or a generation (g), the Unix
// second it was named and a 96-bit nonce.
var billetCacheImage = regexp.MustCompile(`^cache-([vg])-(\d+)-[0-9a-f]{24}$`)

// isCacheVolume reports whether a handle names, in the cache pool, a writable
// cache volume billet created.
func (c *Client) isCacheVolume(handle string) bool {
	name, ok := strings.CutPrefix(handle, c.cfg.CachePool+"/")
	kind, _, named := cacheImageName(name)

	return ok && named && kind == "v"
}

// cacheImageName reports which kind of image billet named (v or g), when it named
// it, and whether billet named it at all.
func cacheImageName(name string) (string, time.Time, bool) {
	match := billetCacheImage.FindStringSubmatch(name)
	if match == nil {
		return "", time.Time{}, false
	}

	// cacheName prints the second with %d, so a spelling it cannot produce, such
	// as a leading zero, is somebody else's.
	seconds, err := strconv.ParseInt(match[2], 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != match[2] {
		return "", time.Time{}, false
	}

	return match[1], time.Unix(seconds, 0), true
}

// PurgeTimeout bounds one image's deletion from the trash. A trash removal that
// is cut short resumes on the next purge, with the image still in the trash, so
// this is a bound on how long one call holds, not on how large an image can be.
const PurgeTimeout = 30 * time.Minute

// halfRemovedAfter is how long ago an image must have been named, and
// halfRemovedRecheck how long it must have stayed unopenable, before it is taken
// for a removal cut short. Creating an image lists its name before it writes its
// header, and the longest command that creates one is a lineage copy under
// cacheCompactionLimit, so the recheck outlasts that with a margin.
const (
	halfRemovedAfter   = time.Hour
	halfRemovedRecheck = cacheCompactionLimit + 5*time.Minute
)

// PurgeTrash deletes the per-job root disks and writable cache volumes discards
// moved to the cache pool's trash, finishes the cache images an `rbd rm` left
// half-removed, and reports how many it deleted.
//
// OFF THE COMMAND PATH and under its own bound, because deleting a large image's
// objects takes minutes. ONLY WHAT BILLET NAMED: a root disk (billet-*) or a
// writable cache volume (cacheImageName); a generation never reaches the trash
// (removeCacheImage) and a name that is not billet's is never touched. An image
// a copy-on-write child still reads (ENOTEMPTY), which is every retired writer
// whose generation is live, is left for a later purge, one already gone counts
// as done, and one that fails for any other reason does not stop the rest: each
// failure is reported together at the end.
func (c *Client) PurgeTrash(ctx context.Context) (int, error) {
	purged, failures := c.purgeTrashEntries(ctx)

	finished, unfinished := c.finishHalfRemoved(ctx)

	return purged + finished, errors.Join(append(failures, unfinished...)...)
}

func (c *Client) purgeTrashEntries(ctx context.Context) (int, []error) {
	out, err := c.rbdCmd(ctx, true, "trash", "list", c.cfg.CachePool)
	if err != nil {
		return 0, []error{fmt.Errorf("ceph: list the cache pool's trash: %w", err)}
	}

	var images []cacheTrashImage
	if err := json.Unmarshal(out, &images); err != nil || images == nil {
		return 0, []error{fmt.Errorf("ceph: %s did not answer with a json trash list", c.bin)}
	}

	purged := 0

	var failures []error

	for _, image := range images {
		if kind, _, ok := cacheImageName(image.Name); !strings.HasPrefix(image.Name, "billet-") &&
			(!ok || kind != "v") {
			continue
		}

		if err := ctx.Err(); err != nil {
			return purged, append(failures, err)
		}

		if image.ID == "" || strings.ContainsAny(image.ID, "/@") ||
			strings.HasPrefix(image.ID, "-") || strings.TrimSpace(image.ID) != image.ID {
			failures = append(failures, fmt.Errorf("ceph: the trash holds %s under an unusable "+
				"identity %q", image.Name, image.ID))

			continue
		}

		handle := c.cfg.CachePool + "/" + image.ID

		if err := c.rbdCmdWithin(ctx, PurgeTimeout, "trash", "rm", handle); err != nil &&
			!isNoSuchFile(err) {
			if !isImageNotEmpty(err) {
				failures = append(failures, fmt.Errorf("ceph: delete %s (%s) from the trash: %w",
					image.Name, handle, err))
			}

			continue
		}

		purged++
	}

	return purged, failures
}

// finishHalfRemoved completes the removals of cache images an `rbd rm` left half
// done, and reports how many it finished.
//
// A REMOVAL CUT SHORT leaves the name listed by `rbd ls` while `rbd info` answers
// ENOENT and the data objects remain, and running `rbd rm` again finishes it
// (measured on root clones, 2026-09-29, #280). THE PROOF IS THAT NOTHING CAN OPEN
// IT: an image without a header cannot be mapped, cloned or read, so no job,
// generation or index record can be using it; and rbd refuses to remove a parent
// with a clone child before it deletes anything, so one left half-removed had no
// child and can acquire none. Only names billet gave its cache images.
//
// NOT AN IMAGE BEING CREATED, which is listed a moment before its header exists.
// Its name must be older than halfRemovedAfter, and it must have been found
// unopenable by an earlier sighting at least halfRemovedRecheck before this one
// on this node's own clock, because the name's time is the creating node's clock. An `rbd info`
// that fails for another reason is could-not-tell: the image is kept, forgotten
// and reported.
func (c *Client) finishHalfRemoved(ctx context.Context) (int, []error) {
	names, err := c.cacheImages(ctx)
	if err != nil {
		return 0, []error{err}
	}

	c.halfRemovedMu.Lock()
	defer c.halfRemovedMu.Unlock()

	seen := c.halfRemoved
	c.halfRemoved = map[string]time.Time{}

	finished := 0

	var failures []error

	for _, name := range names {
		_, named, ok := cacheImageName(name)
		if !ok || c.now().Sub(named) < halfRemovedAfter {
			continue
		}

		if err := ctx.Err(); err != nil {
			return finished, append(failures, err)
		}

		handle := c.cfg.CachePool + "/" + name

		if _, err := c.rbdCmd(ctx, true, "info", handle); err == nil {
			continue
		} else if !isNoSuchFile(err) {
			failures = append(failures, fmt.Errorf("ceph: could not tell whether %s is half-removed: %w",
				handle, err))

			continue
		}

		// THE SIGHTING'S OWN TIME, taken once rbd has answered, never the pass's:
		// a pass can spend half an hour on the removals before this one.
		sighted := c.now()

		first, ok := seen[name]
		if !ok || sighted.Before(first) {
			c.halfRemoved[name] = sighted

			continue
		}

		if sighted.Sub(first) < halfRemovedRecheck {
			c.halfRemoved[name] = first

			continue
		}

		if err := c.rbdCmdWithin(ctx, PurgeTimeout, "rm", handle); err != nil &&
			!isNoSuchFile(err) {
			c.halfRemoved[name] = first

			if !isImageNotEmpty(err) {
				failures = append(failures, fmt.Errorf("ceph: finish removing half-removed %s: %w",
					handle, err))
			}

			continue
		}

		finished++
	}

	return finished, failures
}

func (c *Client) retireCacheImage(ctx context.Context, handle string) error {
	if !c.isCacheVolume(handle) {
		return fmt.Errorf("ceph: refusing to retire %s: only a writable cache volume billet "+
			"named is retired", bounded(handle))
	}

	if _, err := c.rbdCmd(ctx, false, "trash", "mv", handle); err != nil {
		return fmt.Errorf("ceph: retire cache volume %s: %w", handle, err)
	}

	return nil
}

// Evict removes old, unreferenced generations under the same lock as
// publication, and then moves expired writable volumes to the trash on the proof
// ReclaimOrphans takes.
//
// A GENERATION IS REMOVED WITH `rbd rm`, NEVER THE TRASH. Only the evicting
// host's mappings are checked, so what refuses a generation another node's job
// still has open is rbd's own refusal to remove an image with watchers, which
// `rbd trash mv` does not make. One removal cut short is finished by PurgeTrash
// (finishHalfRemoved).
//
// A WRITABLE VOLUME GOES TO THE TRASH, AFTER THE LOCK. No index record names
// one, and a large volume's `rbd rm` was routinely cut short by the command's
// bound, so eviction takes the positive proof judgeVolumes takes (no session on
// this node, no index record, old by the cluster's clock, no snapshot, no
// mapping here, no watcher, no recent publication) and then `rbd trash mv`, which
// is metadata only. PurgeTrash deletes the data under its own bound. Without a
// session reader (WithCacheSessions) every volume is kept.
//
// A FAILURE ON ONE IMAGE IS REPORTED AND THE PASS GOES ON, so one image rbd
// cannot remove or judge never holds every later one back to the next pass; the
// generations' part stops only at evictionLockBudget.
//
// IT NEVER DELETES FROM THE TRASH. Under this lock every writer waits on it, and a
// discarded volume takes minutes to delete, so PurgeTrash does that off the lock.
// A generation parented through a retired writer is therefore reclaimed over
// passes: a pass removes the child, the purge the writer, a later pass the parent.
func (c *Client) Evict(ctx context.Context, olderThan time.Duration) error {
	if olderThan <= 0 {
		return errors.New("ceph: cache eviction needs a positive inactivity age")
	}

	generations := c.withCacheLock(ctx, time.Now(), func(now time.Time) error {
		return c.evictGenerations(ctx, olderThan, now)
	})

	return errors.Join(generations, c.evictVolumes(ctx, olderThan))
}

func (c *Client) evictGenerations(ctx context.Context, olderThan time.Duration, now time.Time) error {
	// EVERY WRITER WAITS ON THIS LOCK, for at most cacheLockWaitLimit, and a
	// removal that fails no longer ends the pass, so the pass bounds how long it
	// holds the lock from the moment it has it and leaves the rest to the next.
	started := c.now()
	overBudget := func(what string) error {
		held := c.now().Sub(started)
		if held < evictionLockBudget {
			return nil
		}

		return fmt.Errorf("ceph: the cache lock was held %s, so %s wait for the next eviction pass",
			held.Round(time.Second), what)
	}

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
			if err := overBudget("expired index records and every generation"); err != nil {
				return err
			}
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
				} else if err := overBudget("expired index records and every generation"); err != nil {
					return err
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
			if err := overBudget("expired index records and every generation"); err != nil {
				return err
			}
			if err := c.metaRemove(ctx, c.cacheIndex(), key); err != nil {
				return err
			}
		}
	}

	generations := make([]string, 0, len(images))
	for _, name := range images {
		if kind, _, ok := cacheImageName(name); ok && kind == "g" {
			generations = append(generations, name)
		}
	}

	// A PASS CUT SHORT IS RESUMED WHERE IT STOPPED, so generations that fail
	// slowly at the front of the order cannot spend every pass's budget ahead of
	// the ones behind them.
	c.evictMu.Lock()
	resume := c.evictResume
	c.evictMu.Unlock()

	first := 0
	if resume != "" {
		if i := slices.IndexFunc(generations, func(name string) bool { return name > resume }); i > 0 {
			first = i
		}
	}

	order := slices.Concat(generations[first:], generations[:first])

	var failures evictionFailures

	for i, name := range order {
		// WHY THE PASS STOPPED is never one of the failures the cap counts away.
		if err := ctx.Err(); err != nil {
			return errors.Join(failures.err(), err)
		}

		if err := overBudget(fmt.Sprintf("%d generation(s) from %s on", len(order)-i, name)); err != nil {
			if i > 0 {
				c.evictMu.Lock()
				c.evictResume = order[i-1]
				c.evictMu.Unlock()
			}

			return errors.Join(failures.err(), err)
		}

		handle := c.cfg.CachePool + "/" + name
		if protected[handle] {
			continue
		}

		if err := c.evictGeneration(ctx, name, handle, max(olderThan, retention[handle]), now,
			generationMetadata[handle]); err != nil {
			failures.add(fmt.Errorf("ceph: evict expired generation %s: %w", handle, err))
		}
	}

	c.evictMu.Lock()
	c.evictResume = ""
	c.evictMu.Unlock()

	return failures.err()
}

// evictGeneration removes one unprotected generation unused for age, keeping it
// on any answer it cannot read.
func (c *Client) evictGeneration(
	ctx context.Context, name, handle string, age time.Duration, now time.Time, metadataKeys []string,
) error {
	usedAt, ok := cacheTimeFromName(name)
	if value, found, err := c.metaGet(ctx, handle, cacheMetaPrefix+"used_at"); err != nil {
		return err
	} else if found {
		if parsed, parseErr := time.Parse(time.RFC3339Nano, value); parseErr == nil {
			usedAt, ok = parsed, true
		}
	}

	if !ok || now.Sub(usedAt) < age {
		return nil
	}

	mapped, err := c.mappedDevices(ctx, name)
	if err != nil {
		return err
	}

	if len(mapped) != 0 {
		return nil
	}

	if _, err := c.rbdCmd(ctx, false, "snap", "purge", handle); err != nil && !isNoSuchFile(err) {
		return fmt.Errorf("ceph: purge snapshots of expired cache %s: %w", handle, err)
	}

	if err := c.removeCacheImage(ctx, handle); err != nil {
		// A newer generation may still be a copy-on-write descendant. Keep this
		// generation's metadata so a later pass can retry it once PurgeTrash has
		// deleted the retired writer between them.
		if isImageNotEmpty(err) {
			return nil
		}

		return err
	}

	for _, key := range metadataKeys {
		if err := c.metaRemove(ctx, c.cacheIndex(), key); err != nil {
			return err
		}
	}

	return nil
}

// evictionVolumeLimit bounds how many expired volumes one eviction pass moves to
// the trash, and so what one pass hands the purge. Eviction runs every six hours,
// so this clears a backlog of a few thousand within a day or two.
const evictionVolumeLimit = 1000

// evictionLockBudget bounds how long one eviction pass holds the cache lock,
// well inside cacheLockWaitLimit, the longest any writer waits for it.
const evictionLockBudget = 5 * time.Minute

// evictionFailuresReported bounds how many failures one eviction error names,
// because one cause, such as an unreadable cluster clock, fails every image alike.
const evictionFailuresReported = 10

// evictionFailures joins the first evictionFailuresReported failures and counts
// the rest.
type evictionFailures struct {
	named      []error
	unreported int
}

func (f *evictionFailures) add(err error) {
	if len(f.named) == evictionFailuresReported {
		f.unreported++

		return
	}

	f.named = append(f.named, err)
}

func (f *evictionFailures) err() error {
	if f.unreported == 0 {
		return errors.Join(f.named...)
	}

	return errors.Join(append(f.named, fmt.Errorf("ceph: and %d more eviction failure(s)",
		f.unreported))...)
}

// evictVolumes moves expired writable volumes to the trash on judgeVolumes'
// proof, outside the cache lock, and reports each volume it could not judge or
// whose move rbd did not confirm.
func (c *Client) evictVolumes(ctx context.Context, olderThan time.Duration) error {
	if c.sessions == nil {
		return errors.New("ceph: expired writable cache volumes are kept: this client was given no " +
			"way to read the node's cache sessions")
	}

	inSession, err := c.sessions()
	if err == nil && inSession == nil {
		err = errors.New("the reader returned none")
	}
	if err != nil {
		return fmt.Errorf("ceph: expired writable cache volumes are kept: could not tell which the "+
			"node's cache sessions hold: %w", err)
	}

	report, err := c.judgeVolumes(ctx, OrphanOptions{
		OlderThan: max(olderThan, OrphanMinimumAge), Limit: evictionVolumeLimit, Reclaim: true,
		InSession: inSession,
	})
	if err != nil {
		return fmt.Errorf("ceph: expired writable cache volumes are kept: %w", err)
	}

	var failures evictionFailures

	for _, image := range report.Images {
		if image.Verdict == OrphanUnknown || image.Verdict == OrphanMoveUnknown {
			failures.add(fmt.Errorf("ceph: expired cache volume %s/%s, %s: %w",
				c.cfg.CachePool, image.Name, image.Verdict, image.Err))
		}
	}

	return failures.err()
}

type cacheTrashImage struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// isImageNotEmpty reports ENOTEMPTY (39): a copy-on-write child still reads the
// image. `rbd trash rm` prints different prose than `rbd rm`, so the errno is the
// stable part measured against both commands.
func isImageNotEmpty(err error) bool {
	return exitedWith(err, 39)
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
		// Strict: a list error must NOT be softened to an empty view. Evict reads
		// this same list to build its protected set, so an empty result on error
		// would let it sweep with an incomplete protected set and delete live
		// caches. Match, which reads this lock-free, tolerates a cold-index
		// ENOENT itself, close to its own miss.
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
