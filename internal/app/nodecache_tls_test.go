package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
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

// THE EC2 LISTENER CARRIES WHAT A FIRECRACKER GUEST'S BRIDGE CARRIES (ADR-023).
// Bazel's and Buck2's gRPC arrives as HTTP/2 with prior knowledge through the guest's
// relay, inside TLS the relay did, and http.Server takes that only from a connection
// that is not a *tls.Conn. So this dials as the relay does: TLS to the listener, then
// plain HTTP/2 or HTTP/1 on top with no ALPN to say which.
func TestTheEC2CacheListenerServesHTTP2WithPriorKnowledgeInsideTLS(t *testing.T) {
	t.Parallel()
	certificate, roots := selfSigned(t)
	base, err := net.Listen("tcp", "127.0.0.1:0")
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
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

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

// A CLIENT THAT DOES NOT TRUST THE LISTENER LEARNS NOTHING, and one that speaks no TLS
// gets no answer: TLS is hidden from the server, not removed.
func TestTheEC2CacheListenerStillRefusesWhatIsNotTLS(t *testing.T) {
	t.Parallel()
	certificate, _ := selfSigned(t)
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	config := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13}
	handled := make(chan struct{}, 1)
	server := &http.Server{
		ReadHeaderTimeout: 2 * time.Second,
		Handler:           http.HandlerFunc(func(http.ResponseWriter, *http.Request) { handled <- struct{}{} }),
	}
	go func() { _ = server.Serve(opaqueTLSListener{tls.NewListener(base, config)}) }()
	t.Cleanup(func() { _ = server.Close() })

	conn, err := net.Dial("tcp", base.Addr().String())
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
	buffer := make([]byte, 16)
	_, _ = conn.Read(buffer)
	select {
	case <-handled:
		t.Fatal("cleartext reached the handler")
	default:
	}
}
