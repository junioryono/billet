package host

import (
	"testing"

	"github.com/junioryono/billet/internal/lifeops"
)

// The unit files a retained node's converge writes or removes without a proposed unit
// document, each by its exact name in the manager's own directory, and nothing beside
// them.
func TestOnlyTheRolesOwnUnitsAreOrdinaryUnitDestinations(t *testing.T) {
	saved := retireOperationInspector
	t.Cleanup(func() { retireOperationInspector = saved })
	retireOperationInspector = func() *lifeops.Inspector {
		return lifeops.NewInspector(lifeops.WithOperationUnitDirectories("/etc/systemd/system"))
	}

	for path, want := range map[string]bool{
		"/etc/systemd/system/billet-network.service":         true,
		"/etc/systemd/system/billet-images-refresh.service":  true,
		"/etc/systemd/system/billet-images-refresh.timer":    true,
		"/etc/systemd/system/billet-ceph-health.service":     true,
		"/etc/systemd/system/billet-ceph-health.timer":       true,
		"/etc/systemd/system/billet-ceph-osd-memory.service": true,
		"/etc/systemd/system/billet-ceph-osd-memory.timer":   true,
		"/etc/systemd/system/billet-dnsmasq@br0.service":     true,
		"/etc/systemd/system/billet-ceph-osd-memory.socket":  false,
		"/etc/systemd/system/billet-node.service":            false,
		"/etc/systemd/system/billet-server.service":          false,
		"/run/systemd/system/billet-ceph-osd-memory.timer":   false,
		"/usr/local/libexec/billet-ceph-osd-memory":          false,
	} {
		if got := retireOrdinaryUnitDestination(path); got != want {
			t.Errorf("retireOrdinaryUnitDestination(%q) = %v, want %v", path, got, want)
		}
	}
}
