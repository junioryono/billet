package lifeops

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRetiredConditionRequiresTheExactTypedEffectiveTuple(t *testing.T) {
	const marker = "/var/lib/billet/retired/0123456789abcdef0123456789abcdef"
	for _, result := range []string{"-1", "0", "1"} {
		data := fmt.Sprintf(`[["ConditionPathExists",false,true,%q,%s]]`, marker, result)
		if err := proveRetiredConditionValue(operationTypedValue{Type: "a(sbbsi)", Data: json.RawMessage(data)}, marker); err != nil {
			t.Fatalf("historical result must not decide inertness: %v", err)
		}
	}
	valid := fmt.Sprintf(`["ConditionPathExists",false,true,%q,0]`, marker)
	wrapped := fmt.Sprintf("[%s]", valid)
	for _, data := range []string{
		`null`, `{}`, `[]`, `[null]`, `[["ConditionPathExists"]]`, fmt.Sprintf("[%s,%s]", valid, valid),
		strings.Replace(wrapped, "false,true", "true,true", 1),
		strings.Replace(wrapped, "false,true", "false,false", 1),
		strings.Replace(wrapped, "false,true", "null,true", 1),
		strings.Replace(wrapped, "false,true", `"false",true`, 1),
		strings.Replace(wrapped, marker, marker+"-other", 1),
		strings.Replace(wrapped, ",0]", ",2]", 1),
		strings.Replace(wrapped, ",0]", ",null]", 1),
		strings.Replace(wrapped, ",0]", ",0,0]", 1),
		strings.Replace(wrapped, "ConditionPathExists", "ConditionPathIsDirectory", 1),
	} {
		if err := proveRetiredConditionValue(operationTypedValue{Type: "a(sbbsi)", Data: json.RawMessage(data)}, marker); err == nil {
			t.Fatalf("accepted malformed or ineffective Conditions: %s", data)
		}
	}
	if err := proveRetiredConditionValue(operationTypedValue{Type: "as", Data: json.RawMessage(wrapped)}, marker); err == nil {
		t.Fatal("accepted the wrong type signature")
	}
}

func TestPendingReloadInventoryRefusesUnknownObservations(t *testing.T) {
	for _, inventory := range []string{`null`, `[]`, `{}`, `[{"unit":""}]`, `[{"unit":"a.service"},{"unit":"a.service"}]`, `[{"unit":"a.service"}]`} {
		for _, property := range []string{"NeedDaemonReload=no\n", "NeedDaemonReload=yes\n", "", "NeedDaemonReload=maybe\n", "NeedDaemonReload=no\nNeedDaemonReload=no\n"} {
			i := NewInspector(withRunner(func(_ context.Context, _ string, args []string) ([]byte, error) {
				if args[0] == "list-units" {
					return []byte(inventory), nil
				}
				if args[0] != "show" {
					t.Fatalf("inventory mutated the manager: %v", args)
				}
				return []byte(property), nil
			}))
			pending, err := i.PendingReloadUnits(t.Context())
			valid := inventory == `[{"unit":"a.service"}]` && (property == "NeedDaemonReload=no\n" || property == "NeedDaemonReload=yes\n")
			if (err == nil) != valid {
				t.Fatalf("inventory=%s property=%q: pending=%v error=%v", inventory, property, pending, err)
			}
			if valid && (len(pending) == 1) != (property == "NeedDaemonReload=yes\n") {
				t.Fatal("the pending flag was not preserved")
			}
		}
	}
}

func TestRetiredConditionEvidenceExemptsOnlyIncomingRuntimeEdges(t *testing.T) {
	for _, hazard := range []string{"clean", "zero evidence", "removed marker", "reset condition", "outgoing", "manager action", "install target"} {
		t.Run(hazard, func(t *testing.T) {
			f := newOperationFixture(t)
			server := f.unit(t, "billet-server.service")
			f.unit(t, "billet-node.service")
			marker := filepath.Join(f.root, "retired")
			if err := os.WriteFile(marker, []byte("retired"), 0o600); err != nil {
				t.Fatal(err)
			}
			data := fmt.Sprintf(`{"type":"a(sbbsi)","data":[["ConditionPathExists",false,true,%q,0]]}`, marker)
			f.pathReply = func(_, _ string) ([]byte, error) { return []byte(data), nil }
			proof, err := f.inspector.ProveRetiredConditionEvidence(t.Context(), "billet-server.service", marker)
			if err != nil {
				t.Fatal(err)
			}
			p := OperationProtection{Units: []string{"billet-server.service", "billet-node.service"}, RetiredConditions: []RetiredConditionEvidence{proof}}
			server["OnSuccessOf"] = "helper.service"
			want := "operation-inert-evidence"
			switch hazard {
			case "zero evidence":
				p.RetiredConditions = []RetiredConditionEvidence{{}}
			case "removed marker":
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			case "reset condition":
				data = `{"type":"a(sbbsi)","data":[]}`
			case "outgoing":
				server["OnSuccess"] = "helper.service"
				want = "operation-edge-outside-set"
			case "manager action":
				server["FailureAction"] = "reboot"
				want = "operation-manager-action"
			case "install target":
				if err := os.WriteFile(server["FragmentPath"], []byte("[Install]\nWantedBy=helper.target\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				want = "operation-edge-outside-set"
			}
			err = f.inspector.AdmitOperations(t.Context(), nil, p)
			if hazard == "clean" {
				if err != nil {
					t.Fatalf("proved incoming edge refused: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("%s did not refuse at %s: %v", hazard, want, err)
			}
		})
	}
}
