package lifeops

import (
	"os"
	"strings"
	"testing"
)

func TestProposedUnitPreservesEnvironmentOptionalityAndInstalledBytes(t *testing.T) {
	for _, optional := range []bool{false, true} {
		t.Run(map[bool]string{false: "required", true: "optional"}[optional], func(t *testing.T) {
			f := newOperationFixture(t)
			props := f.unit(t, "billet-node.service")
			line, err := RenderEnvironmentFiles([]EnvironmentFile{{Path: "/etc/node.env", IgnoreErrors: optional}})
			if err != nil {
				t.Fatal(err)
			}
			body := "[Service]\n" + line
			path := props["FragmentPath"]
			if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := f.inspector.AdmitUnitReplacement(t.Context(), "billet-node.service", path, body, line); err != nil {
				t.Fatal(err)
			}
			opposite, err := RenderEnvironmentFiles([]EnvironmentFile{{Path: "/etc/node.env", IgnoreErrors: !optional}})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.inspector.AdmitUnitReplacement(t.Context(), "billet-node.service", path, body, opposite); err == nil || !strings.Contains(err.Error(), "environment-mismatch") {
				t.Fatalf("optionality mismatch admitted: %v", err)
			}
			if err := f.inspector.AdmitUnitReplacement(t.Context(), "billet-node.service", path, body+"User=other\n", line); err == nil || !strings.Contains(err.Error(), "proposed-unit-changed") {
				t.Fatalf("changed proposed operands admitted: %v", err)
			}
			props["NeedDaemonReload"] = "yes"
			if err := f.inspector.AdmitUnitReplacement(t.Context(), "billet-node.service", path, body, line); err == nil || !strings.Contains(err.Error(), "sources differ") {
				t.Fatalf("stale manager definition admitted: %v", err)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != body {
				t.Fatalf("admission changed installed unit: %v", err)
			}
		})
	}
}
