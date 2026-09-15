package lifeops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationAdmissionReadsCurrentTerminationPolicyLast(t *testing.T) {
	for _, mode := range []string{"control-group", "mixed", "process", "none", ""} {
		t.Run(mode, func(t *testing.T) {
			f := newOperationFixture(t)
			p := f.unit(t, "billet-server.service")
			p["KillMode"] = mode
			err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-server.service"}}, OperationProtection{})
			if mode == "control-group" || mode == "mixed" {
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(f.calls[len(f.calls)-1], "--property=KillMode") {
					t.Fatal("termination policy was read before blocking evidence")
				}
			} else if err == nil || !strings.Contains(err.Error(), "operation-termination-") {
				t.Fatalf("unsupported termination admitted: %v", err)
			}
			delete(p, "KillMode")
			if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-server.service"}}, OperationProtection{}); err == nil {
				t.Fatal("unreadable termination policy admitted")
			}
		})
	}
}

func TestOperationProcessProofRequiresPositiveCgroupDisappearance(t *testing.T) {
	for _, state := range []string{"absent", "empty", "survivor", "descendant", "missing procs", "unreadable procs", "unknown group", "unknown root"} {
		t.Run(state, func(t *testing.T) {
			f := newOperationFixture(t)
			p := f.unit(t, "billet-server.service")
			p["MainPID"], p["ControlPID"], p["ControlGroup"], p["Slice"] = "0", "0", "/system.slice/billet-server.service", "system.slice"
			root := t.TempDir()
			f.inspector.operationCgroupRoot = root
			group := filepath.Join(root, "system.slice/billet-server.service")
			if state != "absent" {
				if err := os.MkdirAll(group, 0o755); err != nil {
					t.Fatal(err)
				}
				if state != "missing procs" {
					if err := os.WriteFile(filepath.Join(group, "cgroup.procs"), nil, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch state {
			case "survivor":
				if err := os.WriteFile(filepath.Join(group, "cgroup.procs"), []byte("4242\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "descendant":
				child := filepath.Join(group, "child")
				if err := os.Mkdir(child, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(child, "cgroup.procs"), []byte("4242\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "unreadable procs":
				if err := os.Remove(filepath.Join(group, "cgroup.procs")); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(group, "cgroup.procs"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "unknown group":
				delete(p, "ControlGroup")
			case "unknown root":
				f.inspector.operationCgroupRoot = filepath.Join(root, "not-mounted")
			}
			err := f.inspector.ProveUnitProcessesGone(t.Context(), "billet-server.service")
			if state == "absent" || state == "empty" {
				if err != nil {
					t.Fatalf("positive disappearance refused: %v", err)
				}
				p["ControlGroup"] = ""
				if err := f.inspector.ProveUnitProcessesGone(t.Context(), "billet-server.service"); err != nil {
					t.Fatalf("released standard cgroup reference: %v", err)
				}
			} else if err == nil {
				t.Fatalf("%s was accepted as no processes", state)
			}
		})
	}
}
