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
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/junioryono/billet/internal/wirecert"
)

const (
	actionsResultsHost = "results-receiver.actions.githubusercontent.com"
	actionsProxyUser   = "billet"
	actionsConnectWait = 10 * time.Second
	actionsHeaderLimit = 64 << 10
)

type actionsProxy struct {
	service  *CacheService
	ca       *wirecert.CA
	leaf     tls.Certificate
	upstream http.RoundTripper
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

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("node: default HTTP transport has an unsupported type")
	}
	transport := defaultTransport.Clone()
	// Never inherit the node process's proxy environment. A passthrough request
	// returning to this listener would recurse until the job timed out.
	transport.Proxy = nil
	// THE TUNNEL SPEAKS HTTP/1.1 AND THE UPSTREAM MUST NOT DECIDE OTHERWISE.
	// With HTTP/2 left on, GitHub's edge negotiates h2 and Response.Write then
	// puts `HTTP/2.0 200 OK` on the wire of an HTTP/1.1 connection — measured:
	// the .NET runner refuses it with "Received an invalid status line", which
	// breaks every passthrough call the runner itself makes (step updates, log
	// uploads, artifacts) while local cache responses keep working. The guest
	// client chose HTTP/1.1; the upstream protocol is this proxy's private
	// business and must not leak into the framing it answers with.
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	s.actions = &actionsProxy{
		service: s, ca: ca, leaf: leaf, upstream: transport,
	}

	return s.actions, nil
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

func (s *CacheService) authenticateProxy(
	ctx context.Context,
	r *http.Request,
) (*cacheSession, bool) {
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
	if err := lockCacheSession(ctx, session); err != nil {
		return nil, false
	}
	allowed := session.intercept && !session.closed
	session.mu.Unlock()

	return session, allowed
}

func (p *actionsProxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	session, ok := p.service.authenticateProxy(r.Context(), r)
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
	if host != actionsResultsHost || port != "443" {
		http.Error(w, "only the Actions results origin is available", http.StatusForbidden)

		return
	}

	p.intercept(r.Context(), w, session)
}

func hijack(w http.ResponseWriter) (net.Conn, *bufio.ReadWriter, error) {
	h, ok := w.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("HTTP server does not support connection hijacking")
	}

	return h.Hijack()
}

func (p *actionsProxy) intercept(
	ctx context.Context,
	w http.ResponseWriter,
	session *cacheSession,
) {
	client, buffered, err := hijack(w)
	if err != nil {
		return
	}
	defer client.Close()
	// The listener's ordinary request deadline remains attached after Hijack.
	// Clear it before the TLS handshake and let the bounded handshake/request
	// contexts below govern this connection.
	if err := client.SetDeadline(time.Time{}); err != nil {
		return
	}

	if _, err := buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffered.Flush(); err != nil {
		return
	}

	tlsConn := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{p.leaf}, MinVersion: tls.VersionTLS13,
	})
	handshakeCtx, cancel := context.WithTimeout(ctx, actionsConnectWait)
	defer cancel()
	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		return
	}

	reader := bufio.NewReader(&actionsHeaderReader{reader: tlsConn, remaining: actionsHeaderLimit})
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
	resp, err := p.roundTrip(req, session)
	if err != nil {
		resp = &http.Response{
			StatusCode: http.StatusBadGateway,
			ProtoMajor: 1,
			ProtoMinor: 1,
			Header:     http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
			Body:       io.NopCloser(strings.NewReader("upstream results service unavailable\n")),
			Request:    req,
		}
	}
	defer resp.Body.Close()
	// One request per intercepted tunnel keeps an upstream framing mistake or
	// partially consumed body from contaminating the next results operation.
	resp.Close = true
	// WRITTEN AS HTTP/1.1 WHATEVER THE UPSTREAM SPOKE. Response.Write renders
	// the status line from these fields, and a response that arrived over h2
	// would otherwise reach the guest's HTTP/1.1 reader as `HTTP/2.0 200 OK` —
	// a status line the official runner rejects outright. The transport above
	// no longer negotiates h2, so this is the backstop that keeps a future
	// transport change from silently reintroducing the mismatch.
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	if err := resp.Write(tlsConn); err != nil {
		return
	}
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

func (p *actionsProxy) roundTrip(req *http.Request, session *cacheSession) (*http.Response, error) {
	var replay []byte
	if actionsLocalRequest(req) && (req.URL.Path == actionsCreatePath ||
		req.URL.Path == actionsFinalizePath || req.URL.Path == actionsDownloadPath) {
		var err error
		replay, err = io.ReadAll(io.LimitReader(req.Body, actionsRequestLimit+1))
		if err != nil {
			return nil, err
		}
		if len(replay) > actionsRequestLimit {
			return actionsBlobError(http.StatusRequestEntityTooLarge,
				"Actions cache metadata request is too large"), nil
		}
		req.Body = io.NopCloser(bytes.NewReader(replay))
	}
	reservationBound := strings.HasPrefix(req.URL.Path, actionsBlobPrefix)
	if req.URL.Path == actionsFinalizePath && replay != nil {
		var err error
		reservationBound, err = p.service.actionsFinalizeReserved(req.Context(), replay, session)
		if err != nil {
			return nil, err
		}
		if response, found, err := p.service.actionsFinalizeReceipt(req.Context(), replay, session); found {
			if err != nil {
				p.service.log.Warn("a reserved Actions cache finalization failed locally",
					"instance", session.instance, "path", req.URL.Path, "error", err)

				return actionsBlobError(http.StatusBadGateway,
					"Actions cache storage is unavailable"), nil
			}
			response.Request = req

			return response, nil
		}
	}
	if response, handled, err := p.service.actionsResponse(req, session); handled && err == nil {
		response.Request = req

		return response, nil
	} else if handled && err != nil {
		if reservationBound {
			p.service.log.Warn("a reserved Actions cache request failed locally",
				"instance", session.instance, "path", req.URL.Path, "error", err)

			return actionsBlobError(http.StatusBadGateway, "Actions cache storage is unavailable"), nil
		}
		p.service.log.Warn("Actions cache interception failed; retrying through GitHub",
			"instance", session.instance, "path", req.URL.Path, "error", err)
		if replay != nil {
			req.Body = io.NopCloser(bytes.NewReader(replay))
		}
	} else if reservationBound {
		p.service.log.Warn("a reserved Actions cache request cannot be passed to GitHub",
			"instance", session.instance, "path", req.URL.Path)

		return actionsBlobError(http.StatusBadGateway, "Actions cache reservation is unavailable"), nil
	}

	request := req.Clone(req.Context())
	request.RequestURI = ""
	request.URL.Scheme = "https"
	request.URL.Host = actionsResultsHost
	request.Host = actionsResultsHost
	request.Header.Del("Proxy-Authorization")

	return p.upstream.RoundTrip(request)
}
