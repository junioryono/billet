package node

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
)

const (
	// npmHeaderWait is how long one attempt at the registry has from the start
	// of its connection to its response headers, DNS, connect and TLS included.
	npmHeaderWait = 8 * time.Second
	// npmBodyIdle ends an attempt whose body stops moving for this long.
	npmBodyIdle = 10 * time.Second
	// npmDocumentLimit bounds a document the node reads whole before answering.
	npmDocumentLimit = 512 << 20
	// npmErrorBodyLimit bounds the body of an answer the node relays without
	// storing it and without streaming it, such as an error.
	npmErrorBodyLimit = 1 << 20
)

// npmRelayedHeaders are the registry's headers a client is given. Everything
// else, Set-Cookie first, stays with the node.
var npmRelayedHeaders = []string{"Content-Type", "ETag", "Last-Modified", "Cache-Control", "Age",
	"Date", "Expires", "Location", "Retry-After", "WWW-Authenticate", "Npm-Notice"}

// npmCache serves registry.npmjs.org to the guests whose tier enables it
// (ADR-020), and splices everything it may not answer to the registry.
type npmCache struct {
	service *CacheService
	leaf    tls.Certificate
	// store is nil when it could not be opened, and then every connection is
	// spliced: the guest reaches the registry as it would without the cache.
	store *npmStore
	// origin is the registry's https origin, a seam for tests.
	origin string
	// pooled makes first attempts; fresh makes a retry, always on a new
	// connection.
	pooled, fresh *http.Client
	// dialUpstream opens the raw TLS connection a splice copies onto.
	dialUpstream func(ctx context.Context) (net.Conn, error)
	now          func() time.Time
	headerWait   time.Duration
	bodyIdle     time.Duration

	mu    sync.Mutex
	fills map[string]chan struct{}
}

func newNpmCache(service *CacheService, leaf tls.Certificate, store *npmStore) *npmCache {
	return &npmCache{
		service: service, leaf: leaf, store: store,
		origin: "https://" + npmRegistryHost,
		pooled: newNpmClient(false), fresh: newNpmClient(true),
		dialUpstream: dialNpmUpstream, now: time.Now,
		headerWait: npmHeaderWait, bodyIdle: npmBodyIdle,
		fills: make(map[string]chan struct{}),
	}
}

// newNpmClient is the node's own client of the registry: HTTP/1.1 (a stalled
// transfer then holds back only its own connection), no proxy (a fill must
// never re-enter this listener), no redirect followed, and no transparent
// decoding, because the node decides what it decodes.
func newNpmClient(oneShot bool) *http.Client {
	dialer := &net.Dialer{Timeout: npmHeaderWait, KeepAlive: 30 * time.Second}

	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}},
			TLSHandshakeTimeout: npmHeaderWait,
			TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
			DisableCompression:  true,
			DisableKeepAlives:   oneShot,
			MaxIdleConnsPerHost: 16,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// dialNpmUpstream opens the TLS connection a splice copies onto, pinned to
// HTTP/1.1 for the reason dialActionsUpstream gives.
func dialNpmUpstream(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer

	raw, err := dialer.DialContext(ctx, "tcp", npmRegistryHost+":443")
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, &tls.Config{
		ServerName: npmRegistryHost, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"},
	})
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()

		return nil, err
	}

	return conn, nil
}

func (n *npmCache) held(key string) bool { return n.store != nil && n.store.held(key) }

// intercept serves one tunnelled connection from a guest whose session enables
// the npm cache.
//
// THE FIRST REQUEST DECIDES THE CONNECTION. One the store may answer is answered
// and the connection closed, so every later request arrives on a new connection
// and is classified afresh; anything else is spliced to the registry, whole,
// with the guest's own bytes.
func (n *npmCache) intercept(ctx context.Context, w http.ResponseWriter, session *cacheSession) {
	client, tlsConn, ok := openTunnel(ctx, w, n.leaf)
	if !ok {
		return
	}
	defer client.Close()

	capture := &actionsCaptureReader{
		reader: &actionsHeaderReader{reader: tlsConn, remaining: actionsHeaderLimit},
		limit:  actionsHeaderLimit + 4096,
	}
	reader := bufio.NewReader(capture)
	if err := tlsConn.SetReadDeadline(time.Now().Add(actionsConnectWait)); err != nil {
		return
	}
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if !npmRequestTargetsRegistry(req) {
		refuseMisdirected(tlsConn, "only the npm registry is available")

		return
	}

	request, cacheable := classifyNpmRequest(req, n.held)
	switch {
	case !cacheable:
		n.service.log.Debug("an npm request was passed to the registry", "instance", session.instance,
			"reason", "not an anonymous read the store answers")
	case n.store == nil:
		cacheable = false
		n.note(session, alloc.BuildCacheUnavailable)
	case !n.service.sessionKindAllowed(ctx, session, config.CacheNpm):
		cacheable = false
		n.note(session, alloc.BuildCacheDisabled)
	}
	if !cacheable {
		spliceTo(ctx, n.service.log, tlsConn, capture, n.dialUpstream, "the npm registry")

		return
	}

	if err := tlsConn.SetDeadline(time.Now().Add(cacheHandlerLimit)); err != nil {
		return
	}
	requestCtx, cancel := context.WithTimeout(ctx, cacheHandlerLimit)
	defer cancel()
	n.serve(requestCtx, tlsConn, req.WithContext(requestCtx), request, session)
}

// npmRequestTargetsRegistry reports whether the inner request names the
// registry in origin form, for the reason actionsRequestTargetsResultsHost
// gives.
func npmRequestTargetsRegistry(req *http.Request) bool {
	if req.URL == nil || req.URL.IsAbs() || req.URL.Host != "" {
		return false
	}
	host := req.Host
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port != "443" {
			return false
		}
		host = h
	}

	return strings.EqualFold(host, npmRegistryHost)
}

func refuseMisdirected(conn net.Conn, message string) {
	misdirected := actionsBlobError(http.StatusMisdirectedRequest, message)
	misdirected.Close = true
	misdirected.Proto, misdirected.ProtoMajor, misdirected.ProtoMinor = "HTTP/1.1", 1, 1
	//nolint:errcheck // best-effort refusal before anything is forwarded
	_ = misdirected.Write(conn)
	_ = misdirected.Body.Close()
}

// note records what the cache did for the session's job.
func (n *npmCache) note(session *cacheSession, outcome alloc.BuildCache) {
	session.mu.Lock()
	noteBuildCache(session, config.CacheNpm, outcome)
	session.mu.Unlock()
}

// serve answers one request the store may answer.
func (n *npmCache) serve(ctx context.Context, conn net.Conn, req *http.Request, request npmRequest,
	session *cacheSession,
) {
	started := n.now()
	key := request.key()
	if entry, ok := n.store.lookup(key); ok && entry.fresh(n.now()) {
		n.answerEntry(conn, req, request, entry, "warm", started, session)

		return
	}
	// ONE FILL PER ENTRY AT A TIME: a second job asking for what is being filled
	// waits for it, and is then answered from the store.
	release, ok := n.lockFill(ctx, key)
	if !ok {
		return
	}
	defer release()
	entry, held := n.store.lookup(key)
	if held && entry.fresh(n.now()) {
		n.answerEntry(conn, req, request, entry, "warm", started, session)

		return
	}
	validator := ""
	if held {
		validator = entry.ETag
	}
	fetch, answered := n.fetch(ctx, request, validator)
	if !answered {
		n.answerNoAnswer(conn, req, request, entry, held, fetch, started, session)

		return
	}
	defer fetch.close()
	status := fetch.resp.StatusCode
	switch {
	case status == http.StatusNotModified && held && validator != "":
		n.answerRevalidated(ctx, conn, req, request, entry, fetch, started, session)
	case status == http.StatusOK && request.object != npmTarball:
		n.answerDocument(conn, req, request, fetch, started, session)
	case status == http.StatusOK:
		n.answerTarball(conn, req, request, fetch, started, session)
	default:
		if npmDefinitive(status) {
			n.store.remove(key)
		}
		n.relay(conn, req, request, fetch, started, session)
	}
}

// npmDefinitive reports whether a status says the registry has nothing to give
// for the name, so the store must stop answering for it.
func npmDefinitive(status int) bool {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	}

	return false
}

func (n *npmCache) lockFill(ctx context.Context, key string) (func(), bool) {
	for {
		n.mu.Lock()
		busy, ok := n.fills[key]
		if !ok {
			done := make(chan struct{})
			n.fills[key] = done
			n.mu.Unlock()

			return func() {
				n.mu.Lock()
				delete(n.fills, key)
				n.mu.Unlock()
				close(done)
			}, true
		}
		n.mu.Unlock()
		select {
		case <-busy:
		case <-ctx.Done():
			return nil, false
		}
	}
}

// npmFetch is one decided answer from the registry. A document's 200 has been
// read whole into document; every other answer's body is still the
// response's.
type npmFetch struct {
	resp      *http.Response
	cancel    context.CancelFunc
	requested time.Time
	received  time.Time
	attempts  int
	document  string
	size      int64
}

func (f npmFetch) close() {
	if f.resp != nil {
		_ = f.resp.Body.Close()
	}
	if f.cancel != nil {
		f.cancel()
	}
	if f.document != "" {
		_ = os.Remove(f.document)
	}
}

// fetch asks the registry, in the order ADR-020 gives: an attempt, one retry on
// a new connection while nothing has reached the client, and otherwise no
// answer. answered is false when neither attempt answered; fetch.resp is then
// the second attempt's 5xx, if it gave one, for the caller to relay when no
// stale entry may answer instead.
func (n *npmCache) fetch(ctx context.Context, request npmRequest, validator string) (npmFetch, bool) {
	var last npmFetch
	for attempt := 1; attempt <= 2; attempt++ {
		client := n.pooled
		if attempt == 2 {
			client = n.fresh
		}
		requested := n.now()
		resp, cancel, err := n.attempt(ctx, client, request, validator)
		if err != nil {
			n.service.log.Info("an npm registry attempt gave no answer", "path", request.path,
				"attempt", attempt, "error", err)

			continue
		}
		f := npmFetch{resp: resp, cancel: cancel, requested: requested, received: n.now(), attempts: attempt}
		switch {
		case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusServiceUnavailable ||
			resp.StatusCode == http.StatusGatewayTimeout:
			n.service.log.Info("the npm registry answered with a retryable status", "path", request.path,
				"attempt", attempt, "status", resp.StatusCode)
			if attempt == 1 {
				f.close()

				continue
			}
			last = f
		case resp.StatusCode == http.StatusOK && request.object != npmTarball:
			document, size, err := n.readDocument(f)
			cancel()
			if err != nil {
				_ = resp.Body.Close()
				n.service.log.Info("an npm registry document stalled before it was answered",
					"path", request.path, "attempt", attempt, "error", err)

				continue
			}
			f.document, f.size = document, size

			return f, true
		default:
			return f, true
		}
	}

	return last, false
}

// attempt makes one request, bounding the wait for its headers and then the
// gaps in its body.
func (n *npmCache) attempt(ctx context.Context, client *http.Client, request npmRequest, validator string,
) (*http.Response, context.CancelFunc, error) {
	attemptCtx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(attemptCtx, http.MethodGet, n.origin+request.path, http.NoBody)
	if err != nil {
		cancel()

		return nil, nil, err
	}
	switch request.variant {
	case npmAbbreviated:
		req.Header.Set("Accept", npmAbbreviatedAccept)
		req.Header.Set("Accept-Encoding", "gzip")
	case npmFull:
		req.Header.Set("Accept", npmFullAccept)
		req.Header.Set("Accept-Encoding", "gzip")
	default:
		req.Header.Set("Accept-Encoding", "identity")
	}
	req.Header.Set("User-Agent", "billet-npm-cache")
	if validator != "" {
		req.Header.Set("If-None-Match", validator)
	}
	headers := time.AfterFunc(n.headerWait, cancel)
	resp, err := client.Do(req)
	if !headers.Stop() {
		if err == nil {
			_ = resp.Body.Close()
		}
		cancel()

		return nil, nil, fmt.Errorf("no response headers within %s", n.headerWait)
	}
	if err != nil {
		cancel()

		return nil, nil, err
	}
	resp.Body = &npmIdleBody{body: resp.Body, timer: time.AfterFunc(n.bodyIdle, cancel), idle: n.bodyIdle}

	return resp, cancel, nil
}

// npmIdleBody ends its request when no byte has arrived for idle.
type npmIdleBody struct {
	body  io.ReadCloser
	timer *time.Timer
	idle  time.Duration
}

func (b *npmIdleBody) Read(p []byte) (int, error) {
	count, err := b.body.Read(p)
	if count > 0 {
		b.timer.Reset(b.idle)
	}

	return count, err
}

func (b *npmIdleBody) Close() error {
	b.timer.Stop()

	return b.body.Close()
}

// readDocument reads a document's 200 whole into a temporary file, decoded.
func (n *npmCache) readDocument(f npmFetch) (string, int64, error) {
	body := io.Reader(f.resp.Body)
	switch strings.ToLower(strings.TrimSpace(f.resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		decoded, err := gzip.NewReader(f.resp.Body)
		if err != nil {
			return "", 0, err
		}
		defer decoded.Close()
		body = decoded
	default:
		return "", 0, fmt.Errorf("an encoding the node did not ask for: %q", f.resp.Header.Get("Content-Encoding"))
	}
	temp, err := n.store.temp()
	if err != nil {
		return "", 0, err
	}
	size, err := io.Copy(temp, io.LimitReader(body, npmDocumentLimit+1))
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil && size > npmDocumentLimit {
		err = errors.New("the document is larger than the node reads whole")
	}
	if err == nil && f.resp.Header.Get("Content-Encoding") == "" && f.resp.ContentLength >= 0 &&
		size != f.resp.ContentLength {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		_ = os.Remove(temp.Name())

		return "", 0, err
	}

	return temp.Name(), size, nil
}

// entryFor is the store entry a 200 becomes.
func entryFor(request npmRequest, f npmFetch, size int64) npmEntry {
	entry := npmEntry{
		Key: request.key(), Path: request.path, Object: request.object, Variant: request.variant,
		ContentType: f.resp.Header.Get("Content-Type"), ETag: f.resp.Header.Get("ETag"),
		LastModified: f.resp.Header.Get("Last-Modified"), Size: size,
	}
	npmFreshness(&entry, f.resp.Header, f.requested, f.received)

	return entry
}

func (n *npmCache) answerDocument(conn net.Conn, req *http.Request, request npmRequest, f npmFetch,
	started time.Time, session *cacheSession,
) {
	if !npmShareable(f.resp.Header) {
		n.relayBody(conn, req, f, nil, started, request, "unshareable", session)

		return
	}
	// OPENED BEFORE THE COMMIT, which consumes the file whether or not it
	// succeeds: a store that refuses the document still answers the client.
	document, err := os.Open(f.document)
	if err != nil {
		n.answerStatus(conn, req, http.StatusBadGateway)
		n.note(session, alloc.BuildCacheUnavailable)

		return
	}
	defer document.Close()
	entry := entryFor(request, f, f.size)
	if err := n.store.commit(entry, f.document); err == nil {
		if stored, ok := n.store.lookup(entry.Key); ok && stored.Body != "" {
			n.answerEntry(conn, req, request, stored, "filled", started, session)

			return
		}
	} else {
		n.service.log.Warn("the npm store could not keep a document", "path", request.path, "error", err)
	}
	n.relayBody(conn, req, f, document, started, request, "filled-unstored", session)
}

// answerTarball streams a tarball's 200 to the client as it arrives and keeps
// it when it arrives whole and may be shared.
func (n *npmCache) answerTarball(conn net.Conn, req *http.Request, request npmRequest, f npmFetch,
	started time.Time, session *cacheSession,
) {
	keep := npmShareable(f.resp.Header) && f.resp.Header.Get("Content-Encoding") == ""
	var temp *os.File
	if keep {
		var err error
		if temp, err = n.store.temp(); err != nil {
			keep = false
		}
	}
	counted := &npmCountingReader{reader: f.resp.Body}
	var body io.Reader = counted
	if keep {
		body = io.TeeReader(counted, temp)
	}
	if req.Method == http.MethodHead {
		// A HEAD IS ANSWERED FROM A COMPLETE BODY: read it whole, keep it, and
		// answer from the store.
		_, err := io.Copy(io.Discard, body)
		if keep {
			n.keepTarball(request, f, temp, counted, err)
		}
		if stored, ok := n.store.lookup(request.key()); ok && err == nil {
			n.answerEntry(conn, req, request, stored, "filled", started, session)

			return
		}
		n.answerStatus(conn, req, http.StatusBadGateway)

		return
	}
	header := relayedHeader(f.resp.Header)
	err := n.writeAnswer(conn, req, http.StatusOK, header, body, f.resp.ContentLength)
	if keep {
		n.keepTarball(request, f, temp, counted, err)
	}
	outcome := alloc.BuildCacheCold
	if err != nil {
		outcome = alloc.BuildCacheUnavailable
	}
	n.note(session, outcome)
	n.logAnswer(request, "filled", http.StatusOK, counted.count, started, f.attempts)
}

func (n *npmCache) keepTarball(request npmRequest, f npmFetch, temp *os.File, counted *npmCountingReader,
	readErr error,
) {
	closeErr := temp.Close()
	complete := readErr == nil && closeErr == nil && counted.eof &&
		(f.resp.ContentLength < 0 || counted.count == f.resp.ContentLength)
	if !complete {
		_ = os.Remove(temp.Name())

		return
	}
	if err := n.store.commit(entryFor(request, f, counted.count), temp.Name()); err != nil {
		n.service.log.Warn("the npm store could not keep a tarball", "path", request.path, "error", err)
	}
}

// npmCountingReader counts what it read and whether it reached the end.
type npmCountingReader struct {
	reader io.Reader
	count  int64
	eof    bool
}

func (r *npmCountingReader) Read(p []byte) (int, error) {
	count, err := r.reader.Read(p)
	r.count += int64(count)
	if errors.Is(err, io.EOF) {
		r.eof = true
	}

	return count, err
}

// answerRevalidated handles a 304 to the node's own validator.
func (n *npmCache) answerRevalidated(ctx context.Context, conn net.Conn, req *http.Request,
	request npmRequest, entry npmEntry, f npmFetch, started time.Time, session *cacheSession,
) {
	merged := entry.headers()
	for _, name := range []string{"Cache-Control", "ETag", "Last-Modified", "Date", "Age", "Expires"} {
		if values := f.resp.Header.Values(name); len(values) > 0 {
			merged[name] = values
		}
	}
	if values := f.resp.Header.Values("Set-Cookie"); len(values) > 0 {
		merged["Set-Cookie"] = values
	}
	if !npmShareable(merged) {
		// THE VALIDATOR WAS THE NODE'S, so this 304 is never the client's answer:
		// ask again without one and relay that, unstored.
		n.store.remove(entry.Key)
		f.close()
		again, answered := n.fetch(ctx, request, "")
		defer again.close()
		if !answered && again.resp == nil {
			n.answerStatus(conn, req, http.StatusGatewayTimeout)
			n.note(session, alloc.BuildCacheUnavailable)
			n.logAnswer(request, "failed", http.StatusGatewayTimeout, 0, started, again.attempts)

			return
		}
		n.relayBody(conn, req, again, nil, started, request, "unshareable", session)

		return
	}
	refreshed := entry
	refreshed.ETag = merged.Get("ETag")
	refreshed.LastModified = merged.Get("Last-Modified")
	npmFreshness(&refreshed, merged, f.requested, f.received)
	if err := n.store.refresh(refreshed); err != nil {
		n.service.log.Warn("the npm store could not keep a revalidation", "path", request.path, "error", err)
	}
	n.answerEntry(conn, req, request, refreshed, "revalidated", started, session)
}

// answerNoAnswer is the end of a fetch neither attempt answered.
func (n *npmCache) answerNoAnswer(conn net.Conn, req *http.Request, request npmRequest, entry npmEntry,
	held bool, f npmFetch, started time.Time, session *cacheSession,
) {
	defer f.close()
	if held && entry.staleUsable(n.now()) {
		n.service.log.Warn("the npm registry gave no answer; serving a stale entry", "path", request.path,
			"instance", session.instance, "expired", entry.expiry())
		n.answerEntry(conn, req, request, entry, "stale", started, session)

		return
	}
	n.note(session, alloc.BuildCacheUnavailable)
	if f.resp != nil {
		n.relayBody(conn, req, f, nil, started, request, "failed", session)

		return
	}
	n.answerStatus(conn, req, http.StatusGatewayTimeout)
	n.logAnswer(request, "failed", http.StatusGatewayTimeout, 0, started, 2)
}

// headers is what a stored entry tells a client of itself.
func (e npmEntry) headers() http.Header {
	header := http.Header{}
	for name, value := range map[string]string{
		"Content-Type": e.ContentType, "ETag": e.ETag, "Last-Modified": e.LastModified,
		"Cache-Control": e.CacheControl,
	} {
		if value != "" {
			header.Set(name, value)
		}
	}

	return header
}

// answerEntry answers from a stored entry, with a 304 when the client's own
// validator names it.
func (n *npmCache) answerEntry(conn net.Conn, req *http.Request, request npmRequest, entry npmEntry,
	how string, started time.Time, session *cacheSession,
) {
	now := n.now()
	header := entry.headers()
	header.Set("Age", strconv.FormatInt(int64(entry.age(now)/time.Second), 10))
	header.Set("Date", now.UTC().Format(http.TimeFormat))
	outcome := alloc.BuildCacheWarm
	if how == "filled" {
		outcome = alloc.BuildCacheCold
	}
	if request.ifNoneMatch != "" && npmETagMatches(request.ifNoneMatch, entry.ETag) {
		err := n.writeAnswer(conn, req, http.StatusNotModified, header, nil, -1)
		n.finishAnswer(session, outcome, err)
		n.logAnswer(request, how, http.StatusNotModified, 0, started, 0)

		return
	}
	body, err := n.store.open(entry, now)
	if err != nil {
		n.service.log.Warn("an npm store entry could not be read", "path", request.path, "error", err)
		n.store.remove(entry.Key)
		n.answerStatus(conn, req, http.StatusBadGateway)
		n.note(session, alloc.BuildCacheUnavailable)

		return
	}
	defer body.Close()
	err = n.writeAnswer(conn, req, http.StatusOK, header, body, entry.Size)
	n.finishAnswer(session, outcome, err)
	n.logAnswer(request, how, http.StatusOK, entry.Size, started, 0)
}

func (n *npmCache) finishAnswer(session *cacheSession, outcome alloc.BuildCache, err error) {
	if err != nil {
		outcome = alloc.BuildCacheUnavailable
	}
	n.note(session, outcome)
}

// relay answers with the registry's own status and headers, its body decoded
// if it was a document, and keeps nothing.
//
// document, when not nil, is a document's 200 already opened for reading.
func (n *npmCache) relayBody(conn net.Conn, req *http.Request, f npmFetch, document *os.File,
	started time.Time, request npmRequest, how string, session *cacheSession,
) {
	header := relayedHeader(f.resp.Header)
	var (
		body   io.Reader
		length int64
	)
	switch {
	case document != nil:
		body, length = document, f.size
	case f.document != "":
		file, err := os.Open(f.document)
		if err != nil {
			n.answerStatus(conn, req, http.StatusBadGateway)
			n.note(session, alloc.BuildCacheUnavailable)

			return
		}
		defer file.Close()
		body, length = file, f.size
	default:
		raw, err := io.ReadAll(io.LimitReader(decodedBody(f.resp), npmErrorBodyLimit))
		if err != nil {
			raw = nil
		}
		body, length = strings.NewReader(string(raw)), int64(len(raw))
	}
	err := n.writeAnswer(conn, req, f.resp.StatusCode, header, body, length)
	outcome := alloc.BuildCacheCold
	if f.resp.StatusCode >= 500 {
		outcome = alloc.BuildCacheUnavailable
	}
	n.finishAnswer(session, outcome, err)
	n.logAnswer(request, how, f.resp.StatusCode, length, started, f.attempts)
}

// relay answers with an answer the store does not keep.
func (n *npmCache) relay(conn net.Conn, req *http.Request, request npmRequest, f npmFetch,
	started time.Time, session *cacheSession,
) {
	n.relayBody(conn, req, f, nil, started, request, "relayed", session)
}

// decodedBody is an answer's body without the gzip the node asked a document
// for. Any other coding is passed on as it came; the node never asks for one.
func decodedBody(resp *http.Response) io.Reader {
	if strings.EqualFold(strings.TrimSpace(resp.Header.Get("Content-Encoding")), "gzip") {
		if decoded, err := gzip.NewReader(resp.Body); err == nil {
			return decoded
		}
	}

	return resp.Body
}

func relayedHeader(upstream http.Header) http.Header {
	header := http.Header{}
	for _, name := range npmRelayedHeaders {
		if values := upstream.Values(name); len(values) > 0 {
			header[http.CanonicalHeaderKey(name)] = values
		}
	}

	return header
}

// writeAnswer writes one HTTP/1.1 answer and ends the connection's keep-alive,
// so the next request is classified on a new connection.
//
// WRITTEN BY HAND, so what a HEAD and a 304 carry is this function's to say: a
// HEAD gets the length a GET would and no body, a 304 neither. A body of
// unknown length (a negative length) is delimited by the connection's close.
// A body shorter than its declared length is an error, and the caller's close
// of the connection is what tells the client its answer was cut.
func (n *npmCache) writeAnswer(conn net.Conn, req *http.Request, status int, header http.Header,
	body io.Reader, length int64,
) error {
	header = header.Clone()
	header.Set("Connection", "close")
	header.Del("Content-Length")
	header.Del("Transfer-Encoding")
	if body == nil && status != http.StatusNotModified && req.Method != http.MethodHead {
		length = 0
	}
	if status != http.StatusNotModified && length >= 0 {
		header.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	buffered := bufio.NewWriter(conn)
	if _, err := fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status)); err != nil {
		return err
	}
	if err := header.Write(buffered); err != nil {
		return err
	}
	if _, err := buffered.WriteString("\r\n"); err != nil {
		return err
	}
	if body != nil && status != http.StatusNotModified && req.Method != http.MethodHead {
		written, err := io.Copy(buffered, body)
		if err != nil {
			return err
		}
		if length >= 0 && written != length {
			return io.ErrUnexpectedEOF
		}
	}

	return buffered.Flush()
}

// answerStatus answers with a bare status the node chose itself. Best effort:
// the connection is closed after it either way.
func (n *npmCache) answerStatus(conn net.Conn, req *http.Request, status int) {
	if err := n.writeAnswer(conn, req, status, http.Header{}, nil, 0); err != nil {
		n.service.log.Debug("an npm answer could not be written", "status", status, "error", err)
	}
}

// npmETagMatches is If-None-Match's weak comparison against one stored tag.
func npmETagMatches(list, etag string) bool {
	if etag == "" {
		return false
	}
	opaque := strings.TrimPrefix(etag, "W/")
	for _, candidate := range strings.Split(list, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" || strings.TrimPrefix(candidate, "W/") == opaque {
			return true
		}
	}

	return false
}

func (n *npmCache) logAnswer(request npmRequest, how string, status int, bytes int64, started time.Time,
	attempts int,
) {
	n.service.log.Debug("an npm request was answered", "path", request.path, "object", request.object,
		"how", how, "status", status, "bytes", bytes, "attempts", attempts,
		"duration", n.now().Sub(started).Round(time.Millisecond))
}
