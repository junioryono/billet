package node

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/durablefile"
	"github.com/junioryono/billet/internal/provider"
)

// fakeRegistry is the registry an npm cache test talks to: it records what each
// request carried and answers with what the test scripts.
type fakeRegistry struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []*http.Request
	answer   func(w http.ResponseWriter, r *http.Request, n int)
}

func newFakeRegistry(t *testing.T, answer func(w http.ResponseWriter, r *http.Request, n int)) *fakeRegistry {
	t.Helper()

	registry := &fakeRegistry{answer: answer}
	registry.server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		registry.mu.Lock()
		registry.requests = append(registry.requests, r.Clone(t.Context()))
		n := len(registry.requests)
		answer := registry.answer
		registry.mu.Unlock()
		answer(w, r, n)
	}))
	t.Cleanup(registry.server.Close)

	return registry
}

func (r *fakeRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return len(r.requests)
}

func (r *fakeRegistry) request(i int) *http.Request {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.requests[i]
}

type cachePolicyFunc func(ctx context.Context, kind config.CacheKind, owner, repository string) (bool, error)

func (f cachePolicyFunc) CacheAllowed(ctx context.Context, kind config.CacheKind, owner, repository string,
) (bool, error) {
	return f(ctx, kind, owner, repository)
}

// npmService is a cache service whose one session enables the npm cache, its
// registry the fake, and its waits short enough for a test.
func npmService(t *testing.T, registry *fakeRegistry) (*CacheService, CacheCredentials) {
	t.Helper()

	return npmServiceWith(t, registry, nil, CacheSessionScope{
		Trust: provider.TrustUntrusted, Cache: &config.CacheSpec{Npm: true},
	})
}

// npmServiceWith is npmService with a kill switch, consulted when the scope
// names a repository, and a scope of the test's own.
func npmServiceWith(t *testing.T, registry *fakeRegistry, policy CachePolicy, scope CacheSessionScope,
) (*CacheService, CacheCredentials) {
	t.Helper()

	service, err := NewCacheService("http://127.0.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	if policy != nil {
		service.SetCachePolicy(policy)
	}
	credentials, err := service.PrepareScoped("billet-lease-npm", scope)
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	if credentials.NpmProxy == "" || credentials.ActionsCAPEM == "" || credentials.ActionsProxy != "" {
		t.Fatalf("an npm-only session's credentials = %+v", credentials)
	}
	npm := service.npmCache()
	npm.origin = registry.server.URL
	transport, ok := registry.server.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatalf("the test registry's client transport is a %T", registry.server.Client().Transport)
	}
	pooled := transport.Clone()
	pooled.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	fresh := pooled.Clone()
	fresh.DisableKeepAlives = true
	npm.pooled = &http.Client{Transport: pooled, CheckRedirect: npm.pooled.CheckRedirect}
	npm.fresh = &http.Client{Transport: fresh, CheckRedirect: npm.fresh.CheckRedirect}
	npm.dialUpstream = func(ctx context.Context) (net.Conn, error) {
		var dialer tls.Dialer
		dialer.Config = &tls.Config{InsecureSkipVerify: true}

		return dialer.DialContext(ctx, "tcp", strings.TrimPrefix(registry.server.URL, "https://"))
	}
	// GENEROUS BY DEFAULT, so an ordinary request under load is never an unintended
	// retry; a test about a timeout shortens the one it is about.
	npm.headerWait = 5 * time.Second
	npm.bodyIdle = 5 * time.Second

	return service, credentials
}

// npmTunnel opens a CONNECT tunnel to host through the service and completes
// TLS against the interception authority, as the guest's relay and client do.
func npmTunnel(t *testing.T, service *CacheService, credentials CacheCredentials, host string) (*tls.Conn, string) {
	t.Helper()

	proxyServer := httptest.NewServer(service)
	t.Cleanup(proxyServer.Close)
	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", strings.TrimPrefix(proxyServer.URL, "http://"))
	if err != nil {
		t.Fatalf("dial the proxy: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	authority := host + ":443"
	credential := base64.StdEncoding.EncodeToString([]byte(actionsProxyUser + ":" + credentials.Token))
	if _, err := conn.Write([]byte("CONNECT " + authority + " HTTP/1.1\r\nHost: " + authority +
		"\r\nProxy-Authorization: Basic " + credential + "\r\n\r\n")); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	reader := bufio.NewReader(conn)
	status, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read the CONNECT answer: %v", err)
	}
	for {
		line, err := reader.ReadString('\n')
		if err != nil || line == "\r\n" {
			break
		}
	}
	if !strings.Contains(status, " 200 ") {
		return nil, status
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM([]byte(credentials.ActionsCAPEM)) {
		t.Fatal("the interception authority is not parseable")
	}
	tlsConn := tls.Client(conn, &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: host})
	if err := tlsConn.HandshakeContext(t.Context()); err != nil {
		t.Fatalf("TLS through the tunnel as %s: %v", host, err)
	}
	if err := tlsConn.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set the tunnel deadline: %v", err)
	}

	return tlsConn, status
}

// npmAnswer is what a client read of one answer, its body already read whole.
type npmAnswer struct {
	StatusCode    int
	Header        http.Header
	Close         bool
	ContentLength int64
}

// npmGet sends one raw request through a new tunnel and reads the answer whole.
func npmGet(t *testing.T, service *CacheService, credentials CacheCredentials, raw string) (npmAnswer, string) {
	t.Helper()

	conn, status := npmTunnel(t, service, credentials, npmRegistryHost)
	if conn == nil {
		t.Fatalf("CONNECT to the registry was answered %q", status)
	}
	if _, err := conn.Write([]byte(raw)); err != nil {
		t.Fatalf("write the request: %v", err)
	}
	method := strings.Fields(raw)[0]
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("read the answer: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read the answer's body: %v", err)
	}

	return npmAnswer{StatusCode: resp.StatusCode, Header: resp.Header, Close: resp.Close,
		ContentLength: resp.ContentLength}, string(body)
}

// serveText and serveBytes write a scripted registry answer, reporting a write
// that failed rather than ending the handler's goroutine.
func serveText(t *testing.T, w http.ResponseWriter, body string) {
	t.Helper()

	if _, err := io.WriteString(w, body); err != nil {
		t.Errorf("the test registry's write: %v", err)
	}
}

func serveBytes(t *testing.T, w http.ResponseWriter, body []byte) {
	t.Helper()

	if _, err := w.Write(body); err != nil {
		t.Errorf("the test registry's write: %v", err)
	}
}

func npmRequestLine(path string, extra ...string) string {
	return "GET " + path + " HTTP/1.1\r\nHost: registry.npmjs.org\r\n" + strings.Join(extra, "") + "\r\n"
}

func gzipped(t *testing.T, body string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, err := writer.Write([]byte(body)); err != nil {
		t.Fatalf("gzip: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("gzip: %v", err)
	}

	return buffer.Bytes()
}

func noted(service *CacheService, credentials CacheCredentials) alloc.BuildCache {
	service.mu.Lock()
	session := service.byToken[credentials.Token]
	service.mu.Unlock()
	session.mu.Lock()
	defer session.mu.Unlock()

	return session.building[config.CacheNpm]
}

// A SECOND JOB'S FETCH NEVER LEAVES THE NODE: the first fills the store with the
// node's own canonical request, decoded, and the second is answered from it.
// This is #631's "done when" for one node.
func TestASecondFetchOfADocumentIsAnsweredFromTheStore(t *testing.T) {
	t.Parallel()

	const document = `{"name":"left-pad","versions":{}}`
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Content-Type", "application/vnd.npm.install-v1+json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("ETag", `"v1"`)
		w.Header().Set("Content-Encoding", "gzip")
		serveBytes(t, w, gzipped(t, document))
	})
	service, credentials := npmService(t, registry)

	request := npmRequestLine("/left-pad", "Accept: "+npmClientAbbreviatedAccept+"\r\n",
		"User-Agent: npm/11 node/v24\r\nnpm-session: abc\r\n")
	for i := range 2 {
		resp, body := npmGet(t, service, credentials, request)
		if resp.StatusCode != http.StatusOK || body != document || resp.Header.Get("Content-Encoding") != "" {
			t.Fatalf("fetch %d = %d %q (encoding %q)", i+1, resp.StatusCode, body,
				resp.Header.Get("Content-Encoding"))
		}
		if !resp.Close {
			t.Fatalf("fetch %d left the connection open for a request the node would not classify", i+1)
		}
	}
	if registry.count() != 1 {
		t.Fatalf("the registry was asked %d times for one document, want once", registry.count())
	}
	// THE FILL IS THE NODE'S OWN REQUEST: the variant's one Accept, gzip, and
	// none of the job's headers.
	fill := registry.request(0)
	if fill.Header.Get("Accept") != npmAbbreviatedAccept || fill.Header.Get("Accept-Encoding") != "gzip" ||
		fill.Header.Get("Npm-Session") != "" || strings.Contains(fill.UserAgent(), "npm/") {
		t.Fatalf("the fill carried %v", fill.Header)
	}
	if got := noted(service, credentials); got != alloc.BuildCacheWarm {
		t.Fatalf("the npm cache's outcome = %q, want warm", got)
	}
}

// A REQUEST CARRYING A CREDENTIAL IS THE REGISTRY'S, whole: the node splices the
// guest's own bytes, token and all, and keeps nothing of the answer.
func TestACredentialedRequestIsSplicedAndNeverStored(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, `{"private":true}`)
	})
	service, credentials := npmService(t, registry)

	resp, body := npmGet(t, service, credentials,
		npmRequestLine("/@acme%2fsecret", "Authorization: Bearer npm_token\r\n"))
	if resp.StatusCode != http.StatusOK || body != `{"private":true}` {
		t.Fatalf("a spliced request = %d %q", resp.StatusCode, body)
	}
	if registry.count() != 1 || registry.request(0).Header.Get("Authorization") != "Bearer npm_token" {
		t.Fatalf("the registry saw %d requests, the first carrying %q", registry.count(),
			registry.request(0).Header.Get("Authorization"))
	}
	if service.npmCache().store.held("full /@acme%2fsecret") {
		t.Fatal("an answer to a credentialed request was stored")
	}
	if _, body := npmGet(t, service, credentials, npmRequestLine("/@acme%2fsecret")); body != `{"private":true}` ||
		registry.count() != 2 {
		t.Fatalf("an anonymous read after a credentialed one was answered %q after %d requests",
			body, registry.count())
	}
}

// A STALLED ATTEMPT IS MADE ONCE MORE ON A NEW CONNECTION, before the client
// sees anything: the failure #631 records, a registry that does not answer.
func TestAnAttemptWithNoHeadersIsRetriedOnANewConnection(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}

			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, `{"name":"left-pad"}`)
	})
	service, credentials := npmService(t, registry)
	service.npmCache().headerWait = 300 * time.Millisecond

	resp, body := npmGet(t, service, credentials, npmRequestLine("/left-pad"))
	if resp.StatusCode != http.StatusOK || body != `{"name":"left-pad"}` || registry.count() != 2 {
		t.Fatalf("after a stalled attempt: %d %q, %d attempts", resp.StatusCode, body, registry.count())
	}
}

// WHEN BOTH ATTEMPTS GIVE NO ANSWER, a stale entry answers if it may; without
// one, the registry's last 5xx is relayed; without that, the node says 504.
func TestASilentRegistryIsAnsweredStaleOrFailedInOrder(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	mode := "fill"
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		mu.Lock()
		current := mode
		mu.Unlock()
		switch current {
		case "fill":
			w.Header().Set("Cache-Control", "public, max-age=0")
			w.Header().Set("ETag", `"v1"`)
			serveText(t, w, `{"name":"stale"}`)
		case "unavailable":
			w.WriteHeader(http.StatusServiceUnavailable)
			serveText(t, w, "down")
		}
	})
	service, credentials := npmService(t, registry)
	if _, body := npmGet(t, service, credentials, npmRequestLine("/stale")); body != `{"name":"stale"}` {
		t.Fatalf("the fill answered %q", body)
	}

	mu.Lock()
	mode = "unavailable"
	mu.Unlock()
	resp, body := npmGet(t, service, credentials, npmRequestLine("/stale"))
	if resp.StatusCode != http.StatusOK || body != `{"name":"stale"}` {
		t.Fatalf("an expired entry behind a 503 = %d %q, want the stale entry", resp.StatusCode, body)
	}
	if registry.count() != 3 {
		t.Fatalf("the registry was asked %d times, want the fill and two attempts", registry.count())
	}
	resp, body = npmGet(t, service, credentials, npmRequestLine("/missing"))
	if resp.StatusCode != http.StatusServiceUnavailable || body != "down" {
		t.Fatalf("a missing entry behind a 503 = %d %q, want the registry's 503", resp.StatusCode, body)
	}
}

func TestARegistryThatNeverAnswersIsA504(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		<-r.Context().Done()
	})
	service, credentials := npmService(t, registry)
	service.npmCache().headerWait = 300 * time.Millisecond

	resp, _ := npmGet(t, service, credentials, npmRequestLine("/left-pad"))
	if resp.StatusCode != http.StatusGatewayTimeout || registry.count() != 2 {
		t.Fatalf("a silent registry = %d after %d attempts, want 504 after 2", resp.StatusCode, registry.count())
	}
	if got := noted(service, credentials); got != alloc.BuildCacheUnavailable {
		t.Fatalf("the npm cache's outcome = %q, want unavailable", got)
	}
}

// A REVALIDATION ASKS WITH THE STORED VALIDATOR, and a 304 that withdraws
// shareability is never relayed: the node asks again without a validator and
// gives that answer to this client, storing nothing.
func TestARevalidationThatWithdrawsSharingIsAskedAgainAndNotStored(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	withdraw := false
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, n int) {
		mu.Lock()
		withdrawn := withdraw
		mu.Unlock()
		switch {
		case n == 1:
			w.Header().Set("Cache-Control", "public, max-age=0")
			w.Header().Set("ETag", `"v1"`)
			serveText(t, w, `{"v":1}`)
		case r.Header.Get("If-None-Match") == `"v1"` && withdrawn:
			w.Header().Set("Cache-Control", "private")
			w.WriteHeader(http.StatusNotModified)
		case r.Header.Get("If-None-Match") == `"v1"`:
			w.Header().Set("Cache-Control", "public, max-age=300")
			w.WriteHeader(http.StatusNotModified)
		default:
			w.Header().Set("Cache-Control", "private")
			serveText(t, w, `{"v":2}`)
		}
	})
	service, credentials := npmService(t, registry)
	npmGet(t, service, credentials, npmRequestLine("/doc"))

	resp, body := npmGet(t, service, credentials, npmRequestLine("/doc"))
	if resp.StatusCode != http.StatusOK || body != `{"v":1}` || registry.count() != 2 {
		t.Fatalf("a revalidated entry = %d %q after %d requests", resp.StatusCode, body, registry.count())
	}
	if registry.request(1).Header.Get("If-None-Match") != `"v1"` {
		t.Fatalf("the revalidation carried %q, want the stored ETag", registry.request(1).Header.Get("If-None-Match"))
	}
	store := service.npmCache().store
	ageNpmEntry(t, store, "full /doc", time.Hour)

	mu.Lock()
	withdraw = true
	mu.Unlock()
	resp, body = npmGet(t, service, credentials, npmRequestLine("/doc"))
	if resp.StatusCode != http.StatusOK || body != `{"v":2}` {
		t.Fatalf("after a withdrawing 304 = %d %q, want the unconditional answer", resp.StatusCode, body)
	}
	if last := registry.request(registry.count() - 1); last.Header.Get("If-None-Match") != "" {
		t.Fatalf("the second ask carried a validator %q", last.Header.Get("If-None-Match"))
	}
	if store.held("full /doc") {
		t.Fatal("an entry the registry withdrew from sharing is still stored")
	}
}

// A DEFINITIVE ANSWER REMOVES THE ENTRY and is relayed as it is.
func TestANotFoundRemovesTheEntry(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Cache-Control", "public, max-age=0")
			serveText(t, w, "{}")

			return
		}
		w.WriteHeader(http.StatusNotFound)
		serveText(t, w, `{"error":"Not found"}`)
	})
	service, credentials := npmService(t, registry)
	npmGet(t, service, credentials, npmRequestLine("/gone"))

	resp, body := npmGet(t, service, credentials, npmRequestLine("/gone"))
	if resp.StatusCode != http.StatusNotFound || body != `{"error":"Not found"}` || registry.count() != 2 {
		t.Fatalf("a removed package = %d %q after %d requests", resp.StatusCode, body, registry.count())
	}
	if service.npmCache().store.held("full /gone") {
		t.Fatal("an entry the registry no longer has is still stored")
	}
}

// A TARBALL IS STREAMED AS IT ARRIVES AND KEPT ONLY WHOLE: a cut transfer fails
// that one client and leaves nothing a later job would be served.
func TestATarballIsKeptOnlyWhenItArrivesWhole(t *testing.T) {
	t.Parallel()

	tarball := strings.Repeat("t", 64<<10)
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("a tarball fill asked for %q, want identity", r.Header.Get("Accept-Encoding"))
		}
		w.Header().Set("Cache-Control", "public, must-revalidate, max-age=31557600")
		w.Header().Set("Content-Length", "65536")
		if strings.Contains(r.URL.Path, "cut") {
			serveText(t, w, tarball[:1000])
			// FLUSHED, THEN CUT: the node has read part of a body it was promised
			// whole when the registry's connection ends.
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("the test registry cannot cut its connection")

				return
			}
			conn, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("cut the test registry's connection: %v", err)

				return
			}
			_ = conn.Close()

			return
		}
		serveText(t, w, tarball)
	})
	service, credentials := npmService(t, registry)

	resp, body := npmGet(t, service, credentials, npmRequestLine("/whole/-/whole-1.0.0.tgz"))
	if resp.StatusCode != http.StatusOK || body != tarball {
		t.Fatalf("a whole tarball = %d, %d bytes", resp.StatusCode, len(body))
	}
	// A TARBALL IS KEPT AFTER ITS LAST BYTE REACHED THE CLIENT, so the store is
	// read once the node has finished the request, not when the client has.
	waitForNpmFills(t, service.npmCache())
	if !service.npmCache().store.held(" /whole/-/whole-1.0.0.tgz") {
		t.Fatal("a whole tarball was not kept")
	}

	conn, _ := npmTunnel(t, service, credentials, npmRegistryHost)
	if _, err := conn.Write([]byte(npmRequestLine("/cut/-/cut-1.0.0.tgz"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	// THE ANSWER BEGAN AT THE REGISTRY'S HEADERS, so the client reads them, and
	// then a body that ends short.
	cut, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatalf("a tarball whose body was cut sent no headers: %v", err)
	}
	_, err = io.ReadAll(cut.Body)
	_ = cut.Body.Close()
	if cut.StatusCode != http.StatusOK || err == nil {
		t.Fatalf("a cut tarball = %d, read error %v, want a 200 whose body ends short", cut.StatusCode, err)
	}
	waitForNpmFills(t, service.npmCache())
	if service.npmCache().store.held(" /cut/-/cut-1.0.0.tgz") {
		t.Fatal("a cut tarball was kept")
	}
	if registry.count() != 2 {
		t.Fatalf("the registry saw %d requests, want the whole tarball and the cut one", registry.count())
	}
	assertNpmQuiet(t, service.npmCache())
}

// waitForNpmFills waits until no fill is in flight: serve releases its fill
// only after it has answered and kept, or discarded, what it fetched.
func waitForNpmFills(t *testing.T, npm *npmCache) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for {
		npm.mu.Lock()
		pending := len(npm.fills)
		npm.mu.Unlock()
		if pending == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d npm fills still in flight after 5s", pending)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A HEAD NEVER CREATES AN ENTRY, and a HEAD of one held is answered with the
// length a GET would get and no body.
func TestAHeadIsAnsweredOnlyFromAHeldEntry(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, `{"name":"held"}`)
	})
	service, credentials := npmService(t, registry)

	head := "HEAD /held HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n"
	npmGet(t, service, credentials, head)
	if service.npmCache().store.held("full /held") || registry.count() != 1 ||
		registry.request(0).Method != http.MethodHead {
		t.Fatalf("a cold HEAD: stored %v, registry saw %d requests", service.npmCache().store.held("full /held"),
			registry.count())
	}
	npmGet(t, service, credentials, npmRequestLine("/held"))
	resp, body := npmGet(t, service, credentials, head)
	if resp.StatusCode != http.StatusOK || resp.ContentLength != int64(len(`{"name":"held"}`)) || body != "" ||
		registry.count() != 2 {
		t.Fatalf("a held HEAD = %d, length %d, body %q, after %d requests", resp.StatusCode,
			resp.ContentLength, body, registry.count())
	}
}

// A COOKIE THE REGISTRY SETS IS THE NODE'S: npm's CDN sets one on every answer
// (measured 2026-10-22), so it confines nothing and the answer is kept, and it
// reaches neither the client that caused the fill nor one answered from the
// store.
func TestACookieTheRegistrySetsIsTheNodesAndReachesNoClient(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		if r.Header.Get("Cookie") != "" {
			t.Errorf("the node sent a cookie back to the registry: %q", r.Header.Get("Cookie"))
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Set-Cookie", "__cf_bm=node; HttpOnly; Secure; Path=/; Domain=npmjs.org")
		serveText(t, w, "{}")
	})
	service, credentials := npmService(t, registry)

	for i := range 2 {
		resp, body := npmGet(t, service, credentials, npmRequestLine("/cookie"))
		if resp.StatusCode != http.StatusOK || body != "{}" || resp.Header.Get("Set-Cookie") != "" {
			t.Fatalf("answer %d to a registry that sets a cookie = %d %q, cookie %q", i+1, resp.StatusCode, body,
				resp.Header.Get("Set-Cookie"))
		}
	}
	if registry.count() != 1 {
		t.Fatalf("the registry was asked %d times; an answer it set a cookie on was not kept", registry.count())
	}
	assertNpmQuiet(t, service.npmCache())
}

// EACH HOST IS ITS OWN SWITCH: an npm-only session cannot reach the results
// origin through the tunnel, and a session without the npm cache cannot reach
// the registry.
func TestASessionReachesOnlyTheOriginsItsTierIntercepts(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {})
	service, credentials := npmService(t, registry)
	if conn, status := npmTunnel(t, service, credentials, actionsResultsHost); conn != nil ||
		!strings.Contains(status, " 403 ") {
		t.Fatalf("an npm-only session's CONNECT to the results origin = %q", status)
	}

	actions, actionsCredentials := interceptService(t)
	if conn, status := npmTunnel(t, actions, actionsCredentials, npmRegistryHost); conn != nil ||
		!strings.Contains(status, " 403 ") {
		t.Fatalf("an Actions-only session's CONNECT to the registry = %q", status)
	}
	if actionsCredentials.NpmProxy != "" {
		t.Fatalf("an Actions-only session was given an npm proxy %q", actionsCredentials.NpmProxy)
	}
}

// THE KILL SWITCH SPLICES: a blocked repository's npm traffic reaches the
// registry uncached, and the outcome says the switch refused it.
func TestTheKillSwitchSplicesTheNpmCache(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, "{}")
	})
	service, err := NewCacheService("http://127.0.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	service.SetCachePolicy(cachePolicyFunc(func(_ context.Context, kind config.CacheKind, _, _ string) (bool, error) {
		return kind != config.CacheNpm, nil
	}))
	credentials, err := service.PrepareScoped("billet-lease-npm-blocked", CacheSessionScope{
		Trust: provider.TrustUntrusted, Owner: "acme", Repository: "api",
		Cache: &config.CacheSpec{Npm: true, Publish: config.CachePublishDefaultBranch, Owner: "acme",
			Repository: "api"},
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	npm := service.npmCache()
	npm.dialUpstream = func(ctx context.Context) (net.Conn, error) {
		var dialer tls.Dialer
		dialer.Config = &tls.Config{InsecureSkipVerify: true}

		return dialer.DialContext(ctx, "tcp", strings.TrimPrefix(registry.server.URL, "https://"))
	}

	if resp, _ := npmGet(t, service, credentials, npmRequestLine("/left-pad")); resp.StatusCode != http.StatusOK {
		t.Fatalf("a blocked repository's npm read = %d", resp.StatusCode)
	}
	if npm.store.held("full /left-pad") {
		t.Fatal("the kill switch's splice was stored")
	}
	if got := noted(service, credentials); got != alloc.BuildCacheDisabled {
		t.Fatalf("the npm cache's outcome = %q, want disabled", got)
	}
}

// holdBackTheBody pins the client socket's receive buffer small, so a body far
// larger than what the two sockets can hold keeps the node's write blocked until
// the client reads: Linux caps a send buffer at tcp_wmem's maximum (4 MiB on the
// shared test host, 2026-10-22), and an explicit receive buffer turns off the
// autotuning that could otherwise grow to tcp_rmem's 32 MiB.
func holdBackTheBody(t *testing.T, conn *tls.Conn) {
	t.Helper()

	raw, ok := conn.NetConn().(*net.TCPConn)
	if !ok {
		t.Fatalf("the tunnel is a %T, not a TCP connection", conn.NetConn())
	}
	if err := raw.SetReadBuffer(64 << 10); err != nil {
		t.Fatalf("pin the client's receive buffer: %v", err)
	}
}

// WHAT THE NPM CACHE DID IS REPORTED WITH THE SESSION'S OTHER CACHES when it
// ends, for the lease's history: warm once the store answered from what it
// held, unused when the tier had the cache and the guest never asked.
func TestTheNpmOutcomeIsReportedWhenTheSessionEnds(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		fetches int
		extra   string
		want    alloc.BuildCache
	}{
		"used, and answered from the store the second time": {fetches: 2, want: alloc.BuildCacheWarm},
		"enabled and never asked":                           {fetches: 0, want: alloc.BuildCacheUnused},
		// A REQUEST THE STORE NEVER ANSWERS is passed to the registry unread, and
		// does not ask the cache.
		"asked only what is passed to the registry": {fetches: 1, extra: "Authorization: Bearer npm_token\r\n",
			want: alloc.BuildCacheUnused},
	} {
		registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
			w.Header().Set("Cache-Control", "public, max-age=300")
			serveText(t, w, "{}")
		})
		service, credentials := npmService(t, registry)
		observer := &recordingObserver{}
		service.SetCacheObserver(observer)
		for range tc.fetches {
			npmGet(t, service, credentials, npmRequestLine("/reported", tc.extra))
		}
		assertNpmQuiet(t, service.npmCache())
		if err := service.Close(t.Context(), "billet-lease-npm"); err != nil {
			t.Fatalf("%s: Close: %v", name, err)
		}
		calls := observer.recorded()
		if len(calls) == 0 {
			t.Fatalf("%s: nothing was reported", name)
		}
		if got := calls[len(calls)-1].obs.Npm; got != tc.want {
			t.Errorf("%s: the npm cache reported %q, want %q", name, got, tc.want)
		}
	}
}

// A REQUEST STILL BEING ANSWERED WHEN THE JOB ENDS IS NOT "UNUSED": the guest
// asked, and what the store did for it is not known yet, so the session reports
// nothing for the npm cache rather than that it was never asked. Whether the
// request is waiting on the registry or on the kill switch.
func TestASessionThatEndsDuringARequestDoesNotCallTheNpmCacheUnused(t *testing.T) {
	t.Parallel()

	for _, held := range []string{"the registry", "the kill switch"} {
		reached, release := make(chan struct{}), make(chan struct{})
		var reachedOnce, releaseOnce sync.Once
		t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
		hold := func() {
			reachedOnce.Do(func() { close(reached) })
			select {
			case <-release:
			case <-time.After(5 * time.Second):
			}
		}
		registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, n int) {
			if held == "the registry" && n == 1 {
				hold()
			}
			w.Header().Set("Cache-Control", "public, max-age=300")
			serveText(t, w, "{}")
		})
		scope := CacheSessionScope{Trust: provider.TrustUntrusted, Cache: &config.CacheSpec{Npm: true}}
		var policy CachePolicy
		if held == "the kill switch" {
			scope = CacheSessionScope{
				Trust: provider.TrustUntrusted, Owner: "acme", Repository: "api",
				Cache: &config.CacheSpec{Npm: true, Publish: config.CachePublishDefaultBranch, Owner: "acme",
					Repository: "api"},
			}
			policy = cachePolicyFunc(func(_ context.Context, kind config.CacheKind, _, _ string) (bool, error) {
				if kind == config.CacheNpm {
					hold()
				}

				return true, nil
			})
		}
		service, credentials := npmServiceWith(t, registry, policy, scope)
		observer := &recordingObserver{}
		service.SetCacheObserver(observer)

		conn, _ := npmTunnel(t, service, credentials, npmRegistryHost)
		if _, err := conn.Write([]byte(npmRequestLine("/in-flight"))); err != nil {
			t.Fatalf("%s: write: %v", held, err)
		}
		select {
		case <-reached:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: the request was never held", held)
		}
		if err := service.Close(t.Context(), "billet-lease-npm"); err != nil {
			t.Fatalf("%s: Close: %v", held, err)
		}
		calls := observer.recorded()
		releaseOnce.Do(func() { close(release) })

		// THE HANDLER IS JOINED: the node closes the connection once it has
		// answered, so the end of the stream is the handler's return.
		if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatalf("%s: set a read deadline: %v", held, err)
		}
		reader := bufio.NewReader(conn)
		resp, err := http.ReadResponse(reader, nil)
		if err != nil {
			t.Fatalf("%s: the held request was never answered: %v", held, err)
		}
		_ = resp.Body.Close()
		if _, err := io.Copy(io.Discard, reader); err != nil {
			t.Fatalf("%s: the node did not close the connection after answering: %v", held, err)
		}
		assertNpmQuiet(t, service.npmCache())

		if len(calls) == 0 {
			t.Fatalf("%s: the session's end reported nothing", held)
		}
		for _, call := range calls {
			if call.obs.Npm != "" {
				t.Errorf("%s: a session that ended during a request reported the npm cache %q, want nothing",
					held, call.obs.Npm)
			}
		}
	}
}

// A REQUEST STILL BEING CLASSIFIED WHEN THE JOB ENDS IS NOT "UNUSED" either: a
// HEAD asks the store whether it holds the entry, and the store's lock can keep
// it waiting, so the session settles with nothing noted and the request not yet
// known to be one the store answers.
func TestASessionThatEndsWhileARequestIsClassifiedDoesNotCallTheNpmCacheUnused(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, `{"name":"held"}`)
	})
	service, credentials := npmService(t, registry)
	observer := &recordingObserver{}
	service.SetCacheObserver(observer)
	// ANOTHER JOB'S FILL, so the entry is held without this session noting it.
	other, err := service.PrepareScoped("billet-lease-npm-other", CacheSessionScope{
		Trust: provider.TrustUntrusted, Cache: &config.CacheSpec{Npm: true},
	})
	if err != nil {
		t.Fatalf("PrepareScoped: %v", err)
	}
	npmGet(t, service, other, npmRequestLine("/held"))
	if got := noted(service, credentials); got != "" {
		t.Fatalf("another job's fill noted %q for this one", got)
	}

	store := service.npmCache().store
	var unlockOnce sync.Once
	store.mu.Lock()
	unlock := func() { unlockOnce.Do(store.mu.Unlock) }
	t.Cleanup(unlock)
	conn, _ := npmTunnel(t, service, credentials, npmRegistryHost)
	if _, err := conn.Write([]byte("HEAD /held HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	service.mu.Lock()
	session := service.byToken[credentials.Token]
	service.mu.Unlock()
	for deadline := time.Now().Add(5 * time.Second); ; {
		session.mu.Lock()
		classifying := session.npmClassifying
		session.mu.Unlock()
		if classifying > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the HEAD never reached its classification")
		}
		time.Sleep(time.Millisecond)
	}
	if err := service.Close(t.Context(), "billet-lease-npm"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	calls := observer.recorded()
	unlock()

	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodHead})
	if err != nil {
		t.Fatalf("the HEAD was never answered: %v", err)
	}
	_ = resp.Body.Close()
	if _, err := io.Copy(io.Discard, reader); err != nil {
		t.Fatalf("the node did not close the connection after answering: %v", err)
	}
	assertNpmQuiet(t, service.npmCache())

	if len(calls) == 0 {
		t.Fatal("the session's end reported nothing")
	}
	for _, call := range calls {
		if call.obs.Npm != "" {
			t.Errorf("a session that ended while a HEAD was classified reported the npm cache %q, want nothing",
				call.obs.Npm)
		}
	}
}

// ageNpmEntry moves an entry's clock back by, so it expires as if time passed.
func ageNpmEntry(t *testing.T, store *npmStore, key string, by time.Duration) {
	t.Helper()

	entry, body, ok := store.acquire(key, time.Now())
	if !ok {
		t.Fatalf("%s is not stored", key)
	}
	_ = body.Close()
	entry.Received = entry.Received.Add(-by)
	if err := store.refresh(entry); err != nil {
		t.Fatalf("age %s: %v", key, err)
	}
}

// scriptedRegistry answers each request with the step of steps its number
// names, the last step repeating.
func scriptedRegistry(t *testing.T, steps ...func(w http.ResponseWriter, r *http.Request)) *fakeRegistry {
	t.Helper()

	return newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, n int) {
		steps[min(n, len(steps))-1](w, r)
	})
}

func answerWith(status int, cacheControl, etag, body string) func(w http.ResponseWriter, r *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if cacheControl != "" {
			w.Header().Set("Cache-Control", cacheControl)
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		w.WriteHeader(status)
		if _, err := io.WriteString(w, body); err != nil {
			return
		}
	}
}

// AN ANSWER THAT MAY NOT BE SHARED TAKES THE OLD ONE WITH IT: once the registry
// withdraws sharing, a later silent registry is not answered from what it
// shared before.
func TestAnUnshareableReplacementRemovesTheEntry(t *testing.T) {
	t.Parallel()

	registry := scriptedRegistry(t,
		answerWith(http.StatusOK, "public, max-age=0", `"v1"`, `{"v":1}`),
		answerWith(http.StatusOK, "private", `"v2"`, `{"v":2}`),
		answerWith(http.StatusServiceUnavailable, "", "", "down"))
	service, credentials := npmService(t, registry)
	npmGet(t, service, credentials, npmRequestLine("/withdrawn"))

	if resp, body := npmGet(t, service, credentials, npmRequestLine("/withdrawn")); resp.StatusCode != http.StatusOK ||
		body != `{"v":2}` {
		t.Fatalf("an unshareable replacement = %d %q", resp.StatusCode, body)
	}
	if service.npmCache().store.held("full /withdrawn") {
		t.Fatal("the entry the registry stopped sharing is still stored")
	}
	if resp, body := npmGet(t, service, credentials, npmRequestLine("/withdrawn")); resp.StatusCode !=
		http.StatusServiceUnavailable || body != "down" {
		t.Fatalf("a silent registry after a withdrawal = %d %q, want its 503, never the old entry",
			resp.StatusCode, body)
	}
}

// A TARBALL FETCHED AGAIN AFTER A WITHDRAWING 304 ARRIVES WHOLE, however large,
// streamed and not kept.
func TestATarballAfterAWithdrawingRevalidationArrivesWhole(t *testing.T) {
	t.Parallel()

	tarball := strings.Repeat("z", 2<<20)
	registry := scriptedRegistry(t,
		answerWith(http.StatusOK, "public, max-age=0", `"v1"`, "small"),
		answerWith(http.StatusNotModified, "private", "", ""),
		answerWith(http.StatusOK, "private", `"v2"`, tarball))
	service, credentials := npmService(t, registry)
	npmGet(t, service, credentials, npmRequestLine("/big/-/big-1.0.0.tgz"))
	waitForNpmFills(t, service.npmCache())
	ageNpmEntry(t, service.npmCache().store, " /big/-/big-1.0.0.tgz", 25*time.Hour)

	resp, body := npmGet(t, service, credentials, npmRequestLine("/big/-/big-1.0.0.tgz"))
	if resp.StatusCode != http.StatusOK || len(body) != len(tarball) || body != tarball {
		t.Fatalf("a tarball after a withdrawing 304 = %d, %d of %d bytes", resp.StatusCode, len(body), len(tarball))
	}
	waitForNpmFills(t, service.npmCache())
	if service.npmCache().store.held(" /big/-/big-1.0.0.tgz") {
		t.Fatal("a tarball the registry stopped sharing is still stored")
	}
}

// A HEAD THAT MADE THE NODE FETCH IS ANSWERED FROM WHAT IT FETCHED, never from
// the older generation the store still held.
func TestAHeadAfterANewTarballAnswersTheNewOne(t *testing.T) {
	t.Parallel()

	registry := scriptedRegistry(t,
		answerWith(http.StatusOK, "public, max-age=0", `"v1"`, "ten bytes!"),
		answerWith(http.StatusOK, "private", `"v2"`, "twenty bytes, a new 1"))
	service, credentials := npmService(t, registry)
	npmGet(t, service, credentials, npmRequestLine("/pkg/-/pkg-1.0.0.tgz"))
	waitForNpmFills(t, service.npmCache())
	ageNpmEntry(t, service.npmCache().store, " /pkg/-/pkg-1.0.0.tgz", 25*time.Hour)

	resp, body := npmGet(t, service, credentials,
		"HEAD /pkg/-/pkg-1.0.0.tgz HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n")
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") != `"v2"` ||
		resp.ContentLength != int64(len("twenty bytes, a new 1")) || body != "" {
		t.Fatalf("a HEAD that fetched = %d, ETag %q, length %d, body %q", resp.StatusCode,
			resp.Header.Get("ETag"), resp.ContentLength, body)
	}
}

// THE REGISTRY'S LAST 5XX IS RELAYED when the retry gives no answer at all,
// with its own body and Retry-After, not replaced by the node's 504.
func TestARetryWithNoAnswerRelaysTheRegistrysLast503(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			w.Header().Set("Retry-After", "7")
			answerWith(http.StatusServiceUnavailable, "", "", "busy")(w, r)

			return
		}
		<-r.Context().Done()
	})
	service, credentials := npmService(t, registry)
	service.npmCache().headerWait = 300 * time.Millisecond

	resp, body := npmGet(t, service, credentials, npmRequestLine("/busy"))
	if resp.StatusCode != http.StatusServiceUnavailable || body != "busy" || resp.Header.Get("Retry-After") != "7" ||
		registry.count() != 2 {
		t.Fatalf("a 503 then silence = %d %q (Retry-After %q) after %d attempts", resp.StatusCode, body,
			resp.Header.Get("Retry-After"), registry.count())
	}
}

// A 304 THAT CHANGES THE VALIDATOR CHANGES IT, so the next revalidation asks
// with the new one.
func TestARevalidationThatChangesTheValidatorKeepsTheNewOne(t *testing.T) {
	t.Parallel()

	registry := scriptedRegistry(t,
		answerWith(http.StatusOK, "public, max-age=0", `"v1"`, "{}"),
		answerWith(http.StatusNotModified, "public, max-age=0", `"v2"`, ""))
	service, credentials := npmService(t, registry)
	for range 3 {
		if resp, body := npmGet(t, service, credentials, npmRequestLine("/etag")); resp.StatusCode != http.StatusOK ||
			body != "{}" {
			t.Fatalf("a revalidated document = %d %q", resp.StatusCode, body)
		}
	}
	if got := registry.request(2).Header.Get("If-None-Match"); got != `"v2"` {
		t.Fatalf("the revalidation after a 304 that changed the ETag asked with %q, want the new one", got)
	}
}

// A TARBALL THE REGISTRY ENCODED REACHES THE CLIENT AS ITS OWN BYTES, which its
// integrity covers, and is not kept.
func TestAnEncodedTarballIsDecodedForTheClient(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Content-Encoding", "gzip")
		serveBytes(t, w, gzipped(t, "the tarball's bytes"))
	})
	service, credentials := npmService(t, registry)

	resp, body := npmGet(t, service, credentials, npmRequestLine("/enc/-/enc-1.0.0.tgz"))
	if resp.StatusCode != http.StatusOK || body != "the tarball's bytes" || resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("an encoded tarball = %d %q (encoding %q)", resp.StatusCode, body, resp.Header.Get("Content-Encoding"))
	}
	waitForNpmFills(t, service.npmCache())
	if service.npmCache().store.held(" /enc/-/enc-1.0.0.tgz") {
		t.Fatal("an encoded tarball was kept")
	}
}

// A FILL PAST THE BUDGET REACHES ITS CLIENT WHOLE AND IS NOT KEPT, so a job
// asking for more than the node may hold in flight costs warm hits, never its
// answers.
func TestATarballPastTheFillBudgetReachesItsClientWholeAndIsNotKept(t *testing.T) {
	t.Parallel()

	tarball := strings.Repeat("b", 64<<10)
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, tarball)
	})
	service, credentials := npmService(t, registry)
	service.npmCache().store.fillLimit = 1000

	resp, body := npmGet(t, service, credentials, npmRequestLine("/huge/-/huge-1.0.0.tgz"))
	if resp.StatusCode != http.StatusOK || body != tarball {
		t.Fatalf("a tarball past the fill budget = %d, %d of %d bytes", resp.StatusCode, len(body), len(tarball))
	}
	waitForNpmFills(t, service.npmCache())
	if service.npmCache().store.held(" /huge/-/huge-1.0.0.tgz") {
		t.Fatal("a tarball past the fill budget was kept")
	}
	assertNpmQuiet(t, service.npmCache())
}

// assertNpmQuiet holds that the node finished every request it was asked and
// left nothing behind: no fill in flight or slot taken, no byte of the fill
// budget still claimed, and no temporary file in the store.
func assertNpmQuiet(t *testing.T, npm *npmCache) {
	t.Helper()

	waitForNpmFills(t, npm)
	if taken := len(npm.slots); taken != 0 {
		t.Errorf("%d fill slots are still taken", taken)
	}
	npm.store.mu.Lock()
	filling := npm.store.filling
	npm.store.mu.Unlock()
	if filling != 0 {
		t.Errorf("%d bytes of the fill budget are still claimed", filling)
	}
	leftovers, err := os.ReadDir(npm.store.tempDir())
	if err != nil {
		t.Fatalf("read the store's temporary directory: %v", err)
	}
	for _, leftover := range leftovers {
		t.Errorf("a temporary file was left in the store: %s", leftover.Name())
	}
	// EVERY READER CLOSED AND EVERY RETIRED BODY GONE. A warm answer closes its
	// body after its last byte reached the client, so this is waited for, not
	// read once.
	deadline := time.Now().Add(5 * time.Second)
	for {
		npm.store.mu.Lock()
		pins, retired, debt := len(npm.store.pins), len(npm.store.retired), len(npm.store.debt)
		owed := npm.store.retiredBytes
		npm.store.mu.Unlock()
		if pins == 0 && retired == 0 && debt == 0 && owed == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("after 5s: %d bodies still pinned, %d retired and %d kept as debt (%d bytes) still on disk",
				pins, retired, debt, owed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// A REPLACEMENT THE STORE COULD NOT KEEP STILL REPLACES: the superseded entry
// never answers a later silent registry.
func TestAReplacementPastTheBudgetRetiresTheEntryItReplaced(t *testing.T) {
	t.Parallel()

	registry := scriptedRegistry(t,
		answerWith(http.StatusOK, "public, max-age=0", `"v1"`, `{"v":1}`),
		answerWith(http.StatusOK, "public, max-age=300", `"v2"`, `{"v":2, "padding":"`+strings.Repeat("p", 4096)+`"}`),
		answerWith(http.StatusServiceUnavailable, "", "", "down"))
	service, credentials := npmService(t, registry)
	npmGet(t, service, credentials, npmRequestLine("/replaced"))
	service.npmCache().store.fillLimit = 100

	if resp, body := npmGet(t, service, credentials, npmRequestLine("/replaced")); resp.StatusCode != http.StatusOK ||
		!strings.HasPrefix(body, `{"v":2`) {
		t.Fatalf("a replacement past the budget = %d %.20q", resp.StatusCode, body)
	}
	if service.npmCache().store.held("full /replaced") {
		t.Fatal("the generation a 200 replaced is still stored")
	}
	if resp, body := npmGet(t, service, credentials, npmRequestLine("/replaced")); resp.StatusCode !=
		http.StatusServiceUnavailable || body != "down" {
		t.Fatalf("a silent registry after the replacement = %d %q, want its 503", resp.StatusCode, body)
	}
	assertNpmQuiet(t, service.npmCache())
}

// A HELD 5XX IS STILL WHOLE WHEN THE RETRY ENDS: its idle clock is paused while
// the node waits on the second attempt, here a document whose body stalls for
// longer than the idle bound, and its body is larger than a transport buffers.
func TestAHeld503IsRelayedWholeAfterARetryWhoseBodyStalled(t *testing.T) {
	t.Parallel()

	busy := strings.Repeat("b", 256<<10)
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if n == 1 {
			answerWith(http.StatusServiceUnavailable, "", "", busy)(w, r)

			return
		}
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Content-Length", "100")
		serveText(t, w, "{")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	})
	service, credentials := npmService(t, registry)
	service.npmCache().bodyIdle = 300 * time.Millisecond

	resp, body := npmGet(t, service, credentials, npmRequestLine("/held"))
	if resp.StatusCode != http.StatusServiceUnavailable || len(body) != len(busy) {
		t.Fatalf("a held 503 after a stalled retry = %d, %d of %d bytes", resp.StatusCode, len(body), len(busy))
	}
	assertNpmQuiet(t, service.npmCache())
}

// AN ANSWER OF UNKNOWN LENGTH IS CHUNKED, and one cut short never ends with the
// last chunk, so the client cannot take it for whole; and its headers arrive
// before the node waits on its body, even when the node decodes it: the
// registry sends no body byte until the client has the headers.
func TestACutAnswerOfUnknownLengthIsNeverTakenForWhole(t *testing.T) {
	t.Parallel()

	encoded := gzipped(t, strings.Repeat("g", 64<<10))
	headersSeen := make(chan struct{})
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-headersSeen:
		case <-time.After(5 * time.Second):
		}
		serveBytes(t, w, encoded[:len(encoded)/2])
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("the test registry cannot cut its connection")

			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			t.Errorf("cut the test registry's connection: %v", err)

			return
		}
		_ = conn.Close()
	})
	service, credentials := npmService(t, registry)

	conn, _ := npmTunnel(t, service, credentials, npmRegistryHost)
	if _, err := conn.Write([]byte(npmRequestLine("/cut/-/cut-1.0.0.tgz"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	// HEADERS BEFORE ANY BODY BYTE: the registry holds its body for 5 s or until
	// this read returns, so a node that waited on the body first fails here.
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("an encoded tarball sent no headers before its body: %v", err)
	}
	close(headersSeen)
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	_, err = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(resp.TransferEncoding) != 1 || resp.TransferEncoding[0] != "chunked" ||
		err == nil {
		t.Fatalf("a cut answer of unknown length = %d, framing %v, read error %v; want chunked and cut",
			resp.StatusCode, resp.TransferEncoding, err)
	}
	assertNpmQuiet(t, service.npmCache())
}

// A 304 TO A REQUEST THAT SENT NO VALIDATOR PROVES NOTHING: the entry, which
// had no ETag to send, is not refreshed and the client is not told 304.
func TestAnUnsolicitedNotModifiedIsAnUpstreamFault(t *testing.T) {
	t.Parallel()

	registry := scriptedRegistry(t,
		answerWith(http.StatusOK, "public, max-age=0", "", "{}"),
		answerWith(http.StatusNotModified, "public, max-age=300", "", ""))
	service, credentials := npmService(t, registry)
	npmGet(t, service, credentials, npmRequestLine("/untagged"))
	before, body, ok := service.npmCache().store.acquire("full /untagged", time.Now())
	if !ok {
		t.Fatal("the untagged document was not kept")
	}
	_ = body.Close()

	if resp, _ := npmGet(t, service, credentials, npmRequestLine("/untagged")); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("an unsolicited 304 = %d, want 502", resp.StatusCode)
	}
	after, body, ok := service.npmCache().store.acquire("full /untagged", time.Now())
	if ok {
		_ = body.Close()
	}
	if !ok || !after.Received.Equal(before.Received) || after.Lifetime != before.Lifetime {
		t.Fatalf("an unsolicited 304 refreshed the entry: before %+v, after %+v", before, after)
	}
}

// A TARBALL IN A CODING THE NODE CANNOT TAKE OFF IS NO ANSWER THE CLIENT ACCEPTED.
func TestATarballInAnUnsupportedCodingIsRefused(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Content-Encoding", "br")
		serveText(t, w, "not brotli, but no matter")
	})
	service, credentials := npmService(t, registry)

	resp, body := npmGet(t, service, credentials, npmRequestLine("/br/-/br-1.0.0.tgz"))
	if resp.StatusCode != http.StatusBadGateway || strings.Contains(body, "brotli") {
		t.Fatalf("a tarball in br = %d %q, want 502 and none of its bytes", resp.StatusCode, body)
	}
	assertNpmQuiet(t, service.npmCache())
}

// A HEAD THAT MAKES THE NODE FETCH IS ANSWERED AT THE REGISTRY'S HEADERS, not
// after its body: the registry holds the body until the client has its answer,
// and the body is then read only to keep it.
func TestAHeadIsAnsweredBeforeTheBodyItKeeps(t *testing.T) {
	t.Parallel()

	answered := make(chan struct{})
	tarball := strings.Repeat("h", 4096)
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, n int) {
		w.Header().Set("Cache-Control", "public, max-age=0")
		w.Header().Set("ETag", `"v`+strconv.Itoa(n)+`"`)
		w.Header().Set("Content-Length", strconv.Itoa(len(tarball)))
		w.WriteHeader(http.StatusOK)
		if n > 1 {
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			select {
			case <-answered:
			case <-time.After(5 * time.Second):
			}
		}
		serveText(t, w, tarball)
	})
	service, credentials := npmService(t, registry)
	npmGet(t, service, credentials, npmRequestLine("/headed/-/headed-1.0.0.tgz"))
	waitForNpmFills(t, service.npmCache())
	ageNpmEntry(t, service.npmCache().store, " /headed/-/headed-1.0.0.tgz", 25*time.Hour)

	conn, _ := npmTunnel(t, service, credentials, npmRegistryHost)
	if _, err := conn.Write([]byte("HEAD /headed/-/headed-1.0.0.tgz HTTP/1.1\r\nHost: registry.npmjs.org\r\n\r\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("set a read deadline: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodHead})
	close(answered)
	if err != nil {
		t.Fatalf("a HEAD that made the node fetch was not answered before the body: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("ETag") != `"v2"` ||
		resp.ContentLength != int64(len(tarball)) {
		t.Fatalf("a HEAD that fetched = %d, ETag %q, length %d", resp.StatusCode, resp.Header.Get("ETag"),
			resp.ContentLength)
	}
	assertNpmQuiet(t, service.npmCache())
	stored, body, ok := service.npmCache().store.acquire(" /headed/-/headed-1.0.0.tgz", time.Now())
	if !ok {
		t.Fatal("the tarball a HEAD fetched was not kept")
	}
	_ = body.Close()
	if stored.ETag != `"v2"` {
		t.Fatalf("the kept tarball is %s, want the one the HEAD fetched", stored.ETag)
	}
}

// A DOCUMENT THE STORE COULD NOT KEEP IS ANSWERED FROM THE FILE IT WAS READ
// INTO, and that file's bytes stay charged to the fill budget until the answer
// ends: the commit failed, the file is gone from the store's directories, and
// only the reservation the read made still counts it.
func TestADocumentAnsweredAfterAFailedCommitStaysCharged(t *testing.T) {
	t.Parallel()

	document := `{"padding":"` + strings.Repeat("f", 16<<20) + `"}`
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, document)
	})
	service, credentials := npmService(t, registry)
	npm := service.npmCache()
	npm.store.fillLimit = int64(len(document)) + 1000
	npm.store.disk = durablefile.Installer{SyncDir: func(dir string) error {
		if dir == npm.store.bodiesDir() {
			return errors.New("the disk refused to flush the bodies directory")
		}

		return nil
	}}

	conn, _ := npmTunnel(t, service, credentials, npmRegistryHost)
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("set a deadline: %v", err)
	}
	holdBackTheBody(t, conn)
	if _, err := conn.Write([]byte(npmRequestLine("/unkept"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read the headers: %v", err)
	}
	// THE CLIENT HAS READ ONLY THE HEADERS, so the fallback is still answering.
	npm.store.mu.Lock()
	filling := npm.store.filling
	npm.store.mu.Unlock()
	if filling < int64(len(document)) {
		t.Fatalf("while a fallback is answered, %d bytes of the fill budget are claimed, want its %d", filling,
			len(document))
	}
	if npm.store.reserve(2000) {
		t.Fatal("the fill budget admitted another fill while the fallback's bytes were on disk")
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || len(body) != len(document) {
		t.Fatalf("the fallback read %d of %d bytes: %v", len(body), len(document), err)
	}
	if npm.store.held("full /unkept") {
		t.Fatal("a document whose commit failed was indexed")
	}
	assertNpmQuiet(t, npm)
}

// A DOCUMENT ANSWERED FROM THE STORE THAT ITS OWN COMMIT EVICTED is pinned and
// charged for as long as its client takes to read it: the store's bound is
// smaller than the document, and the client stops after the headers, so the
// answer is still being written while the store is looked at.
func TestADocumentItsCommitEvictedStaysChargedWhileItIsAnswered(t *testing.T) {
	t.Parallel()

	document := `{"padding":"` + strings.Repeat("d", 16<<20) + `"}`
	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		serveText(t, w, document)
	})
	service, credentials := npmService(t, registry)
	npm := service.npmCache()
	npm.store.mu.Lock()
	npm.store.limit = 1024
	npm.store.mu.Unlock()

	conn, _ := npmTunnel(t, service, credentials, npmRegistryHost)
	if err := conn.SetDeadline(time.Now().Add(30 * time.Second)); err != nil {
		t.Fatalf("set a deadline: %v", err)
	}
	holdBackTheBody(t, conn)
	if _, err := conn.Write([]byte(npmRequestLine("/huge"))); err != nil {
		t.Fatalf("write: %v", err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, nil)
	if err != nil {
		t.Fatalf("read the headers: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		npm.store.mu.Lock()
		pins, owed, held := len(npm.store.pins), npm.store.retiredBytes, len(npm.store.entries)
		npm.store.mu.Unlock()
		if pins == 1 && owed == int64(len(document)) && held == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("while answering a document its commit evicted: %d pinned, %d bytes owed, %d entries; "+
				"want 1, %d, 0", pins, owed, held, len(document))
		}
		time.Sleep(10 * time.Millisecond)
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil || len(body) != len(document) {
		t.Fatalf("the document read %d of %d bytes: %v", len(body), len(document), err)
	}
	assertNpmQuiet(t, npm)
}
