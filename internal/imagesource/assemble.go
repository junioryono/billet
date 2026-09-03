package imagesource

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// AssembleRootfs joins a downloaded root filesystem into one verified file and
// returns the path it landed at.
//
// THIS IS THE ONLY WAY TO GET A USABLE ROOT FILESYSTEM PATH, and that is the
// design rather than a convenience. Every route ends at one check — the digest of
// the assembled bytes against what the signed manifest published for the whole
// file — so there is no shorter path that stops after the individual parts.
//
// WHY THE PER-PART DIGESTS ARE NOT ENOUGH. Download checks each part against the
// manifest as it arrives, which proves every piece is a piece the publisher
// signed for. It says nothing about ORDER. A manifest is signed as a document, so
// an attacker cannot rewrite it — but a reader that concatenated parts and
// trusted the per-part checks would accept its own mistake: an off-by-one in the
// loop, a directory listing sorted lexically where part10 precedes part2, a retry
// that appended a part twice. Every one of those produces a file made entirely of
// signed bytes and is a different filesystem. The whole-file digest is what
// refuses it, and it costs one pass over data already on disk.
//
// A SINGLE-PART IMAGE IS VERIFIED IN PLACE rather than copied onto itself. The
// branch is about avoiding a needless copy of several hundred megabytes; both
// branches finish at the same verification, so the property this function exists
// for does not depend on which one ran.
func AssembleRootfs(dir string, img Multipart) (string, error) {
	if len(img.Parts) == 0 {
		return "", fmt.Errorf("imagesource: the root filesystem has no parts to assemble")
	}

	out := filepath.Join(dir, img.Name)

	if len(img.Parts) == 1 && img.Parts[0].Name == img.Name {
		if err := verifyAssembled(out, img); err != nil {
			return "", err
		}

		return out, nil
	}

	// JOINED UNDER A TEMPORARY NAME, VERIFIED, AND ONLY THEN GIVEN THE REAL ONE.
	// This is the discipline Download states and the first version of this
	// function broke: renaming before the digest check publishes unverified bytes
	// under a trusted name, and anything reading the directory afterwards — or
	// after a crash between the two steps — cannot tell that they were never
	// checked. A test that asserted the failed path leaves nothing behind is what
	// caught it.
	staged, err := joinParts(dir, img)
	if err != nil {
		return "", err
	}

	if err := verifyAssembled(staged, img); err != nil {
		_ = os.Remove(staged)

		return "", err
	}

	if err := os.Rename(staged, out); err != nil {
		_ = os.Remove(staged)

		return "", fmt.Errorf("imagesource: could not put the assembled root filesystem in "+
			"place: %w", err)
	}

	// THE RENAME IS NOT THE COMMIT; THE DIRECTORY SYNC IS. Until the entry is
	// flushed a crash can lose it, so a failure here is still a failed assembly --
	// and it must leave nothing under the assembled name, because a later step
	// would read that as the verified image.
	if err := syncDir(dir); err != nil {
		_ = os.Remove(out)

		return "", err
	}

	// COMMITTED FROM HERE. Everything below is garbage collection, and a failure
	// in it must NOT be reported as a failed assembly: the caller would discard a
	// root filesystem that is present, complete and verified, and a retry would
	// re-download every part to rebuild the file already sitting there.
	//
	// THE PARTS GO ONLY AFTER THE WHOLE IS PROVEN. They are the only way to
	// rebuild the file, so removing them before the digest check would turn a
	// failed verification into a re-download of every part.
	for _, part := range img.Parts {
		if err := os.Remove(filepath.Join(dir, part.Name)); err != nil && !os.IsNotExist(err) {
			// SAID OUT LOUD RATHER THAN RETURNED. On a parity-sized image the
			// leftovers are tens of gigabytes, so silence would be a disk filling
			// with nothing to explain it.
			fmt.Fprintf(os.Stderr, "imagesource: the assembled root filesystem is in place and "+
				"the staged part %s could not be removed: %v\n", part.Name, err)
		}
	}

	return out, nil
}

// joinParts concatenates the parts in published order into a staged file and
// returns the path it wrote, flushed but deliberately still under a name nothing
// treats as an image. The caller verifies it before giving it the real name.
func joinParts(dir string, img Multipart) (string, error) {
	tmp, err := os.CreateTemp(dir, ".billet-rootfs-*")
	if err != nil {
		return "", fmt.Errorf("imagesource: could not stage the assembled root filesystem in "+
			"%s: %w", dir, err)
	}

	staged := tmp.Name()
	written := false

	defer func() {
		_ = tmp.Close()

		if !written {
			// A PARTIAL ASSEMBLY IS TENS OF GIGABYTES on a parity-sized image, and
			// a node that retries weekly fills its disk with them otherwise.
			_ = os.Remove(staged)
		}
	}()

	var total int64

	for _, part := range img.Parts {
		n, err := appendPart(tmp, filepath.Join(dir, part.Name))
		if err != nil {
			return "", err
		}

		// EACH PART'S LENGTH IS CHECKED AS IT IS JOINED, so a truncated staged file
		// is named here rather than surfacing as a digest mismatch over the whole
		// image, which says only that something is wrong somewhere.
		if n != part.Size {
			return "", fmt.Errorf("imagesource: the staged part %s is %d bytes and the manifest "+
				"publishes it as %d", part.Name, n, part.Size)
		}

		total += n
	}

	if total != img.Size {
		return "", fmt.Errorf("imagesource: the assembled root filesystem is %d bytes and the "+
			"manifest publishes it as %d", total, img.Size)
	}

	// FLUSHED BEFORE THE CALLER RENAMES IT. The rename is what makes the name mean
	// "this is complete", and a rename that lands before the data does leaves
	// exactly the file the staging discipline exists to prevent.
	if err := tmp.Sync(); err != nil {
		return "", fmt.Errorf("imagesource: could not flush the assembled root filesystem: %w",
			err)
	}

	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("imagesource: could not close the assembled root filesystem: %w",
			err)
	}

	written = true

	return staged, nil
}

func appendPart(dst *os.File, path string) (int64, error) {
	src, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("imagesource: could not read the staged part %s: %w", path, err)
	}

	defer func() { _ = src.Close() }()

	n, err := io.Copy(dst, src)
	if err != nil {
		return 0, fmt.Errorf("imagesource: could not append %s to the root filesystem: %w",
			path, err)
	}

	return n, nil
}

// verifyAssembled is the check every path through AssembleRootfs ends at.
func verifyAssembled(path string, img Multipart) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("imagesource: could not read the assembled root filesystem: %w", err)
	}

	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("imagesource: could not size the assembled root filesystem: %w", err)
	}

	if info.Size() != img.Size {
		return fmt.Errorf("imagesource: the assembled root filesystem is %d bytes and the "+
			"manifest publishes it as %d", info.Size(), img.Size)
	}

	sum := sha256.New()
	if _, err := io.Copy(sum, f); err != nil {
		return fmt.Errorf("imagesource: could not hash the assembled root filesystem: %w", err)
	}

	if got := hex.EncodeToString(sum.Sum(nil)); got != img.SHA256 {
		return fmt.Errorf("imagesource: the assembled root filesystem hashes to %s and the "+
			"manifest publishes %s. Every part matched its own digest, so the parts are the "+
			"published ones and they did not join into the published file — the order or the "+
			"set is wrong, and the bytes must not be imported", got, img.SHA256)
	}

	return nil
}

// syncDir flushes a directory entry so a rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("imagesource: could not open %s to flush it: %w", dir, err)
	}

	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("imagesource: could not flush %s: %w", dir, err)
	}

	return nil
}
