package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

type fakeDater struct {
	newest map[string]time.Time
	err    error
}

func (f fakeDater) NewestGeneration(_ context.Context, image string) (ceph.Generation, bool, error) {
	if f.err != nil {
		return ceph.Generation{}, false, f.err
	}
	built, ok := f.newest[image]
	if !ok {
		return ceph.Generation{}, false, nil
	}
	return ceph.Generation{Name: image + "-gen", Built: built}, true, nil
}

// refreshHarness points `images refresh` at a fake cluster and counts the pulls.
func refreshHarness(t *testing.T, dater fakeDater) (string, *[]string) {
	t.Helper()

	var pulled []string
	prevOpen, prevPull := openGenerationDater, pullImage
	openGenerationDater = func(*config.Config) (generationDater, error) { return dater, nil }
	pullImage = func(_ context.Context, _, image string) error {
		pulled = append(pulled, image)
		return nil
	}
	t.Cleanup(func() { openGenerationDater, pullImage = prevOpen, prevPull })

	return refreshConfig(t), &pulled
}

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

// A generation inside the cadence is left alone; one older than it is pulled
// through the same `images pull --verify` an operator runs.
func TestRefreshPullsOnlyWhatIsDue(t *testing.T) {
	cfg, pulled := refreshHarness(t, fakeDater{newest: map[string]time.Time{
		"ubuntu-2404-x64": time.Now().Add(-8 * 24 * time.Hour),
	}})

	if err := cmdImagesRefresh(t.Context(), []string{"--config", cfg}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(*pulled) != 1 || (*pulled)[0] != "ubuntu-2404-x64" {
		t.Fatalf("pulled %v, want the one stale image", *pulled)
	}

	cfg, pulled = refreshHarness(t, fakeDater{newest: map[string]time.Time{
		"ubuntu-2404-x64": time.Now().Add(-1 * time.Hour),
	}})
	if err := cmdImagesRefresh(t.Context(), []string{"--config", cfg}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(*pulled) != 0 {
		t.Fatalf("pulled %v for a generation an hour old", *pulled)
	}
}

// NOTHING PUBLISHED IS DUE, not "nothing to do": a node whose cluster holds no
// generation of its tier's image launches nothing.
func TestRefreshPullsAnImageTheClusterHasNeverSeen(t *testing.T) {
	cfg, pulled := refreshHarness(t, fakeDater{newest: map[string]time.Time{}})

	if err := cmdImagesRefresh(t.Context(), []string{"--config", cfg}); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if len(*pulled) != 1 {
		t.Fatalf("pulled %v, want the unpublished image", *pulled)
	}
}

// A cluster that could not answer is an error, never a pull and never a quiet
// exit: a timer that swallowed it would look like a working schedule.
func TestRefreshReportsAClusterItCouldNotAsk(t *testing.T) {
	cfg, pulled := refreshHarness(t, fakeDater{err: errors.New("rbd: connection refused")})

	err := cmdImagesRefresh(t.Context(), []string{"--config", cfg})
	if err == nil || len(*pulled) != 0 {
		t.Fatalf("refresh = %v with pulls %v, want the cluster's error and no pull", err, *pulled)
	}
}
