package retirement

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestNodeConfigVerdictNamesTheFirstDisagreementAndBothValues(t *testing.T) {
	want := NodeConfigVerdict{Schema: 1, Purpose: "node-config", Outcome: "admitted", Run: "ci-1", Guard: strings.Repeat("a", 32),
		Retiring: "control-a", Deployment: strings.Repeat("d", 32), TransitionID: strings.Repeat("b", 32), Variant: VariantRetainedNode,
		RenderingSHA256: strings.Repeat("c", 64), OperationsSHA256: strings.Repeat("e", 64), NodeActivity: "active", State: "nothing"}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	bindings := []string{"run", "guard", "retiring", "deployment", "transition_id", "rendering_sha256", "operations_sha256"}
	for i, member := range bindings {
		t.Run(member, func(t *testing.T) {
			for _, multiple := range []bool{false, true} {
				var changed map[string]any
				if err := json.Unmarshal(body, &changed); err != nil {
					t.Fatal(err)
				}
				expected := changed[member]
				// Quoting keeps even malformed values on one diagnostic line.
				const replacement = "different\nvalue"
				changed[member] = replacement
				if multiple {
					for _, later := range bindings[i+1:] {
						changed[later] = replacement
					}
				}
				raw, err := json.Marshal(changed)
				if err != nil {
					t.Fatal(err)
				}
				_, err = DecodeNodeConfigVerdict(raw, 0, want)
				diagnostic := fmt.Sprintf("node-config verdict binds %s=%q, expected %q", member, replacement, expected)
				if err == nil || err.Error() != diagnostic {
					t.Fatalf("wrong binding diagnostic (multiple=%t): got %v, want %s", multiple, err, diagnostic)
				}
			}
		})
	}
}

func TestNodeConfigVerdictBindsEveryOperandAndItsExitStatus(t *testing.T) {
	want := NodeConfigVerdict{Schema: 1, Purpose: "node-config", Outcome: "admitted", Run: "ci-1", Guard: strings.Repeat("a", 32),
		Retiring: "control-a", Deployment: strings.Repeat("d", 32), TransitionID: strings.Repeat("b", 32), Variant: VariantRetainedNode,
		RenderingSHA256: strings.Repeat("c", 64), OperationsSHA256: strings.Repeat("e", 64), NodeActivity: "quiet-inactive", State: "nothing"}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeNodeConfigVerdict(body, 0, want); err != nil {
		t.Fatal(err)
	}
	var members map[string]any
	if err := json.Unmarshal(body, &members); err != nil {
		t.Fatal(err)
	}
	for member := range members {
		t.Run(member, func(t *testing.T) {
			for _, replacement := range []any{nil, false, "different"} {
				changed := make(map[string]any)
				for key, value := range members {
					changed[key] = value
				}
				changed[member] = replacement
				raw, err := json.Marshal(changed)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := DecodeNodeConfigVerdict(raw, 0, want); err == nil {
					t.Fatalf("accepted changed %s=%v", member, replacement)
				}
				delete(changed, member)
				raw, err = json.Marshal(changed)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := DecodeNodeConfigVerdict(raw, 0, want); err == nil {
					t.Fatalf("accepted missing %s", member)
				}
			}
		})
	}
	for _, raw := range []string{string(body) + "}", string(body) + string(body), string(body[:len(body)-1]),
		strings.Replace(string(body), `"schema":1`, `"schema":1,"schema":1`, 1),
		strings.Replace(string(body), `"schema":1`, `"Schema":1`, 1),
		strings.Replace(string(body), `"schema":1`, `"extra":0,"schema":1`, 1), string(body) + strings.Repeat(" ", 16<<10)} {
		if _, err := DecodeNodeConfigVerdict([]byte(raw), 0, want); err == nil {
			t.Fatal("accepted malformed, unknown, duplicate, oversized or incomplete verdict")
		}
	}
	if _, err := DecodeNodeConfigVerdict(body, 3, want); err == nil {
		t.Fatal("accepted admitted JSON with a nonzero exit")
	}
}
