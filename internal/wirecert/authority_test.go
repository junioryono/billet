package wirecert

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// authorityDir builds a state directory with a real authority in it.
func authorityDir(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	deployment := "0123456789abcdef0123456789abcdef"

	if _, err := LoadOrCreateCA(dir, deployment); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	return dir, deployment
}

// TestTheAuthorityLockExcludesASecondHolder proves that the authority lock excludes a second holder.
//
// MEASURED, NOT ASSUMED: a second flock on a separate descriptor in the SAME
// process is denied with EWOULDBLOCK, so this exclusion is real within one
// process as well as between two. That is what lets a backup take the lock and
// know a rotation cannot be running underneath it.
func TestTheAuthorityLockExcludesASecondHolder(t *testing.T) {
	dir, _ := authorityDir(t)

	first, err := LockAuthority(dir)
	if err != nil {
		t.Fatalf("LockAuthority: %v", err)
	}

	if _, err := LockAuthority(dir); err == nil {
		t.Fatal("a second holder took the authority lock")
	} else if !strings.Contains(err.Error(), "ca rotate") {
		t.Errorf("the refusal does not name the commands that share the lock: %v", err)
	}

	if err := first.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// AND IT LETS GO. A lock that never released would satisfy the assertion
	// above and wedge every rotation and backup on the host.
	second, err := LockAuthority(dir)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}

	if err := second.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestRotateAndRetireTakeTheAuthorityLockThemselves proves that rotate and retire take the authority lock themselves.
//
// THEY ARE EXPORTED ENTRY POINTS, and a rule enforced only at the CLI has a
// second way in that does not enforce it — the same argument alloc.New makes
// about re-applying its own safety rules.
func TestRotateAndRetireTakeTheAuthorityLockThemselves(t *testing.T) {
	dir, deployment := authorityDir(t)

	held, err := LockAuthority(dir)
	if err != nil {
		t.Fatalf("LockAuthority: %v", err)
	}

	if _, err := Rotate(dir, deployment); err == nil {
		t.Error("Rotate ran while the authority lock was held")
	}

	if err := Retire(dir, deployment); err == nil {
		t.Error("Retire ran while the authority lock was held")
	}

	if err := held.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// BOTH DIRECTIONS: with the lock free they work, or the assertions above
	// would pass against a Rotate that had simply been broken.
	if _, err := Rotate(dir, deployment); err != nil {
		t.Fatalf("Rotate with the lock free: %v", err)
	}

	if err := Retire(dir, deployment); err != nil {
		t.Fatalf("Retire with the lock free: %v", err)
	}
}

// TestReadAuthorityCollectsTheWholeUnit, including the marker that lives outside
// the ca directory and the previous generation during a rotation.
func TestReadAuthorityCollectsTheWholeUnit(t *testing.T) {
	dir, deployment := authorityDir(t)

	a, err := ReadAuthority(dir)
	if err != nil {
		t.Fatalf("ReadAuthority: %v", err)
	}

	for _, name := range []string{"ca.key", "ca.crt", markerFile} {
		if len(a.Present[name]) == 0 {
			t.Errorf("ReadAuthority did not collect %s", name)
		}
	}

	if a.Rotating() {
		t.Error("a deployment with no rotation reports one")
	}

	if _, err := Rotate(dir, deployment); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	a, err = ReadAuthority(dir)
	if err != nil {
		t.Fatalf("ReadAuthority during a rotation: %v", err)
	}

	if !a.Rotating() {
		t.Error("a deployment mid-rotation does not report one")
	}

	for _, name := range []string{"ca-previous.key", "ca-previous.crt"} {
		if len(a.Present[name]) == 0 {
			t.Errorf("ReadAuthority did not collect %s during a rotation", name)
		}
	}
}

// TestAnIncompleteAuthorityIsRefused, and a healthy one is not.
//
// The second half is what stops a ReadAuthority that refused EVERYTHING from
// passing the first.
func TestAnIncompleteAuthorityIsRefused(t *testing.T) {
	t.Run("a healthy authority is not refused", func(t *testing.T) {
		dir, _ := authorityDir(t)

		if _, err := ReadAuthority(dir); err != nil {
			t.Errorf("a complete authority was refused: %v", err)
		}
	})

	for _, tc := range []struct {
		name   string
		remove string
		is     error
		says   string
	}{
		{name: "no key", remove: filepath.Join("ca", "ca.key"), is: ErrHalfInitialised},
		{name: "no certificate", remove: filepath.Join("ca", "ca.crt"), is: ErrHalfInitialised},
		{name: "no marker", remove: markerFile, says: markerFile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _ := authorityDir(t)

			if err := os.Remove(filepath.Join(dir, tc.remove)); err != nil {
				t.Fatalf("remove %s: %v", tc.remove, err)
			}

			_, err := ReadAuthority(dir)
			if err == nil {
				t.Fatalf("an authority with no %s was accepted", tc.remove)
			}

			if tc.is != nil && !errors.Is(err, tc.is) {
				t.Errorf("the refusal is not %v: %v", tc.is, err)
			}

			if tc.says != "" && !strings.Contains(err.Error(), tc.says) {
				t.Errorf("the refusal does not name %s: %v", tc.says, err)
			}
		})
	}
}

// TestHalfOfAPreviousAuthorityIsRefused proves that half of a previous authority is refused.
//
// THE PREVIOUS KEY IS OPERATIONALLY REQUIRED, not a nicety: it signs the
// certificate the control plane PRESENTS while the fleet renews, so an archive
// carrying only the previous certificate restores a deployment no un-renewed
// node can verify.
func TestHalfOfAPreviousAuthorityIsRefused(t *testing.T) {
	dir, deployment := authorityDir(t)

	if _, err := Rotate(dir, deployment); err != nil {
		t.Fatalf("Rotate: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "ca", "ca-previous.key")); err != nil {
		t.Fatalf("remove the previous key: %v", err)
	}

	_, err := ReadAuthority(dir)
	if err == nil {
		t.Fatal("half a previous authority was accepted")
	}

	if !errors.Is(err, ErrHalfInitialised) {
		t.Errorf("the refusal is not ErrHalfInitialised: %v", err)
	}

	if !strings.Contains(err.Error(), "un-renewed") {
		t.Errorf("the refusal does not say what breaks: %v", err)
	}
}

// TestAMismatchedPairIsRefused. Two unrelated halves load happily and then sign
// leaves nothing can verify.
func TestAMismatchedPairIsRefused(t *testing.T) {
	dir, _ := authorityDir(t)

	other := t.TempDir()
	if _, err := LoadOrCreateCA(other, "fedcba9876543210fedcba9876543210"); err != nil {
		t.Fatalf("LoadOrCreateCA: %v", err)
	}

	strangerKey, err := os.ReadFile(filepath.Join(other, "ca", "ca.key"))
	if err != nil {
		t.Fatalf("read the other key: %v", err)
	}

	target := filepath.Join(dir, "ca", "ca.key")

	if err := os.Remove(target); err != nil {
		t.Fatalf("clear the key: %v", err)
	}

	if err := os.WriteFile(target, strangerKey, 0o600); err != nil {
		t.Fatalf("plant a stranger's key: %v", err)
	}

	if _, err := ReadAuthority(dir); err == nil ||
		!strings.Contains(err.Error(), "not the key for") {
		t.Errorf("a mismatched pair was accepted: %v", err)
	}
}

// TestUnexpectedFilesAreNamedRatherThanCaptured proves that unexpected files are named rather than captured.
func TestUnexpectedFilesAreNamedRatherThanCaptured(t *testing.T) {
	dir, _ := authorityDir(t)

	leftover := filepath.Join(dir, "ca", "ca.crt.new")
	if err := os.WriteFile(leftover, []byte("half a rotation\n"), 0o600); err != nil {
		t.Fatalf("stage the leftover: %v", err)
	}

	a, err := ReadAuthority(dir)
	if err != nil {
		t.Fatalf("ReadAuthority: %v", err)
	}

	if len(a.Unexpected) != 1 || a.Unexpected[0] != leftover {
		t.Errorf("the leftover was not reported: %v", a.Unexpected)
	}

	if len(RotationLeftovers(a.Unexpected)) != 1 {
		t.Errorf("the leftover was not recognised as an interrupted rotation: %v", a.Unexpected)
	}

	for name := range a.Present {
		if strings.HasSuffix(name, ".new") {
			t.Errorf("a rotation leftover was collected as authority state: %s", name)
		}
	}

	// THE LOCK FILE IS NOT UNEXPECTED. It is billet's own, and naming it every
	// time would train an operator to ignore this list.
	if _, err := LockAuthority(dir); err != nil {
		t.Fatalf("LockAuthority: %v", err)
	}
}
