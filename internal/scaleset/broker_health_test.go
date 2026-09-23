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

	mu    sync.Mutex
	conns []*silenceable
}

type silenceable struct{ silent atomic.Bool }

func newSilenceProxy(t *testing.T, backend string) *silenceProxy {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	p := &silenceProxy{ln: ln, backend: backend}
	t.Cleanup(func() { _ = ln.Close() })

	go p.serve(t)

	return p
}

func (p *silenceProxy) serve(t *testing.T) {
	for {
		client, err := p.ln.Accept()
		if err != nil {
			return
		}

		server, err := net.Dial("tcp", p.backend)
		if err != nil {
			_ = client.Close()
			continue
		}

		p.dialed.Add(1)
		c := &silenceable{}
		p.mu.Lock()
		p.conns = append(p.conns, c)
		p.mu.Unlock()
		t.Cleanup(func() { _ = client.Close(); _ = server.Close() })

		go c.pipe(server, client)
		go c.pipe(client, server)
	}
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

// A SILENT CONNECTION TO THE BROKER IS REPLACED, NOT REUSED UNTIL THE KERNEL GIVES
// UP. Every listener of a target multiplexes its long poll over one HTTP/2
// connection; on 2026-09-23 that connection went half-open and every retry of
// every listener's poll was sent down it again for 18 minutes. With the health
// check, a request on the silenced connection fails within the ping allowance,
// far inside the request timeout, and the next request dials a new connection.
func TestASilentBrokerConnectionIsClosedAndReplaced(t *testing.T) {
	t.Parallel()

	release := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/long-poll" {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_, _ = io.WriteString(w, r.Proto)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	proxy := newSilenceProxy(t, srv.Listener.Addr().String())

	const pingAfter, pingTimeout = 200 * time.Millisecond, 200 * time.Millisecond
	const requestTimeout = 30 * time.Second

	client := newRetryableHTTPClient(pingAfter, pingTimeout)
	client.RetryMax = 0
	client.HTTPClient.Timeout = requestTimeout
	transport := client.HTTPClient.Transport.(*http.Transport)
	transport.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	url := "https://" + proxy.ln.Addr().String()

	get := func(path string) (string, error) {
		resp, err := client.StandardClient().Get(url + path)
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

	// A long poll outlives several ping intervals on a healthy connection: the
	// health check answers silence, not a slow response.
	go func() { time.Sleep(10 * pingAfter); close(release) }()
	if proto, err := get("/long-poll"); err != nil || proto != "HTTP/2.0" {
		t.Fatalf("a long poll on a healthy connection = %q, %v; pings must not end it", proto, err)
	}

	proxy.silenceAll()

	started := time.Now()
	_, err := get("/")
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a request on a silenced connection succeeded; the proxy did not silence it")
	}
	if errors.Is(err, context.DeadlineExceeded) || elapsed >= requestTimeout/2 {
		t.Fatalf("the silenced connection was noticed after %v (%v): the request timeout ended it, "+
			"not the health check", elapsed, err)
	}

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
