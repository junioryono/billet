package node

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/wirecert"
)

const (
	actionsResultsHost = "results-receiver.actions.githubusercontent.com"
	actionsProxyUser   = "billet"
	actionsConnectWait = 10 * time.Second
	actionsHeaderLimit = 64 << 10
)

type actionsProxy struct {
	service *CacheService
	ca      *wirecert.CA
	leaf    tls.Certificate
	// dialUpstream opens the raw TLS connection a passthrough splices onto. A
	// seam, so a test can stand in for GitHub's edge without the network.
	dialUpstream func(ctx context.Context) (net.Conn, error)
}

func validateActionsScope(scope CacheSessionScope) error {
	for name, value := range map[string]string{
		"owner": scope.Owner, "repository": scope.Repository, "workflow ref": scope.WorkflowRef,
	} {
		if strings.TrimSpace(value) == "" || strings.TrimSpace(value) != value ||
			strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("node: intercepted cache session has an invalid %s", name)
		}
	}
	if len(scope.Owner) > 100 || len(scope.Repository) > 100 || len(scope.WorkflowRef) > 2048 {
		return errors.New("node: intercepted cache assignment identity is too long")
	}
	if strings.Contains(scope.Owner, "/") || strings.Contains(scope.Repository, "/") {
		return errors.New("node: intercepted cache owner and repository must be path components")
	}
	workflow, ref, ok := strings.Cut(scope.WorkflowRef, "@")
	if !ok || strings.TrimSpace(workflow) == "" || strings.TrimSpace(ref) == "" {
		return errors.New("node: intercepted cache workflow ref is malformed")
	}

	return nil
}

func (s *CacheService) ensureActionsProxy() (*actionsProxy, error) {
	if s.actions != nil {
		return s.actions, nil
	}

	stateDir := filepath.Join(s.rootState, "actions-interception")
	ca, err := wirecert.LoadOrCreateCA(stateDir, s.namespace+" actions interception")
	if err != nil {
		return nil, fmt.Errorf("node: load the Actions interception authority: %w", err)
	}
	bundle, err := ca.IssueServer([]string{actionsResultsHost})
	if err != nil {
		return nil, fmt.Errorf("node: issue the Actions results certificate: %w", err)
	}
	leaf, err := tls.X509KeyPair(bundle.CertPEM, bundle.KeyPEM)
	if err != nil {
		return nil, fmt.Errorf("node: load the Actions results certificate: %w", err)
	}

	s.actions = &actionsProxy{
		service: s, ca: ca, leaf: leaf, dialUpstream: dialActionsUpstream,
	}

	return s.actions, nil
}

// ensureNpmCache makes the npm cache beside the Actions proxy, under the same
// interception authority. Called with s.mu held.
//
// A STORE THAT CANNOT BE OPENED LEAVES THE CACHE SPLICING: the guest still
// trusts the authority and reaches the registry through it, uncached, rather
// than failing its handshake against a host it was told to remap.
func (s *CacheService) ensureNpmCache() error {
	if s.npm != nil {
		return nil
	}
	proxy, err := s.ensureActionsProxy()
	if err != nil {
		return err
	}
	bundle, err := proxy.ca.IssueServer([]string{npmRegistryHost})
	if err != nil {
		return fmt.Errorf("node: issue the npm registry certificate: %w", err)
	}
	leaf, err := tls.X509KeyPair(bundle.CertPEM, bundle.KeyPEM)
	if err != nil {
		return fmt.Errorf("node: load the npm registry certificate: %w", err)
	}
	store, err := openNpmStore(filepath.Join(s.rootState, "npm-registry"), npmStoreLimit)
	if err != nil {
		s.log.Warn("the npm store could not be opened; npm requests will go to the registry uncached",
			"error", err)
		store = nil
	}
	s.npm = newNpmCache(s, leaf, store)

	return nil
}

func (s *CacheService) npmCache() *npmCache {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.npm
}

// dialActionsUpstream opens the TLS connection a passthrough splices onto.
//
// ALPN IS PINNED TO HTTP/1.1 AND THAT PIN IS THE LOAD-BEARING LINE. The splice
// copies the guest's own HTTP/1.1 bytes verbatim, so an upstream that negotiated
// h2 would be handed h1 bytes it cannot frame. This is also what retired the
// previous defect outright: with a round-tripped passthrough, GitHub's edge
// negotiating h2 put `HTTP/2.0 200 OK` on an HTTP/1.1 connection and the
// official runner refused the status line. A splice has no re-framing to get
// wrong — the guest reads the bytes GitHub wrote.
//
// NO PROXY, DELIBERATELY: a passthrough that re-entered this listener through
// the node's own proxy environment would recurse until the job timed out.
func dialActionsUpstream(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer

	raw, err := dialer.DialContext(ctx, "tcp", actionsResultsHost+":443")
	if err != nil {
		return nil, err
	}

	conn := tls.Client(raw, actionsUpstreamTLSConfig())
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()

		return nil, err
	}

	return conn, nil
}

func actionsUpstreamTLSConfig() *tls.Config {
	return &tls.Config{
		ServerName: actionsResultsHost,
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	}
}

func proxyURL(endpoint, token string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("node: parse the cache proxy endpoint: %w", err)
	}
	u.User = url.UserPassword(actionsProxyUser, token)

	return u.String(), nil
}

func (s *CacheService) actionsProxy() *actionsProxy {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.actions
}

// authenticateProxy names the session a CONNECT belongs to.
//
// IT NEVER WAITS ON THE SESSION'S LOCK. A guest's relay gives the node ten
// seconds to answer a CONNECT and then dials GitHub instead, and the lock is held
// for as long as a block upload, a finalization or a renewal through the site's
// cache index takes; a tunnel for a blob URL billet signed, reaching GitHub that
// way, was answered "404 page not found" and failed the job's save (#694). What
// it reads is fixed when the session is made, and closing mirrors closed.
func (s *CacheService) authenticateProxy(r *http.Request) (*cacheSession, bool) {
	value := r.Header.Get("Proxy-Authorization")
	scheme, encoded, ok := strings.Cut(value, " ")
	if !ok || !strings.EqualFold(scheme, "basic") {
		return nil, false
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, false
	}
	user, token, ok := strings.Cut(string(raw), ":")
	if !ok || user != actionsProxyUser || token == "" || strings.ContainsAny(token, " ,\t\r\n") {
		return nil, false
	}

	s.mu.Lock()
	session := s.byToken[token]
	s.mu.Unlock()
	if session == nil {
		return nil, false
	}

	return session, (session.intercept || session.npmEnabled()) && !session.closing.Load()
}

// npmEnabled reports whether the session's tier has the npm cache. The spec is
// fixed when the session is made.
func (session *cacheSession) npmEnabled() bool { return session.cache != nil && session.cache.Npm }

func (p *actionsProxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	session, ok := p.service.authenticateProxy(r)
	if !ok {
		w.Header().Set("Proxy-Authenticate", `Basic realm="billet"`)
		http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)

		return
	}

	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		http.Error(w, "invalid CONNECT authority", http.StatusBadRequest)

		return
	}
	// EACH HOST IS ITS OWN SWITCH: a session reaches only the origins its tier
	// enables, whatever host it names.
	switch {
	case port != "443":
	case host == actionsResultsHost && session.intercept:
		p.intercept(r.Context(), w, session)

		return
	case host == npmRegistryHost && session.npmEnabled():
		if npm := p.service.npmCache(); npm != nil {
			npm.intercept(r.Context(), w, session)

			return
		}
	}
	http.Error(w, "only the origins this guest's tier intercepts are available", http.StatusForbidden)
}

// actionsRequestTargetsResultsHost reports whether the inner request is
// addressed to the results host in origin form.
//
// Absolute-form targets (URL.Host set) are refused because the tunnel is a
// single origin, and the Host header — with or without the :443 port — must be
// exactly the results host. http.ReadRequest fills req.Host from the Host
// header for an origin-form request, so this reads the guest's own claim.
func actionsRequestTargetsResultsHost(req *http.Request) bool {
	// ANY SCHEME MEANS ABSOLUTE OR OPAQUE FORM, refused wholesale. Go parses
	// `https://host/p` with a Host, but also `https:/p` and `https:evil` with an
	// empty Host and a set Scheme, so keying only on URL.Host let those through.
	// An origin-form request the runner actually sends has neither.
	if req.URL.Scheme != "" || req.URL.Host != "" {
		return false
	}

	// CASE-INSENSITIVE, because a host name is, and the runner has no obligation
	// to spell it lowercase. The port, when present, must be 443.
	host := req.Host
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port != "443" {
			return false
		}
		host = h
	}

	return strings.EqualFold(host, actionsResultsHost)
}

func hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("HTTP server does not support connection hijacking")
	}

	return h.Hijack()
}

// openTunnel answers an authenticated CONNECT and completes the guest's TLS
// handshake as leaf. The caller closes the returned raw connection.
func openTunnel(ctx context.Context, w http.ResponseWriter, leaf tls.Certificate) (net.Conn, *tls.Conn, bool) {
	client, buffered, err := hijack(w)
	if err != nil {
		return nil, nil, false
	}
	// The listener's ordinary request deadline remains attached after Hijack.
	// Clear it before the TLS handshake and let the bounded handshake/request
	// contexts govern this connection.
	if err := client.SetDeadline(time.Time{}); err != nil {
		_ = client.Close()

		return nil, nil, false
	}

	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = client.Close()

		return nil, nil, false
	}
	if err := buffered.Flush(); err != nil {
		_ = client.Close()

		return nil, nil, false
	}

	tlsConn := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS13,
	})
	handshakeCtx, cancel := context.WithTimeout(ctx, actionsConnectWait)
	defer cancel()
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		_ = client.Close()

		return nil, nil, false
	}

	return client, tlsConn, true
}

func (p *actionsProxy) intercept(
	ctx context.Context,
	w http.ResponseWriter,
	session *cacheSession,
) {
	client, tlsConn, ok := openTunnel(ctx, w, p.leaf)
	if !ok {
		return
	}
	defer client.Close()

	// EVERY BYTE THE PARSER CONSUMES IS CAPTURED, so a request that turns out to
	// be GitHub's rather than billet's can be replayed onto the upstream exactly
	// as the guest sent it. The capture is bounded: a request that is not
	// billet's is classified from its headers alone, and the intercepted twirp
	// methods read at most a bounded metadata body — a blob upload's gigabytes
	// never pass through while the capture is armed, because its path decides
	// the classification before its body is read.
	capture := &actionsCaptureReader{
		reader: &actionsHeaderReader{reader: tlsConn, remaining: actionsHeaderLimit},
		limit:  actionsHeaderLimit + actionsRequestLimit + 4096,
	}
	reader := bufio.NewReader(capture)
	if err := tlsConn.SetReadDeadline(time.Now().Add(actionsConnectWait)); err != nil {
		return
	}
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	if err := tlsConn.SetDeadline(time.Now().Add(cacheHandlerLimit)); err != nil {
		return
	}
	requestCtx, requestCancel := context.WithTimeout(ctx, cacheHandlerLimit)
	defer requestCancel()
	req = req.WithContext(requestCtx)

	// THE INNER REQUEST MUST NAME THE RESULTS HOST, restoring the invariant the
	// old round trip enforced by rewriting req.Host. A splice replays the guest's
	// own bytes, so a guest that completed TLS with the results SNI could send an
	// absolute-form target or a foreign Host header and hand routing to whatever
	// GitHub's edge does with the mismatch. Bytes still cannot leave the results
	// TCP connection, so this is hardening rather than an escape — but "exactly
	// one host" should hold at the HTTP layer too, and the check is one line.
	if !actionsRequestTargetsResultsHost(req) {
		misdirected := actionsBlobError(http.StatusMisdirectedRequest,
			"only the Actions results origin is available")
		misdirected.Close = true
		misdirected.Proto, misdirected.ProtoMajor, misdirected.ProtoMinor = "HTTP/1.1", 1, 1
		//nolint:errcheck // best-effort refusal before anything is forwarded
		_ = misdirected.Write(tlsConn)
		_ = misdirected.Body.Close()

		return
	}

	// A BLOB TRANSFER'S BODY MUST NOT BE CAPTURED. Its classification is decided
	// by its path alone, it can never fall back to GitHub (a reservation is
	// billet's own), and its body is up to ten gigabytes.
	if strings.HasPrefix(req.URL.Path, actionsBlobPrefix) {
		capture.disarm()
	}

	resp, handled := p.respond(req, session)
	if !handled {
		p.splice(ctx, tlsConn, capture)

		return
	}

	defer resp.Body.Close()
	// One request per intercepted tunnel keeps a local framing mistake or a
	// partially consumed body from contaminating the next results operation; the
	// toolkit clients reconnect, and each fresh connection classifies afresh.
	resp.Close = true
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	if err := resp.Write(tlsConn); err != nil {
		return
	}
}

// splice joins the guest's connection to GitHub's edge and copies bytes both
// ways until either side finishes.
//
// A SPLICE, NOT A ROUND TRIP, because "everything else remains upstream" has to
// mean the protocol too. The results host carries more than request/response
// HTTP: the official runner holds a WEBSOCKET open to it for live job logs —
// GitHub's own runner documentation requires wss to results-receiver — and a
// passthrough that answered one HTTP response per connection killed that feed
// the moment the upgrade completed. Measured live: every job's log pane was
// blank, and the runner's blob uploads of the finished logs died with it. Raw
// bytes carry upgrades, chunked bodies, 100-continue and keep-alive without
// this proxy having to understand any of them.
func (p *actionsProxy) splice(
	ctx context.Context,
	guest net.Conn,
	capture *actionsCaptureReader,
) {
	spliceTo(ctx, p.service.log, guest, capture, p.dialUpstream, "GitHub's results service")
}

// spliceTo replays the captured request onto a connection dial opens and joins
// the two until either side finishes.
func spliceTo(
	ctx context.Context,
	log *slog.Logger,
	guest net.Conn,
	capture *actionsCaptureReader,
	dial func(context.Context) (net.Conn, error),
	origin string,
) {
	captured, ok := capture.take()
	if !ok {
		// The capture overflowed, so the request cannot be replayed faithfully.
		// Refusing beats forwarding a truncated request the origin would misread.
		log.Warn("a passthrough request was too large to replay upstream", "origin", origin)

		return
	}

	dialCtx, cancel := context.WithTimeout(ctx, actionsConnectWait)
	upstream, err := dial(dialCtx)

	cancel()

	if err != nil {
		log.Warn("a passthrough could not reach its origin", "origin", origin, "error", err)
		response := actionsBlobError(http.StatusBadGateway, origin+" is unavailable")
		response.Close = true
		//nolint:errcheck // best-effort refusal on a connection being abandoned
		_ = response.Write(guest)
		_ = response.Body.Close()

		return
	}
	defer upstream.Close()

	// THE TUNNEL OUTLIVES EVERY PER-REQUEST BOUND from here: a live-log
	// websocket lasts as long as the job does. Its lifetime is the two
	// connections' own.
	if err := guest.SetDeadline(time.Time{}); err != nil {
		return
	}

	if _, err := upstream.Write(captured); err != nil {
		return
	}

	// EITHER DIRECTION ENDING TEARS DOWN BOTH, and this is a correctness rule, not
	// tidiness. A half-close would let a healthy live-log websocket run
	// unbounded — which is wanted — but it also lets a HOSTILE guest that never
	// closes its own write side pin this handler goroutine and both descriptors
	// forever after GitHub has already finished: the second io.Copy would block
	// on a read that never returns. For HTTP/1.1 and for a websocket alike, the
	// first EOF is the connection ending, so closing both unblocks the surviving
	// copy at once. The tunnel therefore lasts exactly as long as both peers keep
	// it open and not one read longer.
	done := make(chan struct{}, 2)

	copyOne := func(dst, src net.Conn) {
		//nolint:errcheck // a spliced peer disconnecting mid-copy is the ordinary end of a tunnel
		_, _ = io.Copy(dst, src)
		done <- struct{}{}
	}

	go copyOne(upstream, guest)
	go copyOne(guest, upstream)

	<-done
	// One side finished. Closing both interrupts the read the other copy is
	// blocked on; upstream's close is also handled by the deferred Close, and a
	// second close is harmless.
	_ = guest.Close()
	_ = upstream.Close()
	<-done
}

// actionsCaptureReader records what passes through it until disarmed, so a
// request already consumed by the HTTP parser can be replayed byte-for-byte.
type actionsCaptureReader struct {
	reader     io.Reader
	limit      int
	stopped    bool
	overflowed bool
	buffer     bytes.Buffer
}

func (r *actionsCaptureReader) Read(p []byte) (int, error) {
	count, err := r.reader.Read(p)
	if count > 0 && !r.stopped && !r.overflowed {
		if r.buffer.Len()+count > r.limit {
			r.overflowed = true
			r.buffer.Reset()
		} else {
			r.buffer.Write(p[:count])
		}
	}

	return count, err
}

func (r *actionsCaptureReader) disarm() {
	r.stopped = true
	r.buffer.Reset()
}

func (r *actionsCaptureReader) take() ([]byte, bool) {
	if r.stopped || r.overflowed {
		return nil, false
	}

	return r.buffer.Bytes(), true
}

type actionsHeaderReader struct {
	reader    io.Reader
	remaining int
	window    [4]byte
	seen      int
	done      bool
}

func (r *actionsHeaderReader) Read(buffer []byte) (int, error) {
	count, err := r.reader.Read(buffer)
	if r.done {
		return count, err
	}
	for index, value := range buffer[:count] {
		r.remaining--
		if r.remaining < 0 {
			return index + 1, errors.New("actions proxy request headers exceed 64KiB")
		}
		r.window[r.seen%len(r.window)] = value
		r.seen++
		if r.seen >= len(r.window) &&
			r.window[(r.seen-4)%4] == '\r' && r.window[(r.seen-3)%4] == '\n' &&
			r.window[(r.seen-2)%4] == '\r' && r.window[(r.seen-1)%4] == '\n' {
			r.done = true

			break
		}
	}

	return count, err
}

// respond serves a request locally, or reports that it belongs to GitHub.
//
// handled=false NEVER FOLLOWS A LOCAL SIDE EFFECT the guest could observe: a
// request that falls through is replayed to GitHub from its captured bytes, so
// anything this consumed beyond the bounded metadata body would be lost. A
// reservation-bound request never falls through at all — the reservation is
// billet's own, and GitHub has nothing to answer it with.
func (p *actionsProxy) respond(req *http.Request, session *cacheSession) (*http.Response, bool) {
	// THE OBSERVATION IS RECORDED WHERE THE DISPOSITION IS FINAL. A CacheService
	// call the handler answered was served; one the handler failed is either
	// retried through GitHub (a splice, whatever billet intended) or, when it is
	// bound to a reservation only billet holds, refused to the guest as
	// unavailable, and so is a blob leg; one refused before it was read is
	// unavailable too. The calls
	// actionsResponse decides on its own -- refused by the kill switch, or from
	// a client billet does not serve -- are recorded there.
	//
	// IN FLIGHT FROM HERE UNTIL THE OUTCOME IS RECORDED, so a settlement that
	// runs in between does not write `unused` over a call still being answered.
	cacheCall := actionsCacheCall(req)
	if cacheCall {
		p.service.beginActionsCall(session)
		defer p.service.endActionsCall(session)
	}
	observe := func(ctx context.Context, outcome alloc.ActionsCache) {
		if cacheCall {
			p.service.observeActions(ctx, session, outcome)
		}
	}

	var replay []byte
	if actionsLocalRequest(req) && cacheCall {
		var err error
		replay, err = io.ReadAll(io.LimitReader(req.Body, actionsRequestLimit+1))
		if err != nil {
			observe(req.Context(), alloc.ActionsCacheUnavailable)

			return actionsBlobError(http.StatusBadRequest,
				"Actions cache metadata request could not be read"), true
		}
		if len(replay) > actionsRequestLimit {
			observe(req.Context(), alloc.ActionsCacheUnavailable)

			return actionsBlobError(http.StatusRequestEntityTooLarge,
				"Actions cache metadata request is too large"), true
		}
		req.Body = io.NopCloser(bytes.NewReader(replay))
	}

	reservationBound := strings.HasPrefix(req.URL.Path, actionsBlobPrefix)
	if req.URL.Path == actionsFinalizePath && replay != nil {
		var err error
		reservationBound, err = p.service.actionsFinalizeReserved(req.Context(), replay, session)
		if err != nil {
			observe(req.Context(), alloc.ActionsCacheUnavailable)

			return actionsBlobError(http.StatusBadGateway,
				"Actions cache storage is unavailable"), true
		}
		if response, found, err := p.service.actionsFinalizeReceipt(req.Context(), replay, session); found {
			if err != nil {
				p.service.log.Warn("a reserved Actions cache finalization failed locally",
					"instance", session.instance, "path", req.URL.Path, "error", err)
				observe(req.Context(), alloc.ActionsCacheUnavailable)

				return actionsBlobError(http.StatusBadGateway,
					"Actions cache storage is unavailable"), true
			}
			response.Request = req
			observe(req.Context(), alloc.ActionsCacheServed)

			return response, true
		}
	}
	if response, handled, err := p.service.actionsResponse(req, session); handled && err == nil {
		response.Request = req
		observe(req.Context(), alloc.ActionsCacheServed)

		return response, true
	} else if handled && err != nil {
		if reservationBound {
			p.service.log.Warn("a reserved Actions cache request failed locally",
				"instance", session.instance, "path", req.URL.Path, "error", err)
			// A BLOB LEG'S FAILURE IS RECORDED TOO, though a blob leg is not a
			// CacheService call: a save whose blocks failed on the node was
			// otherwise recorded as whatever its reservation was (#694).
			p.service.observeActions(req.Context(), session, alloc.ActionsCacheUnavailable)

			return actionsBlobError(http.StatusBadGateway, "Actions cache storage is unavailable"), true
		}
		p.service.log.Warn("Actions cache interception failed; retrying through GitHub",
			"instance", session.instance, "path", req.URL.Path, "error", err)
		observe(req.Context(), alloc.ActionsCacheSpliced)
	} else if reservationBound {
		p.service.log.Warn("a reserved Actions cache request cannot be passed to GitHub",
			"instance", session.instance, "path", req.URL.Path)

		return actionsBlobError(http.StatusBadGateway, "Actions cache reservation is unavailable"), true
	}

	return nil, false
}
