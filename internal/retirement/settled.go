package retirement

import "fmt"

// PurposeSettledEntry and PurposeSettledClosing identify distinct contracts.
const (
	PurposeSettledEntry   = "settled-entry"
	PurposeSettledClosing = "settled-closing"
)

// SettledVerdict is a current observation, never a publication or drain token.
// Neither purpose shares the tail's answer or its running postcondition member.
type SettledVerdict struct {
	Schema       int     `json:"schema"`
	Purpose      string  `json:"purpose"`
	Outcome      string  `json:"outcome"`
	Run          string  `json:"run"`
	Guard        string  `json:"guard"`
	Retiring     string  `json:"retiring"`
	Deployment   string  `json:"deployment"`
	TransitionID string  `json:"transition_id"`
	Variant      Variant `json:"variant"`
	Phase        Phase   `json:"phase"`
	RowDone      bool    `json:"row_done"`
	Settled      bool    `json:"settled"`
	CompletedBy  string  `json:"completed_by"`
	NodeActivity string  `json:"node_activity"`
	State        string  `json:"state"`
}

// SettledRefusal carries no successful bindings or observed postconditions.
type SettledRefusal struct {
	Schema  int    `json:"schema"`
	Purpose string `json:"purpose"`
	Outcome string `json:"outcome"`
	Reason  string `json:"reason"`
	Why     string `json:"why"`
	State   string `json:"state"`
}

// DecodeSettledVerdict keeps entry and closing in distinct parser branches.
func DecodeSettledVerdict(raw []byte, exitCode int, expected SettledVerdict) (SettledVerdict, error) {
	var verdict SettledVerdict
	if len(raw) > 16<<10 {
		return verdict, fmt.Errorf("settled verdict exceeds the read bound")
	}
	if err := DecodeDocument(raw, &verdict); err != nil {
		return verdict, err
	}
	if exitCode != 0 || verdict.Schema != 1 || verdict.Purpose != expected.Purpose ||
		verdict.Variant != VariantRetainedNode || verdict.Variant != expected.Variant || verdict.Phase != PhaseDone || !verdict.RowDone || !verdict.Settled ||
		verdict.CompletedBy == "" || verdict.State != "nothing" {
		return verdict, fmt.Errorf("not a successful settled observation")
	}
	switch verdict.Purpose {
	case PurposeSettledEntry:
		if verdict.Outcome != "admitted" || (verdict.NodeActivity != "active" && verdict.NodeActivity != "quiet-inactive" && verdict.NodeActivity != "quiet-failed") {
			return verdict, fmt.Errorf("not an admitted settled-entry observation")
		}
	case PurposeSettledClosing:
		if verdict.Outcome != "verified" || verdict.NodeActivity != "active" {
			return verdict, fmt.Errorf("not a strict settled-closing observation")
		}
	default:
		return verdict, fmt.Errorf("unknown settled purpose")
	}
	for _, binding := range []struct{ member, have, want string }{
		{"run", verdict.Run, expected.Run},
		{"guard", verdict.Guard, expected.Guard},
		{"retiring", verdict.Retiring, expected.Retiring},
		{"deployment", verdict.Deployment, expected.Deployment},
		{"transition_id", verdict.TransitionID, expected.TransitionID},
	} {
		if binding.have == "" || binding.have != binding.want {
			return verdict, fmt.Errorf("settled verdict binds %s=%q, expected %q", binding.member, binding.have, binding.want)
		}
	}
	if !transitionIDPattern.MatchString(verdict.Guard) || !transitionIDPattern.MatchString(verdict.TransitionID) {
		return verdict, fmt.Errorf("settled verdict has malformed guard or transition")
	}
	return verdict, nil
}

// DecodeSettledRefusal rejects success members and mismatched exit semantics.
func DecodeSettledRefusal(raw []byte, exitCode int, purpose string) (SettledRefusal, error) {
	var refusal SettledRefusal
	if len(raw) > 16<<10 {
		return refusal, fmt.Errorf("settled refusal exceeds the read bound")
	}
	if err := DecodeDocument(raw, &refusal); err != nil {
		return refusal, err
	}
	if refusal.Schema != 1 || refusal.Purpose != purpose ||
		(purpose != PurposeSettledEntry && purpose != PurposeSettledClosing) || refusal.State != "nothing" ||
		refusal.Reason == "" || refusal.Why == "" ||
		(exitCode != 2 || refusal.Outcome != "refused") && (exitCode != 3 || refusal.Outcome != "unknown") {
		return refusal, fmt.Errorf("not a settled refusal with matching exit semantics")
	}
	return refusal, nil
}
