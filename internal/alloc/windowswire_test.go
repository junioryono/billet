package alloc

import (
	"slices"
	"testing"

	"github.com/junioryono/billet/internal/config"
)

// A WINDOWS TIER IS NOT PLACED ON A HOST TOO OLD TO BOOT ITS GUEST, so placement does
// not keep choosing a host the plane then refuses to launch on while a newer one sits
// idle. The older host is named to win placement's ordering, and a Linux tier still
// uses it.
func TestAWindowsTierIsPlacedOnlyOnAHostThatBootsIt(t *testing.T) {
	t.Parallel()

	linux := config.Tier{
		Label: "linux", Provider: config.ProviderEC2, GuestOS: config.GuestLinux,
		VCPU: 2, Memory: 4 * config.GiB, Image: "ami-0123456789abcdef0",
	}
	windows := linux
	windows.Label, windows.GuestOS = "windows", config.GuestWindows

	a := newBareAllocator(t, Limits{MaxVCPU: 64, MaxMemory: 128 * config.GiB},
		[]config.Tier{linux, windows})

	old := testRegistration("a-old-host", config.ProviderEC2)
	old.WireMin, old.WireVersion, old.WireMax = 12, WindowsGuestsWireVersion-1, WindowsGuestsWireVersion-1
	if _, err := a.RegisterNode(t.Context(), old); err != nil {
		t.Fatalf("RegisterNode(old): %v", err)
	}

	if got := headroom(t, a, "windows"); got != 0 {
		t.Fatalf("Windows headroom on an old host = %d, want 0", got)
	}
	if got := headroom(t, a, "linux"); got == 0 {
		t.Fatal("a Linux tier could not use an old host")
	}

	for _, tc := range []struct {
		tier config.Tier
		want bool
	}{{windows, true}, {linux, false}} {
		if waits, err := a.WaitsForWindowsHost(t.Context(), tc.tier); err != nil || waits != tc.want {
			t.Fatalf("tier %s waits = %v, %v; want %v", tc.tier.Label, waits, err, tc.want)
		}
	}

	// `billet check` READS THIS, and a host that can never start the tier's guest is
	// not one it may list as able to place it.
	hosts, err := a.ShapeHosts(t.Context())
	if err != nil {
		t.Fatalf("ShapeHosts: %v", err)
	}
	if got := hosts["windows"]; len(got) != 0 {
		t.Fatalf("ShapeHosts lists %v for the Windows tier, want none", got)
	}
	if got := hosts["linux"]; !slices.Contains(got, "a-old-host") {
		t.Fatalf("ShapeHosts lists %v for the Linux tier, want a-old-host", got)
	}

	current := testRegistration("b-new-host", config.ProviderEC2)
	current.WireMin, current.WireVersion, current.WireMax = 12, WindowsGuestsWireVersion, WindowsGuestsWireVersion
	if _, err := a.RegisterNode(t.Context(), current); err != nil {
		t.Fatalf("RegisterNode(new): %v", err)
	}

	if waits, err := a.WaitsForWindowsHost(t.Context(), windows); err != nil || waits {
		t.Fatalf("with a host on the version, the Windows tier waits = %v, %v", waits, err)
	}

	leases, err := a.Escrow(t.Context(), "windows", 1)
	if err != nil || len(leases) != 1 {
		t.Fatalf("Escrow = %v, %v", leases, err)
	}
	if leases[0].TargetNode != "b-new-host" {
		t.Fatalf("Windows lease placed on %q, want b-new-host", leases[0].TargetNode)
	}
}
