package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/store/ceph"
)

// A COMPUTE HOST WITH NO rbd FAILS THE CHECK.
//
// Only a firecracker node may carry a `node.ceph` block, so a file that has one
// always describes a machine meant to run jobs — and one without the client
// package cannot map a single volume. Reporting it and exiting zero would make
// `billet check` say a host is fine when nothing on it can launch, which is the
// opposite of what the command is for.
//
// A control plane is unaffected and needs no case here: with no node section
// cmdCheck never reaches this function.
//
// This test exists because the package-level one exercises `ceph.New`, and a
// mutation restoring the old "print unverified, return nil" behaviour in the CLI
// leaves that one green.
func TestAHostWithNoRBDFailsTheCheck(t *testing.T) {
	// Not parallel: it edits PATH, which is process-global and which t.Setenv
	// refuses to touch in a parallel test for exactly that reason.
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	err := checkCephCluster(t.Context(), &config.CephConfig{
		User:      config.DefaultCephUser,
		ImagePool: "billet-images",
		CachePool: "billet-cache",
	})
	if err == nil {
		t.Fatal("checkCephCluster reported success on a host with no rbd command")
	}

	if !errors.Is(err, ceph.ErrNoRBD) {
		t.Errorf("the failure is not ErrNoRBD, so a caller cannot tell it from an unreachable "+
			"cluster: %v", err)
	}

	// The pools, because the operator's next question is which cluster this file
	// was talking about, and the remedy, because it is the whole point of failing
	// here rather than at the first job.
	for _, want := range []string{"billet-images", "billet-cache", "ceph-common"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not carry %q: %v", want, err)
		}
	}

	// SAID ONCE. Every wrapper renders the message beneath it, so naming the
	// package at both layers put the same remedy on the terminal twice in one
	// sentence.
	if n := strings.Count(err.Error(), "ceph-common"); n != 1 {
		t.Errorf("the remedy appears %d times in one sentence: %v", n, err)
	}
}

// A CONFIG THE CHECK CANNOT ACT ON AT ALL IS REFUSED BEFORE THE PATH LOOKUP.
//
// `billet check` is reachable with a config that never went through Load — a
// caller building one, or a future command doing so — and the constructor is what
// re-applies the rules there. An identity of `admin` must not reach a cluster
// merely because rbd happened to be installed.
func TestTheCheckRefusesAnAdministratorBeforeItLooksForRBD(t *testing.T) {
	t.Setenv("PATH", filepath.Join(t.TempDir(), "empty"))

	err := checkCephCluster(t.Context(), &config.CephConfig{
		User:      "admin",
		ImagePool: "billet-images",
		CachePool: "billet-cache",
	})
	if err == nil {
		t.Fatal("checkCephCluster accepted an admin identity")
	}

	if errors.Is(err, ceph.ErrNoRBD) {
		t.Errorf("the refusal was about the missing binary rather than the identity: %v", err)
	}

	if !strings.Contains(err.Error(), "can delete the pools") {
		t.Errorf("the error does not explain the refusal: %v", err)
	}
}

// THE COMMAND CALLS IT, which is a different claim from "the helper works".
//
// Deleting cmdCheck's call leaves every helper-level test above green while
// `billet check` reports a healthy host that cannot map a volume — the exact
// regression this whole preflight exists to prevent, reintroduced by a deletion no
// test would notice.
func TestBilletCheckActuallyChecksTheCluster(t *testing.T) {
	// Not parallel: PATH is process-global.
	dir := t.TempDir()
	t.Setenv("PATH", filepath.Join(dir, "empty"))

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate a key: %v", err)
	}

	keyPath := filepath.Join(dir, "app.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600); err != nil {
		t.Fatalf("write the key: %v", err)
	}

	cfgPath := filepath.Join(dir, "billet.yaml")
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
  ceph:
    image_pool: billet-images
    cache_pool: billet-cache
tiers:
  - label: billet-4vcpu
    provider: firecracker
    vcpu: 4
    memory: 16GiB
    image: ubuntu-2404-x64
`, filepath.Join(dir, "server"), keyPath, filepath.Join(dir, "node"))

	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write the config: %v", err)
	}

	err = cmdCheck(t.Context(), []string{"--config", cfgPath})
	if err == nil {
		t.Fatal("billet check passed a firecracker host with no rbd command")
	}

	if !errors.Is(err, ceph.ErrNoRBD) {
		t.Errorf("the failure is not the missing client, so `billet check` may have stopped for "+
			"some other reason and never reached the cluster at all: %v", err)
	}
}
