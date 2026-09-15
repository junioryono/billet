package lifeops

import (
	"strings"
	"testing"
)

func TestOperationAdmissionRequiresDistinctProtectedRoles(t *testing.T) {
	roles := []string{"billet-server.service", "billet-node.service", "billet-backup.service", "billet-upgrade.service",
		"billet-backup.timer", "billet-upgrade.timer", "billet-network.service", "billet-dnsmasq@br0.service", "billet-dnsmasq@br1.service", "ledger.mount"}
	for _, role := range roles {
		for _, collision := range []string{"canonical", "names"} {
			t.Run(role+"/"+collision, func(t *testing.T) {
				f := newOperationFixture(t)
				other := "billet-node.service"
				if role == other {
					other = "billet-server.service"
				}
				unit := f.unit(t, role)
				peer := f.unit(t, other)
				if role == "ledger.mount" {
					unit["Where"], unit["ActiveState"] = "/ledger", "active"
				}
				p := OperationProtection{Units: []string{role, other}}
				if role == "ledger.mount" {
					p.RequiredActive = []string{role}
				}
				if err := f.inspector.AdmitOperations(t.Context(), nil, p); err != nil {
					t.Fatalf("distinct roles refused: %v", err)
				}
				if collision == "canonical" {
					// Only the shared Id collides; neither Names list claims the
					// peer role, so this kills removal of the Id comparison alone.
					peer["Id"] = "shared.service"
					unit["Id"] = peer["Id"]
					unit["Names"] += " shared.service"
					peer["Names"] += " shared.service"
				} else {
					unit["Names"] += " " + other
				}
				if err := f.inspector.AdmitOperations(t.Context(), nil, p); err == nil || !strings.Contains(err.Error(), "operation-role-collision") {
					t.Fatalf("protected role collision admitted: %v", err)
				}
			})
		}
	}
}
