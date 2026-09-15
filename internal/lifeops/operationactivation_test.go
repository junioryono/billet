package lifeops

import (
	"strings"
	"testing"
)

func TestOperationAdmissionChecksEveryQuietServiceActivationSource(t *testing.T) {
	for _, unit := range []string{"billet-server.service", "billet-backup.service", "billet-upgrade.service"} {
		for _, source := range []string{"external.path", "external.socket", "external.timer", "external.automount", "upholder.service"} {
			for _, state := range []string{"active", "inactive"} {
				t.Run(unit+"/"+source+"/"+state, func(t *testing.T) {
					f := newOperationFixture(t)
					quiet := f.unit(t, unit)
					trigger := f.unit(t, source)
					trigger["ActiveState"] = state
					if err := f.inspector.AdmitQuietActivation(t.Context(), []string{unit}, nil); err != nil {
						t.Fatalf("clean control: %v", err)
					}
					relation := "TriggeredBy"
					if source == "upholder.service" {
						relation = "UpheldBy"
					}
					quiet[relation] = source
					if err := f.inspector.AdmitQuietActivation(t.Context(), []string{unit}, nil); err == nil || !strings.Contains(err.Error(), "operation-unit-outside-set") {
						t.Fatalf("external trigger admitted: %v", err)
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
	if err := f.inspector.AdmitQuietActivation(t.Context(), units, nil); err == nil || !strings.Contains(err.Error(), "operation-reactivation") {
		t.Fatalf("stopped proof retained active timer exception: %v", err)
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
		t.Fatalf("source changed across proof: %v", err)
	}
}

func TestQuietActivationDefersBackupActivityOnlyBeforeItsWait(t *testing.T) {
	for _, state := range []string{"activating", "failed"} {
		t.Run(state, func(t *testing.T) {
			f := newOperationFixture(t)
			backup := f.unit(t, "billet-backup.service")
			backup["ActiveState"] = state
			if state == "activating" {
				backup["Job"] = "42"
			}
			units := []string{"billet-backup.service"}
			if err := f.inspector.AdmitQuietActivation(t.Context(), units, nil, units...); err != nil {
				t.Fatalf("backup handling was intercepted: %v", err)
			}
			if err := f.inspector.AdmitQuietActivation(t.Context(), units, nil); err == nil || !strings.Contains(err.Error(), "operation-reactivation") {
				t.Fatalf("stopped proof allowed unfinished backup: %v", err)
			}
		})
	}
}
