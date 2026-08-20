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
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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

	var upstreamAuthorization string
	var upstreamRequests int
	service.actions.upstream = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		upstreamRequests++
		upstreamAuthorization = r.Header.Get("Authorization")
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"artifact":"upstream"}`)),
			Request:    r,
		}, nil
	})

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
	if upstreamAuthorization != "Bearer opaque-github-runtime-token" {
		t.Fatalf("upstream authorization = %q; the GitHub token was changed or decoded", upstreamAuthorization)
	}

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
	if upstreamRequests != 1 {
		t.Fatalf("upstream requests = %d; the exact CacheService method was not intercepted", upstreamRequests)
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

func TestActionsProxyPassesBuildKitMetadataUpstreamWithoutConsumingIt(t *testing.T) {
	t.Parallel()

	service, _, session, _ := testActionsService(t)
	var upstreamBody, upstreamUserAgent string
	service.actions.upstream = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		upstreamBody = string(body)
		upstreamUserAgent = r.UserAgent()

		return &http.Response{
			StatusCode: http.StatusOK, Header: make(http.Header), Body: http.NoBody, Request: r,
		}, nil
	})
	request := actionsRequestForTest(t, http.MethodPost,
		"https://"+actionsResultsHost+actionsCreatePath,
		`{"key":"buildkit","version":"v1"}`)
	request.Header.Set("User-Agent", "buildkit/v0.25")
	response, err := service.actions.roundTrip(request, session)
	if err != nil {
		t.Fatalf("roundTrip: %v", err)
	}
	response.Body.Close()
	if upstreamBody != `{"key":"buildkit","version":"v1"}` ||
		upstreamUserAgent != "buildkit/v0.25" {
		t.Fatalf("upstream body=%q user-agent=%q", upstreamBody, upstreamUserAgent)
	}
}

// THE TUNNEL'S STATUS LINE MUST SAY HTTP/1.1 WHATEVER THE UPSTREAM SPOKE. The
// guest side of the tunnel is the official runner's strict HTTP/1.1 reader, and
// it refuses `HTTP/2.0 200 OK` outright — measured live: with GitHub's edge
// negotiating h2, every passthrough call the runner itself makes (step updates,
// log uploads, artifacts) failed with "Received an invalid status line" while
// local cache responses kept working. Go's own client parses any HTTP/x.y, which
// is exactly why the end-to-end test above could not see this: the status line is
// read raw here, with no tolerant client in between.
func TestPassthroughAnswersHTTP11WhateverTheUpstreamNegotiated(t *testing.T) {
	t.Parallel()

	service, err := NewCacheService("http://127.0.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.PrepareScoped("billet-lease-h2", CacheSessionScope{
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

	// The shape Go's transport produces when the far end negotiated h2.
	service.actions.upstream = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			Status: "200 OK", StatusCode: http.StatusOK,
			Proto: "HTTP/2.0", ProtoMajor: 2, ProtoMinor: 0,
			Header:  http.Header{"Content-Type": []string{"application/json"}},
			Body:    io.NopCloser(strings.NewReader(`{"artifact":"upstream"}`)),
			Request: r,
		}, nil
	})

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

	body := `{"name":"release"}`
	request := "POST /twirp/github.actions.results.api.v1.ArtifactService/CreateArtifact HTTP/1.1\r\n" +
		"Host: " + actionsResultsHost + "\r\n" +
		"Content-Type: application/json\r\n" +
		"Content-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	if _, err := tlsConn.Write([]byte(request)); err != nil {
		t.Fatalf("write the tunneled request: %v", err)
	}

	statusLine, err := bufio.NewReader(tlsConn).ReadString('\n')
	if err != nil {
		t.Fatalf("read the tunneled status line: %v", err)
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") {
		t.Fatalf("the tunnel answered %q; the official runner refuses any status line "+
			"that is not HTTP/1.1", strings.TrimSpace(statusLine))
	}
}
