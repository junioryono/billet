package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/app"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/fakeactions"
	billetgithub "github.com/junioryono/billet/internal/github"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/tokenbroker"
)

// servedBroker is app.ServeBroker running on a real port, holding the fake App's key.
type servedBroker struct {
	url, roots string
}

// serveBroker runs the broker the way `billet broker serve` does.
func serveBroker(t *testing.T, p *plane, deployment, fingerprint string) servedBroker {
	t.Helper()

	dir := t.TempDir()

	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, []byte(p.PrivateKeyPEM()), 0o600); err != nil {
		t.Fatalf("write the broker's App key: %v", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate the broker's key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "broker"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}

	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create the broker's certificate: %v", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse the broker's certificate: %v", err)
	}

	certPath := filepath.Join(dir, "broker.crt")
	if err := os.WriteFile(certPath, encodeCert(cert), 0o600); err != nil {
		t.Fatalf("write the broker's certificate: %v", err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("encode the broker's key: %v", err)
	}

	brokerKey := filepath.Join(dir, "broker.key")
	if err := os.WriteFile(brokerKey, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write the broker's key: %v", err)
	}

	picked, err := net.Listen("tcp", "127.0.0.1:0") //nolint:noctx // a port to name, closed on the next line
	if err != nil {
		t.Fatalf("pick a port: %v", err)
	}

	addr := picked.Addr().String()

	if err := picked.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)

	go func() {
		done <- app.ServeBroker(ctx, &config.BrokerConfig{
			Listen: addr, CertFile: certPath, KeyFile: brokerKey,
			Apps: []config.BrokerApp{{AppID: 12345, PrivateKeyPath: keyPath, APIURL: p.URL + "/api/v3"}},
			Deployments: []config.BrokerDeployment{{
				ID: deployment, Clients: []string{fingerprint},
				Installations: []config.BrokerInstallation{{AppID: 12345, InstallationID: 67890, Target: "acme"}},
			}},
		}, slog.Default())
	}()

	t.Cleanup(func() {
		cancel()

		if err := <-done; err != nil {
			t.Errorf("the broker stopped with: %v", err)
		}
	})

	deadline := time.Now().Add(30 * time.Second)

	for {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(t.Context(), "tcp", addr)
		if err == nil {
			_ = conn.Close()

			break
		}

		if time.Now().After(deadline) {
			t.Fatalf("the broker never listened: %v", err)
		}

		time.Sleep(20 * time.Millisecond)
	}

	return servedBroker{url: "https://" + addr, roots: certPath}
}

// brokerFor stands up a real broker and answers the token source this stack's deployment
// takes its tokens from.
func brokerFor(t *testing.T, dir string, p *plane) billetgithub.TokenSource {
	t.Helper()

	clientKey := filepath.Join(t.TempDir(), "client.key")

	fingerprint, err := tokenbroker.CreateClientKey(clientKey)
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	deployment, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	served := serveBroker(t, p, deployment, fingerprint)

	// BUILT THROUGH WHAT THE CLI BUILDS IT WITH: the config block, the hardened key
	// read and the identity peek, on the deployment's own identity directory.
	client, err := app.NewBrokerClient(&config.Config{Server: &config.ServerConfig{
		IdentityDir: dir,
		TokenBroker: &config.TokenBrokerConfig{URL: served.url, CAFile: served.roots, ClientKeyPath: clientKey},
	}})
	if err != nil {
		t.Fatalf("NewBrokerClient: %v", err)
	}

	return client.Tokens(billetgithub.OrganizationTarget("acme"), 12345, 67890)
}

// A STRANGER IS REFUSED IN THE HANDSHAKE, before it can send a request a handler would
// have to read, and that is the production listener's doing: no answer, not a 401.
func TestAServedBrokerRefusesAStrangerInTheHandshake(t *testing.T) {
	t.Parallel()

	p := newPlane(t)

	known := filepath.Join(t.TempDir(), "known.key")

	fingerprint, err := tokenbroker.CreateClientKey(known)
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	served := serveBroker(t, p, "0123456789abcdef0123456789abcdef", fingerprint)

	stranger := filepath.Join(t.TempDir(), "stranger.key")
	if _, err := tokenbroker.CreateClientKey(stranger); err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	post := func(keyPath string) (*http.Response, error) {
		t.Helper()

		key, err := tokenbroker.ReadClientKey(keyPath)
		if err != nil {
			t.Fatalf("ReadClientKey: %v", err)
		}

		cert, err := tokenbroker.ClientCertificate(key)
		if err != nil {
			t.Fatalf("ClientCertificate: %v", err)
		}

		roots, err := tokenbroker.LoadRoots(served.roots)
		if err != nil {
			t.Fatalf("LoadRoots: %v", err)
		}

		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert},
		}}}
		t.Cleanup(client.CloseIdleConnections)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
			served.url+"/v1/installation-token", strings.NewReader("{}"))
		if err != nil {
			t.Fatalf("request: %v", err)
		}

		return client.Do(req)
	}

	resp, err := post(stranger)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatalf("a stranger's request reached the handler, which answered %s", resp.Status)
	}

	// THE CONTROL: a key the broker knows gets as far as the handler, so what stopped the
	// stranger is the key.
	resp, err = post(known)
	if err != nil {
		t.Fatalf("a known key was refused too, so this proves nothing about the stranger: %v", err)
	}

	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if len(p.Calls("/access_tokens")) != 0 {
		t.Error("a request that names nothing registered reached GitHub")
	}
}

// A DEPLOYMENT RUNS ITS TIERS WITH TOKENS FROM THE BROKER AND NO KEY ON DISK. A job is
// offered, launched, started and completed through the real stack, on a client that
// holds no key: only the broker signed anything, and the token GitHub saw at
// registration is the one the broker minted.
func TestADeploymentRunsAJobOnTokensFromABrokerWithNoKeyOnDisk(t *testing.T) {
	s, _, _ := simulatedStack(t, overTheWire, withTokens(brokerFor))

	s.plane.queue(fakeactions.StatisticsJSON(1, 0),
		fakeactions.JobJSON("JobAvailable", 5201, "push", simulatedTier))

	stop := s.run(t)
	defer stop()

	deadline := time.Now().Add(30 * time.Second)
	for len(s.plane.acquiredIDs()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("billet never bid for the available job")
		}

		time.Sleep(50 * time.Millisecond)
	}

	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		fakeactions.JobJSON("JobAssigned", 5201, "push", simulatedTier))

	names := s.awaitOneRunning(t)

	leaseID, ours := provider.LeaseOf(names[0])
	if !ours {
		t.Fatalf("instance %q does not carry a lease", names[0])
	}

	started := fakeactions.JobFields{RunnerID: 99, RunnerName: names[0]}
	s.plane.queue(fakeactions.StatisticsJSON(0, 1),
		started.Apply(fakeactions.JobJSON("JobStarted", 5201, "push", simulatedTier)))

	completed := fakeactions.JobFields{RunnerID: 99, RunnerName: names[0], Result: "succeeded"}
	s.plane.queue(fakeactions.StatisticsJSON(0, 0),
		completed.Apply(fakeactions.JobJSON("JobCompleted", 5201, "push", simulatedTier)))

	s.awaitGone(t)

	deadline = time.Now().Add(30 * time.Second)
	for {
		rec, err := s.alloc.Job(t.Context(), leaseID)
		if err == nil && rec.Conclusion != "" {
			if rec.Conclusion != "done" {
				t.Fatalf("the job concluded %q", rec.Conclusion)
			}

			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the job's lease never ended: %+v, err %v", rec, err)
		}

		time.Sleep(100 * time.Millisecond)
	}

	// THE TOKEN GITHUB SAW IS THE ONE THE BROKER MINTED, which the App's JWT asked for.
	registrations := s.plane.Calls("/actions/runners/registration-token")
	if len(registrations) == 0 {
		t.Fatal("the deployment never asked for a registration token, so nothing here used a token")
	}

	for _, call := range registrations {
		if call.Authorization != "Bearer installation-token" {
			t.Errorf("a registration carried %q, want the token the broker minted", call.Authorization)
		}
	}

	exchanges := s.plane.Calls("/access_tokens")
	if len(exchanges) != 1 {
		t.Errorf("%d token exchanges for one deployment; the broker hands out what it holds", len(exchanges))
	}
}

// encodeCert is one certificate as PEM.
func encodeCert(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}
