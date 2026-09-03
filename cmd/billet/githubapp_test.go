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

	// With the destination free, a recovery command is offered — and it must be
	// one that REFUSES to overwrite. The operator types it some time after billet
	// composed it, and `mv` would replace a key another run installed in between.
	if !strings.Contains(err.Error(), "ln "+staging) {
		t.Errorf("the error does not tell the operator how to recover it: %v", err)
	}

	if strings.Contains(err.Error(), "mv ") {
		t.Errorf("billet suggested a command that silently replaces the destination: %v", err)
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

	for _, forbidden := range []string{"mv ", "ln "} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("billet recommended %q with two keys present: %v", forbidden, err)
		}
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

	for _, forbidden := range []string{"mv ", "ln "} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("a fragment was reported as a recoverable key (%q): %v", forbidden, err)
		}
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

// recoverKey is the difference between "your key is gone, delete the App" and a
// file the operator can move into place. It had no test at all.
func TestRecoverKeyWritesTheKeyAndSaysWhere(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	key := testKey(t)

	err := recoverKey(dir, path, key, errors.New("the install failed"))
	if err == nil {
		t.Fatal("recoverKey returned no error; it always reports the failure it recovered from")
	}

	if !errors.Is(err, errCredentialPreserved) {
		t.Fatalf("a successful recovery was not reported as preserved: %v", err)
	}

	// The named path must actually hold the key — an error that names a file the
	// operator then cannot use is worse than no advice.
	var recovered string

	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatalf("read dir: %v", readErr)
	}

	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".billet-key-recovered-") {
			recovered = filepath.Join(dir, e.Name())
		}
	}

	if recovered == "" {
		t.Fatal("no recovery file was created")
	}

	if !strings.Contains(err.Error(), recovered) {
		t.Errorf("the error does not name the recovery file %s: %v", recovered, err)
	}

	got, readErr := os.ReadFile(recovered)
	if readErr != nil {
		t.Fatalf("read recovery file: %v", readErr)
	}

	if !bytes.Equal(got, key) {
		t.Error("the recovery file does not hold the key it was given")
	}

	if runtime.GOOS != "windows" {
		info, statErr := os.Stat(recovered)
		if statErr != nil {
			t.Fatalf("stat: %v", statErr)
		}

		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("recovery file mode = %04o, want 0600", perm)
		}
	}
}

// A recovery into a directory that cannot hold it is the only genuine loss, and
// only then may billet say the key is gone.
func TestRecoverKeyReportsLossOnlyWhenNothingLanded(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("directory permission bits do not gate creation here")
	}

	dir := t.TempDir()

	sub := filepath.Join(dir, "keys")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.Chmod(sub, 0o500); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	t.Cleanup(func() {
		if err := os.Chmod(sub, 0o700); err != nil {
			t.Errorf("restore mode: %v", err)
		}
	})

	err := recoverKey(sub, filepath.Join(sub, "app.pem"), testKey(t), errors.New("the install failed"))
	if err == nil {
		t.Fatal("recoverKey reported success writing into a read-only directory")
	}

	// It must NOT claim preservation, and it must not claim more than it knows:
	// this directory failed, which is not proof that no directory would work.
	if errors.Is(err, errCredentialPreserved) {
		t.Errorf("a failed recovery was reported as preserved: %v", err)
	}

	if !strings.Contains(err.Error(), "delete the App") {
		t.Errorf("a genuine loss must say what to do about the orphaned App: %v", err)
	}
}

// lookupPath is what decides whether billet suggests `mv`, and `mv` replaces.
// "Could not tell" must never read as "nothing there".
func TestLookupPathDoesNotTreatAnErrorAsAbsence(t *testing.T) {
	dir := t.TempDir()

	if got := lookupPath(filepath.Join(dir, "nothing")); got != pathAbsent {
		t.Errorf("a missing file: lookupPath = %v, want pathAbsent", got)
	}

	present := filepath.Join(dir, "something")
	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := lookupPath(present); got != pathPresent {
		t.Errorf("an existing file: lookupPath = %v, want pathPresent", got)
	}

	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		// A path under an unsearchable directory cannot be stat'd, and that is
		// not the same as it not existing.
		locked := filepath.Join(dir, "locked")
		if err := os.Mkdir(locked, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		hidden := filepath.Join(locked, "app.pem")
		if err := os.WriteFile(hidden, testKey(t), 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}

		if err := os.Chmod(locked, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}

		t.Cleanup(func() {
			if err := os.Chmod(locked, 0o700); err != nil {
				t.Errorf("restore mode: %v", err)
			}
		})

		if got := lookupPath(hidden); got != pathUnknown {
			t.Errorf("an unstattable path: lookupPath = %v, want pathUnknown", got)
		}
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

// THE KEY GOES WHERE THE CONFIG SAYS, or beside the config — never the
// per-user default when a --config was given. Defaulting to the per-user
// directory moved a local-service deployment's key into a home directory the
// packaged unit's ProtectHome=true can never read, then rewrote the config to
// point there.
func TestDefaultKeyPathFollowsTheConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "billet.yaml")

	named := "/etc/billet/app-private-key.pem"
	body := "github:\n  org: acme\n  private_key_path: " + named + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	got, err := defaultKeyPath(cfgPath)
	if err != nil {
		t.Fatalf("defaultKeyPath: %v", err)
	}
	if got != named {
		t.Errorf("defaultKeyPath = %q, want the config's own %q", got, named)
	}

	// A config that names no key path gets the key BESIDE the config — what
	// the flag's help text has always promised.
	if err := os.WriteFile(cfgPath, []byte("github:\n  org: acme\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err = defaultKeyPath(cfgPath)
	if err != nil {
		t.Fatalf("defaultKeyPath: %v", err)
	}
	if want := filepath.Join(dir, "app-private-key.pem"); got != want {
		t.Errorf("defaultKeyPath = %q, want beside the config at %q", got, want)
	}

	// A missing or malformed config is an ERROR before the irreversible App
	// flow, never a silent fallback: the key must not land beside a config
	// nobody can read, with the real failure surfacing after GitHub already
	// holds the App.
	if _, err := defaultKeyPath(filepath.Join(dir, "missing.yaml")); err == nil {
		t.Error("a missing config did not refuse the key-path default")
	}
	broken := filepath.Join(dir, "broken.yaml")
	if err := os.WriteFile(broken, []byte("github: ["), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := defaultKeyPath(broken); err == nil {
		t.Error("a malformed config did not refuse the key-path default")
	}
}

// THE CONFIG REWRITE PRESERVES MODE AND OWNERSHIP. A rename swaps the inode,
// so without preservation a root:billet 0640 config becomes 0600 owned by the
// invoker the moment `github-app create` runs — and the server unit
// (User=billet) can no longer read its own config, with nothing pointing back
// here.
func TestWriteGitHubBlockPreservesTheMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "billet.yaml")

	if err := os.WriteFile(path, []byte("github:\n  org: acme\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	if err := writeGitHubBlock(path, githubBlock{
		Org: "acme", AppID: 7, InstallationID: 9, PrivateKeyPath: "/etc/billet/app-private-key.pem",
	}); err != nil {
		t.Fatalf("writeGitHubBlock: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("the rewrite left mode %o, want the original 0640", got)
	}
}

// EVERY SEED SHAPE A PERSON WRITES, AND THE ONES BILLET MUST REFUSE.
//
// `github-app create --config` writes into an existing file, and the shape an
// operator hand-writes is `github:` with nothing under it — which parses as a
// null scalar, not a mapping. That used to be filled by appending to a scalar's
// Content, which the encoder ignores: no block written, no error returned, and
// the command printing "(updated)" over a file it had not changed. By then the
// App exists and its one-time key is spent, so re-running mints a second App
// rather than recovering.
func TestWriteGitHubBlockFillsOrRefusesEverySeedShape(t *testing.T) {
	want := githubBlock{
		Org: "acme", AppID: 7, InstallationID: 9,
		PrivateKeyPath: "/etc/billet/app-private-key.pem",
	}

	t.Run("shapes that must accept an identity", func(t *testing.T) {
		for name, seed := range map[string]string{
			"a null github key":      "github:\n",
			"an empty mapping":       "github: {}\n",
			"a bare document":        "{}\n",
			"no github key at all":   "server:\n  listen: 127.0.0.1:7717\n",
			"an existing identity":   "github:\n  org: old\n  app_id: 1\n  installation_id: 2\n",
			"a null key with a peer": "server:\n  listen: 127.0.0.1:7717\ngithub:\n",
		} {
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "billet.yaml")
				if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}

				if err := writeGitHubBlock(path, want); err != nil {
					t.Fatalf("writing an identity into %s: %v", name, err)
				}

				// Read it back the way every later command does, rather than
				// trusting the write returned nil — which is the whole defect.
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read back: %v", err)
				}

				got, _, ok := existingGitHubBlock(raw)
				if !ok {
					t.Fatalf("no identity is readable after writing into %s:\n%s", name, raw)
				}
				if got.AppID != want.AppID || got.InstallationID != want.InstallationID ||
					got.Org != want.Org {
					t.Errorf("read back app %d installation %d org %q, want %d/%d/%q\n%s",
						got.AppID, got.InstallationID, got.Org,
						want.AppID, want.InstallationID, want.Org, raw)
				}
			})
		}
	})

	t.Run("shapes that must be refused rather than overwritten", func(t *testing.T) {
		// Filling these would DESTROY what the operator put there, and this
		// command is writing a credential identity — silently discarding a
		// value is worse than making them look at it.
		for name, seed := range map[string]string{
			"github is a list":   "github:\n  - one\n  - two\n",
			"github is a string": "github: somewhere-else.yaml\n",
			"github is a number": "github: 42\n",
		} {
			t.Run(name, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "billet.yaml")
				if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
					t.Fatalf("seed: %v", err)
				}

				err := writeGitHubBlock(path, want)
				if err == nil {
					t.Fatalf("%s was overwritten instead of refused", name)
				}
				if !strings.Contains(err.Error(), "not a mapping") {
					t.Errorf("the refusal does not say why: %v", err)
				}

				// And it left the file alone.
				raw, readErr := os.ReadFile(path)
				if readErr != nil {
					t.Fatalf("read back: %v", readErr)
				}
				if string(raw) != seed {
					t.Errorf("the refused write still changed the file:\n%s", raw)
				}
			})
		}
	})
}
