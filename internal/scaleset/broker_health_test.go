package scaleset

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// silenceProxy forwards TCP connections to a backend and can make every
// connection open so far go silent: it keeps both sockets open and stops
// forwarding, which is what a half-open connection looks like to the client.
// Nothing reaches EOF, so only a health check can tell the client.
type silenceProxy struct {
	ln      net.Listener
	backend string
	dialed  atomic.Int32

	mu      sync.Mutex
	conns   []*silenceable
	sockets []net.Conn
	wg      sync.WaitGroup
}

type silenceable struct{ silent atomic.Bool }

func newSilenceProxy(t *testing.T, backend string) *silenceProxy {
	t.Helper()

	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	p := &silenceProxy{ln: ln, backend: backend}
	p.wg.Add(1)
	go p.serve()
	t.Cleanup(p.close)

	return p
}

func (p *silenceProxy) serve() {
	defer p.wg.Done()

	var d net.Dialer
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}

		server, err := d.DialContext(context.Background(), "tcp", p.backend)
		if err != nil {
			_ = client.Close()
			continue
		}

		p.dialed.Add(1)
		c := &silenceable{}
		p.mu.Lock()
		p.conns = append(p.conns, c)
		p.sockets = append(p.sockets, client, server)
		p.mu.Unlock()

		p.wg.Add(2)
		go func() { defer p.wg.Done(); c.pipe(server, client) }()
		go func() { defer p.wg.Done(); c.pipe(client, server) }()
	}
}

// close stops accepting, closes every socket and waits for every goroutine.
func (p *silenceProxy) close() {
	_ = p.ln.Close()

	p.mu.Lock()
	for _, s := range p.sockets {
		_ = s.Close()
	}
	p.mu.Unlock()

	p.wg.Wait()
}

// pipe copies until the connection is silenced, then reads and discards.
func (c *silenceable) pipe(dst io.Writer, src io.Reader) {
	buf := make([]byte, 32<<10)
	for {
		n, err := src.Read(buf)
		if n > 0 && !c.silent.Load() {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (p *silenceProxy) silenceAll() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, c := range p.conns {
		c.silent.Store(true)
	}
}

// A SILENT CONNECTION TO THE BROKER IS REPLACED, NOT REUSED UNTIL THE SOCKET
// FAILS. Every listener of a target multiplexes its long poll over one HTTP/2
// connection; on 2026-09-23 that connection went half-open and every retry of
// every listener's poll was sent down it again for 18 minutes. With the health
// check, a request in flight on the silenced connection fails within the ping
// allowance, far inside the request timeout, and the next request dials a new
// connection.
func TestASilentBrokerConnectionIsClosedAndReplaced(t *testing.T) {
	t.Parallel()

	entered := make(chan string, 4)
	release := map[string]chan struct{}{"/long-poll": make(chan struct{}), "/stuck": make(chan struct{})}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if held, ok := release[r.URL.Path]; ok {
			entered <- r.URL.Path
			select {
			case <-held:
			case <-r.Context().Done():
				return
			}
		}
		if _, err := io.WriteString(w, r.Proto); err != nil {
			return
		}
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	proxy := newSilenceProxy(t, srv.Listener.Addr().String())

	// Generous enough that a healthy peer on a loaded runner always answers in
	// time, and still an order of magnitude inside the request timeout.
	const pingAfter, pingTimeout = 500 * time.Millisecond, 2 * time.Second
	const requestTimeout = 60 * time.Second

	client := newRetryableHTTPClient(pingAfter, pingTimeout)
	client.RetryMax = 0
	client.HTTPClient.Timeout = requestTimeout
	transport, ok := client.HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the client's transport is not an *http.Transport")
	}
	serverTransport, ok := srv.Client().Transport.(*http.Transport)
	if !ok {
		t.Fatal("the test server's client transport is not an *http.Transport")
	}
	transport.TLSClientConfig = serverTransport.TLSClientConfig.Clone()
	base := "https://" + proxy.ln.Addr().String()

	get := func(path string) (string, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, http.NoBody)
		if err != nil {
			return "", err
		}
		resp, err := client.StandardClient().Do(req)
		if err != nil {
			return "", err
		}
		defer func() { _ = resp.Body.Close() }()

		body, err := io.ReadAll(resp.Body)

		return string(body), err
	}

	// One connection, HTTP/2, reused: the premise of the incident.
	for range 2 {
		proto, err := get("/")
		if err != nil || proto != "HTTP/2.0" {
			t.Fatalf("request over a healthy connection = %q, %v; want HTTP/2.0", proto, err)
		}
	}
	if n := proxy.dialed.Load(); n != 1 {
		t.Fatalf("dialed %d connections for two requests; the test needs one reused connection", n)
	}

	// A long poll held at the server for several ping intervals survives on a
	// healthy connection: the health check answers silence, not a slow response.
	type result struct {
		proto string
		err   error
	}
	held := make(chan result, 1)
	go func() {
		proto, err := get("/long-poll")
		held <- result{proto, err}
	}()
	if path := <-entered; path != "/long-poll" {
		t.Fatalf("the server entered %q, want the long poll", path)
	}
	time.Sleep(5 * pingAfter)
	close(release["/long-poll"])
	if r := <-held; r.err != nil || r.proto != "HTTP/2.0" {
		t.Fatalf("a long poll on a healthy connection = %q, %v; pings must not end it", r.proto, r.err)
	}

	// Silence the connection UNDER a request already in flight, so the request
	// cannot have been sent on a replacement the health check dialled first.
	stuck := make(chan result, 1)
	started := time.Now()
	go func() {
		proto, err := get("/stuck")
		stuck <- result{proto, err}
	}()
	if path := <-entered; path != "/stuck" {
		t.Fatalf("the server entered %q, want the request that will be stranded", path)
	}
	proxy.silenceAll()

	r := <-stuck
	elapsed := time.Since(started)
	if r.err == nil {
		t.Fatal("a request on a silenced connection succeeded; the proxy did not silence it")
	}
	if errors.Is(r.err, context.DeadlineExceeded) || elapsed >= requestTimeout/2 {
		t.Fatalf("the silenced connection was noticed after %v (%v): the request timeout ended it, "+
			"not the health check", elapsed, r.err)
	}
	close(release["/stuck"])

	proto, err := get("/")
	if err != nil || proto != "HTTP/2.0" {
		t.Fatalf("the request after the silent connection = %q, %v; want a new HTTP/2 connection", proto, err)
	}
	if n := proxy.dialed.Load(); n != 2 {
		t.Fatalf("dialed %d connections; the silenced one must have been replaced by exactly one more", n)
	}
}

// The production client is built with the health check the test above measures.
func TestTheBrokerClientCarriesTheHealthCheck(t *testing.T) {
	t.Parallel()

	transport, ok := newValidatedRetryableHTTPClient().HTTPClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("the client's transport is not the *http.Transport the upstream client asserts")
	}
	if transport.HTTP2 == nil || transport.HTTP2.SendPingTimeout != brokerPingAfter ||
		transport.HTTP2.PingTimeout != brokerPingTimeout {
		t.Fatalf("HTTP2 = %+v; want SendPingTimeout %v and PingTimeout %v",
			transport.HTTP2, brokerPingAfter, brokerPingTimeout)
	}
	if !transport.ForceAttemptHTTP2 {
		t.Fatal("the transport no longer attempts HTTP/2, so the health check guards nothing")
	}
}
