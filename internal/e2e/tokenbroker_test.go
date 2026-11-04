package e2e

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io/fs"
	"log/slog"
	"net/http/httptest"
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

// brokerFor stands up a real broker holding the fake App's key, on a real handshake,
// and answers the token source this stack's deployment takes its tokens from.
func brokerFor(t *testing.T, dir string, p *plane) billetgithub.TokenSource {
	t.Helper()

	keyPath := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(keyPath, []byte(p.PrivateKeyPEM()), 0o600); err != nil {
		t.Fatalf("write the broker's App key: %v", err)
	}

	clientKey := filepath.Join(t.TempDir(), "client.key")

	fingerprint, err := tokenbroker.CreateClientKey(clientKey)
	if err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	deployment, err := state.DeploymentID(dir)
	if err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	server, err := tokenbroker.NewServer(&config.BrokerConfig{
		Listen: "unused", CertFile: "unused", KeyFile: "unused",
		Apps: []config.BrokerApp{{AppID: 12345, PrivateKeyPath: keyPath, APIURL: p.URL + "/api/v3"}},
		Deployments: []config.BrokerDeployment{{
			ID: deployment, Clients: []string{fingerprint},
			Installations: []config.BrokerInstallation{{AppID: 12345, InstallationID: 67890, Target: "acme"}},
		}},
	}, slog.Default())
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}

	srv := httptest.NewUnstartedServer(server)
	srv.TLS = &tls.Config{ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	roots := filepath.Join(t.TempDir(), "broker-ca.pem")
	if err := os.WriteFile(roots, encodeCert(srv.Certificate()), 0o644); err != nil { //nolint:gosec // a public certificate
		t.Fatalf("write the broker's root: %v", err)
	}

	// BUILT THROUGH WHAT THE CLI BUILDS IT WITH: the config block, the hardened key
	// read and the identity peek, on the deployment's own identity directory.
	client, err := app.NewBrokerClient(&config.Config{Server: &config.ServerConfig{
		IdentityDir: dir,
		TokenBroker: &config.TokenBrokerConfig{URL: srv.URL, CAFile: roots, ClientKeyPath: clientKey},
	}})
	if err != nil {
		t.Fatalf("NewBrokerClient: %v", err)
	}

	return client.Tokens(billetgithub.OrganizationTarget("acme"), 12345, 67890)
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

	// AND NO FILE UNDER THE DEPLOYMENT'S DIRECTORY HOLDS THE APP'S KEY.
	key := []byte(s.plane.PrivateKeyPEM())

	if err := filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if !d.Type().IsRegular() {
			return nil
		}

		body, err := os.ReadFile(path) //nolint:gosec // a file this test's own deployment wrote
		if err != nil {
			return err
		}

		if bytes.Contains(body, key) || strings.Contains(string(body), "RSA PRIVATE KEY") {
			t.Errorf("%s holds an App key", path)
		}

		return nil
	}); err != nil {
		t.Fatalf("walk the deployment's directory: %v", err)
	}
}

// encodeCert is one certificate as PEM.
func encodeCert(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
}
