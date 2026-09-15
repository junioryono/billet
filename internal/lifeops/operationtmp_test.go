package lifeops

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOperationAdmissionProtectsPrivateTemporaryTrees(t *testing.T) {
	for _, unit := range []string{"billet-server.service", "billet-node.service", "billet-backup.service", "billet-upgrade.service"} {
		for _, rootName := range []string{"tmp", "var/tmp"} {
			t.Run(unit+"/"+rootName, func(t *testing.T) {
				f := newOperationFixture(t)
				props := f.unit(t, unit)
				props["PrivateTmp"] = "yes"
				props["Names"] += " extra.service"
				f.units["extra.service"] = props
				boot, tmp, varTmp := operationTemporaryFixture(t, f.inspector)
				root := tmp
				if rootName == "var/tmp" {
					root = varTmp
				}
				prefix := "systemd-private-" + strings.ReplaceAll(boot, "-", "") + "-" + unit + "-"
				for _, suffix := range []string{"first", "second"} {
					if err := os.MkdirAll(filepath.Join(root, prefix+suffix, "tmp"), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				sequence := []Operation{{Verb: "stop", Unit: "extra.service"}}
				p := OperationProtection{Units: []string{unit}, Paths: []string{filepath.Join(root, "unrelated", "node.key")}}
				if unit == "billet-backup.service" {
					sequence = nil // Completion is admitted even without a stop.
					p.WaitingUnits = []string{unit}
					props["ActiveState"], props["Job"] = "activating", "42"
				}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err != nil {
					t.Fatalf("unrelated temporary path refused: %v", err)
				}
				key := filepath.Join(root, prefix+"second", "tmp", "node.key")
				if err := os.WriteFile(key, []byte("retained"), 0o600); err != nil {
					t.Fatal(err)
				}
				p.UnitPaths = map[string][]string{unit: {key}}
				if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err == nil || !strings.Contains(err.Error(), "operation-directory-overlap") || !strings.Contains(err.Error(), "PrivateTmp=") {
					t.Fatalf("canonical private tree admitted: %v", err)
				}
				props["PrivateTmp"] = "no"
				if err := f.inspector.AdmitOperations(t.Context(), sequence, p); err != nil {
					t.Fatalf("PrivateTmp=no control refused: %v", err)
				}
			})
		}
	}
}

func operationTemporaryFixture(t *testing.T, i *Inspector) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	boot := "01234567-89ab-cdef-0123-456789abcdef"
	bootPath := filepath.Join(root, "boot_id")
	if err := os.WriteFile(bootPath, []byte(boot+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tmp, varTmp := filepath.Join(root, "tmp"), filepath.Join(root, "var", "tmp")
	for _, path := range []string{tmp, varTmp} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	WithOperationTemporaryDirectories(bootPath, tmp, varTmp)(i)
	return boot, tmp, varTmp
}

func TestOperationAdmissionRefusesUnknownPrivateTemporaryEvidence(t *testing.T) {
	for _, fault := range []string{"property", "boot missing", "boot malformed", "root unreadable"} {
		t.Run(fault, func(t *testing.T) {
			f := newOperationFixture(t)
			unit := f.unit(t, "billet-server.service")
			unit["PrivateTmp"] = "yes"
			operationTemporaryFixture(t, f.inspector)
			switch fault {
			case "property":
				unit["PrivateTmp"] = "unknown"
			case "boot missing":
				if err := os.Remove(f.inspector.operationBootIDPath); err != nil {
					t.Fatal(err)
				}
			case "boot malformed":
				if err := os.WriteFile(f.inspector.operationBootIDPath, []byte("not-a-boot-id"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "root unreadable":
				f.inspector.operationTempRoots = []string{f.inspector.operationBootIDPath}
			}
			if err := f.inspector.AdmitOperations(t.Context(), []Operation{{Verb: "stop", Unit: "billet-server.service"}}, OperationProtection{}); err == nil || !strings.Contains(err.Error(), "operation-private-tmp-unknown") {
				t.Fatalf("unknown private temporary evidence admitted: %v", err)
			}
		})
	}
}

func TestOperationAdmissionProtectsPrivateTmpPathTraversal(t *testing.T) {
	f := newOperationFixture(t)
	unit := f.unit(t, "billet-server.service")
	unit["PrivateTmp"] = "yes"
	boot, _, varTmp := operationTemporaryFixture(t, f.inspector)
	tree := filepath.Join(varTmp, "systemd-private-"+strings.ReplaceAll(boot, "-", "")+"-billet-server.service-fixture")
	if err := os.Mkdir(tree, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "node.key")
	if err := os.WriteFile(outside, []byte("retained"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tree, "node.key")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	sequence := []Operation{{Verb: "stop", Unit: "billet-server.service"}}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{Paths: []string{outside}}); err != nil {
		t.Fatalf("outside target control refused: %v", err)
	}
	if err := f.inspector.AdmitOperations(t.Context(), sequence, OperationProtection{Paths: []string{link}}); err == nil || !strings.Contains(err.Error(), "operation-directory-overlap") || !strings.Contains(err.Error(), "PrivateTmp=") {
		t.Fatalf("private-tree link to outside retained file admitted: %v", err)
	}
}
