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
	npmDocumentLimit = 256 << 20
	// npmFills bounds the fills in flight at once, across every guest: with
	// npmDocumentLimit it bounds the disk a burst of documents can take before
	// the fill budget refuses to keep them.
	npmFills = 32
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
	slots        chan struct{}

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
		slots: make(chan struct{}, npmFills),
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

// npmExchange is one request being answered: who asked, for what, and when.
type npmExchange struct {
	conn    net.Conn
	req     *http.Request
	request npmRequest
	session *cacheSession
	started time.Time
}

// serve answers one request the store may answer.
func (n *npmCache) serve(ctx context.Context, conn net.Conn, req *http.Request, request npmRequest,
	session *cacheSession,
) {
	x := npmExchange{conn: conn, req: req, request: request, session: session, started: n.now()}
	key := request.key()
	if entry, body, ok := n.store.acquire(key, n.now()); ok {
		if entry.fresh(n.now()) {
			defer body.Close()
			n.answerEntry(x, entry, body, "warm")

			return
		}
		_ = body.Close()
	}
	// ONE FILL PER ENTRY AT A TIME: a second job asking for what is being filled
	// waits for it, and is then answered from the store.
	release, ok := n.lockFill(ctx, key)
	if !ok {
		return
	}
	defer release()
	select {
	case n.slots <- struct{}{}:
		defer func() { <-n.slots }()
	case <-ctx.Done():
		return
	}
	// THE ENTRY AND ITS BODY ARE HELD TOGETHER FROM HERE, so the answer a stale
	// fallback or a revalidation gives is the generation that was judged.
	entry, body, held := n.store.acquire(key, n.now())
	if held {
		defer body.Close()
		if entry.fresh(n.now()) {
			n.answerEntry(x, entry, body, "warm")

			return
		}
	}
	validator := ""
	if held {
		validator = entry.ETag
	}
	fetch, answered := n.fetch(ctx, request, validator)
	defer fetch.close()
	if !answered {
		if held && entry.staleUsable(n.now()) {
			n.service.log.Warn("the npm registry gave no answer; serving a stale entry", "path", request.path,
				"instance", session.instance, "expired", entry.expiry())
			n.answerEntry(x, entry, body, "stale")

			return
		}
		n.note(session, alloc.BuildCacheUnavailable)
		if fetch.resp != nil {
			n.relay(x, fetch, "failed")

			return
		}
		n.answerStatus(x, http.StatusGatewayTimeout)

		return
	}
	previous := ""
	if held {
		previous = entry.Body
	}
	status := fetch.resp.StatusCode
	switch {
	case status == http.StatusNotModified && held:
		n.answerRevalidated(ctx, x, entry, body, fetch)
	case status == http.StatusOK && request.object != npmTarball:
		n.answerDocument(x, fetch, previous)
	case status == http.StatusOK:
		n.answerTarball(x, fetch, previous, true)
	default:
		if npmDefinitive(status) && held {
			n.store.removeIf(key, previous)
		}
		n.relay(x, fetch, "relayed")
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

func npmRetryable(status int) bool {
	return status == http.StatusBadGateway || status == http.StatusServiceUnavailable ||
		status == http.StatusGatewayTimeout
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
	// keep says a document's 200 was read within the fill budget, so it may be
	// stored.
	keep bool
}

func (f *npmFetch) close() {
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
// the last 5xx the registry gave, if it gave one, for the caller to relay when
// no stale entry may answer instead.
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
		case npmRetryable(resp.StatusCode):
			// HELD, NOT RELAYED: the retry or a stale entry may still answer
			// instead, and the last one the registry gave is relayed if neither
			// does.
			n.service.log.Info("the npm registry answered with a retryable status", "path", request.path,
				"attempt", attempt, "status", resp.StatusCode)
			last.close()
			last = f
		case resp.StatusCode == http.StatusOK && request.object != npmTarball:
			document, size, keep, err := n.readDocument(f)
			cancel()
			if err != nil {
				_ = resp.Body.Close()
				n.service.log.Info("an npm registry document stalled before it was answered",
					"path", request.path, "attempt", attempt, "error", err)

				continue
			}
			f.document, f.size, f.keep = document, size, keep
			last.close()

			return f, true
		default:
			last.close()

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
// keep is false when the read went past the fill budget: the document still
// answers its client, and is not stored.
func (n *npmCache) readDocument(f npmFetch) (string, int64, bool, error) {
	body := io.Reader(f.resp.Body)
	switch strings.ToLower(strings.TrimSpace(f.resp.Header.Get("Content-Encoding"))) {
	case "", "identity":
	case "gzip":
		decoded, err := gzip.NewReader(f.resp.Body)
		if err != nil {
			return "", 0, false, err
		}
		defer decoded.Close()
		body = decoded
	default:
		return "", 0, false, fmt.Errorf("an encoding the node did not ask for: %q",
			f.resp.Header.Get("Content-Encoding"))
	}
	temp, err := n.store.temp()
	if err != nil {
		return "", 0, false, err
	}
	budget := &npmBudget{store: n.store, keep: true}
	defer budget.done()
	size, err := io.Copy(io.MultiWriter(temp, budget), io.LimitReader(body, npmDocumentLimit+1))
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err == nil && size > npmDocumentLimit {
		err = errors.New("the document is larger than the node reads whole")
	}
	encoding := strings.TrimSpace(f.resp.Header.Get("Content-Encoding"))
	if err == nil && (encoding == "" || strings.EqualFold(encoding, "identity")) && f.resp.ContentLength >= 0 &&
		size != f.resp.ContentLength {
		err = io.ErrUnexpectedEOF
	}
	if err != nil {
		_ = os.Remove(temp.Name())

		return "", 0, false, err
	}

	return temp.Name(), size, budget.keep, nil
}

// npmBudget charges what a fill writes to the store's fill budget, and stops
// keeping it, never stops the fill, once the budget refuses.
type npmBudget struct {
	store    *npmStore
	reserved int64
	keep     bool
}

func (b *npmBudget) Write(p []byte) (int, error) {
	b.charge(len(p))

	return len(p), nil
}

// charge claims n more bytes, reporting whether the fill may still be kept.
func (b *npmBudget) charge(n int) bool {
	if b.keep {
		if b.store.reserve(int64(n)) {
			b.reserved += int64(n)
		} else {
			b.keep = false
		}
	}

	return b.keep
}

func (b *npmBudget) done() {
	b.store.release(b.reserved)
	b.reserved = 0
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

// answerDocument answers a document's 200 and keeps it if it may be shared.
// previous is the body of the generation it replaces, which an answer that may
// not be shared removes: the registry no longer shares what it held.
func (n *npmCache) answerDocument(x npmExchange, f npmFetch, previous string) {
	// OPENED BEFORE THE COMMIT, which consumes the file whether or not it
	// succeeds: a store that refuses the document still answers the client.
	document, err := os.Open(f.document)
	if err != nil {
		n.answerStatus(x, http.StatusBadGateway)
		n.note(x.session, alloc.BuildCacheUnavailable)

		return
	}
	defer document.Close()
	shareable := npmShareable(f.resp.Header)
	if !shareable && previous != "" {
		n.store.removeIf(x.request.key(), previous)
	}
	if !shareable || !f.keep {
		n.relayDocument(x, f, document)

		return
	}
	entry := entryFor(x.request, f, f.size)
	if err := n.store.commit(entry, f.document); err != nil {
		n.service.log.Warn("the npm store could not keep a document", "path", x.request.path, "error", err)
		n.relayDocument(x, f, document)

		return
	}
	stored, body, ok := n.store.acquire(entry.Key, n.now())
	if !ok {
		n.relayDocument(x, f, document)

		return
	}
	defer body.Close()
	n.answerEntry(x, stored, body, "filled")
}

// answerTarball streams a tarball's 200 to the client as it arrives and keeps
// it when it arrives whole, may be shared, is not encoded and fits the fill
// budget. mayKeep false streams it and keeps nothing.
func (n *npmCache) answerTarball(x npmExchange, f npmFetch, previous string, mayKeep bool) {
	shareable := npmShareable(f.resp.Header)
	if !shareable && previous != "" {
		n.store.removeIf(x.request.key(), previous)
	}
	encoding := strings.ToLower(strings.TrimSpace(f.resp.Header.Get("Content-Encoding")))
	header := relayedHeader(f.resp.Header)
	length := f.resp.ContentLength
	var source io.Reader = f.resp.Body
	switch encoding {
	case "", "identity":
	case "gzip":
		// AN HTTP CODING THE NODE DID NOT ASK FOR IS TAKEN OFF, so the client
		// receives the tarball's own bytes, which its integrity covers.
		decoded, err := gzip.NewReader(f.resp.Body)
		if err != nil {
			n.answerStatus(x, http.StatusBadGateway)
			n.note(x.session, alloc.BuildCacheUnavailable)

			return
		}
		defer decoded.Close()
		source, length = decoded, -1
	default:
		header.Set("Content-Encoding", f.resp.Header.Get("Content-Encoding"))
	}
	keep := mayKeep && shareable && (encoding == "" || encoding == "identity")
	counted := &npmCountingReader{reader: source}
	var fill *npmKeeper
	if keep {
		if temp, err := n.store.temp(); err == nil {
			fill = &npmKeeper{file: temp, budget: &npmBudget{store: n.store, keep: true}}
			defer fill.discard()
		}
	}
	var body io.Reader = counted
	if fill != nil {
		body = io.TeeReader(counted, fill)
	}
	var err error
	if x.req.Method == http.MethodHead {
		// A HEAD IS ANSWERED FROM THIS ANSWER, read whole so it can be kept,
		// never from an older generation the store may still hold.
		if _, err = io.Copy(io.Discard, body); err == nil {
			err = n.writeAnswer(x, http.StatusOK, header, nil, counted.count)
		}
	} else {
		err = n.writeAnswer(x, http.StatusOK, header, body, length)
	}
	complete := err == nil && counted.eof && (f.resp.ContentLength < 0 || encoding != "" && encoding != "identity" ||
		counted.count == f.resp.ContentLength)
	if fill != nil && complete {
		if path, ok := fill.finish(); ok {
			if err := n.store.commit(entryFor(x.request, f, counted.count), path); err != nil {
				n.service.log.Warn("the npm store could not keep a tarball", "path", x.request.path, "error", err)
			}
		}
	}
	outcome := alloc.BuildCacheCold
	if err != nil {
		outcome = alloc.BuildCacheUnavailable
	}
	n.note(x.session, outcome)
	n.logAnswer(x, "filled", http.StatusOK, counted.count, f.attempts)
}

// npmKeeper is a tarball's fill: its temporary file, charged to the budget,
// given up the moment the budget or the disk refuses.
type npmKeeper struct {
	file   *os.File
	budget *npmBudget
	failed bool
}

// Write never fails the client's copy: a fill the store cannot keep stops
// keeping.
func (k *npmKeeper) Write(p []byte) (int, error) {
	if k.failed {
		return len(p), nil
	}
	if !k.budget.charge(len(p)) {
		k.failed = true

		return len(p), nil
	}
	if _, err := k.file.Write(p); err != nil {
		k.failed = true
	}

	return len(p), nil
}

// finish closes the fill and names its file, when it was kept whole.
func (k *npmKeeper) finish() (string, bool) {
	if k.file == nil || k.failed {
		return "", false
	}
	if err := k.file.Close(); err != nil {
		k.failed = true

		return "", false
	}
	path := k.file.Name()
	k.file = nil

	return path, true
}

// discard gives back the budget and removes a fill nothing committed.
func (k *npmKeeper) discard() {
	k.budget.done()
	if k.file != nil {
		_ = k.file.Close()
		_ = os.Remove(k.file.Name())
		k.file = nil
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
func (n *npmCache) answerRevalidated(ctx context.Context, x npmExchange, entry npmEntry, body *os.File,
	f npmFetch,
) {
	merged := entry.headers()
	// MERGED AS RFC 9111 4.3.4 MERGES, by canonical name, so a 304 that changes
	// the validator changes it.
	for _, name := range []string{"Cache-Control", "ETag", "Last-Modified", "Date", "Age", "Expires",
		"Set-Cookie"} {
		if values := f.resp.Header.Values(name); len(values) > 0 {
			merged.Del(name)
			for _, value := range values {
				merged.Add(name, value)
			}
		}
	}
	if !npmShareable(merged) {
		// THE VALIDATOR WAS THE NODE'S, so this 304 is never the client's answer:
		// ask again without one and relay that, unkept.
		n.store.removeIf(entry.Key, entry.Body)
		f.close()
		again, answered := n.fetch(ctx, x.request, "")
		defer again.close()
		switch {
		case !answered && again.resp == nil:
			n.note(x.session, alloc.BuildCacheUnavailable)
			n.answerStatus(x, http.StatusGatewayTimeout)
		case !answered:
			n.note(x.session, alloc.BuildCacheUnavailable)
			n.relay(x, again, "failed")
		case again.resp.StatusCode == http.StatusOK && x.request.object == npmTarball:
			n.answerTarball(x, again, "", false)
		case again.resp.StatusCode == http.StatusOK && again.document != "":
			document, err := os.Open(again.document)
			if err != nil {
				n.answerStatus(x, http.StatusBadGateway)

				return
			}
			defer document.Close()
			n.relayDocument(x, again, document)
		default:
			n.relay(x, again, "relayed")
		}

		return
	}
	refreshed := entry
	refreshed.ETag = merged.Get("ETag")
	refreshed.LastModified = merged.Get("Last-Modified")
	npmFreshness(&refreshed, merged, f.requested, f.received)
	if err := n.store.refresh(refreshed); err != nil {
		n.service.log.Warn("the npm store could not keep a revalidation", "path", x.request.path, "error", err)
	}
	n.answerEntry(x, refreshed, body, "revalidated")
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

// answerEntry answers from a stored entry and the body acquired with it, with
// a 304 when the client's own validator names it.
func (n *npmCache) answerEntry(x npmExchange, entry npmEntry, body *os.File, how string) {
	now := n.now()
	header := entry.headers()
	header.Set("Age", strconv.FormatInt(int64(entry.age(now)/time.Second), 10))
	header.Set("Date", now.UTC().Format(http.TimeFormat))
	outcome := alloc.BuildCacheWarm
	if how == "filled" {
		outcome = alloc.BuildCacheCold
	}
	var err error
	status := http.StatusOK
	if x.request.ifNoneMatch != "" && npmETagMatches(x.request.ifNoneMatch, entry.ETag) {
		status = http.StatusNotModified
		err = n.writeAnswer(x, status, header, nil, -1)
	} else {
		err = n.writeAnswer(x, status, header, body, entry.Size)
	}
	if err != nil {
		outcome = alloc.BuildCacheUnavailable
	}
	n.note(x.session, outcome)
	n.logAnswer(x, how, status, entry.Size, 0)
}

// relayDocument answers with a document's 200 read whole, without storing it.
func (n *npmCache) relayDocument(x npmExchange, f npmFetch, document *os.File) {
	err := n.writeAnswer(x, http.StatusOK, relayedHeader(f.resp.Header), document, f.size)
	outcome := alloc.BuildCacheCold
	if err != nil {
		outcome = alloc.BuildCacheUnavailable
	}
	n.note(x.session, outcome)
	n.logAnswer(x, "unkept", http.StatusOK, f.size, f.attempts)
}

// relay answers with the registry's own status, headers and body, streamed and
// never cut short, a document's gzip taken off, and keeps nothing.
func (n *npmCache) relay(x npmExchange, f npmFetch, how string) {
	header := relayedHeader(f.resp.Header)
	var body io.Reader = f.resp.Body
	length := f.resp.ContentLength
	switch encoding := strings.ToLower(strings.TrimSpace(f.resp.Header.Get("Content-Encoding"))); encoding {
	case "", "identity":
	case "gzip":
		if decoded, err := gzip.NewReader(f.resp.Body); err == nil {
			defer decoded.Close()
			body, length = decoded, -1
		} else {
			header.Set("Content-Encoding", f.resp.Header.Get("Content-Encoding"))
		}
	default:
		header.Set("Content-Encoding", f.resp.Header.Get("Content-Encoding"))
	}
	err := n.writeAnswer(x, f.resp.StatusCode, header, body, length)
	outcome := alloc.BuildCacheCold
	if err != nil || f.resp.StatusCode >= 500 {
		outcome = alloc.BuildCacheUnavailable
	}
	n.note(x.session, outcome)
	n.logAnswer(x, how, f.resp.StatusCode, length, f.attempts)
}

func relayedHeader(upstream http.Header) http.Header {
	header := http.Header{}
	for _, name := range npmRelayedHeaders {
		for _, value := range upstream.Values(name) {
			header.Add(name, value)
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
// THE HEADERS ARE SENT BEFORE THE BODY IS READ, so a client whose body then
// stalls has its answer begun. A body shorter than its declared length is an
// error, and the caller's close of the connection tells the client it was cut.
func (n *npmCache) writeAnswer(x npmExchange, status int, header http.Header, body io.Reader, length int64) error {
	header = header.Clone()
	header.Set("Connection", "close")
	header.Del("Content-Length")
	header.Del("Transfer-Encoding")
	head := x.req.Method == http.MethodHead
	if body == nil && status != http.StatusNotModified && !head {
		length = 0
	}
	if status != http.StatusNotModified && length >= 0 {
		header.Set("Content-Length", strconv.FormatInt(length, 10))
	}
	buffered := bufio.NewWriter(x.conn)
	if _, err := fmt.Fprintf(buffered, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status)); err != nil {
		return err
	}
	if err := header.Write(buffered); err != nil {
		return err
	}
	if _, err := buffered.WriteString("\r\n"); err != nil {
		return err
	}
	if err := buffered.Flush(); err != nil {
		return err
	}
	if body == nil || status == http.StatusNotModified || head {
		return nil
	}
	written, err := io.Copy(buffered, body)
	if err != nil {
		return err
	}
	if length >= 0 && written != length {
		return io.ErrUnexpectedEOF
	}

	return buffered.Flush()
}

// answerStatus answers with a bare status the node chose itself. Best effort:
// the connection is closed after it either way.
func (n *npmCache) answerStatus(x npmExchange, status int) {
	if err := n.writeAnswer(x, status, http.Header{}, nil, 0); err != nil {
		n.service.log.Debug("an npm answer could not be written", "status", status, "error", err)
	}
	n.logAnswer(x, "failed", status, 0, 0)
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

func (n *npmCache) logAnswer(x npmExchange, how string, status int, bytes int64, attempts int) {
	n.service.log.Debug("an npm request was answered", "path", x.request.path, "object", x.request.object,
		"how", how, "status", status, "bytes", bytes, "attempts", attempts,
		"duration", n.now().Sub(x.started).Round(time.Millisecond))
}
