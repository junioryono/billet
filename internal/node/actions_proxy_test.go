package node

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/provider"
)

func TestResultsProxyAuthenticatesByTheBoundGuestSession(t *testing.T) {
	t.Parallel()

	service, err := NewCacheService("http://127.0.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.PrepareScoped("billet-lease-1", CacheSessionScope{
		Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	service.actionIO = &fakeActionsVolumeManager{}
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))

	upstream := newFakeResultsUpstream(t, answerRequests(
		"HTTP/1.1 202 Accepted\r\nContent-Type: application/json\r\n"+
			"Content-Length: 23\r\n\r\n{\"artifact\":\"upstream\"}"))
	service.actions.dialUpstream = upstream.dial

	proxyServer := httptest.NewServer(service)
	t.Cleanup(proxyServer.Close)
	proxyURL, err := url.Parse(proxyServer.URL)
	if err != nil {
		t.Fatalf("parse proxy URL: %v", err)
	}
	proxyURL.User = url.UserPassword(actionsProxyUser, credentials.Token)

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(credentials.ActionsCAPEM)) {
		t.Fatal("the guest interception CA was not parseable")
	}
	client := &http.Client{Transport: &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13,
			RootCAs:    roots,
			ServerName: actionsResultsHost,
		},
	}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://"+actionsResultsHost+"/twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact",
		bytes.NewBufferString(`{"name":"release"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer opaque-github-runtime-token")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("artifact request through proxy: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	if resp.StatusCode != http.StatusAccepted || !bytes.Contains(body, []byte("upstream")) {
		t.Fatalf("artifact passthrough = %d %s", resp.StatusCode, body)
	}
	if authorization := upstream.header("Authorization"); authorization != "Bearer opaque-github-runtime-token" {
		t.Fatalf("upstream authorization = %q; the GitHub token was changed or decoded", authorization)
	}
	// A spliced tunnel persists; the local cache request below must classify on a
	// fresh connection the way every real toolkit process does.
	client.CloseIdleConnections()

	local, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath,
		bytes.NewBufferString(`{"key":"linux-go","version":"v1"}`))
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	local.Header.Set("Content-Type", "application/json")
	local.Header.Set("User-Agent", actionsUserAgent+"test")
	local.Header.Set("Authorization", "Bearer another-opaque-token")
	localResponse, err := client.Do(local)
	if err != nil {
		t.Fatalf("cache request through proxy: %v", err)
	}
	localBody, readErr := io.ReadAll(localResponse.Body)
	localResponse.Body.Close()
	if readErr != nil || localResponse.StatusCode != http.StatusOK ||
		!bytes.Contains(localBody, []byte(`"ok":true`)) {
		t.Fatalf("local cache response = %d %s, error=%v",
			localResponse.StatusCode, localBody, readErr)
	}
	if got := upstream.count(); got != 1 {
		t.Fatalf("upstream requests = %d; the exact CacheService method was not intercepted", got)
	}

	badProxy := *proxyURL
	badProxy.User = url.UserPassword(actionsProxyUser, strings.Repeat("0", 64))
	badClient := &http.Client{Transport: &http.Transport{
		Proxy:           http.ProxyURL(&badProxy),
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots},
	}}
	badReq, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		"https://"+actionsResultsHost+"/", http.NoBody)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	badResponse, err := badClient.Do(badReq)
	if badResponse != nil {
		badResponse.Body.Close()
	}
	if err == nil {
		t.Fatal("a guest without the bound session capability entered the results proxy")
	}

	other := httptest.NewRequestWithContext(t.Context(), http.MethodConnect,
		"http://github.com:443", http.NoBody)
	other.Host = "github.com:443"
	credential := base64.StdEncoding.EncodeToString([]byte(actionsProxyUser + ":" + credentials.Token))
	other.Header.Set("Proxy-Authorization", "Basic "+credential)
	recorder := httptest.NewRecorder()
	service.actions.serveConnect(recorder, other)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("non-results CONNECT status = %d, want %d", recorder.Code, http.StatusForbidden)
	}
}

func TestInterceptScopeRejectsMissingAssignmentIdentity(t *testing.T) {
	t.Parallel()

	service, err := NewCacheService("http://127.0.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	for name, scope := range map[string]CacheSessionScope{
		"owner":        {Trust: provider.TrustTrusted, Intercept: true, Repository: "api", WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main"},
		"repository":   {Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main"},
		"workflow ref": {Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api"},
		"workflow ref without revision": {
			Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
			WorkflowRef: "acme/api/.github/workflows/ci.yml",
		},
		"workflow ref without workflow": {
			Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
			WorkflowRef: "@refs/heads/main",
		},
		"oversized": {
			Trust: provider.TrustTrusted, Intercept: true, Owner: strings.Repeat("a", 101),
			Repository: "api", WorkflowRef: strings.Repeat("a", 101) + "/api/ci.yml@refs/heads/main",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := service.PrepareScoped("billet-"+strings.ReplaceAll(name, " ", "-"), scope); err == nil {
				t.Fatal("interception was enabled without an authenticated repository scope")
			}
		})
	}
}

func TestInterceptScopeAcceptsCrossRepositoryReusableWorkflow(t *testing.T) {
	t.Parallel()

	service, err := NewCacheService("http://127.0.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}

	credentials, err := service.PrepareScoped("billet-reusable-workflow", CacheSessionScope{
		Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
		WorkflowRef: "shared/ci/.github/workflows/cache.yml@refs/tags/v1",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	if credentials.ActionsProxy == "" || credentials.ActionsCAPEM == "" {
		t.Fatal("a cross-repository reusable workflow did not receive interception credentials")
	}
}

func TestActionsProxyBoundsOnlyTheRequestHeaders(t *testing.T) {
	t.Parallel()

	accepted := strings.Repeat("x", actionsHeaderLimit-4) + "\r\n\r\n" +
		strings.Repeat("b", actionsHeaderLimit)
	if _, err := io.ReadAll(&actionsHeaderReader{
		reader: strings.NewReader(accepted), remaining: actionsHeaderLimit,
	}); err != nil {
		t.Fatalf("an exact-limit header with a larger body was refused: %v", err)
	}

	rejected := strings.Repeat("x", actionsHeaderLimit-3) + "\r\n\r\n"
	if _, err := io.ReadAll(&actionsHeaderReader{
		reader: strings.NewReader(rejected), remaining: actionsHeaderLimit,
	}); err == nil {
		t.Fatal("a request header larger than 64KiB was accepted")
	}
}

func TestActionsProxyAuthenticationStopsWhenTheSessionIsBusyAndContextEnds(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	session.mu.Lock()
	defer session.mu.Unlock()
	request := httptest.NewRequestWithContext(t.Context(), http.MethodConnect,
		"http://"+actionsResultsHost+":443", http.NoBody)
	request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString(
		[]byte(actionsProxyUser+":"+session.token)))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, ok := service.authenticateProxy(ctx, request); ok {
		t.Fatal("canceled proxy authentication waited through a busy cache session")
	}
}

// BuildKit's type=gha client speaks the same three twirp methods but is not the
// official toolkit, so it stays on GitHub — and its request body must reach
// GitHub intact through the splice, not be consumed by the classification. This
// drives the real tunnel so the body is asserted on the far side, byte for byte,
// where a captured-but-not-replayed request would show as an empty upstream body.
func TestActionsProxyPassesBuildKitMetadataUpstreamThroughTheSplice(t *testing.T) {
	t.Parallel()

	bodies := make(chan string, 1)
	upstream, tlsConn := tunneledActionsService(t, func(u *fakeResultsUpstream, conn net.Conn) {
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		u.record(request)
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return
		}
		bodies <- string(body)
		//nolint:errcheck // the guest reads the answer or the test's deadline fails it
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	})

	body := `{"key":"buildkit","version":"v1"}`
	request := "POST " + actionsCreatePath + " HTTP/1.1\r\n" +
		"Host: " + actionsResultsHost + "\r\n" +
		"User-Agent: buildkit/v0.25\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	if _, err := tlsConn.Write([]byte(request)); err != nil {
		t.Fatalf("write the tunneled BuildKit request: %v", err)
	}

	if reader := bufio.NewReader(tlsConn); func() bool {
		line, err := reader.ReadString('\n')

		return err != nil || line != "HTTP/1.1 200 OK\r\n"
	}() {
		t.Fatal("the BuildKit request was not answered by GitHub through the splice")
	}
	if got := <-bodies; got != body {
		t.Fatalf("upstream body = %q, want %q", got, body)
	}
	if upstream.header("User-Agent") != "buildkit/v0.25" {
		t.Fatalf("upstream user-agent = %q", upstream.header("User-Agent"))
	}
}

// A GUEST THAT COMPLETES TLS WITH THE RESULTS SNI STILL CANNOT ADDRESS ANOTHER
// HOST at the HTTP layer: an absolute-form target or a foreign Host header is
// refused locally, restoring the single-origin invariant the round trip used to
// enforce by rewriting Host.
func TestPassthroughRefusesAForeignInnerHost(t *testing.T) {
	t.Parallel()

	for name, request := range map[string]string{
		"foreign host header": "GET /twirp/x HTTP/1.1\r\nHost: evil.example.com\r\n\r\n",
		"absolute-form target": "GET https://evil.example.com/twirp/x HTTP/1.1\r\n" +
			"Host: evil.example.com\r\n\r\n",
		"foreign host with results port": "GET /twirp/x HTTP/1.1\r\n" +
			"Host: evil.example.com:443\r\n\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			reached := make(chan struct{}, 1)
			_, tlsConn := tunneledActionsService(t, func(_ *fakeResultsUpstream, conn net.Conn) {
				reached <- struct{}{}
				_ = conn.Close()
			})

			if _, err := tlsConn.Write([]byte(request)); err != nil {
				t.Fatalf("write the misdirected request: %v", err)
			}
			statusLine, err := bufio.NewReader(tlsConn).ReadString('\n')
			if err != nil || !strings.Contains(statusLine, " 421 ") {
				t.Fatalf("misdirected request answered %q, error=%v; want a local 421",
					strings.TrimSpace(statusLine), err)
			}
			select {
			case <-reached:
				t.Fatal("a foreign inner host was spliced to GitHub rather than refused locally")
			default:
			}
		})
	}
}

// A HOSTILE GUEST THAT NEVER CLOSES ITS WRITE SIDE MUST NOT PIN THE HANDLER once
// GitHub has finished. Before the fix a half-close left the guest→upstream copy
// blocked forever; the splice now tears down both directions when either ends.
func TestPassthroughDoesNotLeakWhenGitHubFinishesFirst(t *testing.T) {
	t.Parallel()

	_, tlsConn := tunneledActionsService(t, func(u *fakeResultsUpstream, conn net.Conn) {
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		u.record(request)
		// Answer, then close — GitHub is done, but the guest below never will be.
		//nolint:errcheck // best effort; the test fails on a leak, not on this write
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nConnection: close\r\nContent-Length: 0\r\n\r\n"))
		_ = conn.Close()
	})

	if _, err := tlsConn.Write([]byte("GET /twirp/x HTTP/1.1\r\nHost: " +
		actionsResultsHost + "\r\n\r\n")); err != nil {
		t.Fatalf("write the request: %v", err)
	}

	// The guest deliberately never closes its write side. If the splice half-closed
	// and waited, this read would block until the tunnel's own deadline; the fix
	// tears the guest connection down when GitHub closes, so the read returns
	// PROMPTLY. A read that runs to the deadline is the leak reproducing.
	drained := make(chan error, 1)

	go func() {
		_, err := io.Copy(io.Discard, tlsConn)
		drained <- err
	}()

	select {
	case <-drained:
		// EOF or a torn-down connection — either way the tunnel finished.
	case <-time.After(3 * time.Second):
		t.Fatal("the splice did not finish after GitHub closed; the hostile guest pinned it")
	}
}

// fakeResultsUpstream stands in for GitHub's edge on the far side of a splice.
//
// A PLAIN TCP LISTENER, NOT TLS, because the dial seam it replaces returns an
// already-established connection: the splice treats upstream as opaque bytes,
// so the fake exercises exactly what production exercises.
type fakeResultsUpstream struct {
	ln net.Listener

	mu       sync.Mutex
	requests []*http.Request
}

func newFakeResultsUpstream(t *testing.T, handler func(*fakeResultsUpstream, net.Conn)) *fakeResultsUpstream {
	t.Helper()

	var lc net.ListenConfig

	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the fake upstream: %v", err)
	}

	upstream := &fakeResultsUpstream{ln: ln}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}

			go func() {
				defer conn.Close()
				handler(upstream, conn)
			}()
		}
	}()

	return upstream
}

func (u *fakeResultsUpstream) dial(ctx context.Context) (net.Conn, error) {
	var dialer net.Dialer

	return dialer.DialContext(ctx, "tcp", u.ln.Addr().String())
}

func (u *fakeResultsUpstream) record(request *http.Request) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.requests = append(u.requests, request)
}

func (u *fakeResultsUpstream) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()

	return len(u.requests)
}

// header returns a header of the first recorded request.
func (u *fakeResultsUpstream) header(name string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.requests) == 0 {
		return ""
	}

	return u.requests[0].Header.Get(name)
}

// answerRequests serves parsed HTTP/1.1 requests with one canned raw response,
// keep-alive, recording each request it saw.
func answerRequests(response string) func(*fakeResultsUpstream, net.Conn) {
	return func(u *fakeResultsUpstream, conn net.Conn) {
		reader := bufio.NewReader(conn)
		for {
			request, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			if _, err := io.Copy(io.Discard, request.Body); err != nil {
				return
			}
			u.record(request)
			if _, err := conn.Write([]byte(response)); err != nil {
				return
			}
		}
	}
}

// tunneledActionsService builds a service with a fake upstream, an authorized
// session, and an open TLS connection through the CONNECT tunnel.
func tunneledActionsService(
	t *testing.T,
	handler func(*fakeResultsUpstream, net.Conn),
) (*fakeResultsUpstream, *tls.Conn) {
	t.Helper()

	service, err := NewCacheService("http://127.0.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.PrepareScoped("billet-lease-splice", CacheSessionScope{
		Trust: provider.TrustTrusted, Intercept: true, Owner: "acme", Repository: "api",
		WorkflowRef: "acme/api/.github/workflows/ci.yml@refs/heads/main",
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	service.actionIO = &fakeActionsVolumeManager{}
	service.SetActionsPolicy(actionsPolicyFunc(func(context.Context, string, string) (bool, error) {
		return true, nil
	}))

	upstream := newFakeResultsUpstream(t, handler)
	service.actions.dialUpstream = upstream.dial

	proxyServer := httptest.NewServer(service)
	t.Cleanup(proxyServer.Close)

	var dialer net.Dialer

	conn, err := dialer.DialContext(t.Context(), "tcp",
		strings.TrimPrefix(proxyServer.URL, "http://"))
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	authority := actionsResultsHost + ":443"
	credential := base64.StdEncoding.EncodeToString(
		[]byte(actionsProxyUser + ":" + credentials.Token))
	if _, err := conn.Write([]byte("CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority +
		"\r\nProxy-Authorization: Basic " + credential + "\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}

	reader := bufio.NewReader(conn)
	connectLine, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(connectLine, " 200 ") {
		t.Fatalf("CONNECT answered %q, error=%v", connectLine, err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read CONNECT headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(credentials.ActionsCAPEM)) {
		t.Fatal("the guest interception CA was not parseable")
	}
	tlsConn := tls.Client(conn, &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: actionsResultsHost,
	})
	if err := tlsConn.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("TLS handshake through the tunnel: %v", err)
	}

	// A BROKEN SPLICE MUST FAIL, NOT HANG. Without a deadline, a splice that
	// never forwards the request leaves every read below blocked until the test
	// binary is killed, which reports nothing.
	if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the tunnel deadline: %v", err)
	}

	return upstream, tlsConn
}

// THE SPLICE CARRIES GITHUB'S OWN BYTES, so the status line the guest reads is
// the one GitHub wrote. This is what structurally retired the previous defect:
// a round-tripped passthrough re-framed upstream h2 responses as
// `HTTP/2.0 200 OK` on an HTTP/1.1 connection, which the official runner
// refuses. The status line is read raw here, with no tolerant client between.
func TestPassthroughSplicesGitHubsOwnBytesToTheGuest(t *testing.T) {
	t.Parallel()

	upstream, tlsConn := tunneledActionsService(t, answerRequests(
		"HTTP/1.1 200 OK\r\nContent-Type: application/json\r\n"+
			"Content-Length: 23\r\n\r\n{\"artifact\":\"upstream\"}"))

	body := `{"name":"release"}`
	request := "POST /twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact HTTP/1.1\r\n" +
		"Host: " + actionsResultsHost + "\r\n" +
		"Authorization: Bearer opaque-runtime-token\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	if _, err := tlsConn.Write([]byte(request)); err != nil {
		t.Fatalf("write the tunneled request: %v", err)
	}

	reader := bufio.NewReader(tlsConn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the tunneled status line: %v", err)
	}
	if statusLine != "HTTP/1.1 200 OK\r\n" {
		t.Fatalf("the tunnel answered %q; the guest must read the bytes GitHub wrote",
			strings.TrimSpace(statusLine))
	}
	if authorization := upstream.header("Authorization"); authorization != "Bearer opaque-runtime-token" {
		t.Fatalf("upstream authorization = %q; the GitHub token was changed or decoded", authorization)
	}

	// AND THE TUNNEL PERSISTS: the runner's client keeps a passthrough connection
	// alive, so a second request on the same tunnel must reach GitHub too. The
	// status line was already consumed, so the rest of the first response is
	// drained by hand: headers to the blank line, then the declared body.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read the first response headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}
	if _, err := io.ReadFull(reader, make([]byte, 23)); err != nil {
		t.Fatalf("drain the first response body: %v", err)
	}
	if _, err := tlsConn.Write([]byte("GET /twirp/results.services.receiver.Receiver/ping HTTP/1.1\r\n" +
		"Host: " + actionsResultsHost + "\r\n\r\n")); err != nil {
		t.Fatalf("write the second tunneled request: %v", err)
	}
	if secondLine, err := reader.ReadString('\n'); err != nil || secondLine != "HTTP/1.1 200 OK\r\n" {
		t.Fatalf("second request over the spliced tunnel answered %q, error=%v",
			strings.TrimSpace(secondLine), err)
	}
	if got := upstream.count(); got != 2 {
		t.Fatalf("upstream requests = %d, want both requests on the persistent tunnel", got)
	}
}

// THE RUNNER'S LIVE LOGS RIDE A WEBSOCKET TO THE RESULTS HOST — GitHub's own
// runner documentation requires wss to results-receiver — and a passthrough
// that answers one HTTP response per connection kills the upgrade the moment it
// completes. Measured live: every job's log pane was blank because the live-log
// feed died against the tunnel. The splice must carry the upgrade and then the
// frames, both directions.
func TestPassthroughSplicesTheRunnersLiveLogWebSocket(t *testing.T) {
	t.Parallel()

	upstream, tlsConn := tunneledActionsService(t,
		func(u *fakeResultsUpstream, conn net.Conn) {
			reader := bufio.NewReader(conn)
			request, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			u.record(request)
			if request.Header.Get("Upgrade") != "websocket" {
				return
			}
			if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n" +
				"Upgrade: websocket\r\nConnection: Upgrade\r\n\r\n")); err != nil {
				return
			}
			// Echo whatever frames arrive, byte for byte.
			if _, err := io.Copy(conn, reader); err != nil {
				return
			}
		})

	if _, err := tlsConn.Write([]byte("GET /twirp/results.services.receiver.Receiver/feed HTTP/1.1\r\n" +
		"Host: " + actionsResultsHost + "\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: c3BsaWNlLXRlc3Q=\r\nSec-WebSocket-Version: 13\r\n\r\n")); err != nil {
		t.Fatalf("write the upgrade request: %v", err)
	}

	reader := bufio.NewReader(tlsConn)
	statusLine, err := reader.ReadString('\n')
	if err != nil || statusLine != "HTTP/1.1 101 Switching Protocols\r\n" {
		t.Fatalf("upgrade answered %q, error=%v", strings.TrimSpace(statusLine), err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read upgrade headers: %v", err)
		}
		if line == "\r\n" {
			break
		}
	}

	frame := []byte("live-log-frame-bytes")
	if _, err := tlsConn.Write(frame); err != nil {
		t.Fatalf("write a frame through the upgraded tunnel: %v", err)
	}

	echoed := make([]byte, len(frame))
	if _, err := io.ReadFull(reader, echoed); err != nil {
		t.Fatalf("read the echoed frame: %v", err)
	}
	if !bytes.Equal(echoed, frame) {
		t.Fatalf("echoed frame = %q, want %q", echoed, frame)
	}
	if got := upstream.count(); got != 1 {
		t.Fatalf("upstream requests = %d, want the one upgrade", got)
	}
}

// The splice copies the guest's HTTP/1.1 bytes verbatim, so the upstream TLS
// handshake must never negotiate a protocol those bytes are not.
func TestUpstreamDialPinsHTTP11(t *testing.T) {
	t.Parallel()

	config := actionsUpstreamTLSConfig()
	if len(config.NextProtos) != 1 || config.NextProtos[0] != "http/1.1" {
		t.Fatalf("upstream ALPN = %v, want exactly http/1.1", config.NextProtos)
	}
	if config.ServerName != actionsResultsHost {
		t.Fatalf("upstream server name = %q", config.ServerName)
	}
}
