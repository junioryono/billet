package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/junioryono/billet/internal/config"
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

func TestFailedLegacyExactPinCompatibilityBootRequiresAPinChange(t *testing.T) {
	err := compatibilityBootFailure("ubuntu-2404-x64@g20260815033431", false,
		os.ErrDeadlineExceeded)

	if got := exitStatus(err); got == 2 {
		t.Fatalf("exact-pin boot failure exit status = %d, which asks the role to pull an unused replacement", got)
	}
	if !strings.Contains(err.Error(), "update the exact generation pin") {
		t.Errorf("exact-pin boot failure did not explain the required config change: %v", err)
	}
}

func TestFailedFloatingCompatibilityBootRequestsAReplacement(t *testing.T) {
	err := compatibilityBootFailure("ubuntu-2404-x64@g20260815033431", true,
		os.ErrDeadlineExceeded)

	if got := exitStatus(err); got != 2 {
		t.Fatalf("floating boot failure exit status = %d, want replacement status 2", got)
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

func TestVerifiedImageResultFailureDoesNotWithdrawFleetPublication(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(parent, []byte("file\n"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}

	err := writeImageResult(filepath.Join(parent, "prepared-image"),
		"ubuntu-2404-x64@g20260815033431")
	if err == nil {
		t.Fatal("writing a result below a regular file succeeded")
	}
}

func TestEveryDistinctConfiguredFirecrackerImageIsChecked(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{Tiers: []config.Tier{
		{Provider: config.ProviderFirecracker, Image: "ubuntu-2404-x64@verified"},
		{Providers: []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2},
			Launch: map[config.ProviderKind]config.TierLaunch{
				config.ProviderFirecracker: {Image: "ubuntu-2404-arm64@verified"},
				config.ProviderEC2:         {Image: "ami-123"},
			}},
		{Provider: config.ProviderFirecracker, Image: "ubuntu-2404-x64@verified"},
		{Provider: config.ProviderDocker, Image: "golang:1.25"},
	}}

	want := []string{"ubuntu-2404-x64@verified", "ubuntu-2404-arm64@verified"}
	got := firecrackerTierImages(cfg)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("configured firecracker images = %v, want %v", got, want)
	}
}

func TestImageResultCanRecordEveryConfiguredRefresh(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "refreshes")
	want := []string{"ubuntu-2404-x64", "ubuntu-2404-arm64"}
	if err := writeImageResults(path, want); err != nil {
		t.Fatalf("write refresh result: %v", err)
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read refresh result: %v", err)
	}
	if string(body) != strings.Join(want, "\n")+"\n" {
		t.Fatalf("refresh result = %q, want one image per line", body)
	}
}
