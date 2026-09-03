package imagesource

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageParts writes real bytes for each part and returns the multipart that
// describes them.
func stageParts(t *testing.T, dir string, chunks ...[]byte) Multipart {
	t.Helper()

	var whole []byte

	img := Multipart{Name: "rootfs.img.zst"}

	for i, chunk := range chunks {
		name := "rootfs.img.zst.part" + string(rune('0'+i))

		if err := os.WriteFile(filepath.Join(dir, name), chunk, 0o600); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}

		img.Parts = append(img.Parts, Asset{
			Name:   name,
			SHA256: digestOf(chunk),
			Size:   int64(len(chunk)),
		})

		whole = append(whole, chunk...)
	}

	img.SHA256 = digestOf(whole)
	img.Size = int64(len(whole))

	return img
}

func TestAssembleJoinsPartsInPublishedOrder(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	img := stageParts(t, dir, []byte("alpha"), []byte("bravo"), []byte("charlie"))

	path, err := AssembleRootfs(dir, img)
	if err != nil {
		t.Fatalf("AssembleRootfs: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the assembled file: %v", err)
	}

	if string(got) != "alphabravocharlie" {
		t.Errorf("assembled %q, want %q", got, "alphabravocharlie")
	}

	// THE PARTS ARE COLLECTED, but only after the whole was proven.
	for _, part := range img.Parts {
		if _, err := os.Stat(filepath.Join(dir, part.Name)); !os.IsNotExist(err) {
			t.Errorf("the staged part %s outlived a successful assembly; on a parity-sized "+
				"image that is tens of gigabytes nothing will collect", part.Name)
		}
	}
}

// TestReorderedPartsAreRefused is the property the whole-file digest exists for,
// and the one per-part digests cannot provide.
//
// EVERY PART HERE IS A PART THE MANIFEST SIGNED FOR, with a matching digest and
// length. Only their order is wrong — which is what an off-by-one, a lexical sort
// putting part10 before part2, or a retry that appended twice would produce.
func TestReorderedPartsAreRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	img := stageParts(t, dir, []byte("alpha"), []byte("bravo"), []byte("charlie"))

	img.Parts[0], img.Parts[2] = img.Parts[2], img.Parts[0]

	_, err := AssembleRootfs(dir, img)
	if err == nil {
		t.Fatal("parts joined in the wrong order were accepted; every one of them matches " +
			"its own published digest, so nothing but the whole-file digest can refuse this")
	}

	if !strings.Contains(err.Error(), "did not join into the published file") {
		t.Errorf("refused with %q, which does not explain that the parts were genuine and "+
			"the joining was wrong", err)
	}
}

func TestADuplicatedPartIsRefused(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	img := stageParts(t, dir, []byte("alpha"), []byte("bravo"))

	// THE SAME LENGTH, so the running total still reaches the published size and
	// only the digest can catch it.
	img.Parts[1] = img.Parts[0]

	if _, err := AssembleRootfs(dir, img); err == nil {
		t.Fatal("a part repeated in place of another was accepted")
	}
}

// TestATruncatedPartIsNamed proves the per-part length check reports which part
// is wrong rather than leaving a whole-image digest mismatch to be diagnosed.
func TestATruncatedPartIsNamed(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	img := stageParts(t, dir, []byte("alpha"), []byte("bravo"))

	short := filepath.Join(dir, img.Parts[1].Name)
	if err := os.WriteFile(short, []byte("bra"), 0o600); err != nil {
		t.Fatalf("truncate a part: %v", err)
	}

	_, err := AssembleRootfs(dir, img)
	if err == nil {
		t.Fatal("a truncated part was accepted")
	}

	if !strings.Contains(err.Error(), img.Parts[1].Name) {
		t.Errorf("refused with %q, which does not name the part that is wrong", err)
	}
}

// TestAFailedAssemblyKeepsTheParts: they are the only way to rebuild the file, so
// discarding them turns a failed check into a re-download of everything.
func TestAFailedAssemblyKeepsTheParts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	img := stageParts(t, dir, []byte("alpha"), []byte("bravo"))
	img.SHA256 = strings.Repeat("e", 64)

	if _, err := AssembleRootfs(dir, img); err == nil {
		t.Fatal("a wrong whole-file digest was accepted")
	}

	for _, part := range img.Parts {
		if _, err := os.Stat(filepath.Join(dir, part.Name)); err != nil {
			t.Errorf("the staged part %s was removed after a failed assembly, so a retry has "+
				"to fetch every part again: %v", part.Name, err)
		}
	}

	// AND NOTHING CARRIES THE ASSEMBLED NAME. A file under that name is one a
	// later step would treat as the verified image.
	if _, err := os.Stat(filepath.Join(dir, img.Name)); !os.IsNotExist(err) {
		t.Error("a failed assembly left a file under the assembled name, which a later step " +
			"would read as the verified root filesystem")
	}
}

// TestASingleAssetImageStillRunsTheAggregateCheck is the schema 1 path.
//
// IT IS THE BRANCH MOST LIKELY TO ROT: it verifies in place to avoid copying
// several hundred megabytes onto itself, and an implementation that "optimized"
// it into a no-op would leave schema 1 images unverified by this function while
// every test about ordering still passed.
func TestASingleAssetImageStillRunsTheAggregateCheck(t *testing.T) {
	t.Parallel()

	t.Run("a matching image is returned", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		body := []byte("the whole root filesystem")

		if err := os.WriteFile(filepath.Join(dir, "rootfs.img.zst"), body, 0o600); err != nil {
			t.Fatalf("stage: %v", err)
		}

		img := Multipart{
			Name:   "rootfs.img.zst",
			SHA256: digestOf(body),
			Size:   int64(len(body)),
			Parts: []Asset{{
				Name: "rootfs.img.zst", SHA256: digestOf(body), Size: int64(len(body)),
			}},
		}

		path, err := AssembleRootfs(dir, img)
		if err != nil {
			t.Fatalf("AssembleRootfs: %v", err)
		}

		if path != filepath.Join(dir, "rootfs.img.zst") {
			t.Errorf("returned %q", path)
		}
	})

	t.Run("a corrupted image is refused", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		body := []byte("the whole root filesystem")

		if err := os.WriteFile(filepath.Join(dir, "rootfs.img.zst"), body, 0o600); err != nil {
			t.Fatalf("stage: %v", err)
		}

		img := Multipart{
			Name:   "rootfs.img.zst",
			SHA256: digestOf([]byte("something else entirely")),
			Size:   int64(len(body)),
			Parts: []Asset{{
				Name: "rootfs.img.zst", SHA256: digestOf(body), Size: int64(len(body)),
			}},
		}

		if _, err := AssembleRootfs(dir, img); err == nil {
			t.Fatal("the single-asset path returned without checking the whole-file digest, " +
				"so a schema 1 image is never verified by the function that exists to " +
				"verify it")
		}
	})
}

func TestAssembleRefusesAnImageWithNoParts(t *testing.T) {
	t.Parallel()

	if _, err := AssembleRootfs(t.TempDir(), Multipart{Name: "rootfs.img"}); err == nil {
		t.Fatal("an image with no parts was assembled")
	}
}
