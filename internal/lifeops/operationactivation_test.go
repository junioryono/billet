package lifeops

import (
	"strings"
	"testing"
)

func TestOperationAdmissionChecksEveryQuietServiceActivationSource(t *testing.T) {
	for _, unit := range []string{"billet-server.service", "billet-backup.service", "billet-upgrade.service"} {
		for _, source := range []string{"external.path", "external.socket", "external.timer", "upholder.service"} {
			for _, verb := range []string{"enable", "disable", "stop", "start", "filesystem"} {
				t.Run(unit+"/"+source+"/"+verb, func(t *testing.T) {
					f := newOperationFixture(t)
					quiet := f.unit(t, unit)
					trigger := f.unit(t, source)
					f.unit(t, "billet-node.service")
					relation := "TriggeredBy"
					if source == "upholder.service" {
						relation = "UpheldBy"
					}
					quiet[relation] = source
					var sequence []Operation
					if verb != "filesystem" {
						sequence = []Operation{{Verb: verb, Unit: "billet-node.service"}}
					}
					protection := OperationProtection{QuietUnits: []string{unit}}
					if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err != nil {
						t.Fatalf("inactive source control: %v", err)
					}
					trigger["ActiveState"] = "active"
					if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil || !strings.Contains(err.Error(), "operation-reactivation") {
						t.Fatalf("active source escaped admission: %v", err)
					}
					trigger["ActiveState"], trigger["Job"] = "inactive", "42"
					if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil {
						t.Fatal("source with queued job admitted")
					}
					trigger["Job"] = ""
					delete(trigger, "ActiveState")
					if err := f.inspector.AdmitOperations(t.Context(), sequence, protection); err == nil {
						t.Fatal("unknown source activity admitted")
					}
				})
			}
		}
	}
}

func TestQuietActivationExceptionsExpireAtStoppedProof(t *testing.T) {
	f := newOperationFixture(t)
	backup := f.unit(t, "billet-backup.service")
	timer := f.unit(t, "billet-backup.timer")
	backup["TriggeredBy"], timer["ActiveState"] = "billet-backup.timer", "active"
	units := []string{"billet-backup.service"}
	if err := f.inspector.AdmitQuietActivation(t.Context(), units, []string{"billet-backup.timer"}); err != nil {
		t.Fatalf("timer scheduled for stop refused: %v", err)
	}
	if err := f.inspector.AdmitQuietActivation(t.Context(), units, nil); err == nil {
		t.Fatal("stopped proof retained an active-timer exception")
	}
	timer["ActiveState"] = "inactive"
	if err := f.inspector.AdmitQuietActivation(t.Context(), units, nil); err != nil {
		t.Fatalf("stopped timer refused: %v", err)
	}
	reads := 0
	f.before = func(unit string) {
		if unit == "billet-backup.timer" {
			reads++
			if reads == 2 {
				timer["ActiveState"] = "active"
			}
		}
	}
	if err := f.inspector.AdmitQuietActivation(t.Context(), units, nil); err == nil || !strings.Contains(err.Error(), "operation-activation-changed") {
		t.Fatalf("source changed across the proof: %v", err)
	}
}
