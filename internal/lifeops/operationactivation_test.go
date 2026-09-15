package lifeops

import (
	"fmt"
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

func TestQuietActivationClosesEveryReverseRelationship(t *testing.T) {
	for _, relation := range []string{"TriggeredBy", "UpheldBy", "OnSuccessOf", "OnFailureOf", "WantedBy", "RequiredBy", "BoundBy", "RequisiteOf", "ConsistsOf"} {
		for _, source := range []string{"watcher.path", "watcher.socket", "watcher.timer", "watcher.automount", "upholder.service", "completing.service"} {
			t.Run(relation+"/"+source, func(t *testing.T) {
				f := newOperationFixture(t)
				backup := f.unit(t, "billet-backup.service")
				helper := f.unit(t, "helper@instance.service")
				middle := f.unit(t, "middle.service")
				trigger := f.unit(t, source)
				backup[relation] = "helper@instance.service"
				helper["RequiredBy"] = "middle.service"
				last := "TriggeredBy"
				if source == "upholder.service" {
					last = "UpheldBy"
				} else if source == "completing.service" {
					last = "OnSuccessOf"
				}
				middle[last] = source
				units := []string{"billet-backup.service"}
				if err := f.inspector.AdmitQuietActivation(t.Context(), units, nil); err != nil {
					t.Fatalf("inactive closure control: %v", err)
				}
				trigger["ActiveState"] = "active"
				if err := f.inspector.AdmitQuietActivation(t.Context(), units, nil); err == nil || !strings.Contains(err.Error(), "operation-reactivation: "+source) {
					t.Fatalf("transitive source admitted: %v", err)
				}
			})
		}
	}
}

func TestQuietActivationRevisitsAliasesAndRefusesIncompleteClosure(t *testing.T) {
	for _, defect := range []string{"alias completion", "revisited completion", "unreadable", "bound"} {
		t.Run(defect, func(t *testing.T) {
			f := newOperationFixture(t)
			backup := f.unit(t, "billet-backup.service")
			helper := f.unit(t, "helper.service")
			backup["WantedBy"] = "helper.service"
			helper["ActiveState"] = "active"
			if err := f.inspector.AdmitQuietActivation(t.Context(), []string{"billet-backup.service"}, nil); err != nil {
				t.Fatalf("active dependency is not an armed source: %v", err)
			}
			switch defect {
			case "alias completion":
				backup["OnFailureOf"] = "alias.service"
				alias := f.unit(t, "alias.service")
				alias["Id"], alias["Names"], alias["ActiveState"] = "helper.service", "helper.service alias.service", "active"
				helper["Names"] = alias["Names"]
			case "revisited completion":
				backup["RequiredBy"] = "middle.service"
				middle := f.unit(t, "middle.service")
				middle["OnSuccessOf"] = "helper.service"
			case "unreadable":
				delete(helper, "OnSuccessOf")
			case "bound":
				for n := range operationUnitLimit {
					name := fmt.Sprintf("source-%d.service", n)
					helper["WantedBy"] = name
					helper = f.unit(t, name)
				}
			}
			if err := f.inspector.AdmitQuietActivation(t.Context(), []string{"billet-backup.service"}, nil); err == nil {
				t.Fatalf("%s escaped the reverse closure", defect)
			}
		})
	}
}
