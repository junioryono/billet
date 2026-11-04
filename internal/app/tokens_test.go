package app

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/state"
	"github.com/junioryono/billet/internal/tokenbroker"
)

// brokerDeployment is a config on a broker, with its identity and client key on disk.
func brokerDeployment(t *testing.T) (*config.Config, config.GitHubTarget) {
	t.Helper()

	dir := t.TempDir()
	clientKey := filepath.Join(t.TempDir(), "client.key")

	if _, err := tokenbroker.CreateClientKey(clientKey); err != nil {
		t.Fatalf("CreateClientKey: %v", err)
	}

	if _, err := state.DeploymentID(dir); err != nil {
		t.Fatalf("DeploymentID: %v", err)
	}

	cfg := &config.Config{Server: &config.ServerConfig{
		IdentityDir: dir,
		TokenBroker: &config.TokenBrokerConfig{URL: "https://broker.invalid:8443", ClientKeyPath: clientKey},
	}}

	return cfg, config.GitHubTarget{Name: "default", Org: "acme", AppID: 12345, InstallationID: 67890}
}

// A DEPLOYMENT ON A BROKER BUILDS ITS CLIENTS WITH NO KEY. Each path goes to the key
// otherwise, which refuses under a broker, so what is proved here is the branch.
func TestTheAssemblyBuildsOnABrokerWithNoKey(t *testing.T) {
	t.Parallel()

	cfg, target := brokerDeployment(t)

	client, err := NewScaleSetClientFor(t.Context(), cfg, target)
	if err != nil {
		t.Fatalf("NewScaleSetClientFor: %v", err)
	}
	if client == nil {
		t.Fatal("NewScaleSetClientFor answered no client")
	}

	if _, err := ResolveAppKey(t.Context(), cfg, target); err == nil ||
		!strings.Contains(err.Error(), "server.token_broker") {
		t.Errorf("ResolveAppKey on a broker answered %v; a caller that asks for a key has to be told", err)
	}
}
