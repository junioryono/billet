package ceph

import (
	"strings"
	"testing"
)

// A KERNEL AND ITS GENERATION ARE A MATCHED PAIR, so a kernel is removable exactly
// when no surviving generation names it. Every pull installs one, so without this
// a node accumulates 46MB a week forever.
func TestPlanKernelReapRemovesOnlyWhatNothingNeeds(t *testing.T) {
	onDisk := []string{
		"vmlinux-6.1.155-ea1d42638d13",
		"vmlinux-6.1.155-aaaaaaaaaaaa",
		"vmlinux-6.1.140-bbbbbbbbbbbb",
	}

	needed := map[string]bool{"vmlinux-6.1.155-ea1d42638d13": true}

	got, err := PlanKernelReap(onDisk, needed, 3)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	want := map[string]bool{
		"vmlinux-6.1.155-aaaaaaaaaaaa": true,
		"vmlinux-6.1.140-bbbbbbbbbbbb": true,
	}

	if len(got) != len(want) {
		t.Fatalf("planned %v, want %v", got, want)
	}

	for _, name := range got {
		if !want[name] {
			t.Errorf("planned to remove %q, which a generation still needs", name)
		}
	}
}

// TWO KERNELS OF ONE VERSION ARE TWO FILES, and this is why the generation records
// the file name rather than the version: matching on version alone would remove a
// kernel a generation is verified against because a different build happened to
// share its version.
func TestPlanKernelReapDistinguishesKernelsSharingAVersion(t *testing.T) {
	onDisk := []string{
		"vmlinux-6.1.155-ea1d42638d13",
		"vmlinux-6.1.155-ffffffffffff",
	}

	needed := map[string]bool{"vmlinux-6.1.155-ffffffffffff": true}

	got, err := PlanKernelReap(onDisk, needed, 1)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if len(got) != 1 || got[0] != "vmlinux-6.1.155-ea1d42638d13" {
		t.Fatalf("planned %v; the two kernels share a version and only one is needed", got)
	}
}

// A FILE THIS DID NOT WRITE IS LEFT ALONE. The directory is a real path an operator
// can put things in, and a reaper that deletes what it does not recognise is one
// nobody should point at a directory.
func TestPlanKernelReapIgnoresFilesItDoesNotRecognise(t *testing.T) {
	onDisk := []string{
		"vmlinux-6.1.155-ea1d42638d13",
		"README",
		"vmlinux-custom",
		".hidden",
		"vmlinux-6.1.155",
	}

	got, err := PlanKernelReap(onDisk, map[string]bool{}, 0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	for _, name := range got {
		if name != "vmlinux-6.1.155-ea1d42638d13" {
			t.Errorf("planned to remove %q, which this did not write", name)
		}
	}
}

// NO GENERATION NEEDS ANYTHING, BUT GENERATIONS EXIST -- that is far more likely to
// mean the metadata could not be read than that every kernel is orphaned, and
// acting on it deletes the kernel every running tier boots.
func TestPlanKernelReapRefusesToReapEverythingWhenGenerationsExist(t *testing.T) {
	onDisk := []string{"vmlinux-6.1.155-ea1d42638d13"}

	_, err := PlanKernelReap(onDisk, map[string]bool{}, 4)
	if err == nil {
		t.Fatal("planned to remove every kernel while four generations exist; that is a " +
			"metadata read that failed, not a directory of orphans")
	}

	if !strings.Contains(err.Error(), "4") {
		t.Errorf("the refusal does not say how many generations there are: %v", err)
	}
}

// AND WITH NO GENERATIONS AT ALL, everything is genuinely orphaned.
func TestPlanKernelReapReapsAllWhenThereAreNoGenerations(t *testing.T) {
	onDisk := []string{"vmlinux-6.1.155-ea1d42638d13", "vmlinux-6.1.140-bbbbbbbbbbbb"}

	got, err := PlanKernelReap(onDisk, map[string]bool{}, 0)
	if err != nil {
		t.Fatalf("plan: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("planned %v, want both removed", got)
	}
}
