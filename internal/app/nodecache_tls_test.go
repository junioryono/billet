package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func selfSigned(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "cache listener"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(parsed)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, roots
}

// serveUntilClosed serves until the test closes the server.
func serveUntilClosed(t *testing.T, server *http.Server, listener net.Listener) {
	t.Helper()
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve: %v", err)
	}
}

func closeServer(t *testing.T, server *http.Server) {
	t.Helper()
	if err := server.Close(); err != nil {
		t.Errorf("close the server: %v", err)
	}
}

// THE EC2 LISTENER CARRIES WHAT A FIRECRACKER GUEST'S BRIDGE CARRIES (ADR-023).
// Bazel's and Buck2's gRPC arrives as HTTP/2 with prior knowledge through the guest's
// relay, inside TLS the relay did, and http.Server takes that only from a connection
// that is not a *tls.Conn. So this dials as the relay does: TLS to the listener, then
// plain HTTP/2 or HTTP/1 on top with no ALPN to say which.
func TestTheEC2CacheListenerServesHTTP2WithPriorKnowledgeInsideTLS(t *testing.T) {
	certificate, roots := selfSigned(t)
	var listen net.ListenConfig
	base, err := listen.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}
	protos := make(chan int, 2)
	server := &http.Server{
		ReadHeaderTimeout: 5 * time.Second,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			protos <- r.ProtoMajor
			w.WriteHeader(http.StatusNoContent)
		}),
	}
	server.Protocols = new(http.Protocols)
	server.Protocols.SetHTTP1(true)
	server.Protocols.SetUnencryptedHTTP2(true)
	listener := opaqueTLSListener{tls.NewListener(base, config)}
	go serveUntilClosed(t, server, listener)
	t.Cleanup(func() { closeServer(t, server) })

	address := base.Addr().String()
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		dialer := tls.Dialer{Config: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS13, ServerName: "127.0.0.1"}}

		return dialer.DialContext(ctx, "tcp", address)
	}
	for _, tc := range []struct {
		name  string
		major int
		set   func(*http.Protocols)
	}{
		{"HTTP/2 with prior knowledge", 2, func(p *http.Protocols) { p.SetUnencryptedHTTP2(true) }},
		{"HTTP/1", 1, func(p *http.Protocols) { p.SetHTTP1(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			protocols := new(http.Protocols)
			tc.set(protocols)
			client := &http.Client{Transport: &http.Transport{DialContext: dial, Protocols: protocols}}
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://cache/", nil)
			request.RequestURI = ""
			response, err := client.Do(request)
			if err != nil {
				t.Fatalf("Do: %v", err)
			}
			if err := response.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if got := <-protos; got != tc.major {
				t.Fatalf("the handler saw HTTP/%d, want HTTP/%d", got, tc.major)
			}
		})
	}
}

// A CLIENT THAT SPEAKS NO TLS GETS NO ANSWER: TLS is hidden from the server, not removed.
func TestTheEC2CacheListenerStillRefusesWhatIsNotTLS(t *testing.T) {
	t.Parallel()
	certificate, _ := selfSigned(t)
	var listen net.ListenConfig
	base, err := listen.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}
	handled := make(chan struct{}, 1)
	server := &http.Server{
		ReadHeaderTimeout: 2 * time.Second,
		Handler:           http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handled <- struct{}{} }),
	}
	go serveUntilClosed(t, server, opaqueTLSListener{tls.NewListener(base, config)})
	t.Cleanup(func() { closeServer(t, server) })

	var dialer net.Dialer
	conn, err := dialer.DialContext(t.Context(), "tcp", base.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: cache\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// A CLOSE, an alert or the deadline: what matters is what the handler saw.
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Logf("the connection ended with %v", err)
	}
	select {
	case <-handled:
		t.Fatal("cleartext reached the handler")
	default:
	}
}

// THE TWO TESTS ABOVE PROVE THE MECHANISM, and this proves the listener is built with it:
// a real listener needs a store, an attacher and a certificate on disk, and what these
// lines do is what the tests above run. Each is what a failure would be: a guest's gRPC
// refused, the relay stalled on a ticket, or the read cache reaped only every six hours.
func TestTheNodeCacheListenerIsBuiltWithWhatTheEC2GuestNeeds(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("nodecache.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"srv.Protocols.SetUnencryptedHTTP2(true)",
		"SessionTicketsDisabled: true,",
		"serveListener = opaqueTLSListener{tls.NewListener(ln, srv.TLSConfig)}",
		"srv.Serve(serveListener)",
		"go reapReadCache(evictCtx, service)",
		"case <-cleanupTicker.C:\n\t\t\t\t\trenewActive()",
	} {
		if !strings.Contains(string(source), want) {
			t.Errorf("startNodeCache no longer has %q", want)
		}
	}
}
