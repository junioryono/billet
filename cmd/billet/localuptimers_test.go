package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// refreshConfig writes a config for a host that is both a control plane and a
// firecracker node with a Ceph cluster, with a real App key beside it, so `up`
// can load it the way it loads an operator's. It is the host that gets every
// packaged timer.
func refreshConfig(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}

	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(dir, "billet.yaml")
	body := fmt.Sprintf(`server:
  listen: 127.0.0.1:7717
  state_dir: %s
  max_vcpu: 8
  max_memory: 32GiB
github:
  org: acme
  app_id: 1
  installation_id: 1
  private_key_path: %s
node:
  name: epyc-1
  server_addr: 127.0.0.1:7717
  provider: firecracker
  state_dir: %s
  firecracker:
    kernel_image: /var/lib/billet/vmlinux
    bridge: br0
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
tiers:
  - label: billet-4vcpu
    provider: firecracker
    vcpu: 4
    memory: 16GiB
    image: ubuntu-2404-x64@verified
`, filepath.Join(dir, "server"), keyPath, filepath.Join(dir, "node"))

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}
