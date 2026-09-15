package lifeops

import (
	"strings"
	"testing"
)

func TestOperationAdmissionLimitsDeviceStopPropagationToLedger(t *testing.T) {
	for _, c := range []struct{ unit, property, destination string }{
		{"ledger.mount", "StopPropagatedFrom", "dev-vdc1.device"},
		{"ledger.mount", "PropagatesStopTo", "dev-vdb1.device"},
		{"billet-server.service", "StopPropagatedFrom", "dev-vdb1.device"},
		{"billet-server.service", "PropagatesStopTo", "dev-vdb1.device"},
		{"billet-node.service", "StopPropagatedFrom", "dev-vdb1.device"},
		{"billet-node.service", "PropagatesStopTo", "dev-vdb1.device"},
	} {
		t.Run(c.unit+"/"+c.property+"/"+c.destination, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			f.unit(t, "billet-node.service")
			server["Requires"], server["RequiresMountsFor"] = "ledger.mount", "/ledger"
			mount := f.unit(t, "ledger.mount")
			mount["ActiveState"], mount["What"] = "active", "/dev/vdb1"
			mount["StopPropagatedFrom"] = "dev-vdb1.device"
			protection := OperationProtection{
				Units:     []string{"billet-server.service", "billet-node.service"},
				UnitPaths: map[string][]string{"billet-server.service": {"/ledger"}},
			}
			sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
				t.Fatalf("exact What-derived device edge refused: %v", err)
			}
			f.units[c.unit][c.property] = c.destination
			want := "operation-edge-outside-set: " + c.unit + " " + c.property + "=" + c.destination
			if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("device edge escaped its exact direction, mount or backing device: %v", err)
			}
		})
	}
}

func TestOperationAdmissionLimitsInverseDeviceStopPropagationToLedger(t *testing.T) {
	f := newOperationFixture(t)
	f.unit(t, "billet-server.service")
	f.unit(t, "billet-node.service")
	mount := f.unit(t, "ledger.mount")
	mount["ActiveState"], mount["What"] = "active", "/dev/vdb1"
	w := operationWalk{
		inspector: f.inspector,
		protection: OperationProtection{
			Units:          []string{"ledger.mount", "billet-server.service", "billet-node.service"},
			RequiredActive: []string{"ledger.mount"},
		},
		units: make(map[string]operationEvidence), targets: make(map[string]bool),
		paths: make(map[string]operationPathBinding), stopped: make(map[string]bool), standard: make(map[string]bool),
	}
	if err := w.admitClosedSet(t.Context()); err != nil {
		t.Fatalf("clean protected units: %v", err)
	}
	// Device units remain standard leaves; exercise their inverse spelling at
	// the same edge gate without expanding traversal into the device's graph.
	if err := w.closedRelation(t.Context(), "dev-vdb1.device", "PropagatesStopTo", "ledger.mount"); err != nil {
		t.Fatalf("exact inverse device edge refused: %v", err)
	}
	for _, c := range []struct{ device, property, destination string }{
		{"dev-vdc1.device", "PropagatesStopTo", "ledger.mount"},
		{"dev-vdb1.device", "StopPropagatedFrom", "ledger.mount"},
		{"dev-vdb1.device", "PropagatesStopTo", "billet-server.service"},
		{"dev-vdb1.device", "StopPropagatedFrom", "billet-server.service"},
		{"dev-vdb1.device", "PropagatesStopTo", "billet-node.service"},
		{"dev-vdb1.device", "StopPropagatedFrom", "billet-node.service"},
	} {
		t.Run(c.device+"/"+c.property+"/"+c.destination, func(t *testing.T) {
			want := "operation-edge-outside-set: " + c.device + " " + c.property + "=" + c.destination
			if err := w.closedRelation(t.Context(), c.device, c.property, c.destination); err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("inverse device edge escaped its exact direction, mount or backing device: %v", err)
			}
		})
	}
	t.Run("device follows What", func(t *testing.T) {
		w.units["ledger.mount"].props["What"] = []string{"/dev/disk/by-id/ledger-volume"}
		if err := w.closedRelation(t.Context(), "dev-vdb1.device", "PropagatesStopTo", "ledger.mount"); err == nil || !strings.Contains(err.Error(), "operation-edge-outside-set") {
			t.Fatalf("old backing device remained admitted: %v", err)
		}
		device := `dev-disk-by\x2did-ledger\x2dvolume.device`
		if err := w.closedRelation(t.Context(), device, "PropagatesStopTo", "ledger.mount"); err != nil {
			t.Fatalf("exact escaped What-derived inverse refused: %v", err)
		}
		if err := w.closedRelation(t.Context(), "ledger.mount", "StopPropagatedFrom", device); err != nil {
			t.Fatalf("exact escaped What-derived forward edge refused: %v", err)
		}
	})
}
