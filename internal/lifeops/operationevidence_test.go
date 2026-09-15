package lifeops

import (
	"os"
	"strings"
	"testing"
)

func TestOperationRereadRejudgesStateAndUnorderedEdges(t *testing.T) {
	for _, change := range []string{"edge order", "activity", "enablement", "timer unloaded", "unsafe trigger", "policy"} {
		t.Run(change, func(t *testing.T) {
			f := newOperationFixture(t)
			backup := f.unit(t, "billet-backup.service")
			f.unit(t, "billet-backup.timer")
			backup["After"] = "basic.target sysinit.target"
			backup["TriggeredBy"] = "billet-backup.timer"
			standard := f.unit(t, "sysinit.target")
			standard["ActiveState"] = "active"
			backup["Requires"] = "sysinit.target"
			p := OperationProtection{Units: []string{"billet-backup.service", "billet-backup.timer"}, QuietUnits: []string{"billet-backup.service"}, WaitingUnits: []string{"billet-backup.service"}}
			sequence := []Operation{{Verb: "stop", Unit: "billet-backup.timer"}}
			if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err != nil {
				t.Fatalf("clean control: %v", err)
			}
			reads := 0
			f.before = func(unit string) {
				if unit != "billet-backup.service" || !strings.Contains(f.calls[len(f.calls)-1], "--property=FragmentPath") {
					return
				}
				reads++
				if reads != 2 {
					return
				}
				switch change {
				case "edge order":
					backup["After"] = "sysinit.target basic.target"
				case "activity":
					backup["ActiveState"], backup["Job"] = "activating", "42"
				case "enablement":
					backup["UnitFileState"] = "static"
				case "timer unloaded":
					backup["TriggeredBy"] = ""
				case "unsafe trigger":
					backup["TriggeredBy"] = "external.path"
				case "policy":
					backup["SuccessAction"] = "reboot"
				}
			}
			err := f.inspector.AdmitOperations(t.Context(), sequence, p)
			if reads < 2 {
				t.Fatalf("did not reach reread: reads=%d error=%v", reads, err)
			}
			if change == "unsafe trigger" || change == "policy" {
				if err == nil {
					t.Fatal("reread admitted a new effect")
				}
				if change == "unsafe trigger" && !strings.Contains(err.Error(), "operation-edge-outside-set: billet-backup.service TriggeredBy=external.path") {
					t.Fatalf("trigger refused for unrelated reason: %v", err)
				}
				if change == "policy" && (!strings.Contains(err.Error(), "SuccessAction") || !strings.Contains(err.Error(), "none") || !strings.Contains(err.Error(), "reboot")) {
					t.Fatalf("policy diagnostic omitted property or values: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unchanged definition refused: %v", err)
			}
		})
	}
}

func TestOperationAdmissionQuietMaskRequiresPositiveEvidence(t *testing.T) {
	for _, state := range []string{"masked", "masked-runtime"} {
		for _, problem := range []string{"", "dev-null", "activity", "job", "fragment", "missing", "wrong target", "enablement", "load"} {
			t.Run(state+"/"+problem, func(t *testing.T) {
				f := newOperationFixture(t)
				timer := f.unit(t, "billet-backup.timer")
				path := timer["FragmentPath"]
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("/dev/null", path); err != nil {
					t.Fatal(err)
				}
				timer["LoadState"], timer["UnitFileState"] = "masked", state
				switch problem {
				case "dev-null":
					timer["FragmentPath"] = "/dev/null"
				case "activity":
					timer["ActiveState"] = "active"
				case "job":
					timer["Job"] = "42"
				case "fragment":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, nil, 0o644); err != nil {
						t.Fatal(err)
					}
				case "missing", "wrong target":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
					if problem == "wrong target" {
						if err := os.Symlink("/dev/zero", path); err != nil {
							t.Fatal(err)
						}
					}
				case "enablement":
					timer["UnitFileState"] = "enabled"
				case "load":
					timer["LoadState"] = "error"
				}
				p := OperationProtection{Units: []string{"billet-backup.timer"}}
				err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-backup.timer"}, {Verb: "disable", Unit: "billet-backup.timer"}}, p)
				if problem == "" || problem == "dev-null" {
					if err != nil {
						t.Fatalf("quiet mask refused: %v", err)
					}
					return
				}
				if problem == "missing" {
					if err == nil || !strings.Contains(err.Error(), "operation-mask-unreadable: lstat "+path) {
						t.Fatalf("missing mask became positive absence: %v", err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "operation-source-unsupported") ||
					!strings.Contains(err.Error(), "LoadState=") || !strings.Contains(err.Error(), "FragmentPath=") || !strings.Contains(err.Error(), "UnitFileState=") {
					t.Fatalf("incomplete mask proof admitted or unexplained: %v", err)
				}
			})
		}
	}
}

func TestOperationRereadRefusesAStandardStartThatCeasedToBeANoop(t *testing.T) {
	f := newOperationFixture(t)
	node := f.unit(t, "billet-node.service")
	node["Requires"] = "sysinit.target"
	standard := f.unit(t, "sysinit.target")
	standard["ActiveState"] = "active"
	p := OperationProtection{Units: []string{"billet-node.service"}}
	sequence := []Operation{{Verb: "start", Unit: "billet-node.service"}}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err != nil {
		t.Fatalf("active standard control: %v", err)
	}
	reads := 0
	f.before = func(unit string) {
		if unit == "sysinit.target" {
			reads++
			if reads == 2 {
				standard["ActiveState"] = "inactive"
			}
		}
	}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err == nil || !strings.Contains(err.Error(), "operation-standard-effect") {
		t.Fatalf("fresh standard activity was not judged: %v", err)
	}
}
