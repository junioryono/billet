package scripts_test

import (
	"bytes"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/imagesource"
)

// TestTheManifestWriterAndTheGoReaderAgree is a round trip across the language
// boundary: the shell writes a manifest and billet's own reader parses it.
//
// THE TWO HALVES ARE WRITTEN IN DIFFERENT LANGUAGES AND HAVE TO AGREE EXACTLY.
// The reader refuses an unknown key, refuses a schema it does not know, and
// refuses a document that describes the root filesystem twice — so a jq
// expression that emitted `rootfs` beside `rootfs_multipart`, or spelled a field
// differently, produces a release nothing can import. That failure would
// otherwise be found by a node, after a build, after an upload.
func TestTheManifestWriterAndTheGoReaderAgree(t *testing.T) {
	t.Parallel()

	t.Run("a small image stays schema 1", func(t *testing.T) {
		t.Parallel()

		m := runManifestWriter(t, 4096, 0)

		if m.Schema != imagesource.SchemaV1 {
			t.Errorf("schema %d; an image that fits in one release asset must stay the layout "+
				"every deployed billet can read", m.Schema)
		}

		if m.RootfsMultipart != nil {
			t.Error("a one-asset image was described as multipart")
		}
	})

	t.Run("a large image becomes schema 2", func(t *testing.T) {
		t.Parallel()

		// SPLIT AT 1 KiB so the test makes real parts without writing gigabytes.
		// The production part size is a constant in split-image.sh; what is under
		// test here is the splitting and describing, not the number.
		m := runManifestWriter(t, 4096, 1024)

		if m.Schema != imagesource.SchemaV2 {
			t.Fatalf("schema %d; an image that had to be split cannot be described by a "+
				"layout with nowhere to put the parts", m.Schema)
		}

		if m.RootfsMultipart == nil {
			t.Fatal("a split image carries no parts")
		}

		if (m.Rootfs != imagesource.Asset{}) {
			t.Error("a split image also carries a single-asset rootfs; the reader refuses a " +
				"document that describes the image twice")
		}

		if n := len(m.RootfsMultipart.Parts); n != 4 {
			t.Errorf("4096 bytes split at 1024 produced %d parts, want 4", n)
		}
	})
}

// TestASplitImageReassemblesIntoTheOriginal is the property the whole scheme
// rests on, proven against bytes the shell actually produced.
//
// NOT A HAND-BUILT FIXTURE. The parts here were cut by split-image.sh and
// described by write-image-manifest.sh, so this fails if either changes in a way
// that stops round-tripping — which a Go-only test with a Go-made fixture could
// not detect.
func TestASplitImageReassemblesIntoTheOriginal(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	original := makeRootfs(t, dir, 4096)

	m := runManifestWriterIn(t, dir, original, 1024)

	if m.RootfsMultipart == nil {
		t.Fatal("the image was not split")
	}

	// THE PARTS ARE STAGED THE WAY A PULL STAGES THEM: by their published names,
	// in a directory of their own, with the original absent. Assembling beside the
	// original would let a bug that never wrote anything still find the right file.
	staging := t.TempDir()

	for _, part := range m.RootfsMultipart.Parts {
		body, err := os.ReadFile(filepath.Join(dir, part.Name))
		if err != nil {
			t.Fatalf("read the produced part %s: %v", part.Name, err)
		}

		if err := os.WriteFile(filepath.Join(staging, part.Name), body, 0o600); err != nil {
			t.Fatalf("stage %s: %v", part.Name, err)
		}
	}

	path, err := imagesource.AssembleRootfs(staging, m.RootfsImage())
	if err != nil {
		t.Fatalf("AssembleRootfs against parts the shell produced: %v", err)
	}

	want, err := os.ReadFile(original)
	if err != nil {
		t.Fatalf("read the original: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the reassembled image: %v", err)
	}

	if !bytes.Equal(got, want) {
		t.Error("the reassembled image differs from the one that was split")
	}
}

// TestSplitLeavesNoPartsFromAnEarlierLargerImage: a rebuild in the same workspace
// must not leave a stale part beside the new set, because the manifest lists what
// it is told and would describe a set that does not concatenate.
func TestSplitLeavesNoPartsFromAnEarlierLargerImage(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	img := makeRootfs(t, dir, 8192)

	if out := runSplit(t, img, 1024); len(out) != 8 {
		t.Fatalf("the first split produced %d parts, want 8", len(out))
	}

	// A SMALLER SECOND BUILD. Without the cleanup, parts 4..7 from the first
	// remain and a glob would pick them up.
	smaller := makeRootfs(t, dir, 4096)
	if smaller != img {
		t.Fatalf("the fixture moved: %s", smaller)
	}

	out := runSplit(t, img, 1024)
	if len(out) != 4 {
		t.Errorf("the second split reported %d parts, want 4", len(out))
	}

	leftovers, err := filepath.Glob(img + ".part*")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}

	if len(leftovers) != 4 {
		t.Errorf("%d part files are on disk after a smaller rebuild, want 4; a stale part "+
			"from a larger image would be described as part of the new one", len(leftovers))
	}
}

func makeRootfs(t *testing.T, dir string, size int) string {
	t.Helper()

	// RANDOM, NOT ZEROS. Zeros make every part identical, so a test that joined
	// them in the wrong order would still produce the right bytes.
	body := make([]byte, size)
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("generate a rootfs: %v", err)
	}

	path := filepath.Join(dir, "rootfs.img.zst")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write a rootfs: %v", err)
	}

	return path
}

func runSplit(t *testing.T, image string, partBytes int) []string {
	t.Helper()

	cmd := exec.CommandContext(t.Context(), repoScript(t, "split-image.sh"),
		image, itoa(partBytes))

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("split-image.sh: %v\n%s", err, out)
	}

	var parts []string

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			parts = append(parts, line)
		}
	}

	return parts
}

func runManifestWriter(t *testing.T, size, partBytes int) imagesource.Manifest {
	t.Helper()

	dir := t.TempDir()

	return runManifestWriterIn(t, dir, makeRootfs(t, dir, size), partBytes)
}

func runManifestWriterIn(t *testing.T, dir, image string, partBytes int) imagesource.Manifest {
	t.Helper()

	kernel := filepath.Join(dir, "vmlinux-billet")

	// THE WRITER READS A VERSION OUT OF THE KERNEL with `strings`, and refuses a
	// binary it cannot identify -- so the fixture has to carry that exact line.
	if err := os.WriteFile(kernel, []byte("Linux version 6.1.155 (billet@test)\n"), 0o600); err != nil {
		t.Fatalf("write a kernel: %v", err)
	}

	info := filepath.Join(dir, "build-info.env")
	if err := os.WriteFile(info, []byte(
		"RUNNER_VERSION=2.336.0\nGUEST_CONTRACT=9\nARCH=x86_64\n"), 0o600); err != nil {
		t.Fatalf("write build-info.env: %v", err)
	}

	var parts string
	if partBytes > 0 {
		parts = strings.Join(runSplit(t, image, partBytes), "\n")
	}

	cmd := exec.CommandContext(t.Context(), repoScript(t, "write-image-manifest.sh"))
	cmd.Env = append(os.Environ(),
		"ROOTFS="+image,
		"KERNEL="+kernel,
		"INFO="+info,
		"ROOTFS_PARTS="+parts,
	)

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("write-image-manifest.sh: %v", err)
	}

	// PARSED BY THE READER THAT WILL PARSE IT IN PRODUCTION, strict keys and all.
	m, err := imagesource.ParseManifest(out)
	if err != nil {
		t.Fatalf("billet's own reader refuses the manifest the build writes: %v\n%s", err, out)
	}

	return *m
}

func repoScript(t *testing.T, name string) string {
	t.Helper()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	return filepath.Join(wd, name)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}

	var digits []byte

	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}

	return string(digits)
}
