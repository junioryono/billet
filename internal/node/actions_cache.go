package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
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
	actionsCreatePath   = "/twirp/github.actions.results.api.v1.CacheService/CreateCacheEntry"
	actionsFinalizePath = "/twirp/github.actions.results.api.v1.CacheService/FinalizeCacheEntryUpload"
	actionsDownloadPath = "/twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL"
	actionsBlobPrefix   = "/_billet/actions-cache/"
	actionsArchiveLimit = int64(10 << 30)
	actionsVolumeSize   = int64(22 << 30)
	actionsArchiveCount = 32
	actionsRequestLimit = 64 << 10
	actionsPolicyLimit  = 2 * time.Second
	actionsModeUpload   = "upload"
	actionsModeDownload = "download"
	actionsLocalHeader  = "X-Billet-Actions-Cache"
	actionsUserAgent    = "@actions/cache-"

	// INTENDED FOR BILLET'S LOOPBACK ADAPTER, AND NOT AN IDENTITY CREDENTIAL.
	// Any process in the guest can set it, exactly as any of them could already
	// send "@actions/cache-1.0" and be served locally. It grants nothing that
	// forging the user agent did not: what bounds both is the same boundary —
	// interception is refused on an untrusted pool, requires a static
	// cache_scope, and that scope's workflow must be one the tier allowlists, so
	// anything reaching either gate is a workflow this deployment vouched for.
	//
	// It exists because BuildKit cannot be told apart by user agent without
	// admitting every client that types "buildkit/", and because a plaintext
	// blob URL is only usable by a caller inside the guest the adapter serves.
	actionsClientHeader = "X-Billet-Cache-Client"
	actionsOriginHeader = "X-Billet-Cache-Origin"
	actionsClientLocal  = "billet-loopback"
)

type actionsArchive struct {
	mu        sync.Mutex           `json:"-"`
	ID        string               `json:"id"`
	Signature string               `json:"signature"`
	Mode      string               `json:"mode"`
	CacheKey  string               `json:"cache_key"`
	StoreKey  string               `json:"store_key"`
	Version   string               `json:"version"`
	Volume    storecontract.Volume `json:"volume"`
	Unmounted bool                 `json:"unmounted,omitempty"`
}

type actionsReceipt struct {
	ID        string                     `json:"id"`
	CacheKey  string                     `json:"cache_key"`
	StoreKey  string                     `json:"store_key"`
	Version   string                     `json:"version"`
	Candidate storecontract.Candidate    `json:"candidate"`
	Lease     storecontract.WriterLease  `json:"lease"`
	Fence     storecontract.FencingToken `json:"fence"`
	EntryID   string                     `json:"entry_id"`
	Size      int64                      `json:"size_bytes"`
	Complete  bool                       `json:"complete,omitempty"`
	OK        bool                       `json:"ok,omitempty"`
}

func (r *actionsReceipt) valid() error {
	rawID, idErr := hex.DecodeString(r.ID)
	entryID, err := strconv.ParseInt(r.EntryID, 10, 64)
	if idErr != nil || len(rawID) != 32 || r.ID != actionsReceiptID(r.CacheKey, r.Version) ||
		!validActionsCacheField(r.CacheKey) ||
		!validActionsCacheField(r.Version) || strings.TrimSpace(r.StoreKey) == "" ||
		r.Candidate.Key != r.StoreKey || r.Candidate.Generation == "" ||
		r.Candidate.Handle == "" || r.Candidate.Filesystem.Valid() != nil ||
		r.Lease.Key != r.StoreKey || r.Lease.ID == "" || r.Fence == 0 || err != nil || entryID <= 0 ||
		r.Size < 0 || r.Size > actionsArchiveLimit || r.OK && !r.Complete {
		return errors.New("invalid finalization receipt")
	}

	return nil
}

func (a *actionsArchive) valid() error {
	for name, value := range map[string]string{"id": a.ID, "signature": a.Signature} {
		raw, err := hex.DecodeString(value)
		if err != nil || len(raw) != 32 {
			return fmt.Errorf("invalid %s", name)
		}
	}
	if a.Mode != actionsModeUpload && a.Mode != actionsModeDownload {
		return errors.New("invalid mode")
	}
	if !validActionsCacheField(a.CacheKey) || !validActionsCacheField(a.Version) ||
		strings.TrimSpace(a.StoreKey) == "" || a.Volume.Key != a.StoreKey ||
		a.Volume.Handle == "" || a.Volume.Device == "" {
		return errors.New("invalid cache identity or volume")
	}
	if a.Mode == actionsModeDownload && a.Volume.Generation == "" {
		return errors.New("download has no generation")
	}
	if a.Mode == actionsModeDownload && a.Unmounted {
		return errors.New("download is marked unmounted")
	}

	return nil
}

func (s *CacheService) actionsMountPath(session *cacheSession, archive *actionsArchive) string {
	return filepath.Join(s.rootState, "actions-cache-volumes", session.token, archive.ID)
}

func (s *CacheService) actionsArchivePath(session *cacheSession, archive *actionsArchive) string {
	return filepath.Join(s.actionsMountPath(session, archive), "archive")
}

func (s *CacheService) actionsStagingPath(
	session *cacheSession,
	archive *actionsArchive,
) string {
	return filepath.Join(s.actionsMountPath(session, archive), "staging")
}

func (s *CacheService) actionsBlockPath(
	session *cacheSession,
	archive *actionsArchive,
	blockID string,
) string {
	digest := sha256.Sum256([]byte(blockID))

	return filepath.Join(s.actionsStagingPath(session, archive), "blocks",
		hex.EncodeToString(digest[:]))
}

func actionsReceiptID(key, version string) string {
	digest := sha256.Sum256([]byte(key + "\x00" + version))

	return hex.EncodeToString(digest[:])
}

func (s *CacheService) actionsResponse(
	req *http.Request,
	session *cacheSession,
) (*http.Response, bool, error) {
	// A CacheService call is what the observation is about. The blob legs that
	// follow a served call, and everything on the artifact side, are not. Only
	// the dispositions decided HERE are recorded here; a call that goes to a
	// handler is recorded by the proxy, once the handler has answered.
	cacheCall := actionsCacheCall(req)

	if !actionsLocalRequest(req) {
		if cacheCall {
			s.observeActions(req.Context(), session, alloc.ActionsCacheSpliced)
		}

		return nil, false, nil
	}

	// Untrusted work never writes a generation that trusted work can read. GitHub
	// remains authoritative for untrusted-pool cache traffic; Billet does not
	// derive storage authority from the assignment that caused scale-up.
	if session.trust != provider.TrustTrusted || s.actionRule == nil {
		if cacheCall {
			s.observeActions(req.Context(), session, alloc.ActionsCacheSpliced)
		}

		return nil, false, nil
	}
	policyCtx, cancel := context.WithTimeout(req.Context(), actionsPolicyLimit)
	allowed, policyErr := s.actionRule.ActionsCacheAllowed(policyCtx,
		session.owner, session.repository)
	cancel()
	if policyErr != nil {
		s.log.Warn("Actions cache policy is unavailable",
			"instance", session.instance, "owner", session.owner,
			"repository", session.repository, "error", policyErr)
		if cacheCall {
			s.observeActions(req.Context(), session, alloc.ActionsCacheSpliced)
		}

		return nil, false, nil
	}
	if !allowed {
		if cacheCall {
			s.observeActions(req.Context(), session, alloc.ActionsCacheDisabled)
		}

		return nil, false, nil
	}
	// A CALL THAT IS ANSWERED HERE IS NOT OBSERVED HERE. Whether the guest was
	// served is known only once the handler has answered, and a handler that
	// fails is retried through GitHub by the proxy, which is where the final
	// disposition is recorded.
	// Resolved once, from the request, and checked there: a signed URL must name
	// an origin this client can reach without TLS, and only the request says
	// which that is.
	origin := actionsLoopbackOrigin(req)

	switch req.URL.Path {
	case actionsCreatePath:
		response, err := s.createActionsCache(req.Context(), req.Body, session, origin)
		return response, true, err
	case actionsFinalizePath:
		response, err := s.finalizeActionsCache(req.Context(), req.Body, session)
		return response, true, err
	case actionsDownloadPath:
		response, err := s.findActionsCache(req.Context(), req.Body, session, origin)
		return response, true, err
	default:
		response, err := s.serveActionsBlob(req, session)
		return response, true, err
	}
}

// actionsCacheCall reports whether a request is one of the three CacheService
// calls billet serves, whoever sent it.
func actionsCacheCall(req *http.Request) bool {
	if req.URL.RawPath != "" {
		return false
	}

	switch req.URL.Path {
	case actionsCreatePath, actionsFinalizePath, actionsDownloadPath:
		return true
	default:
		return false
	}
}

// observeActions records what interception did for a CacheService call.
//
// TAKES THE SESSION LOCK ITSELF, briefly, before the handlers below take it
// for the work: the observation is decided by then, and holding the lock across
// the report would hold it across a plane round trip inside every cache call.
func (s *CacheService) observeActions(
	ctx context.Context, session *cacheSession, outcome alloc.ActionsCache,
) {
	if err := lockCacheSession(ctx, session); err != nil {
		return
	}
	defer session.mu.Unlock()

	s.observe(ctx, session, alloc.CacheObservation{ActionsCache: outcome})
}

func actionsLocalRequest(req *http.Request) bool {
	if req.URL.RawPath != "" {
		return false
	}
	if strings.HasPrefix(req.URL.Path, actionsBlobPrefix) {
		return true
	}

	switch req.URL.Path {
	case actionsCreatePath, actionsFinalizePath, actionsDownloadPath:
		return actionsTwirpRequest(req)
	default:
		return false
	}
}

func actionsTwirpRequest(req *http.Request) bool {
	if req.Method != http.MethodPost || req.URL.RawQuery != "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(req.Header.Get("Content-Type"))

	if err != nil || mediaType != "application/json" {
		return false
	}

	// The official toolkit client by its user agent, or billet's own adapter by
	// the header it sets. A container-driver BuildKit is neither on its own: it
	// sends "buildkit/<version>" and is spliced upstream, which is why the
	// adapter has to speak for it.
	// A CALLER CLAIMING THE ADAPTER MUST BRING A USABLE ORIGIN. Admitted without
	// one, it is served locally and handed an https results-host URL it cannot
	// verify — the x509 failure this whole path exists to remove, arriving after
	// storage has already been allocated. Refusing admission instead splices it
	// upstream, which is what happens to such a client today.
	if req.Header.Get(actionsClientHeader) == actionsClientLocal {
		return actionsLoopbackOrigin(req) != ""
	}

	return strings.HasPrefix(req.UserAgent(), actionsUserAgent) &&
		len(req.UserAgent()) > len(actionsUserAgent)
}

// actionsLoopbackOrigin is the origin a signed blob URL must name for a client
// that reached billet through the guest's plaintext adapter.
//
// THE WHOLE POINT OF THE ADAPTER IS DEFEATED WITHOUT THIS. A locally served
// entry answers with a signed URL, and building that URL against the results
// host sends the client back over TLS to a certificate it does not trust — the
// x509 failure this path exists to remove, one leg later.
//
// The value comes from the client, so it is checked rather than believed: only
// a bare http origin on a loopback or private address is accepted. THE PRIVATE
// RANGE IS THERE FOR THE GUEST'S DOCKER GATEWAY: the adapter binds it so that a
// builder running in a container reaches the adapter with no `network=host`,
// and a signed URL naming that address is usable only inside the guest's own
// network, exactly as a loopback one is. A public address, a link-local one or
// a name is refused, because a URL naming any of those is a token carried
// somewhere billet does not serve.
func actionsLoopbackOrigin(req *http.Request) string {
	if req.Header.Get(actionsClientHeader) != actionsClientLocal {
		return ""
	}

	raw := req.Header.Get(actionsOriginHeader)
	if raw == "" {
		return ""
	}

	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Path != "" || u.RawQuery != "" || u.User != nil {
		return ""
	}

	host := u.Hostname()
	if host == "" {
		return ""
	}

	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && !ip.IsPrivate()) {
		return ""
	}

	// An explicit port has to be one that can be dialled. "0", "99999" and ""
	// all survive url.Parse and all mint a URL the client cannot use.
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return ""
		}
	} else if strings.Contains(u.Host, ":") && !strings.HasSuffix(u.Host, "]") {
		return ""
	}

	return u.Scheme + "://" + u.Host
}

func decodeActionsRequest(body io.Reader, into any) error {
	limited := io.LimitReader(body, actionsRequestLimit+1)
	decoder := json.NewDecoder(limited)
	if err := decoder.Decode(into); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("request has trailing data")
	}

	return nil
}

func validActionsCacheField(value string) bool {
	return value != "" && len(value) <= cacheKeyLimit && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func actionsScopeDigest(session *cacheSession) string {
	digest := sha256.Sum256([]byte(session.owner + "\x00" + session.repository + "\x00" +
		session.workflowRef))

	return hex.EncodeToString(digest[:])
}

func actionsVersionPrefix(session *cacheSession, version string) string {
	digest := sha256.Sum256([]byte(version))

	return "actions-cache/" + actionsScopeDigest(session) + "/" + hex.EncodeToString(digest[:]) + "/"
}

func (s *CacheService) actionsStoreKey(session *cacheSession, key, version string) string {
	return s.qualifiedKey(actionsVersionPrefix(session, version) + key)
}

func (s *CacheService) actionsSignedURL(archive *actionsArchive, origin string) string {
	u := url.URL{
		Scheme: "https", Host: actionsResultsHost, Path: actionsBlobPrefix + archive.ID,
	}

	// A loopback client gets a loopback URL. The path and signature are the same;
	// only the origin moves, so the blob leg avoids TLS exactly as the metadata
	// leg does. An empty origin is every other client, unchanged.
	if origin != "" {
		if parsed, err := url.Parse(origin); err == nil {
			u.Scheme, u.Host = parsed.Scheme, parsed.Host
		}
	}
	query := u.Query()
	query.Set("sig", archive.Signature)
	u.RawQuery = query.Encode()

	return u.String()
}

func (s *CacheService) createActionsCache(
	ctx context.Context,
	body io.Reader,
	session *cacheSession,
	origin string,
) (*http.Response, error) {
	var request struct {
		Key     string `json:"key"`
		Version string `json:"version"`
	}
	if err := decodeActionsRequest(body, &request); err != nil ||
		!validActionsCacheField(request.Key) || !validActionsCacheField(request.Version) {
		return nil, errors.New("invalid Actions cache reservation")
	}
	storeKey := s.actionsStoreKey(session, request.Key, request.Version)
	if err := lockCacheSession(ctx, session); err != nil {
		return nil, err
	}
	defer session.mu.Unlock()
	if session.closed {
		return nil, errors.New("cache session has ended")
	}
	if len(session.actions)+len(session.receipts) >= actionsArchiveCount {
		return nil, errors.New("this job has too many active Actions cache archives")
	}
	if session.receipts[actionsReceiptID(request.Key, request.Version)] != nil {
		return actionsJSONResponse(map[string]any{
			"ok": false, "message": "this job already finalized that cache key and version",
		})
	}
	for _, active := range session.actions {
		if active.Mode == actionsModeUpload && active.CacheKey == request.Key &&
			active.Version == request.Version {
			return actionsJSONResponse(map[string]any{
				"ok": false, "message": "this job already reserved that cache key and version",
			})
		}
	}
	if _, err := s.store.Current(ctx, storeKey); err == nil {
		return actionsJSONResponse(map[string]any{
			"ok": false, "message": "a cache entry already exists for this key and version",
		})
	} else if !errors.Is(err, storecontract.ErrMiss) {
		return nil, err
	}

	id, err := randomCacheToken()
	if err != nil {
		return nil, err
	}
	signature, err := randomCacheToken()
	if err != nil {
		return nil, err
	}
	volume, err := s.store.Create(ctx, storeKey, actionsVolumeSize)
	if err != nil {
		return nil, err
	}
	archive := &actionsArchive{
		ID: id, Signature: signature, Mode: actionsModeUpload,
		CacheKey: request.Key, StoreKey: storeKey, Version: request.Version, Volume: volume,
	}
	session.actions[id] = archive
	if err := s.persistSession(session); err != nil {
		discardErr := s.store.Discard(ctx, volume)
		var retryErr error
		if discardErr == nil {
			delete(session.actions, id)
		} else {
			retryErr = s.persistSession(session)
		}

		return nil, errors.Join(err, discardErr, retryErr)
	}

	mountPath := s.actionsMountPath(session, archive)
	if err := s.actionIO.MountNew(ctx, volume.Device, mountPath); err != nil {
		return nil, errors.Join(err, s.discardActionsArchive(ctx, session, archive))
	}
	if err := os.MkdirAll(filepath.Join(s.actionsStagingPath(session, archive), "blocks"),
		0o700); err != nil {
		return nil, errors.Join(err, s.discardActionsArchive(ctx, session, archive))
	}

	return actionsJSONResponse(map[string]any{
		"ok": true, "signed_upload_url": s.actionsSignedURL(archive, origin),
	})
}

// discardActionsArchive is called while session.mu is held. Custody is removed
// only after both the host mount and the storage handle are gone.
func (s *CacheService) discardActionsArchive(
	ctx context.Context,
	session *cacheSession,
	archive *actionsArchive,
) error {
	if !archive.Unmounted {
		if err := s.actionIO.Unmount(ctx, s.actionsMountPath(session, archive)); err != nil {
			return err
		}
		archive.Unmounted = true
		if err := s.persistSession(session); err != nil {
			return err
		}
	}
	if err := s.store.Discard(ctx, archive.Volume); err != nil {
		return err
	}
	delete(session.actions, archive.ID)
	if err := s.persistSession(session); err != nil {
		session.actions[archive.ID] = archive

		return err
	}

	return nil
}

type actionsFinalizeRequest struct {
	Key       string          `json:"key"`
	Version   string          `json:"version"`
	SizeBytes json.RawMessage `json:"size_bytes"`
}

func (s *CacheService) actionsFinalizeReserved(
	ctx context.Context,
	body []byte,
	session *cacheSession,
) (bool, error) {
	var request actionsFinalizeRequest
	if err := decodeActionsRequest(bytes.NewReader(body), &request); err != nil ||
		!validActionsCacheField(request.Key) || !validActionsCacheField(request.Version) {
		return false, nil
	}
	if err := lockCacheSession(ctx, session); err != nil {
		return false, err
	}
	defer session.mu.Unlock()
	if session.receipts[actionsReceiptID(request.Key, request.Version)] != nil {
		return true, nil
	}
	for _, archive := range session.actions {
		if archive.Mode == actionsModeUpload && archive.CacheKey == request.Key &&
			archive.Version == request.Version {
			return true, nil
		}
	}

	return false, nil
}

func (s *CacheService) actionsFinalizeReceipt(
	ctx context.Context,
	body []byte,
	session *cacheSession,
) (*http.Response, bool, error) {
	var request actionsFinalizeRequest
	if err := decodeActionsRequest(bytes.NewReader(body), &request); err != nil ||
		!validActionsCacheField(request.Key) || !validActionsCacheField(request.Version) {
		return nil, false, nil
	}
	if err := lockCacheSession(ctx, session); err != nil {
		return nil, true, err
	}
	defer session.mu.Unlock()
	receipt := session.receipts[actionsReceiptID(request.Key, request.Version)]
	if receipt == nil || !receipt.Complete {
		return nil, false, nil
	}
	size, err := parseActionsSize(request.SizeBytes)
	if err != nil || size != receipt.Size {
		return nil, true, errors.New("finalization does not match the local reservation receipt")
	}
	response, err := actionsReceiptResponse(receipt)

	return response, true, err
}

func parseActionsSize(raw json.RawMessage) (int64, error) {
	var encoded string
	if len(raw) > 0 && raw[0] == '"' {
		if err := json.Unmarshal(raw, &encoded); err != nil {
			return 0, err
		}
	} else {
		encoded = string(raw)
	}
	size, err := strconv.ParseInt(encoded, 10, 64)
	if err != nil || size < 0 || size > actionsArchiveLimit {
		return 0, errors.New("invalid Actions cache archive size")
	}

	return size, nil
}

func (s *CacheService) finalizeActionsCache(
	ctx context.Context,
	body io.Reader,
	session *cacheSession,
) (*http.Response, error) {
	var request actionsFinalizeRequest
	if err := decodeActionsRequest(body, &request); err != nil ||
		!validActionsCacheField(request.Key) || !validActionsCacheField(request.Version) {
		return nil, errors.New("invalid Actions cache finalization")
	}
	size, err := parseActionsSize(request.SizeBytes)
	if err != nil {
		return nil, err
	}

	if err := lockCacheSession(ctx, session); err != nil {
		return nil, err
	}
	if receipt := session.receipts[actionsReceiptID(request.Key, request.Version)]; receipt != nil {
		defer session.mu.Unlock()
		if receipt.Size != size {
			return nil, errors.New("finalization does not match the local reservation receipt")
		}

		return s.publishActionsReceipt(ctx, session, receipt)
	}
	var archive *actionsArchive
	for _, candidate := range session.actions {
		if candidate.Mode == actionsModeUpload && candidate.CacheKey == request.Key &&
			candidate.Version == request.Version {
			archive = candidate

			break
		}
	}
	if archive == nil || session.closed {
		session.mu.Unlock()

		return actionsJSONResponse(map[string]any{"ok": false})
	}
	if err := lockCacheMutex(ctx, &archive.mu); err != nil {
		session.mu.Unlock()

		return nil, err
	}
	defer archive.mu.Unlock()
	defer session.mu.Unlock()

	info, err := os.Stat(s.actionsArchivePath(session, archive))
	if err != nil || !info.Mode().IsRegular() || info.Size() != size {
		return nil, errors.New("uploaded Actions cache archive has the wrong size")
	}
	if err := os.RemoveAll(s.actionsStagingPath(session, archive)); err != nil {
		return nil, fmt.Errorf("remove staged Actions cache files: %w", err)
	}
	if err := s.actionIO.Trim(ctx, s.actionsMountPath(session, archive)); err != nil {
		return nil, fmt.Errorf("trim freed Actions cache blocks: %w", err)
	}
	lease, fence, err := s.store.AcquireWriter(ctx, archive.StoreKey,
		session.instance+"/actions/"+archive.ID, cacheWriterTTL)
	if err != nil {
		return nil, err
	}
	// Until the receipt below is durable, this frame is all that holds the
	// lease, so a failure gives it back rather than holding the key for the
	// lease's lifetime. The durable receipt then carries the lease for a
	// retried finalize, and nothing past it releases.
	abandon := func(err error) error {
		return errors.Join(err, s.releaseWriter(ctx, lease, fence))
	}
	if !archive.Unmounted {
		if err := s.actionIO.Unmount(ctx, s.actionsMountPath(session, archive)); err != nil {
			return nil, abandon(err)
		}
		archive.Unmounted = true
		if err := s.persistSession(session); err != nil {
			return nil, abandon(err)
		}
	}
	candidate, err := s.store.Snapshot(ctx, archive.Volume)
	if err != nil {
		return nil, abandon(err)
	}
	entryDigest := sha256.Sum256([]byte(archive.StoreKey + "\x00" + candidate.Generation))
	entryNumber := binary.BigEndian.Uint64(entryDigest[:8]) & math.MaxInt64
	if entryNumber == 0 {
		entryNumber = 1
	}
	receipt := &actionsReceipt{
		ID: actionsReceiptID(archive.CacheKey, archive.Version), CacheKey: archive.CacheKey,
		StoreKey: archive.StoreKey, Version: archive.Version, Candidate: candidate,
		Lease: lease, Fence: fence, EntryID: strconv.FormatUint(entryNumber, 10), Size: size,
	}
	delete(session.actions, archive.ID)
	session.receipts[receipt.ID] = receipt
	if err := s.persistSession(session); err != nil {
		return nil, abandon(err)
	}

	return s.publishActionsReceipt(ctx, session, receipt)
}

// publishActionsReceipt is called while session.mu is held. The receipt is
// durable before the pointer CAS and complete before success is returned.
func (s *CacheService) publishActionsReceipt(
	ctx context.Context,
	session *cacheSession,
	receipt *actionsReceipt,
) (*http.Response, error) {
	if receipt.Complete {
		return actionsReceiptResponse(receipt)
	}
	current, err := s.store.Current(ctx, receipt.StoreKey)
	if err == nil {
		return s.completeReceipt(ctx, session, receipt, current)
	}
	if !errors.Is(err, storecontract.ErrMiss) {
		return nil, err
	}
	if err := receipt.Lease.ValidAt(receipt.StoreKey, s.now()); err != nil {
		if err := s.reacquireReceiptWriter(ctx, session, receipt); err != nil {
			return nil, err
		}
	}
	for attempt := range 2 {
		err = s.store.PublishCAS(ctx, receipt.StoreKey, "", receipt.Candidate,
			receipt.Lease, receipt.Fence)
		if !errors.Is(err, storecontract.ErrConflict) {
			if err != nil {
				return nil, err
			}
			receipt.Complete = true
			receipt.OK = true
			if err := s.persistSession(session); err != nil {
				return nil, err
			}

			return actionsReceiptResponse(receipt)
		}
		current, currentErr := s.store.Current(ctx, receipt.StoreKey)
		if currentErr == nil {
			return s.completeReceipt(ctx, session, receipt, current)
		}
		if !errors.Is(currentErr, storecontract.ErrMiss) {
			return nil, currentErr
		}
		if err := s.reacquireReceiptWriter(ctx, session, receipt); err != nil {
			return nil, err
		}
		if attempt == 1 {
			return nil, storecontract.ErrConflict
		}
	}

	return nil, storecontract.ErrConflict
}

// completeReceipt records the pointer the store now shows as the receipt's
// verdict. The lease is given back first, because a receipt completed this way
// never uses it again and, left standing — a pointer written whose writer
// removal then failed — it holds the key until expiry. A lease the publication
// consumed makes the release a no-op. A release that fails does not withhold a
// verdict that is already known; the lease then ends on its own.
func (s *CacheService) completeReceipt(
	ctx context.Context,
	session *cacheSession,
	receipt *actionsReceipt,
	current string,
) (*http.Response, error) {
	if err := s.releaseWriter(ctx, receipt.Lease, receipt.Fence); err != nil {
		s.log.Warn("could not give back a completed Actions cache receipt's writer; it ends on its own",
			"instance", session.instance, "key", receipt.StoreKey, "error", err)
	}
	receipt.Complete = true
	receipt.OK = current == receipt.Candidate.Generation
	if err := s.persistSession(session); err != nil {
		return nil, err
	}

	return actionsReceiptResponse(receipt)
}

// reacquireReceiptWriter replaces a receipt's writer with a fresh one. The
// lease it holds is given back FIRST and unconditionally: the conflict that
// brought us here may be contention rather than supersession, in which case
// the receipt's own live lease would refuse the acquisition for the rest of
// its lifetime — and whether it is live is the store's question, not this
// clock's, since exact matching already makes a consumed, superseded or
// expired lease a no-op. A fresh lease counts only once the receipt carrying
// it is durable; one the receipt could not record is released again, or a
// crash would leave the store holding a writer nothing durable can publish
// with or give back.
func (s *CacheService) reacquireReceiptWriter(
	ctx context.Context,
	session *cacheSession,
	receipt *actionsReceipt,
) error {
	if err := s.releaseWriter(ctx, receipt.Lease, receipt.Fence); err != nil {
		return err
	}
	lease, fence, err := s.store.AcquireWriter(ctx, receipt.StoreKey,
		session.instance+"/actions/"+receipt.ID, cacheWriterTTL)
	if err != nil {
		return err
	}
	previous, previousFence := receipt.Lease, receipt.Fence
	receipt.Lease, receipt.Fence = lease, fence
	if err := s.persistSession(session); err != nil {
		receipt.Lease, receipt.Fence = previous, previousFence

		return errors.Join(err, s.releaseWriter(ctx, lease, fence))
	}

	return nil
}

func actionsReceiptResponse(receipt *actionsReceipt) (*http.Response, error) {
	if !receipt.OK {
		return actionsJSONResponse(map[string]any{"ok": false})
	}

	return actionsJSONResponse(map[string]any{"ok": true, "entry_id": receipt.EntryID})
}

func (s *CacheService) findActionsCache(
	ctx context.Context,
	body io.Reader,
	session *cacheSession,
	origin string,
) (*http.Response, error) {
	var request struct {
		Key         string   `json:"key"`
		RestoreKeys []string `json:"restore_keys"`
		Version     string   `json:"version"`
	}
	if err := decodeActionsRequest(body, &request); err != nil ||
		!validActionsCacheField(request.Key) || !validActionsCacheField(request.Version) ||
		len(request.RestoreKeys) > 9 {
		return nil, errors.New("invalid Actions cache lookup")
	}
	for _, key := range request.RestoreKeys {
		if !validActionsCacheField(key) {
			return nil, errors.New("invalid Actions cache restore key")
		}
	}
	if err := lockCacheSession(ctx, session); err != nil {
		return nil, err
	}
	defer session.mu.Unlock()
	if session.closed {
		return nil, errors.New("cache session has ended")
	}
	if len(session.actions)+len(session.receipts) >= actionsArchiveCount {
		return nil, errors.New("this job has too many active Actions cache archives")
	}
	matcher, ok := s.store.(storecontract.KeyMatcher)
	if !ok {
		return nil, errors.New("site cache store cannot match restore keys")
	}
	prefix := s.qualifiedKey(actionsVersionPrefix(session, request.Version))
	restore := make([]string, len(request.RestoreKeys))
	for index, key := range request.RestoreKeys {
		restore[index] = prefix + key
	}
	matched, generation, err := matcher.Match(ctx, prefix+request.Key, restore)
	if errors.Is(err, storecontract.ErrMiss) {
		return actionsJSONResponse(map[string]any{"ok": false})
	}
	if err != nil {
		return nil, err
	}
	matchedKey, ok := strings.CutPrefix(matched, prefix)
	if !ok || !validActionsCacheField(matchedKey) {
		return nil, errors.New("site cache returned a key outside the Actions namespace")
	}
	volume, err := s.store.Clone(ctx, matched, generation)
	if err != nil {
		return nil, err
	}
	id, err := randomCacheToken()
	if err != nil {
		return nil, errors.Join(err, s.store.Discard(ctx, volume))
	}
	signature, err := randomCacheToken()
	if err != nil {
		return nil, errors.Join(err, s.store.Discard(ctx, volume))
	}
	archive := &actionsArchive{
		ID: id, Signature: signature, Mode: actionsModeDownload, CacheKey: matchedKey,
		StoreKey: matched, Version: request.Version, Volume: volume,
	}
	mountPath := s.actionsMountPath(session, archive)
	if err := s.actionIO.MountReadOnly(ctx, volume.Device, mountPath); err != nil {
		return nil, errors.Join(err, s.store.Discard(ctx, volume))
	}
	if info, err := os.Stat(s.actionsArchivePath(session, archive)); err != nil ||
		!info.Mode().IsRegular() || info.Size() > actionsArchiveLimit {
		return nil, errors.Join(errors.New("published Actions cache has no usable archive"),
			s.actionIO.Unmount(ctx, mountPath), s.store.Discard(ctx, volume))
	}

	session.actions[id] = archive
	if err := s.persistSession(session); err != nil {
		delete(session.actions, id)

		return nil, errors.Join(err, s.actionIO.Unmount(ctx, mountPath), s.store.Discard(ctx, volume))
	}

	return actionsJSONResponse(map[string]any{
		"ok": true, "signed_download_url": s.actionsSignedURL(archive, origin), "matched_key": matchedKey,
	})
}

func actionsJSONResponse(value any) (*http.Response, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encode Actions cache response: %w", err)
	}

	return &http.Response{
		StatusCode: http.StatusOK, ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{
			"Content-Type":     []string{"application/json"},
			"Content-Length":   []string{strconv.Itoa(len(body))},
			actionsLocalHeader: []string{"local"},
		},
		Body: io.NopCloser(bytes.NewReader(body)),
	}, nil
}

func (s *CacheService) serveActionsBlob(
	req *http.Request,
	session *cacheSession,
) (*http.Response, error) {
	id := strings.TrimPrefix(req.URL.Path, actionsBlobPrefix)
	if id == "" || strings.Contains(id, "/") {
		return actionsBlobError(http.StatusNotFound, "cache archive not found"), nil
	}
	if err := lockCacheSession(req.Context(), session); err != nil {
		return nil, err
	}
	archive := session.actions[id]
	if session.closed || archive == nil || req.URL.Query().Get("sig") != archive.Signature {
		session.mu.Unlock()

		return actionsBlobError(http.StatusForbidden, "cache archive is unavailable"), nil
	}
	if err := lockCacheMutex(req.Context(), &archive.mu); err != nil {
		session.mu.Unlock()

		return nil, err
	}
	session.mu.Unlock()
	defer archive.mu.Unlock()
	if archive.Mode == actionsModeUpload {
		return s.uploadActionsBlob(req, session, archive)
	}

	return s.downloadActionsBlob(req, session, archive)
}

func (s *CacheService) uploadActionsBlob(
	req *http.Request,
	session *cacheSession,
	archive *actionsArchive,
) (*http.Response, error) {
	if req.Method == http.MethodHead {
		return actionsBlobResponse(http.StatusOK), nil
	}
	if req.Method != http.MethodPut || archive.Unmounted {
		return actionsBlobError(http.StatusMethodNotAllowed, "upload method is unavailable"), nil
	}

	switch req.URL.Query().Get("comp") {
	case "":
		if !strings.EqualFold(req.Header.Get("X-Ms-Blob-Type"), "BlockBlob") {
			return actionsBlobError(http.StatusBadRequest, "blob type must be BlockBlob"), nil
		}
		if err := writeActionsFile(req.Context(), s.actionsArchivePath(session, archive),
			s.actionsStagingPath(session, archive), req.Body, actionsArchiveLimit); err != nil {
			return nil, err
		}
	case "block":
		blockID := req.URL.Query().Get("blockid")
		if blockID == "" || len(blockID) > 1024 {
			return actionsBlobError(http.StatusBadRequest, "block id is invalid"), nil
		}
		if err := writeActionsFile(req.Context(), s.actionsBlockPath(session, archive, blockID),
			s.actionsStagingPath(session, archive), req.Body, actionsArchiveLimit); err != nil {
			return nil, err
		}
	case "blocklist":
		if err := s.commitActionsBlockList(req.Context(), req.Body, session, archive); err != nil {
			return nil, err
		}
	default:
		return actionsBlobError(http.StatusBadRequest, "blob operation is unsupported"), nil
	}

	response := actionsBlobResponse(http.StatusCreated)
	response.Header.Set("ETag", `"billet-`+archive.ID+`"`)
	response.Header.Set("X-Ms-Request-Id", archive.ID)

	return response, nil
}

func writeActionsFile(
	ctx context.Context,
	path string,
	temporaryDirectory string,
	body io.Reader,
	limit int64,
) error {
	temporary, err := os.CreateTemp(temporaryDirectory, ".upload-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, copyErr := copyActionsData(ctx, temporary, body, limit)
	if copyErr == nil && written > limit {
		copyErr = errors.New("actions cache archive exceeds the site limit")
	}
	workErr := finishActionsFile(ctx, temporary, copyErr)
	if workErr != nil {
		return workErr
	}

	return os.Rename(temporaryPath, path)
}

type actionsUploadFile interface {
	Sync() error
	Close() error
}

func finishActionsFile(ctx context.Context, file actionsUploadFile, copyErr error) error {
	if copyErr != nil {
		return errors.Join(copyErr, file.Close())
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(err, file.Close())
	}

	return errors.Join(file.Sync(), file.Close())
}

func copyActionsData(
	ctx context.Context,
	destination io.Writer,
	source io.Reader,
	limit int64,
) (int64, error) {
	reader := io.LimitReader(source, limit+1)
	buffer := make([]byte, 1<<20)
	var written int64
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}
		read, readErr := reader.Read(buffer)
		if read > 0 {
			select {
			case <-ctx.Done():
				return written, ctx.Err()
			default:
			}
			stored, writeErr := destination.Write(buffer[:read])
			written += int64(stored)
			if writeErr != nil {
				return written, writeErr
			}
			if stored != read {
				return written, io.ErrShortWrite
			}
		}
		if errors.Is(readErr, io.EOF) {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

func (s *CacheService) commitActionsBlockList(
	ctx context.Context,
	body io.Reader,
	session *cacheSession,
	archive *actionsArchive,
) error {
	blocks, err := decodeActionsBlockList(body)
	if err != nil {
		return err
	}
	if len(blocks) == 0 || len(blocks) > 10000 {
		return errors.New("azure block list is empty or too large")
	}
	target := s.actionsArchivePath(session, archive)
	temporary, err := os.CreateTemp(s.actionsStagingPath(session, archive), ".blocks-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	var total int64
	for _, blockID := range blocks {
		if blockID == "" || len(blockID) > 1024 {
			temporary.Close()

			return errors.New("azure block list contains an invalid id")
		}
		block, err := os.Open(s.actionsBlockPath(session, archive, blockID))
		if err != nil {
			temporary.Close()

			return err
		}
		written, copyErr := copyActionsData(ctx, temporary, block, actionsArchiveLimit-total)
		closeErr := block.Close()
		total += written
		if err := errors.Join(copyErr, closeErr); err != nil {
			temporary.Close()

			return err
		}
		if total > actionsArchiveLimit {
			temporary.Close()

			return errors.New("actions cache archive exceeds the site limit")
		}
	}
	if err := errors.Join(temporary.Sync(), temporary.Close()); err != nil {
		return err
	}

	return os.Rename(temporaryPath, target)
}

func decodeActionsBlockList(body io.Reader) ([]string, error) {
	raw, err := io.ReadAll(io.LimitReader(body, actionsRequestLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > actionsRequestLimit {
		return nil, errors.New("azure block list exceeds 64KiB")
	}
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	var blocks []string
	root := false
	closed := false
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("decode Azure block list: %w", err)
		}
		switch value := token.(type) {
		case xml.StartElement:
			if !root {
				if value.Name.Space != "" || value.Name.Local != "BlockList" || len(value.Attr) != 0 {
					return nil, errors.New("azure block list has an invalid root")
				}
				root = true

				continue
			}
			if closed || value.Name.Space != "" || len(value.Attr) != 0 ||
				value.Name.Local != "Latest" && value.Name.Local != "Committed" &&
					value.Name.Local != "Uncommitted" {
				return nil, errors.New("azure block list contains an unsupported element")
			}
			var blockID string
			if err := decoder.DecodeElement(&blockID, &value); err != nil {
				return nil, fmt.Errorf("decode Azure block id: %w", err)
			}
			blocks = append(blocks, blockID)
		case xml.EndElement:
			if !root || closed || value.Name.Space != "" || value.Name.Local != "BlockList" {
				return nil, errors.New("azure block list has invalid nesting")
			}
			closed = true
		case xml.CharData:
			if strings.TrimSpace(string(value)) != "" {
				return nil, errors.New("azure block list contains text outside an id")
			}
		}
	}
	if !root || !closed {
		return nil, errors.New("azure block list is incomplete")
	}

	return blocks, nil
}

func (s *CacheService) downloadActionsBlob(
	req *http.Request,
	session *cacheSession,
	archive *actionsArchive,
) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return actionsBlobError(http.StatusMethodNotAllowed, "download method is unavailable"), nil
	}
	file, err := os.Open(s.actionsArchivePath(session, archive))
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()

		return nil, err
	}
	rangeHeader := req.Header.Get("Range")
	azureRange := req.Header.Get("X-Ms-Range")
	if rangeHeader == "" {
		rangeHeader = azureRange
	} else if azureRange != "" && azureRange != rangeHeader {
		file.Close()
		response := actionsBlobError(http.StatusRequestedRangeNotSatisfiable,
			"range headers conflict")
		response.Header.Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size(), 10))

		return response, nil
	}
	start, length, partial, rangeResponse := actionsResponseByteRange(rangeHeader, info.Size())
	if rangeResponse != nil {
		file.Close()

		return rangeResponse, nil
	}
	status := http.StatusOK
	if partial {
		status = http.StatusPartialContent
	}
	response := actionsBlobResponse(status)
	response.Header.Set("Accept-Ranges", "bytes")
	response.Header.Set("Content-Type", "application/octet-stream")
	response.Header.Set("Content-Length", strconv.FormatInt(length, 10))
	response.Header.Set("ETag", `"billet-`+archive.ID+`"`)
	response.Header.Set("Last-Modified", info.ModTime().UTC().Format(http.TimeFormat))
	if partial {
		response.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d",
			start, start+length-1, info.Size()))
	}
	if req.Method == http.MethodHead {
		file.Close()

		return response, nil
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		file.Close()

		return nil, err
	}
	response.Body = &limitedFile{Reader: io.LimitReader(file, length), file: file}

	return response, nil
}

type limitedFile struct {
	io.Reader
	file *os.File
}

func (f *limitedFile) Close() error { return f.file.Close() }

func actionsResponseByteRange(value string, size int64) (int64, int64, bool, *http.Response) {
	start, length, partial, err := actionsByteRange(value, size)
	if err == nil {
		return start, length, partial, nil
	}
	response := actionsBlobError(http.StatusRequestedRangeNotSatisfiable, "range is invalid")
	response.Header.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))

	return 0, 0, false, response
}

func actionsByteRange(value string, size int64) (int64, int64, bool, error) {
	if value == "" {
		return 0, size, false, nil
	}
	if size == 0 {
		return 0, 0, false, errors.New("an empty archive has no satisfiable byte range")
	}
	raw, ok := strings.CutPrefix(value, "bytes=")
	if !ok || strings.Contains(raw, ",") {
		return 0, 0, false, errors.New("unsupported range")
	}
	first, last, ok := strings.Cut(raw, "-")
	if !ok {
		return 0, 0, false, errors.New("invalid range")
	}
	if first == "" {
		suffix, err := strconv.ParseInt(last, 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false, errors.New("invalid suffix range")
		}
		if suffix > size {
			suffix = size
		}

		return size - suffix, suffix, true, nil
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false, errors.New("invalid range start")
	}
	end := size - 1
	if last != "" {
		end, err = strconv.ParseInt(last, 10, 64)
		if err != nil || end < start {
			return 0, 0, false, errors.New("invalid range end")
		}
		if end >= size {
			end = size - 1
		}
	}

	return start, end - start + 1, true, nil
}

func actionsBlobResponse(status int) *http.Response {
	return &http.Response{
		StatusCode: status, ProtoMajor: 1, ProtoMinor: 1,
		Header: http.Header{actionsLocalHeader: []string{"local"}}, Body: http.NoBody,
	}
}

func actionsBlobError(status int, message string) *http.Response {
	body := []byte(message + "\n")
	response := actionsBlobResponse(status)
	response.Header.Set("Content-Type", "text/plain; charset=utf-8")
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	response.Body = io.NopCloser(bytes.NewReader(body))

	return response
}
