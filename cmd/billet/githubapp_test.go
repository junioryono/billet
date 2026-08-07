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

	"github.com/junioryono/billet/internal/github"
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
	ours := testKey(t)

	// Named apart from every other err in this test: reassigning it with a later
	// os.ReadFile silently moved the assertions below onto the wrong error.
	writeErr := writeKeyAtomically(reserved, path, ours, func() { installed = true })
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

	// The CONTENTS, not merely the path. installViaStagingFile creates the
	// staging file before it writes, so asserting existence alone passes even if
	// the key was never written into it — and "your key is preserved at X" is
	// destructive advice when X is empty.
	recovery := filepath.Join(dir, ".app.pem.billet-partial")

	preserved, statErr := os.ReadFile(recovery)
	if statErr != nil {
		t.Errorf("this run's credential was not preserved at %s: %v", recovery, statErr)
	} else if !bytes.Equal(preserved, ours) {
		t.Errorf("the preserved file does not hold this run's key")
	}

	if !strings.Contains(writeErr.Error(), recovery) {
		t.Errorf("the error does not name where the key was preserved, got: %v", writeErr)
	}
}

// A staging file that already holds the complete key must stop the fallback.
//
// Falling back after a staging failure was keyed on "did staging report
// success", not on "does staging hold the key" — and those diverge the moment
// the write lands and a later step fails. The result was the key written a
// SECOND time to the destination, leaving two copies of an unrepeatable private
// key on disk, one of them at a path nothing ever cleans up and the operator is
// never told about.
//
// Driven through installViaStagingFile directly, because provoking a Sync
// failure from writeKeyAtomically is not portable — but this IS the branch
// writeKeyAtomically decides on.
func TestACompleteStagingWriteIsReportedAsPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	defer reserved.Close()

	key := testKey(t)

	// Removing the reservation makes the install step fail AFTER the staging
	// write has completed, which is the shape every "staged but not installed"
	// failure has.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove reservation: %v", err)
	}

	if err := os.WriteFile(path, []byte("another run's key"), 0o600); err != nil {
		t.Fatalf("seed the other run's file: %v", err)
	}

	err = installViaStagingFile(reserved, path, key, func() {})
	if err == nil {
		t.Fatal("installViaStagingFile reported success without installing")
	}

	if !errors.Is(err, errCredentialPreserved) {
		t.Fatalf("a completed staging write was not reported as preserved: %v", err)
	}

	// The complete key must still be there — this is the file the error tells
	// the operator to move into place.
	got, err := os.ReadFile(staging)
	if err != nil {
		t.Fatalf("the staged key was discarded: %v", err)
	}

	if !bytes.Equal(got, key) {
		t.Error("the staged file does not hold the key that was passed in")
	}
}

// A staged key must be found even when the DESTINATION IS ABSENT.
//
// This was the silent one. The install clears the reservation before it links,
// so a process killed in that window leaves the key at the staging path and the
// destination name free. Looking for the staged key only after O_EXCL failed
// meant the next run reserved cleanly, said nothing, and went on to create a
// SECOND App — while the first App's unrepeatable private key sat in the same
// directory, unmentioned.
func TestReserveKeyFileFindsAStagedKeyWithNoReservation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	if err := os.WriteFile(staging, testKey(t), 0o600); err != nil {
		t.Fatalf("seed staged key: %v", err)
	}

	f, err := reserveKeyFile(path)
	if err == nil {
		f.Close()
		t.Fatal("reserveKeyFile reserved over an orphaned key without mentioning it")
	}

	if !strings.Contains(err.Error(), staging) {
		t.Errorf("the error does not name the orphaned key at %s: %v", staging, err)
	}

	// Following the advice must not mean starting again with a new App.
	if !strings.Contains(err.Error(), "do not create another App") {
		t.Errorf("the error does not warn against creating a second App: %v", err)
	}
}

// A FRAGMENT is not a key, and must not be reported as one — the advice for a
// real staged key is "keep the App and install this file", which is destructive
// when the file is a truncated PEM.
func TestReserveKeyFileDoesNotReportAFragmentAsAKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("seed empty reservation: %v", err)
	}

	if err := os.WriteFile(staging, testKey(t)[:60], 0o600); err != nil {
		t.Fatalf("seed fragment: %v", err)
	}

	_, err := reserveKeyFile(path)
	if err == nil {
		t.Fatal("reserveKeyFile adopted an empty reservation")
	}

	// The empty-reservation guidance is correct here: there is no key to save.
	if !strings.Contains(err.Error(), "rm "+path) {
		t.Errorf("a fragment was reported as a recoverable key: %v", err)
	}
}

// A write that fails AFTER producing something parsable has still produced a
// credential, and the file is what decides that — not the return value.
//
// GitHub's PEM ends in a newline. Stopping one byte short of it yields a key
// pem.Decode reads perfectly, so "the write errored" and "there is nothing here"
// are different facts. Treating them as one deleted a working key.
func TestAParsableShortWriteIsKeptAndReported(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	defer reserved.Close()

	// Exactly the shape described: everything but the trailing newline.
	full := testKey(t)

	truncated := full[:len(full)-1]
	if github.ValidatePrivateKey(truncated) != nil {
		t.Skip("this PEM does not parse without its trailing newline; the premise does not hold here")
	}

	if err := os.WriteFile(staging, truncated, 0o600); err != nil {
		t.Fatalf("seed a parsable partial write: %v", err)
	}

	// reserveKeyFile refuses to start while that file is there, which is the
	// behaviour protecting it.
	_, err = reserveKeyFile(path)
	if err == nil {
		t.Fatal("a parsable staged key did not stop a fresh reservation")
	}

	if !strings.Contains(err.Error(), staging) {
		t.Errorf("the parsable partial write was not reported: %v", err)
	}

	if _, statErr := os.Stat(staging); statErr != nil {
		t.Errorf("the parsable key was removed: %v", statErr)
	}
}

// installByLink must not destroy a key that is at the destination, whatever
// the caller believed when it decided to call.
//
// The point of linking instead of renaming is that install can never REPLACE.
// That only holds if the step which clears the destination is bounded too — a
// key sitting at the path is not something to remove on the way to installing
// another one.
func TestInstallByLinkRefusesANonEmptyDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")
	staging := filepath.Join(dir, ".app.pem.billet-partial")

	theirs := testKey(t)
	if err := os.WriteFile(path, theirs, 0o600); err != nil {
		t.Fatalf("seed the other run's key: %v", err)
	}

	ours := testKey(t)
	if err := os.WriteFile(staging, ours, 0o600); err != nil {
		t.Fatalf("seed staging: %v", err)
	}

	if err := installByLink(staging, path); err == nil {
		t.Error("installByLink installed over an occupied destination")
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the destination is gone: %v", err)
	}

	if !bytes.Equal(got, theirs) {
		t.Error("installByLink destroyed a key it did not put there")
	}

	// And ours must survive, or refusing has cost a credential instead of saving
	// one.
	if kept, err := os.ReadFile(staging); err != nil || !bytes.Equal(kept, ours) {
		t.Errorf("this run's staged key did not survive the refusal: %v", err)
	}
}

// destinationIsStillReserved is what both install paths and the cleanup rely on
// to decide whether the pathname still refers to this run's file.
//
// Tested directly because the interleaving it guards — the reservation being
// replaced BETWEEN the check and the act — cannot be produced deterministically
// from the outside. What can be pinned is that the predicate itself answers
// correctly, which is what every caller's correctness rests on.
func TestDestinationIsStillReservedDetectsAReplacedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	defer reserved.Close()

	if err := destinationIsStillReserved(reserved, path); err != nil {
		t.Fatalf("an untouched reservation was reported as replaced: %v", err)
	}

	// Same pathname, different inode — the case a pathname cannot distinguish.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := os.WriteFile(path, testKey(t), 0o600); err != nil {
		t.Fatalf("replace: %v", err)
	}

	if err := destinationIsStillReserved(reserved, path); err == nil {
		t.Error("a replaced destination was accepted as this run's reservation")
	}

	// And an absent one is not silently treated as ours.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove: %v", err)
	}

	if err := destinationIsStillReserved(reserved, path); err == nil {
		t.Error("a missing destination was accepted as this run's reservation")
	}
}

// A key written through a descriptor whose name is gone is NOT preserved.
//
// The distinction matters more than it looks: ErrCredentialPreserved tells the
// operator to keep the App and go find the file. Attaching it here would send
// them looking for a key that no longer has a path — the write went to an inode
// with no directory entry, so it disappears when the process exits.
func TestFallbackWriteToALostReservationIsNotReportedAsPreserved(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.pem")

	reserved, err := reserveKeyFile(path)
	if err != nil {
		t.Fatalf("reserveKeyFile: %v", err)
	}

	defer reserved.Close()

	// The reservation is replaced before the fallback runs, which is the state
	// the post-write re-check exists to notice.
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove reservation: %v", err)
	}

	if err := os.WriteFile(path, []byte("another run's file"), 0o600); err != nil {
		t.Fatalf("replace: %v", err)
	}

	installed := false

	err = installIntoReservation(reserved, path, testKey(t), func() { installed = true })
	if err == nil {
		t.Fatal("installIntoReservation reported success writing to a path it does not own")
	}

	if installed {
		t.Error("onInstalled fired for a key that is not at the destination")
	}

	if errors.Is(err, errCredentialPreserved) {
		t.Errorf("an unreachable key was reported as preserved, which tells the operator to keep the App: %v", err)
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
