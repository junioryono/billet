package retirement

import (
	"encoding/json"
	"strings"
	"testing"
)

func settledVerdictFixture(purpose string) SettledVerdict {
	verdict := SettledVerdict{Schema: SettledSchema, Purpose: purpose, Outcome: "admitted", Run: "ci-1", Guard: strings.Repeat("a", 32),
		Retiring: "control-a", Deployment: strings.Repeat("d", 32), TransitionID: strings.Repeat("b", 32), Variant: VariantRetainedNode,
		Phase: PhaseDone, RowDone: true, Settled: true, CompletedBy: "control-b", NodeActivity: "quiet-inactive", State: "nothing", EnablementChanges: map[string]string{}}
	if purpose == PurposeSettledClosing {
		verdict.Outcome, verdict.NodeActivity = "verified", "active"
	}
	return verdict
}

// Removing exact-member, type, bound or exit checks accepts a corrupt variant.
func TestSettledVerdictRequiresCompleteStrictBoundDocuments(t *testing.T) {
	for _, purpose := range []string{PurposeSettledEntry, PurposeSettledClosing} {
		t.Run(purpose, func(t *testing.T) {
			want := settledVerdictFixture(purpose)
			body, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeSettledVerdict(body, 0, want); err != nil {
				t.Fatal(err)
			}
			var members map[string]any
			if err := json.Unmarshal(body, &members); err != nil {
				t.Fatal(err)
			}
			for member := range members {
				t.Run(member, func(t *testing.T) {
					for _, replacement := range []any{nil, []string{}, false} {
						changed := make(map[string]any)
						for key, value := range members {
							changed[key] = value
						}
						changed[member] = replacement
						raw, err := json.Marshal(changed)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := DecodeSettledVerdict(raw, 0, want); err == nil {
							t.Fatalf("accepted %s=%v", member, replacement)
						}
						delete(changed, member)
						raw, err = json.Marshal(changed)
						if err != nil {
							t.Fatal(err)
						}
						if _, err := DecodeSettledVerdict(raw, 0, want); err == nil {
							t.Fatalf("accepted absent %s", member)
						}
					}
				})
			}
			for _, raw := range []string{string(body) + "}", string(body) + string(body), string(body[:len(body)-1]),
				strings.Replace(string(body), `"schema":2`, `"schema":2,"schema":2`, 1),
				strings.Replace(string(body), `"schema":2`, `"Schema":2`, 1),
				strings.Replace(string(body), `"schema":2`, `"postconditions":{"node":"running"},"schema":2`, 1),
				string(body) + strings.Repeat(" ", 16<<10)} {
				if _, err := DecodeSettledVerdict([]byte(raw), 0, want); err == nil {
					t.Fatal("accepted malformed, duplicate, oversized, tail-shaped or incomplete verdict")
				}
			}
			for _, code := range []int{-1, 2, 3} {
				if _, err := DecodeSettledVerdict(body, code, want); err == nil {
					t.Fatal("accepted success with a nonzero exit")
				}
			}
			for _, change := range []func(*SettledVerdict){
				func(v *SettledVerdict) { v.Run = "other" },
				func(v *SettledVerdict) { v.Guard = strings.Repeat("f", 32) },
				func(v *SettledVerdict) { v.Retiring = "other" },
				func(v *SettledVerdict) { v.Deployment = strings.Repeat("f", 32) },
				func(v *SettledVerdict) { v.TransitionID = strings.Repeat("f", 32) },
				func(v *SettledVerdict) { v.Variant = VariantServerOnly },
			} {
				other := want
				change(&other)
				if _, err := DecodeSettledVerdict(body, 0, other); err == nil {
					t.Fatalf("accepted another classifier or invocation: %+v", other)
				}
			}
		})
	}
}

// Substituting the entry parser for closing would accept quiet activity or an
// entry-purpose document despite the caller expecting strict closing.
func TestSettledEntryAndClosingAreDifferentContracts(t *testing.T) {
	for _, purpose := range []string{PurposeSettledEntry, PurposeSettledClosing} {
		want := settledVerdictFixture(purpose)
		for _, activity := range []string{"active", "quiet-inactive", "quiet-failed", "inactive", "failed", "running", "deactivating"} {
			verdict := want
			verdict.NodeActivity = activity
			body, err := json.Marshal(verdict)
			if err != nil {
				t.Fatal(err)
			}
			_, err = DecodeSettledVerdict(body, 0, want)
			admitted := activity == "active" || purpose == PurposeSettledEntry && (activity == "quiet-inactive" || activity == "quiet-failed")
			if (err == nil) != admitted {
				t.Fatalf("%s/%s: %v", purpose, activity, err)
			}
			other := settledVerdictFixture(PurposeSettledEntry)
			if purpose == PurposeSettledEntry {
				other = settledVerdictFixture(PurposeSettledClosing)
			}
			if _, err := DecodeSettledVerdict(body, 0, other); err == nil {
				t.Fatal("entry and closing purposes were interchangeable")
			}
			var completion Completion
			if err := DecodeDocument(body, &completion); err == nil {
				t.Fatal("settled observation parsed as a completion document")
			}
			if _, err := DecodeNodeConfigVerdict(body, 0, NodeConfigVerdict{}); err == nil {
				t.Fatal("settled observation parsed as future-operation admission")
			}
		}
	}
}

func TestSettledRefusalRequiresItsOwnBranchAndExit(t *testing.T) {
	for _, purpose := range []string{PurposeSettledEntry, PurposeSettledClosing} {
		for _, code := range []int{2, 3} {
			outcome := "refused"
			if code == 3 {
				outcome = "unknown"
			}
			want := SettledRefusal{Schema: SettledSchema, Purpose: purpose, Outcome: outcome, Reason: "guard", Why: "not this holder", State: "nothing"}
			body, err := json.Marshal(want)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeSettledRefusal(body, code, purpose); err != nil {
				t.Fatal(err)
			}
			for _, otherCode := range []int{0, 5 - code} {
				if _, err := DecodeSettledRefusal(body, otherCode, purpose); err == nil {
					t.Fatal("refusal accepted with mismatched exit")
				}
			}
			for _, raw := range []string{string(body) + "{}", string(body[:len(body)-1]), string(body) + strings.Repeat(" ", 16<<10),
				strings.Replace(string(body), `"schema":2`, `"schema":2,"row_done":true`, 1),
				strings.Replace(string(body), `"schema":2`, `"Schema":2`, 1),
				strings.Replace(string(body), `"schema":2`, `"schema":2,"schema":2`, 1),
				strings.Replace(string(body), `"reason":"guard"`, `"reason":null`, 1)} {
				if _, err := DecodeSettledRefusal([]byte(raw), code, purpose); err == nil {
					t.Fatal("refusal accepted incomplete, malformed or success-branch members")
				}
			}
			if _, err := DecodeSettledVerdict(body, code, settledVerdictFixture(purpose)); err == nil {
				t.Fatal("refusal admitted as success")
			}
		}
	}
}

func TestSettledEnablementChangesAreTypedReports(t *testing.T) {
	want := settledVerdictFixture(PurposeSettledClosing)
	want.EnablementChanges = map[string]string{"billet-server.service": "enabled"}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeSettledVerdict(body, 0, want); err != nil {
		t.Fatal(err)
	}
	for _, replacement := range []string{`null`, `[]`, `{"other.service":"enabled"}`, `{"billet-server.service":true}`, `{"billet-server.service":""}`} {
		changed := strings.Replace(string(body), `{"billet-server.service":"enabled"}`, replacement, 1)
		if changed == string(body) {
			t.Fatal("corruption did not change the report")
		}
		if _, err := DecodeSettledVerdict([]byte(changed), 0, want); err == nil {
			t.Fatalf("accepted malformed enablement report: %s", changed)
		}
	}
}
