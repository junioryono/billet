package firecracker

import (
	"strings"
	"testing"
)

// A GENERATION THAT NAMES ITS KERNEL BOOTS THAT KERNEL, not whatever this node
// happens to be configured with.
//
// This is what makes the pairing an invariant rather than bookkeeping. Recording
// which kernel a generation was verified against means nothing if the launch then
// uses a different one -- and a guest booted with the wrong kernel fails in the
// middle of somebody's job, which is the failure the pairing exists to prevent.
func TestKernelForGenerationPrefersWhatTheGenerationRecorded(t *testing.T) {
	got, err := kernelForGeneration("vmlinux-6.1.155-ea1d42638d13",
		"/var/lib/billet/kernels", "/etc/billet/some-other-kernel")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	want := "/var/lib/billet/kernels/vmlinux-6.1.155-ea1d42638d13"

	if got != want {
		t.Errorf("resolved %q, want %q; the configured kernel won over the one this "+
			"generation was verified against", got, want)
	}
}

// A GENERATION THAT RECORDS NOTHING FALLS BACK TO THE CONFIGURED KERNEL.
//
// Generations published by build-guest-image.sh record no kernel -- it does not
// install one and genuinely does not know -- and refusing to launch them would
// break every deployment that builds by hand.
func TestKernelForGenerationFallsBackWhenNothingIsRecorded(t *testing.T) {
	got, err := kernelForGeneration("", "/var/lib/billet/kernels", "/srv/vmlinux-billet")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got != "/srv/vmlinux-billet" {
		t.Errorf("resolved %q, want the configured kernel", got)
	}
}

// A RECORDED VALUE THAT IS NOT A PLAIN FILE NAME IS REFUSED. It comes from cluster
// metadata, which any client with write access to the pool can set, and it is
// about to be joined to a path this process opens.
// THE MANAGED DIRECTORY IS CONFIGURABLE, so nothing may assume where it is.
func TestKernelForGenerationHonoursTheDirectoryItIsGiven(t *testing.T) {
	got, err := kernelForGeneration("vmlinux-6.1.155-ea1d42638d13",
		"/srv/billet/kernels", "/srv/configured")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}

	if got != "/srv/billet/kernels/vmlinux-6.1.155-ea1d42638d13" {
		t.Errorf("resolved %q; the managed directory was ignored", got)
	}
}

func TestKernelForGenerationRefusesARecordedValueThatIsNotAFileName(t *testing.T) {
	for _, recorded := range []string{
		"../../etc/shadow",
		"/etc/shadow",
		"sub/vmlinux",
		"..",
	} {
		if _, err := kernelForGeneration(recorded, "/var/lib/billet/kernels",
			"/srv/vmlinux"); err == nil {
			t.Errorf("%q was accepted as a recorded kernel name", recorded)
		}
	}
}

// NOTHING RECORDED AND NOTHING CONFIGURED IS AN ERROR, not an empty path handed to
// the VMM -- which would fail later, further away, with a message about a file
// called "".
func TestKernelForGenerationRefusesWhenThereIsNoKernelAtAll(t *testing.T) {
	_, err := kernelForGeneration("", "/var/lib/billet/kernels", "")
	if err == nil {
		t.Fatal("a launch with no kernel at all was allowed to proceed")
	}

	if !strings.Contains(strings.ToLower(err.Error()), "kernel") {
		t.Errorf("the refusal does not mention the kernel: %v", err)
	}
}
