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

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/provider"
	storecontract "github.com/junioryono/billet/internal/store"
)

const (
	cacheRequestLimit  = 8 << 10
	cacheKeyLimit      = 512
	cacheHandlerLimit  = 12*time.Minute + 45*time.Second
	cacheCleanupMargin = 30 * time.Second
	cacheWorkLimit     = cacheHandlerLimit - cacheCleanupMargin
	cacheWriterTTL     = 15 * time.Minute
	cacheVolumeLimit   = int64(100 << 30)
	dockerSettleWait   = 2 * time.Minute
	dockerStoreKey     = "docker-images/"
	publicationCAS     = "cas"
	publicationLWW     = "last-write-wins"

	// A last-write-wins writer waiting on a live holder polls on this schedule,
	// never past the holder's expiry. Measured before it existed: 100ms flat
	// against S3 was ~20 requests a second for a six-minute wait.
	cacheWriterWaitFloor = 500 * time.Millisecond
	cacheWriterWaitCap   = 30 * time.Second

	// cacheReportLimit bounds telling the control plane what the cache did, the
	// way actionsPolicyLimit bounds asking it about the kill switch: both run
	// inside a guest's cache request, and a plane that is slow must cost the job
	// a diagnostic rather than its cache.
	cacheReportLimit = 2 * time.Second
)

// CacheService lets one authenticated microVM replace its reserved drive slots.
type CacheService struct {
	endpoint  string
	namespace string
	rootState string
	stateDir  string
	store     storecontract.Store
	attacher  provider.VolumeAttacher
	log       *slog.Logger
	wait      func(ctx context.Context, d time.Duration) error
	now       func() time.Time

	mu         sync.Mutex
	byToken    map[string]*cacheSession
	byInstance map[string]string
	actions    *actionsProxy
	actionIO   actionsVolumeManager
	actionRule ActionsPolicy
	observer   CacheObserver
}

type cacheSession struct {
	mu          sync.Mutex
	admit       chan struct{}
	token       string
	instance    string
	trust       provider.TrustClass
	owner       string
	repository  string
	workflowRef string
	intercept   bool
	leaseID     string
	epoch       int64
	observed    cacheObserved
	// inflight counts CacheService calls between dispatch and their recorded
	// outcome, so settlement does not write `unused` over a call still being
	// answered.
	inflight int
	closed   bool
	// finished says the session has left the service's indexes and its record
	// is gone; an outcome recorded after that is reported but no longer written,
	// or the record would come back as an orphan.
	finished bool
	slots    [provider.MaxVolumes]*cacheAttachment
	actions  map[string]*actionsArchive
	receipts map[string]*actionsReceipt
}

// CacheCredentials identifies one managed guest's cache session.
type CacheCredentials struct {
	Token        string
	ActionsProxy string
	ActionsCAPEM string
}

// CacheSessionScope is the static authority configured for one runner pool.
type CacheSessionScope struct {
	Trust       provider.TrustClass
	Intercept   bool
	Owner       string
	Repository  string
	WorkflowRef string
	// LeaseID and Epoch name the lease the guest runs under, so an observation
	// can be attributed after the runner has forgotten the instance. Recorded
	// on the durable session; they authorise nothing, and the runner's own
	// mapping is preferred while it exists because a re-adoption moves the
	// epoch.
	LeaseID string
	Epoch   int64
}

// ActionsPolicy reads the control plane's current interception kill switch.
type ActionsPolicy interface {
	ActionsCacheAllowed(ctx context.Context, owner, repository string) (bool, error)
}

// CacheObserver is told what the cache did for one guest, so the lease's
// history can carry it. The runner implements it.
//
// THE LEASE TRAVELS WITH THE CALL, from the session's durable record, so a
// process that never launched the guest can still attribute what it saw. The
// runner prefers its own mapping while it holds one, because that carries the
// epoch in force now.
type CacheObserver interface {
	ObserveCache(ctx context.Context, instance, leaseID string, epoch int64,
		obs alloc.CacheObservation) error
}

// cacheObserved is what one session has seen the cache do, and whether the
// control plane has been told.
//
// DURABLE WITH THE SESSION, because the report is best effort at the moment of
// observation and is resent when the compute ends; a restart in between must
// not forget what the guest saw. The first observation of each half is kept,
// which is the same rule the ledger applies, so the two cannot disagree about
// which observation was first.
type cacheObserved struct {
	ImageCache      string `json:"image_cache,omitempty"`
	CacheGeneration string `json:"cache_generation,omitempty"`
	ActionsCache    string `json:"actions_cache,omitempty"`
	// Reported says everything above has reached the control plane.
	Reported bool `json:"reported,omitempty"`
	// ActionsPending says the first CacheService call was dispatched and its
	// outcome not yet recorded. DURABLE, so a crash inside that call leaves the
	// Actions half unknown after a restart rather than settled as unused: the
	// guest made a call, and what it got could not be told.
	ActionsPending bool `json:"actions_pending,omitempty"`
}

func (o cacheObserved) observation() alloc.CacheObservation {
	return alloc.CacheObservation{
		ImageCache:      alloc.ImageCache(o.ImageCache),
		CacheGeneration: o.CacheGeneration,
		ActionsCache:    alloc.ActionsCache(o.ActionsCache),
	}
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
		rootState:  stateDir,
		stateDir:   filepath.Join(stateDir, cacheSessionDirectory),
		byToken:    make(map[string]*cacheSession),
		byInstance: make(map[string]string),
		actionIO:   hostActionsVolumeManager{},
		wait:       sleepFor,
		now:        time.Now,
	}
	if err := service.loadSessions(); err != nil {
		return nil, err
	}
	for _, session := range service.byToken {
		if session.intercept {
			if _, err := service.ensureActionsProxy(); err != nil {
				return nil, err
			}

			break
		}
	}

	return service, nil
}

func (s *CacheService) qualifiedKey(key string) string { return s.namespace + "/" + key }

// Endpoint is the origin placed in a microVM's metadata.
func (s *CacheService) Endpoint() string { return s.endpoint }

// SetActionsPolicy installs the control-plane policy reader before the listener starts.
func (s *CacheService) SetActionsPolicy(policy ActionsPolicy) { s.actionRule = policy }

// SetCacheObserver installs where observations are reported, before the
// listener starts. Without one they are kept on the session and reported to
// nobody.
func (s *CacheService) SetCacheObserver(observer CacheObserver) { s.observer = observer }

// observe records what the cache did for a session and tells the control
// plane. Called with session.mu held.
//
// THE FIRST OBSERVATION OF EACH HALF IS KEPT, and the generation moves with
// the image-cache token. A half already observed is left alone, so a repeat
// costs nothing and a later, different observation cannot replace what the
// guest first saw.
func (s *CacheService) observe(ctx context.Context, session *cacheSession, obs alloc.CacheObservation) {
	changed := false

	if obs.ImageCache != "" && session.observed.ImageCache == "" {
		session.observed.ImageCache = string(obs.ImageCache)
		session.observed.CacheGeneration = obs.CacheGeneration
		changed = true
	}

	if obs.ActionsCache != "" && session.observed.ActionsCache == "" {
		session.observed.ActionsCache = string(obs.ActionsCache)
		session.observed.ActionsPending = false
		changed = true
	}

	if !changed {
		return
	}

	session.observed.Reported = false

	// A SESSION ALREADY FINISHED HAS NO RECORD TO WRITE. Its file is gone and
	// it has left the indexes, so a write here would leave an orphan that loads
	// on the next start as a session for compute that does not exist. The
	// outcome is still reported, with the lease the session carries, and lost
	// only if that report fails.
	if session.finished {
		s.report(ctx, session)

		return
	}

	// DURABLE BEFORE IT IS SENT, so a report the plane never answered is resent
	// by whichever process ends the compute. A record that could not be written
	// is not reported either: an observation that exists only in memory would be
	// told to the plane once and, after a restart, never again, and the plane
	// would hold a fact this host cannot account for.
	if err := s.persistSession(session); err != nil {
		s.log.Warn("could not make a cache observation durable; it is kept in memory and "+
			"reported when the session next persists", "instance", session.instance, "error", err)

		return
	}

	s.report(ctx, session)
}

// report tells the control plane what the session has observed, bounded, and
// records that it landed. Called with session.mu held.
//
// BEST EFFORT HERE, AUTHORITATIVE AT THE END. A failure leaves Reported false,
// and settleObservation resends when the compute ends; what a plane blip costs
// is nothing, and what a wedged teardown costs is the resend. The caller's
// context is detached because it is a guest's request, and a guest that went
// away is no reason to lose what it saw.
func (s *CacheService) report(ctx context.Context, session *cacheSession) {
	if s.observer == nil || session.observed.Reported {
		return
	}

	obs := session.observed.observation()
	if obs == (alloc.CacheObservation{}) {
		return
	}

	reportCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheReportLimit)
	defer cancel()

	if err := s.observer.ObserveCache(reportCtx, session.instance, session.leaseID,
		session.epoch, obs); err != nil {
		s.log.Warn("could not record what the cache did for a job; it is kept here and resent "+
			"when the compute ends", "instance", session.instance, "error", err)

		return
	}

	session.observed.Reported = true
	if session.finished {
		return
	}
	if err := s.persistSession(session); err != nil {
		s.log.Warn("could not record that a cache observation was reported; it may be resent",
			"instance", session.instance, "error", err)
	}
}

// settleObservation fills in whatever the session ended without observing and
// makes sure the control plane has been told. Called with session.mu held.
//
// WHAT IS WRITTEN IS STILL AN OBSERVATION: the session closed and the guest
// never asked for an image store, or never made a CacheService request. A
// session with no interception says so as "off", which is the one token that
// comes from how the session was scoped rather than from a request, because a
// guest with no proxy can never make one.
func (s *CacheService) settleObservation(ctx context.Context, session *cacheSession) {
	var obs alloc.CacheObservation

	if session.observed.ImageCache == "" {
		obs.ImageCache = alloc.ImageCacheUnused
	}

	// A CALL STILL BEING ANSWERED IS NOT "UNUSED". Its outcome is recorded
	// when the handler returns, under this same lock, and reported then with
	// the lease the session carries. A call a crash interrupted is not "unused"
	// either: the durable pending mark says the guest asked, and what it got
	// stays unknown.
	if session.observed.ActionsCache == "" && session.inflight == 0 &&
		!session.observed.ActionsPending {
		obs.ActionsCache = alloc.ActionsCacheOff
		if session.intercept {
			obs.ActionsCache = alloc.ActionsCacheUnused
		}
	}

	if obs != (alloc.CacheObservation{}) {
		s.observe(ctx, session, obs)

		return
	}

	s.report(ctx, session)
}

// beginActionsCall marks a CacheService call as in flight, and endActionsCall
// clears it once its outcome has been recorded. Between the two, settlement
// leaves the Actions half alone.
//
// THE FIRST CALL IS ALSO MARKED DURABLY, before it is dispatched, so a crash
// inside it does not let the next process settle the half as unused. The mark
// is cleared by the outcome's own write.
func (s *CacheService) beginActionsCall(session *cacheSession) {
	session.mu.Lock()
	defer session.mu.Unlock()

	session.inflight++

	if session.observed.ActionsCache != "" || session.observed.ActionsPending || session.finished {
		return
	}

	session.observed.ActionsPending = true
	if err := s.persistSession(session); err != nil {
		s.log.Warn("could not record that an Actions cache call is in flight; a crash inside it "+
			"would settle the job's cache outcome as unused", "instance", session.instance,
			"error", err)
	}
}

func (s *CacheService) endActionsCall(session *cacheSession) {
	session.mu.Lock()
	session.inflight--
	session.mu.Unlock()
}

// SettleObservation settles what the cache did for one instance while the
// caller still holds its lease.
//
// CALLED BY THE RUNNER BEFORE IT FORGETS A REQUEST, because the report needs
// the lease the instance belongs to and Cleanup runs after that mapping is
// gone. Cleanup settles too, for the paths where the mapping survives it.
func (s *CacheService) SettleObservation(ctx context.Context, instance string) {
	s.mu.Lock()
	token, ok := s.byInstance[instance]
	if !ok {
		s.mu.Unlock()

		return
	}

	session := s.byToken[token]
	s.mu.Unlock()

	// DETACHED FROM THE TEARDOWN'S CONTEXT, and bounded. A destroy arrives on a
	// context that may already be cancelled, and a settlement that gave up on
	// the lock wait would let the runner forget the lease before the last
	// report was attempted. The bound keeps a wedged session from holding the
	// teardown.
	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheReportLimit)
	defer cancel()

	if err := lockCacheSession(settleCtx, session); err != nil {
		s.log.Warn("could not settle what the cache did for a job before its compute was "+
			"forgotten; the session's cleanup will try once more",
			"instance", instance, "error", err)

		return
	}
	defer session.mu.Unlock()

	s.settleObservation(settleCtx, session)
}

// Prepare creates one unguessable session credential before compute starts.
func (s *CacheService) Prepare(instance string, trust provider.TrustClass) (CacheCredentials, error) {
	return s.PrepareScoped(instance, CacheSessionScope{Trust: trust})
}

// PrepareScoped creates a session bound to the pool's configured cache scope.
func (s *CacheService) PrepareScoped(
	instance string,
	scope CacheSessionScope,
) (CacheCredentials, error) {
	if strings.TrimSpace(instance) == "" {
		return CacheCredentials{}, errors.New("node: a cache session needs an instance")
	}

	if scope.Trust != provider.TrustTrusted && scope.Trust != provider.TrustUntrusted {
		return CacheCredentials{}, errors.New("node: cannot give cache access to work with unknown trust")
	}
	if scope.Intercept {
		if err := validateActionsScope(scope); err != nil {
			return CacheCredentials{}, err
		}
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
			token: token, instance: instance, trust: scope.Trust,
			owner: scope.Owner, repository: scope.Repository, workflowRef: scope.WorkflowRef,
			intercept: scope.Intercept,
			leaseID:   scope.LeaseID, epoch: scope.Epoch,
			admit:    make(chan struct{}, 1),
			actions:  make(map[string]*actionsArchive),
			receipts: make(map[string]*actionsReceipt),
		}
		credentials := CacheCredentials{Token: token}
		if scope.Intercept {
			proxy, err := s.ensureActionsProxy()
			if err != nil {
				return CacheCredentials{}, err
			}
			credentials.ActionsProxy, err = proxyURL(s.endpoint, token)
			if err != nil {
				return CacheCredentials{}, err
			}
			credentials.ActionsCAPEM = string(proxy.ca.CertPEM())
		}
		if err := s.persistSession(session); err != nil {
			return CacheCredentials{}, err
		}

		s.byToken[token] = session
		s.byInstance[instance] = token

		return credentials, nil
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
	session.finished = true

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
		if err := lockCacheSession(ctx, session); err != nil {
			failures = append(failures, err)

			continue
		}
		if !session.closed {
			for slot, attachment := range session.slots {
				if attachment == nil || attachment.Volume.Lease.ID == "" {
					continue
				}
				if err := s.store.RenewActive(ctx, attachment.Volume, until); err != nil {
					failures = append(failures, fmt.Errorf("%s slot %d: %w", session.instance, slot, err))
				}
			}
			for id, archive := range session.actions {
				if archive.Volume.Lease.ID == "" {
					continue
				}
				if err := s.store.RenewActive(ctx, archive.Volume, until); err != nil {
					failures = append(failures, fmt.Errorf("%s Actions cache %s: %w",
						session.instance, id, err))
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
	if err := lockCacheSession(ctx, session); err != nil {
		return err
	}
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

	// THE SESSION IS OVER, so what it never observed is now known, and a report
	// the plane never answered gets its last chance from this process.
	s.settleObservation(ctx, session)

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
	for id, archive := range session.actions {
		if err := lockCacheMutex(ctx, &archive.mu); err != nil {
			failures = append(failures, fmt.Errorf("actions cache %s wait for archive: %w", id, err))

			break
		}
		if !archive.Unmounted {
			if err := s.actionIO.Unmount(ctx, s.actionsMountPath(session, archive)); err != nil {
				failures = append(failures, fmt.Errorf("actions cache %s unmount: %w", id, err))
				archive.mu.Unlock()

				continue
			}
		}
		if err := s.store.Discard(ctx, archive.Volume); err != nil {
			failures = append(failures, fmt.Errorf("actions cache %s discard: %w", id, err))
			archive.mu.Unlock()

			continue
		}
		archive.mu.Unlock()
		delete(session.actions, id)
		if err := s.persistSession(session); err != nil {
			return fmt.Errorf("node: record discarded Actions cache of %s: %w",
				session.instance, err)
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
	if r.Method == http.MethodConnect {
		proxy := s.actionsProxy()
		if proxy == nil {
			http.Error(w, "actions interception is unavailable", http.StatusServiceUnavailable)

			return
		}
		proxy.serveConnect(w, r)

		return
	}
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
		s.observe(ctx, session, alloc.CacheObservation{ImageCache: alloc.ImageCacheUnavailable})
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}

	// WHAT THE STORE ANSWERED: a clone of a generation is warm and names it, a
	// miss that became a fresh volume is cold. OBSERVED ONLY ONCE THE VOLUME IS
	// IN DURABLE CUSTODY, below: an observation persists the session and then
	// talks to the plane, and doing either before the slot is recorded is a
	// window in which a crash leaves a volume restart cleanup cannot find.
	observed := alloc.CacheObservation{ImageCache: alloc.ImageCacheCold}
	if !cold {
		observed = alloc.CacheObservation{
			ImageCache: alloc.ImageCacheWarm, CacheGeneration: volume.Generation,
		}
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
		s.observe(ctx, session, alloc.CacheObservation{ImageCache: alloc.ImageCacheUnavailable})
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
		s.observe(ctx, session, alloc.CacheObservation{ImageCache: alloc.ImageCacheUnavailable})
		http.Error(w, "cache unavailable", http.StatusServiceUnavailable)

		return
	}

	// THE GUEST HAS ITS STORE, so what the store answered is what it saw.
	s.observe(ctx, session, observed)

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
	if _, step, err := s.publishAcquired(ctx, session, 0, attachment, lease, fence); err != nil {
		return fmt.Errorf("%s Docker image store: %w", step, err)
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

	candidate, step, err := s.publishAcquired(ctx, session, slot, attachment, lease, fence)
	if err != nil {
		s.nonFatalCommit(w, session, commitOperation(step), err)

		return
	}

	writeCacheJSON(w, http.StatusOK, map[string]any{
		"published": true, "generation": candidate.Generation,
	})
}

// publishStep names the step of publishAcquired that failed, for the caller's
// own diagnostic.
type publishStep string

const (
	stepSnapshot       publishStep = "snapshot"
	stepRecordConsumed publishStep = "record consumed"
	stepPublish        publishStep = "publish"
)

func commitOperation(step publishStep) string {
	if step == stepRecordConsumed {
		return "record consumed cache clone"
	}

	return string(step)
}

// publishAcquired runs everything between a writer acquisition and the pointer
// advance, and gives the writer back on any failure: left standing, the lease
// holds the key for its whole lifetime and every other writer of the key waits
// it out. The consumed slot is cleared here because Snapshot consumes the
// writable clone: whatever publication does next, the slot no longer owns a
// mapped volume and cleanup must not try to unmap it.
func (s *CacheService) publishAcquired(
	ctx context.Context,
	session *cacheSession,
	slot int,
	attachment *cacheAttachment,
	lease storecontract.WriterLease,
	fence storecontract.FencingToken,
) (storecontract.Candidate, publishStep, error) {
	candidate, err := s.store.Snapshot(ctx, attachment.Volume)
	if err != nil {
		return storecontract.Candidate{}, stepSnapshot, errors.Join(err, s.releaseWriter(ctx, lease, fence))
	}
	session.slots[slot] = nil
	if err := s.persistSession(session); err != nil {
		return storecontract.Candidate{}, stepRecordConsumed,
			errors.Join(err, s.releaseWriter(ctx, lease, fence))
	}
	if err := s.publish(ctx, attachment, candidate, lease, fence); err != nil {
		return storecontract.Candidate{}, stepPublish, errors.Join(err, s.releaseWriter(ctx, lease, fence))
	}

	return candidate, "", nil
}

// releaseWriter runs on a failure path, so it must not inherit the failure: a
// commit whose budget expired still owes the key back, bounded by the margin
// the handler reserves for storage cleanup.
func (s *CacheService) releaseWriter(
	ctx context.Context,
	lease storecontract.WriterLease,
	fence storecontract.FencingToken,
) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheCleanupMargin)
	defer cancel()
	if err := s.store.ReleaseWriter(ctx, lease, fence); err != nil {
		return fmt.Errorf("release cache writer: %w", err)
	}

	return nil
}

func lockCacheSession(ctx context.Context, session *cacheSession) error {
	return lockCacheMutex(ctx, &session.mu)
}

func lockCacheMutex(ctx context.Context, mutex *sync.Mutex) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}
	for !mutex.TryLock() {
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

// acquireWriter takes the key's writer. A last-write-wins publication waits
// for a live holder instead of failing; the wait is a doubling back-off capped
// at cacheWriterWaitCap and never longer than the holder's remaining lease —
// a poll bounded by the expiry rather than one sleep to it, because a holder
// whose publication fails gives the lease back early. It is announced once per
// lease it waits on — a holder that re-acquires has a new deadline, and the
// deadline is the point of the announcement — so a settlement that lasts
// minutes does not read as a hang.
func (s *CacheService) acquireWriter(
	ctx context.Context,
	attachment *cacheAttachment,
	holder string,
) (storecontract.WriterLease, storecontract.FencingToken, error) {
	key := attachment.Volume.Key
	started := s.now()
	delay := cacheWriterWaitFloor
	var announced *storecontract.WriterHeldError
	contended := false
	for attempt := 0; ; attempt++ {
		lease, fence, err := s.store.AcquireWriter(ctx, key, holder, cacheWriterTTL)
		if err == nil && attempt > 0 {
			s.log.Info("acquired the cache writer after waiting",
				"instance", holder, "key", key,
				"waited", s.now().Sub(started).Round(time.Millisecond))
		}
		if err == nil || attachment.Publication != publicationLWW ||
			!errors.Is(err, storecontract.ErrConflict) {
			return lease, fence, err
		}

		pause := delay
		if held, ok := errors.AsType[*storecontract.WriterHeldError](err); ok {
			if announced == nil || announced.Holder != held.Holder ||
				!announced.Expires.Equal(held.Expires) {
				s.log.Info("waiting for another writer of this cache key to finish or for its lease to end",
					"instance", holder, "key", key, "held_by", held.Holder,
					"until", held.Expires.UTC().Format(time.RFC3339),
					"remaining", held.Expires.Sub(s.now()).Round(time.Second))
				announced = held
			}
			// Past the expiry the store still refused, which is its clock against
			// this one; the schedule carries on rather than spinning.
			if remaining := held.Expires.Sub(s.now()); remaining > 0 && remaining < pause {
				pause = remaining
			}
		} else if !contended {
			s.log.Info("waiting to retry a contended cache writer acquisition",
				"instance", holder, "key", key, "error", err)
			contended = true
		}
		if err := s.wait(ctx, pause); err != nil {
			return storecontract.WriterLease{}, 0, err
		}
		delay = min(delay*2, cacheWriterWaitCap)
	}
}

// sleepFor is the default CacheService.wait: one timer, stopped on cancellation.
func sleepFor(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	select {
	case <-ctx.Done():
		timer.Stop()

		return ctx.Err()
	case <-timer.C:
		return nil
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
