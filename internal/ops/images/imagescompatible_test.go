package images

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/junioryono/billet/internal/cli"
	"github.com/junioryono/billet/internal/config"
	"github.com/junioryono/billet/internal/provider/firecracker"
	"github.com/junioryono/billet/internal/store/ceph"
)

func TestIncompatibleGuestHasTheDocumentedRefreshExitStatus(t *testing.T) {
	err := incompatibleGuest("ubuntu-2404-x64@g20260815033431", "contract mismatch")
	if got := cli.ExitStatus(err); got != 2 {
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
	if got := cli.ExitStatus(err); got == 2 {
		t.Fatalf("exact-pin mismatch exit status = %d, which asks the role to pull without changing the pin", got)
	}
	if !strings.Contains(err.Error(), "update the exact generation pin") {
		t.Errorf("exact-pin mismatch did not explain the required operator action: %v", err)
	}
}

func TestFailedLegacyExactPinCompatibilityBootRequiresAPinChange(t *testing.T) {
	err := compatibilityBootFailure("ubuntu-2404-x64@g20260815033431", false,
		os.ErrDeadlineExceeded)

	if got := cli.ExitStatus(err); got == 2 {
		t.Fatalf("exact-pin boot failure exit status = %d, which asks the role to pull an unused replacement", got)
	}
	if !strings.Contains(err.Error(), "update the exact generation pin") {
		t.Errorf("exact-pin boot failure did not explain the required config change: %v", err)
	}
}

func TestFailedFloatingCompatibilityBootRequestsAReplacement(t *testing.T) {
	err := compatibilityBootFailure("ubuntu-2404-x64@g20260815033431", true,
		os.ErrDeadlineExceeded)

	if got := cli.ExitStatus(err); got != 2 {
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

	cfg := &config.Config{
		Node: &config.NodeConfig{
			Name:     "epyc-1",
			Provider: config.ProviderFirecracker,
			Site:     "home",
		},
		Tiers: []config.Tier{
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
	got, err := FirecrackerTierImages(cfg)
	if err != nil {
		t.Fatalf("select configured firecracker images: %v", err)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("configured firecracker images = %v, want %v", got, want)
	}
}

func TestCompatibilityChecksOnlyImagesThisNodeCanRun(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Node: &config.NodeConfig{
			Name:     "epyc-1",
			Provider: config.ProviderFirecracker,
			Site:     "home",
		},
		Tiers: []config.Tier{
			{Provider: config.ProviderFirecracker, Image: "shared@verified"},
			{Provider: config.ProviderFirecracker, Image: "this-node@verified", Node: "epyc-1"},
			{Provider: config.ProviderFirecracker, Image: "other-node@verified", Node: "epyc-2"},
			{Provider: config.ProviderFirecracker, Image: "this-site@verified", Site: "home"},
			{Provider: config.ProviderFirecracker, Image: "other-site@verified", Site: "cloud"},
		},
	}

	want := []string{"shared@verified", "this-node@verified", "this-site@verified"}
	got, err := FirecrackerTierImages(cfg)
	if err != nil {
		t.Fatalf("select eligible firecracker images: %v", err)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("eligible firecracker images = %v, want %v", got, want)
	}
}

func TestCompatibilityChecksNoImagesForANonFirecrackerNode(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Node: &config.NodeConfig{
			Name:     "cloud-1",
			Provider: config.ProviderEC2,
			Site:     "cloud",
		},
		Tiers: []config.Tier{
			{Provider: config.ProviderFirecracker, Image: "other-host@verified", Site: "home"},
			{Providers: []config.ProviderKind{config.ProviderFirecracker, config.ProviderEC2},
				Launch: map[config.ProviderKind]config.TierLaunch{
					config.ProviderFirecracker: {Image: "fallback@verified"},
					config.ProviderEC2:         {Image: "ami-123"},
				}},
		},
	}

	got, err := FirecrackerTierImages(cfg)
	if err != nil {
		t.Fatalf("select non-firecracker node images: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("non-firecracker node images = %v, want none", got)
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

// fakeCompatibleStore answers checkImageCompatible for one exact, verified
// generation speaking this binary's contract, resolving it with resolveErr.
type fakeCompatibleStore struct {
	generation string
	resolveErr error
	resolved   []string
}

func (f *fakeCompatibleStore) NewestVerifiedForContract(context.Context, string, string) (ceph.Generation, bool, error) {
	return ceph.Generation{}, false, nil
}

func (f *fakeCompatibleStore) NewestForContract(context.Context, string, string) (ceph.Generation, bool, error) {
	return ceph.Generation{}, false, nil
}

func (f *fakeCompatibleStore) NewestVerified(context.Context, string) (ceph.Generation, bool, error) {
	return ceph.Generation{}, false, nil
}

func (f *fakeCompatibleStore) Generations(context.Context, string) ([]ceph.Generation, error) {
	gen, _ := ceph.ParseGeneration(f.generation)

	return []ceph.Generation{gen}, nil
}

func (f *fakeCompatibleStore) VerifiedGenerations(context.Context, string) (map[string]bool, error) {
	return map[string]bool{f.generation: true}, nil
}

func (f *fakeCompatibleStore) GuestContract(context.Context, string, string) (string, bool, error) {
	return firecracker.GuestContract, true, nil
}

func (f *fakeCompatibleStore) ResolveGeneration(_ context.Context, image, _ string) (string, error) {
	f.resolved = append(f.resolved, image)

	return image, f.resolveErr
}

// AN EXACT PIN OF ANOTHER MACHINE'S GENERATION IS NOT COMPATIBLE, though it exists,
// is verified and speaks this binary's contract: every launch of the tier would
// refuse it after the upgrade committed. A record that could not be read is said
// as could-not-tell, with no advice to move the pin; and this host's own is
// compatible with no download.
func TestImagesCompatibleRefusesAnExactPinOfAnotherMachine(t *testing.T) {
	const generation = "g20260815033431"

	exact := "ubuntu-2404-x64@" + generation

	for _, tc := range []struct {
		name       string
		resolveErr error
		want       []string
		refused    bool
	}{
		{
			name:       "another machine's",
			resolveErr: fmt.Errorf("%w: built for aarch64", ceph.ErrOtherMachine),
			want:       []string{"cannot be launched on this host", "update the exact pin"},
			refused:    true,
		},
		{
			name:       "a record that could not be read",
			resolveErr: errors.New("rbd timed out"),
			want:       []string{"could not tell whether this host can launch"},
			refused:    true,
		},
		{name: "this host's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeCompatibleStore{generation: generation, resolveErr: tc.resolveErr}

			restore := openCompatibleStore
			t.Cleanup(func() { openCompatibleStore = restore })
			openCompatibleStore = func(*config.Config) (compatibleStore, error) { return store, nil }

			var out strings.Builder
			err := checkImageCompatible(t.Context(), cli.Env{Stdout: &out, Stderr: &out}, &config.Config{},
				"", exact, time.Minute)

			if len(store.resolved) != 1 || store.resolved[0] != exact {
				t.Fatalf("the command resolved %v, want the exact pin %s once", store.resolved, exact)
			}

			if !tc.refused {
				if err != nil || !strings.Contains(out.String(), "no image download is needed") {
					t.Fatalf("checkImageCompatible = %v (%s), want this host's pin compatible", err, out.String())
				}

				return
			}

			if err == nil {
				t.Fatal("the pin was reported compatible")
			}

			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal does not say %q: %v", want, err)
				}
			}

			if tc.resolveErr != nil && !errors.Is(tc.resolveErr, ceph.ErrOtherMachine) &&
				strings.Contains(err.Error(), "update the exact pin") {
				t.Errorf("a record that could not be read was answered with advice to move the pin: %v", err)
			}
		})
	}
}
