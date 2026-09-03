// Package integration proves contracts that cross billet's enforced package layers.
package integration_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/store/ceph"
)

// A REAL CEPH CLONE, GROWN BY THE PRODUCTION CLIENT AND BOOTED BY THE PRODUCTION
// FIRECRACKER PROVIDER. The leaf-package tests prove each argv and API boundary in
// isolation; this proves those two real implementations compose into the ext4
// capacity the tier promised.
func TestRealCephGrowthBecomesFirecrackerGuestCapacity(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("not root: the jailer chroots, creates a device node and attaches a tap")
	}
	kvm, err := os.Open("/dev/kvm")
	if err != nil {
		t.Skipf("no usable /dev/kvm: %v", err)
	}
	if err := kvm.Close(); err != nil {
		t.Fatalf("close /dev/kvm after the preflight: %v", err)
	}
	if _, err := exec.LookPath("dumpe2fs"); err != nil {
		t.Skipf("dumpe2fs is not installed: %v", err)
	}

	image := requiredEnv(t, "BILLET_TEST_GOLDEN_IMAGE",
		"a golden image snapshot written image@generation")
	imagePool := requiredEnv(t, "BILLET_TEST_IMAGE_POOL", "the golden-image pool")
	cachePool := requiredEnv(t, "BILLET_TEST_CACHE_POOL", "the per-job clone pool")
	bridge := requiredEnv(t, "BILLET_TEST_BRIDGE", "the host bridge for the guest")
	kernel := requiredEnv(t, "BILLET_TEST_KERNEL", "an uncompressed guest kernel")

	cephCfg := config.CephConfig{
		ConfPath:    os.Getenv("BILLET_TEST_CEPH_CONF"),
		User:        os.Getenv("BILLET_TEST_CEPH_USER"),
		KeyringPath: os.Getenv("BILLET_TEST_CEPH_KEYRING"),
		ImagePool:   imagePool,
		CachePool:   cachePool,
	}
	if cephCfg.User == "" {
		cephCfg.User = config.DefaultCephUser
	}

	client, err := ceph.New(cephCfg)
	if err != nil {
		t.Fatalf("construct the production Ceph client: %v", err)
	}
	disk := &recordingDisk{Client: client}

	fcCfg := config.FirecrackerConfig{KernelImage: kernel, Bridge: bridge}
	fcCfg.Normalize()
	p, err := firecracker.New("0123456789abcdef0123456789abcdef", fcCfg, disk,
		firecracker.WithBootWait(20*time.Second))
	if err != nil {
		t.Fatalf("construct the production Firecracker provider: %v", err)
	}

	name := provider.InstanceName(freshLease(t))
	capacity := sourceCapacityPlusOneGiB(t, cephCfg, image)
	t.Cleanup(func() {
		if _, err := p.Destroy(context.WithoutCancel(t.Context()), name); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	if _, err := p.Launch(t.Context(), provider.Spec{
		Name: name, Image: image, VCPU: 1, Memory: 512 * config.MiB, Disk: capacity,
		Command: []string{"./run.sh"}, Trust: provider.TrustTrusted,
		JITConfig: "not-a-real-registration",
	}); err != nil {
		t.Fatalf("launch through production Ceph and Firecracker: %v", err)
	}

	if disk.device == "" {
		t.Fatal("the production Ceph client did not return a mapped root device")
	}
	if got := ext4Size(t, disk.device); got < int64(capacity) {
		t.Errorf("the guest root filesystem is %d bytes, want at least %d", got, capacity)
	}
}

type recordingDisk struct {
	*ceph.Client
	device string
}

func (d *recordingDisk) CloneRoot(
	ctx context.Context, image, name string, capacity config.ByteSize,
) (string, error) {
	device, err := d.Client.CloneRoot(ctx, image, name, capacity)
	if err == nil {
		d.device = device
	}

	return device, err
}

func requiredEnv(t *testing.T, name, purpose string) string {
	t.Helper()

	value := os.Getenv(name)
	if value == "" {
		t.Skipf("set %s to %s to run this", name, purpose)
	}

	return value
}

func sourceCapacityPlusOneGiB(t *testing.T, cfg config.CephConfig, image string) config.ByteSize {
	t.Helper()

	args := cephArgs(cfg, "--format", "json", "info", cfg.ImagePool+"/"+image)
	out, err := exec.CommandContext(t.Context(), "rbd", args...).Output()
	if err != nil {
		t.Fatalf("inspect the source image before the growth proof: %v", err)
	}
	var info struct {
		Size int64 `json:"size"`
	}
	if err := json.Unmarshal(out, &info); err != nil || info.Size <= 0 {
		t.Fatalf("source image did not report a positive size: %v, %s", err, out)
	}
	if info.Size > int64(config.ByteSize(1<<63-1)-config.GiB) {
		t.Fatalf("source image is too large for the growth proof: %d", info.Size)
	}

	return config.ByteSize(info.Size) + config.GiB
}

func cephArgs(cfg config.CephConfig, rest ...string) []string {
	args := make([]string, 0, len(rest)+6)
	if cfg.ConfPath != "" {
		args = append(args, "--conf", cfg.ConfPath)
	}
	if cfg.User != "" {
		args = append(args, "--id", cfg.User)
	}
	if cfg.KeyringPath != "" {
		args = append(args, "--keyring", cfg.KeyringPath)
	}

	return append(args, rest...)
}

func ext4Size(t *testing.T, device string) int64 {
	t.Helper()

	out, err := exec.CommandContext(t.Context(), "dumpe2fs", "-h", device).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect the guest root filesystem: %v: %s", err, strings.TrimSpace(string(out)))
	}

	values := make(map[string]int64)
	for _, line := range strings.Split(string(out), "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found || (key != "Block count" && key != "Block size") {
			continue
		}
		n, parseErr := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if parseErr != nil {
			t.Fatalf("parse %s from dumpe2fs: %v", key, parseErr)
		}
		values[key] = n
	}
	if values["Block count"] <= 0 || values["Block size"] <= 0 {
		t.Fatalf("dumpe2fs did not report a positive block count and size: %s", out)
	}

	return values["Block count"] * values["Block size"]
}

func freshLease(t *testing.T) string {
	t.Helper()

	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("mint a unique lease id: %v", err)
	}
	id := hex.EncodeToString(raw)
	if len(id) != 32 {
		t.Fatalf("the lease id has %d characters, want 32", len(id))
	}

	return id
}
