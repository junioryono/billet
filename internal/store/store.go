// Package store defines the cache-volume contract shared by storage backends.
package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// ErrMiss means a site has no published generation for a cache key.
var ErrMiss = errors.New("store: cache miss")

// ErrConflict means the expected generation or fencing token is no longer current.
var ErrConflict = errors.New("store: publication conflict")

// Volume is one writable, per-job clone mapped on the node that owns it.
//
// Device is the concrete host path a compute backend can attach. Handle is the
// storage backend's stable identity and is never interpreted by a provider.
type Volume struct {
	Key        string
	Generation string
	Handle     string
	Device     string
	Lease      ActiveLease
	// Filesystem is proof collected after the cooperative guest unmounts a
	// remotely attached volume. A host-mapped backend verifies the device itself;
	// a remote EBS backend has no block path on the orchestrator and requires this.
	Filesystem Filesystem
}

// ActiveLease prevents eviction while a job can still read a generation.
type ActiveLease struct {
	ID      string
	Expires time.Time
}

// Candidate is an immutable generation that has passed filesystem verification.
type Candidate struct {
	Key        string
	Generation string
	Handle     string
	Filesystem Filesystem
}

// Filesystem is the metadata proved immediately before a candidate is published.
//
// UUID distinguishes the filesystem that was checked from an empty or substituted
// device. Clean means a read-only filesystem check completed without finding a
// journal or structural repair to make; non-empty alone proves neither property.
type Filesystem struct {
	Type  string
	UUID  string
	Clean bool
	// CheckedAsPublished says the candidate's own bytes, as a clone of it reads
	// them, were checked too, not only the volume it was made from. A candidate
	// recorded before stores set it decodes false, and a store that sets it
	// checks such a candidate before publishing it.
	CheckedAsPublished bool
}

// Valid refuses metadata that cannot identify a checked filesystem.
func (f Filesystem) Valid() error {
	if strings.TrimSpace(f.Type) == "" {
		return errors.New("store: a candidate has no filesystem type")
	}

	if strings.TrimSpace(f.UUID) == "" {
		return errors.New("store: a candidate has no filesystem identity")
	}

	if !f.Clean {
		return errors.New("store: a candidate filesystem was not proved clean")
	}

	return nil
}

// WriterLease is the short-lived right to attempt a publication for one key.
//
// It is deliberately separate from a capacity lease. Compute remains charged
// until teardown is proved; a cache writer needs authority only across quiesce,
// verification and pointer commit, and conflating those lifetimes would make a
// stalled cache commit retain a machine's capacity.
type WriterLease struct {
	Key     string
	ID      string
	Expires time.Time
}

// ValidAt checks the facts a backend must re-check under its publication lock.
func (l WriterLease) ValidAt(key string, now time.Time) error {
	if strings.TrimSpace(l.ID) == "" {
		return errors.New("store: a writer lease has no identity")
	}

	if l.Key != key {
		return fmt.Errorf("store: writer lease for %q cannot publish %q", l.Key, key)
	}

	if l.Expires.IsZero() || !now.Before(l.Expires) {
		return errors.New("store: the writer lease has expired")
	}

	return nil
}

// FencingToken orders writers of one key. The zero value authorises nothing.
type FencingToken uint64

// WriterHeldError is AcquireWriter's refusal while another writer's lease is
// live. It unwraps to ErrConflict, so a caller asking only "conflict?" is
// unchanged, and carries what a waiting writer has to say out loud: who holds
// the key and when the lease ends if nothing releases it first.
type WriterHeldError struct {
	Key     string
	Holder  string
	Expires time.Time
}

func (e *WriterHeldError) Error() string {
	return fmt.Sprintf("%v: cache %q already has a live writer (%s, until %s)",
		ErrConflict, e.Key, e.Holder, e.Expires.UTC().Format(time.RFC3339))
}

func (e *WriterHeldError) Unwrap() error { return ErrConflict }

// Store owns cache generations at one site.
type Store interface {
	// Current reports the site's published generation for a key. ErrMiss means
	// there is no pointer. Publication policies use this only after a CAS conflict.
	Current(ctx context.Context, key string) (string, error)
	// Create maps a new, unformatted writable volume for a cold key.
	Create(ctx context.Context, key string, sizeBytes int64) (Volume, error)
	// Clone maps a writable clone of generation. Empty generation means the
	// site's current pointer; ErrMiss is a cold cache and never a job failure.
	Clone(ctx context.Context, key, generation string) (Volume, error)
	// RenewActive keeps a mounted generation out of eviction.
	RenewActive(ctx context.Context, volume Volume, until time.Time) error
	// AcquireWriter issues a separate storage lease and monotonically newer fence.
	// While another writer's lease is live it refuses with a *WriterHeldError.
	AcquireWriter(
		ctx context.Context, key, holder string, ttl time.Duration,
	) (WriterLease, FencingToken, error)
	// ReleaseWriter gives back a writer whose publication is not going to
	// happen, so the next writer need not wait out its lease. It removes exactly
	// the lease it is handed — matched by identity, expiry and fence — and
	// leaves anything else standing: a lease PublishCAS already consumed, one
	// that expired and was replaced, one this caller never held. Nothing else
	// moves: not the pointer, not a candidate, and never the fence, so a
	// released lease can publish no more than an expired one can.
	ReleaseWriter(ctx context.Context, lease WriterLease, fence FencingToken) error
	// Snapshot verifies an already-quiesced, unmounted volume and creates an
	// immutable candidate. The current pointer is unchanged on every failure.
	Snapshot(ctx context.Context, volume Volume) (Candidate, error)
	// PublishCAS advances the pointer only if expected, lease and fence are still
	// current. A caller not permitted to publish never calls this method.
	PublishCAS(
		ctx context.Context,
		key, expected string,
		candidate Candidate,
		lease WriterLease,
		fence FencingToken,
	) error
	// Discard releases a clone. It is idempotent.
	Discard(ctx context.Context, volume Volume) error
	// Evict removes inactive generations and abandoned candidates while preserving
	// every generation protected by a live active-clone lease.
	Evict(ctx context.Context, olderThan time.Duration) error
}

// VolumeSizer reports how large a mapped volume is, so a caller can refuse a
// generation larger than it allows. Optional: a store that cannot tell leaves
// the ceiling to apply at creation only.
type VolumeSizer interface {
	SizeOf(ctx context.Context, volume Volume) (int64, error)
}

// KeyMatcher finds the exact cache key or the newest current key under the first
// restore prefix that has a match. Stores used by transparent Actions caching
// implement it; ordinary sticky disks need only Store.
type KeyMatcher interface {
	Match(ctx context.Context, exact string, restorePrefixes []string) (key, generation string, err error)
}

// Archive is one immutable whole object an ArchiveStore holds.
type Archive struct {
	Key      string
	Size     int64
	Modified time.Time
}

// ErrArchiveSize means the blocks a commit names do not add up to the size the
// client declared, so nothing was published.
var ErrArchiveSize = errors.New("store: the staged blocks are not the declared size")

// ArchiveStore keeps a cache whose unit is a file rather than a filesystem as
// whole objects, on a store where a volume per file would cost more than the file
// saves (ADR-023). Optional: a store without it serves archives from volumes.
//
// The first commit of a key wins and an archive never changes, so there is no
// pointer, no writer lease and no fence: two writers of one key cannot publish
// anything but one of two whole archives.
type ArchiveStore interface {
	// ArchiveExists reports whether a key is published; an error is could-not-tell.
	ArchiveExists(ctx context.Context, key string) (bool, error)
	// MatchArchive finds the exact key, or the newest archive under the first
	// restore prefix that has one. ErrMiss means no archive matches.
	MatchArchive(ctx context.Context, exact string, restorePrefixes []string) (Archive, error)
	// StageArchive stores one block of an upload, under the name the client gave
	// it. size is the block's exact length.
	StageArchive(ctx context.Context, upload, block string, body io.Reader, size int64) error
	// CommitArchive publishes the named blocks, in order, as key, if they total
	// size, and releases the staged blocks. It is idempotent per upload: published
	// is true when this upload's archive is the one at key, whether this call or an
	// earlier one whose answer was lost put it there, and false when another
	// upload's is.
	CommitArchive(ctx context.Context, key, upload string, blocks []string, size int64) (published bool, err error)
	// AbortArchive releases an upload that will not be committed. It is idempotent.
	AbortArchive(ctx context.Context, upload string) error
	// ReadArchive reads length bytes of an archive from start.
	ReadArchive(ctx context.Context, archive Archive, start, length int64) (io.ReadCloser, error)
}

// BlobTable names one of a content-addressed cache's two tables, as its path does.
type BlobTable string

const (
	// BlobTableAC holds action results, a job's claim about an action.
	BlobTableAC BlobTable = "ac"
	// BlobTableCAS holds objects named by the digest of their bytes.
	BlobTableCAS BlobTable = "cas"
)

// BlobRef names one object of a content-addressed cache.
type BlobRef struct {
	Table  BlobTable
	Digest string
}

// BlobStore keeps a content-addressed cache as one object per entry (ADR-023).
// Optional: a store without it serves these caches from a mounted volume.
//
// AN ENTRY'S NAME IS WHAT IT HOLDS, or a job's claim that first-writer-wins keeps,
// so concurrent jobs merge by writing and there is no generation to publish.
type BlobStore interface {
	// ListBlobs is every entry a key holds, so a miss costs the store no request.
	ListBlobs(ctx context.Context, key string) (map[BlobRef]struct{}, error)
	// ReadBlob reads one entry. ErrMiss means it is not there.
	ReadBlob(ctx context.Context, key string, ref BlobRef) (io.ReadCloser, error)
	// PutBlob stores one entry unless it is already there, which is not an error.
	PutBlob(ctx context.Context, key string, ref BlobRef, body io.Reader, size int64) error
	// RecordBlobUse says one job used these entries, which keeps them from eviction
	// for the retention that starts now. id names the record, unique per job.
	RecordBlobUse(ctx context.Context, key, id string, used []BlobRef) error
}

// GenerationChecker checks a published generation's own filesystem, read-only.
// true is proof the generation is damaged, whatever error comes with it; false
// with an error is could-not-tell, never "clean". Optional: a store that cannot
// check leaves a generation that will not mount in place.
type GenerationChecker interface {
	GenerationDamaged(ctx context.Context, key, generation string) (bool, error)
}
