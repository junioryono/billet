package tart

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// AN ENTRY THAT CANNOT BE READ AS A VM, OR A STORE PAST THE BOUND, IS
// COULD-NOT-TELL: a symlink, a configuration that is not a regular file, and a
// store of more entries than are compared leave the network unmeasured.
func TestAStoreThatCannotBeReadWholeNamesNoDevice(t *testing.T) {
	t.Parallel()

	for name, stage := range map[string]func(t *testing.T, vms string){
		"an entry is a symlink": func(t *testing.T, vms string) {
			t.Helper()
			if err := os.Symlink(t.TempDir(), filepath.Join(vms, "linked-vm")); err != nil {
				t.Fatal(err)
			}
		},
		"a configuration is a directory": func(t *testing.T, vms string) {
			t.Helper()
			if err := os.MkdirAll(filepath.Join(vms, "odd-vm", "config.json"), 0o750); err != nil {
				t.Fatal(err)
			}
		},
		// EVERY ONE A VM OF ITS OWN ADDRESS, so the bound alone refuses.
		"the store is past the bound": func(t *testing.T, vms string) {
			t.Helper()
			for i := range 256 {
				dir := filepath.Join(vms, fmt.Sprintf("vm-%03d", i))
				config := fmt.Sprintf(`{"macAddress":"02:00:00:00:01:%02x"}`, i)
				if err := os.Mkdir(dir, 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(config), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newNetworkHost(t)
			h.withNetwork(t, network{mac: sharedMAC, tables: table, ps: psWith(),
				sockets: map[string]string{"74207": printed(unitLine("1"))}})
			stage(t, filepath.Dir(filepath.Dir(h.disk)))

			target, err := h.p.UsageTarget(t.Context(), usageVM)
			if err != nil {
				t.Fatalf("UsageTarget: %v", err)
			}
			if want := (provider.UsageTarget{PID: 74207, PIDStart: 16664085551217, Process: true}); target != want {
				t.Errorf("the VM is %+v, want %+v, with no device", target, want)
			}
		})
	}
}

// A VM ANOTHER ACCOUNT RUNS ON THE HOST LEAVES EVERY VM'S NETWORK UNMEASURED: its
// store is one billet cannot read, so an address it shares would not be seen. The
// node account's own VMs, however many, do not.
func TestAVMAnotherAccountRunsNamesNoDevice(t *testing.T) {
	t.Parallel()

	me := os.Getuid()
	for name, tc := range map[string]struct {
		owners   string
		measured bool
	}{
		"two of the node's VMs": {owners: fmt.Sprintf("%d %s\n%d %s\n", me, vmService, me, vmService),
			measured: true},
		"one of another account's":         {owners: fmt.Sprintf("%d %s\n%d %s\n", me, vmService, me+1, vmService)},
		"a line ps could not have written": {owners: "x " + vmService + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newNetworkHost(t)
			h.withNetwork(t, network{mac: sharedMAC, tables: table, ps: psWith(), owners: tc.owners,
				sockets: map[string]string{"74207": printed(unitLine("1"))}})
			target, err := h.p.UsageTarget(t.Context(), usageVM)
			if err != nil {
				t.Fatalf("UsageTarget: %v", err)
			}
			want := provider.UsageTarget{PID: 74207, PIDStart: 16664085551217, Process: true}
			if tc.measured {
				want.NetDevice, want.NetHostView = "vmenet0", true
			}
			if target != want {
				t.Errorf("the VM is %+v, want %+v", target, want)
			}
		})
	}
}
