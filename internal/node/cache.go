package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

const (
	cacheRequestLimit  = 8 << 10
	cacheKeyLimit      = 512
	cacheHandlerLimit  = 12 * time.Minute
	cacheCleanupMargin = 30 * time.Second
	cacheWorkLimit     = cacheHandlerLimit - cacheCleanupMargin
	cacheWriterTTL     = 15 * time.Minute
	cacheVolumeLimit   = int64(100 << 30)
	dockerSettleWait   = 2 * time.Minute
	dockerStoreKey     = "docker-images/"
	publicationCAS     = "cas"
	publicationLWW     = "last-write-wins"
)

// CacheService lets one authenticated microVM replace its reserved drive slots.
type CacheService struct {
	endpoint  string
	namespace string
	stateDir  string
	store     storecontract.Store
	attacher  provider.VolumeAttacher
	log       *slog.Logger

	mu         sync.Mutex
	byToken    map[string]*cacheSession
	byInstance map[string]string
}

type cacheSession struct {
	mu       sync.Mutex
	admit    chan struct{}
	token    string
	instance string
	trust    provider.TrustClass
	closed   bool
	slots    [provider.MaxVolumes]*cacheAttachment
}

// CacheCredentials identifies one managed guest's cache session.
type CacheCredentials struct {
	Token string
}

type cacheAttachment struct {
	Volume      storecontract.Volume `json:"volume"`
	Publication string               `json:"publication,omitempty"`
	Docker      bool                 `json:"docker,omitempty"`
	Settling    bool                 `json:"settling,omitempty"`
	Ready       bool                 `json:"ready,omitempty"`
}

// NewCacheService constructs a node-local cache endpoint.
func NewCacheService(
	endpoint string,
	namespace string,
	stateDir string,
	storage storecontract.Store,
	attacher provider.VolumeAttacher,
	log *slog.Logger,
) (*CacheService, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("node: cache endpoint %q must be an http(s) origin", endpoint)
	}

	if storage == nil || attacher == nil {
		return nil, errors.New("node: a cache service needs storage and a volume attacher")
	}
	if strings.TrimSpace(namespace) == "" || strings.ContainsRune(namespace, 0) {
		return nil, errors.New("node: a cache service needs a non-empty deployment namespace")
	}
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("node: a cache service needs the node state directory")
	}

	if log == nil {
		log = slog.Default()
	}

	service := &CacheService{
		endpoint: endpoint, namespace: namespace, store: storage, attacher: attacher, log: log,
		stateDir:   filepath.Join(stateDir, cacheSessionDirectory),
		byToken:    make(map[string]*cacheSession),
		byInstance: make(map[string]string),
	}
	if err := service.loadSessions(); err != nil {
		return nil, err
	}

	return service, nil
}

func (s *CacheService) qualifiedKey(key string) string { return s.namespace + "/" + key }

// Endpoint is the origin placed in a microVM's metadata.
func (s *CacheService) Endpoint() string { return s.endpoint }

// Prepare creates one unguessable session credential before compute starts.
func (s *CacheService) Prepare(instance string, trust provider.TrustClass) (CacheCredentials, error) {
	if strings.TrimSpace(instance) == "" {
		return CacheCredentials{}, errors.New("node: a cache session needs an instance")
	}

	if trust != provider.TrustTrusted && trust != provider.TrustUntrusted {
		return CacheCredentials{}, errors.New("node: cannot give cache access to work with unknown trust")
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, exists := s.byInstance[instance]; exists {
		return CacheCredentials{}, fmt.Errorf("node: cache session for %s already exists", instance)
	}

	for {
		token, err := randomCacheToken()
		if err != nil {
			return CacheCredentials{}, err
		}
		if s.tokenExists(token) {
			continue
		}

		session := &cacheSession{
			token: token, instance: instance, trust: trust,
			admit: make(chan struct{}, 1),
		}
		if err := s.persistSession(session); err != nil {
			return CacheCredentials{}, err
		}

		s.byToken[token] = session
		s.byInstance[instance] = token

		return CacheCredentials{Token: token}, nil
	}
}

func randomCacheToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("node: mint a cache session capability: %w", err)
	}

	return hex.EncodeToString(raw[:]), nil
}

// tokenExists is called while s.mu is held.
func (s *CacheService) tokenExists(token string) bool {
	return s.byToken[token] != nil
}

// finishSession removes a fully discarded session while session.mu is held.
func (s *CacheService) finishSession(session *cacheSession) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	token, ok := s.byInstance[session.instance]
	if !ok || token != session.token || s.byToken[token] != session {
		return nil
	}

	if err := s.removeSession(session); err != nil {
		return err
	}
	delete(s.byInstance, session.instance)
	delete(s.byToken, token)

	return nil
}

// Cleanup discards every volume after its compute is proved gone.
func (s *CacheService) Cleanup(ctx context.Context, instance string) error {
	s.mu.Lock()
	token, ok := s.byInstance[instance]
	if !ok {
		s.mu.Unlock()

		return nil
	}

	session := s.byToken[token]
	s.mu.Unlock()

	return s.cleanupSession(ctx, session, true)
}

// RetryClosed releases cache volumes whose earlier cleanup was interrupted.
func (s *CacheService) RetryClosed(ctx context.Context) error {
	s.mu.Lock()
	sessions := make([]*cacheSession, 0, len(s.byToken))
	for _, session := range s.byToken {
		sessions = append(sessions, session)
	}
	s.mu.Unlock()

	var failures []error
	for _, session := range sessions {
		if err := s.cleanupSession(ctx, session, false); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

// ReconcileInventory closes cache sessions whose compute is absent from a
// successful provider inventory. Only a successful list is evidence: an empty
// inventory is meaningful, while a list error must leave every session fenced.
func (s *CacheService) ReconcileInventory(ctx context.Context, instances []*provider.Instance) error {
	present := make(map[string]bool, len(instances))
	for _, instance := range instances {
		present[instance.Name] = true
	}

	s.mu.Lock()
	missing := make([]*cacheSession, 0)
	for _, session := range s.byToken {
		if !present[session.instance] {
			missing = append(missing, session)
		}
	}
	s.mu.Unlock()

	var failures []error
	for _, session := range missing {
		if err := s.cleanupSession(ctx, session, true); err != nil {
			failures = append(failures, err)
		}
	}

	return errors.Join(failures...)
}

// RenewActive keeps every mounted generation out of eviction. A job may outlive
// the store's initial lease, so the node refreshes these for as long as it owns
// the corresponding compute session.
func (s *CacheService) RenewActive(ctx context.Context, until time.Time) error {
	s.mu.Lock()
	sessions := make([]*cacheSession, 0, len(s.byToken))
	for _, session := range s.byToken {
		sessions = append(sessions, session)
	}
	s.mu.Unlock()

	var failures []error
	for _, session := range sessions {
		session.mu.Lock()
		if !session.closed {
			for slot, attachment := range session.slots {
				if attachment == nil || attachment.Volume.Lease.ID == "" {
					continue
				}
				if err := s.store.RenewActive(ctx, attachment.Volume, until); err != nil {
					failures = append(failures, fmt.Errorf("%s slot %d: %w", session.instance, slot, err))
				}
			}
		}
		session.mu.Unlock()
	}

	return errors.Join(failures...)
}

func (s *CacheService) cleanupSession(
	ctx context.Context,
	session *cacheSession,
	closeSession bool,
) error {
	session.mu.Lock()
	defer session.mu.Unlock()

	s.mu.Lock()
	owned := s.byToken[session.token] == session && s.byInstance[session.instance] == session.token
	s.mu.Unlock()
	if !owned || (!closeSession && !session.closed) {
		return nil
	}

	session.closed = true
	if err := s.persistSession(session); err != nil {
		return fmt.Errorf("node: record closed cache session for %s: %w", session.instance, err)
	}

	var failures []error
	for slot, attachment := range session.slots {
		if attachment == nil {
			continue
		}

		if err := s.store.Discard(ctx, attachment.Volume); err != nil {
			failures = append(failures, fmt.Errorf("slot %d: %w", slot, err))

			continue
		}

		session.slots[slot] = nil
		if err := s.persistSession(session); err != nil {
			return fmt.Errorf("node: record discarded cache volume of %s slot %d: %w",
				session.instance, slot, err)
		}
	}

	if err := errors.Join(failures...); err != nil {
		return fmt.Errorf("node: discard cache volumes of %s: %w", session.instance, err)
	}
	if err := s.finishSession(session); err != nil {
		return fmt.Errorf("node: finish cache cleanup of %s: %w", session.instance, err)
	}

	return nil
}

// ServeHTTP serves the exact API understood by the sticky-disk action.
func (s *CacheService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	session, ok := s.authenticate(r)
	if !ok {
		http.Error(w, "unauthorised cache session", http.StatusUnauthorized)

		return
	}
	select {
	case session.admit <- struct{}{}:
		defer func() { <-session.admit }()
	default:
		http.Error(w, "another cache operation is already in progress", http.StatusTooManyRequests)

		return
	}

	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/docker-store":
		s.attachDockerStore(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/docker-store/ready":
		s.markDockerStoreReady(w, r, session)
	case r.Method == http.MethodPost && r.URL.Path == "/v1/volumes":
		s.attach(w, r, session)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/volumes/") &&
		strings.HasSuffix(r.URL.Path, "/commit"):
		s.commit(w, r, session)
	case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v1/volumes/") &&
		strings.HasSuffix(r.URL.Path, "/discard"):
		s.discard(w, r, session)
	default:
		http.NotFound(w, r)
	}
}

func (s *CacheService) attachDockerStore(
	w http.ResponseWriter,
	r *http.Request,
	session *cacheSession,
) {
	ctx, cancel := context.WithTimeout(r.Context(), cacheWorkLimit)
	defer cancel()

	var request struct {
		Architecture string `json:"architecture"`
	}
	if err := decodeCacheRequest(r.Body, &request); err != nil {
		http.Error(w, "invalid Docker image-store request", http.StatusBadRequest)

		return
	}
	if !validCacheArchitecture(request.Architecture) {
		http.Error(w, "invalid Docker image-store architecture", http.StatusBadRequest)

		return
	}
	if err := lockCacheSession(ctx, session); err != nil {
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}
	defer session.mu.Unlock()
	if session.closed {
		http.Error(w, "cache session has ended", http.StatusGone)

		return
	}
	if session.slots[0] != nil {
		http.Error(w, "Docker image-store slot is unavailable", http.StatusConflict)

		return
	}

	key := s.qualifiedKey(dockerStoreKey + request.Architecture)
	volume, err := s.store.Clone(ctx, key, "")
	cold := errors.Is(err, storecontract.ErrMiss)
	if cold {
		volume, err = s.store.Create(ctx, key, cacheVolumeLimit)
	}
	if err != nil {
		s.log.Warn("Docker image store is unavailable; the job can continue cold",
			"instance", session.instance, "error", err)
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}

	session.slots[0] = &cacheAttachment{
		Volume: volume, Publication: publicationLWW, Docker: true,
	}
	if err := s.persistSession(session); err != nil {
		discardErr := s.store.Discard(ctx, volume)
		var retryErr error
		if discardErr == nil {
			session.slots[0] = nil
		} else {
			retryErr = s.persistSession(session)
		}
		s.log.Warn("Docker image-store custody could not be made durable; the job can continue cold",
			"instance", session.instance, "error", errors.Join(err, discardErr, retryErr))
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}

	if err := s.attacher.AttachVolume(ctx, session.instance, 0, volume.Device); err != nil {
		detachErr := s.attacher.DetachVolume(ctx, session.instance, 0, volume.Device)
		var discardErr, clearErr error
		if detachErr == nil {
			discardErr = s.store.Discard(ctx, volume)
			if discardErr == nil {
				session.slots[0] = nil
				clearErr = s.persistSession(session)
				if clearErr != nil {
					session.slots[0] = &cacheAttachment{
						Volume: volume, Publication: publicationLWW, Docker: true,
					}
				}
			}
		}
		s.log.Warn("Docker image store could not be attached; the job can continue cold",
			"instance", session.instance,
			"error", errors.Join(err, detachErr, discardErr, clearErr))
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}

	guestDevice := guestVolumeDevice(0)
	if locator, ok := s.attacher.(provider.GuestVolumeLocator); ok {
		guestDevice = locator.GuestVolumeDevice(0, volume.Device)
	}
	writeCacheJSON(w, http.StatusCreated, map[string]any{
		"slot": 0, "device": guestDevice, "generation": volume.Generation, "cold": cold,
	})
}

func (s *CacheService) markDockerStoreReady(
	w http.ResponseWriter,
	r *http.Request,
	session *cacheSession,
) {
	// Readiness is cooperation from the guest, not authority granted to one
	// process inside it. Workflows have passwordless sudo and Docker-root
	// equivalence, so no metadata bearer can distinguish the helper from workflow
	// code. Settling remains closed until the node independently has both a
	// trusted job classification and GitHub's successful completed-job result.
	var request struct {
		Filesystem storecontract.Filesystem `json:"filesystem"`
	}
	if err := decodeCacheRequest(r.Body, &request); err != nil || request.Filesystem.Valid() != nil {
		http.Error(w, "invalid Docker image-store readiness proof", http.StatusBadRequest)

		return
	}
	if err := lockCacheSession(r.Context(), session); err != nil {
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}
	defer session.mu.Unlock()
	attachment := session.slots[0]
	if session.closed || attachment == nil || !attachment.Docker {
		http.Error(w, "Docker image store is not attached", http.StatusNotFound)

		return
	}
	if !attachment.Settling {
		http.Error(w, "Docker image store is not settling", http.StatusConflict)

		return
	}
	attachment.Volume.Filesystem = request.Filesystem
	attachment.Ready = true
	if err := s.persistSession(session); err != nil {
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}
	writeCacheJSON(w, http.StatusOK, map[string]any{"ready": true})
}

// SettleDocker publishes a prepared store only when GitHub reported success.
func (s *CacheService) SettleDocker(ctx context.Context, instance string, succeeded bool) error {
	s.mu.Lock()
	token, ok := s.byInstance[instance]
	if !ok {
		s.mu.Unlock()

		return nil
	}
	session := s.byToken[token]
	s.mu.Unlock()

	if !succeeded || session.trust != provider.TrustTrusted {
		return nil
	}
	if err := lockCacheSession(ctx, session); err != nil {
		return err
	}
	attachment := session.slots[0]
	if attachment == nil || !attachment.Docker {
		session.mu.Unlock()

		return nil
	}
	if !attachment.Settling {
		attachment.Settling = true
		attachment.Ready = false
		attachment.Volume.Filesystem = storecontract.Filesystem{}
		if err := s.persistSession(session); err != nil {
			session.mu.Unlock()

			return fmt.Errorf("open Docker image-store settlement: %w", err)
		}
	}
	session.mu.Unlock()

	deadline := time.NewTimer(dockerSettleWait)
	defer deadline.Stop()

	for {
		if err := lockCacheSession(ctx, session); err != nil {
			return err
		}
		attachment := session.slots[0]
		if attachment == nil || !attachment.Docker {
			session.mu.Unlock()

			return nil
		}
		if attachment.Ready {
			err := s.publishDocker(ctx, session, attachment)
			session.mu.Unlock()

			return err
		}
		session.mu.Unlock()

		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		case <-deadline.C:
			timer.Stop()

			return errors.New("docker image store did not become ready before teardown")
		case <-timer.C:
		}
	}
}

// publishDocker consumes a ready attachment while its session is locked.
func (s *CacheService) publishDocker(
	ctx context.Context,
	session *cacheSession,
	attachment *cacheAttachment,
) error {
	if err := s.attacher.DetachVolume(ctx, session.instance, 0, attachment.Volume.Device); err != nil {
		return fmt.Errorf("detach Docker image store: %w", err)
	}

	lease, fence, err := s.acquireWriter(ctx, attachment, session.instance)
	if err != nil {
		return fmt.Errorf("acquire Docker image-store writer: %w", err)
	}
	candidate, err := s.store.Snapshot(ctx, attachment.Volume)
	if err != nil {
		return fmt.Errorf("snapshot Docker image store: %w", err)
	}
	session.slots[0] = nil
	if err := s.persistSession(session); err != nil {
		return fmt.Errorf("record consumed Docker image store: %w", err)
	}
	if err := s.publish(ctx, attachment, candidate, lease, fence); err != nil {
		return fmt.Errorf("publish Docker image store: %w", err)
	}

	return nil
}

func validCacheArchitecture(architecture string) bool {
	if architecture == "" || len(architecture) > 64 {
		return false
	}
	for i, character := range []byte(architecture) {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || i > 0 && (character == '.' || character == '_' || character == '-') {
			continue
		}

		return false
	}

	return true
}

func (s *CacheService) authenticate(r *http.Request) (*cacheSession, bool) {
	value := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(value, "Bearer ")
	if !ok || token == "" || strings.ContainsAny(token, " ,\t\r\n") {
		return nil, false
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	session, ok := s.byToken[token]

	return session, ok
}

type attachCacheRequest struct {
	Key         string `json:"key"`
	SizeBytes   int64  `json:"size_bytes"`
	Publication string `json:"publication,omitempty"`
}

func (s *CacheService) attach(w http.ResponseWriter, r *http.Request, session *cacheSession) {
	ctx, cancel := context.WithTimeout(r.Context(), cacheWorkLimit)
	defer cancel()

	var request attachCacheRequest
	if err := decodeCacheRequest(r.Body, &request); err != nil {
		http.Error(w, "invalid cache request", http.StatusBadRequest)

		return
	}

	if strings.TrimSpace(request.Key) == "" || strings.TrimSpace(request.Key) != request.Key ||
		len(request.Key) > cacheKeyLimit ||
		strings.HasPrefix(request.Key, dockerStoreKey) || request.SizeBytes <= 0 ||
		request.SizeBytes > cacheVolumeLimit {
		http.Error(w, "cache key or size is outside the allowed range", http.StatusBadRequest)

		return
	}
	if request.Publication == "" {
		request.Publication = publicationCAS
	}
	if request.Publication != publicationCAS && request.Publication != publicationLWW {
		http.Error(w, "unknown cache publication policy", http.StatusBadRequest)

		return
	}

	if err := lockCacheSession(ctx, session); err != nil {
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}
	defer session.mu.Unlock()
	if session.closed {
		http.Error(w, "cache session has ended", http.StatusGone)

		return
	}

	slot := -1
	for i := range session.slots {
		if session.slots[i] == nil {
			slot = i

			break
		}
	}

	if slot < 0 {
		http.Error(w, "this job already attached five cache volumes", http.StatusConflict)

		return
	}

	key := s.qualifiedKey(request.Key)
	volume, err := s.store.Clone(ctx, key, "")
	cold := errors.Is(err, storecontract.ErrMiss)
	if cold {
		volume, err = s.store.Create(ctx, key, request.SizeBytes)
	}

	if err != nil {
		s.log.Warn("cache volume is unavailable; the job can continue cold",
			"instance", session.instance, "error", err)
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}

	session.slots[slot] = &cacheAttachment{Volume: volume, Publication: request.Publication}
	if err := s.persistSession(session); err != nil {
		discardErr := s.store.Discard(ctx, volume)
		var retryErr error
		if discardErr == nil {
			session.slots[slot] = nil
		} else {
			// Keep the handle in memory and make a second durable attempt. An
			// unmounted backend orphan is also visible to eviction, but that is a
			// backstop rather than custody.
			retryErr = s.persistSession(session)
		}
		s.log.Warn("cache attachment custody could not be made durable; the job can continue cold",
			"instance", session.instance, "error", errors.Join(err, discardErr, retryErr))
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}

	// DURABLE BEFORE ATTACH. A provider call can succeed and lose its response;
	// if the process dies in that window, this record is the only fact that lets
	// restart cleanup find the mapped device and its storage handle.
	if err := s.attacher.AttachVolume(ctx, session.instance, slot, volume.Device); err != nil {
		detachErr := s.attacher.DetachVolume(ctx, session.instance, slot, volume.Device)
		var discardErr, clearErr error
		if detachErr == nil {
			discardErr = s.store.Discard(ctx, volume)
			if discardErr == nil {
				session.slots[slot] = nil
				clearErr = s.persistSession(session)
				if clearErr != nil {
					session.slots[slot] = &cacheAttachment{
						Volume: volume, Publication: request.Publication,
					}
				}
			}
		}
		s.log.Warn("cache volume could not be attached; the job can continue cold",
			"instance", session.instance,
			"error", errors.Join(err, detachErr, discardErr, clearErr))
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}
	guestDevice := guestVolumeDevice(slot)
	if locator, ok := s.attacher.(provider.GuestVolumeLocator); ok {
		guestDevice = locator.GuestVolumeDevice(slot, volume.Device)
	}
	writeCacheJSON(w, http.StatusCreated, map[string]any{
		"slot": slot, "device": guestDevice, "generation": volume.Generation, "cold": cold,
	})
}

func (s *CacheService) commit(w http.ResponseWriter, r *http.Request, session *cacheSession) {
	slot, ok := cacheSlot(r.URL.Path, "/commit")
	if !ok {
		http.NotFound(w, r)

		return
	}

	// Storage cleanup deliberately survives cancellation for at most
	// cacheCleanupMargin, so work stops early enough for the whole handler to stay
	// inside cacheHandlerLimit and the action's client deadline.
	ctx, cancel := context.WithTimeout(r.Context(), cacheWorkLimit)
	defer cancel()
	if err := lockCacheSession(ctx, session); err != nil {
		s.nonFatalCommit(w, session, "wait for session", err)

		return
	}
	defer session.mu.Unlock()
	if session.closed {
		http.Error(w, "cache session has ended", http.StatusGone)

		return
	}

	attachment := session.slots[slot]
	if attachment == nil {
		http.Error(w, "cache slot is not attached", http.StatusNotFound)

		return
	}
	if attachment.Docker {
		http.Error(w, "Docker image stores settle from GitHub's completed-job result", http.StatusForbidden)

		return
	}
	var request struct {
		Filesystem storecontract.Filesystem `json:"filesystem"`
	}
	if r.Body != http.NoBody {
		if err := decodeCacheRequest(r.Body, &request); err != nil {
			http.Error(w, "invalid cache commit", http.StatusBadRequest)

			return
		}
		attachment.Volume.Filesystem = request.Filesystem
	}

	if err := s.attacher.DetachVolume(ctx, session.instance, slot,
		attachment.Volume.Device); err != nil {
		s.nonFatalCommit(w, session, "detach", err)

		return
	}

	if session.trust != provider.TrustTrusted {
		if err := s.store.Discard(ctx, attachment.Volume); err != nil {
			s.nonFatalCommit(w, session, "discard untrusted write", err)

			return
		}

		session.slots[slot] = nil
		if err := s.persistSession(session); err != nil {
			s.nonFatalCommit(w, session, "record discarded untrusted write", err)

			return
		}
		writeCacheJSON(w, http.StatusOK, map[string]any{"published": false, "reason": "untrusted"})

		return
	}

	lease, fence, err := s.acquireWriter(ctx, attachment, session.instance)
	if err != nil {
		s.nonFatalCommit(w, session, "acquire writer", err)

		return
	}

	candidate, err := s.store.Snapshot(ctx, attachment.Volume)
	if err != nil {
		s.nonFatalCommit(w, session, "snapshot", err)

		return
	}

	// Snapshot consumes the writable clone. Whatever publication does next, this
	// slot no longer owns a mapped volume and cleanup must not try to unmap it.
	session.slots[slot] = nil
	if err := s.persistSession(session); err != nil {
		s.nonFatalCommit(w, session, "record consumed cache clone", err)

		return
	}

	if err := s.publish(ctx, attachment, candidate, lease, fence); err != nil {
		s.nonFatalCommit(w, session, "publish", err)

		return
	}

	writeCacheJSON(w, http.StatusOK, map[string]any{
		"published": true, "generation": candidate.Generation,
	})
}

func lockCacheSession(ctx context.Context, session *cacheSession) error {
	for !session.mu.TryLock() {
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()

			return ctx.Err()
		case <-timer.C:
		}
	}

	return nil
}

func (s *CacheService) acquireWriter(
	ctx context.Context,
	attachment *cacheAttachment,
	holder string,
) (storecontract.WriterLease, storecontract.FencingToken, error) {
	for {
		lease, fence, err := s.store.AcquireWriter(ctx, attachment.Volume.Key, holder, cacheWriterTTL)
		if err == nil || attachment.Publication != publicationLWW ||
			!errors.Is(err, storecontract.ErrConflict) {
			return lease, fence, err
		}

		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()

			return storecontract.WriterLease{}, 0, ctx.Err()
		case <-timer.C:
		}
	}
}

func (s *CacheService) publish(
	ctx context.Context,
	attachment *cacheAttachment,
	candidate storecontract.Candidate,
	lease storecontract.WriterLease,
	fence storecontract.FencingToken,
) error {
	err := s.store.PublishCAS(ctx, attachment.Volume.Key, attachment.Volume.Generation,
		candidate, lease, fence)
	if attachment.Publication != publicationLWW || !errors.Is(err, storecontract.ErrConflict) {
		return err
	}

	current, currentErr := s.store.Current(ctx, attachment.Volume.Key)
	if currentErr != nil {
		return errors.Join(err, currentErr)
	}

	return s.store.PublishCAS(ctx, attachment.Volume.Key, current, candidate, lease, fence)
}

func (s *CacheService) discard(w http.ResponseWriter, r *http.Request, session *cacheSession) {
	slot, ok := cacheSlot(r.URL.Path, "/discard")
	if !ok {
		http.NotFound(w, r)

		return
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if session.closed {
		http.Error(w, "cache session has ended", http.StatusGone)

		return
	}

	attachment := session.slots[slot]
	if attachment == nil {
		http.Error(w, "cache slot is not attached", http.StatusNotFound)

		return
	}

	if err := s.attacher.DetachVolume(r.Context(), session.instance, slot,
		attachment.Volume.Device); err != nil {
		s.nonFatalCommit(w, session, "detach discarded volume", err)

		return
	}
	if err := s.store.Discard(r.Context(), attachment.Volume); err != nil {
		s.nonFatalCommit(w, session, "discard rejected volume", err)

		return
	}

	session.slots[slot] = nil
	if err := s.persistSession(session); err != nil {
		s.nonFatalCommit(w, session, "record discarded volume", err)

		return
	}
	writeCacheJSON(w, http.StatusOK, map[string]any{"discarded": true})
}

func cacheSlot(path, suffix string) (int, bool) {
	middle := strings.TrimSuffix(strings.TrimPrefix(path, "/v1/volumes/"), suffix)
	if middle == "" || strings.Contains(middle, "/") {
		return 0, false
	}

	slot, err := strconv.Atoi(middle)

	return slot, err == nil && slot >= 0 && slot < provider.MaxVolumes
}

func (s *CacheService) nonFatalCommit(
	w http.ResponseWriter,
	session *cacheSession,
	operation string,
	err error,
) {
	s.log.Warn("cache commit failed; the job result is unchanged",
		"instance", session.instance, "operation", operation, "error", err)
	writeCacheJSON(w, http.StatusOK, map[string]any{"published": false, "reason": "cache unavailable"})
}

func decodeCacheRequest(body io.ReadCloser, into any) error {
	defer body.Close()

	decoder := json.NewDecoder(io.LimitReader(body, cacheRequestLimit+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(into); err != nil {
		return err
	}

	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("request carries more than one json value")
	}

	return nil
}

func writeCacheJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)

	if err := json.NewEncoder(w).Encode(value); err != nil {
		return
	}
}

func guestVolumeDevice(slot int) string {
	return "/dev/vd" + string(rune('b'+slot))
}
