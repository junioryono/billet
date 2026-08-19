package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/provider/firecracker"
)

func TestIncompatibleGuestHasTheDocumentedRefreshExitStatus(t *testing.T) {
	err := incompatibleGuest("ubuntu-2404-x64@g20260815033431", "contract mismatch")
	if got := exitStatus(err); got != 2 {
		t.Fatalf("exit status = %d, want 2", got)
	}
	if !strings.Contains(err.Error(), "contract mismatch") {
		t.Errorf("the incompatibility reason was lost: %v", err)
	}
}

func TestImportedButUnverifiedGuestStillNeedsACompatibilityBoot(t *testing.T) {
	needsBoot, err := guestNeedsCompatibilityBoot(
		"ubuntu-2404-x64@g20260815033431",
		firecracker.GuestContract,
		true,
		false,
		false,
	)
	if err != nil {
		t.Fatalf("compatibility decision: %v", err)
	}
	if !needsBoot {
		t.Fatal("a manifest contract was accepted without a successful guest boot")
	}
}

func TestVerifiedGuestWithMatchingRecordedContractNeedsNoBoot(t *testing.T) {
	needsBoot, err := guestNeedsCompatibilityBoot(
		"ubuntu-2404-x64@g20260815033431",
		firecracker.GuestContract,
		true,
		true,
		false,
	)
	if err != nil {
		t.Fatalf("compatibility decision: %v", err)
	}
	if needsBoot {
		t.Fatal("a verified guest with the matching contract was scheduled for another boot")
	}
}

func TestExactPinMismatchCannotBeRepairedByPromotingAnotherGeneration(t *testing.T) {
	_, err := guestNeedsCompatibilityBoot(
		"ubuntu-2404-x64@g20260815033431",
		"older-contract",
		true,
		true,
		false,
	)
	if err == nil {
		t.Fatal("an exact incompatible pin was treated as refreshable through promotion")
	}
	if got := exitStatus(err); got == 2 {
		t.Fatalf("exact-pin mismatch exit status = %d, which asks the role to pull without changing the pin", got)
	}
	if !strings.Contains(err.Error(), "update the exact generation pin") {
		t.Errorf("exact-pin mismatch did not explain the required operator action: %v", err)
	}
}

func TestImagePullResultIsInstalledOnlyAsAProtectedCompleteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prepared-image")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatalf("stage stale result: %v", err)
	}

	want := "ubuntu-2404-x64@g20260815033431"
	if err := writeImageResult(path, want); err != nil {
		t.Fatalf("write result: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	if string(body) != want+"\n" {
		t.Errorf("result = %q", body)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat result: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("result mode = %o, want 600", got)
	}
}

func TestVerifiedImageIsWithdrawnWhenItsRollbackHandleCannotBeWritten(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("file\n"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}

	withdrawn := false
	err := writeVerifiedImageResult(filepath.Join(parent, "prepared-image"),
		"ubuntu-2404-x64@g20260815033431", func() error {
			withdrawn = true

			return nil
		})
	if err == nil {
		t.Fatal("writing a result below a regular file succeeded")
	}
	if !withdrawn {
		t.Fatal("the verified generation remained promoted without a rollback handle")
	}
}
