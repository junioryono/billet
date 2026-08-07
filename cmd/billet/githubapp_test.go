package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// testKey returns a valid PEM-encoded App private key.
func testKey(t *testing.T) []byte {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	return pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
}

// The reservation exists so a registered App never outlives the ability to
// store its key: creating the real file proves the directory is writable at the
// point where failing is still free.
func TestReserveKeyFileCreatesRestrictedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "app.pem")

	f, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	defer f.Close()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("reservation mode = %04o, want 0600", perm)
		}
	}
}

// A non-empty file may be a real key, and GitHub cannot re-issue one that is
// lost, so it is never overwritten.
func TestReserveKeyFileRefusesAnExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pem")

	existing := testKey(t)
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile overwrote an existing key")
	}

	if !strings.Contains(err.Error(), "cannot re-issue") {
		t.Errorf("error should explain why this is refused, got: %v", err)
	}

	// Returning the right diagnostic is not the property under test — leaving
	// the key alone is. An implementation that truncated or unlinked the file
	// and then returned this exact error passed the assertion above.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the existing key is gone: %v", err)
	}

	if !bytes.Equal(got, existing) {
		t.Error("reserveKeyFile modified the existing key it claims to refuse")
	}
}

// An EMPTY file is what an interrupted run leaves — but it is also what a
// CONCURRENT run's reservation looks like, so adopting it would put two
// processes on one destination where either can delete the other's key. It is
// refused, with the one instruction that is safe to give.
func TestReserveKeyFileRefusesAnEmptyReservationWithGuidance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed empty reservation: %v", err)
	}

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile adopted an empty file")
	}

	if !strings.Contains(err.Error(), "rm "+path) {
		t.Errorf("error should tell the operator exactly what to remove, got: %v", err)
	}

	// Refusing means leaving it alone. Removing it here would be indistinguishable
	// from adopting a CONCURRENT run's reservation, which is the case this refusal
	// exists to prevent.
	if _, statErr := os.Stat(path); statErr != nil {
		t.Errorf("reserveKeyFile removed the file it refused: %v", statErr)
	}
}

func TestWriteKeyAtomicallyInstallsTheKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pem")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	key := testKey(t)
	installed := false

	if err := writeKeyAtomically(reserved, path, key, func() { installed = true }); err != nil {
		t.Fatalf("writeKeyAtomically: %v", err)
	}

	if !installed {
		t.Error("onInstalled was never called")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed key: %v", err)
	}

	if !bytes.Equal(got, key) {
		t.Error("the installed key does not match what was written")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat: %v", err)
		}

		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("installed key mode = %04o, want 0600", perm)
		}
	}
}

// No temporary file may survive a successful install: one would be a second
// copy of a private key sitting in the same directory.
func TestWriteKeyAtomicallyLeavesNoTemporaryFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	if err := writeKeyAtomically(reserved, path, testKey(t), func() {}); err != nil {
		t.Fatalf("writeKeyAtomically: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}

	for _, e := range entries {
		if e.Name() != "app.pem" {
			t.Errorf("a file other than the key survived the install: %s", e.Name())
		}
	}

	// The count is what actually carries this test. A successful rename consumes
	// the staging pathname on its own, so a name-shaped assertion passes even
	// with the cleanup deleted outright; only "nothing else is here" catches a
	// second copy of a private key left in the directory.
	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want just the key", len(entries))
	}
}

// The reservation exists so that a key GitHub has already issued can always be
// stored — but nothing ever wrote to it. The staged write creates a SECOND file
// after issuance, and every step of that can fail: a directory that became
// read-only during the browser flow, an exhausted inode table, a full disk.
// GitHub had issued the one-time key by then, and billet exited holding it in
// memory and wrote nothing anywhere.
func TestWriteKeyAtomicallyFallsBackToTheReservation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory permission bits do not gate creation on Windows")
	}

	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write permissions")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	// Read and traverse, but no create — exactly what a directory that turned
	// read-only mid-flow looks like. The reservation descriptor was opened
	// before this and stays writable, because unix checks permissions at open.
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}

	// Restored so t.TempDir's own cleanup can remove the directory.
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Errorf("restore dir mode: %v", err)
		}
	})

	key := testKey(t)
	installed := false

	if err := writeKeyAtomically(reserved, path, key, func() { installed = true }); err != nil {
		t.Fatalf("writeKeyAtomically discarded an issued credential: %v", err)
	}

	if !installed {
		t.Error("onInstalled was never called, so the caller believes nothing was written")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read installed key: %v", err)
	}

	if !bytes.Equal(got, key) {
		t.Error("the key at the destination does not match what GitHub issued")
	}
}

// Cleanup and install both act on a PATHNAME, while the thing this run owns is
// a descriptor. If the empty reservation is removed and a second run installs
// its own key at the same path, this run's rename would overwrite a real
// credential with its own — and its cleanup would delete one.
func TestWriteKeyAtomicallyRefusesToOverwriteAnotherRunsKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	// Someone else's key now occupies the pathname this run reserved.
	theirs := testKey(t)
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove reservation: %v", err)
	}

	if err := os.WriteFile(path, theirs, 0o600); err != nil {
		t.Fatalf("seed the other run's key: %v", err)
	}

	installed := false

	// Named apart from every other err in this test: reassigning it with a later
	// os.ReadFile silently moved the assertions below onto the wrong error.
	writeErr := writeKeyAtomically(reserved, path, testKey(t), func() { installed = true })
	if writeErr == nil {
		t.Fatal("writeKeyAtomically overwrote a key it did not reserve")
	}

	if installed {
		t.Error("onInstalled reported success while refusing to install")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !bytes.Equal(got, theirs) {
		t.Fatal("the other run's key was destroyed")
	}

	// Refusing is only safe if THIS run's credential survives somewhere and the
	// error says where — otherwise the safe choice has thrown away a key GitHub
	// cannot re-issue.
	if !errors.Is(writeErr, errCredentialPreserved) {
		t.Errorf("the error does not mark the credential as preserved: %v", writeErr)
	}

	recovery := filepath.Join(dir, ".app.pem.billet-partial")
	if _, statErr := os.Stat(recovery); statErr != nil {
		t.Errorf("this run's credential was not preserved at %s: %v", recovery, statErr)
	}

	if !strings.Contains(writeErr.Error(), recovery) {
		t.Errorf("the error does not name where the key was preserved, got: %v", writeErr)
	}
}

// A crash between the synced staging file and the rename leaves the only copy
// of the key under a name nothing reports. The next run has to find it and say
// so, because the guidance for an empty reservation is "delete it and re-run" —
// which, followed literally, abandons that key.
func TestReserveKeyFileReportsAPreservedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	recovery := filepath.Join(dir, ".app.pem.billet-partial")

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed empty reservation: %v", err)
	}

	if err := os.WriteFile(recovery, testKey(t), 0o600); err != nil {
		t.Fatalf("seed preserved key: %v", err)
	}

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile ignored a preserved key")
	}

	if !strings.Contains(err.Error(), recovery) {
		t.Errorf("the error does not name the preserved key at %s, got: %v", recovery, err)
	}

	// The empty-reservation guidance must not survive here: `rm` alone would
	// leave the recovered key orphaned and the operator back where they started.
	if strings.Contains(err.Error(), "delete it and re-run") {
		t.Errorf("the error still tells the operator to delete and re-run: %v", err)
	}
}

// maxKeySize is a documented bound. Asserting it against itself would let a
// production edit move the limit and the test with it.
func TestMaxKeySizeIsPinned(t *testing.T) {
	if maxKeySize != 65536 {
		t.Errorf("maxKeySize = %d, want 65536 (64 KiB, as documented)", maxKeySize)
	}
}

// billet check must prove the key is USABLE, not merely present. os.Stat alone
// accepted every one of these.
func TestCheckPrivateKeyRejectsUnusableKeys(t *testing.T) {
	valid := testKey(t)

	for name, setup := range map[string]func(t *testing.T, dir string) (string, string){
		"missing": func(_ *testing.T, dir string) (string, string) {
			return filepath.Join(dir, "absent.pem"), ""
		},
		"directory": func(t *testing.T, dir string) (string, string) {
			t.Helper()

			p := filepath.Join(dir, "adir")
			if err := os.Mkdir(p, 0o700); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			return p, "not a regular file"
		},
		"empty": func(t *testing.T, dir string) (string, string) {
			t.Helper()

			p := filepath.Join(dir, "empty.pem")
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			return p, "is empty"
		},
		"truncated": func(t *testing.T, dir string) (string, string) {
			t.Helper()

			p := filepath.Join(dir, "trunc.pem")
			if err := os.WriteFile(p, valid[:80], 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			return p, "not PEM-encoded"
		},
		"oversized": func(t *testing.T, dir string) (string, string) {
			t.Helper()

			p := filepath.Join(dir, "huge.pem")
			if err := os.WriteFile(p, make([]byte, maxKeySize+1), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}

			return p, "not an App key"
		},
	} {
		t.Run(name, func(t *testing.T) {
			path, want := setup(t, t.TempDir())

			err := checkPrivateKey(path)
			if err == nil {
				t.Fatalf("checkPrivateKey accepted %s", name)
			}

			if want != "" && !strings.Contains(err.Error(), want) {
				t.Errorf("error = %v, want it to mention %q", err, want)
			}
		})
	}
}

// A world-readable App private key is a local credential exposure, and this is
// the command that exists to catch it.
func TestCheckPrivateKeyRejectsPermissiveModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix permission bits are meaningless on Windows")
	}

	for _, mode := range []os.FileMode{0o644, 0o640, 0o604, 0o666} {
		t.Run(mode.String(), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "app.pem")
			if err := os.WriteFile(path, testKey(t), mode); err != nil {
				t.Fatalf("write: %v", err)
			}

			// WriteFile is subject to umask, so set the mode explicitly.
			if err := os.Chmod(path, mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}

			err := checkPrivateKey(path)
			if err == nil {
				t.Fatalf("checkPrivateKey accepted mode %04o", mode.Perm())
			}

			if !strings.Contains(err.Error(), "chmod 600") {
				t.Errorf("error should give the remedy, got: %v", err)
			}
		})
	}
}

func TestCheckPrivateKeyAcceptsAGoodKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pem")
	if err := os.WriteFile(path, testKey(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := checkPrivateKey(path); err != nil {
		t.Errorf("checkPrivateKey rejected a valid 0600 key: %v", err)
	}
}
