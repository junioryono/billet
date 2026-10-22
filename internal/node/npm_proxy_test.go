package node

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/alloc"
	"github.com/junioryono/billet/internal/config"
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

	service, err := NewCacheService("http://127.0.0.1:7718", "test-deployment", t.TempDir(),
		&fakeCacheStore{}, &fakeVolumeAttacher{}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("NewCacheService: %v", err)
	}
	credentials, err := service.PrepareScoped("billet-lease-npm", CacheSessionScope{
		Trust: provider.TrustUntrusted, Cache: &config.CacheSpec{Npm: true},
	})
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
	npm.headerWait = 300 * time.Millisecond
	npm.bodyIdle = 300 * time.Millisecond

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
	entry, _ := store.lookup("full /doc")
	entry.Received = entry.Received.Add(-time.Hour)
	if err := store.refresh(entry); err != nil {
		t.Fatalf("age the entry: %v", err)
	}

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
	cut, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		_, err = io.ReadAll(cut.Body)
		_ = cut.Body.Close()
		if err == nil {
			t.Fatal("a cut tarball reached the client whole")
		}
	}
	waitForNpmFills(t, service.npmCache())
	if service.npmCache().store.held(" /cut/-/cut-1.0.0.tgz") {
		t.Fatal("a cut tarball was kept")
	}
	if registry.count() != 2 {
		t.Fatalf("the registry saw %d requests, want the whole tarball and the cut one", registry.count())
	}
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

// AN ANSWER THAT SETS A COOKIE IS RELAYED BUT NEVER KEPT, and the cookie stays
// with the node.
func TestAnAnswerThatSetsACookieIsNotKept(t *testing.T) {
	t.Parallel()

	registry := newFakeRegistry(t, func(w http.ResponseWriter, r *http.Request, _ int) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		w.Header().Set("Set-Cookie", "session=node")
		serveText(t, w, "{}")
	})
	service, credentials := npmService(t, registry)

	resp, _ := npmGet(t, service, credentials, npmRequestLine("/cookie"))
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Set-Cookie") != "" {
		t.Fatalf("an answer with a cookie = %d, cookie %q", resp.StatusCode, resp.Header.Get("Set-Cookie"))
	}
	if service.npmCache().store.held("full /cookie") {
		t.Fatal("an answer that set a cookie was kept")
	}
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
