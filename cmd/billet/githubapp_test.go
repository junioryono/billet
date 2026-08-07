package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
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
	if err := os.WriteFile(path, testKey(t), 0o600); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile overwrote an existing key")
	}

	if !strings.Contains(err.Error(), "cannot re-issue") {
		t.Errorf("error should explain why this is refused, got: %v", err)
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
		if strings.HasPrefix(e.Name(), ".billet-key-") {
			t.Errorf("a temporary key file survived: %s", e.Name())
		}
	}

	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want just the key", len(entries))
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
