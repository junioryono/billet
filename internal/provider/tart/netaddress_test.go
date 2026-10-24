package tart

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/junioryono/billet/internal/provider"
)

// AN ADDRESS ANOTHER VM IN THE STORE IS CONFIGURED WITH IS NO VM'S DEVICE: a clone
// keeps its image's address, so the bridge's one entry for it could be the other
// VM's link, and the network is left unmeasured. Another VM with an address of
// its own, or a directory with no configuration, changes nothing.
func TestAnAddressAnotherVMSharesNamesNoDevice(t *testing.T) {
	t.Parallel()

	for name, tc := range map[string]struct {
		config   string
		measured bool
	}{
		"another VM is configured with the same address": {config: `{"macAddress":"` + sharedMAC + `"}`},
		"another VM has an address of its own":           {config: `{"macAddress":"02:00:00:00:00:09"}`, measured: true},
		"another directory holds no configuration":       {measured: true},
		"another VM's configuration cannot be read":      {config: `{`},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h := newNetworkHost(t)
			h.withNetwork(t, network{mac: sharedMAC, tables: table, ps: psWith(),
				sockets: map[string]string{"74207": printed(unitLine("1"))}})
			other := filepath.Join(filepath.Dir(filepath.Dir(h.disk)), "operator-vm")
			if err := os.MkdirAll(other, 0o750); err != nil {
				t.Fatalf("make the other VM: %v", err)
			}
			if tc.config != "" {
				if err := os.WriteFile(filepath.Join(other, "config.json"), []byte(tc.config), 0o600); err != nil {
					t.Fatalf("write the other VM's configuration: %v", err)
				}
			}

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
