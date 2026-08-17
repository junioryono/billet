// Package store defines the cache-volume contract shared by storage backends.
package store

import (
	"context"
	"errors"
	"fmt"
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
	AcquireWriter(
		ctx context.Context, key, holder string, ttl time.Duration,
	) (WriterLease, FencingToken, error)
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
