// Package ebss3 implements site-local cache generations with EBS snapshots and S3 state.
package ebss3

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/config"
	storecontract "github.com/junioryono/billet/internal/store"
)

const (
	cacheVolumeTTL = 7 * time.Hour
	maxCASAttempts = 16
)

var (
	errObjectConflict  = errors.New("ebs-s3: object changed")
	errObjectAmbiguous = errors.New("ebs-s3: conditional write outcome is unknown")
	errSnapshotMissing = errors.New("ebs-s3: snapshot is absent")
	errAlreadyApplied  = errors.New("ebs-s3: mutation was already applied")
)

type snapshotInfo struct {
	ID      string
	Created time.Time
}

type volumeInfo struct {
	ID      string
	Created time.Time
}

type blockAPI interface {
	CreateVolume(ctx context.Context, snapshot string, sizeBytes int64) (string, error)
	DeleteVolume(ctx context.Context, id string) error
	CreateSnapshot(ctx context.Context, volume string, now time.Time) (string, error)
	SnapshotExists(ctx context.Context, id string) (bool, error)
	ListSnapshots(ctx context.Context) ([]snapshotInfo, error)
	DeleteSnapshot(ctx context.Context, id string) error
	ListAvailableVolumes(ctx context.Context) ([]volumeInfo, error)
}

type objectAPI interface {
	Get(ctx context.Context, key string) (body []byte, etag string, found bool, err error)
	Put(ctx context.Context, key string, body []byte, expectedETag string) (string, error)
	List(ctx context.Context, prefix string) ([]string, error)
}

// Store owns EBS cache snapshots and their strongly consistent S3 state objects.
type Store struct {
	cfg     config.EBSS3Config
	blocks  blockAPI
	objects objectAPI
	now     func() time.Time
}

var _ storecontract.Store = (*Store)(nil)

type option func(*Store)

func withNow(now func() time.Time) option {
	return func(s *Store) {
		if now != nil {
			s.now = now
		}
	}
}

func newStore(
	cfg config.EBSS3Config,
	blocks blockAPI,
	objects objectAPI,
	opts ...option,
) *Store {
	s := &Store{cfg: cfg, blocks: blocks, objects: objects, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}

	return s
}

type generationState struct {
	Handle         string                     `json:"handle"`
	Filesystem     storecontract.Filesystem   `json:"filesystem"`
	UsedAt         time.Time                  `json:"used_at"`
	RetentionHours int                        `json:"retention_hours,omitempty"`
	WriterID       string                     `json:"writer_id,omitempty"`
	Fence          storecontract.FencingToken `json:"fence,omitempty"`
	Previous       string                     `json:"previous,omitempty"`
}

type candidateState struct {
	Handle     string                   `json:"handle"`
	Filesystem storecontract.Filesystem `json:"filesystem"`
	CreatedAt  time.Time                `json:"created_at"`
}

type writerState struct {
	Lease  storecontract.WriterLease  `json:"lease"`
	Fence  storecontract.FencingToken `json:"fence"`
	Holder string                     `json:"holder"`
}

type activeState struct {
	Generation string    `json:"generation"`
	Volume     string    `json:"volume,omitempty"`
	Expires    time.Time `json:"expires"`
}

type keyState struct {
	Key         string                     `json:"key"`
	Pointer     string                     `json:"pointer,omitempty"`
	Fence       storecontract.FencingToken `json:"fence"`
	Writer      *writerState               `json:"writer,omitempty"`
	Generations map[string]generationState `json:"generations,omitempty"`
	Candidates  map[string]candidateState  `json:"candidates,omitempty"`
	Active      map[string]activeState     `json:"active,omitempty"`
}

func (s *Store) statePrefix() string { return s.cfg.Prefix + "/state/" }

func (s *Store) stateKey(key string) string {
	digest := sha256.Sum256([]byte(key))

	return s.statePrefix() + hex.EncodeToString(digest[:]) + ".json"
}

func emptyState(key string) keyState {
	return keyState{
		Key: key, Generations: map[string]generationState{},
		Candidates: map[string]candidateState{}, Active: map[string]activeState{},
	}
}

func (s *Store) load(ctx context.Context, key string) (keyState, string, error) {
	return s.loadObject(ctx, s.stateKey(key), key)
}

func (s *Store) loadObject(
	ctx context.Context,
	objectKey, expectedKey string,
) (keyState, string, error) {
	body, etag, found, err := s.objects.Get(ctx, objectKey)
	if err != nil {
		return keyState{}, "", err
	}
	if !found {
		return emptyState(expectedKey), "", nil
	}

	var state keyState
	if err := json.Unmarshal(body, &state); err != nil {
		return keyState{}, "", fmt.Errorf("ebs-s3: state object %s is not valid json", objectKey)
	}
	if state.Key != expectedKey || strings.TrimSpace(state.Key) == "" {
		return keyState{}, "", fmt.Errorf("ebs-s3: state object %s names a different cache key", objectKey)
	}
	if state.Generations == nil {
		state.Generations = map[string]generationState{}
	}
	if state.Candidates == nil {
		state.Candidates = map[string]candidateState{}
	}
	if state.Active == nil {
		state.Active = map[string]activeState{}
	}
	if state.Pointer != "" {
		if _, ok := state.Generations[state.Pointer]; !ok {
			return keyState{}, "", fmt.Errorf("ebs-s3: cache %q points at an unrecorded generation", state.Key)
		}
	}

	return state, etag, nil
}

func (s *Store) mutate(
	ctx context.Context,
	key string,
	fn func(*keyState) error,
) error {
	objectKey := s.stateKey(key)
	for range maxCASAttempts {
		state, etag, err := s.loadObject(ctx, objectKey, key)
		if err != nil {
			return err
		}
		if err := fn(&state); err != nil {
			if errors.Is(err, errAlreadyApplied) {
				return nil
			}

			return err
		}
		body, err := json.Marshal(state)
		if err != nil {
			return fmt.Errorf("ebs-s3: encode state for cache %q: %w", key, err)
		}
		if _, err := s.objects.Put(ctx, objectKey, body, etag); err == nil {
			return nil
		} else if !errors.Is(err, errObjectConflict) && !errors.Is(err, errObjectAmbiguous) {
			return err
		}
	}

	return fmt.Errorf("%w: cache %q changed during every conditional write attempt",
		storecontract.ErrConflict, key)
}

func checkKey(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("ebs-s3: a cache volume needs a key")
	}
	if strings.TrimSpace(key) != key || strings.ContainsRune(key, 0) {
		return errors.New("ebs-s3: a cache key cannot have surrounding whitespace or a NUL byte")
	}

	return nil
}

func randomID(prefix string) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", fmt.Errorf("ebs-s3: make %s identity: %w", prefix, err)
	}

	return prefix + "-" + hex.EncodeToString(nonce[:]), nil
}

// Current reports the generation named by a key's S3 state object.
func (s *Store) Current(ctx context.Context, key string) (string, error) {
	if err := checkKey(key); err != nil {
		return "", err
	}
	state, _, err := s.load(ctx, key)
	if err != nil {
		return "", err
	}
	if state.Pointer == "" {
		return "", fmt.Errorf("%w: site has no generation for cache %q", storecontract.ErrMiss, key)
	}

	return state.Pointer, nil
}

// Create allocates a new encrypted, unformatted EBS volume.
func (s *Store) Create(
	ctx context.Context,
	key string,
	sizeBytes int64,
) (storecontract.Volume, error) {
	if err := checkKey(key); err != nil {
		return storecontract.Volume{}, err
	}
	if sizeBytes <= 0 {
		return storecontract.Volume{}, fmt.Errorf("ebs-s3: cache %q asks for %d bytes", key, sizeBytes)
	}

	leaseID, err := randomID("active")
	if err != nil {
		return storecontract.Volume{}, err
	}
	volume, err := s.blocks.CreateVolume(ctx, "", sizeBytes)
	if err != nil {
		return storecontract.Volume{}, err
	}
	lease := storecontract.ActiveLease{ID: leaseID, Expires: s.now().Add(cacheVolumeTTL)}
	if err := s.mutate(ctx, key, func(state *keyState) error {
		state.Active[leaseID] = activeState{Volume: volume, Expires: lease.Expires}

		return nil
	}); err != nil {
		return storecontract.Volume{}, errors.Join(err, s.blocks.DeleteVolume(ctx, volume))
	}

	return storecontract.Volume{
		Key: key, Handle: volume, Device: volume, Lease: lease,
	}, nil
}

// AcquireWriter conditionally records a newer lease and fencing token in S3.
func (s *Store) AcquireWriter(
	ctx context.Context,
	key, holder string,
	ttl time.Duration,
) (storecontract.WriterLease, storecontract.FencingToken, error) {
	if err := checkKey(key); err != nil {
		return storecontract.WriterLease{}, 0, err
	}
	if strings.TrimSpace(holder) == "" || ttl <= 0 {
		return storecontract.WriterLease{}, 0,
			errors.New("ebs-s3: a cache writer needs a holder and a positive lifetime")
	}

	now := s.now()
	id, err := randomID("writer")
	if err != nil {
		return storecontract.WriterLease{}, 0, err
	}
	var issued writerState
	err = s.mutate(ctx, key, func(state *keyState) error {
		if state.Writer != nil && state.Writer.Lease.ID == id &&
			state.Writer.Lease.Key == key && state.Writer.Holder == holder &&
			state.Writer.Lease.Expires.Equal(now.Add(ttl)) &&
			state.Writer.Fence == state.Fence {
			issued = *state.Writer

			return errAlreadyApplied
		}
		if state.Writer != nil && now.Before(state.Writer.Lease.Expires) {
			return fmt.Errorf("%w: cache %q already has a live writer", storecontract.ErrConflict, key)
		}
		if state.Fence == storecontract.FencingToken(math.MaxUint64) {
			return fmt.Errorf("ebs-s3: cache %q exhausted its fencing tokens", key)
		}
		issued = writerState{
			Lease: storecontract.WriterLease{Key: key, ID: id, Expires: now.Add(ttl)},
			Fence: state.Fence + 1, Holder: holder,
		}
		state.Fence = issued.Fence
		state.Writer = &issued

		return nil
	})

	return issued.Lease, issued.Fence, err
}

// Snapshot consumes a detached EBS volume and records its immutable candidate.
func (s *Store) Snapshot(
	ctx context.Context,
	volume storecontract.Volume,
) (storecontract.Candidate, error) {
	if err := checkKey(volume.Key); err != nil {
		return storecontract.Candidate{}, err
	}
	if !strings.HasPrefix(volume.Handle, "vol-") || volume.Handle != volume.Device {
		return storecontract.Candidate{}, errors.New("ebs-s3: a candidate must come from an EBS volume")
	}
	if err := volume.Filesystem.Valid(); err != nil {
		return storecontract.Candidate{}, fmt.Errorf("ebs-s3: remote filesystem proof: %w", err)
	}

	now := s.now()
	snapshot, err := s.blocks.CreateSnapshot(ctx, volume.Handle, now)
	if err != nil {
		return storecontract.Candidate{}, err
	}
	candidate := storecontract.Candidate{
		Key: volume.Key, Generation: snapshot, Handle: snapshot, Filesystem: volume.Filesystem,
	}
	if err := s.mutate(ctx, volume.Key, func(state *keyState) error {
		state.Candidates[snapshot] = candidateState{
			Handle: snapshot, Filesystem: volume.Filesystem, CreatedAt: now.UTC(),
		}

		return nil
	}); err != nil {
		return storecontract.Candidate{}, errors.Join(err, s.blocks.DeleteSnapshot(ctx, snapshot))
	}
	if err := s.blocks.DeleteVolume(ctx, volume.Handle); err != nil {
		return storecontract.Candidate{}, err
	}
	if volume.Lease.ID != "" {
		if err := s.dropActive(ctx, volume.Key, volume.Lease.ID); err != nil {
			return storecontract.Candidate{}, err
		}
	}

	return candidate, nil
}

// PublishCAS advances a pointer in the same conditional S3 write that consumes its writer.
func (s *Store) PublishCAS(
	ctx context.Context,
	key, expected string,
	candidate storecontract.Candidate,
	lease storecontract.WriterLease,
	fence storecontract.FencingToken,
) error {
	if err := checkKey(key); err != nil {
		return err
	}
	if candidate.Key != key || candidate.Generation == "" || candidate.Handle == "" ||
		candidate.Generation != candidate.Handle {
		return errors.New("ebs-s3: candidate does not belong to this cache key")
	}
	if err := candidate.Filesystem.Valid(); err != nil {
		return err
	}
	if fence == 0 {
		return fmt.Errorf("%w: fencing token zero authorises nothing", storecontract.ErrConflict)
	}
	present, err := s.blocks.SnapshotExists(ctx, candidate.Handle)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: immutable EBS candidate %s is absent", storecontract.ErrMiss,
			candidate.Handle)
	}

	return s.mutate(ctx, key, func(state *keyState) error {
		now := s.now()
		if published, ok := state.Generations[candidate.Generation]; ok &&
			state.Pointer == candidate.Generation && published.Handle == candidate.Handle &&
			published.Filesystem == candidate.Filesystem && published.WriterID == lease.ID &&
			published.Fence == fence && published.Previous == expected {
			return errAlreadyApplied
		}
		if err := lease.ValidAt(key, now); err != nil {
			return fmt.Errorf("%w: %w", storecontract.ErrConflict, err)
		}
		if state.Writer == nil || state.Writer.Lease.ID != lease.ID ||
			!state.Writer.Lease.Expires.Equal(lease.Expires) || state.Writer.Fence != fence {
			return fmt.Errorf("%w: cache %q has a newer writer", storecontract.ErrConflict, key)
		}
		if state.Pointer != expected {
			return fmt.Errorf("%w: cache %q is at generation %q, not %q",
				storecontract.ErrConflict, key, state.Pointer, expected)
		}
		recorded, ok := state.Candidates[candidate.Generation]
		if !ok || recorded.Handle != candidate.Handle || recorded.Filesystem != candidate.Filesystem {
			return errors.New("ebs-s3: candidate is not recorded in this cache's state")
		}
		state.Generations[candidate.Generation] = generationState{
			Handle: candidate.Handle, Filesystem: candidate.Filesystem, UsedAt: now.UTC(),
			RetentionHours: retentionHours(key), WriterID: lease.ID, Fence: fence, Previous: expected,
		}
		delete(state.Candidates, candidate.Generation)
		state.Pointer = candidate.Generation
		state.Writer = nil

		return nil
	})
}

// Clone allocates a writable EBS volume from a published snapshot.
func (s *Store) Clone(
	ctx context.Context,
	key, generation string,
) (storecontract.Volume, error) {
	if err := checkKey(key); err != nil {
		return storecontract.Volume{}, err
	}

	now := s.now()
	leaseID, err := randomID("active")
	if err != nil {
		return storecontract.Volume{}, err
	}
	var selected string
	var lease storecontract.ActiveLease
	err = s.mutate(ctx, key, func(state *keyState) error {
		selected = generation
		if selected == "" {
			selected = state.Pointer
		}
		record, ok := state.Generations[selected]
		if selected == "" || !ok {
			return fmt.Errorf("%w: site has no generation for cache %q", storecontract.ErrMiss, key)
		}
		record.UsedAt = now.UTC()
		state.Generations[selected] = record
		lease = storecontract.ActiveLease{ID: leaseID, Expires: now.Add(cacheVolumeTTL)}
		state.Active[leaseID] = activeState{Generation: selected, Expires: lease.Expires}

		return nil
	})
	if err != nil {
		return storecontract.Volume{}, err
	}

	volume, err := s.blocks.CreateVolume(ctx, selected, 0)
	if err != nil {
		cleanupErr := s.dropActive(ctx, key, leaseID)
		if errors.Is(err, errSnapshotMissing) {
			return storecontract.Volume{}, errors.Join(
				fmt.Errorf("%w: cache %q generation disappeared before clone",
					storecontract.ErrMiss, key), cleanupErr)
		}

		return storecontract.Volume{}, errors.Join(err, cleanupErr)
	}
	if err := s.bindActiveVolume(ctx, key, leaseID, selected, volume); err != nil {
		return storecontract.Volume{}, errors.Join(err, s.blocks.DeleteVolume(ctx, volume),
			s.dropActive(ctx, key, leaseID))
	}

	return storecontract.Volume{
		Key: key, Generation: selected, Handle: volume, Device: volume, Lease: lease,
	}, nil
}

func (s *Store) bindActiveVolume(
	ctx context.Context,
	key, leaseID, generation, volume string,
) error {
	return s.mutate(ctx, key, func(state *keyState) error {
		active, ok := state.Active[leaseID]
		if !ok || active.Generation != generation {
			return fmt.Errorf("%w: active cache lease disappeared before its volume was recorded",
				storecontract.ErrConflict)
		}
		if active.Volume != "" && active.Volume != volume {
			return fmt.Errorf("%w: active cache lease already names another volume",
				storecontract.ErrConflict)
		}
		active.Volume = volume
		state.Active[leaseID] = active

		return nil
	})
}

// RenewActive extends a generation's eviction protection.
func (s *Store) RenewActive(
	ctx context.Context,
	volume storecontract.Volume,
	until time.Time,
) error {
	if volume.Lease.ID == "" || !s.now().Before(until) {
		return errors.New("ebs-s3: an active cache renewal needs an identity and a future expiry")
	}

	return s.mutate(ctx, volume.Key, func(state *keyState) error {
		active, ok := state.Active[volume.Lease.ID]
		if !ok || active.Generation != volume.Generation ||
			(active.Volume != "" && active.Volume != volume.Handle) {
			return fmt.Errorf("%w: active cache lease no longer names this volume",
				storecontract.ErrConflict)
		}
		active.Expires = until.UTC()
		state.Active[volume.Lease.ID] = active

		return nil
	})
}

// Discard deletes a writable EBS volume and then releases its active lease.
func (s *Store) Discard(ctx context.Context, volume storecontract.Volume) error {
	if volume.Handle == "" {
		return nil
	}
	if !strings.HasPrefix(volume.Handle, "vol-") {
		return errors.New("ebs-s3: refusing to discard something that is not an EBS volume")
	}
	if err := s.blocks.DeleteVolume(ctx, volume.Handle); err != nil {
		return err
	}
	if volume.Lease.ID == "" {
		return nil
	}

	return s.dropActive(ctx, volume.Key, volume.Lease.ID)
}

func (s *Store) dropActive(ctx context.Context, key, leaseID string) error {
	return s.mutate(ctx, key, func(state *keyState) error {
		delete(state.Active, leaseID)

		return nil
	})
}

// Evict removes inactive generations and abandoned candidates without touching live clones.
func (s *Store) Evict(ctx context.Context, olderThan time.Duration) error {
	if olderThan <= 0 {
		return errors.New("ebs-s3: cache eviction needs a positive inactivity age")
	}

	keys, err := s.objects.List(ctx, s.statePrefix())
	if err != nil {
		return err
	}
	now := s.now()
	referenced := map[string]bool{}
	activeVolumes := map[string]bool{}
	var remove []string
	for _, objectKey := range keys {
		body, _, found, err := s.objects.Get(ctx, objectKey)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		var observed keyState
		if err := json.Unmarshal(body, &observed); err != nil || observed.Key == "" {
			return fmt.Errorf("ebs-s3: state object %s is not a valid cache state", objectKey)
		}
		var planned []string
		if err := s.mutate(ctx, observed.Key, func(state *keyState) error {
			planned = planned[:0]
			protected := map[string]bool{}
			for id, active := range state.Active {
				if !now.Before(active.Expires) {
					delete(state.Active, id)
					continue
				}
				protected[active.Generation] = true
			}
			if pointer, ok := state.Generations[state.Pointer]; ok && !protected[state.Pointer] &&
				now.Sub(pointer.UsedAt) >= retentionDuration(pointer, olderThan) {
				state.Pointer = ""
			}
			for generation := range state.Generations {
				record := state.Generations[generation]
				if generation == state.Pointer || protected[generation] ||
					now.Sub(record.UsedAt) < retentionDuration(record, olderThan) {
					continue
				}
				planned = append(planned, record.Handle)
				delete(state.Generations, generation)
			}
			for generation, candidate := range state.Candidates {
				if now.Sub(candidate.CreatedAt) < olderThan {
					continue
				}
				planned = append(planned, candidate.Handle)
				delete(state.Candidates, generation)
			}

			return nil
		}); err != nil {
			return err
		}
		remove = append(remove, planned...)
	}

	// Re-read after every conditional state update. Anything referenced now is
	// protected; anything published concurrently was created recently and cannot
	// qualify as an old orphan in the sweep below.
	keys, err = s.objects.List(ctx, s.statePrefix())
	if err != nil {
		return err
	}
	for _, objectKey := range keys {
		body, _, found, err := s.objects.Get(ctx, objectKey)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		var state keyState
		if err := json.Unmarshal(body, &state); err != nil {
			return fmt.Errorf("ebs-s3: state object %s is not valid json", objectKey)
		}
		for name := range state.Generations {
			generation := state.Generations[name]
			referenced[generation.Handle] = true
		}
		for _, candidate := range state.Candidates {
			referenced[candidate.Handle] = true
		}
		for _, active := range state.Active {
			if now.Before(active.Expires) && active.Volume != "" {
				activeVolumes[active.Volume] = true
			}
		}
	}
	for _, snapshot := range remove {
		if !referenced[snapshot] {
			if err := s.blocks.DeleteSnapshot(ctx, snapshot); err != nil {
				return err
			}
		}
	}

	snapshots, err := s.blocks.ListSnapshots(ctx)
	if err != nil {
		return err
	}
	for _, snapshot := range snapshots {
		if referenced[snapshot.ID] || now.Sub(snapshot.Created) < olderThan {
			continue
		}
		if err := s.blocks.DeleteSnapshot(ctx, snapshot.ID); err != nil {
			return err
		}
	}
	volumes, err := s.blocks.ListAvailableVolumes(ctx)
	if err != nil {
		return err
	}
	for _, volume := range volumes {
		if activeVolumes[volume.ID] || now.Sub(volume.Created) < olderThan {
			continue
		}
		if err := s.blocks.DeleteVolume(ctx, volume.ID); err != nil {
			return err
		}
	}

	return nil
}

func retentionHours(key string) int {
	if strings.Contains(key, "/docker-images/") || strings.HasPrefix(key, "docker-images/") {
		return 8 * 24
	}

	return 7 * 24
}

func retentionDuration(generation generationState, fallback time.Duration) time.Duration {
	if generation.RetentionHours <= 0 {
		return fallback
	}

	return time.Duration(generation.RetentionHours) * time.Hour
}
