package ceph

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A FILESYSTEM e2fsck REFUSES IS REFUSED IN e2fsck'S WORDS, so a publication
// abandoned for it says what was found and not an exit status alone (#689).
func TestAnUncleanFilesystemIsRefusedInE2fscksWords(t *testing.T) {
	bin := t.TempDir()
	for name, script := range map[string]string{
		"blkid":  "#!/bin/sh\necho TYPE=ext4\necho UUID=0f0e0d0c-0b0a-0908-0706-050403020100\n",
		"e2fsck": "#!/bin/sh\necho 'e2fsck 1.47.2 (1-Jan-2025)'\necho 'Journal superblock is corrupt.' >&2\nexit 12\n",
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := verifyFilesystem(t.Context(), "/dev/rbd-test")
	if err == nil {
		t.Fatal("a filesystem e2fsck refused was accepted")
	}

	for _, want := range []string{"is not clean", "exit status 12", "Journal superblock is corrupt."} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}
