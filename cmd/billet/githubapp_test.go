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

// The reservation proves the directory is writable while failing is still free,
// and it does NOT occupy the destination.
//
// That separation is the design. While the reservation sat at the key path,
// installing meant unlinking the key path first, and a pathname unlink cannot be
// made safe by any check that precedes it.
func TestReserveKeyFileStagesBesideTheDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	f, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	defer f.Close()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the destination was created; nothing may occupy it before the install links it")
	}

	info, err := os.Stat(staging)
	if err != nil {
		t.Fatalf("the reservation was not created at %s: %v", staging, err)
	}

	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("reservation mode = %04o, want 0600", perm)
		}
	}
}

// An existing key at the destination is refused and left untouched. GitHub
// cannot re-issue one that is lost, so it is never overwritten.
func TestReserveKeyFileRefusesAnExistingKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "app.pem")

	existing := testKey(t)
	if err := os.WriteFile(path, existing, 0o600); err != nil {
		t.Fatalf("seed key: %v", err)
	}

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile accepted an occupied destination")
	}

	if !strings.Contains(err.Error(), "cannot re-issue") {
		t.Errorf("error should explain why this is refused, got: %v", err)
	}

	// Returning the right diagnostic is not the property under test — leaving the
	// key alone is. An implementation that truncated or unlinked it and then
	// returned this exact error passed the assertion above.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the existing key is gone: %v", err)
	}

	if !bytes.Equal(got, existing) {
		t.Error("reserveKeyFile modified the key it claims to refuse")
	}
}

// A leftover reservation is never adopted: it is equally a crashed run's scrap
// and a CONCURRENT run's live file, and adopting it puts two processes on one
// descriptor where either can destroy the other's key.
func TestReserveKeyFileRefusesALeftoverReservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	if err := os.WriteFile(staging, nil, 0o600); err != nil {
		t.Fatalf("seed leftover: %v", err)
	}

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile adopted a leftover reservation")
	}

	if !strings.Contains(err.Error(), "rm "+staging) {
		t.Errorf("error should say exactly what to remove, got: %v", err)
	}

	if _, statErr := os.Stat(staging); statErr != nil {
		t.Errorf("reserveKeyFile removed the file it refused: %v", statErr)
	}
}

// A staged file holding a real key stops everything, because the alternative is
// creating a second App beside an orphaned credential.
func TestReserveKeyFileReportsAStagedKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	if err := os.WriteFile(staging, testKey(t), 0o600); err != nil {
		t.Fatalf("seed staged key: %v", err)
	}

	f, err := reserveKeyFile(path)
	if err == nil {
		f.Close()
		t.Fatal("reserveKeyFile ignored an orphaned key")
	}

	if !strings.Contains(err.Error(), staging) {
		t.Errorf("the error does not name the orphaned key: %v", err)
	}

	if !strings.Contains(err.Error(), "do not create another App") {
		t.Errorf("the error does not warn against creating a second App: %v", err)
	}

	// With the destination free, `mv` is the correct instruction.
	if !strings.Contains(err.Error(), "mv "+staging) {
		t.Errorf("the error does not tell the operator how to recover it: %v", err)
	}
}

// With keys at BOTH paths, `mv` is destructive — Unix mv replaces — so it must
// not be recommended.
func TestReserveKeyFileWillNotRecommendClobberingASecondKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	if err := os.WriteFile(staging, testKey(t), 0o600); err != nil {
		t.Fatalf("seed staged key: %v", err)
	}

	if err := os.WriteFile(path, testKey(t), 0o600); err != nil {
		t.Fatalf("seed destination key: %v", err)
	}

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile proceeded with two keys present")
	}

	if strings.Contains(err.Error(), "mv ") {
		t.Errorf("billet recommended a command that would destroy one of two keys: %v", err)
	}

	for _, want := range []string{staging, path} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not name %s: %v", want, err)
		}
	}
}

// A FRAGMENT is not a key. Reporting one as recoverable tells the operator to
// install a truncated PEM and keep an App whose real key is gone.
func TestReserveKeyFileDoesNotReportAFragmentAsAKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	if err := os.WriteFile(staging, testKey(t)[:60], 0o600); err != nil {
		t.Fatalf("seed fragment: %v", err)
	}

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile adopted a fragment")
	}

	if strings.Contains(err.Error(), "mv ") {
		t.Errorf("a fragment was reported as a recoverable key: %v", err)
	}
}

func TestWriteKeyAtomicallyInstallsTheKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")

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

// os.Link leaves TWO names for one private key, so the staging one must be gone
// when the install reports success — a second copy of an App key that nothing
// mentions is exactly what nobody finds until it matters.
func TestWriteKeyAtomicallyLeavesNoSecondCopy(t *testing.T) {
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
			t.Errorf("a second file survived the install: %s", e.Name())
		}
	}

	if len(entries) != 1 {
		t.Errorf("directory holds %d entries, want just the key", len(entries))
	}
}

// The install creates the destination or fails; it never replaces. A key that
// appears at the destination during the browser flow must survive, and this
// run's key must survive too.
func TestWriteKeyAtomicallyRefusesToReplaceTheDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	defer reserved.Close()

	// Another run claims the destination after this one reserved.
	theirs := testKey(t)
	if err := os.WriteFile(path, theirs, 0o600); err != nil {
		t.Fatalf("seed the other run's key: %v", err)
	}

	ours := testKey(t)
	installed := false

	writeErr := writeKeyAtomically(reserved, path, ours, func() { installed = true })
	if writeErr == nil {
		t.Fatal("writeKeyAtomically installed over a destination it did not create")
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

	// Refusing is only safe if THIS run's key survives and the error says where.
	if !errors.Is(writeErr, errCredentialPreserved) {
		t.Errorf("the error does not mark the credential as preserved: %v", writeErr)
	}

	preserved, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("this run's key was not preserved: %v", err)
	}

	if !bytes.Equal(preserved, ours) {
		t.Error("the preserved file does not hold this run's key")
	}

	if !strings.Contains(writeErr.Error(), staging) {
		t.Errorf("the error does not name where the key was preserved: %v", writeErr)
	}
}

// destinationIsStillReserved is what the staging cleanup relies on to decide
// that a pathname still refers to this run's file.
//
// Tested directly because the interleaving it guards — the file being replaced
// BETWEEN the check and the act — cannot be produced deterministically from
// outside the package. What can be pinned is that the predicate answers
// correctly, which is what its caller's correctness rests on.
func TestDestinationIsStillReservedDetectsAReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	defer reserved.Close()

	if err := destinationIsStillReserved(reserved, staging); err != nil {
		t.Fatalf("an untouched reservation was reported as replaced: %v", err)
	}

	// Same pathname, different inode — the case a pathname cannot distinguish.
	if err := os.Remove(staging); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := os.WriteFile(staging, testKey(t), 0o600); err != nil {
		t.Fatalf("replace: %v", err)
	}

	if err := destinationIsStillReserved(reserved, staging); err == nil {
		t.Error("a replaced file was accepted as this run's reservation")
	}

	// And an absent one is not silently treated as ours.
	if err := os.Remove(staging); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := destinationIsStillReserved(reserved, staging); err == nil {
		t.Error("a missing file was accepted as this run's reservation")
	}
}

// "Could not tell" is not "no key here", and it is not "the key is saved"
// either. Both collapses are destructive in opposite directions.
func TestInspectKeyDistinguishesAbsentFromUnverifiable(t *testing.T) {
	dir := t.TempDir()

	absent := filepath.Join(dir, "nothing.pem")
	if got := inspectKey(absent); got != keyAbsent {
		t.Errorf("a missing file: inspectKey = %v, want keyAbsent", got)
	}

	valid := filepath.Join(dir, "good.pem")
	if err := os.WriteFile(valid, testKey(t), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := inspectKey(valid); got != keyPresent {
		t.Errorf("a valid key: inspectKey = %v, want keyPresent", got)
	}

	fragment := filepath.Join(dir, "partial.pem")
	if err := os.WriteFile(fragment, testKey(t)[:60], 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := inspectKey(fragment); got != keyAbsent {
		t.Errorf("a truncated PEM: inspectKey = %v, want keyAbsent", got)
	}

	// A file that cannot be opened is UNVERIFIABLE, never absent — the whole
	// point of the third state.
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		unreadable := filepath.Join(dir, "locked.pem")
		if err := os.WriteFile(unreadable, testKey(t), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		if err := os.Chmod(unreadable, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		t.Cleanup(func() {
			if err := os.Chmod(unreadable, 0o600); err != nil {
				t.Errorf("restore mode: %v", err)
			}
		})

		if got := inspectKey(unreadable); got != keyUnverifiable {
			t.Errorf("an unreadable file: inspectKey = %v, want keyUnverifiable", got)
		}
	}
}

// The third state only matters if the CALLER acts on it.
//
// Asserting a helper left the destructive path unproven: reserveKeyFile is what
// prints `rm`, and it was reaching that branch for a file it had merely failed
// to read. Unlink permission comes from the DIRECTORY, not the file, so an
// operator can follow that advice and destroy a key billet simply could not open.
func TestReserveKeyFileWillNotOfferToDeleteAFileItCannotRead(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("unix permission bits do not gate reads here")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	if err := os.WriteFile(staging, testKey(t), 0o600); err != nil {
		t.Fatalf("seed staged key: %v", err)
	}

	if err := os.Chmod(staging, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Chmod(staging, 0o600); err != nil {
			t.Errorf("restore mode: %v", err)
		}
	})

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile proceeded past a file it could not read")
	}

	if strings.Contains(err.Error(), "rm ") {
		t.Errorf("billet offered to delete a file it could not read: %v", err)
	}

	if !strings.Contains(err.Error(), "cannot read it") {
		t.Errorf("the error does not say why billet stopped: %v", err)
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
