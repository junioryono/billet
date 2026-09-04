package wiring_test

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
)

// deploymentFixture is a config a control plane could start from: a state
// directory of its own, a github section naming a real key, and one docker
// tier. The key is real because the GitHub module reads and parses it.
type deploymentFixture struct {
	configPath string
	stateDir   string
	keyPath    string
}

func writeDeployment(t *testing.T) deploymentFixture {
	t.Helper()

	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	keyPath := filepath.Join(dir, "app.pem")

	if err := os.WriteFile(keyPath, testKeyPEM(t), 0o600); err != nil {
		t.Fatalf("write the app key: %v", err)
	}

	body := `
server:
  listen: 127.0.0.1:7717
  state_dir: ` + stateDir + `
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 2
  private_key_path: ` + keyPath + `
tiers:
  - label: billet-2vcpu
    provider: docker
    vcpu: 2
    memory: 8GiB
    image: ubuntu:24.04
`

	configPath := filepath.Join(dir, "billet.yaml")
	if err := os.WriteFile(configPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	return deploymentFixture{configPath: configPath, stateDir: stateDir, keyPath: keyPath}
}

func testKeyPEM(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}
