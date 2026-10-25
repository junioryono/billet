package app

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/nodeclient"
	"github.com/junioryono/billet/internal/wirecert"
)

// EACH CONTROLLER IS VERIFIED AGAINST THE NAME THE NODE DIALLED FOR IT. Two
// controllers of one deployment, one certified only for `localhost` and the
// other only for 127.0.0.1; the node dials the first by name and the second by
// address. The first is down, so the registration goes to the second, and its
// handshake succeeds only if it was checked against 127.0.0.1: checked against
// the first controller's name it fails, which the test proves it would.
func TestEachControllerIsVerifiedAgainstItsOwnName(t *testing.T) {
	t.Parallel()

	ca, err := wirecert.LoadOrCreateCA(t.TempDir(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatalf("the deployment's authority: %v", err)
	}

	node, err := ca.IssueNode("epyc-1")
	if err != nil {
		t.Fatalf("issue the node's certificate: %v", err)
	}

	dir := t.TempDir()
	if err := node.Write(dir); err != nil {
		t.Fatalf("write the node's bundle: %v", err)
	}

	identity, err := wirecert.NewRotating(filepath.Join(dir, "node.crt"), filepath.Join(dir, "node.key"),
		filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatalf("the node's identity: %v", err)
	}

	named := controllerServing(t, ca, "localhost", nil)
	named.Close()

	var reached atomic.Int32

	addressed := controllerServing(t, ca, "127.0.0.1", &reached)

	_, namedPort, err := net.SplitHostPort(named.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}

	cfg := &config.Config{Node: &config.NodeConfig{
		Name: "epyc-1",
		ServerAddrs: []string{
			net.JoinHostPort("localhost", namedPort), addressed.Listener.Addr().String(),
		},
	}}

	client, err := NewNodeClient(cfg, identity)
	if err != nil {
		t.Fatalf("NewNodeClient: %v", err)
	}

	// THE FIXTURE CAN TELL THE DIFFERENCE: the second controller, checked
	// against the first one's name, refuses the handshake.
	misnamed := &http.Client{Transport: &http.Transport{TLSClientConfig: identity.ClientTLS("localhost")}}

	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, addressed.URL+"/v1/register", http.NoBody)
	if err != nil {
		t.Fatalf("build the request: %v", err)
	}

	res, err := misnamed.Do(req)
	if err == nil {
		_ = res.Body.Close()

		t.Fatal("the second controller was trusted under the first one's name, so this proves nothing")
	}

	// REFUSED FOR ITS NAME, not for anything else a handshake can fail on.
	var misnamedErr x509.HostnameError
	if !errors.As(err, &misnamedErr) {
		t.Fatalf("the handshake under the wrong name failed with %v, want a hostname mismatch", err)
	}

	_ = client.Register(t.Context(), nodeclient.Registration{ //nolint:errcheck // the stand-in answers 503; what is asserted is that it was reached
		Provider: config.ProviderDocker, GuestOS: []config.GuestOS{config.GuestLinux},
		Deployment: "0123456789abcdef0123456789abcdef", VCPU: 2, Memory: 2 * config.GiB,
	})

	if reached.Load() == 0 {
		t.Fatal("the registration never reached the controller certified for the address the node dialled")
	}
}

// controllerServing is a TLS stand-in for a controller, certified by ca for
// host alone, that answers every request 503 and counts them in reached.
func controllerServing(t *testing.T, ca *wirecert.CA, host string, reached *atomic.Int32) *httptest.Server {
	t.Helper()

	bundle, err := ca.IssueServer([]string{host})
	if err != nil {
		t.Fatalf("issue the controller's certificate: %v", err)
	}

	pair, err := tls.X509KeyPair(bundle.CertPEM, bundle.KeyPEM)
	if err != nil {
		t.Fatalf("the controller's key pair: %v", err)
	}

	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if reached != nil {
			reached.Add(1)
		}

		http.Error(w, "a stand-in", http.StatusServiceUnavailable)
	}))
	srv.TLS = &tls.Config{
		Certificates: []tls.Certificate{pair},
		ClientAuth:   tls.RequestClientCert,
		MinVersion:   tls.VersionTLS13,
	}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	if !strings.HasPrefix(srv.URL, "https://") {
		t.Fatal(errors.New("the stand-in is not serving TLS"))
	}

	return srv
}
