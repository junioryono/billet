package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/imagesource"
)

// THE ORDER IS FLAG, THEN CONFIG, THEN THE BUILT-IN. A deployment that mirrors
// internally says so once; the flag is for a one-off.
func TestResolveSourcePrefersTheFlagThenTheConfig(t *testing.T) {
	withConfig := &config.Config{Images: &config.ImagesConfig{
		Source: "https://mirror.internal/billet",
	}}

	got, err := resolveSource(withConfig, "https://oneoff.test/images")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got.BaseURL != "https://oneoff.test/images" {
		t.Errorf("the flag did not win: %q", got.BaseURL)
	}

	got, err = resolveSource(withConfig, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got.BaseURL != "https://mirror.internal/billet" {
		t.Errorf("the config was not used: %q", got.BaseURL)
	}

	// AND A DEPLOYMENT THAT SAYS NOTHING GETS BILLET'S OWN IMAGES, which is the
	// whole point of publishing them centrally.
	got, err = resolveSource(&config.Config{}, "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got.BaseURL != imagesource.DefaultBaseURL {
		t.Errorf("an unconfigured deployment did not fall back to the default: %q", got.BaseURL)
	}
}

// A BLANK VALUE IS NOT A SOURCE. An `images:` section left in a config with its
// source commented out must not resolve to the empty string and fail obscurely.
func TestResolveSourceIgnoresABlankConfiguredSource(t *testing.T) {
	for _, blank := range []string{"", "   ", "\t"} {
		got, err := resolveSource(&config.Config{
			Images: &config.ImagesConfig{Source: blank},
		}, "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}

		if got.BaseURL != imagesource.DefaultBaseURL {
			t.Errorf("a blank configured source resolved to %q", got.BaseURL)
		}
	}
}

func TestResolveSourceRefusesAnUnusableOne(t *testing.T) {
	if _, err := resolveSource(&config.Config{}, "http://plaintext.test/images"); err == nil {
		t.Error("a plaintext source was accepted through the flag")
	}

	if _, err := resolveSource(&config.Config{
		Images: &config.ImagesConfig{Source: "not a url at all"},
	}, ""); err == nil {
		t.Error("a configured source that is not a url was accepted")
	}
}

// GO'S ARCHITECTURE NAMES ARE NOT uname'S, and a manifest records what the build
// recorded, which is uname's. Getting this wrong refuses every image on the
// architecture it names.
func TestHostArchIsSpelledTheWayAManifestSpellsIt(t *testing.T) {
	got := hostArch()

	for _, wrong := range []string{"amd64", "arm64"} {
		if got == wrong {
			t.Fatalf("hostArch returned %q, which is go's spelling; a manifest records "+
				"what uname -m says", got)
		}
	}

	if got == "" {
		t.Fatal("hostArch returned nothing")
	}
}

func stageDir(t *testing.T, rootfs, kernel []byte) (string, *imagesource.Manifest) {
	t.Helper()

	dir := t.TempDir()

	digest := func(b []byte) string {
		sum := sha256.Sum256(b)

		return hex.EncodeToString(sum[:])
	}

	m := &imagesource.Manifest{
		Schema:        imagesource.SchemaVersion,
		GuestContract: "2",
		Arch:          "x86_64",
		RunnerVersion: "2.336.0",
		BuiltAt:       time.Now().UTC().Truncate(time.Second),
		Rootfs: imagesource.Asset{
			Name:   "rootfs.img",
			SHA256: digest(rootfs),
			Size:   int64(len(rootfs)),
		},
		Kernel: imagesource.Asset{
			Name:    "vmlinux-billet",
			SHA256:  digest(kernel),
			Size:    int64(len(kernel)),
			Version: "6.1.155",
		},
	}

	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"), rootfs, 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "vmlinux-billet"), kernel, 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}

	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, imagesource.ManifestName), data, 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}

	return dir, m
}

// A FILE THAT ARRIVED ON A USB STICK IS NO MORE TRUSTWORTHY than one that arrived
// over http -- less, arguably, since nothing about its journey is even in
// principle observable.
func TestVerifyLocalChecksTheSameDigestsTheNetworkPathDoes(t *testing.T) {
	dir, m := stageDir(t, []byte("a root filesystem"), []byte("a kernel"))

	if err := verifyLocal(dir, m); err != nil {
		t.Fatalf("a correctly staged directory was refused: %v", err)
	}

	// Substituted content, correct length: only the digest catches this.
	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"),
		[]byte("A ROOT FILESYSTEM"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	err := verifyLocal(dir, m)
	if err == nil {
		t.Fatal("content substituted at the same length was accepted; the size check alone " +
			"cannot catch it and the digest is the only thing that can")
	}

	if !strings.Contains(err.Error(), "not the file that was signed") {
		t.Errorf("the refusal does not say what is wrong: %v", err)
	}
}

func TestVerifyLocalReportsAMissingAsset(t *testing.T) {
	dir, m := stageDir(t, []byte("rootfs"), []byte("kernel"))

	if err := os.Remove(filepath.Join(dir, "vmlinux-billet")); err != nil {
		t.Fatalf("remove: %v", err)
	}

	err := verifyLocal(dir, m)
	if err == nil {
		t.Fatal("a directory missing an asset was accepted")
	}

	if !strings.Contains(err.Error(), "vmlinux-billet") {
		t.Errorf("the refusal does not name the missing asset: %v", err)
	}
}

func TestVerifyLocalReportsAWrongSize(t *testing.T) {
	dir, m := stageDir(t, []byte("rootfs"), []byte("kernel"))

	if err := os.WriteFile(filepath.Join(dir, "rootfs.img"),
		[]byte("rootfs and then some"), 0o600); err != nil {
		t.Fatalf("rewrite: %v", err)
	}

	if err := verifyLocal(dir, m); err == nil {
		t.Fatal("an asset of the wrong size was accepted")
	}
}

// /tmp IS tmpfs ON MOST DISTRIBUTIONS -- it is RAM -- and a guest image
// decompresses to four gigabytes. Staging there would exhaust memory or push the
// machine into swap partway through an import holding a cluster-wide lock.
func TestStagingDefaultIsNotMemoryBacked(t *testing.T) {
	if strings.HasPrefix(DefaultStagingDir, "/tmp/") || DefaultStagingDir == "/tmp" {
		t.Fatalf("the default staging directory is %s; /tmp is tmpfs on most distributions "+
			"and this unpacks four gigabytes into it", DefaultStagingDir)
	}
}

func TestHumanBytesReadsTheWayAnOperatorReadsIt(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1536, "1.5 KiB"},
		{397_000_000, "378.6 MiB"},
		{4 << 30, "4.0 GiB"},
	} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// THE KERNEL HAS TO OUTLIVE THE PULL THAT FETCHED IT.
//
// The first version of this reported the kernel's path inside the staging
// directory, which is removed when the pull finishes -- so it told the operator to
// point node.firecracker.kernel_image at a file it had just deleted. Nothing in
// the code could have said so; it was found by pulling against a real cluster and
// then looking for the file.
//
// This asserts the property that was missing: the installed kernel is still there
// after the staging directory is gone.
func TestInstallKernelSurvivesTheStagingDirectory(t *testing.T) {
	staging := t.TempDir()
	kernels := t.TempDir()

	_, m := stageDir(t, []byte("rootfs"), []byte("a kernel"))

	// stageDir made its own directory; restage the kernel where the test controls it.
	if err := os.WriteFile(filepath.Join(staging, m.Kernel.Name), []byte("a kernel"), 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}

	got, err := installKernel(m, staging, kernels)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	// THE STAGING DIRECTORY GOES AWAY, exactly as the pull's deferred cleanup does.
	if err := os.RemoveAll(staging); err != nil {
		t.Fatalf("remove staging: %v", err)
	}

	content, err := os.ReadFile(got)
	if err != nil {
		t.Fatalf("the kernel the pull reported does not survive the staging directory, "+
			"so the path printed to the operator names a file that no longer exists: %v", err)
	}

	if string(content) != "a kernel" {
		t.Errorf("the installed kernel holds %q", content)
	}

	if strings.HasPrefix(got, staging) {
		t.Errorf("the kernel was installed inside the staging directory (%s), which is "+
			"removed when the pull finishes", got)
	}
}

// NAMED BY VERSION AND DIGEST. Version alone is not enough: two builds can produce
// the same kernel version from different sources, and silently overwriting one
// with the other repoints every generation verified against the first.
func TestInstallKernelSeparatesDifferentKernelsWithOneVersion(t *testing.T) {
	kernels := t.TempDir()

	first := t.TempDir()
	second := t.TempDir()

	_, a := stageDir(t, []byte("rootfs"), []byte("kernel one"))
	_, b := stageDir(t, []byte("rootfs"), []byte("kernel two"))

	if a.Kernel.Version != b.Kernel.Version {
		t.Fatal("the fixture should give both the same version")
	}

	if err := os.WriteFile(filepath.Join(first, a.Kernel.Name), []byte("kernel one"), 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}

	if err := os.WriteFile(filepath.Join(second, b.Kernel.Name), []byte("kernel two"), 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}

	pathA, err := installKernel(a, first, kernels)
	if err != nil {
		t.Fatalf("install a: %v", err)
	}

	pathB, err := installKernel(b, second, kernels)
	if err != nil {
		t.Fatalf("install b: %v", err)
	}

	if pathA == pathB {
		t.Fatalf("two different kernels of version %s were installed to one path (%s); the "+
			"second silently repointed every generation verified against the first",
			a.Kernel.Version, pathA)
	}

	for path, want := range map[string]string{pathA: "kernel one", pathB: "kernel two"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}

		if string(got) != want {
			t.Errorf("%s holds %q, want %q", path, got, want)
		}
	}
}

// RE-PULLING THE SAME IMAGE MUST NOT FAIL. The name carries the digest, so a file
// already at that path IS this kernel.
func TestInstallKernelIsIdempotent(t *testing.T) {
	staging := t.TempDir()
	kernels := t.TempDir()

	_, m := stageDir(t, []byte("rootfs"), []byte("a kernel"))

	if err := os.WriteFile(filepath.Join(staging, m.Kernel.Name), []byte("a kernel"), 0o600); err != nil {
		t.Fatalf("stage: %v", err)
	}

	first, err := installKernel(m, staging, kernels)
	if err != nil {
		t.Fatalf("first install: %v", err)
	}

	second, err := installKernel(m, staging, kernels)
	if err != nil {
		t.Fatalf("installing the same kernel twice failed: %v", err)
	}

	if first != second {
		t.Errorf("the same kernel installed to two paths: %s and %s", first, second)
	}
}
