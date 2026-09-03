package wirecert_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/wirecert"
)

// A REPLACEMENT GETS EXACTLY THE MODE IT ASKED FOR, whatever was there before.
//
// os.WriteFile applies its mode only when it CREATES the file. Re-enrolling a
// machine whose node.key already existed at 0644 therefore wrote a fresh private
// key into a world-readable file and reported success — the one failure a
// permission bug can have where nothing ever complains.
//
// BOTH MODES ARE EXERCISED ON PURPOSE. A staging file is created 0600 by
// CreateTemp, so a secret landing at 0600 proves nothing on its own: it is what
// you get by doing nothing. The public 0644 case is what shows the mode is being
// applied rather than inherited, and the two together pin both directions.
func TestAtomicWriteSetsTheModeOnAnExistingFile(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		existed os.FileMode
		want    os.FileMode
	}{
		{"a private key over a world-readable file", 0o644, 0o600},
		{"a certificate over a private one", 0o600, 0o644},
		{"a private key with nothing there", 0, 0o600},
		{"a certificate with nothing there", 0, 0o644},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			path := filepath.Join(t.TempDir(), "bundle.pem")

			if tc.existed != 0 {
				if err := os.WriteFile(path, []byte("old"), tc.existed); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}

			if err := wirecert.WriteFileAtomic(path, []byte("fresh"), tc.want); err != nil {
				t.Fatalf("write: %v", err)
			}

			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("stat: %v", err)
			}

			if got := info.Mode().Perm(); got != tc.want {
				t.Errorf("the file is mode %04o, want %04o", got, tc.want)
			}

			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}

			if string(body) != "fresh" {
				t.Errorf("the file holds %q", body)
			}
		})
	}
}

// AND IT DOES NOT FOLLOW A NAME SOMEBODY ELSE PLANTED.
//
// The temporary used to be derived from the destination — `.node.key.tmp` —
// which is predictable, and os.WriteFile follows symlinks. Anyone able to write
// the TLS directory during the approval wait, which lasts as long as it takes a
// human to compare a fingerprint, could point that name at a file they can read
// and collect the private key on its way past.
func TestAtomicWriteIgnoresAPlantedTemporary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	path := filepath.Join(dir, "node.key")
	stolen := filepath.Join(t.TempDir(), "stolen")

	// The old, guessable temporary name, pointing somewhere the attacker reads.
	if err := os.Symlink(stolen, filepath.Join(dir, ".node.key.tmp")); err != nil {
		t.Fatalf("plant: %v", err)
	}

	if err := wirecert.WriteFileAtomic(path, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := os.Stat(stolen); !os.IsNotExist(err) {
		t.Fatalf("the private key was written through a planted symlink to %s", stolen)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if string(body) != "secret" {
		t.Errorf("the key did not land at its own path; got %q", body)
	}
}

// AND IT LEAVES NOTHING BEHIND. A temporary holding a private key is worse
// litter than most.
func TestAtomicWriteLeavesNoTemporary(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := wirecert.WriteFileAtomic(filepath.Join(dir, "node.key"), []byte("secret"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read the directory: %v", err)
	}

	for _, e := range entries {
		if e.Name() != "node.key" {
			t.Errorf("left %s behind", e.Name())
		}
	}
}
